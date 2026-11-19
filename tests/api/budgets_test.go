package api_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/hqgroups/mcp/internal/adminops"
	"github.com/hqgroups/mcp/internal/ai/provider"
	"github.com/hqgroups/mcp/internal/shared/signedctx"
)

// TestBudgets: cấu hình budget qua API, và việc thực thi nó quanh từng lượt gọi model:
// budget của execution (limits.budget của Agent Version) và budget theo phạm vi
// (cost-management §4).
func TestBudgets(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	adminA, adminB, viewerA, devA := e.tok("admin-a"), e.tok("admin-b"), e.tok("viewer-a"), e.tok("dev-a")
	signer, _ := signedctx.NewSigner(signingKey)
	conn, err := e.db.Admin.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	post := func(tok, path string, body any, status int) map[string]any {
		t.Helper()
		r := e.do("POST", path, tok, body, map[string]string{"Idempotency-Key": uuid.NewString()})
		if r.status != status {
			t.Fatalf("POST %s: %d %s", path, r.status, r.body)
		}
		return r.json(t)
	}
	up := newUpstream(t, "docker", readTool("list_containers", "Liệt kê container."))
	if _, err := adminops.RegisterManagedMCPServer(ctx, conn.Conn(), signer, adminops.MCPServer{OrgSlug: "org-a", Slug: "docker"}, "Docker", up.srv.URL); err != nil {
		t.Fatal(err)
	}
	serverID := e.do("GET", "/v1/mcp-servers", adminA, nil, nil).json(t)["data"].([]any)[0].(map[string]any)["id"].(string)
	provID := post(adminA, "/v1/model-providers", map[string]any{"slug": "local", "adapter": "claude-cli"}, 201)["id"].(string)
	modelID := post(adminA, "/v1/models", map[string]any{"provider_id": provID, "provider_model": "sonnet", "display_name": "Sonnet",
		"tier": "standard", "capabilities": map[string]any{"tool_calling": true, "context_window": 200000}}, 201)["id"].(string)
	// 3 USD / triệu token vào, 15 USD / triệu token ra.
	post(adminA, "/v1/models/"+modelID+"/pricing", map[string]any{"currency": "USD", "input_per_mtok": "3.00", "output_per_mtok": "15.00",
		"effective_from": time.Now().UTC().Format(time.RFC3339Nano)}, 201)

	// Mỗi ca dùng một project riêng để budget của ca này không ảnh hưởng ca khác.
	type fixture struct{ project, agent string }
	newAgent := func(slug string, limits map[string]any) fixture {
		t.Helper()
		proj := e.createProject(adminA, slug, slug)["id"].(string)
		base := map[string]any{"max_iterations": 6, "max_tool_calls": 10, "timeout_seconds": 30}
		for k, v := range limits {
			base[k] = v
		}
		a := post(adminA, "/v1/projects/"+proj+"/agents", map[string]any{"slug": "agent", "name": "agent", "system_prompt": "Trợ lý.",
			"config": map[string]any{
				// max_output_tokens nhỏ để trần chi phí ước lượng của một lượt nhỏ và tính được.
				"model":  map[string]any{"policy": "fixed", "fixed_model_id": modelID, "max_output_tokens": 100},
				"limits": base,
				"tools":  []any{map[string]any{"server_id": serverID, "allow": []string{"list_containers"}}},
			}}, 201)
		id := a["id"].(string)
		if r := e.do("PUT", "/v1/agents/"+id+"/active-version", adminA, map[string]any{"version": 1}, nil); r.status != 200 {
			t.Fatalf("publish: %d %s", r.status, r.body)
		}
		return fixture{project: proj, agent: id}
	}
	start := func(f fixture) resp {
		return e.do("POST", "/v1/projects/"+f.project+"/executions", adminA, map[string]any{"mode": "single_agent", "agent_id": f.agent,
			"input": map[string]any{"message": "kiểm tra"}}, map[string]string{"Idempotency-Key": uuid.NewString()})
	}
	run := func(f fixture) (string, map[string]any) {
		t.Helper()
		r := start(f)
		if r.status != 202 {
			t.Fatalf("start: %d %s", r.status, r.body)
		}
		id := r.json(t)["id"].(string)
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			x := e.do("GET", "/v1/executions/"+id, adminA, nil, nil).json(t)
			if s := x["status"]; s != "queued" && s != "running" {
				return id, x
			}
		}
		t.Fatalf("execution %s không kết thúc", id)
		return "", nil
	}
	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := e.db.Admin.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// Model giả luôn đòi gọi tool: mỗi lượt 1000 token vào + 100 token ra = 0.0045 USD.
	// Trần ước lượng của một lượt: 100 token ra (0.0015) cộng phần đầu vào (dưới 0.002).
	e.model.script(func(context.Context, int, provider.Request) (provider.Response, error) {
		r := toolUse("docker.list_containers")
		r.Text = "đang kiểm tra"
		return r, nil
	})
	budget := func(scopeType, scopeID, period, limit string, extra map[string]any) map[string]any {
		body := map[string]any{"scope_type": scopeType, "scope_id": scopeID, "period": period, "limit": map[string]any{"amount": limit, "currency": "USD"}}
		for k, v := range extra {
			body[k] = v
		}
		return body
	}

	t.Run("budgets are managed through the API; other tenants cannot see or touch them", func(t *testing.T) {
		f := newAgent("crud", nil)
		b := post(adminA, "/v1/budgets", budget("project", f.project, "monthly", "25.5", map[string]any{"soft_threshold_pct": 80}), 201)
		id := b["id"].(string)
		cur := b["current_period"].(map[string]any)
		if b["hard_action"] != "block" || b["enabled"] != true || b["limit"].(map[string]any)["amount"] != "25.50000000" ||
			cur["spent"].(map[string]any)["amount"] != "0" && cur["spent"].(map[string]any)["amount"] != "0.00000000" || cur["end"] == nil {
			t.Errorf("budget mới: %v", b)
		}
		// Mỗi (phạm vi, chu kỳ) một budget.
		want(t, e.do("POST", "/v1/budgets", adminA, budget("project", f.project, "monthly", "1", nil), map[string]string{"Idempotency-Key": uuid.NewString()}), 409, "budget_exists")
		post(adminA, "/v1/budgets", budget("project", f.project, "daily", "1", nil), 201)
		post(adminA, "/v1/budgets", budget("organization", e.orgA.String(), "total", "1000", map[string]any{"hard_action": "alert_only"}), 201)

		for name, body := range map[string]map[string]any{
			"scope lạ":               budget("team", f.project, "monthly", "1", nil),
			"chu kỳ lạ":              budget("project", f.project, "weekly", "1", nil),
			"số âm":                  budget("project", f.project, "total", "-1", nil),
			"quá 8 chữ số thập phân": budget("project", f.project, "total", "1.123456789", nil),
			"ký hiệu khoa học":       budget("project", f.project, "total", "1e3", nil),
			"vượt trần":              budget("project", f.project, "total", "10000001", nil),
			"tiền tệ khác USD":       {"scope_type": "project", "scope_id": f.project, "period": "total", "limit": map[string]any{"amount": "1", "currency": "VND"}},
			"ngưỡng mềm 0":           budget("project", f.project, "total", "1", map[string]any{"soft_threshold_pct": 0}),
			"hard_action lạ":         budget("project", f.project, "total", "1", map[string]any{"hard_action": "ignore"}),
			"project không tồn tại":  budget("project", uuid.NewString(), "total", "1", nil),
			"agent không tồn tại":    budget("agent", uuid.NewString(), "total", "1", nil),
			"user không thuộc org":   budget("user", uuid.NewString(), "total", "1", nil),
			"organization khác":      budget("organization", e.orgB.String(), "total", "1", nil),
		} {
			r := e.do("POST", "/v1/budgets", adminA, body, map[string]string{"Idempotency-Key": uuid.NewString()})
			if r.status != 422 {
				t.Errorf("%s: %d %s", name, r.status, r.body)
			}
		}
		// Org B không đặt được budget lên project / agent của org A, và câu trả lời giống
		// hệt trường hợp id không tồn tại.
		for _, body := range []map[string]any{budget("project", f.project, "total", "1", nil), budget("agent", f.agent, "total", "1", nil)} {
			want(t, e.do("POST", "/v1/budgets", adminB, body, map[string]string{"Idempotency-Key": uuid.NewString()}), 422, "validation_failed")
		}

		// Phân quyền: đọc cần budget:read, ghi cần budget:write.
		want(t, e.do("GET", "/v1/budgets", viewerA, nil, nil), 403, "permission_denied")
		want(t, e.do("GET", "/v1/budgets/"+id, devA, nil, nil), 200, "")
		want(t, e.do("POST", "/v1/budgets", devA, budget("project", f.project, "total", "1", nil), map[string]string{"Idempotency-Key": uuid.NewString()}), 403, "permission_denied")
		want(t, e.do("PATCH", "/v1/budgets/"+id, devA, map[string]any{"enabled": false}, nil), 403, "permission_denied")
		want(t, e.do("DELETE", "/v1/budgets/"+id, devA, nil, nil), 403, "permission_denied")

		// Truy cập chéo tenant: 404, và danh sách của org B không có gì của org A.
		want(t, e.do("GET", "/v1/budgets/"+id, adminB, nil, nil), 404, "not_found")
		want(t, e.do("PATCH", "/v1/budgets/"+id, adminB, map[string]any{"enabled": false}, nil), 404, "not_found")
		want(t, e.do("DELETE", "/v1/budgets/"+id, adminB, nil, nil), 404, "not_found")
		if lb := e.do("GET", "/v1/budgets", adminB, nil, nil); lb.status != 200 || len(lb.json(t)["data"].([]any)) != 0 {
			t.Errorf("org B thấy budget: %d %s", lb.status, lb.body)
		}
		if lb := e.do("GET", "/v1/budgets?scope_id="+f.project, adminB, nil, nil); len(lb.json(t)["data"].([]any)) != 0 {
			t.Errorf("org B lọc theo project của org A thấy budget: %s", lb.body)
		}

		// Danh sách, lọc, phân trang.
		all := e.do("GET", "/v1/budgets", adminA, nil, nil).json(t)["data"].([]any)
		byProject := e.do("GET", "/v1/budgets?scope_type=project&scope_id="+f.project, adminA, nil, nil).json(t)["data"].([]any)
		if len(all) != 3 || len(byProject) != 2 {
			t.Errorf("danh sách: %d budget, %d của project", len(all), len(byProject))
		}
		page := e.do("GET", "/v1/budgets?limit=2", adminA, nil, nil).json(t)
		next, _ := page["next_cursor"].(string)
		if len(page["data"].([]any)) != 2 || next == "" {
			t.Fatalf("trang 1: %v", page)
		}
		if p2 := e.do("GET", "/v1/budgets?limit=2&cursor="+next, adminA, nil, nil).json(t); len(p2["data"].([]any)) != 1 || p2["next_cursor"] != nil {
			t.Errorf("trang 2: %v", p2)
		}

		// If-Match: ETag cũ bị từ chối (412), ETag hiện tại thì qua.
		etag := e.do("GET", "/v1/budgets/"+id, adminA, nil, nil).header.Get("ETag")
		if etag == "" {
			t.Fatal("GET budget không trả ETag")
		}
		first := e.do("PATCH", "/v1/budgets/"+id, adminA, map[string]any{"soft_threshold_pct": 70}, map[string]string{"If-Match": etag})
		want(t, first, 200, "")
		want(t, e.do("PATCH", "/v1/budgets/"+id, adminA, map[string]any{"soft_threshold_pct": 60}, map[string]string{"If-Match": etag}), 412, "precondition_failed")
		want(t, e.do("PATCH", "/v1/budgets/"+uuid.NewString(), adminA, map[string]any{"enabled": false}, map[string]string{"If-Match": etag}), 404, "not_found")
		want(t, e.do("PATCH", "/v1/budgets/"+id, adminA, map[string]any{"soft_threshold_pct": 80}, map[string]string{"If-Match": first.header.Get("ETag")}), 200, "")

		// Sửa và xoá.
		r := e.do("PATCH", "/v1/budgets/"+id, adminA, map[string]any{"limit": map[string]any{"amount": "30", "currency": "USD"},
			"soft_threshold_pct": nil, "hard_action": "alert_only", "enabled": false}, nil)
		want(t, r, 200, "")
		if u := r.json(t); u["limit"].(map[string]any)["amount"] != "30.00000000" || u["soft_threshold_pct"] != nil || u["hard_action"] != "alert_only" || u["enabled"] != false {
			t.Errorf("sau PATCH: %v", u)
		}
		// Không gửi soft_threshold_pct thì giữ nguyên.
		e.do("PATCH", "/v1/budgets/"+id, adminA, map[string]any{"soft_threshold_pct": 50}, nil)
		if u := e.do("PATCH", "/v1/budgets/"+id, adminA, map[string]any{"enabled": true}, nil).json(t); u["soft_threshold_pct"] != float64(50) {
			t.Errorf("soft_threshold_pct bị đổi khi không gửi: %v", u["soft_threshold_pct"])
		}
		want(t, e.do("PATCH", "/v1/budgets/"+id, adminA, map[string]any{"limit": map[string]any{"amount": "x", "currency": "USD"}}, nil), 422, "validation_failed")
		want(t, e.do("DELETE", "/v1/budgets/"+id, adminA, nil, nil), 204, "")
		want(t, e.do("GET", "/v1/budgets/"+id, adminA, nil, nil), 404, "not_found")
		if n := count(`SELECT count(*) FROM audit_logs WHERE org_id = $1 AND action IN ('budget.create','budget.update','budget.delete')`, e.orgA); n < 5 {
			t.Errorf("audit của budget: %d dòng", n)
		}
		// Budget bị xoá hẳn; nội dung của nó lúc xoá nằm trong audit.
		if n := count(`SELECT count(*) FROM audit_logs WHERE action = 'budget.delete' AND resource_id = $1
			AND details->>'scope_type' = 'project' AND details->>'period' = 'monthly' AND details->>'limit' = '30.00000000'`, id); n != 1 {
			t.Errorf("audit budget.delete thiếu nội dung của budget")
		}
		// Dọn budget organization để không ảnh hưởng các ca sau.
		for _, b := range e.do("GET", "/v1/budgets", adminA, nil, nil).json(t)["data"].([]any) {
			e.do("DELETE", "/v1/budgets/"+b.(map[string]any)["id"].(string), adminA, nil, nil)
		}
	})

	t.Run("an execution stops with budget_exceeded when the agent version's budget runs out, keeping its partial result", func(t *testing.T) {
		// Budget 0.01 USD: hai lượt (0.009) qua được; lượt ba cần thêm trần ước lượng nên bị chặn.
		f := newAgent("exec-budget", map[string]any{"budget": map[string]any{"amount": "0.01", "currency": "USD"}})
		id, x := run(f)
		errObj, _ := x["error"].(map[string]any)
		if x["status"] != "budget_exceeded" || errObj["code"] != "budget_exceeded" {
			t.Fatalf("execution: %v %v", x["status"], x["error"])
		}
		if out, _ := x["output"].(map[string]any); out["message"] != "đang kiểm tra" {
			t.Errorf("kết quả dở dang: %v", x["output"])
		}
		if n := count(`SELECT count(*) FROM usage_records WHERE execution_id = $1`, id); n != 2 {
			t.Errorf("usage_records = %d, muốn 2 (lượt bị chặn không gọi model)", n)
		}
		if n := count(`SELECT count(*) FROM execution_steps s JOIN agent_tasks t ON t.id = s.task_id
			WHERE t.execution_id = $1 AND s.kind = 'llm_call' AND s.status = 'failed' AND s.error->>'code' = 'budget_exceeded'`, id); n != 1 {
			t.Errorf("step bị chặn vì budget = %d, muốn 1", n)
		}
		if n := count(`SELECT count(*) FROM agent_tasks WHERE execution_id = $1 AND status = 'budget_exceeded' AND budget_limit = 0.01 AND estimated_cost = 0.009`, id); n != 1 {
			t.Errorf("task không ở trạng thái budget_exceeded với mức tiêu 0.009")
		}
		if seen := e.model.seen(); len(seen) != 2 {
			t.Errorf("model được gọi %d lần, muốn 2", len(seen))
		}
		// Budget nhỏ hơn trần chi phí của riêng phần trả lời một lượt (100 token ra = 0.0015):
		// execution bị từ chối ngay thay vì được nhận rồi dừng mà chưa làm gì.
		tiny := newAgent("exec-budget-tiny", map[string]any{"budget": map[string]any{"amount": "0.001", "currency": "USD"}})
		r := start(tiny)
		want(t, r, 422, "validation_failed")
		if !strings.Contains(string(r.body), "limits.budget") {
			t.Errorf("thông báo không nêu limits.budget: %s", r.body)
		}
	})

	t.Run("a blocking scope budget stops running executions and refuses new ones with 402", func(t *testing.T) {
		f := newAgent("scope-budget", nil)
		b := post(adminA, "/v1/budgets", budget("project", f.project, "daily", "0.005", nil), 201)
		// Lượt đầu (0.0045) qua; lượt hai cần đặt trước thêm nên chạm ngưỡng 0.005.
		e.model.script(func(context.Context, int, provider.Request) (provider.Response, error) {
			return toolUse("docker.list_containers"), nil
		})
		id, x := run(f)
		if x["status"] != "budget_exceeded" {
			t.Fatalf("execution: %v %v", x["status"], x["error"])
		}
		if n := count(`SELECT count(*) FROM usage_records WHERE execution_id = $1`, id); n != 1 {
			t.Errorf("usage_records = %d, muốn 1", n)
		}
		// Mức sử dụng của kỳ lấy từ ledger.
		got := e.do("GET", "/v1/budgets/"+b["id"].(string), adminA, nil, nil).json(t)["current_period"].(map[string]any)
		if got["spent"].(map[string]any)["amount"] != "0.00450000" || got["utilization_pct"].(float64) < 89 || got["utilization_pct"].(float64) > 91 {
			t.Errorf("current_period: %v", got)
		}
		// Chưa chạm ngưỡng theo ledger (0.0045 < 0.005): execution mới được nhận, nhưng
		// lượt gọi đầu tiên của nó không đặt trước được.
		_, x = run(f)
		if x["status"] != "budget_exceeded" {
			t.Errorf("execution thứ hai: %v", x["status"])
		}
		// Hạ hạn mức xuống dưới mức đã tiêu: execution mới bị từ chối ngay (402).
		want(t, e.do("PATCH", "/v1/budgets/"+b["id"].(string), adminA, map[string]any{"limit": map[string]any{"amount": "0.004", "currency": "USD"}}, nil), 200, "")
		want(t, start(f), 402, "budget_exceeded")
		// Budget của project khác / agent khác không bị ảnh hưởng.
		other := newAgent("scope-other", nil)
		e.model.script(func(context.Context, int, provider.Request) (provider.Response, error) { return answer("ổn"), nil })
		if _, x := run(other); x["status"] != "completed" {
			t.Errorf("project khác bị chặn: %v", x["status"])
		}
		// Tắt budget: execution chạy lại được.
		want(t, e.do("PATCH", "/v1/budgets/"+b["id"].(string), adminA, map[string]any{"enabled": false}, nil), 200, "")
		if _, x := run(f); x["status"] != "completed" {
			t.Errorf("sau khi tắt budget: %v %v", x["status"], x["error"])
		}
	})

	t.Run("an alert-only budget never blocks; crossing the soft threshold is recorded once per period", func(t *testing.T) {
		f := newAgent("alert-only", nil)
		b := post(adminA, "/v1/budgets", budget("agent", f.agent, "monthly", "0.001", map[string]any{"hard_action": "alert_only", "soft_threshold_pct": 50}), 201)
		e.model.script(func(context.Context, int, provider.Request) (provider.Response, error) { return answer("xong"), nil })
		for i := 0; i < 2; i++ {
			if _, x := run(f); x["status"] != "completed" {
				t.Fatalf("budget chỉ cảnh báo đã chặn execution: %v %v", x["status"], x["error"])
			}
		}
		if n := count(`SELECT count(*) FROM audit_logs WHERE action = 'budget.soft_threshold' AND resource_id = $1 AND actor_type = 'system'`, b["id"]); n != 1 {
			t.Errorf("audit ngưỡng mềm = %d dòng, muốn đúng 1", n)
		}
	})

	t.Run("budgets are enforced at the gateway for models it serves; the provider is not called", func(t *testing.T) {
		var pp, pm uuid.UUID
		if err := e.db.Admin.QueryRow(ctx, `INSERT INTO model_providers (org_id, slug, adapter) VALUES (NULL, 'anthropic-budget', 'anthropic') RETURNING id`).Scan(&pp); err != nil {
			t.Fatal(err)
		}
		if err := e.db.Admin.QueryRow(ctx, `INSERT INTO models (org_id, provider_id, provider_model, display_name) VALUES (NULL, $1, 'claude-budget', 'Budget') RETURNING id`, pp).Scan(&pm); err != nil {
			t.Fatal(err)
		}
		if _, err := e.db.Admin.Exec(ctx, `INSERT INTO model_pricing (model_id, org_id, version, input_per_mtok, output_per_mtok, effective_from) VALUES ($1, NULL, 1, 3, 15, now())`, pm); err != nil {
			t.Fatal(err)
		}
		gwAgent := func(slug string) fixture {
			t.Helper()
			proj := e.createProject(adminA, slug, slug)["id"].(string)
			a := post(adminA, "/v1/projects/"+proj+"/agents", map[string]any{"slug": "agent", "name": "agent", "system_prompt": "Trợ lý.",
				"config": map[string]any{"model": map[string]any{"policy": "fixed", "fixed_model_id": pm.String(), "max_output_tokens": 100},
					"limits": map[string]any{"max_iterations": 3, "max_tool_calls": 1, "timeout_seconds": 30}, "tools": []any{}}}, 201)
			f := fixture{project: proj, agent: a["id"].(string)}
			if r := e.do("PUT", "/v1/agents/"+f.agent+"/active-version", adminA, map[string]any{"version": 1}, nil); r.status != 200 {
				t.Fatalf("publish: %d %s", r.status, r.body)
			}
			return f
		}
		f := gwAgent("gw-free")
		// Không có budget: chạy được, provider được gọi.
		_, _, before := e.llm.lastCall()
		if _, x := run(f); x["status"] != "completed" {
			t.Fatalf("không có budget: %v %v", x["status"], x["error"])
		}
		_, _, after := e.llm.lastCall()
		if after != before+1 {
			t.Fatalf("provider được gọi %d lần", after-before)
		}
		// Project chưa tiêu gì, budget nhỏ hơn trần ước lượng của một lượt: execution được
		// nhận (ledger chưa chạm ngưỡng), nhưng Gateway từ chối lượt gọi và provider không bị gọi.
		f = gwAgent("gw-capped")
		post(adminA, "/v1/budgets", budget("project", f.project, "total", "0.0001", nil), 201)
		id, x := run(f)
		_, _, final := e.llm.lastCall()
		if x["status"] != "budget_exceeded" || final != after {
			t.Errorf("execution: %v %v; provider được gọi thêm %d lần", x["status"], x["error"], final-after)
		}
		if n := count(`SELECT count(*) FROM usage_records WHERE execution_id = $1`, id); n != 0 {
			t.Errorf("usage_records = %d, muốn 0", n)
		}
		// Quyết định từ chối của Gateway có audit, như mọi lượt gọi model lỗi khác.
		if n := count(`SELECT count(*) FROM audit_logs WHERE action = 'llm.call' AND execution_id = $1 AND outcome = 'error'
			AND details->>'error_code' = 'budget_exceeded'`, id); n != 1 {
			t.Errorf("audit lượt bị từ chối vì budget = %d dòng, muốn 1", n)
		}
		if strings.Contains(string(e.do("GET", "/v1/executions/"+id, adminA, nil, nil).body), "0.0001") {
			t.Error("phản hồi execution lộ hạn mức của budget")
		}
	})
}
