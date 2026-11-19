package api_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/hqgroups/mcp/internal/ai/provider"
	apperr "github.com/hqgroups/mcp/internal/shared/errors"
)

// TestModelRouter: Agent Version dùng model.policy tiered chạy bằng chuỗi model của
// tier, cấu hình trong settings của organization / project (ADR-0010). Chuỗi cố định
// từ lúc tạo task, model dự phòng được dùng khi model đứng trước lỗi tạm thời, và
// Gateway chỉ phục vụ model trong chuỗi.
func TestModelRouter(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	adminA, adminB := e.tok("admin-a"), e.tok("admin-b")
	post := func(tok, path string, body any, status int) map[string]any {
		t.Helper()
		r := e.do("POST", path, tok, body, nil)
		if r.status != status {
			t.Fatalf("POST %s: %d %s", path, r.status, r.body)
		}
		return r.json(t)
	}
	// Model của org A trên adapter chạy trong process (model giả theo kịch bản).
	provID := post(adminA, "/v1/model-providers", map[string]any{"slug": "local", "adapter": "claude-cli"}, 201)["id"].(string)
	model := func(tok, prov, name string, tools, priced bool) string {
		t.Helper()
		id := post(tok, "/v1/models", map[string]any{"provider_id": prov, "provider_model": name, "display_name": name,
			"tier": "standard", "capabilities": map[string]any{"tool_calling": tools, "context_window": 200000}}, 201)["id"].(string)
		if priced {
			post(tok, "/v1/models/"+id+"/pricing", map[string]any{"currency": "USD", "input_per_mtok": "3.00", "output_per_mtok": "15.00",
				"effective_from": time.Now().UTC().Format(time.RFC3339Nano)}, 201)
		}
		return id
	}
	primary := model(adminA, provID, "primary", true, true)
	backup := model(adminA, provID, "backup", true, true)
	cheap := model(adminA, provID, "cheap", false, true)
	unpriced := model(adminA, provID, "unpriced", true, false)
	provB := post(adminB, "/v1/model-providers", map[string]any{"slug": "local", "adapter": "claude-cli"}, 201)["id"].(string)
	modelB := model(adminB, provB, "b-model", true, true)

	proj := e.createProject(adminA, "router-a", "Router A")["id"].(string)
	agent := func(slug string, modelCfg map[string]any) string {
		t.Helper()
		a := post(adminA, "/v1/projects/"+proj+"/agents", map[string]any{"slug": slug, "name": slug, "system_prompt": "Trợ lý.",
			"config": map[string]any{"model": modelCfg, "tools": []any{},
				"limits": map[string]any{"max_iterations": 3, "max_tool_calls": 3, "timeout_seconds": 30}}}, 201)
		id := a["id"].(string)
		if r := e.do("PUT", "/v1/agents/"+id+"/active-version", adminA, map[string]any{"version": 1}, nil); r.status != 200 {
			t.Fatalf("publish: %d %s", r.status, r.body)
		}
		return id
	}
	tiered := agent("tiered", map[string]any{"policy": "tiered"})
	economy := agent("economy", map[string]any{"policy": "tiered", "default_tier": "economy"})
	start := func(agentID string) resp {
		return e.do("POST", "/v1/projects/"+proj+"/executions", adminA, map[string]any{"mode": "single_agent", "agent_id": agentID,
			"input": map[string]any{"message": "xin chào"}}, map[string]string{"Idempotency-Key": uuid.NewString()})
	}
	run := func(agentID string) (string, map[string]any) {
		t.Helper()
		r := start(agentID)
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
	routing := func(tiers map[string][]string) map[string]any {
		m := map[string]any{}
		for tier, ids := range tiers {
			m[tier] = map[string]any{"models": ids}
		}
		return map[string]any{"model_routing": map[string]any{"tiers": m}}
	}
	setOrg := func(settings map[string]any) resp {
		return e.do("PATCH", "/v1/organization", adminA, map[string]any{"settings": settings}, nil)
	}
	setProject := func(settings map[string]any) resp {
		return e.do("PATCH", "/v1/projects/"+proj, adminA, map[string]any{"settings": settings}, nil)
	}
	// stepModel trả về model và lý do định tuyến của lượt model đầu tiên, cùng chuỗi của task.
	stepModel := func(execID string) (modelID, reason string, chain []string) {
		t.Helper()
		var ids []uuid.UUID
		var mid uuid.UUID
		if err := e.db.Admin.QueryRow(ctx, `SELECT s.model_id,
			CASE WHEN s.routing->>'policy' = 'fixed' THEN 'fixed' ELSE 'tier:' || (s.routing->>'tier') END ||
			CASE WHEN (s.routing->>'fallback_index')::int > 0 THEN ';fallback:' || (s.routing->>'fallback_index') ELSE '' END,
			t.model_chain FROM execution_steps s
			JOIN agent_tasks t ON t.id = s.task_id WHERE t.execution_id = $1 AND s.kind = 'llm_call' ORDER BY s.step_seq LIMIT 1`, execID).
			Scan(&mid, &reason, &ids); err != nil {
			t.Fatal(err)
		}
		for _, id := range ids {
			chain = append(chain, id.String())
		}
		return mid.String(), reason, chain
	}
	usageModel := func(execID string) string {
		t.Helper()
		var id uuid.UUID
		if err := e.db.Admin.QueryRow(ctx, `SELECT model_id FROM usage_records WHERE execution_id = $1`, execID).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id.String()
	}
	answerAs := func(_ context.Context, _ int, req provider.Request) (provider.Response, error) {
		r := answer("xong")
		r.ModelUsed = req.Model
		return r, nil
	}

	t.Run("a tiered agent cannot run until its tier is configured", func(t *testing.T) {
		r := start(tiered)
		want(t, r, 422, "validation_failed")
		if !strings.Contains(string(r.body), "standard") {
			t.Errorf("thông báo không nêu tier: %s", r.body)
		}
	})

	t.Run("routing settings are validated when written", func(t *testing.T) {
		five := []string{primary, backup, cheap, unpriced, uuid.NewString()}
		for name, settings := range map[string]map[string]any{
			"tier lạ":               routing(map[string][]string{"ultra": {primary}}),
			"chuỗi rỗng":            routing(map[string][]string{"standard": {}}),
			"quá bốn model":         routing(map[string][]string{"standard": five}),
			"model lặp":             routing(map[string][]string{"standard": {primary, primary}}),
			"model không tồn tại":   routing(map[string][]string{"standard": {uuid.NewString()}}),
			"model của org khác":    routing(map[string][]string{"standard": {modelB}}),
			"không phải uuid":       routing(map[string][]string{"standard": {"claude-sonnet"}}),
			"khoá lạ":               {"model_routing": map[string]any{"tiers": map[string]any{}, "strategy": "smart"}},
			"model_routing là mảng": {"model_routing": []int{1}},
		} {
			want(t, setOrg(settings), 422, "validation_failed")
			if r := setProject(settings); r.status != 422 {
				t.Errorf("%s (project): %d %s", name, r.status, r.body)
			}
		}
		// Org B không dùng được model của org A, kể cả khi biết id.
		r := e.do("PATCH", "/v1/organization", adminB, map[string]any{"settings": routing(map[string][]string{"standard": {primary}})}, nil)
		want(t, r, 422, "validation_failed")
		// Khoá khác của settings không bị router đụng tới.
		want(t, setOrg(map[string]any{"theme": "dark"}), 200, "")
	})

	t.Run("the organization's chain is used; the decision is recorded on the step and the task", func(t *testing.T) {
		want(t, setOrg(routing(map[string][]string{"standard": {primary, backup}, "economy": {cheap}})), 200, "")
		e.model.script(answerAs)
		id, x := run(tiered)
		if x["status"] != "completed" {
			t.Fatalf("execution: %v %v", x["status"], x["error"])
		}
		m, reason, chain := stepModel(id)
		if m != primary || reason != "tier:standard" || fmt.Sprint(chain) != fmt.Sprint([]string{primary, backup}) || usageModel(id) != primary {
			t.Errorf("model %s, lý do %q, chuỗi %v, usage %s", m, reason, chain, usageModel(id))
		}
		if seen := e.model.seen(); len(seen) != 1 || seen[0].Model != "primary" {
			t.Errorf("model được gọi: %+v", seen)
		}
		// default_tier của Agent Version chọn tier khác.
		id, x = run(economy)
		if m, reason, _ := stepModel(id); x["status"] != "completed" || m != cheap || reason != "tier:economy" {
			t.Errorf("economy: %v, model %s, lý do %q", x["status"], m, reason)
		}
	})

	t.Run("a fallback model answers when the primary fails transiently; usage is billed to the model that answered", func(t *testing.T) {
		e.model.script(func(ctx context.Context, n int, req provider.Request) (provider.Response, error) {
			if req.Model == "primary" {
				return provider.Response{}, apperr.New(apperr.KindUnavailable, "provider_unavailable", "quá tải").AsRetryable()
			}
			return answerAs(ctx, n, req)
		})
		id, x := run(tiered)
		if x["status"] != "completed" {
			t.Fatalf("execution: %v %v", x["status"], x["error"])
		}
		m, reason, _ := stepModel(id)
		if m != backup || reason != "tier:standard;fallback:1" || usageModel(id) != backup {
			t.Errorf("model %s, lý do %q, usage %s; muốn model dự phòng", m, reason, usageModel(id))
		}
		if seen := e.model.seen(); len(seen) != 2 || seen[0].Model != "primary" || seen[1].Model != "backup" {
			t.Errorf("thứ tự gọi: %+v", seen)
		}
		// Lỗi không tạm thời (provider từ chối yêu cầu) không chuyển sang model khác.
		e.model.script(func(context.Context, int, provider.Request) (provider.Response, error) {
			return provider.Response{}, apperr.New(apperr.KindUnprocessable, "provider_rejected", "yêu cầu không hợp lệ")
		})
		_, x = run(tiered)
		if seen := e.model.seen(); x["status"] != "failed" || len(seen) != 1 {
			t.Errorf("lỗi không tạm thời: %v, model được gọi %d lần", x["status"], len(seen))
		}
	})

	t.Run("a model that cannot serve is skipped inside the chain; every attempt is recorded on the step", func(t *testing.T) {
		want(t, setOrg(routing(map[string][]string{"standard": {primary, backup, cheap}, "economy": {cheap}})), 200, "")
		// Model chính lỗi tạm thời, dự phòng 1 không phục vụ được (lỗi không thử lại được,
		// riêng của model đó), dự phòng 2 trả lời: lỗi của dự phòng 1 không làm hỏng lượt.
		unavailable := apperr.New(apperr.KindUnprocessable, "model_unavailable", "model hiện không phục vụ được")
		e.model.script(func(ctx context.Context, n int, req provider.Request) (provider.Response, error) {
			switch req.Model {
			case "primary":
				return provider.Response{}, apperr.New(apperr.KindUnavailable, "provider_unavailable", "quá tải").AsRetryable()
			case "backup":
				return provider.Response{}, unavailable
			}
			return answerAs(ctx, n, req)
		})
		id, x := run(tiered)
		if x["status"] != "completed" {
			t.Fatalf("execution: %v %v", x["status"], x["error"])
		}
		if m, reason, _ := stepModel(id); m != cheap || reason != "tier:standard;fallback:2" {
			t.Errorf("model %s, lý do %q; muốn dự phòng thứ hai", m, reason)
		}
		var attempts, reasonField string
		if err := e.db.Admin.QueryRow(ctx, `SELECT s.routing->>'attempts', s.routing->>'reason' FROM execution_steps s JOIN agent_tasks t ON t.id = s.task_id
			WHERE t.execution_id = $1 AND s.kind = 'llm_call'`, id).Scan(&attempts, &reasonField); err != nil {
			t.Fatal(err)
		}
		if reasonField != "fallback" || !strings.Contains(attempts, primary) || !strings.Contains(attempts, "provider_unavailable") ||
			!strings.Contains(attempts, backup) || !strings.Contains(attempts, "model_unavailable") || strings.Contains(attempts, "quá tải") {
			t.Errorf("routing: reason %q, attempts %s", reasonField, attempts)
		}

		// Không model nào phục vụ được: lượt hỏng ngay, mỗi model đúng một lần gọi, và step
		// lỗi vẫn ghi đã thử những gì.
		e.model.script(func(context.Context, int, provider.Request) (provider.Response, error) {
			return provider.Response{}, unavailable
		})
		id, x = run(tiered)
		if seen := e.model.seen(); x["status"] != "failed" || len(seen) != 3 {
			t.Errorf("không model nào dùng được: %v, %d lời gọi (muốn 3)", x["status"], len(seen))
		}
		var n int
		if err := e.db.Admin.QueryRow(ctx, `SELECT jsonb_array_length(s.routing->'attempts') FROM execution_steps s JOIN agent_tasks t ON t.id = s.task_id
			WHERE t.execution_id = $1 AND s.kind = 'llm_call' AND s.status = 'failed'`, id).Scan(&n); err != nil || n != 3 {
			t.Errorf("step lỗi ghi %d lần thử (err %v), muốn 3", n, err)
		}

		want(t, setOrg(routing(map[string][]string{"standard": {primary, backup}, "economy": {cheap}})), 200, "")
	})

	t.Run("hard constraints and broken configuration", func(t *testing.T) {
		e.model.script(answerAs)
		// Model có cửa sổ ngữ cảnh quá nhỏ cho lời gọi bị loại lúc chọn.
		tiny := post(adminA, "/v1/models", map[string]any{"provider_id": provID, "provider_model": "tiny", "display_name": "tiny",
			"tier": "economy", "capabilities": map[string]any{"tool_calling": true, "context_window": 2000}}, 201)["id"].(string)
		post(adminA, "/v1/models/"+tiny+"/pricing", map[string]any{"currency": "USD", "input_per_mtok": "1.00", "output_per_mtok": "1.00",
			"effective_from": time.Now().UTC().Format(time.RFC3339Nano)}, 201)
		want(t, setOrg(routing(map[string][]string{"standard": {tiny, backup}, "economy": {cheap}})), 200, "")
		id, x := run(tiered)
		if m, _, chain := stepModel(id); x["status"] != "completed" || m != backup || len(chain) != 1 {
			t.Errorf("context window: %v, model %s, chuỗi %v", x["status"], m, chain)
		}
		// Quyết định của router nằm trong audit của execution.create.
		var tier, skipped string
		if err := e.db.Admin.QueryRow(ctx, `SELECT details->>'model_tier', details->>'models_skipped' FROM audit_logs
			WHERE action = 'execution.create' AND resource_id = $1`, id).Scan(&tier, &skipped); err != nil || tier != "standard" || !strings.Contains(skipped, tiny) {
			t.Errorf("audit execution.create: tier %q, skipped %q, err %v", tier, skipped, err)
		}
		// Thay đổi định tuyến truy được trong audit: chuỗi model của từng tier.
		var recorded string
		if err := e.db.Admin.QueryRow(ctx, `SELECT details->>'model_routing.standard' FROM audit_logs
			WHERE action = 'organization.update' AND org_id = $1 ORDER BY occurred_at DESC LIMIT 1`, e.orgA).Scan(&recorded); err != nil || recorded != tiny+","+backup {
			t.Errorf("audit organization.update: %q, err %v", recorded, err)
		}

		// Khoá trùng khác chữ hoa / thường: không được kiểm một đằng, dùng một nẻo.
		want(t, setOrg(map[string]any{"model_routing": map[string]any{
			"tiers": map[string]any{"standard": map[string]any{"models": []string{backup}}},
			"Tiers": map[string]any{"standard": map[string]any{"models": []string{primary}}}}}), 422, "validation_failed")
		want(t, setOrg(map[string]any{"model_routing": map[string]any{
			"tiers": map[string]any{"standard": map[string]any{"models": []string{backup}, "Models": []string{primary}}}}}), 422, "validation_failed")

		// Cấu hình hỏng ghi bằng đường khác (không qua API): dừng, không âm thầm dùng
		// chuỗi của organization.
		if _, err := e.db.Admin.Exec(ctx, `UPDATE projects SET settings = '{"model_routing":{"tiers":{"standard":{"models":["not-a-uuid"]}}}}' WHERE id = $1`, proj); err != nil {
			t.Fatal(err)
		}
		r := start(tiered)
		want(t, r, 422, "validation_failed")
		if !strings.Contains(string(r.body), "project") {
			t.Errorf("thông báo không nêu cấu hình của project hỏng: %s", r.body)
		}
		want(t, setProject(map[string]any{}), 200, "")
		want(t, setOrg(routing(map[string][]string{"standard": {primary, backup}, "economy": {cheap}})), 200, "")
	})

	t.Run("the project overrides the organization per tier; the chain is fixed when the task is created", func(t *testing.T) {
		want(t, setProject(routing(map[string][]string{"standard": {backup}})), 200, "")
		hold := make(chan struct{})
		e.model.script(func(ctx context.Context, n int, req provider.Request) (provider.Response, error) {
			<-hold
			return answerAs(ctx, n, req)
		})
		r := start(tiered)
		want(t, r, 202, "")
		id := r.json(t)["id"].(string)
		// Đổi cấu hình khi task đang chạy: task giữ chuỗi cũ.
		want(t, setProject(routing(map[string][]string{"standard": {primary}})), 200, "")
		close(hold)
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if s := e.do("GET", "/v1/executions/"+id, adminA, nil, nil).json(t)["status"]; s != "queued" && s != "running" {
				break
			}
		}
		if m, _, chain := stepModel(id); m != backup || len(chain) != 1 || chain[0] != backup {
			t.Errorf("model %s, chuỗi %v; muốn chuỗi lúc tạo task (model của project)", m, chain)
		}
		// Economy không bị project ghi đè: vẫn theo organization.
		e.model.script(answerAs)
		id, _ = run(economy)
		if m, _, _ := stepModel(id); m != cheap {
			t.Errorf("economy: model %s, muốn model của organization", m)
		}
		want(t, setProject(map[string]any{}), 200, "")
	})

	t.Run("models that cannot serve the call are skipped; nothing usable is an error", func(t *testing.T) {
		// unpriced không có giá hiệu lực → bỏ qua, model kế tiếp thành model chính.
		want(t, setOrg(routing(map[string][]string{"standard": {unpriced, backup}})), 200, "")
		e.model.script(answerAs)
		id, x := run(tiered)
		if m, reason, chain := stepModel(id); x["status"] != "completed" || m != backup || reason != "tier:standard" || len(chain) != 1 {
			t.Errorf("%v: model %s, lý do %q, chuỗi %v", x["status"], m, reason, chain)
		}
		want(t, setOrg(routing(map[string][]string{"standard": {unpriced}})), 200, "")
		r := start(tiered)
		want(t, r, 422, "validation_failed")
		if !strings.Contains(string(r.body), "giá") {
			t.Errorf("thông báo không nêu lý do: %s", r.body)
		}
		// Model bị tắt sau khi đã nằm trong cấu hình cũng bị bỏ qua.
		want(t, setOrg(routing(map[string][]string{"standard": {primary, backup}})), 200, "")
		if r := e.do("PATCH", "/v1/models/"+primary, adminA, map[string]any{"enabled": false}, nil); r.status != 200 {
			t.Fatalf("tắt model: %d %s", r.status, r.body)
		}
		id, x = run(tiered)
		if m, _, _ := stepModel(id); x["status"] != "completed" || m != backup {
			t.Errorf("model chính bị tắt: %v, model %s", x["status"], m)
		}
	})

	t.Run("through the gateway: only models of the task's chain are served, and the fallback is used", func(t *testing.T) {
		var pp uuid.UUID
		if err := e.db.Admin.QueryRow(ctx, `INSERT INTO model_providers (org_id, slug, adapter) VALUES (NULL, 'anthropic-router', 'anthropic') RETURNING id`).Scan(&pp); err != nil {
			t.Fatal(err)
		}
		ids := map[string]uuid.UUID{}
		for _, name := range []string{"claude-primary", "claude-backup"} {
			var id uuid.UUID
			if err := e.db.Admin.QueryRow(ctx, `INSERT INTO models (org_id, provider_id, provider_model, display_name) VALUES (NULL, $1, $2, $2) RETURNING id`, pp, name).Scan(&id); err != nil {
				t.Fatal(err)
			}
			if _, err := e.db.Admin.Exec(ctx, `INSERT INTO model_pricing (model_id, org_id, version, input_per_mtok, output_per_mtok, effective_from) VALUES ($1, NULL, 1, 1, 1, now())`, id); err != nil {
				t.Fatal(err)
			}
			ids[name] = id
		}
		want(t, setOrg(routing(map[string][]string{"premium": {ids["claude-primary"].String(), ids["claude-backup"].String()}})), 200, "")
		premium := agent("premium", map[string]any{"policy": "tiered", "default_tier": "premium"})
		_, _, before := e.llm.lastCall()
		e.llm.setModelDown("claude-primary", true)
		defer e.llm.setModelDown("claude-primary", false)
		id, x := run(premium)
		if x["status"] != "completed" {
			t.Fatalf("execution: %v %v", x["status"], x["error"])
		}
		called := e.llm.calledModels(before)
		if len(called) < 2 || called[0] != "claude-primary" || called[len(called)-1] != "claude-backup" {
			t.Errorf("model tới provider: %v", called)
		}
		if m, reason, _ := stepModel(id); m != ids["claude-backup"].String() || reason != "tier:premium;fallback:1" || usageModel(id) != m {
			t.Errorf("model %s, lý do %q, usage %s", m, reason, usageModel(id))
		}
		// Lượt gọi lỗi tới model chính cũng để lại audit ở Gateway, kèm mã lỗi.
		var failed, okCalls int
		if err := e.db.Admin.QueryRow(ctx, `SELECT count(*) FILTER (WHERE outcome = 'error' AND resource_id = $2 AND details->>'error_code' = 'provider_unavailable'),
			count(*) FILTER (WHERE outcome = 'success' AND resource_id = $3) FROM audit_logs WHERE action = 'llm.call' AND execution_id = $1`,
			id, ids["claude-primary"], ids["claude-backup"]).Scan(&failed, &okCalls); err != nil || failed < 1 || okCalls != 1 {
			t.Errorf("audit llm.call: %d lỗi (model chính), %d thành công (dự phòng), err %v", failed, okCalls, err)
		}
	})
}
