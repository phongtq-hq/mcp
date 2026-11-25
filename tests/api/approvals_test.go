package api_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hqgroups/mcp/internal/adminops"
	"github.com/hqgroups/mcp/internal/ai/provider"
	"github.com/hqgroups/mcp/internal/mcp/registry"
	"github.com/hqgroups/mcp/internal/shared/exectoken"
)

// TestApprovals: tool ghi và approval (security §4.3, §4.4). Tool ghi chạy tự do trên
// non-prod; tool destructive cần một người duyệt không phải người yêu cầu; approval
// gắn với đúng một hành động, có hạn, dùng một lần; tool ghi không chạy hai lần cho
// cùng một bước.
func TestApprovals(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	adminA, adminB, viewerA, devA, otherAdminA := e.tok("admin-a"), e.tok("admin-b"), e.tok("viewer-a"), e.tok("dev-a"), e.tok("multi-a")
	signer := testSigner
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
	up := newUpstream(t, "ops",
		readTool("list_services", "Liệt kê service."),
		writeTool("create_ticket", "Tạo ticket."),
		writeTool("set_replicas", "Đặt số replica."),
		writeTool("restart_service", "Khởi động lại service."),
		&mcp.Tool{Name: "rotate_secret", Description: "Xoay một secret.",
			InputSchema: schemaOf(`"name":{"type":"string"},"secret_name":{"type":"string"}`)})
	srv := adminops.MCPServer{OrgSlug: "org-a", Slug: "ops"}
	if _, err := adminops.RegisterManagedMCPServer(ctx, conn.Conn(), signer, srv, "Ops", up.srv.URL); err != nil {
		t.Fatal(err)
	}
	on := true
	for name, c := range map[string][2]string{"create_ticket": {"write", "non_idempotent"}, "set_replicas": {"write", "idempotent"},
		"restart_service": {"destructive", "non_idempotent"}, "rotate_secret": {"destructive", "non_idempotent"}} {
		risk, side := c[0], c[1]
		if err := adminops.ClassifyMCPTool(ctx, conn.Conn(), srv, name, registry.Classification{Enabled: &on, RiskLevel: &risk, SideEffect: &side}); err != nil {
			t.Fatal(err)
		}
	}
	serverID := e.do("GET", "/v1/mcp-servers", adminA, nil, nil).json(t)["data"].([]any)[0].(map[string]any)["id"].(string)
	provID := post(adminA, "/v1/model-providers", map[string]any{"slug": "local", "adapter": "claude-cli"}, 201)["id"].(string)
	modelID := post(adminA, "/v1/models", map[string]any{"provider_id": provID, "provider_model": "sonnet", "display_name": "Sonnet",
		"tier": "standard", "capabilities": map[string]any{"tool_calling": true, "context_window": 200000}}, 201)["id"].(string)
	post(adminA, "/v1/models/"+modelID+"/pricing", map[string]any{"currency": "USD", "input_per_mtok": "3.00", "output_per_mtok": "15.00",
		"effective_from": time.Now().UTC().Format(time.RFC3339Nano)}, 201)

	type fixture struct{ project, agent string }
	newAgent := func(slug, environment string, timeout int, toolEntry map[string]any) fixture {
		t.Helper()
		r := e.do("POST", "/v1/projects", adminA, map[string]any{"slug": slug, "name": slug, "environment": environment}, nil)
		if r.status != 201 {
			t.Fatalf("project: %d %s", r.status, r.body)
		}
		proj := r.json(t)["id"].(string)
		entry := map[string]any{"server_id": serverID, "allow": []string{"*"}}
		for k, v := range toolEntry {
			entry[k] = v
		}
		a := post(adminA, "/v1/projects/"+proj+"/agents", map[string]any{"slug": "ops", "name": "ops", "system_prompt": "Bạn vận hành hệ thống.",
			"config": map[string]any{"model": map[string]any{"policy": "fixed", "fixed_model_id": modelID},
				"limits": map[string]any{"max_iterations": 6, "max_tool_calls": 10, "timeout_seconds": timeout},
				"tools":  []any{entry}}}, 201)
		id := a["id"].(string)
		if r := e.do("PUT", "/v1/agents/"+id+"/active-version", adminA, map[string]any{"version": 1}, nil); r.status != 200 {
			t.Fatalf("publish: %d %s", r.status, r.body)
		}
		return fixture{project: proj, agent: id}
	}
	start := func(f fixture) string {
		t.Helper()
		r := e.do("POST", "/v1/projects/"+f.project+"/executions", adminA, map[string]any{"mode": "single_agent", "agent_id": f.agent,
			"input": map[string]any{"message": "xử lý sự cố"}}, map[string]string{"Idempotency-Key": uuid.NewString()})
		if r.status != 202 {
			t.Fatalf("start: %d %s", r.status, r.body)
		}
		return r.json(t)["id"].(string)
	}
	status := func(id string) string {
		return e.do("GET", "/v1/executions/"+id, adminA, nil, nil).json(t)["status"].(string)
	}
	waitStatus := func(id string, wantStatus ...string) map[string]any {
		t.Helper()
		var x map[string]any
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			x = e.do("GET", "/v1/executions/"+id, adminA, nil, nil).json(t)
			for _, s := range wantStatus {
				if x["status"] == s {
					return x
				}
			}
		}
		t.Fatalf("execution %s ở trạng thái %v, muốn %v (error %v)", id, x["status"], wantStatus, x["error"])
		return nil
	}
	terminal := []string{"completed", "failed", "cancelled", "timed_out", "budget_exceeded"}
	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := e.db.Admin.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	upstream := func(tool string) (calls int, keys []string) {
		up.mu.Lock()
		defer up.mu.Unlock()
		for i, c := range up.calls {
			if strings.HasPrefix(c, tool+" ") {
				calls++
				keys = append(keys, up.idemKeys[i])
			}
		}
		return calls, keys
	}
	// Model giả: lượt đầu gọi một tool, lượt sau trả lời kèm kết quả tool nó thấy.
	callThen := func(tool string, args string) {
		e.model.script(func(_ context.Context, n int, req provider.Request) (provider.Response, error) {
			if n == 0 {
				return provider.Response{StopReason: provider.StopToolUse, ModelUsed: "sonnet", Usage: provider.Usage{InputTokens: 10, OutputTokens: 5},
					ToolCalls: []provider.ToolCall{{ID: "c1", Name: "ops." + tool, Arguments: []byte(args)}}}, nil
			}
			last := req.Messages[len(req.Messages)-1]
			return answer(fmt.Sprintf("tool trả về: %s (lỗi=%v)", last.Text, last.IsError)), nil
		})
	}
	pendingOf := func(tok, execID string) []any {
		t.Helper()
		r := e.do("GET", "/v1/approvals?status=pending&execution_id="+execID, tok, nil, nil)
		if r.status != 200 {
			t.Fatalf("list approvals: %d %s", r.status, r.body)
		}
		return r.json(t)["data"].([]any)
	}
	decide := func(tok, id, decision string) resp {
		return e.do("POST", "/v1/approvals/"+id+"/decisions", tok, map[string]any{"decision": decision, "comment": "đã kiểm tra"},
			map[string]string{"Idempotency-Key": uuid.NewString()})
	}

	t.Run("a write tool runs freely on non-prod, once, with an idempotency key bound to its step", func(t *testing.T) {
		f := newAgent("write-free", "non_prod", 30, nil)
		callThen("create_ticket", `{"name":"db down"}`)
		id := start(f)
		x := waitStatus(id, terminal...)
		if x["status"] != "completed" || !strings.Contains(fmt.Sprint(x["output"]), "result of create_ticket") {
			t.Fatalf("execution: %v %v %v", x["status"], x["error"], x["output"])
		}
		calls, keys := upstream("create_ticket")
		var task uuid.UUID
		if err := e.db.Admin.QueryRow(ctx, `SELECT id FROM agent_tasks WHERE execution_id = $1`, id).Scan(&task); err != nil {
			t.Fatal(err)
		}
		if calls != 1 || keys[0] != task.String()+":2" {
			t.Errorf("upstream: %d lần, idempotency key %q, muốn 1 lần và %s:2", calls, keys, task)
		}
		if n := count(`SELECT count(*) FROM approvals WHERE execution_id = $1`, id); n != 0 {
			t.Errorf("tool ghi trên non-prod tạo %d approval", n)
		}
		if n := count(`SELECT count(*) FROM audit_logs WHERE action = 'mcp_tool.call' AND execution_id = $1 AND details->>'rule' = 'risk_write_non_prod' AND outcome = 'success'`, id); n != 1 {
			t.Errorf("audit của tool ghi: %d dòng", n)
		}
		if n := count(`SELECT count(*) FROM mcp_tool_claims WHERE task_id = $1 AND step_seq = 2`, task); n != 1 {
			t.Errorf("claim của tool ghi: %d dòng", n)
		}
	})

	var approvedID string
	t.Run("a destructive tool waits for an approver who is not the requester; then runs exactly once", func(t *testing.T) {
		f := newAgent("destructive", "non_prod", 60, nil)
		callThen("restart_service", `{"name":"api"}`)
		id := start(f)
		waitStatus(id, "waiting_approval")
		if calls, _ := upstream("restart_service"); calls != 0 {
			t.Fatal("tool destructive đã chạy trước khi được duyệt")
		}
		list := pendingOf(otherAdminA, id)
		if len(list) != 1 {
			t.Fatalf("approval đang chờ: %d", len(list))
		}
		a := list[0].(map[string]any)
		approvedID = a["id"].(string)
		payload := a["payload"].(map[string]any)
		if a["risk_level"] != "destructive" || a["subject_type"] != "tool_call" || payload["tool_name"] != "ops.restart_service" ||
			payload["arguments"].(map[string]any)["name"] != "api" || a["required_approvals"] != float64(1) || a["requested_by"] == nil {
			t.Errorf("approval: %v", a)
		}
		// Step đang chờ ghi lại approval của nó.
		if n := count(`SELECT count(*) FROM execution_steps s JOIN agent_tasks t ON t.id = s.task_id
			WHERE t.execution_id = $1 AND s.kind = 'tool_call' AND s.status = 'started' AND s.approval_id = $2`, id, approvedID); n != 1 {
			t.Errorf("step đang chờ không ghi approval_id")
		}

		// Ai KHÔNG quyết định được.
		want(t, decide(adminA, approvedID, "approve"), 403, "permission_denied") // người yêu cầu
		// viewer không có approval:read: với họ approval này không tồn tại (404, không phải 403).
		want(t, decide(viewerA, approvedID, "approve"), 404, "not_found")
		want(t, decide(devA, approvedID, "approve"), 403, "permission_denied") // chỉ có approval:read
		want(t, decide(adminB, approvedID, "approve"), 404, "not_found")       // tenant khác
		want(t, e.do("GET", "/v1/approvals/"+approvedID, adminB, nil, nil), 404, "not_found")
		want(t, e.do("GET", "/v1/approvals/"+approvedID, viewerA, nil, nil), 404, "not_found")
		want(t, e.do("GET", "/v1/approvals/"+approvedID, devA, nil, nil), 200, "")
		if lb := e.do("GET", "/v1/approvals", adminB, nil, nil); lb.status != 200 || len(lb.json(t)["data"].([]any)) != 0 {
			t.Errorf("org B thấy approval của org A: %d %s", lb.status, lb.body)
		}
		if lb := e.do("GET", "/v1/approvals?execution_id="+id, adminB, nil, nil); len(lb.json(t)["data"].([]any)) != 0 {
			t.Errorf("org B lọc theo execution của org A thấy approval: %s", lb.body)
		}
		// assigned_to_me: người yêu cầu không thấy approval của chính mình; người khác thấy.
		if mine := e.do("GET", "/v1/approvals?assigned_to_me=true", adminA, nil, nil).json(t)["data"].([]any); len(mine) != 0 {
			t.Errorf("người yêu cầu thấy %d approval 'của mình cần duyệt'", len(mine))
		}
		if theirs := e.do("GET", "/v1/approvals?assigned_to_me=true", otherAdminA, nil, nil).json(t)["data"].([]any); len(theirs) != 1 {
			t.Errorf("người duyệt thấy %d approval cần duyệt", len(theirs))
		}
		// Yêu cầu sai.
		want(t, e.do("POST", "/v1/approvals/"+approvedID+"/decisions", otherAdminA, map[string]any{"decision": "approve"}, nil), 400, "idempotency_key_required")
		want(t, decide(otherAdminA, approvedID, "maybe"), 422, "validation_failed")
		if status(id) != "waiting_approval" {
			t.Fatalf("execution không còn chờ sau các quyết định bị từ chối: %s", status(id))
		}

		// Người duyệt hợp lệ duyệt: tool chạy đúng một lần, execution hoàn tất.
		r := decide(otherAdminA, approvedID, "approve")
		want(t, r, 200, "")
		if d := r.json(t); d["status"] != "approved" || len(d["decisions"].([]any)) != 1 {
			t.Errorf("sau khi duyệt: %v", d)
		}
		x := waitStatus(id, terminal...)
		if x["status"] != "completed" || !strings.Contains(fmt.Sprint(x["output"]), "result of restart_service") {
			t.Fatalf("execution: %v %v %v", x["status"], x["error"], x["output"])
		}
		if calls, _ := upstream("restart_service"); calls != 1 {
			t.Errorf("tool destructive chạy %d lần, muốn 1", calls)
		}
		// Dùng một lần: approval đã bị tiêu thụ; không quyết định lại được.
		if got := e.do("GET", "/v1/approvals/"+approvedID, otherAdminA, nil, nil).json(t); got["status"] != "consumed" {
			t.Errorf("approval sau khi thực thi: %v", got["status"])
		}
		want(t, decide(otherAdminA, approvedID, "reject"), 409, "approval_not_pending")
		for _, action := range []string{"approval.request", "approval.decide", "approval.consume"} {
			if n := count(`SELECT count(*) FROM audit_logs WHERE action = $1 AND resource_id = $2`, action, approvedID); n != 1 {
				t.Errorf("audit %s: %d dòng", action, n)
			}
		}
		if n := count(`SELECT count(*) FROM audit_logs WHERE action = 'mcp_tool.call' AND execution_id = $1 AND details->>'approval_id' = $2
			AND details->>'decision' = 'require_approval'`, id, approvedID); n != 1 {
			t.Errorf("audit của quyết định require_approval: %d dòng", n)
		}
	})

	t.Run("a rejected call never runs; the model is told and the execution goes on", func(t *testing.T) {
		f := newAgent("rejected", "non_prod", 60, nil)
		callThen("restart_service", `{"name":"db"}`)
		before, _ := upstream("restart_service")
		id := start(f)
		waitStatus(id, "waiting_approval")
		a := pendingOf(otherAdminA, id)[0].(map[string]any)["id"].(string)
		want(t, decide(otherAdminA, a, "reject"), 200, "")
		x := waitStatus(id, terminal...)
		after, _ := upstream("restart_service")
		if x["status"] != "completed" || after != before || !strings.Contains(fmt.Sprint(x["output"]), "lỗi=true") {
			t.Errorf("execution: %v %v; tool chạy thêm %d lần", x["status"], x["output"], after-before)
		}
		if n := count(`SELECT count(*) FROM execution_steps s JOIN agent_tasks t ON t.id = s.task_id
			WHERE t.execution_id = $1 AND s.kind = 'tool_call' AND s.status = 'denied'`, id); n != 1 {
			t.Errorf("step bị từ chối: %d", n)
		}
	})

	t.Run("an approval expires with the task; cancelling the execution cancels its approval", func(t *testing.T) {
		// Hạn của task 2 giây: approval hết hạn cùng lúc, execution kết thúc timed_out.
		f := newAgent("expires", "non_prod", 2, nil)
		callThen("restart_service", `{"name":"cache"}`)
		before, _ := upstream("restart_service")
		id := start(f)
		waitStatus(id, "waiting_approval")
		a := pendingOf(otherAdminA, id)[0].(map[string]any)["id"].(string)
		x := waitStatus(id, terminal...)
		if x["status"] != "timed_out" {
			t.Errorf("execution: %v %v", x["status"], x["error"])
		}
		if got := e.do("GET", "/v1/approvals/"+a, otherAdminA, nil, nil).json(t); got["status"] != "expired" {
			t.Errorf("approval quá hạn hiển thị là %v", got["status"])
		}
		want(t, decide(otherAdminA, a, "approve"), 409, "approval_not_pending")

		// Huỷ execution đang chờ: approval bị huỷ theo, tool không chạy.
		f = newAgent("cancelled", "non_prod", 60, nil)
		callThen("restart_service", `{"name":"cache"}`)
		id = start(f)
		waitStatus(id, "waiting_approval")
		a = pendingOf(otherAdminA, id)[0].(map[string]any)["id"].(string)
		if r := e.do("POST", "/v1/executions/"+id+"/cancel", adminA, nil, nil); r.status != 202 && r.status != 200 {
			t.Fatalf("cancel: %d %s", r.status, r.body)
		}
		if x := waitStatus(id, terminal...); x["status"] != "cancelled" {
			t.Errorf("execution: %v", x["status"])
		}
		if got := e.do("GET", "/v1/approvals/"+a, otherAdminA, nil, nil).json(t); got["status"] != "cancelled" {
			t.Errorf("approval của execution bị huỷ: %v", got["status"])
		}
		want(t, decide(otherAdminA, a, "approve"), 409, "approval_not_pending")
		if after, _ := upstream("restart_service"); after != before {
			t.Errorf("tool chạy %d lần dù approval hết hạn / bị huỷ", after-before)
		}
	})

	t.Run("the person behind an API key cannot approve what that key requested", func(t *testing.T) {
		// multi-a tạo một API key, khởi tạo execution bằng key, rồi thử tự duyệt bằng tài khoản của mình.
		f := newAgent("by-key", "non_prod", 60, nil)
		k := e.do("POST", "/v1/api-keys", otherAdminA, map[string]any{"name": "automation", "role": "operator"}, nil)
		if k.status != 201 {
			t.Fatalf("tạo API key: %d %s", k.status, k.body)
		}
		key := k.json(t)["key"].(string)
		callThen("restart_service", `{"name":"by-key"}`)
		before, _ := upstream("restart_service")
		r := e.do("POST", "/v1/projects/"+f.project+"/executions", key, map[string]any{"mode": "single_agent", "agent_id": f.agent,
			"input": map[string]any{"message": "xử lý"}}, map[string]string{"Idempotency-Key": uuid.NewString()})
		if r.status != 202 {
			t.Fatalf("start bằng API key: %d %s", r.status, r.body)
		}
		id := r.json(t)["id"].(string)
		waitStatus(id, "waiting_approval")
		a := pendingOf(adminA, id)[0].(map[string]any)["id"].(string)
		want(t, decide(otherAdminA, a, "approve"), 403, "permission_denied") // người tạo key = người yêu cầu
		if n, _ := upstream("restart_service"); n != before || status(id) != "waiting_approval" {
			t.Fatalf("người tạo key tự duyệt được: tool chạy %d lần, execution %s", n-before, status(id))
		}
		// Database cũng từ chối quyết định của chính người yêu cầu, kể cả khi code bị bỏ qua.
		var owner uuid.UUID
		if err := e.db.Admin.QueryRow(ctx, `SELECT requested_by_owner FROM approvals WHERE id = $1`, a).Scan(&owner); err != nil {
			t.Fatal(err)
		}
		if _, err := e.db.Admin.Exec(ctx, `UPDATE approvals SET status = 'approved', resolved_at = now(),
			decisions = decisions || jsonb_build_array(jsonb_build_object('user_id', $2::text, 'decision', 'approve')) WHERE id = $1`, a, owner); err == nil {
			t.Fatal("database nhận quyết định của chính người yêu cầu")
		}
		if _, err := e.db.Admin.Exec(ctx, `UPDATE approvals SET status = 'approved', resolved_at = now() WHERE id = $1`, a); err == nil {
			t.Fatal("database cho approved mà không có quyết định nào")
		}
		// Người khác duyệt thì được.
		want(t, decide(adminA, a, "approve"), 200, "")
		if x := waitStatus(id, terminal...); x["status"] != "completed" {
			t.Errorf("execution: %v %v", x["status"], x["error"])
		}
	})

	t.Run("the approver sees secrets redacted, but the tool runs with the exact arguments that were approved", func(t *testing.T) {
		f := newAgent("redacted", "non_prod", 60, nil)
		callThen("rotate_secret", `{"secret_name":"db-prod","name":"primary"}`)
		id := start(f)
		waitStatus(id, "waiting_approval")
		a := pendingOf(otherAdminA, id)[0].(map[string]any)
		payload := a["payload"].(map[string]any)
		shown := payload["arguments"].(map[string]any)
		if shown["name"] != "primary" || shown["secret_name"] == "db-prod" || payload["redactions"] != float64(1) {
			t.Errorf("người duyệt thấy: %v (redactions %v)", shown, payload["redactions"])
		}
		want(t, decide(otherAdminA, a["id"].(string), "approve"), 200, "")
		if x := waitStatus(id, terminal...); x["status"] != "completed" {
			t.Fatalf("execution: %v %v", x["status"], x["error"])
		}
		// Upstream nhận đúng arguments thật, không phải bản đã che.
		up.mu.Lock()
		var sent string
		for _, c := range up.calls {
			if strings.HasPrefix(c, "rotate_secret ") {
				sent = c
			}
		}
		up.mu.Unlock()
		if !strings.Contains(sent, `"secret_name":"db-prod"`) || strings.Contains(sent, "REDACTED") {
			t.Errorf("arguments gửi tới tool: %s", sent)
		}
	})

	t.Run("production is not open for write tools yet; an agent can require approval for a read tool", func(t *testing.T) {
		f := newAgent("prod", "prod", 30, nil)
		callThen("create_ticket", `{"name":"prod"}`)
		before, _ := upstream("create_ticket")
		id := start(f)
		x := waitStatus(id, terminal...)
		after, _ := upstream("create_ticket")
		if x["status"] != "completed" || after != before || !strings.Contains(fmt.Sprint(x["output"]), "lỗi=true") {
			t.Errorf("tool ghi trên prod: %v %v, chạy thêm %d lần", x["status"], x["output"], after-before)
		}
		if n := count(`SELECT count(*) FROM audit_logs WHERE action = 'mcp_tool.call' AND execution_id = $1 AND outcome = 'denied'
			AND details->>'rule' = 'risk_write_production_not_enabled'`, id); n != 1 {
			t.Errorf("audit từ chối trên prod: %d dòng", n)
		}
		// Tool đọc vẫn tự do trên prod.
		callThen("list_services", `{}`)
		if x := waitStatus(start(f), terminal...); x["status"] != "completed" || !strings.Contains(fmt.Sprint(x["output"]), "lỗi=false") {
			t.Errorf("tool đọc trên prod: %v %v", x["status"], x["output"])
		}

		// Agent Version đặt require_approval: always: tool đọc cũng phải chờ duyệt.
		strict := newAgent("strict", "non_prod", 60, map[string]any{"require_approval": "always"})
		callThen("list_services", `{}`)
		id = start(strict)
		waitStatus(id, "waiting_approval")
		a := pendingOf(otherAdminA, id)[0].(map[string]any)
		if a["risk_level"] != "read" {
			t.Errorf("approval do Agent yêu cầu: %v", a)
		}
		want(t, decide(otherAdminA, a["id"].(string), "approve"), 200, "")
		if x := waitStatus(id, terminal...); x["status"] != "completed" {
			t.Errorf("execution: %v %v", x["status"], x["error"])
		}
	})

	t.Run("a write tool never runs twice for the same step; an approval cannot be replayed for another action", func(t *testing.T) {
		// Giữ lượt model đầu để task ở trạng thái running, rồi gọi thẳng Gateway bằng
		// execution token như một worker chạy lại bước của nó.
		f := newAgent("replay", "non_prod", 60, nil)
		hold := make(chan struct{})
		e.model.script(func(ctx context.Context, n int, _ provider.Request) (provider.Response, error) {
			if n == 0 {
				select {
				case <-hold:
				case <-ctx.Done():
				}
			}
			return answer("xong"), nil
		})
		defer close(hold)
		id := start(f)
		waitStatus(id, "running")
		var claims exectoken.Claims
		if err := e.db.Admin.QueryRow(ctx, `SELECT t.org_id, x.project_id, x.id, t.id, t.agent_version_id, x.initiator_type, x.initiator_id
			FROM agent_tasks t JOIN agent_executions x ON x.id = t.execution_id WHERE x.id = $1`, id).Scan(&claims.OrgID, &claims.ProjectID,
			&claims.ExecutionID, &claims.TaskID, &claims.AgentVersionID, &claims.InitiatorType, &claims.InitiatorID); err != nil {
			t.Fatal(err)
		}
		iss, _ := exectoken.New(execTokenKey)
		token, _ := iss.Sign(claims, time.Now(), time.Now().Add(time.Minute))
		call := func(tool string, args map[string]any, step int) (code string, isErr bool) {
			t.Helper()
			client := mcp.NewClient(&mcp.Implementation{Name: "test-worker", Version: "1"}, nil)
			sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: e.gw.URL + "/mcp/internal/v1/tasks/" + claims.TaskID.String(),
				HTTPClient: &http.Client{Transport: bearer{token}}, DisableStandaloneSSE: true, MaxRetries: -1}, nil)
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			defer func() { _ = sess.Close() }()
			res, err := sess.CallTool(ctx, &mcp.CallToolParams{Meta: mcp.Meta{"hq/step_seq": step}, Name: "ops." + tool, Arguments: args})
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			code, _ = res.Meta["hq/code"].(string)
			return code, res.IsError
		}
		// Tool ghi không idempotent: lần hai của cùng bước không chạy, trả outcome_unknown.
		before, _ := upstream("create_ticket")
		if code, isErr := call("create_ticket", map[string]any{"name": "x"}, 10); code != "" || isErr {
			t.Fatalf("lần đầu: code %q, lỗi %v", code, isErr)
		}
		if code, _ := call("create_ticket", map[string]any{"name": "x"}, 10); code != "outcome_unknown" {
			t.Errorf("lần hai của cùng bước: code %q, muốn outcome_unknown", code)
		}
		if after, _ := upstream("create_ticket"); after != before+1 {
			t.Errorf("tool ghi chạy %d lần cho một bước", after-before)
		}
		// Tool khai idempotent: chạy lại được, với CÙNG idempotency key; arguments khác thì không.
		if code, _ := call("set_replicas", map[string]any{"name": "api"}, 11); code != "" {
			t.Fatalf("idempotent lần đầu: %q", code)
		}
		if code, _ := call("set_replicas", map[string]any{"name": "api"}, 11); code != "" {
			t.Errorf("idempotent lần hai: %q, muốn được chạy lại", code)
		}
		if code, _ := call("set_replicas", map[string]any{"name": "other"}, 11); code != "outcome_unknown" {
			t.Errorf("cùng bước, arguments khác: %q, muốn outcome_unknown", code)
		}
		if _, keys := upstream("set_replicas"); len(keys) < 2 || keys[len(keys)-1] != keys[len(keys)-2] || keys[len(keys)-1] != claims.TaskID.String()+":11" {
			t.Errorf("idempotency key của hai lần gọi: %v", keys)
		}
		// Tool ghi không kèm tham chiếu bước bị từ chối.
		client := mcp.NewClient(&mcp.Implementation{Name: "test-worker", Version: "1"}, nil)
		sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: e.gw.URL + "/mcp/internal/v1/tasks/" + claims.TaskID.String(),
			HTTPClient: &http.Client{Transport: bearer{token}}, DisableStandaloneSSE: true, MaxRetries: -1}, nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "ops.create_ticket", Arguments: map[string]any{"name": "y"}})
		_ = sess.Close()
		if err != nil || res.Meta["hq/code"] != "denied" {
			t.Errorf("thiếu step_seq: %+v %v", res, err)
		}

		// Approval gắn với đúng hành động: bước 20 xin duyệt restart "api"; sau khi được
		// duyệt, cùng bước đó với arguments khác KHÔNG dùng được approval ấy.
		beforeRestart, _ := upstream("restart_service")
		if code, _ := call("restart_service", map[string]any{"name": "api"}, 20); code != "approval_required" {
			t.Fatalf("destructive: %q", code)
		}
		a := pendingOf(otherAdminA, id)[0].(map[string]any)["id"].(string)
		want(t, decide(otherAdminA, a, "approve"), 200, "")
		if code, _ := call("restart_service", map[string]any{"name": "payments"}, 20); code != "approval_denied" {
			t.Errorf("arguments bị đổi sau khi duyệt: %q, muốn approval_denied", code)
		}
		// Tool destructive khác ở cùng bước cũng không dùng được approval đó (approval ghi tool của nó).
		// (Không thử một tool ghi không cần duyệt ở bước này: nó sẽ chạy hợp lệ và chiếm bước.)
		if n, _ := upstream("restart_service"); n != beforeRestart {
			t.Fatal("tool destructive đã chạy với arguments không được duyệt")
		}
		// Đúng hành động đã duyệt thì chạy, đúng một lần; gọi lại không chạy lần hai.
		if code, isErr := call("restart_service", map[string]any{"name": "api"}, 20); code != "" || isErr {
			t.Errorf("hành động đã duyệt: code %q, lỗi %v", code, isErr)
		}
		if code, _ := call("restart_service", map[string]any{"name": "api"}, 20); code != "outcome_unknown" {
			t.Errorf("dùng lại approval đã tiêu thụ: %q", code)
		}
		if n, _ := upstream("restart_service"); n != beforeRestart+1 {
			t.Errorf("tool destructive chạy %d lần", n-beforeRestart)
		}
		// Trần max_tool_calls của Agent Version (10) được Gateway áp trên số bước nó đã cho
		// chạy: một worker không tự đếm vẫn không vượt được trần.
		// (Dùng một phiên cho cả loạt để không chạm rate limit của Gateway. Bước 10, 11 và
		// 20 ở trên đã giữ 3 chỗ; 9 lần gọi tiếp theo: 7 lần chạy, 2 lần bị từ chối.)
		ticketsBefore, _ := upstream("create_ticket")
		denied := 0
		bulk, err := mcp.NewClient(&mcp.Implementation{Name: "test-worker", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint:   e.gw.URL + "/mcp/internal/v1/tasks/" + claims.TaskID.String(),
			HTTPClient: &http.Client{Transport: bearer{token}}, DisableStandaloneSSE: true, MaxRetries: -1}, nil)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		for step := 100; step < 109; step++ {
			res, err := bulk.CallTool(ctx, &mcp.CallToolParams{Meta: mcp.Meta{"hq/step_seq": step}, Name: "ops.create_ticket", Arguments: map[string]any{"name": "spam"}})
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			if res.Meta["hq/code"] == "denied" {
				denied++
			}
		}
		_ = bulk.Close()
		ticketsAfter, _ := upstream("create_ticket")
		var held int
		if err := e.db.Admin.QueryRow(ctx, `SELECT count(*) FROM mcp_tool_claims WHERE task_id = $1`, claims0(t, e, id)).Scan(&held); err != nil {
			t.Fatal(err)
		}
		if held != 10 || denied != 2 || ticketsAfter-ticketsBefore != 7 {
			t.Errorf("trần tool call ở Gateway: %d claim, %d lần bị từ chối, tool chạy thêm %d lần", held, denied, ticketsAfter-ticketsBefore)
		}
		if n := count(`SELECT count(*) FROM audit_logs WHERE action = 'mcp_tool.call' AND execution_id = $1 AND details->>'rule' = 'max_tool_calls'`, id); n != denied {
			t.Errorf("audit từ chối vì trần tool call: %d dòng, muốn %d", n, denied)
		}
	})
	_ = approvedID
}

// claims0 trả về id của task của một execution.
func claims0(t *testing.T, e *env, execID string) uuid.UUID {
	t.Helper()
	var task uuid.UUID
	if err := e.db.Admin.QueryRow(context.Background(), `SELECT id FROM agent_tasks WHERE execution_id = $1`, execID).Scan(&task); err != nil {
		t.Fatal(err)
	}
	return task
}
