package api_test

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/hqgroups/mcp/internal/ai/provider"
)

func TestAuditAndUsageRead(t *testing.T) {
	e := setup(t)
	adminA, adminB, devA, viewerA := e.tok("admin-a"), e.tok("admin-b"), e.tok("dev-a"), e.tok("viewer-a")
	post := func(tok, path string, body any) map[string]any {
		t.Helper()
		r := e.do("POST", path, tok, body, nil)
		if r.status != 201 {
			t.Fatalf("POST %s: %d %s", path, r.status, r.body)
		}
		return r.json(t)
	}
	// Tạo hoạt động ở cả hai org.
	projA := e.createProject(adminA, "au-a", "A")["id"].(string)
	e.createProject(adminA, "au-a2", "A2")
	e.createProject(adminB, "au-b", "B SECRET NAME")
	provID := post(adminA, "/v1/model-providers", map[string]any{"slug": "local", "adapter": "claude-cli"})["id"].(string)
	modelID := post(adminA, "/v1/models", map[string]any{"provider_id": provID, "provider_model": "sonnet", "display_name": "S", "tier": "standard",
		"capabilities": map[string]any{"tool_calling": true, "context_window": 200000}})["id"].(string)
	post(adminA, "/v1/models/"+modelID+"/pricing", map[string]any{"currency": "USD", "input_per_mtok": "3.00", "output_per_mtok": "15.00",
		"effective_from": time.Now().UTC().Format(time.RFC3339Nano)})
	agentID := post(adminA, "/v1/projects/"+projA+"/agents", map[string]any{"slug": "audit-agent", "name": "a", "system_prompt": "x",
		"config": map[string]any{"model": map[string]any{"policy": "fixed", "fixed_model_id": modelID},
			"limits": map[string]any{"max_iterations": 3, "max_tool_calls": 3, "timeout_seconds": 30}, "tools": []any{}}})["id"].(string)
	if r := e.do("PUT", "/v1/agents/"+agentID+"/active-version", adminA, map[string]any{"version": 1}, nil); r.status != 200 {
		t.Fatalf("publish: %d", r.status)
	}
	e.model.script(func(context.Context, int, provider.Request) (provider.Response, error) { return answer("xong"), nil })
	start := e.do("POST", "/v1/projects/"+projA+"/executions", adminA, map[string]any{"mode": "single_agent", "agent_id": agentID,
		"input": map[string]any{"message": "hi"}}, map[string]string{"Idempotency-Key": uuid.NewString()})
	want(t, start, 202, "")
	execID := start.json(t)["id"].(string)
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if s := e.do("GET", "/v1/executions/"+execID, adminA, nil, nil).json(t)["status"]; s != "queued" && s != "running" {
			break
		}
	}
	// Một lần từ chối quyền để có dòng outcome = denied.
	want(t, e.do("POST", "/v1/projects", viewerA, map[string]any{"slug": "x", "name": "x", "environment": "non_prod"}, nil), 403, "permission_denied")

	list := func(tok, path string) []map[string]any {
		t.Helper()
		r := e.do("GET", path, tok, nil, nil)
		if r.status != 200 {
			t.Fatalf("GET %s: %d %s", path, r.status, r.body)
		}
		var out []map[string]any
		for _, d := range r.json(t)["data"].([]any) {
			out = append(out, d.(map[string]any))
		}
		return out
	}

	t.Run("audit logs: filters, order, pagination", func(t *testing.T) {
		all := list(adminA, "/v1/audit-logs?limit=200")
		if len(all) < 8 {
			t.Fatalf("audit có %d dòng", len(all))
		}
		for i := 1; i < len(all); i++ {
			if all[i-1]["occurred_at"].(string) < all[i]["occurred_at"].(string) {
				t.Fatal("audit không sắp mới nhất trước")
			}
		}
		for _, row := range list(adminA, "/v1/audit-logs?action=project.") {
			if !strings.HasPrefix(row["action"].(string), "project.") {
				t.Errorf("lọc action: %v", row["action"])
			}
		}
		if rows := list(adminA, "/v1/audit-logs?action=execution.&execution_id="+execID); len(rows) != 2 {
			t.Errorf("audit của execution: %d dòng, muốn 2 (create, finish)", len(rows))
		}
		denied := list(adminA, "/v1/audit-logs?outcome=denied")
		if len(denied) == 0 || denied[0]["action"] != "authz.deny" || denied[0]["details"].(map[string]any)["permission"] != "project:create" {
			t.Errorf("outcome=denied: %v", denied)
		}
		if rows := list(adminA, "/v1/audit-logs?project_id="+projA+"&resource_type=project"); len(rows) != 1 || rows[0]["action"] != "project.create" {
			t.Errorf("lọc project: %v", rows)
		}
		// "_" trong tiền tố action khớp nguyên văn, không phải ký tự đại diện.
		if rows := list(adminA, "/v1/audit-logs?action=model_provider."); len(rows) != 1 {
			t.Errorf("action=model_provider.: %d dòng", len(rows))
		}
		under := list(adminA, "/v1/audit-logs?action=model_")
		if len(under) != 2 {
			t.Errorf("action=model_: %d dòng, muốn 2 (model_provider.create, model_pricing.create)", len(under))
		}
		for _, row := range under {
			if !strings.HasPrefix(row["action"].(string), "model_") {
				t.Errorf("'_' bị coi là ký tự đại diện: khớp %v", row["action"])
			}
		}
		// Phân trang keyset đi hết, không trùng, không sót.
		seen := map[string]bool{}
		cursor := ""
		for page := 0; page < 50; page++ {
			r := e.do("GET", "/v1/audit-logs?limit=3&cursor="+url.QueryEscape(cursor), adminA, nil, nil).json(t)
			for _, d := range r["data"].([]any) {
				id := d.(map[string]any)["id"].(string)
				if seen[id] {
					t.Fatalf("dòng %s lặp lại giữa các trang", id)
				}
				seen[id] = true
			}
			if r["next_cursor"] == nil {
				break
			}
			cursor = r["next_cursor"].(string)
		}
		if len(seen) != len(all) {
			t.Errorf("phân trang thấy %d dòng, một trang lớn thấy %d", len(seen), len(all))
		}
		for _, bad := range []string{"actor_type=root", "outcome=maybe", "action=DROP%20TABLE", "actor_id=nope", "from=yesterday",
			"from=2020-01-01T00:00:00Z&to=2026-01-01T00:00:00Z", "from=2026-11-18T00:00:00Z&to=2026-11-17T00:00:00Z", "cursor=xx", "limit=0"} {
			want(t, e.do("GET", "/v1/audit-logs?"+bad, adminA, nil, nil), 400, "")
		}
	})

	t.Run("audit logs: org admins only, own tenant only", func(t *testing.T) {
		for _, tok := range []string{devA, viewerA, e.tok("norole-a")} {
			want(t, e.do("GET", "/v1/audit-logs", tok, nil, nil), 403, "permission_denied")
		}
		want(t, e.do("GET", "/v1/audit-logs", "", nil, nil), 401, "unauthenticated")
		b := e.do("GET", "/v1/audit-logs?limit=200", adminB, nil, nil)
		if strings.Contains(string(b.body), projA) || strings.Contains(string(b.body), execID) {
			t.Error("org B thấy audit của org A")
		}
		a := e.do("GET", "/v1/audit-logs?limit=200", adminA, nil, nil)
		if strings.Contains(string(a.body), "B SECRET NAME") {
			t.Error("org A thấy audit của org B")
		}
		// Lọc theo tài nguyên của org khác không trả gì.
		if rows := list(adminB, "/v1/audit-logs?execution_id="+execID); len(rows) != 0 {
			t.Errorf("org B lọc execution của org A: %d dòng", len(rows))
		}
	})

	t.Run("usage records", func(t *testing.T) {
		rng := "from=" + url.QueryEscape(time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)) + "&to=" + url.QueryEscape(time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		rows := list(adminA, "/v1/usage/records?"+rng)
		if len(rows) != 1 {
			t.Fatalf("usage: %d dòng, muốn 1", len(rows))
		}
		u := rows[0]
		// 2000 input × 3 + 500 output × 15 + 1000 cached × 3 (không có giá cache riêng), trên một triệu.
		if u["kind"] != "llm" || u["execution_id"] != execID || u["model_id"] != modelID || u["input_tokens"] != float64(2000) ||
			u["output_tokens"] != float64(500) || u["estimated_cost"].(map[string]any)["amount"] != "0.01650000" {
			t.Errorf("usage record: %v", u)
		}
		if n := len(list(adminA, "/v1/usage/records?"+rng+"&execution_id="+uuid.NewString())); n != 0 {
			t.Errorf("lọc execution khác: %d dòng", n)
		}
		if n := len(list(devA, "/v1/usage/records?"+rng)); n != 1 {
			t.Errorf("developer đọc usage: %d dòng", n)
		}
		want(t, e.do("GET", "/v1/usage/records?"+rng, viewerA, nil, nil), 403, "permission_denied")
		if n := len(list(adminB, "/v1/usage/records?"+rng+"&execution_id="+execID)); n != 0 {
			t.Errorf("org B thấy usage của org A: %d dòng", n)
		}
		want(t, e.do("GET", "/v1/usage/records", adminA, nil, nil), 400, "invalid_parameter")
		want(t, e.do("GET", "/v1/usage/records?from=2020-01-01T00:00:00Z&to=2026-01-01T00:00:00Z", adminA, nil, nil), 400, "invalid_parameter")
		want(t, e.do("GET", "/v1/usage/records?"+rng+"&model_id=x", adminA, nil, nil), 400, "invalid_parameter")
	})
}
