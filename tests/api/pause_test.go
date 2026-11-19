package api_test

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/hqgroups/mcp/internal/agent/runtime/durable"
	"github.com/hqgroups/mcp/internal/ai/provider"
)

func (f *plannedFixture) upstreamCalls(tool string) int {
	f.up.mu.Lock()
	defer f.up.mu.Unlock()
	n := 0
	for _, c := range f.up.calls {
		if strings.HasPrefix(c, tool+" ") {
			n++
		}
	}
	return n
}

// pauseScript: lượt đầu của mỗi task treo tới khi `hold` đóng rồi gọi một tool; lượt sau trả lời.
func pauseScript(entered chan<- struct{}, hold <-chan struct{}) func(context.Context, int, provider.Request) (provider.Response, error) {
	return func(ctx context.Context, _ int, req provider.Request) (provider.Response, error) {
		if hasToolResult(req) {
			return answer("XONG-" + agentOf(req)), nil
		}
		entered <- struct{}{}
		select {
		case <-hold:
		case <-ctx.Done():
			return provider.Response{}, ctx.Err()
		}
		return toolUse("infra.list_containers"), nil
	}
}

func TestPauseResume(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	adminA, adminB, viewerA, devA := e.tok("admin-a"), e.tok("admin-b"), e.tok("viewer-a"), e.tok("dev-a")
	f := newPlannedFixture(t, e, "pause-a")
	f.agent("monitoring", []string{"list_containers"}, nil)
	f.agent("database", []string{"list_containers"}, nil)
	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := e.db.Admin.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	post := func(tok, id, action string) resp {
		return e.do("POST", "/v1/executions/"+id+"/"+action, tok, nil, nil)
	}

	t.Run("an execution pauses at the next step boundary and resumes exactly there", func(t *testing.T) {
		entered, hold := make(chan struct{}, 4), make(chan struct{})
		e.model.script(pauseScript(entered, hold))
		before := f.upstreamCalls("list_containers")
		id := f.single(adminA, "monitoring")
		<-entered
		// Quyền: người chỉ đọc không tạm dừng được; tenant khác không thấy execution.
		want(t, post(viewerA, id, "pause"), 403, "permission_denied")
		want(t, post(adminB, id, "pause"), 404, "not_found")
		want(t, post(adminB, id, "resume"), 404, "not_found")
		r := post(adminA, id, "pause")
		want(t, r, 202, "")
		// Lượt model đang chạy được chạy xong; execution chỉ sang paused khi vòng lặp dừng,
		// nhưng yêu cầu tạm dừng hiện ngay trong API.
		if x := r.json(t); x["status"] != "running" || x["pause_requested_at"] == nil {
			t.Errorf("ngay sau yêu cầu tạm dừng: %v / %v", x["status"], x["pause_requested_at"])
		}
		close(hold)
		x := f.waitStatus(id, 20*time.Second, "paused")
		tasks := f.allTasks(id)
		if x["status"] != "paused" || len(tasks) != 1 || tasks[0]["status"] != "paused" {
			t.Fatalf("execution %v, task %v", x["status"], tasks)
		}
		// Dừng TRƯỚC tool call: lượt model đã ghi, tool chưa chạy, và không có gì chạy trong lúc dừng.
		time.Sleep(300 * time.Millisecond)
		if n := f.upstreamCalls("list_containers") - before; n != 0 {
			t.Errorf("tool chạy %d lần trong lúc tạm dừng", n)
		}
		if n := count(`SELECT count(*) FROM execution_steps WHERE execution_id = $1`, id); n != 1 {
			t.Errorf("số step khi tạm dừng = %d, muốn 1", n)
		}
		want(t, post(adminA, id, "pause"), 202, "") // idempotent: không đổi gì, không audit thêm
		// Gỡ yêu cầu tạm dừng của người khác cần quyền execution:resume: developer có
		// execution:cancel (tự tạm dừng và tự gỡ được) nhưng không gỡ được yêu cầu của admin.
		want(t, post(devA, id, "resume"), 403, "permission_denied")
		if s := f.get(id)["status"]; s != "paused" {
			t.Fatalf("execution = %v sau khi developer bị từ chối resume", s)
		}
		want(t, post(adminA, id, "resume"), 202, "")
		want(t, post(adminA, id, "resume"), 202, "") // không còn yêu cầu nào: không đổi gì
		x = f.waitStatus(id, 20*time.Second, plannedTerminal...)
		if x["status"] != "completed" || x["output"].(map[string]any)["message"] != "XONG-monitoring" {
			t.Fatalf("sau khi chạy tiếp: %v", x)
		}
		if n := f.upstreamCalls("list_containers") - before; n != 1 {
			t.Errorf("tool chạy %d lần, muốn 1", n)
		}
		if n := count(`SELECT count(*) FROM execution_steps WHERE execution_id = $1 AND status = 'succeeded'`, id); n != 3 {
			t.Errorf("số step = %d, muốn 3", n)
		}
		if f.get(id)["pause_requested_at"] != nil {
			t.Errorf("pause_requested_at còn sau khi resume")
		}
		for action, wantN := range map[string]int{"execution.pause": 1, "execution.resume": 1} {
			if n := count(`SELECT count(*) FROM audit_logs WHERE org_id = $1 AND action = $2 AND resource_id = $3`, e.orgA, action, id); n != wantN {
				t.Errorf("audit %s = %d, muốn %d", action, n, wantN)
			}
		}
		// Execution đã kết thúc: không tạm dừng hay chạy tiếp được.
		want(t, post(adminA, id, "pause"), 409, "execution_finished")
		want(t, post(adminA, id, "resume"), 409, "execution_finished")
	})

	t.Run("whoever paused can resume their own pause", func(t *testing.T) {
		entered, hold := make(chan struct{}, 4), make(chan struct{})
		e.model.script(pauseScript(entered, hold))
		id := f.single(adminA, "monitoring")
		<-entered
		want(t, post(devA, id, "pause"), 202, "")
		close(hold)
		f.waitStatus(id, 20*time.Second, "paused")
		want(t, post(devA, id, "resume"), 202, "")
		if x := f.waitStatus(id, 20*time.Second, plannedTerminal...); x["status"] != "completed" {
			t.Fatalf("execution: %v", x["status"])
		}
	})

	t.Run("cancelling a paused execution ends it as cancelled", func(t *testing.T) {
		entered, hold := make(chan struct{}, 4), make(chan struct{})
		e.model.script(pauseScript(entered, hold))
		before := f.upstreamCalls("list_containers")
		id := f.single(adminA, "monitoring")
		<-entered
		want(t, post(adminA, id, "pause"), 202, "")
		close(hold)
		f.waitStatus(id, 20*time.Second, "paused")
		want(t, post(adminA, id, "cancel"), 202, "")
		x := f.waitStatus(id, 20*time.Second, plannedTerminal...)
		if x["status"] != "cancelled" || f.allTasks(id)[0]["status"] != "cancelled" || f.upstreamCalls("list_containers") != before {
			t.Fatalf("execution %v, task %v", x["status"], f.allTasks(id)[0]["status"])
		}
	})

	t.Run("every task of a plan pauses, and a task started while paused does not run", func(t *testing.T) {
		entered, hold := make(chan struct{}, 4), make(chan struct{})
		e.model.script(pauseScript(entered, hold))
		// monitoring và database song song; report phụ thuộc cả hai nên chỉ bắt đầu sau khi chạy tiếp.
		f.agent("report", nil, nil)
		id := f.mustStart(planTask("monitoring", f.agents["monitoring"]), planTask("database", f.agents["database"]),
			planTask("report", f.agents["report"], "monitoring", "database"))
		<-entered
		<-entered
		want(t, post(adminA, id, "pause"), 202, "")
		close(hold)
		f.waitStatus(id, 20*time.Second, "paused")
		for deadline := time.Now().Add(10 * time.Second); f.taskStatuses(id) != "database=paused monitoring=paused report=queued" && time.Now().Before(deadline); {
			time.Sleep(25 * time.Millisecond)
		}
		if got := f.taskStatuses(id); got != "database=paused monitoring=paused report=queued" {
			t.Fatalf("task: %s", got)
		}
		want(t, post(adminA, id, "resume"), 202, "")
		x := f.waitStatus(id, 30*time.Second, plannedTerminal...)
		if x["status"] != "completed" || f.taskStatuses(id) != "database=completed monitoring=completed report=completed" {
			t.Fatalf("execution %v; task: %s", x["status"], f.taskStatuses(id))
		}
	})
}

// TestDurablePauseResume: tạm dừng trên Temporal thật. Execution đang tạm dừng vẫn tạm
// dừng sau khi worker khởi động lại; resume đánh thức nó ngay (signal), không phải đợi
// lần tự kiểm tra; lịch sử chỉ chứa tham chiếu và phát lại được.
func TestDurablePauseResume(t *testing.T) {
	e := setup(t, true)
	adminA := e.tok("admin-a")
	f := newPlannedFixture(t, e, "pause-durable")
	f.agent("monitoring", []string{"list_containers"}, nil)
	post := func(id, action string) resp {
		return e.do("POST", "/v1/executions/"+id+"/"+action, adminA, nil, nil)
	}
	entered, hold := make(chan struct{}, 4), make(chan struct{})
	e.model.script(pauseScript(entered, hold))
	kill := e.startWorker(500 * time.Millisecond)
	before := f.upstreamCalls("list_containers")
	id := f.single(adminA, "monitoring")
	select {
	case <-entered:
	case <-time.After(40 * time.Second):
		t.Fatal("worker không nhận execution")
	}
	want(t, post(id, "pause"), 202, "")
	close(hold)
	f.waitStatus(id, 30*time.Second, "paused")

	// Worker chết trong lúc execution tạm dừng: trạng thái tạm dừng bền vững.
	kill()
	e.startWorker(5 * time.Second)
	time.Sleep(2 * time.Second)
	if s := f.get(id)["status"]; s != "paused" || f.upstreamCalls("list_containers") != before {
		t.Fatalf("sau khi worker khởi động lại: %v, tool chạy %d lần", s, f.upstreamCalls("list_containers")-before)
	}
	resumed := time.Now()
	want(t, post(id, "resume"), 202, "")
	x := f.waitStatus(id, 60*time.Second, plannedTerminal...)
	if d := time.Since(resumed); x["status"] != "completed" || d > 30*time.Second || f.upstreamCalls("list_containers")-before != 1 {
		t.Fatalf("execution %v sau %s; tool chạy %d lần", x["status"], d, f.upstreamCalls("list_containers")-before)
	}

	h := e.historyOf(durable.WorkflowID(e.orgA, uuid.MustParse(id)))
	raw, err := protojson.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"AGENT:", "XONG-", "Kiểm tra production", "result of list_containers"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Errorf("lịch sử chứa nội dung %q", secret)
		}
	}
	rp := worker.NewWorkflowReplayer()
	rp.RegisterWorkflowWithOptions(durable.Workflow, workflow.RegisterOptions{Name: durable.WorkflowName})
	if err := rp.ReplayWorkflowHistory(quiet, h); err != nil {
		t.Errorf("replay: %v", err)
	}
	if os.Getenv("HQ_UPDATE_REPLAY_FIXTURE") == "1" {
		if host, err := os.Hostname(); err == nil && host != "" {
			raw = bytes.ReplaceAll(raw, []byte(host), []byte("fixture-host"))
		}
		//nolint:gosec // lịch sử chỉ chứa tham chiếu, là dữ liệu test được commit
		if err := os.WriteFile("../../internal/agent/runtime/durable/testdata/single_agent_paused_v1.json", raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
