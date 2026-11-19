package anthropic_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/hqgroups/mcp/internal/ai/provider"
	"github.com/hqgroups/mcp/internal/ai/provider/anthropic"
	apperr "github.com/hqgroups/mcp/internal/shared/errors"
)

// fakeAPI là Anthropic Messages API giả: ghi lại request và phát lại một luồng SSE.
type fakeAPI struct {
	mu       sync.Mutex
	requests []map[string]any
	headers  []http.Header
	status   int
	events   []string // mỗi phần tử là JSON của một event
	srv      *httptest.Server
}

func newFake(t *testing.T) *fakeAPI {
	f := &fakeAPI{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		f.mu.Lock()
		f.requests = append(f.requests, req)
		f.headers = append(f.headers, r.Header.Clone())
		status, events := f.status, f.events
		f.mu.Unlock()
		if status != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("request-id", "req_test")
			w.WriteHeader(status)
			_, _ = fmt.Fprintf(w, `{"type":"error","error":{"type":"some_error","message":"SECRET PROMPT ECHO"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range events {
			var typ struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal([]byte(ev), &typ)
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typ.Type, ev)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) provider(t *testing.T) *anthropic.Provider {
	p, err := anthropic.New(anthropic.Config{APIKey: "test-key", BaseURL: f.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func start(stop string) string {
	return `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"model-x-served","content":[],"stop_reason":null,"usage":{"input_tokens":120,"output_tokens":1,"cache_read_input_tokens":900,"cache_creation_input_tokens":300}}}`
}

func end(stop string, out int) []string {
	return []string{
		fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q},"usage":{"output_tokens":%d}}`, stop, out),
		`{"type":"message_stop"}`,
	}
}

var tools = []provider.Tool{
	{Name: "docker.list_containers", Description: "Liệt kê container.",
		InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["all"],"properties":{"all":{"type":"boolean"}}}`)},
}

func TestToolUseTurn(t *testing.T) {
	f := newFake(t)
	f.events = append([]string{
		start(""),
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"cần xem container"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"SIG123"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Để tôi kiểm tra."}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"docker__list_containers","input":{}}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"all\":"}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":" true}"}}`,
		`{"type":"content_block_stop","index":2}`,
	}, end("tool_use", 77)...)
	p := f.provider(t)

	resp, err := p.Complete(context.Background(), provider.Request{
		Model: "model-x", System: "Bạn là DevOps Agent.", MaxOutputTokens: 2048, Tools: tools,
		Messages: []provider.Message{{Role: provider.RoleUser, Text: "Container nào lỗi?"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != provider.StopToolUse || resp.Text != "Để tôi kiểm tra." || resp.ModelUsed != "model-x-served" {
		t.Errorf("response: %+v", resp)
	}
	// Tên tool trở lại dạng của platform; arguments là JSON đầy đủ.
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "docker.list_containers" || resp.ToolCalls[0].ID != "toolu_1" {
		t.Fatalf("tool calls: %+v", resp.ToolCalls)
	}
	var args map[string]any
	if json.Unmarshal(resp.ToolCalls[0].Arguments, &args) != nil || args["all"] != true {
		t.Errorf("arguments: %s", resp.ToolCalls[0].Arguments)
	}
	if u := resp.Usage; u.InputTokens != 120 || u.OutputTokens != 77 || u.CachedInputTokens != 900 || u.CacheWriteTokens != 300 {
		t.Errorf("usage: %+v", u)
	}

	// Request: tên tool hợp lệ trên dây, schema đầy đủ, system tách riêng, có đánh dấu cache, không retry ở SDK.
	req := f.requests[0]
	tool := req["tools"].([]any)[0].(map[string]any)
	schema := tool["input_schema"].(map[string]any)
	if tool["name"] != "docker__list_containers" || schema["type"] != "object" || schema["additionalProperties"] != false ||
		fmt.Sprint(schema["required"]) != "[all]" || schema["properties"].(map[string]any)["all"] == nil {
		t.Errorf("tool trên dây: %v", tool)
	}
	if req["model"] != "model-x" || req["max_tokens"] != float64(2048) || req["stream"] != true || req["cache_control"] == nil ||
		req["system"].([]any)[0].(map[string]any)["text"] != "Bạn là DevOps Agent." || req["temperature"] != nil {
		t.Errorf("request: %v", req)
	}
	if f.headers[0].Get("Authorization") != "Bearer test-key" {
		t.Error("thiếu API key trong header")
	}

	// Lượt kế: khối suy luận và chữ ký được gửi lại y nguyên; kết quả tool đi trong lượt user.
	f.mu.Lock()
	f.events = append([]string{start(""),
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"web đang restart."}}`,
		`{"type":"content_block_stop","index":0}`}, end("end_turn", 12)...)
	f.mu.Unlock()
	resp2, err := p.Complete(context.Background(), provider.Request{
		Model: "model-x", System: "Bạn là DevOps Agent.", MaxOutputTokens: 2048, Tools: tools,
		Messages: []provider.Message{
			{Role: provider.RoleUser, Text: "Container nào lỗi?"},
			{Role: provider.RoleAssistant, Text: resp.Text, ToolCalls: resp.ToolCalls, Opaque: resp.Opaque},
			{Role: provider.RoleTool, ToolCallID: "toolu_1", Text: `{"containers":[]}`},
			{Role: provider.RoleTool, ToolCallID: "toolu_2", Text: "bị từ chối", IsError: true},
		},
	})
	if err != nil || resp2.StopReason != provider.StopEndTurn || resp2.Text != "web đang restart." {
		t.Fatalf("lượt hai: %+v %v", resp2, err)
	}
	msgs := f.requests[1]["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("muốn 3 message (user, assistant, user với 2 tool_result), có %d", len(msgs))
	}
	assistant := msgs[1].(map[string]any)["content"].([]any)
	th := assistant[0].(map[string]any)
	if msgs[1].(map[string]any)["role"] != "assistant" || len(assistant) != 3 || th["type"] != "thinking" ||
		th["thinking"] != "cần xem container" || th["signature"] != "SIG123" ||
		assistant[2].(map[string]any)["name"] != "docker__list_containers" {
		t.Errorf("lượt assistant gửi lại: %v", assistant)
	}
	results := msgs[2].(map[string]any)["content"].([]any)
	r1, r2 := results[0].(map[string]any), results[1].(map[string]any)
	if msgs[2].(map[string]any)["role"] != "user" || len(results) != 2 || r1["type"] != "tool_result" || r1["tool_use_id"] != "toolu_1" ||
		r2["is_error"] != true {
		t.Errorf("kết quả tool: %v", results)
	}
}

func TestHistoryFromAnotherAdapter(t *testing.T) {
	f := newFake(t)
	f.events = append([]string{start("")}, end("end_turn", 1)...)
	_, err := f.provider(t).Complete(context.Background(), provider.Request{
		Model: "m", MaxOutputTokens: 100, Tools: tools,
		Messages: []provider.Message{
			{Role: provider.RoleUser, Text: "q"},
			{Role: provider.RoleAssistant, Text: "xem", Opaque: json.RawMessage(`{"adapter":"claude-cli","x":1}`),
				ToolCalls: []provider.ToolCall{{ID: "c1", Name: "docker.list_containers", Arguments: json.RawMessage(`{"all":false}`)}}},
			{Role: provider.RoleTool, ToolCallID: "c1", Text: "ok"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	a := f.requests[0]["messages"].([]any)[1].(map[string]any)["content"].([]any)
	use := a[1].(map[string]any)
	if len(a) != 2 || a[0].(map[string]any)["text"] != "xem" || use["type"] != "tool_use" || use["name"] != "docker__list_containers" ||
		use["input"].(map[string]any)["all"] != false {
		t.Errorf("lượt assistant dựng lại: %v", a)
	}
}

func TestStopReasons(t *testing.T) {
	for wire, want := range map[string]provider.StopReason{
		"end_turn": provider.StopEndTurn, "max_tokens": provider.StopMaxTokens, "refusal": provider.StopRefusal,
		"stop_sequence": provider.StopEndTurn, "pause_turn": "pause_turn", "model_context_window_exceeded": "model_context_window_exceeded",
	} {
		f := newFake(t)
		f.events = append([]string{start("")}, end(wire, 3)...)
		resp, err := f.provider(t).Complete(context.Background(), provider.Request{Model: "m", MaxOutputTokens: 10,
			Messages: []provider.Message{{Role: provider.RoleUser, Text: "q"}}})
		if err != nil || resp.StopReason != want {
			t.Errorf("%s: %q %v, muốn %q", wire, resp.StopReason, err, want)
		}
	}
}

func TestErrors(t *testing.T) {
	for status, retryable := range map[int]bool{429: true, 500: true, 529: true, 400: false, 401: false, 404: false, 413: false} {
		f := newFake(t)
		f.status = status
		_, err := f.provider(t).Complete(context.Background(), provider.Request{Model: "m", MaxOutputTokens: 10,
			Messages: []provider.Message{{Role: provider.RoleUser, Text: "q"}}})
		if err == nil || apperr.IsRetryable(err) != retryable {
			t.Errorf("HTTP %d: err = %v, retryable = %v, muốn %v", status, err, apperr.IsRetryable(err), retryable)
		}
		if err != nil && (strings.Contains(err.Error(), "SECRET PROMPT ECHO") || strings.Contains(err.Error(), "test-key")) {
			t.Errorf("HTTP %d: lỗi chứa nội dung của provider hoặc khoá: %v", status, err)
		}
		// Retry thuộc về runtime: SDK chỉ gọi đúng một lần.
		if len(f.requests) != 1 {
			t.Errorf("HTTP %d: SDK gọi %d lần", status, len(f.requests))
		}
	}
	// Luồng bị ngắt giữa chừng là lỗi tạm thời.
	f := newFake(t)
	f.events = []string{start(""), `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`}
	if resp, err := f.provider(t).Complete(context.Background(), provider.Request{Model: "m", MaxOutputTokens: 10,
		Messages: []provider.Message{{Role: provider.RoleUser, Text: "q"}}}); err == nil && resp.StopReason == provider.StopEndTurn {
		t.Errorf("luồng bị cắt được coi là trả lời cuối: %+v", resp)
	}
	// Luồng hỏng sau khi provider đã bắt đầu xử lý: lỗi được trả về KÈM usage đã báo
	// (provider vẫn tính tiền phần đó), để nơi giữ budget quyết toán thay vì hoàn toàn bộ.
	f = newFake(t)
	f.events = []string{start(""), `{"type":"content_block_delta","index":7,"delta":{"type":"text_delta","text":"x"}}`}
	resp, err := f.provider(t).Complete(context.Background(), provider.Request{Model: "m", MaxOutputTokens: 10,
		Messages: []provider.Message{{Role: provider.RoleUser, Text: "q"}}})
	if err == nil || !apperr.IsRetryable(err) {
		t.Fatalf("luồng hỏng giữa chừng: err = %v, muốn lỗi tạm thời", err)
	}
	if resp.Usage.InputTokens != 120 || resp.Usage.CachedInputTokens != 900 || resp.Usage.CacheWriteTokens != 300 || resp.Text != "" {
		t.Errorf("usage dở dang kèm lỗi: %+v (text %q)", resp.Usage, resp.Text)
	}
}

func TestToolNames(t *testing.T) {
	f := newFake(t)
	p := f.provider(t)
	req := func(names ...string) provider.Request {
		r := provider.Request{Model: "m", MaxOutputTokens: 10, Messages: []provider.Message{{Role: provider.RoleUser, Text: "q"}}}
		for _, n := range names {
			r.Tools = append(r.Tools, provider.Tool{Name: n, InputSchema: json.RawMessage(`{"type":"object"}`)})
		}
		return r
	}
	if _, err := p.Complete(context.Background(), req("a.b_c", "a__b.c", "a.b__c")); err == nil {
		t.Error("hai tên trùng nhau trên dây được chấp nhận")
	}
	if _, err := p.Complete(context.Background(), req("server."+strings.Repeat("x", 70))); err == nil {
		t.Error("tên quá dài được chấp nhận")
	}
	if len(f.requests) != 0 {
		t.Error("request vẫn được gửi với tên tool không hợp lệ")
	}
	if _, err := anthropic.New(anthropic.Config{}); err == nil {
		t.Error("tạo được provider không có API key")
	}
}

// Chạy với API thật: HQ_TEST_ANTHROPIC=1 HQ_ANTHROPIC_API_KEY=... HQ_TEST_ANTHROPIC_MODEL=<model id>
// go test ./internal/ai/provider/anthropic -run Real -v   (tốn tiền thật)
func TestRealAPI(t *testing.T) {
	key, model := os.Getenv("HQ_ANTHROPIC_API_KEY"), os.Getenv("HQ_TEST_ANTHROPIC_MODEL")
	if os.Getenv("HQ_TEST_ANTHROPIC") != "1" || key == "" || model == "" {
		t.Skip("đặt HQ_TEST_ANTHROPIC=1, HQ_ANTHROPIC_API_KEY và HQ_TEST_ANTHROPIC_MODEL để chạy với API thật")
	}
	p, err := anthropic.New(anthropic.Config{APIKey: key})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	msgs := []provider.Message{{Role: provider.RoleUser, Text: "Liệt kê mọi container (kể cả đã dừng) bằng tool, rồi cho biết có bao nhiêu cái."}}
	resp, err := p.Complete(ctx, provider.Request{Model: model, System: "Bạn là DevOps Agent chỉ đọc.", MaxOutputTokens: 4096, Tools: tools, Messages: msgs})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("lượt 1: stop=%s tool_calls=%+v usage=%+v", resp.StopReason, resp.ToolCalls, resp.Usage)
	if resp.StopReason != provider.StopToolUse || len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "docker.list_containers" {
		t.Fatalf("muốn một tool call docker.list_containers: %+v", resp)
	}
	msgs = append(msgs, provider.Message{Role: provider.RoleAssistant, Text: resp.Text, ToolCalls: resp.ToolCalls, Opaque: resp.Opaque},
		provider.Message{Role: provider.RoleTool, ToolCallID: resp.ToolCalls[0].ID, Text: `{"containers":[{"names":["web"],"state":"running"},{"names":["db"],"state":"exited"}]}`})
	resp, err = p.Complete(ctx, provider.Request{Model: model, System: "Bạn là DevOps Agent chỉ đọc.", MaxOutputTokens: 4096, Tools: tools, Messages: msgs})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("lượt 2: stop=%s text=%q usage=%+v", resp.StopReason, resp.Text, resp.Usage)
	if resp.StopReason != provider.StopEndTurn || !strings.Contains(resp.Text, "2") {
		t.Errorf("trả lời cuối: %+v", resp)
	}
}
