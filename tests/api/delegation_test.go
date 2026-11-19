package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/hqgroups/mcp/internal/agent/orchestrator"
	"github.com/hqgroups/mcp/internal/ai/provider"
)

// delegateCall là lượt model gọi tool nội bộ delegate_task.
func delegateCall(agent, goal string) provider.Response {
	args, _ := json.Marshal(map[string]string{"agent": agent, "goal": goal})
	return provider.Response{StopReason: provider.StopToolUse, ModelUsed: "sonnet", Usage: provider.Usage{InputTokens: 1000, OutputTokens: 100},
		ToolCalls: []provider.ToolCall{{ID: "d" + uuid.NewString()[:8], Name: "delegate_task", Arguments: args}}}
}

// lastToolResult là kết quả tool gần nhất trong hội thoại ("" nếu chưa có).
func lastToolResult(req provider.Request) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == provider.RoleTool {
			return req.Messages[i].Text
		}
	}
	return ""
}

func toolNames(req provider.Request) []string {
	names := make([]string, len(req.Tools))
	for i, t := range req.Tools {
		names[i] = t.Name
	}
	return names
}

// delegating đặt delegation của một Agent Version.
func delegating(maxDepth int, targets ...string) func(cfg map[string]any) {
	return func(cfg map[string]any) {
		cfg["delegation"] = map[string]any{"can_delegate_to": targets, "max_depth": maxDepth}
	}
}

func (f *plannedFixture) single(tok, agent string) string {
	f.t.Helper()
	r := f.e.do("POST", "/v1/projects/"+f.project+"/executions", tok, map[string]any{"mode": "single_agent", "agent_id": f.agents[agent],
		"input": map[string]any{"message": "Kiểm tra production"}}, map[string]string{"Idempotency-Key": uuid.NewString()})
	if r.status != 202 {
		f.t.Fatalf("start: %d %s", r.status, r.body)
	}
	return r.json(f.t)["id"].(string)
}

// allTasks trả về mọi task của execution theo thứ tự tạo.
func (f *plannedFixture) allTasks(id string) []map[string]any {
	f.t.Helper()
	var out []map[string]any
	for _, raw := range f.e.do("GET", "/v1/executions/"+id+"/tasks", f.e.tok("admin-a"), nil, nil).json(f.t)["data"].([]any) {
		out = append(out, raw.(map[string]any))
	}
	return out
}

// newVersion tạo và publish một version mới của Agent với cấu hình đã sửa.
func (f *plannedFixture) newVersion(name string, tools []string, mutate func(cfg map[string]any)) {
	f.t.Helper()
	cfg := map[string]any{
		"model": map[string]any{"policy": "fixed", "fixed_model_id": f.modelID},
		"limits": map[string]any{"max_iterations": 4, "max_tool_calls": 6, "timeout_seconds": 120,
			"budget": map[string]any{"amount": "1", "currency": "USD"}},
		"tools": []any{},
	}
	if len(tools) > 0 {
		cfg["tools"] = []any{map[string]any{"server_id": f.server, "allow": tools}}
	}
	if mutate != nil {
		mutate(cfg)
	}
	id := f.agents[name]
	r := f.e.do("POST", "/v1/agents/"+id+"/versions", f.e.tok("admin-a"), map[string]any{"system_prompt": "AGENT:" + name, "config": cfg},
		map[string]string{"Idempotency-Key": uuid.NewString()})
	if r.status != 201 {
		f.t.Fatalf("version: %d %s", r.status, r.body)
	}
	v := r.json(f.t)["version"]
	if r := f.e.do("PUT", "/v1/agents/"+id+"/active-version", f.e.tok("admin-a"), map[string]any{"version": v}, nil); r.status != 200 {
		f.t.Fatalf("publish: %d %s", r.status, r.body)
	}
}

func TestDelegation(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	adminA, adminB := e.tok("admin-a"), e.tok("admin-b")
	f := newPlannedFixture(t, e, "delegation-a")
	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := e.db.Admin.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	db := f.agent("database", []string{"list_containers"}, nil)
	lead := f.agent("lead", nil, delegating(1, db))

	t.Run("delegation config is validated when the version is created", func(t *testing.T) {
		other := e.createProject(adminA, "delegation-other", "other")["id"].(string)
		foreign := e.do("POST", "/v1/projects/"+other+"/agents", adminA, map[string]any{"slug": "elsewhere", "name": "x", "system_prompt": "x",
			"config": map[string]any{"model": map[string]any{"policy": "tiered"}, "limits": map[string]any{"max_iterations": 1, "max_tool_calls": 1, "timeout_seconds": 10}, "tools": []any{}}}, nil).json(t)["id"].(string)
		projB := e.createProject(adminB, "delegation-b", "b")["id"].(string)
		orgB := e.do("POST", "/v1/projects/"+projB+"/agents", adminB, map[string]any{"slug": "orgb", "name": "x", "system_prompt": "x",
			"config": map[string]any{"model": map[string]any{"policy": "tiered"}, "limits": map[string]any{"max_iterations": 1, "max_tool_calls": 1, "timeout_seconds": 10}, "tools": []any{}}}, nil).json(t)["id"].(string)
		many := make([]string, 9)
		for i := range many {
			many[i] = uuid.NewString()
		}
		bodies := map[string]string{}
		for name, d := range map[string]map[string]any{
			"agent của project khác": {"can_delegate_to": []string{foreign}, "max_depth": 1},
			"agent của org khác":     {"can_delegate_to": []string{orgB}, "max_depth": 1},
			"agent không tồn tại":    {"can_delegate_to": []string{uuid.NewString()}, "max_depth": 1},
			"thiếu max_depth":        {"can_delegate_to": []string{db}},
			"max_depth 0":            {"can_delegate_to": []string{db}, "max_depth": 0},
			"max_depth quá lớn":      {"can_delegate_to": []string{db}, "max_depth": 6},
			"lặp lại":                {"can_delegate_to": []string{db, db}, "max_depth": 1},
			"quá nhiều agent đích":   {"can_delegate_to": many, "max_depth": 1},
		} {
			r := e.do("POST", "/v1/projects/"+f.project+"/agents", adminA, map[string]any{"slug": "bad-" + uuid.NewString()[:8], "name": "bad", "system_prompt": "x",
				"config": map[string]any{"model": map[string]any{"policy": "fixed", "fixed_model_id": f.modelID},
					"limits": map[string]any{"max_iterations": 2, "max_tool_calls": 2, "timeout_seconds": 30}, "tools": []any{}, "delegation": d}}, nil)
			if r.status != 422 {
				t.Errorf("%s: %d %s", name, r.status, r.body)
			}
			bodies[name] = strings.NewReplacer(foreign, "X", orgB, "X").Replace(fmt.Sprint(r.json(t)["detail"]))
		}
		// Agent của tenant / project khác không lộ ra: cùng một dạng thông báo với agent không tồn tại.
		if !strings.HasSuffix(bodies["agent của org khác"], "không tồn tại trong project này") ||
			!strings.HasSuffix(bodies["agent của project khác"], "không tồn tại trong project này") {
			t.Errorf("thông báo: %v", bodies)
		}
	})

	t.Run("an agent delegates a task and continues with the summary of its result", func(t *testing.T) {
		e.model.script(func(_ context.Context, _ int, req provider.Request) (provider.Response, error) {
			switch {
			case agentOf(req) == "database" && !hasToolResult(req):
				return toolUse("infra.list_containers"), nil
			case agentOf(req) == "database":
				return answer("DB-KHOE-MANH"), nil
			case !hasToolResult(req):
				return delegateCall("database", "GOAL-CHO-DATABASE"), nil
			}
			return answer("LEAD-XONG: " + lastToolResult(req)), nil
		})
		id := f.single(adminA, "lead")
		x := f.waitStatus(id, 30*time.Second, plannedTerminal...)
		if x["status"] != "completed" {
			t.Fatalf("execution: %v", x)
		}
		want := "LEAD-XONG: <task_result agent=\"database\" status=\"completed\">\nDB-KHOE-MANH\n</task_result>"
		if got := x["output"].(map[string]any)["message"]; got != want {
			t.Errorf("output = %q", got)
		}
		tasks := f.allTasks(id)
		if len(tasks) != 2 {
			t.Fatalf("muốn 2 task, có %d", len(tasks))
		}
		root, child := tasks[0], tasks[1]
		if child["parent_task_id"] != root["id"] || child["depth"] != float64(1) || child["agent_id"] != db ||
			child["goal"] != "GOAL-CHO-DATABASE" || child["status"] != "completed" || child["output_summary"] != "DB-KHOE-MANH" {
			t.Errorf("task con: %v", child)
		}
		// Budget của task con: phần nhỏ hơn giữa budget của Agent đích (1) và phần còn lại
		// của task cha sau khi giữ lại một lượt cho chính nó (1 − 0.0045 đã tiêu − 0.06144).
		if b := child["budget"].(map[string]any); b["amount"] != "0.93406000" {
			t.Errorf("budget của task con: %v", b)
		}
		var kinds []string
		for _, s := range e.do("GET", "/v1/tasks/"+root["id"].(string)+"/steps", adminA, nil, nil).json(t)["data"].([]any) {
			m := s.(map[string]any)
			kinds = append(kinds, fmt.Sprintf("%v:%v:%v", m["step_seq"], m["kind"], m["status"]))
		}
		if strings.Join(kinds, " ") != "1:llm_call:succeeded 2:delegation:succeeded 3:llm_call:succeeded" {
			t.Errorf("step của task cha: %v", kinds)
		}
		// Lead thấy tool delegate_task với đúng một Agent đích; task con chỉ thấy goal của
		// nó và tool của Agent đích, không thấy hội thoại hay tool giao việc của lead.
		for _, req := range e.model.seen() {
			switch agentOf(req) {
			case "lead":
				if names := toolNames(req); len(names) != 1 || names[0] != "delegate_task" || !strings.Contains(string(req.Tools[0].InputSchema), `"enum":["database"]`) {
					t.Errorf("tool của lead: %v %s", names, req.Tools[0].InputSchema)
				}
			case "database":
				// Goal do model của lead sinh ra: được đánh dấu là việc do Agent khác giao.
				first := req.Messages[0].Text
				if names := toolNames(req); len(names) != 1 || names[0] != "infra.list_containers" ||
					!strings.HasPrefix(first, "<delegated_task>") || !strings.Contains(first, "\n\nGOAL-CHO-DATABASE\n</delegated_task>") ||
					req.System != "AGENT:database" {
					t.Errorf("task con: tools %v, system %q, messages %+v", names, req.System, req.Messages)
				}
			}
		}
		// Mức đã tiêu của task cha (căn cứ chặn budget) gồm cả task con; execution là tổng.
		var parentCost, spent, childCost, total string
		if err := e.db.Admin.QueryRow(ctx, `SELECT p.estimated_cost::text, agent_task_spent(p.org_id, p.id)::text, c.estimated_cost::text, x.estimated_cost::text
			FROM agent_tasks p JOIN agent_tasks c ON c.parent_task_id = p.id JOIN agent_executions x ON x.id = p.execution_id WHERE p.id = $1`, root["id"]).Scan(&parentCost, &spent, &childCost, &total); err != nil {
			t.Fatal(err)
		}
		if spent != total || childCost == "0.00000000" || total == parentCost {
			t.Errorf("chi phí: cha %s, cha + con %s, con %s, execution %s", parentCost, spent, childCost, total)
		}
		if n := count(`SELECT count(*) FROM audit_logs WHERE org_id = $1 AND action = 'delegation.create' AND execution_id = $2 AND details->>'depth' = '1'`, e.orgA, id); n != 1 {
			t.Errorf("audit delegation.create: %d", n)
		}
		if n := count(`SELECT count(*) FROM audit_logs WHERE org_id = $1 AND action = 'task.finish' AND execution_id = $2`, e.orgA, id); n != 1 {
			t.Errorf("audit task.finish của task con: %d", n)
		}
		want404 := func(r resp) {
			t.Helper()
			if r.status != 404 {
				t.Errorf("tenant khác: %d", r.status)
			}
		}
		want404(e.do("GET", "/v1/tasks/"+child["id"].(string), adminB, nil, nil))
		want404(e.do("GET", "/v1/tasks/"+child["id"].(string)+"/steps", adminB, nil, nil))
	})

	t.Run("fan-out is capped: the execution never exceeds the task limit, and repeated denials stop the task", func(t *testing.T) {
		f.agent("fanout", nil, func(cfg map[string]any) {
			delegating(1, db)(cfg)
			cfg["limits"].(map[string]any)["max_iterations"] = 40
			cfg["limits"].(map[string]any)["max_tool_calls"] = 60
			cfg["limits"].(map[string]any)["budget"] = map[string]any{"amount": "5", "currency": "USD"}
		})
		var n atomic.Int64
		e.model.script(func(_ context.Context, _ int, req provider.Request) (provider.Response, error) {
			if agentOf(req) == "database" {
				return answer("ok"), nil
			}
			// Model cứ giao việc mãi (ví dụ vì bị tiêm chỉ dẫn).
			return delegateCall("database", fmt.Sprintf("việc %d", n.Add(1))), nil
		})
		id := f.single(e.tok("multi-a"), "fanout")
		x := f.waitStatus(id, 60*time.Second, plannedTerminal...)
		if x["status"] != "failed" || x["error"].(map[string]any)["code"] != "tool_denied" {
			t.Fatalf("execution: %v %v", x["status"], x["error"])
		}
		// Trần 16 task mỗi execution: task gốc + 15 task con; ba lần sau bị từ chối rồi task dừng.
		if n := count(`SELECT count(*) FROM agent_tasks WHERE execution_id = $1`, id); n != orchestrator.MaxTasks {
			t.Errorf("số task = %d, muốn đúng %d", n, orchestrator.MaxTasks)
		}
		if n := count(`SELECT count(*) FROM audit_logs WHERE org_id = $1 AND action = 'delegation.deny' AND execution_id = $2 AND details->>'rule' = 'task_limit'`, e.orgA, id); n != 3 {
			t.Errorf("audit delegation.deny (task_limit) = %d, muốn 3", n)
		}
		if n := count(`SELECT count(*) FROM agent_tasks WHERE execution_id = $1 AND finished_at IS NULL`, id); n != 0 {
			t.Errorf("%d task còn mở", n)
		}
	})

	t.Run("no cycles, and the tightest max_depth in the chain applies to everything below", func(t *testing.T) {
		// a (max_depth 2) → b (max_depth 5) → c; c được khai báo giao việc cho a, b, database.
		c := f.agent("cc", nil, nil)
		b := f.agent("bb", nil, delegating(5, c))
		a := f.agent("aa", nil, delegating(2, b))
		f.newVersion("cc", nil, delegating(5, a, b, db))
		probe := make(chan struct{})
		probed := make(chan struct{})
		var ccTools []string
		e.model.script(func(_ context.Context, _ int, req provider.Request) (provider.Response, error) {
			switch agentOf(req) {
			case "aa":
				if !hasToolResult(req) {
					return delegateCall("bb", "xuống b"), nil
				}
			case "bb":
				if !hasToolResult(req) {
					return delegateCall("cc", "xuống c"), nil
				}
			case "cc":
				ccTools = toolNames(req)
				close(probe) // giữ task c ở trạng thái đang chạy trong lúc test gọi thẳng điểm quyết định
				<-probed
			}
			return answer("xong " + agentOf(req)), nil
		})
		id := f.single(adminA, "aa")
		select {
		case <-probe:
		case <-time.After(20 * time.Second):
			t.Fatal("task c không chạy")
		}
		// c ở độ sâu 2. Giao lại cho a hoặc b là chu trình; giao cho database là độ sâu 3,
		// vượt max_depth 2 của a dù c tự đặt 5. Gọi thẳng hàm quyết định để chắc luật nằm ở
		// database chứ không chỉ ở danh sách tool đưa cho model.
		var ccTask uuid.UUID
		if err := e.db.Admin.QueryRow(ctx, `SELECT id FROM agent_tasks WHERE execution_id = $1 AND depth = 2`, id).Scan(&ccTask); err != nil {
			t.Fatal(err)
		}
		if _, err := e.db.Admin.Exec(ctx, `INSERT INTO execution_steps (org_id, execution_id, task_id, step_seq, kind, status) VALUES ($1,$2,$3,90,'delegation','started')`, e.orgA, id, ccTask); err != nil {
			t.Fatal(err)
		}
		decide := func(target string) string {
			t.Helper()
			tx, err := e.db.Admin.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			var rule string
			if _, err := tx.Exec(ctx, `SELECT set_config('app.org_id', $1, true)`, e.orgA.String()); err != nil {
				t.Fatal(err)
			}
			if err := tx.QueryRow(ctx, `SELECT agent_task_delegate($1, 90, $2, 'g')->>'rule'`, ccTask, target).Scan(&rule); err != nil {
				t.Fatal(err)
			}
			return rule
		}
		if got := []string{decide(a), decide(b), decide(db)}; got[0] != "cycle" || got[1] != "cycle" || got[2] != "max_depth" {
			t.Errorf("luật = %v, muốn [cycle cycle max_depth]", got)
		}
		close(probed)
		x := f.waitStatus(id, 30*time.Second, plannedTerminal...)
		if x["status"] != "completed" {
			t.Fatalf("execution: %v", x)
		}
		if n := count(`SELECT count(*) FROM agent_tasks WHERE execution_id = $1`, id); n != 3 {
			t.Errorf("số task = %d, muốn 3 (a, b, c)", n)
		}
		// Model của c không được chào một việc chắc chắn bị từ chối: tool không xuất hiện.
		if len(ccTools) != 0 {
			t.Errorf("task c được chào tool %v dù mọi lần giao việc đều bị từ chối", ccTools)
		}
	})

	t.Run("a delegated task needs a budget carved out of what its parent has left", func(t *testing.T) {
		noBudget := func(cfg map[string]any) { delete(cfg["limits"].(map[string]any), "budget") }
		free := f.agent("free", nil, noBudget)
		f.agent("lead-free", nil, func(cfg map[string]any) { delegating(1, free, db)(cfg); noBudget(cfg) })
		// Budget của lead-poor đủ cho hai lượt của chính nó nhưng phần còn lại sau lượt đầu
		// (0.13 − 0.0045) không đủ sàn một lượt của task con cộng… vẫn đủ: dùng 0.065.
		f.agent("lead-poor", nil, func(cfg map[string]any) {
			delegating(1, db)(cfg)
			cfg["limits"].(map[string]any)["budget"] = map[string]any{"amount": "0.065", "currency": "USD"}
		})
		var results []string
		e.model.script(func(_ context.Context, _ int, req provider.Request) (provider.Response, error) {
			switch agentOf(req) {
			case "lead-free":
				switch len(req.Messages) {
				case 1:
					return delegateCall("free", "không ai có budget"), nil
				case 3:
					results = append(results, lastToolResult(req))
					return delegateCall("database", "agent đích có budget"), nil
				}
				results = append(results, lastToolResult(req))
			case "lead-poor":
				if !hasToolResult(req) {
					return delegateCall("database", "không đủ budget"), nil
				}
				results = append(results, lastToolResult(req))
			}
			return answer("xong " + agentOf(req)), nil
		})
		id := f.single(adminA, "lead-free")
		if x := f.waitStatus(id, 30*time.Second, plannedTerminal...); x["status"] != "completed" {
			t.Fatalf("execution: %v", x)
		}
		// Cả cha lẫn đích không có budget: từ chối. Đích có budget: task con nhận budget đó.
		if len(results) != 2 || !strings.Contains(results[0], "không có budget") || !strings.Contains(results[1], `status="completed"`) {
			t.Errorf("kết quả: %q", results)
		}
		if n := count(`SELECT count(*) FROM agent_tasks WHERE execution_id = $1 AND parent_task_id IS NOT NULL AND budget_limit = 1`, id); n != 1 {
			t.Errorf("task con với budget của agent đích: %d", n)
		}
		results = nil
		id = f.single(adminA, "lead-poor")
		f.waitStatus(id, 30*time.Second, plannedTerminal...)
		if n := count(`SELECT count(*) FROM audit_logs WHERE org_id = $1 AND action = 'delegation.deny' AND execution_id = $2 AND details->>'rule' = 'budget_insufficient'`, e.orgA, id); n != 1 {
			t.Errorf("audit budget_insufficient: %d", n)
		}
		if n := count(`SELECT count(*) FROM agent_tasks WHERE execution_id = $1`, id); n != 1 {
			t.Errorf("task con được tạo dù không đủ budget: %d task", n)
		}
	})

	t.Run("targets are fixed when the execution starts; unknown or unavailable agents are refused", func(t *testing.T) {
		gone := f.agent("gone", nil, nil)
		f.agent("lead-gone", nil, delegating(1, gone, db))
		// Agent đích bị tắt trước khi execution được tạo: không có trong danh sách.
		etag := e.do("GET", "/v1/agents/"+gone, adminA, nil, nil).header.Get("ETag")
		if r := e.do("PATCH", "/v1/agents/"+gone, adminA, map[string]any{"status": "disabled"}, map[string]string{"If-Match": etag}); r.status != 200 {
			t.Fatalf("disable: %d %s", r.status, r.body)
		}
		var first string
		e.model.script(func(_ context.Context, _ int, req provider.Request) (provider.Response, error) {
			if agentOf(req) == "lead-gone" {
				switch len(req.Messages) {
				case 1:
					if !strings.Contains(string(req.Tools[0].InputSchema), `"enum":["database"]`) {
						t.Errorf("agent đã tắt vẫn được chào: %s", req.Tools[0].InputSchema)
					}
					return delegateCall("gone", "agent đã tắt"), nil
				case 3:
					first = lastToolResult(req)
					// Arguments sai dạng cũng là từ chối, không phải lỗi của task.
					bad := delegateCall("database", "x")
					bad.ToolCalls[0].Arguments = json.RawMessage(`{"agent":"database","goal":"x","budget":"999"}`)
					return bad, nil
				}
			}
			return answer("xong"), nil
		})
		id := f.single(adminA, "lead-gone")
		if x := f.waitStatus(id, 30*time.Second, plannedTerminal...); x["status"] != "completed" {
			t.Fatalf("execution: %v", x)
		}
		if !strings.Contains(first, "bị từ chối") {
			t.Errorf("giao việc cho agent đã tắt: %q", first)
		}
		for _, rule := range []string{"target_not_allowed", "invalid_arguments"} {
			if n := count(`SELECT count(*) FROM audit_logs WHERE org_id = $1 AND action = 'delegation.deny' AND execution_id = $2 AND details->>'rule' = $3`, e.orgA, id, rule); n != 1 {
				t.Errorf("audit %s: %d", rule, n)
			}
		}
		if n := count(`SELECT count(*) FROM agent_tasks WHERE execution_id = $1`, id); n != 1 {
			t.Errorf("số task = %d, muốn 1", n)
		}
	})

	t.Run("disabling or rolling back the target takes effect for tasks not yet created", func(t *testing.T) {
		target := f.agent("volatile", nil, nil)
		f.agent("lead-volatile", nil, delegating(1, target))
		hold := make(chan struct{})
		entered := make(chan struct{})
		var results []string
		e.model.script(func(_ context.Context, _ int, req provider.Request) (provider.Response, error) {
			if agentOf(req) == "lead-volatile" {
				switch len(req.Messages) {
				case 1:
					close(entered)
					<-hold // execution đã bắt đầu; Agent đích bị tắt trong lúc này
					return delegateCall("volatile", "việc"), nil
				}
				results = append(results, lastToolResult(req))
			}
			return answer("xong"), nil
		})
		id := f.single(adminA, "lead-volatile")
		<-entered
		etag := e.do("GET", "/v1/agents/"+target, adminA, nil, nil).header.Get("ETag")
		if r := e.do("PATCH", "/v1/agents/"+target, adminA, map[string]any{"status": "disabled"}, map[string]string{"If-Match": etag}); r.status != 200 {
			t.Fatalf("disable: %d %s", r.status, r.body)
		}
		close(hold)
		if x := f.waitStatus(id, 30*time.Second, plannedTerminal...); x["status"] != "completed" {
			t.Fatalf("execution: %v", x)
		}
		if len(results) != 1 || !strings.Contains(results[0], "không dùng được") {
			t.Errorf("giao việc cho agent vừa bị tắt: %q", results)
		}
		if n := count(`SELECT count(*) FROM agent_tasks WHERE execution_id = $1`, id); n != 1 {
			t.Errorf("task con được tạo cho agent đã tắt: %d task", n)
		}
		if n := count(`SELECT count(*) FROM audit_logs WHERE org_id = $1 AND action = 'delegation.deny' AND execution_id = $2 AND details->>'rule' = 'target_unavailable'`, e.orgA, id); n != 1 {
			t.Errorf("audit target_unavailable: %d", n)
		}
	})

	t.Run("a write tool called by a delegated task always needs a human approval", func(t *testing.T) {
		writer := f.agent("writer", []string{"create_ticket"}, nil)
		f.agent("lead-writer", nil, delegating(1, writer))
		e.model.script(func(_ context.Context, _ int, req provider.Request) (provider.Response, error) {
			switch {
			case agentOf(req) == "writer" && !hasToolResult(req):
				return toolUse("infra.create_ticket"), nil
			case agentOf(req) == "writer":
				return answer("DA-TAO-TICKET"), nil
			case !hasToolResult(req):
				return delegateCall("writer", "tạo ticket"), nil
			}
			return answer("xong"), nil
		})
		tickets := func() int {
			f.up.mu.Lock()
			defer f.up.mu.Unlock()
			n := 0
			for _, c := range f.up.calls {
				if strings.HasPrefix(c, "create_ticket ") {
					n++
				}
			}
			return n
		}
		// Chạy thẳng Agent đó (task gốc): tool write trên non-prod chạy tự do.
		id := f.single(adminA, "writer")
		if x := f.waitStatus(id, 30*time.Second, plannedTerminal...); x["status"] != "completed" || tickets() != 1 {
			t.Fatalf("writer chạy trực tiếp: %v, %d ticket", x["status"], tickets())
		}
		// Cùng Agent, cùng tool, nhưng được giao việc: phải có người duyệt.
		id = f.single(adminA, "lead-writer")
		f.waitStatus(id, 30*time.Second, "waiting_approval")
		if tickets() != 1 {
			t.Fatalf("tool ghi của task được giao việc chạy khi chưa được duyệt")
		}
		approver := e.tok("multi-a")
		pending := e.do("GET", "/v1/approvals?execution_id="+id+"&status=pending", approver, nil, nil).json(t)["data"].([]any)
		if len(pending) != 1 {
			t.Fatalf("approval đang chờ: %v", pending)
		}
		if n := count(`SELECT count(*) FROM audit_logs WHERE org_id = $1 AND action = 'mcp_tool.call' AND details->>'execution_id' = $2 AND details->>'rule' = 'delegated_write_requires_approval'`, e.orgA, id); n != 1 {
			t.Errorf("audit luật delegated_write_requires_approval: %d", n)
		}
		r := e.do("POST", "/v1/approvals/"+pending[0].(map[string]any)["id"].(string)+"/decisions", approver, map[string]any{"decision": "approve"},
			map[string]string{"Idempotency-Key": uuid.NewString()})
		if r.status != 200 {
			t.Fatalf("approve: %d %s", r.status, r.body)
		}
		if x := f.waitStatus(id, 30*time.Second, plannedTerminal...); x["status"] != "completed" || tickets() != 2 {
			t.Fatalf("sau khi duyệt: %v, %d ticket", x["status"], tickets())
		}
	})

	t.Run("cancelling stops the delegated task and its parent", func(t *testing.T) {
		entered := make(chan struct{}, 1)
		release := make(chan struct{})
		e.model.script(func(ctx context.Context, _ int, req provider.Request) (provider.Response, error) {
			if agentOf(req) == "database" {
				entered <- struct{}{}
				select {
				case <-release:
				case <-ctx.Done():
				}
				return toolUse("infra.list_containers"), nil
			}
			return delegateCall("database", "việc dài"), nil
		})
		id := f.single(adminA, "lead")
		select {
		case <-entered:
		case <-time.After(20 * time.Second):
			t.Fatal("task con không chạy")
		}
		if r := e.do("POST", "/v1/executions/"+id+"/cancel", adminA, nil, nil); r.status != 202 {
			t.Fatalf("cancel: %d", r.status)
		}
		close(release)
		x := f.waitStatus(id, 20*time.Second, plannedTerminal...)
		tasks := f.allTasks(id)
		if x["status"] != "cancelled" || len(tasks) != 2 || tasks[0]["status"] != "cancelled" || tasks[1]["status"] != "cancelled" {
			t.Fatalf("execution %v; task %v / %v", x["status"], tasks[0]["status"], tasks[len(tasks)-1]["status"])
		}
		if n := count(`SELECT count(*) FROM execution_steps WHERE execution_id = $1 AND kind = 'tool_call' AND status = 'succeeded'`, id); n != 0 {
			t.Errorf("%d tool call chạy sau khi huỷ", n)
		}
	})

	t.Run("a planned task can delegate too, within its own budget share", func(t *testing.T) {
		e.model.script(func(_ context.Context, _ int, req provider.Request) (provider.Response, error) {
			switch {
			case agentOf(req) == "database":
				return answer("DB-OK"), nil
			case !hasToolResult(req):
				return delegateCall("database", "kiểm tra db"), nil
			}
			return answer("LEAD-OK"), nil
		})
		task := planTask("lead", lead)
		task["budget"] = map[string]any{"amount": "0.5", "currency": "USD"}
		r := f.start(adminA, map[string]any{"tasks": []any{task}}, nil)
		if r.status != 202 {
			t.Fatalf("start: %d %s", r.status, r.body)
		}
		id := r.json(t)["id"].(string)
		x := f.waitStatus(id, 30*time.Second, plannedTerminal...)
		if x["status"] != "completed" || x["output"].(map[string]any)["message"] != "LEAD-OK" {
			t.Fatalf("execution: %v", x)
		}
		// Task con lấy budget từ phần của task trong kế hoạch (0.5 − 0.0045 − một lượt giữ
		// lại 0.06144), không phải 1 của agent đích.
		if n := count(`SELECT count(*) FROM agent_tasks WHERE execution_id = $1 AND parent_task_id IS NOT NULL AND budget_limit = 0.43406 AND status = 'completed'`, id); n != 1 {
			t.Errorf("task con của task trong kế hoạch: %d", n)
		}
	})
}
