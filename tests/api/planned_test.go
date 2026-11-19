package api_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/hqgroups/mcp/internal/adminops"
	"github.com/hqgroups/mcp/internal/ai/provider"
	"github.com/hqgroups/mcp/internal/mcp/registry"
	"github.com/hqgroups/mcp/internal/shared/signedctx"
)

// plannedFixture là hạ tầng của org A cho execution chế độ planned: một MCP server có
// tool đọc và tool destructive, một model có giá, một project đã bật planned, và các
// Agent phân biệt nhau bằng system prompt "AGENT:<tên>".
type plannedFixture struct {
	e       *env
	t       *testing.T
	up      *upstream
	project string
	modelID string
	server  string
	agents  map[string]string
}

func newPlannedFixture(t *testing.T, e *env, slug string) *plannedFixture {
	t.Helper()
	ctx := context.Background()
	adminA := e.tok("admin-a")
	signer, _ := signedctx.NewSigner(signingKey)
	conn, err := e.db.Admin.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	f := &plannedFixture{e: e, t: t, agents: map[string]string{}}
	f.up = newUpstream(t, "infra", readTool("list_containers", "Liệt kê container."), writeTool("restart_container", "Khởi động lại container."),
		writeTool("create_ticket", "Tạo ticket."))
	srv := adminops.MCPServer{OrgSlug: "org-a", Slug: "infra"}
	if _, err := adminops.RegisterManagedMCPServer(ctx, conn.Conn(), signer, srv, "Infra", f.up.srv.URL); err != nil {
		t.Fatal(err)
	}
	on, risk, side := true, "destructive", "non_idempotent"
	if err := adminops.ClassifyMCPTool(ctx, conn.Conn(), srv, "restart_container",
		registry.Classification{Enabled: &on, RiskLevel: &risk, SideEffect: &side}); err != nil {
		t.Fatal(err)
	}
	write := "write"
	if err := adminops.ClassifyMCPTool(ctx, conn.Conn(), srv, "create_ticket",
		registry.Classification{Enabled: &on, RiskLevel: &write, SideEffect: &side}); err != nil {
		t.Fatal(err)
	}
	f.server = e.do("GET", "/v1/mcp-servers", adminA, nil, nil).json(t)["data"].([]any)[0].(map[string]any)["id"].(string)
	provID := f.post("/v1/model-providers", map[string]any{"slug": "local", "adapter": "claude-cli"})["id"].(string)
	f.modelID = f.post("/v1/models", map[string]any{"provider_id": provID, "provider_model": "sonnet", "display_name": "Sonnet",
		"tier": "standard", "capabilities": map[string]any{"tool_calling": true, "context_window": 200000}})["id"].(string)
	f.post("/v1/models/"+f.modelID+"/pricing", map[string]any{"currency": "USD", "input_per_mtok": "3.00", "output_per_mtok": "15.00",
		"effective_from": time.Now().UTC().Format(time.RFC3339Nano)})
	f.project = e.createProject(adminA, slug, slug)["id"].(string)
	want(t, f.settings(map[string]any{"orchestration": map[string]any{"planned_enabled": true}}), 200, "")
	return f
}

func (f *plannedFixture) post(path string, body any) map[string]any {
	f.t.Helper()
	r := f.e.do("POST", path, f.e.tok("admin-a"), body, map[string]string{"Idempotency-Key": uuid.NewString()})
	if r.status != 201 {
		f.t.Fatalf("POST %s: %d %s", path, r.status, r.body)
	}
	return r.json(f.t)
}

func (f *plannedFixture) settings(settings map[string]any) resp {
	return f.e.do("PATCH", "/v1/projects/"+f.project, f.e.tok("admin-a"), map[string]any{"settings": settings}, nil)
}

// agent tạo và publish một Agent. tools là tên tool được phép trên server infra.
func (f *plannedFixture) agent(name string, tools []string, mutate func(cfg map[string]any)) string {
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
	a := f.post("/v1/projects/"+f.project+"/agents", map[string]any{"slug": name, "name": name, "system_prompt": "AGENT:" + name, "config": cfg})
	id := a["id"].(string)
	if r := f.e.do("PUT", "/v1/agents/"+id+"/active-version", f.e.tok("admin-a"), map[string]any{"version": 1}, nil); r.status != 200 {
		f.t.Fatalf("publish: %d %s", r.status, r.body)
	}
	f.agents[name] = id
	return id
}

// planTask là một task của kế hoạch trong request.
func planTask(id, agentID string, deps ...string) map[string]any {
	return map[string]any{"id": id, "agent_id": agentID, "goal": "GOAL:" + id, "depends_on": deps}
}

func (f *plannedFixture) start(tok string, plan map[string]any, extra map[string]any) resp {
	body := map[string]any{"mode": "planned", "input": map[string]any{"message": "Kiểm tra production"}, "plan": plan}
	for k, v := range extra {
		body[k] = v
	}
	return f.e.do("POST", "/v1/projects/"+f.project+"/executions", tok, body, map[string]string{"Idempotency-Key": uuid.NewString()})
}

func (f *plannedFixture) mustStart(tasks ...map[string]any) string {
	f.t.Helper()
	r := f.start(f.e.tok("admin-a"), map[string]any{"tasks": tasks}, nil)
	if r.status != 202 {
		f.t.Fatalf("start: %d %s", r.status, r.body)
	}
	return r.json(f.t)["id"].(string)
}

func (f *plannedFixture) get(id string) map[string]any {
	return f.e.do("GET", "/v1/executions/"+id, f.e.tok("admin-a"), nil, nil).json(f.t)
}

func (f *plannedFixture) waitStatus(id string, within time.Duration, statuses ...string) map[string]any {
	f.t.Helper()
	var x map[string]any
	for deadline := time.Now().Add(within); time.Now().Before(deadline); time.Sleep(25 * time.Millisecond) {
		x = f.get(id)
		if slices.Contains(statuses, x["status"].(string)) {
			return x
		}
	}
	f.t.Fatalf("execution %s ở trạng thái %v, muốn %v (error %v)", id, x["status"], statuses, x["error"])
	return nil
}

var plannedTerminal = []string{"completed", "failed", "cancelled", "timed_out", "budget_exceeded"}

// tasks trả về task của execution theo plan_key.
func (f *plannedFixture) tasks(id string) map[string]map[string]any {
	f.t.Helper()
	out := map[string]map[string]any{}
	for _, raw := range f.e.do("GET", "/v1/executions/"+id+"/tasks", f.e.tok("admin-a"), nil, nil).json(f.t)["data"].([]any) {
		task := raw.(map[string]any)
		out[task["plan_key"].(string)] = task
	}
	return out
}

func (f *plannedFixture) taskStatuses(id string) string {
	tasks := f.tasks(id)
	keys := make([]string, 0, len(tasks))
	for k := range tasks {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + tasks[k]["status"].(string)
	}
	return strings.Join(parts, " ")
}

// agentOf trả về tên Agent của một lượt model (system prompt "AGENT:<tên>").
func agentOf(req provider.Request) string { return strings.TrimPrefix(req.System, "AGENT:") }

// hasToolResult cho biết hội thoại đã có kết quả tool hay chưa.
func hasToolResult(req provider.Request) bool {
	for _, m := range req.Messages {
		if m.Role == provider.RoleTool {
			return true
		}
	}
	return false
}

// barrier chặn tới khi đủ n bên cùng đến: chứng minh n task chạy đồng thời.
type barrier struct {
	mu      sync.Mutex
	n       int
	arrived int
	open    chan struct{}
}

func newBarrier(n int) *barrier { return &barrier{n: n, open: make(chan struct{})} }

func (b *barrier) wait(ctx context.Context) error {
	b.mu.Lock()
	b.arrived++
	if b.arrived == b.n {
		close(b.open)
	}
	b.mu.Unlock()
	select {
	case <-b.open:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(15 * time.Second):
		return errors.New("không đủ task chạy đồng thời")
	}
}

func TestPlannedExecutions(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	adminA, adminB, viewerA, approverA := e.tok("admin-a"), e.tok("admin-b"), e.tok("viewer-a"), e.tok("multi-a")
	f := newPlannedFixture(t, e, "planned-a")
	diagnosers := []string{"monitoring", "database", "security", "devops"}
	for _, name := range diagnosers {
		f.agent(name, []string{"list_containers"}, nil)
	}
	synth := f.agent("synth", nil, nil)
	fixer := f.agent("fixer", []string{"restart_container"}, nil)
	// Agent Version không có limits.budget: task của nó phải được kế hoạch cấp budget.
	uncapped := f.agent("uncapped", nil, func(cfg map[string]any) {
		delete(cfg["limits"].(map[string]any), "budget")
	})
	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := e.db.Admin.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	diagnosis := func() []map[string]any {
		var tasks []map[string]any
		for _, name := range diagnosers {
			tasks = append(tasks, planTask(name, f.agents[name]))
		}
		return append(tasks, planTask("synthesis", synth, diagnosers...))
	}

	t.Run("an invalid plan is rejected, never repaired", func(t *testing.T) {
		mon := f.agents["monitoring"]
		one := map[string]any{"tasks": []any{planTask("a", mon)}}
		// Agent của org khác và Agent không tồn tại cho cùng một lỗi: không lộ sự tồn tại.
		otherProject := e.createProject(adminB, "planned-b", "planned-b")["id"].(string)
		foreign := e.do("POST", "/v1/projects/"+otherProject+"/agents", adminB, map[string]any{"slug": "foreign", "name": "x", "system_prompt": "x",
			"config": map[string]any{"model": map[string]any{"policy": "tiered"}, "limits": map[string]any{"max_iterations": 1, "max_tool_calls": 1, "timeout_seconds": 10}, "tools": []any{}}}, nil)
		foreignID, _ := foreign.json(t)["id"].(string)
		if foreignID == "" {
			t.Fatalf("agent của org B: %d %s", foreign.status, foreign.body)
		}
		// Mười hai task, mỗi task một budget gần trần của kiểu tiền: tổng vượt int64. Phép
		// so với budget của execution không được tràn rồi lọt qua.
		var huge []any
		for i := 0; i < 12; i++ {
			huge = append(huge, map[string]any{"id": fmt.Sprintf("t%d", i), "agent_id": uncapped, "goal": "g",
				"budget": map[string]any{"amount": "9999999999", "currency": "USD"}})
		}
		overflow := map[string]any{"tasks": huge}
		// Agent của project khác trong CÙNG org, chưa có version đang hoạt động: câu trả
		// lời không được khác với Agent không tồn tại.
		sibling := e.createProject(adminA, "planned-sibling", "sibling")["id"].(string)
		draft := e.do("POST", "/v1/projects/"+sibling+"/agents", adminA, map[string]any{"slug": "draft", "name": "x", "system_prompt": "x",
			"config": map[string]any{"model": map[string]any{"policy": "tiered"}, "limits": map[string]any{"max_iterations": 1, "max_tool_calls": 1, "timeout_seconds": 10}, "tools": []any{}}}, nil)
		draftID, _ := draft.json(t)["id"].(string)
		if draftID == "" {
			t.Fatalf("agent của project khác: %d %s", draft.status, draft.body)
		}
		cases := map[string]struct {
			plan  map[string]any
			extra map[string]any
		}{
			"agent của project khác, chưa publish": {map[string]any{"tasks": []any{planTask("a", draftID)}}, nil},
			"thiếu plan":             {nil, nil},
			"không có task":          {map[string]any{"tasks": []any{}}, nil},
			"chu trình":              {map[string]any{"tasks": []any{planTask("a", mon, "b"), planTask("b", mon, "a")}}, nil},
			"phụ thuộc không có":     {map[string]any{"tasks": []any{planTask("a", mon, "zz")}}, nil},
			"id trùng":               {map[string]any{"tasks": []any{planTask("a", mon), planTask("a", mon)}}, nil},
			"agent không tồn tại":    {map[string]any{"tasks": []any{planTask("a", uuid.NewString())}}, nil},
			"agent của org khác":     {map[string]any{"tasks": []any{planTask("a", foreignID)}}, nil},
			"song song vượt project": {map[string]any{"tasks": []any{planTask("a", mon)}, "max_parallel_tasks": 5}, nil},
			"kèm agent_id":           {one, map[string]any{"agent_id": mon}},
			"task không có budget":   {map[string]any{"tasks": []any{planTask("a", uncapped)}}, nil},
			"budget task vượt agent": {map[string]any{"tasks": []any{map[string]any{"id": "a", "agent_id": mon, "goal": "g", "budget": map[string]any{"amount": "2", "currency": "USD"}}}}, nil},
			"budget âm":              {map[string]any{"tasks": []any{map[string]any{"id": "a", "agent_id": uncapped, "goal": "g", "budget": map[string]any{"amount": "-1", "currency": "USD"}}}}, nil},
			"tổng budget tràn số":    {overflow, map[string]any{"budget": map[string]any{"amount": "1", "currency": "USD"}}},
			"tổng budget vượt execution": {map[string]any{"tasks": []any{
				map[string]any{"id": "a", "agent_id": uncapped, "goal": "g", "budget": map[string]any{"amount": "3", "currency": "USD"}},
				map[string]any{"id": "b", "agent_id": uncapped, "goal": "g", "budget": map[string]any{"amount": "3", "currency": "USD"}}}},
				map[string]any{"budget": map[string]any{"amount": "5", "currency": "USD"}}},
			"timeout vượt kế hoạch": {one, map[string]any{"timeout_seconds": 121}},
		}
		before := count(`SELECT count(*) FROM agent_executions WHERE project_id = $1`, f.project)
		for name, c := range cases {
			body := map[string]any{"mode": "planned", "input": map[string]any{"message": "x"}}
			if c.plan != nil {
				body["plan"] = c.plan
			}
			for k, v := range c.extra {
				body[k] = v
			}
			r := e.do("POST", "/v1/projects/"+f.project+"/executions", adminA, body, map[string]string{"Idempotency-Key": uuid.NewString()})
			if r.status != 422 || r.code(t) != "validation_failed" {
				t.Errorf("%s: %d %s", name, r.status, r.body)
			}
		}
		r1 := f.start(adminA, map[string]any{"tasks": []any{planTask("a", uuid.NewString())}}, nil)
		r2 := f.start(adminA, map[string]any{"tasks": []any{planTask("a", foreignID)}}, nil)
		r3 := f.start(adminA, map[string]any{"tasks": []any{planTask("a", draftID)}}, nil)
		if d := r1.json(t)["detail"]; d != r2.json(t)["detail"] || d != r3.json(t)["detail"] {
			t.Errorf("agent ngoài project lộ qua thông báo lỗi: %s vs %s vs %s", r1.body, r2.body, r3.body)
		}
		if n := count(`SELECT count(*) FROM agent_executions WHERE project_id = $1`, f.project); n != before {
			t.Errorf("kế hoạch bị từ chối vẫn tạo %d execution", n-before)
		}
		// plan với mode single_agent bị từ chối; viewer không tạo được execution.
		r := e.do("POST", "/v1/projects/"+f.project+"/executions", adminA, map[string]any{"mode": "single_agent", "agent_id": mon,
			"input": map[string]any{"message": "x"}, "plan": one}, map[string]string{"Idempotency-Key": uuid.NewString()})
		want(t, r, 422, "validation_failed")
		want(t, f.start(viewerA, one, nil), 403, "permission_denied")
	})

	t.Run("planned mode must be enabled per project, and broken settings fail closed", func(t *testing.T) {
		one := map[string]any{"tasks": []any{planTask("a", f.agents["monitoring"])}}
		want(t, f.settings(map[string]any{"orchestration": map[string]any{"planned": true}}), 422, "validation_failed")
		want(t, f.settings(map[string]any{"orchestration": map[string]any{"planned_enabled": true, "max_parallel_tasks": 99}}), 422, "validation_failed")
		want(t, f.settings(map[string]any{}), 200, "")
		want(t, f.start(adminA, one, nil), 422, "planned_not_enabled")
		// Cấu hình đã lưu bị hỏng (ghi thẳng vào database): dừng, không hiểu thành "bật".
		if _, err := e.db.Admin.Exec(ctx, `UPDATE projects SET settings = '{"orchestration":{"planned_enabled":"yes"}}' WHERE id = $1`, f.project); err != nil {
			t.Fatal(err)
		}
		want(t, f.start(adminA, one, nil), 422, "orchestration_config_invalid")
		want(t, f.settings(map[string]any{"orchestration": map[string]any{"planned_enabled": true}}), 200, "")
		if n := count(`SELECT count(*) FROM audit_logs WHERE org_id = $1 AND action = 'project.update' AND details->>'orchestration_planned_enabled' = 'true'`, e.orgA); n == 0 {
			t.Error("bật planned không để lại dấu vết trong audit")
		}
	})

	t.Run("four agents diagnose in parallel, then one synthesises from their summaries", func(t *testing.T) {
		gate := newBarrier(len(diagnosers))
		e.model.script(func(ctx context.Context, _ int, req provider.Request) (provider.Response, error) {
			name := agentOf(req)
			switch {
			case name == "synth":
				return answer("TONG-HOP: nguyên nhân là web"), nil
			case !hasToolResult(req):
				// Cả bốn Agent chẩn đoán phải đang chạy cùng lúc thì mới qua được đây.
				if err := gate.wait(ctx); err != nil {
					return provider.Response{}, err
				}
				return toolUse("infra.list_containers"), nil
			}
			return answer("FINDING-" + name), nil
		})
		r := f.start(adminA, map[string]any{"tasks": diagnosis()}, nil)
		want(t, r, 202, "")
		created := r.json(t)
		id := created["id"].(string)
		if created["mode"] != "planned" || created["status"] != "queued" {
			t.Errorf("execution mới: %s", r.body)
		}
		x := f.waitStatus(id, 30*time.Second, plannedTerminal...)
		out, _ := x["output"].(map[string]any)
		if x["status"] != "completed" || out["message"] != "TONG-HOP: nguyên nhân là web" || x["error"] != nil {
			t.Fatalf("kết quả: %v", x)
		}
		if results := out["tasks"].([]any); len(results) != 5 || results[4].(map[string]any)["id"] != "synthesis" ||
			results[0].(map[string]any)["summary"] != "FINDING-monitoring" {
			t.Errorf("output.tasks: %v", out["tasks"])
		}
		tasks := f.tasks(id)
		if len(tasks) != 5 {
			t.Fatalf("muốn 5 task, có %d", len(tasks))
		}
		if got := f.taskStatuses(id); got != "database=completed devops=completed monitoring=completed security=completed synthesis=completed" {
			t.Errorf("task: %s", got)
		}
		syn := tasks["synthesis"]
		if deps := syn["depends_on"].([]any); len(deps) != 4 || !slices.Contains(deps, tasks["database"]["id"]) || syn["on_failure"] != "skip_dependents" {
			t.Errorf("synthesis: %v", syn)
		}
		if tasks["monitoring"]["output_summary"] != "FINDING-monitoring" || tasks["monitoring"]["goal"] != "GOAL:monitoring" {
			t.Errorf("monitoring: %v", tasks["monitoring"])
		}

		// Task tổng hợp chỉ thấy goal của nó và TÓM TẮT của bốn task kia, trong một khối
		// dữ liệu ở vai user; system prompt của nó không bị nối thêm gì.
		var synthReq *provider.Request
		for _, req := range e.model.seen() {
			if agentOf(req) == "synth" {
				synthReq = &req
			}
		}
		if synthReq == nil {
			t.Fatal("Agent tổng hợp không được gọi")
		}
		first := synthReq.Messages[0]
		if synthReq.System != "AGENT:synth" || len(synthReq.Messages) != 1 || first.Role != provider.RoleUser ||
			!strings.HasPrefix(first.Text, "GOAL:synthesis") || !strings.Contains(first.Text, "<task_results>") {
			t.Fatalf("lượt của synth: system %q, messages %+v", synthReq.System, synthReq.Messages)
		}
		for _, name := range diagnosers {
			if !strings.Contains(first.Text, `<task_result id="`+name+`" status="completed">`+"\nFINDING-"+name) {
				t.Errorf("synth không thấy kết quả của %s: %s", name, first.Text)
			}
		}
		if strings.Contains(first.Text, "result of list_containers") || strings.Contains(first.Text, "GOAL:monitoring") {
			t.Errorf("synth thấy hội thoại của task khác: %s", first.Text)
		}

		// Mỗi task có step, usage và audit của riêng nó; chi phí của execution là tổng.
		for _, name := range diagnosers {
			if n := count(`SELECT count(*) FROM execution_steps WHERE task_id = $1`, tasks[name]["id"]); n != 3 {
				t.Errorf("%s có %d step, muốn 3", name, n)
			}
		}
		if n := count(`SELECT count(*) FROM usage_records WHERE execution_id = $1`, id); n != 9 {
			t.Errorf("usage_records: %d, muốn 9", n)
		}
		if n := count(`SELECT count(*) FROM audit_logs WHERE org_id = $1 AND action = 'task.finish' AND execution_id = $2`, e.orgA, id); n != 5 {
			t.Errorf("audit task.finish: %d, muốn 5", n)
		}
		if n := count(`SELECT count(*) FROM audit_logs WHERE org_id = $1 AND action = 'execution.finish' AND resource_id = $2 AND details->>'tasks' = 'completed=5'`, e.orgA, id); n != 1 {
			t.Errorf("audit execution.finish: %d", n)
		}
		// Dấu vết lúc tạo ghi Agent Version, chuỗi model và budget của từng task.
		if n := count(`SELECT count(*) FROM audit_logs WHERE org_id = $1 AND action = 'execution.create' AND resource_id = $2
			AND details->>'plan' LIKE 'monitoring: agent=%version=1%models=%budget=1%synthesis:%'`, e.orgA, id); n != 1 {
			t.Errorf("audit execution.create thiếu chi tiết kế hoạch")
		}
		var sum, total string
		if err := e.db.Admin.QueryRow(ctx, `SELECT (SELECT sum(estimated_cost)::text FROM agent_tasks WHERE execution_id = $1),
			(SELECT estimated_cost::text FROM agent_executions WHERE id = $1)`, id).Scan(&sum, &total); err != nil || sum != total || total == "0.00000000" {
			t.Errorf("chi phí: tổng task %s, execution %s (%v)", sum, total, err)
		}

		// Tenant khác không thấy execution hay task của nó.
		want(t, e.do("GET", "/v1/executions/"+id, adminB, nil, nil), 404, "not_found")
		want(t, e.do("GET", "/v1/executions/"+id+"/tasks", adminB, nil, nil), 404, "not_found")
		want(t, e.do("GET", "/v1/tasks/"+syn["id"].(string), adminB, nil, nil), 404, "not_found")
		want(t, e.do("POST", "/v1/executions/"+id+"/cancel", adminB, nil, nil), 404, "not_found")
	})

	t.Run("max_parallel_tasks caps how many tasks run at once", func(t *testing.T) {
		var mu sync.Mutex
		running, peak := 0, 0
		e.model.script(func(context.Context, int, provider.Request) (provider.Response, error) {
			mu.Lock()
			running++
			peak = max(peak, running)
			mu.Unlock()
			time.Sleep(150 * time.Millisecond)
			mu.Lock()
			running--
			mu.Unlock()
			return answer("xong"), nil
		})
		var tasks []any
		for _, name := range diagnosers {
			tasks = append(tasks, planTask(name, f.agents[name]))
		}
		r := f.start(adminA, map[string]any{"tasks": tasks, "max_parallel_tasks": 2}, nil)
		want(t, r, 202, "")
		if x := f.waitStatus(r.json(t)["id"].(string), 30*time.Second, plannedTerminal...); x["status"] != "completed" {
			t.Fatalf("kết quả: %v", x)
		}
		if peak != 2 {
			t.Errorf("chạy đồng thời tối đa %d task, muốn đúng 2", peak)
		}
	})

	t.Run("a failed task skips its dependents; the rest of the plan still runs", func(t *testing.T) {
		e.model.script(func(_ context.Context, _ int, req provider.Request) (provider.Response, error) {
			if agentOf(req) == "database" {
				return provider.Response{StopReason: provider.StopRefusal, ModelUsed: "sonnet"}, nil
			}
			return answer("FINDING-" + agentOf(req)), nil
		})
		// database → report (bị bỏ qua) → final (bị bỏ qua theo); monitoring độc lập.
		id := f.mustStart(planTask("database", f.agents["database"]), planTask("monitoring", f.agents["monitoring"]),
			planTask("report", synth, "database"), planTask("final", synth, "report"))
		x := f.waitStatus(id, 30*time.Second, plannedTerminal...)
		if got := f.taskStatuses(id); got != "database=failed final=skipped monitoring=completed report=skipped" {
			t.Errorf("task: %s", got)
		}
		if x["status"] != "failed" || x["error"].(map[string]any)["code"] != "task_failed" {
			t.Errorf("execution: %v", x)
		}
		// Kết quả dở dang vẫn có: trạng thái từng task và phần đã xong.
		if results := x["output"].(map[string]any)["tasks"].([]any); len(results) != 4 || results[1].(map[string]any)["summary"] != "FINDING-monitoring" {
			t.Errorf("output: %v", x["output"])
		}
		for _, req := range e.model.seen() {
			if agentOf(req) == "synth" {
				t.Error("task bị bỏ qua vẫn gọi model")
			}
		}
		if n := count(`SELECT count(*) FROM audit_logs WHERE org_id = $1 AND action = 'task.skip' AND execution_id = $2`, e.orgA, id); n != 2 {
			t.Errorf("audit task.skip: %d, muốn 2", n)
		}
	})

	t.Run("on_failure continue lets dependents run without the failed result", func(t *testing.T) {
		e.model.script(func(_ context.Context, _ int, req provider.Request) (provider.Response, error) {
			if agentOf(req) == "database" {
				return provider.Response{StopReason: provider.StopRefusal, ModelUsed: "sonnet"}, nil
			}
			return answer("FINDING-" + agentOf(req)), nil
		})
		db := planTask("database", f.agents["database"])
		db["on_failure"] = "continue"
		id := f.mustStart(db, planTask("monitoring", f.agents["monitoring"]), planTask("synthesis", synth, "database", "monitoring"))
		x := f.waitStatus(id, 30*time.Second, plannedTerminal...)
		if got := f.taskStatuses(id); got != "database=failed monitoring=completed synthesis=completed" || x["status"] != "completed" {
			t.Errorf("task: %s; execution %v", got, x["status"])
		}
		for _, req := range e.model.seen() {
			if agentOf(req) == "synth" {
				text := req.Messages[0].Text
				if !strings.Contains(text, `<task_result id="database" status="failed">`+"\n(không có kết quả)") || !strings.Contains(text, "FINDING-monitoring") {
					t.Errorf("synth: %s", text)
				}
			}
		}
	})

	t.Run("fail_execution stops running tasks and starts no more", func(t *testing.T) {
		release := make(chan struct{})
		entered := make(chan struct{}, 1)
		e.model.script(func(ctx context.Context, _ int, req provider.Request) (provider.Response, error) {
			switch agentOf(req) {
			case "database":
				<-entered // chờ task kia thực sự đang chạy rồi mới thất bại
				return provider.Response{StopReason: provider.StopRefusal, ModelUsed: "sonnet"}, nil
			case "monitoring":
				entered <- struct{}{}
				select {
				case <-release:
				case <-ctx.Done():
					return provider.Response{}, ctx.Err()
				}
			}
			return answer("xong"), nil
		})
		db := planTask("database", f.agents["database"])
		db["on_failure"] = "fail_execution"
		id := f.mustStart(db, planTask("monitoring", f.agents["monitoring"]), planTask("after", synth, "monitoring"))
		x := f.waitStatus(id, 30*time.Second, plannedTerminal...)
		close(release)
		if got := f.taskStatuses(id); got != "after=cancelled database=failed monitoring=cancelled" {
			t.Errorf("task: %s", got)
		}
		if x["status"] != "failed" || x["error"].(map[string]any)["code"] != "task_failed" {
			t.Errorf("execution: %v", x)
		}
	})

	t.Run("cancelling the execution stops every task", func(t *testing.T) {
		gate := newBarrier(len(diagnosers))
		release := make(chan struct{})
		e.model.script(func(ctx context.Context, _ int, _ provider.Request) (provider.Response, error) {
			if err := gate.wait(ctx); err != nil {
				return provider.Response{}, err
			}
			select {
			case <-release:
			case <-ctx.Done():
			}
			return toolUse("infra.list_containers"), nil
		})
		id := f.mustStart(diagnosis()...)
		select {
		case <-gate.open:
		case <-time.After(20 * time.Second):
			t.Fatal("các task không bắt đầu")
		}
		want(t, e.do("POST", "/v1/executions/"+id+"/cancel", adminA, nil, nil), 202, "")
		close(release)
		began := time.Now()
		x := f.waitStatus(id, 20*time.Second, plannedTerminal...)
		if x["status"] != "cancelled" {
			t.Fatalf("execution: %v", x)
		}
		if got := f.taskStatuses(id); got != "database=cancelled devops=cancelled monitoring=cancelled security=cancelled synthesis=cancelled" {
			t.Errorf("task: %s", got)
		}
		if d := time.Since(began); d > 10*time.Second {
			t.Errorf("huỷ mất %s", d)
		}
		// Không tool nào chạy sau khi huỷ, và không task nào còn mở.
		if n := count(`SELECT count(*) FROM agent_tasks WHERE execution_id = $1 AND finished_at IS NULL`, id); n != 0 {
			t.Errorf("%d task còn mở sau khi huỷ", n)
		}
		if n := count(`SELECT count(*) FROM execution_steps WHERE execution_id = $1 AND kind = 'tool_call' AND status = 'succeeded'`, id); n != 0 {
			t.Errorf("%d tool call chạy sau khi huỷ", n)
		}
	})

	t.Run("one task waits for approval while the others keep running", func(t *testing.T) {
		otherDone := make(chan struct{})
		var once sync.Once
		// Model giả của task monitoring chờ tới khi execution ở waiting_approval (vì task
		// fix) rồi mới gọi tool; nó đọc id của execution qua biến bao ngoài.
		var idMu sync.Mutex
		execID := ""
		currentID := func() string {
			idMu.Lock()
			defer idMu.Unlock()
			return execID
		}
		mon := planTask("monitoring", f.agents["monitoring"])
		fix := planTask("fix", fixer)
		e.model.script(func(_ context.Context, _ int, req provider.Request) (provider.Response, error) {
			switch {
			case agentOf(req) == "fixer" && !hasToolResult(req):
				return toolUse("infra.restart_container"), nil
			case agentOf(req) == "fixer":
				return answer("ĐÃ-KHỞI-ĐỘNG-LẠI"), nil
			case !hasToolResult(req):
				for i := 0; i < 400; i++ {
					if id := currentID(); id != "" && f.get(id)["status"] == "waiting_approval" {
						break
					}
					time.Sleep(25 * time.Millisecond)
				}
				return toolUse("infra.list_containers"), nil
			}
			once.Do(func() { close(otherDone) })
			return answer("FINDING-monitoring"), nil
		})
		started := f.mustStart(mon, fix)
		idMu.Lock()
		execID = started
		idMu.Unlock()
		// Task monitoring gọi tool và xong TRONG LÚC execution chờ duyệt: Gateway vẫn phục
		// vụ task đang chạy của một execution có task khác đang chờ.
		select {
		case <-otherDone:
		case <-time.After(30 * time.Second):
			t.Fatalf("task không chờ duyệt bị chặn theo: %s", f.taskStatuses(execID))
		}
		x := f.waitStatus(execID, 20*time.Second, "waiting_approval")
		if x["status"] != "waiting_approval" {
			t.Fatalf("execution: %v", x)
		}
		for deadline := time.Now().Add(10 * time.Second); f.tasks(execID)["monitoring"]["status"] != "completed" && time.Now().Before(deadline); {
			time.Sleep(25 * time.Millisecond)
		}
		if got := f.taskStatuses(execID); got != "fix=waiting_approval monitoring=completed" {
			t.Fatalf("task: %s", got)
		}
		// Task kia xong không kéo execution ra khỏi waiting_approval.
		if s := f.get(execID)["status"]; s != "waiting_approval" {
			t.Errorf("execution = %v khi vẫn còn task chờ duyệt", s)
		}
		pending := e.do("GET", "/v1/approvals?execution_id="+execID+"&status=pending", approverA, nil, nil).json(t)["data"].([]any)
		if len(pending) != 1 {
			t.Fatalf("approval đang chờ: %v", pending)
		}
		approval := pending[0].(map[string]any)
		if approval["task_id"] != f.tasks(execID)["fix"]["id"] {
			t.Errorf("approval gắn sai task: %v", approval)
		}
		r := e.do("POST", "/v1/approvals/"+approval["id"].(string)+"/decisions", approverA, map[string]any{"decision": "approve"},
			map[string]string{"Idempotency-Key": uuid.NewString()})
		want(t, r, 200, "")
		x = f.waitStatus(execID, 30*time.Second, plannedTerminal...)
		if x["status"] != "completed" || f.taskStatuses(execID) != "fix=completed monitoring=completed" {
			t.Fatalf("execution: %v; task: %s", x, f.taskStatuses(execID))
		}
		f.up.mu.Lock()
		restarts := 0
		for _, c := range f.up.calls {
			if strings.HasPrefix(c, "restart_container ") {
				restarts++
			}
		}
		f.up.mu.Unlock()
		if restarts != 1 {
			t.Errorf("restart_container chạy %d lần, muốn 1", restarts)
		}
		// Hai task không có task nào phụ thuộc: kết quả của execution gồm cả hai.
		msg := fmt.Sprint(x["output"].(map[string]any)["message"])
		if !strings.Contains(msg, "## monitoring\n\nFINDING-monitoring") || !strings.Contains(msg, "## fix\n\nĐÃ-KHỞI-ĐỘNG-LẠI") {
			t.Errorf("output.message: %q", msg)
		}
	})

	t.Run("a task's budget share is enforced on that task only", func(t *testing.T) {
		e.model.script(func(_ context.Context, _ int, req provider.Request) (provider.Response, error) {
			if agentOf(req) == "database" && !hasToolResult(req) {
				return toolUse("infra.list_containers"), nil
			}
			return answer("FINDING-" + agentOf(req)), nil
		})
		// Trần chi phí một lượt là 4096 × 15 / 1e6 = 0.06144 USD: budget 0.063 đủ đặt trước
		// cho lượt đầu của database; sau khi lượt đó tiêu 0.0045, không đủ cho lượt thứ hai.
		db := planTask("database", f.agents["database"])
		db["budget"] = map[string]any{"amount": "0.063", "currency": "USD"}
		mon := planTask("monitoring", f.agents["monitoring"])
		mon["budget"] = map[string]any{"amount": "0.5", "currency": "USD"}
		// Người khởi tạo khác: rate limit của Gateway tính theo principal, và các ca trước
		// đã dùng gần hết hạn mức một phút của admin-a.
		r := f.start(approverA, map[string]any{"tasks": []any{db, mon}}, map[string]any{"budget": map[string]any{"amount": "2", "currency": "USD"}})
		want(t, r, 202, "")
		id := r.json(t)["id"].(string)
		x := f.waitStatus(id, 30*time.Second, plannedTerminal...)
		if got := f.taskStatuses(id); got != "database=budget_exceeded monitoring=completed" {
			rows, _ := e.db.Admin.Query(ctx, `SELECT step_seq, kind, status, COALESCE(error::text,'') FROM execution_steps WHERE task_id = $1 ORDER BY step_seq`, f.tasks(id)["database"]["id"])
			for rows.Next() {
				var seq int
				var kind, st, er string
				_ = rows.Scan(&seq, &kind, &st, &er)
				t.Logf("step %d %s %s %s", seq, kind, st, er)
			}
			rows.Close()
			t.Fatalf("task: %s (%v) database: %v", got, x["status"], f.tasks(id)["database"]["error"])
		}
		if x["status"] != "budget_exceeded" || x["error"].(map[string]any)["code"] != "budget_exceeded" {
			t.Errorf("execution: %v", x)
		}
		if b := f.tasks(id)["database"]["budget"].(map[string]any); b["amount"] != "0.06300000" {
			t.Errorf("budget của task: %v", b)
		}
		// Budget nhỏ hơn trần chi phí của một lượt bị từ chối ngay khi tạo.
		db["budget"] = map[string]any{"amount": "0.01", "currency": "USD"}
		want(t, f.start(adminA, map[string]any{"tasks": []any{db}}, nil), 422, "validation_failed")
	})
}
