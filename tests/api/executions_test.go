package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/hqgroups/mcp/internal/adminops"
	"github.com/hqgroups/mcp/internal/ai/provider"
	"github.com/hqgroups/mcp/internal/shared/exectoken"
)

func toolUse(names ...string) provider.Response {
	r := provider.Response{StopReason: provider.StopToolUse, ModelUsed: "sonnet", Usage: provider.Usage{InputTokens: 1000, OutputTokens: 100}}
	for i, n := range names {
		r.ToolCalls = append(r.ToolCalls, provider.ToolCall{ID: fmt.Sprintf("c%d", i), Name: n, Arguments: json.RawMessage(`{"name":"web"}`)})
	}
	return r
}

func answer(text string) provider.Response {
	return provider.Response{StopReason: provider.StopEndTurn, Text: text, ModelUsed: "sonnet",
		Usage: provider.Usage{InputTokens: 2000, OutputTokens: 500, CachedInputTokens: 1000}}
}

func TestExecutions(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	adminA, adminB, viewerA := e.tok("admin-a"), e.tok("admin-b"), e.tok("viewer-a")
	signer := testSigner
	conn, err := e.db.Admin.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()

	// Hạ tầng của org A: MCP server Docker giả, model có giá, agent chỉ được list_containers.
	up := newUpstream(t, "docker", readTool("list_containers", "Liệt kê container."), readTool("get_container_logs", "Đọc log."))
	if _, err := adminops.RegisterManagedMCPServer(ctx, conn.Conn(), signer, adminops.MCPServer{OrgSlug: "org-a", Slug: "docker"}, "Docker", up.srv.URL); err != nil {
		t.Fatal(err)
	}
	post := func(tok, path string, body any, status int) map[string]any {
		t.Helper()
		r := e.do("POST", path, tok, body, nil)
		if r.status != status {
			t.Fatalf("POST %s: %d %s", path, r.status, r.body)
		}
		return r.json(t)
	}
	serverID := e.do("GET", "/v1/mcp-servers", adminA, nil, nil).json(t)["data"].([]any)[0].(map[string]any)["id"].(string)
	provID := post(adminA, "/v1/model-providers", map[string]any{"slug": "claude-local", "adapter": "claude-cli"}, 201)["id"].(string)
	modelID := post(adminA, "/v1/models", map[string]any{"provider_id": provID, "provider_model": "sonnet", "display_name": "Sonnet",
		"tier": "standard", "capabilities": map[string]any{"tool_calling": true, "context_window": 200000}}, 201)["id"].(string)
	post(adminA, "/v1/models/"+modelID+"/pricing", map[string]any{"currency": "USD", "input_per_mtok": "3.00", "output_per_mtok": "15.00",
		"cached_input_per_mtok": "0.30", "effective_from": time.Now().UTC().Format(time.RFC3339Nano)}, 201)

	projA := e.createProject(adminA, "exec-a", "Exec A")["id"].(string)
	agent := func(slug string, mutate func(cfg map[string]any)) string {
		t.Helper()
		cfg := map[string]any{
			"model":  map[string]any{"policy": "fixed", "fixed_model_id": modelID},
			"limits": map[string]any{"max_iterations": 4, "max_tool_calls": 6, "timeout_seconds": 30},
			"tools":  []any{map[string]any{"server_id": serverID, "allow": []string{"list_containers"}}},
		}
		if mutate != nil {
			mutate(cfg)
		}
		a := post(adminA, "/v1/projects/"+projA+"/agents", map[string]any{"slug": slug, "name": slug,
			"system_prompt": "Bạn là DevOps Agent chỉ đọc.", "config": cfg}, 201)
		id := a["id"].(string)
		if r := e.do("PUT", "/v1/agents/"+id+"/active-version", adminA, map[string]any{"version": 1}, nil); r.status != 200 {
			t.Fatalf("publish: %d %s", r.status, r.body)
		}
		return id
	}
	devops := agent("devops", nil)

	start := func(tok, agentID string, extra map[string]any) resp {
		body := map[string]any{"mode": "single_agent", "agent_id": agentID, "input": map[string]any{"message": "Container nào đang lỗi?"}}
		for k, v := range extra {
			body[k] = v
		}
		return e.do("POST", "/v1/projects/"+projA+"/executions", tok, body, map[string]string{"Idempotency-Key": uuid.NewString()})
	}
	wait := func(id string) map[string]any {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			x := e.do("GET", "/v1/executions/"+id, adminA, nil, nil).json(t)
			if s := x["status"]; s != "queued" && s != "running" {
				return x
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("execution %s không kết thúc", id)
		return nil
	}
	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := e.db.Admin.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	var doneID string
	t.Run("agent diagnoses through the gateway; steps, usage and audit are recorded", func(t *testing.T) {
		e.model.script(func(_ context.Context, n int, _ provider.Request) (provider.Response, error) {
			if n == 0 {
				// Tool thứ hai không nằm trong Agent Version: không được chạy.
				return toolUse("docker.list_containers", "docker.get_container_logs"), nil
			}
			return answer("Có 2 container, web đang restart."), nil
		})
		key := uuid.NewString()
		body := map[string]any{"mode": "single_agent", "agent_id": devops, "input": map[string]any{"message": "Container nào đang lỗi?"}}
		r := e.do("POST", "/v1/projects/"+projA+"/executions", adminA, body, map[string]string{"Idempotency-Key": key})
		want(t, r, 202, "")
		created := r.json(t)
		id := created["id"].(string)
		doneID = id
		if created["status"] != "queued" || created["mode"] != "single_agent" || r.header.Get("Location") != "/v1/executions/"+id {
			t.Errorf("execution mới: %s", r.body)
		}
		x := wait(id)
		if x["status"] != "completed" || x["output"].(map[string]any)["message"] != "Có 2 container, web đang restart." || x["error"] != nil {
			t.Fatalf("kết quả: %v", x)
		}
		// 3000 input × 3 + 600 output × 15 + 1000 cached × 0.30, trên một triệu token.
		if c := x["estimated_cost"].(map[string]any); c["amount"] != "0.01830000" || c["currency"] != "USD" {
			t.Errorf("estimated_cost: %v", c)
		}

		// Gọi lại cùng Idempotency-Key: cùng execution, không chạy lần hai.
		again := e.do("POST", "/v1/projects/"+projA+"/executions", adminA, body, map[string]string{"Idempotency-Key": key})
		if again.status != 202 || again.json(t)["id"] != id || len(e.model.seen()) != 2 {
			t.Errorf("replay: %d %s (%d lượt model)", again.status, again.body, len(e.model.seen()))
		}

		tasks := e.do("GET", "/v1/executions/"+id+"/tasks", adminA, nil, nil).json(t)["data"].([]any)
		if len(tasks) != 1 {
			t.Fatalf("muốn 1 task, có %d", len(tasks))
		}
		task := tasks[0].(map[string]any)
		taskID := task["id"].(string)
		if task["status"] != "completed" || task["iterations"] != float64(2) || task["tool_calls"] != float64(2) || task["agent_version"] != float64(1) {
			t.Errorf("task: %v", task)
		}
		steps := e.do("GET", "/v1/tasks/"+taskID+"/steps", adminA, nil, nil).json(t)["data"].([]any)
		var got []string
		for _, s := range steps {
			m := s.(map[string]any)
			got = append(got, fmt.Sprintf("%v:%v:%v", m["step_seq"], m["kind"], m["status"]))
			if m["request"] != nil || m["response"] != nil {
				t.Error("nội dung step lộ ra khi không yêu cầu include_content")
			}
		}
		if strings.Join(got, " ") != "1:llm_call:succeeded 2:tool_call:succeeded 3:tool_call:denied 4:llm_call:succeeded" {
			t.Errorf("steps: %v", got)
		}
		full := e.do("GET", "/v1/tasks/"+taskID+"/steps?include_content=true&kind=tool_call", adminA, nil, nil)
		if !strings.Contains(string(full.body), "result of list_containers") {
			t.Errorf("include_content: %s", full.body)
		}
		want(t, e.do("GET", "/v1/tasks/"+taskID+"/steps?include_content=true", viewerA, nil, nil), 403, "permission_denied")
		want(t, e.do("GET", "/v1/tasks/"+taskID+"/steps", viewerA, nil, nil), 200, "")

		// Tool đi qua Gateway đúng một lần, với ngữ cảnh của execution; tool ngoài Agent Version không tới upstream.
		up.mu.Lock()
		calls := strings.Join(up.calls, "|")
		up.mu.Unlock()
		if calls != `list_containers {"name":"web"}` {
			t.Errorf("upstream calls: %s", calls)
		}
		if n := count(`SELECT count(*) FROM audit_logs WHERE org_id = $1 AND action = 'mcp_tool.call' AND details->>'execution_id' = $2 AND details->>'rule' = 'risk_read'`, e.orgA, id); n != 1 {
			t.Errorf("audit mcp_tool.call của execution: %d", n)
		}
		for _, action := range []string{"execution.create", "execution.finish"} {
			if n := count(`SELECT count(*) FROM audit_logs WHERE org_id = $1 AND action = $2 AND resource_id = $3`, e.orgA, action, id); n != 1 {
				t.Errorf("audit %s: %d", action, n)
			}
		}
		if n := count(`SELECT count(*) FROM usage_records WHERE execution_id = $1 AND kind = 'llm' AND estimated_cost IS NOT NULL AND pricing_id IS NOT NULL AND user_id IS NOT NULL`, id); n != 2 {
			t.Errorf("usage_records: %d, muốn 2", n)
		}

		// Kết quả tool vào hội thoại ở vai tool; system prompt không đổi.
		reqs := e.model.seen()
		second := reqs[1]
		if second.System != "Bạn là DevOps Agent chỉ đọc." || len(second.Tools) != 1 || second.Tools[0].Name != "docker.list_containers" {
			t.Errorf("lượt hai: system %q, tools %v", second.System, second.Tools)
		}
		if m := second.Messages[2]; m.Role != provider.RoleTool || m.Text != "result of list_containers" {
			t.Errorf("kết quả tool: %+v", m)
		}
		if m := second.Messages[3]; m.Role != provider.RoleTool || !m.IsError {
			t.Errorf("tool bị từ chối phải có kết quả lỗi: %+v", m)
		}
	})

	t.Run("NUL bytes in tool results and model output do not lose the step or the usage", func(t *testing.T) {
		up.mu.Lock()
		up.reply = "log line\x00with nul"
		up.mu.Unlock()
		defer func() { up.mu.Lock(); up.reply = ""; up.mu.Unlock() }()
		e.model.script(func(_ context.Context, n int, _ provider.Request) (provider.Response, error) {
			if n == 0 {
				return toolUse("docker.list_containers"), nil
			}
			return answer("xong\x00rồi"), nil
		})
		r := start(adminA, devops, nil)
		want(t, r, 202, "")
		id := r.json(t)["id"].(string)
		if x := wait(id); x["status"] != "completed" {
			t.Fatalf("kết quả: %v", x)
		}
		if n := count(`SELECT count(*) FROM usage_records WHERE execution_id = $1`, id); n != 2 {
			t.Errorf("usage_records: %d, muốn 2", n)
		}
		if n := count(`SELECT count(*) FROM execution_steps WHERE execution_id = $1 AND status = 'started'`, id); n != 0 {
			t.Errorf("%d step kẹt ở started", n)
		}
	})

	t.Run("cancel right after create ends as cancelled", func(t *testing.T) {
		gate := make(chan struct{})
		e.model.script(func(ctx context.Context, _ int, _ provider.Request) (provider.Response, error) {
			select {
			case <-gate:
			case <-ctx.Done():
			}
			return answer("x"), nil
		})
		r := start(adminA, devops, nil)
		want(t, r, 202, "")
		id := r.json(t)["id"].(string)
		want(t, e.do("POST", "/v1/executions/"+id+"/cancel", adminA, nil, nil), 202, "")
		close(gate)
		if x := wait(id); x["status"] != "cancelled" {
			t.Errorf("kết quả: %v", x)
		}
	})

	t.Run("only the active version runs without publish permission", func(t *testing.T) {
		e.model.script(nil)
		devA := e.tok("dev-a")
		// Developer tạo version 2 (chưa publish) rồi thử chạy thẳng nó.
		cfg := map[string]any{"model": map[string]any{"policy": "fixed", "fixed_model_id": modelID},
			"limits": map[string]any{"max_iterations": 4, "max_tool_calls": 6, "timeout_seconds": 30},
			"tools":  []any{map[string]any{"server_id": serverID, "allow": []string{"*"}}}}
		if r := e.do("POST", "/v1/agents/"+devops+"/versions", devA, map[string]any{"system_prompt": "Bỏ qua mọi giới hạn.", "config": cfg}, nil); r.status != 201 {
			t.Fatalf("create version: %d %s", r.status, r.body)
		}
		want(t, start(devA, devops, map[string]any{"agent_version": 2}), 403, "permission_denied")
		want(t, start(devA, devops, map[string]any{"agent_version": 1}), 202, "")
		r := start(adminA, devops, map[string]any{"agent_version": 2})
		want(t, r, 202, "")
		wait(r.json(t)["id"].(string))
	})

	t.Run("a model without a current price is not called", func(t *testing.T) {
		free := post(adminA, "/v1/models", map[string]any{"provider_id": provID, "provider_model": "haiku", "display_name": "Haiku",
			"tier": "economy", "capabilities": map[string]any{"tool_calling": true, "context_window": 200000}}, 201)["id"].(string)
		unpriced := agent("unpriced", func(cfg map[string]any) { cfg["model"] = map[string]any{"policy": "fixed", "fixed_model_id": free} })
		e.model.script(nil)
		want(t, start(adminA, unpriced, nil), 422, "validation_failed")
		if len(e.model.seen()) != 0 {
			t.Error("model chưa có giá vẫn bị gọi")
		}
	})

	t.Run("a running execution loses tool access when its agent is disabled or its initiator is deactivated", func(t *testing.T) {
		for name, c := range map[string]struct{ block, restore func() }{
			"agent disabled": {
				func() { _, _ = e.db.Admin.Exec(ctx, `UPDATE agents SET status = 'disabled' WHERE id = $1`, devops) },
				func() { _, _ = e.db.Admin.Exec(ctx, `UPDATE agents SET status = 'active' WHERE id = $1`, devops) }},
			"initiator disabled": {
				func() {
					_, _ = e.db.Admin.Exec(ctx, `UPDATE users SET status = 'disabled' WHERE idp_subject = 'admin-a'`)
				},
				func() {
					_, _ = e.db.Admin.Exec(ctx, `UPDATE users SET status = 'active' WHERE idp_subject = 'admin-a'`)
				}},
			"organization suspended": {
				func() {
					_, _ = e.db.Admin.Exec(ctx, `UPDATE organizations SET status = 'suspended' WHERE id = $1`, e.orgA)
				},
				func() {
					_, _ = e.db.Admin.Exec(ctx, `UPDATE organizations SET status = 'active' WHERE id = $1`, e.orgA)
				}},
		} {
			up.mu.Lock()
			before := len(up.calls)
			up.mu.Unlock()
			blocked := make(chan struct{})
			e.model.script(func(_ context.Context, n int, _ provider.Request) (provider.Response, error) {
				if n == 0 {
					c.block() // quyền bị thu hồi khi execution đang chạy, trước tool call đầu tiên
					close(blocked)
					return toolUse("docker.list_containers"), nil
				}
				return answer("không đọc được"), nil
			})
			r := start(adminA, devops, nil)
			if r.status != 202 {
				t.Fatalf("%s: start %d %s", name, r.status, r.body)
			}
			id := r.json(t)["id"].(string)
			<-blocked
			deadline := time.Now().Add(15 * time.Second)
			for time.Now().Before(deadline) && count(`SELECT count(*) FROM agent_executions WHERE id = $1 AND status IN ('queued','running')`, id) > 0 {
				time.Sleep(20 * time.Millisecond)
			}
			c.restore()
			up.mu.Lock()
			after := len(up.calls)
			up.mu.Unlock()
			if after != before {
				t.Errorf("%s: tool vẫn tới upstream sau khi quyền bị thu hồi", name)
			}
			if n := count(`SELECT count(*) FROM agent_executions WHERE id = $1 AND status IN ('queued','running')`, id); n != 0 {
				t.Errorf("%s: execution không kết thúc", name)
			}
		}
	})

	t.Run("limits stop a looping model", func(t *testing.T) {
		e.model.script(func(context.Context, int, provider.Request) (provider.Response, error) {
			return toolUse("docker.list_containers"), nil
		})
		r := start(adminA, devops, nil)
		want(t, r, 202, "")
		x := wait(r.json(t)["id"].(string))
		if x["status"] != "failed" || x["error"].(map[string]any)["code"] != "limit_exceeded" || len(e.model.seen()) != 4 {
			t.Errorf("kết quả: %v sau %d lượt model", x, len(e.model.seen()))
		}
	})

	t.Run("timeout", func(t *testing.T) {
		e.model.script(func(ctx context.Context, _ int, _ provider.Request) (provider.Response, error) {
			<-ctx.Done()
			return provider.Response{}, ctx.Err()
		})
		r := start(adminA, devops, map[string]any{"timeout_seconds": 1})
		want(t, r, 202, "")
		if x := wait(r.json(t)["id"].(string)); x["status"] != "timed_out" {
			t.Errorf("kết quả: %v", x)
		}
		want(t, start(adminA, devops, map[string]any{"timeout_seconds": 3600}), 422, "validation_failed")
	})

	t.Run("cancel", func(t *testing.T) {
		release := make(chan struct{})
		e.model.script(func(ctx context.Context, n int, _ provider.Request) (provider.Response, error) {
			if n == 0 {
				select {
				case <-release:
				case <-ctx.Done():
					return provider.Response{}, ctx.Err()
				}
			}
			return toolUse("docker.list_containers"), nil
		})
		r := start(adminA, devops, nil)
		want(t, r, 202, "")
		id := r.json(t)["id"].(string)
		want(t, e.do("POST", "/v1/executions/"+id+"/cancel", viewerA, nil, nil), 403, "permission_denied")
		want(t, e.do("POST", "/v1/executions/"+id+"/cancel", adminB, nil, nil), 404, "not_found")
		want(t, e.do("POST", "/v1/executions/"+id+"/cancel", adminA, nil, nil), 202, "")
		close(release)
		if x := wait(id); x["status"] != "cancelled" {
			t.Errorf("kết quả: %v", x)
		}
		want(t, e.do("POST", "/v1/executions/"+id+"/cancel", adminA, nil, nil), 409, "execution_finished")
	})

	t.Run("model failure does not leak provider details", func(t *testing.T) {
		e.model.script(func(context.Context, int, provider.Request) (provider.Response, error) {
			return provider.Response{}, errors.New("dial tcp 10.1.2.3:443: connection refused")
		})
		r := start(adminA, devops, nil)
		want(t, r, 202, "")
		x := wait(r.json(t)["id"].(string))
		if x["status"] != "failed" || x["error"].(map[string]any)["code"] != "model_error" || strings.Contains(fmt.Sprint(x), "10.1.2.3") {
			t.Errorf("kết quả: %v", x)
		}
	})

	t.Run("validation", func(t *testing.T) {
		e.model.script(nil)
		body := map[string]any{"mode": "single_agent", "agent_id": devops, "input": map[string]any{"message": "x"}}
		want(t, e.do("POST", "/v1/projects/"+projA+"/executions", adminA, body, nil), 400, "idempotency_key_required")
		tiered := agent("tiered", func(cfg map[string]any) {
			cfg["model"] = map[string]any{"policy": "tiered", "default_tier": "standard"}
		})
		want(t, start(adminA, tiered, nil), 422, "validation_failed")
		want(t, start(adminA, uuid.NewString(), nil), 422, "validation_failed")
		want(t, start(adminA, devops, map[string]any{"mode": "planned"}), 422, "validation_failed")
		want(t, start(adminA, devops, map[string]any{"input": map[string]any{"message": ""}}), 422, "validation_failed")
		want(t, start(adminA, devops, map[string]any{"budget": map[string]any{"amount": "1", "currency": "USD"}}), 422, "validation_failed")
		// Agent chưa publish không chạy được.
		draft := post(adminA, "/v1/projects/"+projA+"/agents", map[string]any{"slug": "draft", "name": "draft", "system_prompt": "x",
			"config": map[string]any{"model": map[string]any{"policy": "fixed", "fixed_model_id": modelID},
				"limits": map[string]any{"max_iterations": 2, "max_tool_calls": 2, "timeout_seconds": 30}, "tools": []any{}}}, 201)["id"].(string)
		want(t, start(adminA, draft, nil), 409, "no_active_version")
	})

	t.Run("access control and tenant isolation", func(t *testing.T) {
		want(t, start(viewerA, devops, nil), 403, "permission_denied")
		want(t, start(adminB, devops, nil), 404, "not_found")
		want(t, start("", devops, nil), 401, "unauthenticated")
		tasks := e.do("GET", "/v1/executions/"+doneID+"/tasks", adminA, nil, nil).json(t)["data"].([]any)
		taskID := tasks[0].(map[string]any)["id"].(string)
		for _, path := range []string{"/v1/executions/" + doneID, "/v1/executions/" + doneID + "/tasks", "/v1/tasks/" + taskID,
			"/v1/tasks/" + taskID + "/steps", "/v1/projects/" + projA + "/executions"} {
			want(t, e.do("GET", path, adminB, nil, nil), 404, "not_found")
			want(t, e.do("GET", path, e.tok("norole-a"), nil, nil), 404, "not_found")
			want(t, e.do("GET", path, viewerA, nil, nil), 200, "")
		}
		// Người chỉ có execution:read thấy trạng thái nhưng không thấy nội dung (input, output,
		// goal): câu trả lời của Agent có thể trích dữ liệu nhạy cảm nó đã đọc.
		if x := e.do("GET", "/v1/executions/"+doneID, viewerA, nil, nil).json(t); x["output"] != nil || x["input"] != nil || x["status"] == nil {
			t.Errorf("viewer thấy nội dung execution: %v", x)
		}
		if x := e.do("GET", "/v1/executions/"+doneID, adminA, nil, nil).json(t); x["output"] == nil || x["input"] == nil {
			t.Errorf("người có execution:read_content phải thấy nội dung: %v", x)
		}
		for _, path := range []string{"/v1/tasks/" + taskID, "/v1/executions/" + doneID + "/tasks", "/v1/projects/" + projA + "/executions"} {
			if body := string(e.do("GET", path, viewerA, nil, nil).body); strings.Contains(body, `"output":{`) || strings.Contains(body, `"input":{`) ||
				strings.Contains(body, `"output_summary":"`) || strings.Contains(body, "Container nào") {
				t.Errorf("%s lộ nội dung cho viewer: %.400s", path, body)
			}
		}
		// Danh sách: phân trang keyset, lọc trạng thái, không lẫn project khác.
		list := e.do("GET", "/v1/projects/"+projA+"/executions?limit=2", adminA, nil, nil).json(t)
		if len(list["data"].([]any)) != 2 || list["next_cursor"] == nil {
			t.Fatalf("trang đầu: %v", list)
		}
		seen := map[string]bool{}
		for cursor, pages := "", 0; pages < 10; pages++ {
			pg := e.do("GET", "/v1/projects/"+projA+"/executions?limit=2&cursor="+cursor, adminA, nil, nil).json(t)
			for _, d := range pg["data"].([]any) {
				seen[d.(map[string]any)["id"].(string)] = true
			}
			if pg["next_cursor"] == nil {
				break
			}
			cursor = pg["next_cursor"].(string)
		}
		if n := count(`SELECT count(*) FROM agent_executions WHERE org_id = $1`, e.orgA); len(seen) != n {
			t.Errorf("phân trang thấy %d execution, database có %d", len(seen), n)
		}
		done := e.do("GET", "/v1/projects/"+projA+"/executions?status=completed", adminA, nil, nil).json(t)["data"].([]any)
		found := false
		for _, d := range done {
			m := d.(map[string]any)
			found = found || m["id"] == doneID
			if m["status"] != "completed" {
				t.Errorf("lọc completed trả về execution %v", m["status"])
			}
		}
		if !found {
			t.Error("lọc completed thiếu execution đã xong")
		}
		want(t, e.do("GET", "/v1/projects/"+projA+"/executions?status=bogus", adminA, nil, nil), 400, "invalid_parameter")
	})

	t.Run("gateway internal endpoint accepts only live execution tokens", func(t *testing.T) {
		tasks := e.do("GET", "/v1/executions/"+doneID+"/tasks", adminA, nil, nil).json(t)["data"].([]any)
		taskID := uuid.MustParse(tasks[0].(map[string]any)["id"].(string))
		var org, project, version, initiator uuid.UUID
		if err := e.db.Admin.QueryRow(ctx, `SELECT t.org_id, e.project_id, t.agent_version_id, e.initiator_id
			FROM agent_tasks t JOIN agent_executions e ON e.id = t.execution_id WHERE t.id = $1`, taskID).Scan(&org, &project, &version, &initiator); err != nil {
			t.Fatal(err)
		}
		call := func(token string) int {
			req, _ := http.NewRequest("POST", e.gw.URL+"/mcp/internal/v1/tasks/"+taskID.String(), strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
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
		iss, _ := exectoken.New(execTokenKey)
		forged, _ := exectoken.New([]byte(strings.Repeat("z", 32)))
		claims := exectoken.Claims{OrgID: org, ProjectID: project, ExecutionID: uuid.MustParse(doneID), TaskID: taskID,
			AgentVersionID: version, InitiatorType: "user", InitiatorID: initiator}
		now := time.Now()
		finished, _ := iss.Sign(claims, now, now.Add(time.Minute))
		bad, _ := forged.Sign(claims, now, now.Add(time.Minute))
		for name, c := range map[string]struct {
			token string
			want  int
		}{
			"no token":                    {"", 401},
			"OIDC token":                  {adminA, 401},
			"token signed by another key": {bad, 401},
			"task already finished":       {finished, 403},
		} {
			if got := call(c.token); got != c.want {
				t.Errorf("%s: status %d, muốn %d", name, got, c.want)
			}
		}
		// Và execution token không dùng được ở endpoint dành cho client ngoài.
		req, _ := http.NewRequest("POST", e.gw.URL+"/mcp/v1/projects/"+project.String(), strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+finished)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != 401 {
			t.Errorf("execution token ở endpoint ngoài: %d", res.StatusCode)
		}
	})
}
