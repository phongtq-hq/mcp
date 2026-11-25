package api_test

import (
	"bytes"
	"context"
	historypb "go.temporal.io/api/history/v1"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/hqgroups/mcp/internal/agent/runtime/durable"
	"github.com/hqgroups/mcp/internal/ai/provider"
	"github.com/hqgroups/mcp/internal/workflow/definitions/schedule"
)

// workflowDef là định nghĩa: a → pause (chờ) → b, với b cũng phụ thuộc a để thấy kết quả của nó.
func workflowDef(a, b string, wait int) map[string]any {
	return map[string]any{"steps": []any{
		map[string]any{"id": "check", "type": "agent", "agent_id": a, "goal": "GOAL-check"},
		map[string]any{"id": "pause", "type": "wait", "seconds": wait, "depends_on": []string{"check"}},
		map[string]any{"id": "report", "type": "agent", "agent_id": b, "goal": "GOAL-report", "depends_on": []string{"pause", "check"}},
	}}
}

// gateDef là định nghĩa: a → gate (cổng chờ người duyệt) → b.
func gateDef(a, b string, required int) map[string]any {
	return map[string]any{"steps": []any{
		map[string]any{"id": "check", "type": "agent", "agent_id": a, "goal": "GOAL-check"},
		map[string]any{"id": "gate", "type": "approval", "message": "DUYET-khac-phuc", "required_approvals": required, "timeout_seconds": 600,
			"depends_on": []string{"check"}},
		map[string]any{"id": "report", "type": "agent", "agent_id": b, "goal": "GOAL-report", "depends_on": []string{"gate", "check"}},
	}}
}

// waitGate chờ lần chạy dừng ở cổng duyệt và trả về approval đang chờ của nó.
func (f *plannedFixture) waitGate(runID string, within time.Duration) map[string]any {
	f.t.Helper()
	var x map[string]any
	for deadline := time.Now().Add(within); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		x = f.e.do("GET", "/v1/workflow-executions/"+runID, f.e.tok("admin-a"), nil, nil).json(f.t)
		if x["status"] != "waiting_approval" {
			continue
		}
		pending := f.e.do("GET", "/v1/approvals?execution_id="+x["execution_id"].(string)+"&status=pending", f.e.tok("multi-a"), nil, nil).json(f.t)["data"].([]any)
		if len(pending) == 1 {
			return pending[0].(map[string]any)
		}
	}
	f.t.Fatalf("lần chạy %s không dừng ở cổng duyệt: %v", runID, x["status"])
	return nil
}

// closedHistoryOf chờ workflow đóng hẳn rồi đọc lịch sử của nó: lịch sử mẫu phải có cả
// sự kiện kết thúc, không dừng giữa chừng.
func (e *env) closedHistoryOf(workflowID string) *historypb.History {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_ = e.temporal.GetWorkflow(ctx, workflowID, "").Get(ctx, nil)
	return e.historyOf(workflowID)
}

func (f *plannedFixture) decide(tok, approvalID, decision string) resp {
	return f.e.do("POST", "/v1/approvals/"+approvalID+"/decisions", tok, map[string]any{"decision": decision},
		map[string]string{"Idempotency-Key": uuid.NewString()})
}

func (f *plannedFixture) createWorkflow(tok, slug string, def map[string]any) resp {
	return f.e.do("POST", "/v1/projects/"+f.project+"/workflows", tok, map[string]any{"slug": slug, "name": "Workflow " + slug, "definition": def}, nil)
}

func (f *plannedFixture) activate(id string) {
	f.t.Helper()
	if r := f.e.do("PATCH", "/v1/workflows/"+id, f.e.tok("admin-a"), map[string]any{"status": "active"}, nil); r.status != 200 {
		f.t.Fatalf("activate: %d %s", r.status, r.body)
	}
}

func (f *plannedFixture) runWorkflow(tok, id string) resp {
	return f.e.do("POST", "/v1/workflows/"+id+"/executions", tok, map[string]any{"input": map[string]any{"message": "GHI-CHU-lan-chay"}},
		map[string]string{"Idempotency-Key": uuid.NewString()})
}

func (f *plannedFixture) waitRun(id string, within time.Duration) map[string]any {
	f.t.Helper()
	var x map[string]any
	for deadline := time.Now().Add(within); time.Now().Before(deadline); time.Sleep(25 * time.Millisecond) {
		x = f.e.do("GET", "/v1/workflow-executions/"+id, f.e.tok("admin-a"), nil, nil).json(f.t)
		switch x["status"] {
		case "completed", "failed", "cancelled", "budget_exceeded", "timed_out":
			return x
		}
	}
	f.t.Fatalf("lần chạy %s ở trạng thái %v", id, x["status"])
	return nil
}

func TestWorkflows(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	adminA, adminB, viewerA := e.tok("admin-a"), e.tok("admin-b"), e.tok("viewer-a")
	f := newPlannedFixture(t, e, "workflow-a")
	mon := f.agent("monitoring", []string{"list_containers"}, nil)
	rep := f.agent("reporter", nil, nil)
	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := e.db.Admin.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	t.Run("definitions are validated as data; nothing is repaired", func(t *testing.T) {
		step := func(over map[string]any) map[string]any {
			s := map[string]any{"id": "a", "type": "agent", "agent_id": mon, "goal": "g"}
			for k, v := range over {
				if v == nil {
					delete(s, k)
				} else {
					s[k] = v
				}
			}
			return s
		}
		for name, def := range map[string]map[string]any{
			"không có bước":          {"steps": []any{}},
			"chỉ có bước chờ":        {"steps": []any{map[string]any{"id": "w", "type": "wait", "seconds": 5}}},
			"bước tool chưa hỗ trợ":  {"steps": []any{step(nil), map[string]any{"id": "t", "type": "tool", "tool_id": uuid.NewString()}}},
			"bước approval":          {"steps": []any{step(nil), map[string]any{"id": "g", "type": "approval"}}},
			"loại lạ":                {"steps": []any{step(map[string]any{"type": "loop"})}},
			"thiếu agent_id":         {"steps": []any{step(map[string]any{"agent_id": nil})}},
			"thiếu goal":             {"steps": []any{step(map[string]any{"goal": nil})}},
			"chờ không có seconds":   {"steps": []any{step(nil), map[string]any{"id": "w", "type": "wait"}}},
			"chờ quá lâu":            {"steps": []any{step(nil), map[string]any{"id": "w", "type": "wait", "seconds": 3601}}},
			"chờ có goal":            {"steps": []any{step(nil), map[string]any{"id": "w", "type": "wait", "seconds": 5, "goal": "x"}}},
			"duyệt không thông điệp": {"steps": []any{step(nil), map[string]any{"id": "g", "type": "approval"}}},
			"duyệt cần ba người":     {"steps": []any{step(nil), map[string]any{"id": "g", "type": "approval", "message": "m", "required_approvals": 3}}},
			"duyệt hạn quá ngắn":     {"steps": []any{step(nil), map[string]any{"id": "g", "type": "approval", "message": "m", "timeout_seconds": 30}}},
			"duyệt có goal":          {"steps": []any{step(nil), map[string]any{"id": "g", "type": "approval", "message": "m", "goal": "x"}}},
			"chờ có message":         {"steps": []any{step(nil), map[string]any{"id": "w", "type": "wait", "seconds": 5, "message": "m"}}},
			"duyệt mà vẫn chạy tiếp": {"steps": []any{step(nil), map[string]any{"id": "g", "type": "approval", "message": "m", "on_failure": "continue"}}},
			"chu trình":              {"steps": []any{step(map[string]any{"depends_on": []string{"b"}}), map[string]any{"id": "b", "type": "agent", "agent_id": mon, "goal": "g", "depends_on": []string{"a"}}}},
			"phụ thuộc không có":     {"steps": []any{step(map[string]any{"depends_on": []string{"zz"}})}},
			"khoá lạ trong bước":     {"steps": []any{step(map[string]any{"input": map[string]any{"x": "{{steps.a.output}}"}})}},
			"khoá lạ ở gốc":          {"steps": []any{step(nil)}, "variables": map[string]any{}},
			"budget sai tiền tệ":     {"steps": []any{step(map[string]any{"budget": map[string]any{"amount": "1", "currency": "VND"}})}},
			"song song quá lớn":      {"steps": []any{step(nil)}, "max_parallel_steps": 9},
		} {
			if r := f.createWorkflow(adminA, "bad-"+uuid.NewString()[:8], def); r.status != 422 || r.code(t) != "validation_failed" {
				t.Errorf("%s: %d %s", name, r.status, r.body)
			}
		}
		want(t, f.createWorkflow(adminA, "Bad Slug", workflowDef(mon, rep, 1)), 422, "validation_failed")
		want(t, f.createWorkflow(viewerA, "viewer-wf", workflowDef(mon, rep, 1)), 403, "permission_denied")
		if n := count(`SELECT count(*) FROM workflows WHERE project_id = $1`, f.project); n != 0 {
			t.Errorf("định nghĩa bị từ chối vẫn tạo %d workflow", n)
		}
	})

	var wfID string
	t.Run("create, read, update with versions and concurrency control, archive", func(t *testing.T) {
		r := f.createWorkflow(adminA, "daily-check", workflowDef(mon, rep, 1))
		want(t, r, 201, "")
		wf := r.json(t)
		wfID = wf["id"].(string)
		if wf["status"] != "draft" || wf["version"] != float64(1) || r.header.Get("ETag") == "" || r.header.Get("Location") != "/v1/workflows/"+wfID {
			t.Fatalf("workflow mới: %s", r.body)
		}
		want(t, f.createWorkflow(adminA, "daily-check", workflowDef(mon, rep, 1)), 409, "slug_taken")
		// Tenant khác và người không có quyền.
		want(t, e.do("GET", "/v1/workflows/"+wfID, adminB, nil, nil), 404, "not_found")
		want(t, e.do("PATCH", "/v1/workflows/"+wfID, adminB, map[string]any{"name": "x"}, nil), 404, "not_found")
		want(t, e.do("DELETE", "/v1/workflows/"+wfID, adminB, nil, nil), 404, "not_found")
		want(t, e.do("POST", "/v1/workflows/"+wfID+"/executions", adminB, nil, map[string]string{"Idempotency-Key": uuid.NewString()}), 404, "not_found")
		want(t, e.do("GET", "/v1/workflows/"+wfID, viewerA, nil, nil), 200, "")
		want(t, e.do("PATCH", "/v1/workflows/"+wfID, viewerA, map[string]any{"name": "x"}, nil), 403, "permission_denied")
		if list := e.do("GET", "/v1/projects/"+f.project+"/workflows", adminA, nil, nil).json(t)["data"].([]any); len(list) != 1 {
			t.Errorf("danh sách: %d workflow", len(list))
		}

		// Chưa kích hoạt thì không chạy được.
		want(t, f.runWorkflow(adminA, wfID), 409, "workflow_not_active")
		// Sửa tên không tăng version; sửa definition tăng version; ETag cũ bị từ chối.
		etag := r.header.Get("ETag")
		r = e.do("PATCH", "/v1/workflows/"+wfID, adminA, map[string]any{"name": "Kiểm tra hằng ngày"}, map[string]string{"If-Match": etag})
		want(t, r, 200, "")
		if r.json(t)["version"] != float64(1) {
			t.Errorf("đổi tên làm tăng version: %s", r.body)
		}
		want(t, e.do("PATCH", "/v1/workflows/"+wfID, adminA, map[string]any{"name": "x"}, map[string]string{"If-Match": etag}), 412, "precondition_failed")
		r = e.do("PATCH", "/v1/workflows/"+wfID, adminA, map[string]any{"definition": workflowDef(mon, rep, 2)}, nil)
		want(t, r, 200, "")
		if r.json(t)["version"] != float64(2) {
			t.Errorf("đổi definition không tăng version: %s", r.body)
		}
		want(t, e.do("PATCH", "/v1/workflows/"+wfID, adminA, map[string]any{"definition": map[string]any{"steps": []any{}}}, nil), 422, "validation_failed")
		want(t, e.do("PATCH", "/v1/workflows/"+wfID, adminA, map[string]any{"status": "archived"}, nil), 422, "validation_failed")
		// Môi trường test không cấu hình lịch chạy: schedule bị từ chối thay vì được lưu mà không có tác dụng.
		want(t, e.do("PATCH", "/v1/workflows/"+wfID, adminA, map[string]any{"schedule": "0 7 * * *"}, nil), 422, "validation_failed")
		// Kích hoạt ghi người gọi làm người chạy của workflow.
		f.activate(wfID)
		if n := count(`SELECT count(*) FROM workflows w JOIN audit_logs a ON a.resource_id = w.id AND a.action = 'workflow.update'
			WHERE w.id = $1 AND w.status = 'active' AND w.run_as IS NOT NULL AND a.details->>'run_as' = w.run_as::text`, wfID); n != 1 {
			t.Errorf("kích hoạt không ghi người chạy (%d)", n)
		}
	})

	t.Run("a run is a planned execution: steps in order, a wait in between, results flow along dependencies", func(t *testing.T) {
		var mu sync.Mutex
		at := map[string]time.Time{}
		e.model.script(func(_ context.Context, _ int, req provider.Request) (provider.Response, error) {
			mu.Lock()
			at[agentOf(req)] = time.Now()
			mu.Unlock()
			return answer("KET-QUA-" + agentOf(req)), nil
		})
		// Rút thời gian chờ xuống 1 giây cho lần chạy này.
		want(t, e.do("PATCH", "/v1/workflows/"+wfID, adminA, map[string]any{"definition": workflowDef(mon, rep, 1)}, nil), 200, "")
		want(t, e.do("POST", "/v1/workflows/"+wfID+"/executions", adminA, nil, nil), 400, "idempotency_key_required")
		want(t, f.runWorkflow(viewerA, wfID), 403, "permission_denied")
		r := f.runWorkflow(adminA, wfID)
		want(t, r, 202, "")
		run := r.json(t)
		runID := run["id"].(string)
		if run["status"] != "queued" || run["trigger_type"] != "manual" || run["workflow_version"] != float64(3) || run["execution_id"] == nil {
			t.Fatalf("lần chạy mới: %s", r.body)
		}
		done := f.waitRun(runID, 30*time.Second)
		if done["status"] != "completed" || done["output"].(map[string]any)["message"] != "KET-QUA-reporter" || done["finished_at"] == nil {
			t.Fatalf("lần chạy: %v", done)
		}
		mu.Lock()
		gap := at["reporter"].Sub(at["monitoring"])
		mu.Unlock()
		if gap < time.Second {
			t.Errorf("bước sau bước chờ bắt đầu sau %s, phải ít nhất 1 giây", gap)
		}
		// Bước report thấy tóm tắt của bước check (nó phụ thuộc trực tiếp), trong khối dữ liệu.
		var reportGoal string
		for _, req := range e.model.seen() {
			if agentOf(req) == "reporter" {
				reportGoal = req.Messages[0].Text
			}
		}
		if !strings.HasPrefix(reportGoal, "GOAL-report") || !strings.Contains(reportGoal, `<task_result id="check" status="completed">`+"\nKET-QUA-monitoring") ||
			strings.Contains(reportGoal, "GHI-CHU-lan-chay") {
			t.Errorf("goal của bước report: %q", reportGoal)
		}
		// Lần chạy là một execution planned gắn với workflow; bước chờ có trạng thái riêng.
		execID := done["execution_id"].(string)
		x := f.get(execID)
		if x["mode"] != "planned" || x["status"] != "completed" {
			t.Fatalf("execution của lần chạy: %v", x)
		}
		var kinds []string
		for _, raw := range x["output"].(map[string]any)["tasks"].([]any) {
			m := raw.(map[string]any)
			kinds = append(kinds, m["id"].(string)+":"+m["status"].(string))
		}
		if strings.Join(kinds, " ") != "check:completed pause:completed report:completed" {
			t.Errorf("các bước: %v", kinds)
		}
		if n := count(`SELECT count(*) FROM execution_plan_steps WHERE execution_id = $1 AND kind = 'wait' AND status = 'completed' AND finished_at - started_at >= interval '1 second'`, execID); n != 1 {
			t.Errorf("bước chờ không được ghi đúng (%d)", n)
		}
		if n := count(`SELECT count(*) FROM agent_executions WHERE id = $1 AND workflow_execution_id = $2 AND plan_source = 'caller'`, execID, runID); n != 1 {
			t.Errorf("execution không trỏ về lần chạy workflow")
		}
		// Sửa workflow sau đó không đổi bản chụp của lần chạy đã có.
		want(t, e.do("PATCH", "/v1/workflows/"+wfID, adminA, map[string]any{"definition": workflowDef(rep, mon, 1)}, nil), 200, "")
		if n := count(`SELECT count(*) FROM workflow_executions WHERE id = $1 AND workflow_version = 3 AND definition_snapshot->'steps'->0->>'agent_id' = $2`, runID, mon); n != 1 {
			t.Errorf("bản chụp của lần chạy bị đổi theo workflow")
		}
		// Danh sách lần chạy, quyền đọc và cách ly tenant.
		runs := e.do("GET", "/v1/workflows/"+wfID+"/executions", viewerA, nil, nil).json(t)["data"].([]any)
		if len(runs) != 1 || runs[0].(map[string]any)["status"] != "completed" {
			t.Errorf("danh sách lần chạy: %v", runs)
		}
		// Viewer (chỉ execution:read) thấy trạng thái nhưng không thấy input / output của lần
		// chạy: chúng chiếu lại nội dung của execution, có thể trích dữ liệu nhạy cảm.
		if m := runs[0].(map[string]any); m["output"] != nil || m["input"] != nil {
			t.Errorf("viewer thấy nội dung lần chạy trong danh sách: %v", m)
		}
		if m := e.do("GET", "/v1/workflow-executions/"+runID, viewerA, nil, nil).json(t); m["output"] != nil || m["input"] != nil || m["status"] != "completed" {
			t.Errorf("viewer thấy nội dung lần chạy: %v", m)
		}
		if m := e.do("GET", "/v1/workflow-executions/"+runID, adminA, nil, nil).json(t); m["output"] == nil {
			t.Errorf("người có execution:read_content phải thấy output: %v", m)
		}
		want(t, e.do("GET", "/v1/workflow-executions/"+runID, adminB, nil, nil), 404, "not_found")
		want(t, e.do("GET", "/v1/workflows/"+wfID+"/executions", adminB, nil, nil), 404, "not_found")
		want(t, e.do("POST", "/v1/workflow-executions/"+runID+"/cancel", adminB, nil, nil), 404, "not_found")
		want(t, e.do("POST", "/v1/workflow-executions/"+runID+"/cancel", adminA, nil, nil), 409, "execution_finished")
		if n := count(`SELECT count(*) FROM audit_logs WHERE org_id = $1 AND action = 'workflow.run' AND resource_id = $2`, e.orgA, runID); n != 1 {
			t.Errorf("audit workflow.run: %d", n)
		}
	})

	t.Run("whoever changes what an active workflow runs becomes its runner", func(t *testing.T) {
		runner := func() string {
			var sub string
			if err := e.db.Admin.QueryRow(context.Background(), `SELECT u.idp_subject FROM workflows w JOIN users u ON u.id = w.run_as WHERE w.id = $1`, wfID).Scan(&sub); err != nil {
				t.Fatal(err)
			}
			return sub
		}
		if got := runner(); got != "admin-a" {
			t.Fatalf("người chạy ban đầu: %s", got)
		}
		devA := e.tok("dev-a")
		// Đổi tên không đổi thứ workflow chạy: người chạy giữ nguyên.
		want(t, e.do("PATCH", "/v1/workflows/"+wfID, devA, map[string]any{"name": "Tên mới"}, nil), 200, "")
		if got := runner(); got != "admin-a" {
			t.Errorf("đổi tên làm đổi người chạy thành %s", got)
		}
		// Đổi definition của workflow đang active: người sửa chịu trách nhiệm về thứ nó chạy.
		want(t, e.do("PATCH", "/v1/workflows/"+wfID, devA, map[string]any{"definition": workflowDef(mon, rep, 3)}, nil), 200, "")
		if got := runner(); got != "dev-a" {
			t.Errorf("sửa definition của workflow active: người chạy là %s, muốn dev-a", got)
		}
		if n := count(`SELECT count(*) FROM audit_logs WHERE action = 'workflow.update' AND resource_id = $1 AND details->>'fields' LIKE '%run_as%'`, wfID); n != 2 { // lần kích hoạt đầu và lần sửa này
			t.Errorf("audit đổi người chạy: %d", n)
		}
		// Kích hoạt lại một workflow đã active là nhận lại vai trò người chạy.
		want(t, e.do("PATCH", "/v1/workflows/"+wfID, adminA, map[string]any{"status": "active"}, nil), 200, "")
		if got := runner(); got != "admin-a" {
			t.Errorf("kích hoạt lại: người chạy là %s, muốn admin-a", got)
		}
		// API key không là người chạy được: nó sửa được tên, không sửa được thứ workflow active chạy, không kích hoạt được.
		k := e.do("POST", "/v1/api-keys", adminA, map[string]any{"name": "wf-editor", "role": "developer"}, nil)
		want(t, k, 201, "")
		key := k.json(t)["key"].(string)
		want(t, e.do("PATCH", "/v1/workflows/"+wfID, key, map[string]any{"name": "Tên của key"}, nil), 200, "")
		want(t, e.do("PATCH", "/v1/workflows/"+wfID, key, map[string]any{"definition": workflowDef(rep, mon, 4)}, nil), 422, "validation_failed")
		want(t, e.do("PATCH", "/v1/workflows/"+wfID, key, map[string]any{"status": "active"}, nil), 422, "validation_failed")
		if got := runner(); got != "admin-a" {
			t.Errorf("API key đổi được người chạy thành %s", got)
		}
		// Người không đọc được project không thấy lần chạy.
		runs := e.do("GET", "/v1/workflows/"+wfID+"/executions", adminA, nil, nil).json(t)["data"].([]any)
		if len(runs) == 0 {
			t.Fatal("không có lần chạy để thử")
		}
		want(t, e.do("GET", "/v1/workflow-executions/"+runs[0].(map[string]any)["id"].(string), e.tok("norole-a"), nil, nil), 404, "not_found")
	})

	t.Run("a wait step whose dependency did not complete is skipped, and so is what depends on it", func(t *testing.T) {
		r := f.createWorkflow(adminA, "skip-wait", workflowDef(mon, rep, 600))
		want(t, r, 201, "")
		id := r.json(t)["id"].(string)
		f.activate(id)
		e.model.script(func(_ context.Context, _ int, req provider.Request) (provider.Response, error) {
			if agentOf(req) == "monitoring" {
				return provider.Response{StopReason: provider.StopRefusal, ModelUsed: "sonnet"}, nil
			}
			return answer("KET-QUA-" + agentOf(req)), nil
		})
		run := f.runWorkflow(adminA, id)
		want(t, run, 202, "")
		done := f.waitRun(run.json(t)["id"].(string), 30*time.Second)
		execID := done["execution_id"].(string)
		if done["status"] != "failed" {
			t.Errorf("lần chạy: %v", done["status"])
		}
		if n := count(`SELECT count(*) FROM execution_plan_steps WHERE execution_id = $1 AND status = 'skipped' AND started_at IS NULL`, execID); n != 1 {
			t.Errorf("bước chờ không được ghi là skipped (%d)", n)
		}
		if n := count(`SELECT count(*) FROM agent_tasks WHERE execution_id = $1 AND status = 'skipped'`, execID); n != 1 {
			t.Errorf("bước sau bước chờ không bị bỏ qua (%d)", n)
		}
		e.model.script(func(_ context.Context, _ int, req provider.Request) (provider.Response, error) {
			return answer("KET-QUA-" + agentOf(req)), nil
		})
	})

	t.Run("an approval step holds the run until enough different people approve", func(t *testing.T) {
		approver, second := e.tok("multi-a"), e.is.token(t, "multi", map[string]any{"org_id": e.orgA.String()})
		r := f.createWorkflow(adminA, "gated", gateDef(mon, rep, 2))
		want(t, r, 201, "")
		id := r.json(t)["id"].(string)
		f.activate(id)
		start := func() (runID, execID string) {
			t.Helper()
			run := f.runWorkflow(adminA, id)
			want(t, run, 202, "")
			return run.json(t)["id"].(string), run.json(t)["execution_id"].(string)
		}
		stepStatus := func(execID string) string {
			var st string
			if err := e.db.Admin.QueryRow(context.Background(), `SELECT status FROM execution_plan_steps WHERE execution_id = $1 AND plan_key = 'gate'`, execID).Scan(&st); err != nil {
				t.Fatal(err)
			}
			return st
		}
		approvalStatus := func(id any) string {
			var st string
			if err := e.db.Admin.QueryRow(context.Background(), `SELECT status FROM approvals WHERE id = $1`, id).Scan(&st); err != nil {
				t.Fatal(err)
			}
			return st
		}

		// Hai người duyệt khác nhau; người chạy không tự duyệt; một người không duyệt hai lần.
		runID, execID := start()
		ap := f.waitGate(runID, 30*time.Second)
		apID := ap["id"].(string)
		payload := ap["payload"].(map[string]any)
		if ap["subject_type"] != "workflow_step" || payload["message"] != "DUYET-khac-phuc" || payload["plan_key"] != "gate" ||
			ap["required_approvals"] != float64(2) || ap["task_id"] != nil {
			t.Errorf("approval của bước: %v", ap)
		}
		if n := count(`SELECT count(*) FROM agent_tasks WHERE execution_id = $1 AND status = 'queued'`, execID); n != 1 {
			t.Errorf("bước sau cổng duyệt đã chạy trước khi được duyệt (%d task queued)", n)
		}
		want(t, e.do("GET", "/v1/approvals/"+apID, adminB, nil, nil), 404, "not_found")
		want(t, f.decide(adminA, apID, "approve"), 403, "permission_denied")
		first := f.decide(approver, apID, "approve")
		want(t, first, 200, "")
		if first.json(t)["status"] != "pending" || len(first.json(t)["decisions"].([]any)) != 1 {
			t.Errorf("sau người duyệt thứ nhất: %s", first.body)
		}
		want(t, f.decide(approver, apID, "approve"), 409, "already_decided")
		if st := stepStatus(execID); st != "running" {
			t.Errorf("bước duyệt sau một người duyệt: %s", st)
		}
		last := f.decide(second, apID, "approve")
		want(t, last, 200, "")
		if last.json(t)["status"] != "approved" {
			t.Errorf("sau người duyệt thứ hai: %s", last.body)
		}
		if done := f.waitRun(runID, 30*time.Second); done["status"] != "completed" {
			t.Fatalf("lần chạy đã được duyệt: %v %v", done["status"], done["error"])
		}
		if st, as := stepStatus(execID), approvalStatus(apID); st != "completed" || as != "consumed" {
			t.Errorf("bước %s, approval %s", st, as)
		}
		if n := count(`SELECT count(*) FROM audit_logs WHERE org_id = $1 AND execution_id = $2 AND action IN ('approval.request','approval.decide','approval.consume')`, e.orgA, execID); n != 4 {
			t.Errorf("audit của approval: %d, muốn 4 (request, 2 decide, consume)", n)
		}

		// Từ chối: bước thất bại, bước sau nó bị bỏ qua.
		runID, execID = start()
		ap = f.waitGate(runID, 30*time.Second)
		want(t, f.decide(approver, ap["id"].(string), "reject"), 200, "")
		if done := f.waitRun(runID, 30*time.Second); done["status"] != "failed" {
			t.Fatalf("lần chạy bị từ chối: %v", done["status"])
		}
		if st := stepStatus(execID); st != "failed" {
			t.Errorf("bước bị từ chối: %s", st)
		}
		if n := count(`SELECT count(*) FROM agent_tasks WHERE execution_id = $1 AND status = 'skipped'`, execID); n != 1 {
			t.Errorf("bước sau cổng bị từ chối không bị bỏ qua (%d)", n)
		}

		// Huỷ trong lúc chờ duyệt: approval bị huỷ, không duyệt được nữa.
		runID, execID = start()
		ap = f.waitGate(runID, 30*time.Second)
		want(t, e.do("POST", "/v1/workflow-executions/"+runID+"/cancel", adminA, nil, nil), 202, "")
		if done := f.waitRun(runID, 30*time.Second); done["status"] != "cancelled" {
			t.Fatalf("lần chạy bị huỷ lúc chờ duyệt: %v", done["status"])
		}
		if st, as := stepStatus(execID), approvalStatus(ap["id"]); st != "cancelled" || as != "cancelled" {
			t.Errorf("bước %s, approval %s", st, as)
		}
		want(t, f.decide(approver, ap["id"].(string), "approve"), 409, "approval_not_pending")

		// Người viết definition không duyệt được cổng của lần chạy definition đó, kể cả khi
		// người khác bấm chạy: không ai viết hành động rồi tự duyệt nó.
		want(t, e.do("PATCH", "/v1/workflows/"+id, approver, map[string]any{"definition": gateDef(rep, mon, 1)}, nil), 200, "")
		runID, _ = start()
		ap = f.waitGate(runID, 30*time.Second)
		want(t, f.decide(approver, ap["id"].(string), "approve"), 403, "permission_denied")
		want(t, f.decide(adminA, ap["id"].(string), "approve"), 403, "permission_denied")
		want(t, f.decide(second, ap["id"].(string), "approve"), 200, "")
		if done := f.waitRun(runID, 30*time.Second); done["status"] != "completed" {
			t.Fatalf("lần chạy do người thứ ba duyệt: %v", done["status"])
		}
	})

	t.Run("a step's own timeout stops that step; it can only be tighter than the agent version's", func(t *testing.T) {
		def := func(seconds int) map[string]any {
			return map[string]any{"steps": []any{
				map[string]any{"id": "slow", "type": "agent", "agent_id": mon, "goal": "GOAL-slow", "timeout_seconds": seconds},
				map[string]any{"id": "after", "type": "agent", "agent_id": rep, "goal": "GOAL-after", "depends_on": []string{"slow"}},
			}}
		}
		want(t, f.createWorkflow(adminA, "too-short", def(5)), 422, "validation_failed")
		// Dài hơn thời hạn của Agent Version: bị từ chối khi chạy (lúc đó mới biết version).
		long := f.createWorkflow(adminA, "too-long", def(86400))
		want(t, long, 201, "")
		f.activate(long.json(t)["id"].(string))
		want(t, f.runWorkflow(adminA, long.json(t)["id"].(string)), 422, "validation_failed")

		r := f.createWorkflow(adminA, "slow-step", def(10))
		want(t, r, 201, "")
		id := r.json(t)["id"].(string)
		f.activate(id)
		e.model.script(func(ctx context.Context, _ int, _ provider.Request) (provider.Response, error) {
			select {
			case <-ctx.Done():
				return provider.Response{}, ctx.Err()
			case <-time.After(40 * time.Second):
				return answer("muộn"), nil
			}
		})
		began := time.Now()
		run := f.runWorkflow(adminA, id)
		want(t, run, 202, "")
		done := f.waitRun(run.json(t)["id"].(string), 35*time.Second)
		e.model.script(func(_ context.Context, _ int, req provider.Request) (provider.Response, error) {
			return answer("KET-QUA-" + agentOf(req)), nil
		})
		if d := time.Since(began); d < 9*time.Second || d > 30*time.Second {
			t.Errorf("bước có hạn 10 giây dừng sau %s", d)
		}
		execID := done["execution_id"].(string)
		if n := count(`SELECT count(*) FROM agent_tasks WHERE execution_id = $1 AND plan_key = 'slow' AND status = 'timed_out'`, execID); n != 1 {
			t.Errorf("bước quá hạn riêng không kết thúc timed_out (lần chạy: %v)", done["status"])
		}
		if n := count(`SELECT count(*) FROM agent_tasks WHERE execution_id = $1 AND plan_key = 'after' AND status = 'skipped'`, execID); n != 1 {
			t.Errorf("bước sau bước quá hạn không bị bỏ qua")
		}
	})

	t.Run("a run is validated again when it starts, and cancelling it cancels its execution", func(t *testing.T) {
		// Agent của một bước bị tắt sau khi workflow được lưu: lần chạy bị từ chối, và bản chiếu ghi lý do.
		gone := f.agent("temporary", nil, nil)
		r := f.createWorkflow(adminA, "uses-temporary", map[string]any{"steps": []any{map[string]any{"id": "a", "type": "agent", "agent_id": gone, "goal": "g"}}})
		want(t, r, 201, "")
		id := r.json(t)["id"].(string)
		f.activate(id)
		etag := e.do("GET", "/v1/agents/"+gone, adminA, nil, nil).header.Get("ETag")
		want(t, e.do("PATCH", "/v1/agents/"+gone, adminA, map[string]any{"status": "disabled"}, map[string]string{"If-Match": etag}), 200, "")
		want(t, f.runWorkflow(adminA, id), 409, "agent_not_active")
		if n := count(`SELECT count(*) FROM workflow_executions WHERE workflow_id = $1 AND status = 'failed' AND error->>'code' = 'agent_not_active' AND execution_id IS NULL`, id); n != 1 {
			t.Errorf("lần chạy bị từ chối không được ghi là failed (%d)", n)
		}

		// Huỷ trong lúc bước chờ đang chạy: execution và mọi bước dừng.
		long := f.createWorkflow(adminA, "long-wait", workflowDef(mon, rep, 600))
		want(t, long, 201, "")
		longID := long.json(t)["id"].(string)
		f.activate(longID)
		e.model.script(func(context.Context, int, provider.Request) (provider.Response, error) { return answer("xong"), nil })
		run := f.runWorkflow(adminA, longID)
		want(t, run, 202, "")
		runID := run.json(t)["id"].(string)
		execID := run.json(t)["execution_id"].(string)
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(25 * time.Millisecond) {
			if count(`SELECT count(*) FROM execution_plan_steps WHERE execution_id = $1 AND status = 'running'`, execID) == 1 {
				break
			}
		}
		want(t, e.do("POST", "/v1/workflow-executions/"+runID+"/cancel", viewerA, nil, nil), 403, "permission_denied")
		want(t, e.do("POST", "/v1/workflow-executions/"+runID+"/cancel", adminA, nil, nil), 202, "")
		done := f.waitRun(runID, 20*time.Second)
		if done["status"] != "cancelled" {
			t.Fatalf("lần chạy sau khi huỷ: %v", done["status"])
		}
		if n := count(`SELECT count(*) FROM execution_plan_steps WHERE execution_id = $1 AND status = 'cancelled'`, execID); n != 1 {
			t.Errorf("bước chờ không bị huỷ theo (%d)", n)
		}
		if n := count(`SELECT count(*) FROM agent_tasks WHERE execution_id = $1 AND finished_at IS NULL`, execID); n != 0 {
			t.Errorf("%d task còn mở sau khi huỷ", n)
		}

		// Lưu trữ: không chạy được nữa, không sửa được nữa, và biến khỏi danh sách mặc định.
		want(t, e.do("DELETE", "/v1/workflows/"+longID, adminA, nil, nil), 204, "")
		want(t, f.runWorkflow(adminA, longID), 409, "workflow_not_active")
		want(t, e.do("PATCH", "/v1/workflows/"+longID, adminA, map[string]any{"name": "x"}, nil), 409, "workflow_archived")
		for _, raw := range e.do("GET", "/v1/projects/"+f.project+"/workflows", adminA, nil, nil).json(t)["data"].([]any) {
			if raw.(map[string]any)["id"] == longID {
				t.Error("workflow đã lưu trữ vẫn hiện trong danh sách mặc định")
			}
		}
	})
}

// TestDurableWorkflowRun: một lần chạy workflow có bước chờ trên Temporal thật. Bước chờ
// là một timer của workflow cha; lịch sử chỉ chứa tham chiếu và phát lại được.
func TestDurableWorkflowRun(t *testing.T) {
	e := setup(t, true)
	adminA := e.tok("admin-a")
	f := newPlannedFixture(t, e, "workflow-durable")
	mon := f.agent("monitoring", nil, nil)
	rep := f.agent("reporter", nil, nil)
	e.model.script(func(_ context.Context, _ int, req provider.Request) (provider.Response, error) {
		return answer("KET-QUA-" + agentOf(req)), nil
	})
	e.startWorker(5 * time.Second)
	r := f.createWorkflow(adminA, "durable-check", workflowDef(mon, rep, 2))
	want(t, r, 201, "")
	id := r.json(t)["id"].(string)
	f.activate(id)
	run := f.runWorkflow(adminA, id)
	want(t, run, 202, "")
	began := time.Now()
	done := f.waitRun(run.json(t)["id"].(string), 90*time.Second)
	if done["status"] != "completed" || time.Since(began) < 2*time.Second {
		t.Fatalf("lần chạy: %v sau %s", done["status"], time.Since(began))
	}
	execID := done["execution_id"].(string)
	h := e.closedHistoryOf(durable.WorkflowID(e.orgA, uuid.MustParse(execID)))
	raw, err := protojson.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"GOAL-", "AGENT:", "KET-QUA-", "GHI-CHU", "durable-check"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Errorf("lịch sử chứa nội dung %q", secret)
		}
	}
	rp := worker.NewWorkflowReplayer()
	rp.RegisterWorkflowWithOptions(durable.PlanWorkflow, workflow.RegisterOptions{Name: durable.PlanWorkflowName})
	if err := rp.ReplayWorkflowHistory(quiet, h); err != nil {
		t.Errorf("replay: %v", err)
	}
	if os.Getenv("HQ_UPDATE_REPLAY_FIXTURE") == "1" {
		if host, err := os.Hostname(); err == nil && host != "" {
			raw = bytes.ReplaceAll(raw, []byte(host), []byte("fixture-host"))
		}
		//nolint:gosec // lịch sử chỉ chứa tham chiếu, là dữ liệu test được commit
		if err := os.WriteFile("../../internal/agent/runtime/durable/testdata/planned_parent_wait_v1.json", raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Bước đầu không hoàn thành: bước chờ và bước sau nó bị bỏ qua, không timer nào chạy.
	e.model.script(func(_ context.Context, _ int, req provider.Request) (provider.Response, error) {
		if agentOf(req) == "monitoring" {
			return provider.Response{StopReason: provider.StopRefusal, ModelUsed: "sonnet"}, nil
		}
		return answer("KET-QUA-" + agentOf(req)), nil
	})
	run = f.runWorkflow(adminA, id)
	want(t, run, 202, "")
	done = f.waitRun(run.json(t)["id"].(string), 90*time.Second)
	execID = done["execution_id"].(string)
	var skipped int
	if err := e.db.Admin.QueryRow(context.Background(), `SELECT count(*) FROM execution_plan_steps WHERE execution_id = $1 AND status = 'skipped'`, execID).Scan(&skipped); err != nil {
		t.Fatal(err)
	}
	if done["status"] != "failed" || skipped != 1 {
		t.Fatalf("lần chạy có bước đầu thất bại: %v, %d bước chờ skipped", done["status"], skipped)
	}
	h = e.closedHistoryOf(durable.WorkflowID(e.orgA, uuid.MustParse(execID)))
	if err := rp.ReplayWorkflowHistory(quiet, h); err != nil {
		t.Errorf("replay (skip): %v", err)
	}
	if os.Getenv("HQ_UPDATE_REPLAY_FIXTURE") == "1" {
		raw, err := protojson.Marshal(h)
		if err != nil {
			t.Fatal(err)
		}
		if host, err := os.Hostname(); err == nil && host != "" {
			raw = bytes.ReplaceAll(raw, []byte(host), []byte("fixture-host"))
		}
		//nolint:gosec // lịch sử chỉ chứa tham chiếu, là dữ liệu test được commit
		if err := os.WriteFile("../../internal/agent/runtime/durable/testdata/planned_parent_wait_skip_v1.json", raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Cổng chờ người duyệt: workflow cha ngủ trên signal, và thức ngay khi có quyết định.
	e.model.script(func(_ context.Context, _ int, req provider.Request) (provider.Response, error) {
		return answer("KET-QUA-" + agentOf(req)), nil
	})
	g := f.createWorkflow(adminA, "durable-gate", gateDef(mon, rep, 1))
	want(t, g, 201, "")
	gateID := g.json(t)["id"].(string)
	f.activate(gateID)
	run = f.runWorkflow(adminA, gateID)
	want(t, run, 202, "")
	gateRun := run.json(t)["id"].(string)
	ap := f.waitGate(gateRun, 60*time.Second)
	decided := time.Now()
	want(t, f.decide(e.tok("multi-a"), ap["id"].(string), "approve"), 200, "")
	done = f.waitRun(gateRun, 60*time.Second)
	if d := time.Since(decided); done["status"] != "completed" || d > 30*time.Second {
		t.Fatalf("lần chạy có cổng duyệt: %v sau %s", done["status"], d)
	}
	h = e.closedHistoryOf(durable.WorkflowID(e.orgA, uuid.MustParse(done["execution_id"].(string))))
	raw, err = protojson.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"DUYET-", "GOAL-", "KET-QUA-"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Errorf("lịch sử chứa nội dung %q", secret)
		}
	}
	if err := rp.ReplayWorkflowHistory(quiet, h); err != nil {
		t.Errorf("replay (gate): %v", err)
	}
	if os.Getenv("HQ_UPDATE_REPLAY_FIXTURE") == "1" {
		if host, err := os.Hostname(); err == nil && host != "" {
			raw = bytes.ReplaceAll(raw, []byte(host), []byte("fixture-host"))
		}
		//nolint:gosec // lịch sử chỉ chứa tham chiếu, là dữ liệu test được commit
		if err := os.WriteFile("../../internal/agent/runtime/durable/testdata/planned_parent_gate_v1.json", raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Hai cổng chờ song song: quyết định của cổng nào cũng đánh thức đúng cổng đó ngay
	// (một signal không bị cổng kia nhận mất), không phải đợi lần tự kiểm tra sau hai phút.
	two := f.createWorkflow(adminA, "durable-two-gates", map[string]any{"steps": []any{
		map[string]any{"id": "check", "type": "agent", "agent_id": mon, "goal": "GOAL-check"},
		map[string]any{"id": "gate-a", "type": "approval", "message": "DUYET-a", "depends_on": []string{"check"}},
		map[string]any{"id": "gate-b", "type": "approval", "message": "DUYET-b", "depends_on": []string{"check"}},
		map[string]any{"id": "report", "type": "agent", "agent_id": rep, "goal": "GOAL-report", "depends_on": []string{"gate-a", "gate-b"}},
	}})
	want(t, two, 201, "")
	twoID := two.json(t)["id"].(string)
	f.activate(twoID)
	run = f.runWorkflow(adminA, twoID)
	want(t, run, 202, "")
	twoRun, twoExec := run.json(t)["id"].(string), run.json(t)["execution_id"].(string)
	var pending []any
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline) && len(pending) != 2; time.Sleep(100 * time.Millisecond) {
		pending = e.do("GET", "/v1/approvals?execution_id="+twoExec+"&status=pending", e.tok("multi-a"), nil, nil).json(t)["data"].([]any)
	}
	if len(pending) != 2 {
		t.Fatalf("hai cổng song song: %d approval đang chờ", len(pending))
	}
	decided = time.Now()
	for _, raw := range pending {
		want(t, f.decide(e.tok("multi-a"), raw.(map[string]any)["id"].(string), "approve"), 200, "")
		time.Sleep(2 * time.Second) // hai quyết định là hai signal riêng
	}
	done = f.waitRun(twoRun, 60*time.Second)
	if d := time.Since(decided); done["status"] != "completed" || d > 40*time.Second {
		t.Fatalf("lần chạy có hai cổng: %v sau %s", done["status"], d)
	}
	if err := rp.ReplayWorkflowHistory(quiet, e.closedHistoryOf(durable.WorkflowID(e.orgA, uuid.MustParse(twoExec)))); err != nil {
		t.Errorf("replay (hai cổng): %v", err)
	}
}

// TestWorkflowSchedules: lịch chạy của workflow trên Temporal Schedules thật (ADR-0015).
// Lịch tồn tại khi và chỉ khi workflow active và có cron; khi lịch kích hoạt, process api
// tạo lần chạy dưới quyền người đã kích hoạt workflow, mỗi lần kích hoạt đúng một lần chạy.
func TestWorkflowSchedules(t *testing.T) {
	e := setup(t, true)
	ctx := context.Background()
	adminA := e.tok("admin-a")
	f := newPlannedFixture(t, e, "workflow-sched")
	mon := f.agent("monitoring", nil, nil)
	rep := f.agent("reporter", nil, nil)
	e.model.script(func(_ context.Context, _ int, req provider.Request) (provider.Response, error) {
		return answer("KET-QUA-" + agentOf(req)), nil
	})
	e.startWorker(5 * time.Second)
	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := e.db.Admin.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	r := f.createWorkflow(adminA, "nightly", workflowDef(mon, rep, 1))
	want(t, r, 201, "")
	id := r.json(t)["id"].(string)
	handle := e.temporal.ScheduleClient().GetHandle(ctx, schedule.ID(e.orgA, uuid.MustParse(id)))
	exists := func() bool {
		_, err := handle.Describe(ctx)
		return err == nil
	}
	patch := func(body map[string]any) resp { return e.do("PATCH", "/v1/workflows/"+id, adminA, body, nil) }

	// Biểu thức cron được kiểm chặt: 5 trường số, không macro, không dày hơn 5 phút một lần.
	for _, bad := range []string{"* * * * *", "*/2 * * * *", "0,3 7 * * *", "@hourly", "0 7 * * MON", "0 0 7 * * *", "CRON_TZ=Asia/Ho_Chi_Minh 0 7 * * *",
		"61 7 * * *", "0 7 * *", "0  7 * * *", ""} {
		if r := patch(map[string]any{"schedule": bad}); r.status != 422 || r.code(t) != "validation_failed" {
			t.Errorf("schedule %q: %d %s", bad, r.status, r.body)
		}
	}
	// Workflow nháp có cron nhưng chưa có lịch; kích hoạt thì có; tắt thì mất.
	want(t, patch(map[string]any{"schedule": "0 7 * * *"}), 200, "")
	if exists() {
		t.Fatal("workflow nháp đã có lịch trên Temporal")
	}
	f.activate(id)
	desc, err := handle.Describe(ctx)
	if err != nil || len(desc.Schedule.Spec.CronExpressions)+len(desc.Schedule.Spec.Calendars) == 0 {
		t.Fatalf("workflow active có cron nhưng không có lịch: %v", err)
	}
	want(t, patch(map[string]any{"status": "disabled"}), 200, "")
	if exists() {
		t.Fatal("workflow đã tắt vẫn còn lịch")
	}
	f.activate(id)
	want(t, patch(map[string]any{"schedule": "*/15 * * * *"}), 200, "")
	if !exists() {
		t.Fatal("đổi cron làm mất lịch")
	}

	runs := func() []any {
		return e.do("GET", "/v1/workflows/"+id+"/executions", adminA, nil, nil).json(t)["data"].([]any)
	}
	waitRuns := func(n int) []any {
		t.Helper()
		var got []any
		for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
			if got = runs(); len(got) >= n {
				return got
			}
		}
		t.Fatalf("có %d lần chạy, chờ %d", len(got), n)
		return nil
	}
	t.Run("a firing creates exactly one run, as the user who activated the workflow", func(t *testing.T) {
		if err := handle.Trigger(ctx, client.ScheduleTriggerOptions{}); err != nil {
			t.Fatal(err)
		}
		run := waitRuns(1)[0].(map[string]any)
		if run["trigger_type"] != "schedule" {
			t.Fatalf("lần chạy: %v", run)
		}
		done := f.waitRun(run["id"].(string), 60*time.Second)
		if done["status"] != "completed" {
			t.Fatalf("lần chạy theo lịch: %v %v", done["status"], done["error"])
		}
		if n := count(`SELECT count(*) FROM workflow_executions w JOIN users u ON u.id = w.triggered_by JOIN agent_executions x ON x.id = w.execution_id
			WHERE w.id = $1 AND u.idp_subject = 'admin-a' AND w.trigger_key IS NOT NULL AND x.initiator_type = 'user' AND x.initiator_id = u.id`, run["id"]); n != 1 {
			t.Errorf("lần chạy theo lịch không mang danh tính người đã kích hoạt")
		}
		// Cùng một lần kích hoạt được phát lại (workflow của Temporal chạy lại với cùng ID):
		// không có lần chạy thứ hai.
		var key string
		if err := e.db.Admin.QueryRow(ctx, `SELECT trigger_key FROM workflow_executions WHERE id = $1`, run["id"]).Scan(&key); err != nil {
			t.Fatal(err)
		}
		again, err := e.temporal.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: key, TaskQueue: schedule.TaskQueue}, schedule.WorkflowName,
			schedule.Ref{OrgID: e.orgA, WorkflowID: uuid.MustParse(id)})
		if err != nil {
			t.Fatal(err)
		}
		if err := again.Get(ctx, nil); err != nil {
			t.Fatalf("phát lại lần kích hoạt: %v", err)
		}
		if n := len(runs()); n != 1 {
			t.Errorf("phát lại một lần kích hoạt tạo thêm lần chạy (%d)", n)
		}
		// Lịch sử của workflow được lịch khởi chạy chỉ chứa định danh.
		raw, err := protojson.Marshal(e.historyOf(key))
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"GOAL-", "KET-QUA-", "nightly", "admin-a"} {
			if bytes.Contains(raw, []byte(secret)) {
				t.Errorf("lịch sử chứa nội dung %q", secret)
			}
		}
		if os.Getenv("HQ_UPDATE_REPLAY_FIXTURE") == "1" {
			if host, err := os.Hostname(); err == nil && host != "" {
				raw = bytes.ReplaceAll(raw, []byte(host), []byte("fixture-host"))
			}
			if err := os.MkdirAll("../../internal/workflow/definitions/schedule/testdata", 0o755); err != nil { //nolint:gosec // thư mục dữ liệu test
				t.Fatal(err)
			}
			//nolint:gosec // lịch sử chỉ chứa định danh, là dữ liệu test được commit
			if err := os.WriteFile("../../internal/workflow/definitions/schedule/testdata/scheduled_run_v1.json", raw, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	})

	// forge khởi chạy workflow kích hoạt như một bên ghi được vào namespace Temporal sẽ làm.
	forge := func(key string, ref schedule.Ref) {
		t.Helper()
		run, err := e.temporal.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: key, TaskQueue: schedule.TaskQueue}, schedule.WorkflowName, ref)
		if err != nil {
			t.Fatal(err)
		}
		if err := run.Get(ctx, nil); err != nil {
			t.Fatalf("lần kích hoạt giả %q: %v", key, err)
		}
	}
	firing := func(org uuid.UUID) string {
		return "workflow-run/" + org.String() + "/" + id + "-" + time.Now().UTC().Format(time.RFC3339)
	}
	t.Run("a forged firing runs nothing: wrong tenant, wrong key, or too soon after the last run", func(t *testing.T) {
		// Tham chiếu ghép org B với workflow của org A: với api, workflow đó không tồn tại.
		// Không ghi audit: tham chiếu đến từ Temporal có thể nêu org bất kỳ, và một org không
		// nhận dòng audit chỉ vì có người nhắc tới nó. (Trước đây test này đòi một dòng audit
		// not_found trong org B; đó chính là đường spam audit mà review bảo mật chỉ ra.)
		forge(firing(e.orgB), schedule.Ref{OrgID: e.orgB, WorkflowID: uuid.MustParse(id)})
		if n := count(`SELECT count(*) FROM workflow_executions WHERE org_id = $1`, e.orgB); n != 0 {
			t.Errorf("org B có %d lần chạy", n)
		}
		if n := count(`SELECT count(*) FROM audit_logs WHERE org_id = $1 AND action = 'workflow.schedule_skip'`, e.orgB); n != 0 {
			t.Errorf("tham chiếu giả ghi %d dòng audit vào org B", n)
		}
		// Tham chiếu thật nhưng khoá không phải của một lần kích hoạt của lịch này.
		ref := schedule.Ref{OrgID: e.orgA, WorkflowID: uuid.MustParse(id)}
		forge("forged-"+uuid.NewString(), ref)
		forge("workflow-run/"+e.orgA.String()+"/"+uuid.NewString()+"-"+time.Now().UTC().Format(time.RFC3339), ref)
		// Khoá đúng dạng, mới, nhưng workflow vừa có một lần chạy theo lịch: trần tần suất
		// được giữ trong database, không tin nơi kích hoạt.
		forge(firing(e.orgA), ref)
		if n := len(runs()); n != 1 {
			t.Errorf("lần kích hoạt giả tạo lần chạy (%d)", n)
		}
		if n := count(`SELECT count(*) FROM audit_logs WHERE org_id = $1 AND action = 'workflow.schedule_skip' AND resource_id = $2 AND details->>'reason' = 'schedule_too_frequent'`, e.orgA, id); n != 1 {
			t.Errorf("lần kích hoạt quá dày: %d audit", n)
		}
	})

	t.Run("the activator's authority is re-checked at every firing", func(t *testing.T) {
		if _, err := e.db.Admin.Exec(ctx, `UPDATE users SET status = 'disabled' WHERE idp_subject = 'admin-a'`); err != nil {
			t.Fatal(err)
		}
		defer func() {
			_, _ = e.db.Admin.Exec(ctx, `UPDATE users SET status = 'active' WHERE idp_subject = 'admin-a'`)
		}()
		before := count(`SELECT count(*) FROM workflow_executions WHERE workflow_id = $1`, id)
		refused := func() int {
			return count(`SELECT count(*) FROM audit_logs WHERE org_id = $1 AND action = 'workflow.schedule_skip' AND resource_id = $2
				AND outcome = 'denied' AND details->>'reason' NOT IN ('schedule_too_frequent','workflow_not_scheduled')`, e.orgA, id)
		}
		// Ba lần kích hoạt liên tiếp bị từ chối: mỗi lần một dòng audit, rồi workflow tự tắt
		// và lịch của nó bị xoá.
		for i := 1; i <= 3; i++ {
			forge(firing(e.orgA)+"-"+strconv.Itoa(i), schedule.Ref{OrgID: e.orgA, WorkflowID: uuid.MustParse(id)})
			if i == 1 {
				// Khoá có hậu tố lạ không phải của lịch: bị bỏ qua, không tính.
				if n := refused(); n != 0 {
					t.Fatalf("khoá sai dạng được xử lý (%d audit)", n)
				}
			}
			time.Sleep(1100 * time.Millisecond) // mỗi lần kích hoạt một khoá (thời điểm tính theo giây)
			forge(firing(e.orgA), schedule.Ref{OrgID: e.orgA, WorkflowID: uuid.MustParse(id)})
			if n := refused(); n != i {
				t.Fatalf("sau %d lần kích hoạt của người chạy đã bị vô hiệu hoá: %d audit", i, n)
			}
		}
		if after := count(`SELECT count(*) FROM workflow_executions WHERE workflow_id = $1`, id); after != before {
			t.Errorf("người chạy đã bị vô hiệu hoá vẫn tạo được lần chạy (%d → %d)", before, after)
		}
		if n := count(`SELECT count(*) FROM workflows WHERE id = $1 AND status = 'disabled'`, id); n != 1 {
			t.Error("workflow bị từ chối ba lần liên tiếp không tự tắt")
		}
		if n := count(`SELECT count(*) FROM audit_logs WHERE org_id = $1 AND action = 'workflow.auto_disable' AND resource_id = $2`, e.orgA, id); n != 1 {
			t.Errorf("audit workflow.auto_disable: %d", n)
		}
		if exists() {
			t.Error("workflow tự tắt vẫn còn lịch")
		}
	})

	t.Run("archiving removes the schedule", func(t *testing.T) {
		want(t, e.do("DELETE", "/v1/workflows/"+id, adminA, nil, nil), 204, "")
		if exists() {
			t.Error("workflow đã lưu trữ vẫn còn lịch")
		}
	})
}
