package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hqgroups/mcp/internal/agent/runtime"
	"github.com/hqgroups/mcp/internal/ai/provider"
)

type sseEvent struct {
	name string
	data map[string]any
}

// readEvents đọc một luồng SSE tới khi server đóng nó.
func readEvents(t *testing.T, body string) []sseEvent {
	t.Helper()
	var out []sseEvent
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	cur := sseEvent{}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			cur.name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &cur.data); err != nil {
				t.Fatalf("data không phải JSON: %q", line)
			}
		case line == "" && cur.name != "":
			out = append(out, cur)
			cur = sseEvent{}
		}
	}
	return out
}

func TestExecutionEvents(t *testing.T) {
	e := setup(t)
	adminA, adminB, viewerA := e.tok("admin-a"), e.tok("admin-b"), e.tok("viewer-a")
	f := newPlannedFixture(t, e, "events-a")
	f.agent("monitoring", []string{"list_containers"}, nil)
	oldPoll := runtime.EventsPoll
	runtime.EventsPoll = 20 * time.Millisecond
	t.Cleanup(func() { runtime.EventsPoll = oldPoll })

	const secret = "NOI-DUNG-KHONG-DUOC-VAO-SU-KIEN"
	f.up.mu.Lock()
	f.up.reply = "KET-QUA-TOOL-" + secret
	f.up.mu.Unlock()
	entered, hold := make(chan struct{}, 2), make(chan struct{})
	e.model.script(func(ctx context.Context, _ int, req provider.Request) (provider.Response, error) {
		if hasToolResult(req) {
			return answer("TRA-LOI-" + secret), nil
		}
		entered <- struct{}{}
		select {
		case <-hold:
		case <-ctx.Done():
		}
		return toolUse("infra.list_containers"), nil
	})
	id := f.single(adminA, "monitoring")
	<-entered
	// Tenant khác không thấy luồng; người chỉ đọc thì xem được.
	want(t, e.do("GET", "/v1/executions/"+id+"/events", adminB, nil, nil), 404, "not_found")

	// Luồng mở khi execution đang chạy và đóng khi nó kết thúc: client nhận trạng thái hiện
	// tại rồi tới từng thay đổi.
	type result struct {
		r resp
	}
	got := make(chan result, 1)
	go func() { got <- result{e.do("GET", "/v1/executions/"+id+"/events", viewerA, nil, nil)} }()
	time.Sleep(200 * time.Millisecond)
	close(hold)
	var r resp
	select {
	case res := <-got:
		r = res.r
	case <-time.After(30 * time.Second):
		t.Fatal("luồng sự kiện không đóng khi execution kết thúc")
	}
	if r.status != http.StatusOK || !strings.HasPrefix(r.header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("luồng: %d %q", r.status, r.header.Get("Content-Type"))
	}
	if strings.Contains(string(r.body), secret) || strings.Contains(string(r.body), "Kiểm tra production") || strings.Contains(string(r.body), "AGENT:") {
		t.Fatalf("luồng sự kiện chứa nội dung: %s", r.body)
	}
	events := readEvents(t, string(r.body))
	var seq []string
	for _, ev := range events {
		switch ev.name {
		case "execution":
			seq = append(seq, "execution:"+ev.data["status"].(string))
		case "task":
			seq = append(seq, "task:"+ev.data["status"].(string))
		case "step":
			seq = append(seq, "step:"+ev.data["kind"].(string)+":"+ev.data["status"].(string))
		default:
			seq = append(seq, ev.name)
		}
	}
	joined := strings.Join(seq, " ")
	// Trạng thái lúc nối (task và lượt model đang chạy), rồi các thay đổi theo thứ tự, rồi kết thúc.
	for _, wantPart := range []string{"task:running", "step:llm_call:started", "execution:running",
		"step:llm_call:succeeded", "step:tool_call:succeeded", "task:completed", "execution:completed"} {
		if !strings.Contains(joined, wantPart) {
			t.Errorf("thiếu sự kiện %q trong: %s", wantPart, joined)
		}
	}
	if len(seq) == 0 || seq[len(seq)-1] != "end" || seq[len(seq)-2] != "execution:completed" {
		t.Errorf("luồng không kết thúc bằng execution:completed rồi end: %s", joined)
	}
	if i, j := strings.Index(joined, "step:llm_call:started"), strings.Index(joined, "step:llm_call:succeeded"); i < 0 || j < i {
		t.Errorf("thứ tự step sai: %s", joined)
	}
	for _, ev := range events {
		if ev.name == "step" && ev.data["kind"] == "tool_call" && ev.data["tool_name"] != "infra.list_containers" {
			t.Errorf("step tool: %v", ev.data)
		}
	}

	// Execution đã kết thúc: luồng phát trạng thái cuối rồi đóng ngay.
	done := e.do("GET", "/v1/executions/"+id+"/events", adminA, nil, nil)
	final := readEvents(t, string(done.body))
	if done.status != 200 || len(final) < 3 || final[len(final)-1].name != "end" {
		t.Errorf("luồng của execution đã kết thúc: %d, %d sự kiện", done.status, len(final))
	}
}
