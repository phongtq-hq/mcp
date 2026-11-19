package api_test

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/hqgroups/mcp/internal/shared/exectoken"
)

func TestProviderAPIKey(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	adminA, adminB, devA := e.tok("admin-a"), e.tok("admin-b"), e.tok("dev-a")
	const keyOne, keyTwo = "sk-ant-org-a-key-one-111111", "sk-ant-org-a-key-two-222222"
	e.llm.accept(keyOne, keyTwo)
	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := e.db.Admin.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	provider := func(slug, key string) map[string]any {
		return map[string]any{"slug": slug, "adapter": "anthropic", "api_key": key}
	}

	t.Run("an invalid key is rejected and nothing is stored", func(t *testing.T) {
		r := e.do("POST", "/v1/model-providers", adminA, provider("bad", "sk-ant-wrong"), nil)
		want(t, r, 422, "validation_failed")
		if strings.Contains(string(r.body), "sk-ant-wrong") {
			t.Error("phản hồi lặp lại khoá")
		}
		// Không kiểm tra được (provider lỗi) thì cũng không lưu, và báo để thử lại.
		e.llm.setDown(true)
		want(t, e.do("POST", "/v1/model-providers", adminA, provider("down", keyOne), nil), 503, "provider_unavailable")
		e.llm.setDown(false)
		for _, bad := range []string{"", "  ", "has space", strings.Repeat("k", 600)} {
			want(t, e.do("POST", "/v1/model-providers", adminA, provider("bad", bad), nil), 422, "validation_failed")
		}
		if n := count(`SELECT count(*) FROM model_providers WHERE org_id = $1`, e.orgA) + count(`SELECT count(*) FROM secrets`); n != 0 {
			t.Errorf("có %d dòng được lưu sau khi khoá bị từ chối", n)
		}
		// Adapter không dùng khoá của org.
		want(t, e.do("POST", "/v1/model-providers", adminA, map[string]any{"slug": "cli", "adapter": "claude-cli", "api_key": keyOne}, nil), 422, "validation_failed")
		// Đặt khoá là quyền riêng: developer không có.
		want(t, e.do("POST", "/v1/model-providers", devA, provider("dev", keyOne), nil), 403, "permission_denied")
	})

	r := e.do("POST", "/v1/model-providers", adminA, provider("claude", keyOne), nil)
	want(t, r, 201, "")
	prov := r.json(t)
	provID := prov["id"].(string)

	t.Run("a valid key is encrypted, never returned, never logged", func(t *testing.T) {
		if prov["has_credential"] != true || strings.Contains(string(r.body), keyOne) || strings.Contains(string(r.body), "api_key") {
			t.Errorf("provider: %s", r.body)
		}
		for _, path := range []string{"/v1/model-providers/" + provID, "/v1/model-providers"} {
			if g := e.do("GET", path, adminA, nil, nil); strings.Contains(string(g.body), keyOne) {
				t.Errorf("GET %s lộ khoá", path)
			}
		}
		want(t, e.do("GET", "/v1/model-providers/"+provID, adminB, nil, nil), 404, "not_found")
		// Database chỉ có bản mã; audit và idempotency không chứa khoá.
		if n := count(`SELECT count(*) FROM secrets WHERE org_id = $1 AND kind = 'api_key' AND version = 1 AND name = $2`, e.orgA, "model-provider/"+provID); n != 1 {
			t.Fatalf("secrets: %d dòng", n)
		}
		for table, sql := range map[string]string{
			"secrets":          `SELECT count(*) FROM secrets WHERE position($1::bytea in ciphertext) > 0 OR position($1::bytea in wrapped_dek) > 0`,
			"audit_logs":       `SELECT count(*) FROM audit_logs WHERE details::text LIKE '%' || convert_from($1::bytea, 'UTF8') || '%'`,
			"idempotency_keys": `SELECT count(*) FROM idempotency_keys WHERE response_body::text LIKE '%' || convert_from($1::bytea, 'UTF8') || '%'`,
			"model_providers":  `SELECT count(*) FROM model_providers WHERE settings::text LIKE '%' || convert_from($1::bytea, 'UTF8') || '%'`,
		} {
			if n := count(sql, []byte(keyOne)); n != 0 {
				t.Errorf("khoá ở dạng rõ nằm trong %s", table)
			}
		}
		if n := count(`SELECT count(*) FROM audit_logs WHERE org_id = $1 AND action = 'secret.put'`, e.orgA); n != 1 {
			t.Errorf("audit secret.put: %d", n)
		}
	})

	// Model + giá + agent dùng provider này.
	post := func(tok, path string, body any) map[string]any {
		t.Helper()
		r := e.do("POST", path, tok, body, nil)
		if r.status != 201 {
			t.Fatalf("POST %s: %d %s", path, r.status, r.body)
		}
		return r.json(t)
	}
	model := func(tok, providerID string) string {
		id := post(tok, "/v1/models", map[string]any{"provider_id": providerID, "provider_model": "claude-test-model", "display_name": "Claude",
			"tier": "standard", "capabilities": map[string]any{"tool_calling": true, "context_window": 200000}})["id"].(string)
		post(tok, "/v1/models/"+id+"/pricing", map[string]any{"currency": "USD", "input_per_mtok": "3.00", "output_per_mtok": "15.00",
			"effective_from": time.Now().UTC().Format(time.RFC3339Nano)})
		return id
	}
	modelID := model(adminA, provID)
	projA := e.createProject(adminA, "keys-a", "Keys A")["id"].(string)
	agent := func(slug, modelID string) string {
		a := post(adminA, "/v1/projects/"+projA+"/agents", map[string]any{"slug": slug, "name": slug, "system_prompt": "Bạn là trợ lý.",
			"config": map[string]any{"model": map[string]any{"policy": "fixed", "fixed_model_id": modelID},
				"limits": map[string]any{"max_iterations": 3, "max_tool_calls": 3, "timeout_seconds": 30}, "tools": []any{}}})
		id := a["id"].(string)
		if r := e.do("PUT", "/v1/agents/"+id+"/active-version", adminA, map[string]any{"version": 1}, nil); r.status != 200 {
			t.Fatalf("publish: %d %s", r.status, r.body)
		}
		return id
	}
	run := func(agentID string) map[string]any {
		t.Helper()
		r := e.do("POST", "/v1/projects/"+projA+"/executions", adminA, map[string]any{"mode": "single_agent", "agent_id": agentID,
			"input": map[string]any{"message": "xin chào"}}, map[string]string{"Idempotency-Key": uuid.NewString()})
		if r.status != 202 {
			t.Fatalf("start: %d %s", r.status, r.body)
		}
		id := r.json(t)["id"].(string)
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			x := e.do("GET", "/v1/executions/"+id, adminA, nil, nil).json(t)
			if s := x["status"]; s != "queued" && s != "running" {
				return x
			}
		}
		t.Fatal("execution không kết thúc")
		return nil
	}
	assistant := agent("assistant", modelID)
	last := func() string {
		keys := e.llm.usedKeys()
		if len(keys) == 0 {
			return ""
		}
		return keys[len(keys)-1]
	}

	t.Run("an execution calls the model through the gateway with the org's key", func(t *testing.T) {
		x := run(assistant)
		if x["status"] != "completed" || x["output"].(map[string]any)["message"] != "trả lời từ API" {
			t.Fatalf("kết quả: %v", x)
		}
		if last() != keyOne {
			t.Errorf("provider được gọi bằng khoá %q", last())
		}
		// 100 input × 3 + 20 output × 15 trên một triệu token.
		if c := x["estimated_cost"].(map[string]any); c["amount"] != "0.00060000" {
			t.Errorf("estimated_cost: %v", c)
		}
		id := x["id"].(string)
		if n := count(`SELECT count(*) FROM usage_records WHERE execution_id = $1 AND input_tokens = 100 AND output_tokens = 20`, id); n != 1 {
			t.Errorf("usage_records: %d", n)
		}
		// Mở khoá được ghi audit; nội dung step không chứa khoá.
		if n := count(`SELECT count(*) FROM audit_logs WHERE org_id = $1 AND action = 'secret.access' AND details->>'purpose' = 'llm_call'`, e.orgA); n < 1 {
			t.Error("thiếu audit secret.access")
		}
		if n := count(`SELECT count(*) FROM execution_steps WHERE execution_id = $1 AND (request::text LIKE '%sk-ant-%' OR response::text LIKE '%sk-ant-%')`, id); n != 0 {
			t.Error("khoá nằm trong execution_steps")
		}
		// Model tên do catalog quyết định; provider nhận đúng định danh đó.
		if e.model != nil && len(e.model.seen()) != 0 {
			t.Error("adapter cục bộ bị gọi cho model của provider anthropic")
		}
	})

	t.Run("rotating the key creates a new secret version and takes effect", func(t *testing.T) {
		etag := e.do("GET", "/v1/model-providers/"+provID, adminA, nil, nil).header.Get("ETag")
		want(t, e.do("PATCH", "/v1/model-providers/"+provID, adminA, map[string]any{"api_key": "sk-ant-nope"}, map[string]string{"If-Match": etag}), 422, "validation_failed")
		want(t, e.do("PATCH", "/v1/model-providers/"+provID, devA, map[string]any{"api_key": keyTwo}, map[string]string{"If-Match": etag}), 403, "permission_denied")
		p := e.do("PATCH", "/v1/model-providers/"+provID, adminA, map[string]any{"api_key": keyTwo}, map[string]string{"If-Match": etag})
		want(t, p, 200, "")
		if strings.Contains(string(p.body), keyTwo) || p.json(t)["has_credential"] != true {
			t.Errorf("PATCH: %s", p.body)
		}
		if n := count(`SELECT max(version) FROM secrets WHERE org_id = $1`, e.orgA); n != 2 {
			t.Errorf("version của secret: %d", n)
		}
		if x := run(assistant); x["status"] != "completed" || last() != keyTwo {
			t.Errorf("sau khi xoay khoá: %v, khoá dùng %q", x["status"], last())
		}
	})

	t.Run("an org provider without a key cannot run; a platform provider uses the platform key", func(t *testing.T) {
		bare := post(adminA, "/v1/model-providers", map[string]any{"slug": "no-key", "adapter": "anthropic"})["id"].(string)
		bareAgent := agent("no-key", model(adminA, bare))
		r := e.do("POST", "/v1/projects/"+projA+"/executions", adminA, map[string]any{"mode": "single_agent", "agent_id": bareAgent,
			"input": map[string]any{"message": "x"}}, map[string]string{"Idempotency-Key": uuid.NewString()})
		want(t, r, 422, "validation_failed")

		// Provider + model + giá của platform, do người vận hành tạo.
		var pp, pm uuid.UUID
		if err := e.db.Admin.QueryRow(ctx, `INSERT INTO model_providers (org_id, slug, adapter) VALUES (NULL, 'anthropic-platform', 'anthropic') RETURNING id`).Scan(&pp); err != nil {
			t.Fatal(err)
		}
		if err := e.db.Admin.QueryRow(ctx, `INSERT INTO models (org_id, provider_id, provider_model, display_name) VALUES (NULL, $1, 'claude-platform-model', 'Platform') RETURNING id`, pp).Scan(&pm); err != nil {
			t.Fatal(err)
		}
		if _, err := e.db.Admin.Exec(ctx, `INSERT INTO model_pricing (model_id, org_id, version, input_per_mtok, output_per_mtok, effective_from) VALUES ($1, NULL, 1, 1, 1, now())`, pm); err != nil {
			t.Fatal(err)
		}
		if x := run(agent("on-platform", pm.String())); x["status"] != "completed" || last() != platformLLMKey {
			t.Errorf("provider platform: %v, khoá dùng %q", x["status"], last())
		}
	})

	t.Run("a live task's token calls only the pinned model, with the catalog's model name and the version's limits", func(t *testing.T) {
		// Giữ lượt gọi model đầu tiên để task ở trạng thái đang chạy.
		hold := make(chan struct{})
		e.llm.setHold(hold)
		released := false
		release := func() {
			if !released {
				released = true
				e.llm.setHold(nil)
				close(hold)
			}
		}
		defer release()
		_, _, callsBefore := e.llm.lastCall()
		r := e.do("POST", "/v1/projects/"+projA+"/executions", adminA, map[string]any{"mode": "single_agent", "agent_id": assistant,
			"input": map[string]any{"message": "giữ"}}, map[string]string{"Idempotency-Key": uuid.NewString()})
		want(t, r, 202, "")
		execID := uuid.MustParse(r.json(t)["id"].(string))
		var org, project, task, version, initiator uuid.UUID
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if err := e.db.Admin.QueryRow(ctx, `SELECT t.org_id, e.project_id, t.id, t.agent_version_id, e.initiator_id
				FROM agent_tasks t JOIN agent_executions e ON e.id = t.execution_id WHERE e.id = $1 AND t.status = 'running'`, execID).
				Scan(&org, &project, &task, &version, &initiator); err == nil {
				break
			}
		}
		if task == uuid.Nil {
			t.Fatal("task không chuyển sang running")
		}
		iss, _ := exectoken.New(execTokenKey)
		now := time.Now()
		token, _ := iss.Sign(exectoken.Claims{OrgID: org, ProjectID: project, ExecutionID: execID, TaskID: task, AgentVersionID: version,
			InitiatorType: "user", InitiatorID: initiator}, now, now.Add(time.Minute))
		call := func(taskID, model string, maxOut int) int {
			body := `{"model_id":"` + model + `","request":{"model":"attacker-chosen-model","max_output_tokens":` + strconv.Itoa(maxOut) + `,"messages":[{"role":"user","text":"hi"}]}}`
			req, _ := http.NewRequest("POST", e.gw.URL+"/llm/internal/v1/tasks/"+taskID+"/complete", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = res.Body.Close()
			return res.StatusCode
		}
		// Chờ lượt gọi của chính execution tới provider (nó đang bị giữ ở đó).
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if _, _, n := e.llm.lastCall(); n > callsBefore {
				break
			}
		}
		_, _, before := e.llm.lastCall()
		if before != callsBefore+1 {
			t.Fatalf("lượt gọi của execution chưa tới provider (%d → %d)", callsBefore, before)
		}
		// Model khác model được ghim, và token dùng cho task khác: đều bị từ chối, provider không bị gọi.
		other := model(adminA, post(adminA, "/v1/model-providers", provider("claude-two", keyTwo))["id"].(string))
		if got := call(task.String(), other, 100); got != 403 {
			t.Errorf("model không được ghim: status %d, muốn 403", got)
		}
		if got := call(uuid.NewString(), modelID, 100); got != 404 {
			t.Errorf("token cho task khác: status %d, muốn 404", got)
		}
		if _, _, n := e.llm.lastCall(); n != before {
			t.Error("provider bị gọi bởi request bị từ chối")
		}
		// Đúng model: tên model lấy từ catalog, max_output_tokens bị chặn theo Agent Version.
		release()
		if got := call(task.String(), modelID, 999999); got != 200 {
			t.Fatalf("đúng model: status %d", got)
		}
		gotModel, gotMax, _ := e.llm.lastCall()
		if gotModel != "claude-test-model" || gotMax > 4096 {
			t.Errorf("provider nhận model %q, max_tokens %v", gotModel, gotMax)
		}
		if n := count(`SELECT count(*) FROM audit_logs WHERE org_id = $1 AND action = 'llm.call' AND details->>'execution_id' = $2 AND details->>'credential' = 'organization'`, e.orgA, execID.String()); n < 1 {
			t.Error("thiếu audit llm.call")
		}
	})

	t.Run("the model endpoint rejects anything that is not a live execution token", func(t *testing.T) {
		call := func(token, task, modelID string) int {
			req, _ := http.NewRequest("POST", e.gw.URL+"/llm/internal/v1/tasks/"+task+"/complete",
				strings.NewReader(`{"model_id":"`+modelID+`","request":{"model":"x","max_output_tokens":10,"messages":[{"role":"user","text":"hi"}]}}`))
			req.Header.Set("Content-Type", "application/json")
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = res.Body.Close()
			return res.StatusCode
		}
		before := len(e.llm.usedKeys())
		var org, project, exec, task, version, initiator uuid.UUID
		if err := e.db.Admin.QueryRow(ctx, `SELECT t.org_id, e.project_id, e.id, t.id, t.agent_version_id, e.initiator_id
			FROM agent_tasks t JOIN agent_executions e ON e.id = t.execution_id WHERE t.org_id = $1 ORDER BY t.created_at LIMIT 1`, e.orgA).
			Scan(&org, &project, &exec, &task, &version, &initiator); err != nil {
			t.Fatal(err)
		}
		iss, _ := exectoken.New(execTokenKey)
		now := time.Now()
		finished, _ := iss.Sign(exectoken.Claims{OrgID: org, ProjectID: project, ExecutionID: exec, TaskID: task, AgentVersionID: version,
			InitiatorType: "user", InitiatorID: initiator}, now, now.Add(time.Minute))
		for name, c := range map[string]struct {
			token string
			want  int
		}{"no token": {"", 401}, "OIDC token": {adminA, 401}, "finished task": {finished, 403}} {
			if got := call(c.token, task.String(), modelID); got != c.want {
				t.Errorf("%s: status %d, muốn %d", name, got, c.want)
			}
		}
		if len(e.llm.usedKeys()) != before {
			t.Error("provider bị gọi bởi request không hợp lệ")
		}
	})
}
