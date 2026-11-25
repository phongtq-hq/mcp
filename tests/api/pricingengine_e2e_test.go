package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hqgroups/mcp/internal/adminops"
	"github.com/hqgroups/mcp/internal/ai/provider"
	"github.com/hqgroups/mcp/internal/integration/pricingengine"
	"github.com/hqgroups/mcp/internal/integration/serve"
	"github.com/hqgroups/mcp/internal/mcp/registry"
)

// TestPricingEngineEndToEnd chạy integration Pricing Engine qua đúng đường của client bên
// ngoài: MCP client → Gateway (xác thực, schema đã ghim, policy, ngữ cảnh ký, che secret,
// audit) → managed server → API của PE, và qua đường của Agent Runtime (execution → Gateway
// bằng execution token → managed server) cho hai tool giá, vốn chỉ chạy trong execution. PE ở đây là GIẢ LẬP (httptest, hình dạng JSON
// chép từ mã nguồn PE): test này không chứng minh gì về một PE thật.
func TestPricingEngineEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("cần Docker")
	}
	e := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	const apiKey = "sk_live_e2e_0123456789abcdef"
	const bookJSON = `{"sku":"HQ-FREEFIRE-GC-20","name":"Free Fire Gift Card $20","generatedAt":"2026-10-23T08:00:05.000Z",
		"comparisonCurrency":"USD","scoringProfile":{"id":"default","version":3},"offers":[
		{"rank":1,"supplier":{"code":"SUP_A","name":"Supplier A"},"supplierProductId":"a-123","price":"17.000000","currency":"USD",
		 "available":true,"priceUpdatedAt":"2026-10-23T07:55:00.000Z","stale":false,"eligible":true,"score":92.3,
		 "scoreBreakdown":{"price":100,"speed":63.2,"success":60.2},"lowConfidence":false}]}`
	var requests atomic.Int64
	pe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer "+apiKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/prices/HQ-FREEFIRE-GC-20":
			_, _ = io.WriteString(w, bookJSON)
		case "POST /api/v1/prices/batch":
			var in struct {
				SKUs []string `json:"skus"`
			}
			_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in)
			results, notFound := map[string]json.RawMessage{}, []string{}
			for _, sku := range in.SKUs {
				if sku == "HQ-FREEFIRE-GC-20" {
					results[sku] = json.RawMessage(bookJSON)
				} else {
					notFound = append(notFound, sku)
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"results": results, "notFound": notFound})
		case "GET /api/v1/brands":
			_, _ = io.WriteString(w, `{"data":[{"id":"0198a1b2-c3d4-7e5f-8a9b-000000000001","name":"Free Fire","nameKey":"freefire",
				"status":"active","regions":["GLOBAL"],"createdAt":"2026-10-01T00:00:00.000Z","updatedAt":"2026-10-23T08:00:00.000Z"}],
				"meta":{"nextCursor":"bmV4dA","hasMore":true,"deltaToken":null}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"errors_sku_not_found"}`)
		}
	}))
	t.Cleanup(pe.Close)

	signer := testSigner
	client, err := pricingengine.NewClient(pe.URL, pricingengine.Options{APIKey: apiKey, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	up := httptest.NewServer(pricingengine.Handler(client, signer.Verifier(), e.orgA))
	t.Cleanup(up.Close)
	conn, err := e.db.Admin.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Đăng ký như người vận hành: discovery thật qua MCP, ghim hash định nghĩa từng tool.
	if _, err := adminops.RegisterManagedMCPServer(ctx, conn.Conn(), signer, adminops.MCPServer{OrgSlug: "org-a", Slug: pricingengine.Slug}, "Pricing Engine", up.URL); err != nil {
		conn.Release()
		t.Fatalf("đăng ký server: %v", err)
	}
	conn.Release()

	adminA, adminB := e.tok("admin-a"), e.tok("admin-b")
	projA := e.createProject(adminA, "pe-a", "PE A")["id"].(string)
	projB := e.createProject(adminB, "pe-b", "PE B")["id"].(string)
	connect := func(tok, project string) *mcp.ClientSession {
		t.Helper()
		c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
		s, err := c.Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint: e.gw.URL + "/mcp/v1/projects/" + project, HTTPClient: &http.Client{Transport: bearer{tok}},
			DisableStandaloneSSE: true, MaxRetries: -1,
		}, nil)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	}
	text := func(r *mcp.CallToolResult) string {
		var b strings.Builder
		for _, c := range r.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				b.WriteString(tc.Text)
			}
		}
		return b.String()
	}
	sess := connect(adminA, projA)

	t.Run("discovery through the gateway lists the six read tools", func(t *testing.T) {
		res, err := sess.ListTools(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, tool := range res.Tools {
			names = append(names, tool.Name)
		}
		sort.Strings(names)
		want := "pricing-engine.get_price_book,pricing-engine.get_price_books,pricing-engine.get_sku,pricing-engine.list_brands,pricing-engine.list_products,pricing-engine.list_skus"
		if got := strings.Join(names, ","); got != want {
			t.Fatalf("tools = %s", got)
		}
		// Org khác không thấy server của org A.
		other, err := connect(adminB, projB).ListTools(ctx, nil)
		if err != nil || len(other.Tools) != 0 {
			t.Fatalf("org B thấy %d tool (%v)", len(other.Tools), err)
		}
	})

	t.Run("an external client cannot read prices but can read the catalogue", func(t *testing.T) {
		// Client bên ngoài (token OIDC qua /mcp/v1/projects/…) không thuộc execution nào: hai
		// tool giá bị managed server từ chối, và PE không nhận request nào.
		before := requests.Load()
		for name, args := range map[string]map[string]any{
			"pricing-engine.get_price_book":  {"sku": "HQ-FREEFIRE-GC-20"},
			"pricing-engine.get_price_books": {"skus": []any{"HQ-FREEFIRE-GC-20"}},
		} {
			res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
			if err == nil && !res.IsError {
				t.Fatalf("%s: client bên ngoài đọc được giá: %s", name, text(res))
			}
			if err == nil {
				if body := text(res); strings.Contains(body, "17.000000") || strings.Contains(body, "SUP_A") {
					t.Errorf("%s: lời từ chối mang dữ liệu giá: %s", name, body)
				} else if !strings.Contains(body, serve.ErrExecutionOnly) {
					t.Errorf("%s: lời từ chối phải nói rõ tool chỉ chạy trong execution: %s", name, body)
				}
			}
		}
		if n := requests.Load() - before; n != 0 {
			t.Errorf("tool giá gọi từ ngoài execution vẫn tạo %d request tới PE", n)
		}

		res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "pricing-engine.list_brands", Arguments: map[string]any{}})
		body := text(res)
		if err != nil || res.IsError || !strings.Contains(body, `"next_cursor":"bmV4dA"`) || !strings.Contains(body, `"name":"Free Fire"`) {
			t.Errorf("list_brands (con trỏ phân trang phải qua được Gateway): %v %s", err, body)
		}
		if strings.Contains(body, "REDACTED") || strings.Contains(body, "freefire") {
			t.Errorf("list_brands: kết quả bị che hoặc mang nameKey: %s", body)
		}
	})

	t.Run("price tools classified sensitive do not exist for external clients", func(t *testing.T) {
		// Người vận hành gắn nhãn sensitive cho hai tool giá (runbook): từ đó Gateway tự thực
		// thi mức sàn, không phụ thuộc việc integration có tự chặn hay không.
		c, err := e.db.Admin.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Release()
		sensitive := "sensitive"
		for _, tool := range []string{"get_price_book", "get_price_books"} {
			if err := adminops.ClassifyMCPTool(ctx, c.Conn(), adminops.MCPServer{OrgSlug: "org-a", Slug: pricingengine.Slug}, tool,
				registry.Classification{DataSensitivity: &sensitive}); err != nil {
				t.Fatal(err)
			}
		}
		res, err := sess.ListTools(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, tool := range res.Tools {
			if strings.Contains(tool.Name, "price_book") {
				t.Errorf("tool sensitive lộ ra cho client bên ngoài: %s", tool.Name)
			}
		}
		if len(res.Tools) != 4 {
			t.Errorf("client bên ngoài thấy %d tool, muốn 4 tool danh mục", len(res.Tools))
		}
		before := requests.Load()
		if r, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "pricing-engine.get_price_book", Arguments: map[string]any{"sku": "HQ-FREEFIRE-GC-20"}}); err == nil && !r.IsError {
			t.Errorf("gọi trực tiếp tool sensitive được: %s", text(r))
		}
		if n := requests.Load() - before; n != 0 {
			t.Errorf("tool sensitive gọi trực tiếp vẫn tạo %d request tới PE", n)
		}
	})

	t.Run("an agent execution reads price books through the gateway", func(t *testing.T) {
		post := func(path string, body any, status int) map[string]any {
			t.Helper()
			r := e.do("POST", path, adminA, body, nil)
			if r.status != status {
				t.Fatalf("POST %s: %d %s", path, r.status, r.body)
			}
			return r.json(t)
		}
		var serverID string
		for _, srv := range e.do("GET", "/v1/mcp-servers", adminA, nil, nil).json(t)["data"].([]any) {
			if m := srv.(map[string]any); m["slug"] == pricingengine.Slug {
				serverID = m["id"].(string)
			}
		}
		if serverID == "" {
			t.Fatal("không thấy server pricing-engine trong Registry")
		}
		provID := post("/v1/model-providers", map[string]any{"slug": "claude-local-pe", "adapter": "claude-cli"}, 201)["id"].(string)
		modelID := post("/v1/models", map[string]any{"provider_id": provID, "provider_model": "sonnet", "display_name": "Sonnet",
			"tier": "standard", "capabilities": map[string]any{"tool_calling": true, "context_window": 200000}}, 201)["id"].(string)
		post("/v1/models/"+modelID+"/pricing", map[string]any{"currency": "USD", "input_per_mtok": "3.00", "output_per_mtok": "15.00",
			"cached_input_per_mtok": "0.30", "effective_from": time.Now().UTC().Format(time.RFC3339Nano)}, 201)
		agentID := post("/v1/projects/"+projA+"/agents", map[string]any{"slug": "pricing-advisor", "name": "Pricing advisor",
			"system_prompt": "Bạn khuyến nghị nhà cung cấp cho người đọc; không ra quyết định.", "config": map[string]any{
				"model":  map[string]any{"policy": "fixed", "fixed_model_id": modelID},
				"limits": map[string]any{"max_iterations": 4, "max_tool_calls": 4, "timeout_seconds": 30},
				"tools":  []any{map[string]any{"server_id": serverID, "allow": []string{"get_price_book", "get_price_books"}}},
			}}, 201)["id"].(string)
		if r := e.do("PUT", "/v1/agents/"+agentID+"/active-version", adminA, map[string]any{"version": 1}, nil); r.status != 200 {
			t.Fatalf("publish: %d %s", r.status, r.body)
		}
		e.model.script(func(_ context.Context, n int, _ provider.Request) (provider.Response, error) {
			if n == 0 {
				return provider.Response{StopReason: provider.StopToolUse, ModelUsed: "sonnet", Usage: provider.Usage{InputTokens: 1000, OutputTokens: 100},
					ToolCalls: []provider.ToolCall{
						{ID: "c0", Name: "pricing-engine.get_price_book", Arguments: json.RawMessage(`{"sku":"HQ-FREEFIRE-GC-20"}`)},
						{ID: "c1", Name: "pricing-engine.get_price_books", Arguments: json.RawMessage(`{"skus":["HQ-FREEFIRE-GC-20","HQ-NOPE"]}`)},
					}}, nil
			}
			return provider.Response{StopReason: provider.StopEndTurn, Text: "Khuyến nghị: Supplier A.", ModelUsed: "sonnet",
				Usage: provider.Usage{InputTokens: 2000, OutputTokens: 200}}, nil
		})
		before := requests.Load()
		r := e.do("POST", "/v1/projects/"+projA+"/executions", adminA, map[string]any{"mode": "single_agent", "agent_id": agentID,
			"input": map[string]any{"message": "Nên mua HQ-FREEFIRE-GC-20 của nhà cung cấp nào?"}}, map[string]string{"Idempotency-Key": uuid.NewString()})
		if r.status != 202 {
			t.Fatalf("tạo execution: %d %s", r.status, r.body)
		}
		id := r.json(t)["id"].(string)
		var x map[string]any
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			x = e.do("GET", "/v1/executions/"+id, adminA, nil, nil).json(t)
			if st := x["status"]; st != "queued" && st != "running" {
				break
			}
		}
		if x["status"] != "completed" {
			t.Fatalf("execution: %v", x)
		}
		if n := requests.Load() - before; n != 2 {
			t.Errorf("PE nhận %d request trong execution, muốn 2", n)
		}
		taskID := e.do("GET", "/v1/executions/"+id+"/tasks", adminA, nil, nil).json(t)["data"].([]any)[0].(map[string]any)["id"].(string)
		full := string(e.do("GET", "/v1/tasks/"+taskID+"/steps?include_content=true&kind=tool_call", adminA, nil, nil).body)
		// Kết quả tool tới step nguyên vẹn sau khi qua bộ che secret của Gateway. Nội dung step
		// là JSON lồng trong JSON, nên chỉ so các mảnh không chứa dấu nháy.
		for _, want := range []string{"17.000000", "a-123", "SUP_A", "HQ-NOPE", "not_found", "low_confidence"} {
			if !strings.Contains(full, want) {
				t.Errorf("step tool_call thiếu %s: %.600s", want, full)
			}
		}
		if strings.Contains(full, "REDACTED") || strings.Contains(full, apiKey) || strings.Contains(full, serve.ErrExecutionOnly) {
			t.Errorf("step tool_call bị che, lộ khoá hoặc bị từ chối: %.600s", full)
		}
		var ok int
		if err := e.db.Admin.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE org_id = $1 AND action = 'mcp_tool.call' AND outcome = 'success' AND details->>'execution_id' = $2`, e.orgA, id).Scan(&ok); err != nil || ok != 2 {
			t.Errorf("audit mcp_tool.call của execution = %d (%v), muốn 2", ok, err)
		}
		// Execution đã đọc dữ liệu sensitive: Gateway ghi lại, để mọi tool egress về sau của
		// nó cần người duyệt (security §5.4).
		var marked int
		if err := e.db.Admin.QueryRow(ctx, `SELECT count(*) FROM mcp_execution_sensitive_reads WHERE org_id = $1 AND execution_id = $2`, e.orgA, id).Scan(&marked); err != nil || marked != 1 {
			t.Errorf("execution đọc tool sensitive phải được đánh dấu: %d (%v)", marked, err)
		}
	})

	t.Run("an unknown SKU is a tool error, a bad argument never leaves the gateway", func(t *testing.T) {
		res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "pricing-engine.get_sku", Arguments: map[string]any{"id": "0198a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b"}})
		if err != nil || !res.IsError || !strings.Contains(text(res), "SKU not found") {
			t.Fatalf("SKU không có phải là lỗi của tool: %v %+v", err, res)
		}
		if strings.Contains(text(res), pe.URL) || strings.Contains(text(res), "errors_sku_not_found") {
			t.Errorf("lỗi lộ địa chỉ hoặc văn bản của PE: %s", text(res))
		}
		before := requests.Load()
		for name, args := range map[string]map[string]any{
			"pricing-engine.get_sku":        {"id": "../admin"},
			"pricing-engine.list_brands":    {"url": "http://evil.example"},
			"pricing-engine.list_skus":      {"limit": 101},
			"pricing-engine.get_price_book": {"sku": "../admin"},
		} {
			res, err = sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
			if err == nil && !res.IsError {
				t.Errorf("%s: arguments sai được chấp nhận: %v", name, args)
			}
		}
		if n := requests.Load() - before; n != 0 {
			t.Errorf("arguments sai vẫn tạo %d request tới PE", n)
		}
	})
}
