package api_test

import (
	"bytes"
	"context"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/hqgroups/mcp/internal/agent/runtime/durable"
	"github.com/hqgroups/mcp/internal/ai/provider"
)

func (e *env) historyOf(workflowID string) *historypb.History {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	it := e.temporal.GetWorkflowHistory(ctx, workflowID, "", false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	h := &historypb.History{}
	for it.HasNext() {
		ev, err := it.Next()
		if err != nil {
			e.t.Fatalf("history %s: %v", workflowID, err)
		}
		h.Events = append(h.Events, ev)
	}
	return h
}

// TestDurablePlannedExecutions chạy execution chế độ planned trên Temporal thật: workflow
// cha chạy kế hoạch, mỗi task là một child workflow. Execution tiếp tục sau khi worker
// chết mà không ghi trùng hay gọi lại tool; huỷ lan tới mọi task con; lịch sử chỉ chứa
// tham chiếu và phát lại được.
func TestDurablePlannedExecutions(t *testing.T) {
	e := setup(t, true)
	ctx := context.Background()
	adminA, approverA := e.tok("admin-a"), e.tok("multi-a")
	f := newPlannedFixture(t, e, "planned-durable")
	diagnosers := []string{"monitoring", "database", "security", "devops"}
	for _, name := range diagnosers {
		f.agent(name, []string{"list_containers"}, nil)
	}
	synth := f.agent("synth", nil, nil)
	fixer := f.agent("fixer", []string{"restart_container"}, nil)
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
	// record đọc lịch sử của workflow cha và của child workflow của một task: chúng chỉ
	// được chứa tham chiếu và phải phát lại được bằng code hiện tại. Với
	// HQ_UPDATE_REPLAY_FIXTURE=1 chúng được ghi làm lịch sử mẫu cho test replay không cần
	// Docker (internal/agent/runtime/durable). child rỗng = chỉ workflow cha.
	record := func(t *testing.T, id, taskKey, parentName, childName string) {
		t.Helper()
		histories := map[string]*historypb.History{parentName: e.historyOf(durable.WorkflowID(e.orgA, uuid.MustParse(id)))}
		if childName != "" {
			histories[childName] = e.historyOf(durable.TaskWorkflowID(e.orgA, uuid.MustParse(id), uuid.MustParse(f.tasks(id)[taskKey]["id"].(string))))
		}
		rp := worker.NewWorkflowReplayer()
		rp.RegisterWorkflowWithOptions(durable.PlanWorkflow, workflow.RegisterOptions{Name: durable.PlanWorkflowName})
		rp.RegisterWorkflowWithOptions(durable.TaskWorkflow, workflow.RegisterOptions{Name: durable.TaskWorkflowName})
		for name, h := range histories {
			raw, err := protojson.Marshal(h)
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"GOAL:", "AGENT:", "FINDING-", "TONG-HOP", "Kiểm tra production", "result of list_containers", "DA-KHOI-DONG-LAI"} {
				if bytes.Contains(raw, []byte(secret)) {
					t.Errorf("lịch sử %s chứa nội dung %q", name, secret)
				}
			}
			if err := rp.ReplayWorkflowHistory(quiet, h); err != nil {
				t.Errorf("replay %s: %v", name, err)
			}
			if os.Getenv("HQ_UPDATE_REPLAY_FIXTURE") == "1" {
				if host, err := os.Hostname(); err == nil && host != "" {
					raw = bytes.ReplaceAll(raw, []byte(host), []byte("fixture-host"))
				}
				//nolint:gosec // lịch sử chỉ chứa tham chiếu, là dữ liệu test được commit
				if err := os.WriteFile("../../internal/agent/runtime/durable/testdata/"+name+"_v1.json", raw, 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	upstreamCalls := func(tool string) int {
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

	t.Run("worker dies while four tasks run in parallel; the plan finishes without duplicates", func(t *testing.T) {
		var killed atomic.Bool
		gate := newBarrier(len(diagnosers))
		e.model.script(func(ctx context.Context, _ int, req provider.Request) (provider.Response, error) {
			name := agentOf(req)
			switch {
			case name == "synth":
				return answer("TONG-HOP-ket-qua"), nil
			case hasToolResult(req):
				return answer("FINDING-" + name), nil
			case !killed.Load():
				// Trước khi worker chết: cả bốn lượt đầu treo cùng lúc tới khi bị ngắt.
				_ = gate.wait(ctx)
				<-ctx.Done()
				return provider.Response{}, ctx.Err()
			}
			return toolUse("infra.list_containers"), nil
		})
		before := upstreamCalls("list_containers")
		kill := e.startWorker(500 * time.Millisecond)
		id := f.mustStart(diagnosis()...)
		select {
		case <-gate.open:
		case <-time.After(40 * time.Second):
			t.Fatalf("bốn task không chạy đồng thời: %s", f.taskStatuses(id))
		}
		if n := count(`SELECT count(*) FROM execution_steps WHERE execution_id = $1 AND status = 'started'`, id); n != 4 {
			t.Fatalf("step đang mở = %d, muốn 4", n)
		}
		killed.Store(true)
		kill()
		if s := f.get(id)["status"]; s != "running" {
			t.Fatalf("sau khi worker chết, execution phải còn running: %v", s)
		}

		e.startWorker(5 * time.Second)
		x := f.waitStatus(id, 120*time.Second, plannedTerminal...)
		if x["status"] != "completed" || x["output"].(map[string]any)["message"] != "TONG-HOP-ket-qua" {
			t.Fatalf("execution: %v %v; task: %s", x["status"], x["error"], f.taskStatuses(id))
		}
		if got := f.taskStatuses(id); got != "database=completed devops=completed monitoring=completed security=completed synthesis=completed" {
			t.Errorf("task: %s", got)
		}
		// Mỗi task chẩn đoán có đúng ba step, không step_seq nào bị ghi trùng; chín lượt
		// model thành công → chín usage; mỗi tool đúng một lần.
		if n := count(`SELECT count(*) FROM execution_steps WHERE execution_id = $1`, id); n != 13 {
			t.Errorf("số step = %d, muốn 13", n)
		}
		if n := count(`SELECT count(*) FROM (SELECT task_id, step_seq FROM execution_steps WHERE execution_id = $1
			GROUP BY task_id, step_seq HAVING count(*) > 1) d`, id); n != 0 {
			t.Errorf("có %d step bị ghi trùng", n)
		}
		if n := count(`SELECT count(*) FROM execution_steps WHERE execution_id = $1 AND status <> 'succeeded'`, id); n != 0 {
			t.Errorf("còn %d step chưa succeeded", n)
		}
		if n := count(`SELECT count(*) FROM usage_records WHERE execution_id = $1`, id); n != 9 {
			t.Errorf("usage_records = %d, muốn 9", n)
		}
		if n := upstreamCalls("list_containers") - before; n != 4 {
			t.Errorf("tool được gọi %d lần, muốn 4", n)
		}

		// Lịch sử của workflow cha và của từng child workflow chỉ chứa tham chiếu.
		record(t, id, "monitoring", "planned_parent", "planned_task")
	})

	t.Run("cancel reaches every child workflow within a bounded time", func(t *testing.T) {
		e.stopWorkers()
		e.startWorker(5 * time.Second)
		gate := newBarrier(len(diagnosers))
		var mu sync.Mutex
		interrupted := 0
		e.model.script(func(ctx context.Context, _ int, _ provider.Request) (provider.Response, error) {
			_ = gate.wait(ctx)
			<-ctx.Done() // chỉ thoát khi yêu cầu huỷ tới được activity đang chạy
			mu.Lock()
			interrupted++
			mu.Unlock()
			return provider.Response{}, ctx.Err()
		})
		id := f.mustStart(diagnosis()...)
		select {
		case <-gate.open:
		case <-time.After(40 * time.Second):
			t.Fatalf("các task không bắt đầu: %s", f.taskStatuses(id))
		}
		began := time.Now()
		want(t, e.do("POST", "/v1/executions/"+id+"/cancel", adminA, nil, nil), 202, "")
		x := f.waitStatus(id, 30*time.Second, plannedTerminal...)
		if d := time.Since(began); x["status"] != "cancelled" || d > 20*time.Second {
			t.Fatalf("execution %v sau %s", x["status"], d)
		}
		if got := f.taskStatuses(id); got != "database=cancelled devops=cancelled monitoring=cancelled security=cancelled synthesis=cancelled" {
			t.Errorf("task: %s", got)
		}
		mu.Lock()
		n := interrupted
		mu.Unlock()
		if n != 4 {
			t.Errorf("yêu cầu huỷ tới %d / 4 lượt model đang chạy", n)
		}
		if n := count(`SELECT count(*) FROM execution_steps WHERE execution_id = $1 AND status = 'started'`, id); n != 0 {
			t.Errorf("%d step còn mở sau khi huỷ", n)
		}
		// Không child workflow nào còn chạy.
		for key, task := range f.tasks(id) {
			if key == "synthesis" {
				continue // chưa bao giờ bắt đầu
			}
			d, err := e.temporal.DescribeWorkflow(ctx, durable.TaskWorkflowID(e.orgA, uuid.MustParse(id), uuid.MustParse(task["id"].(string))), "")
			if err != nil {
				t.Errorf("child workflow của %s: %v", key, err)
			} else if d.Status == enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING {
				t.Errorf("child workflow của %s còn chạy sau khi huỷ", key)
			}
		}
		record(t, id, "monitoring", "planned_parent_cancelled", "planned_task_cancelled")
	})

	t.Run("a failed task skips its dependents on Temporal too", func(t *testing.T) {
		e.model.script(func(_ context.Context, _ int, req provider.Request) (provider.Response, error) {
			if agentOf(req) == "database" {
				return provider.Response{StopReason: provider.StopRefusal, ModelUsed: "sonnet"}, nil
			}
			return answer("FINDING-" + agentOf(req)), nil
		})
		id := f.mustStart(planTask("database", f.agents["database"]), planTask("monitoring", f.agents["monitoring"]),
			planTask("report", synth, "database"), planTask("final", synth, "report"))
		x := f.waitStatus(id, 60*time.Second, plannedTerminal...)
		if got := f.taskStatuses(id); got != "database=failed final=skipped monitoring=completed report=skipped" || x["status"] != "failed" {
			t.Errorf("task: %s; execution %v", got, x["status"])
		}
		record(t, id, "", "planned_parent_skip", "")
	})

	t.Run("fail_execution cancels the other running child workflows", func(t *testing.T) {
		entered := make(chan struct{}, 1)
		e.model.script(func(ctx context.Context, _ int, req provider.Request) (provider.Response, error) {
			if agentOf(req) == "database" {
				<-entered
				return provider.Response{StopReason: provider.StopRefusal, ModelUsed: "sonnet"}, nil
			}
			entered <- struct{}{}
			<-ctx.Done()
			return provider.Response{}, ctx.Err()
		})
		db := planTask("database", f.agents["database"])
		db["on_failure"] = "fail_execution"
		id := f.mustStart(db, planTask("monitoring", f.agents["monitoring"]), planTask("after", synth, "monitoring"))
		x := f.waitStatus(id, 60*time.Second, plannedTerminal...)
		if got := f.taskStatuses(id); got != "after=cancelled database=failed monitoring=cancelled" {
			t.Errorf("task: %s", got)
		}
		if x["status"] != "failed" || x["error"].(map[string]any)["code"] != "task_failed" {
			t.Errorf("execution: %v %v", x["status"], x["error"])
		}
		record(t, id, "", "planned_parent_halt", "")
	})

	t.Run("an approval decision wakes the waiting child workflow while a sibling task finishes", func(t *testing.T) {
		execID := ""
		var mu sync.Mutex
		e.model.script(func(_ context.Context, _ int, req provider.Request) (provider.Response, error) {
			switch {
			case agentOf(req) == "fixer" && !hasToolResult(req):
				return toolUse("infra.restart_container"), nil
			case agentOf(req) == "fixer":
				return answer("DA-KHOI-DONG-LAI"), nil
			case !hasToolResult(req):
				for i := 0; i < 800; i++ {
					mu.Lock()
					id := execID
					mu.Unlock()
					if id != "" && f.get(id)["status"] == "waiting_approval" {
						break
					}
					time.Sleep(25 * time.Millisecond)
				}
				return toolUse("infra.list_containers"), nil
			}
			return answer("FINDING-monitoring"), nil
		})
		before := upstreamCalls("restart_container")
		id := f.mustStart(planTask("monitoring", f.agents["monitoring"]), planTask("fix", fixer))
		mu.Lock()
		execID = id
		mu.Unlock()
		for deadline := time.Now().Add(60 * time.Second); f.taskStatuses(id) != "fix=waiting_approval monitoring=completed" && time.Now().Before(deadline); {
			time.Sleep(50 * time.Millisecond)
		}
		if got := f.taskStatuses(id); got != "fix=waiting_approval monitoring=completed" || f.get(id)["status"] != "waiting_approval" {
			t.Fatalf("task: %s; execution %v", got, f.get(id)["status"])
		}
		pending := e.do("GET", "/v1/approvals?execution_id="+id+"&status=pending", approverA, nil, nil).json(t)["data"].([]any)
		if len(pending) != 1 {
			t.Fatalf("approval đang chờ: %v", pending)
		}
		decided := time.Now()
		want(t, e.do("POST", "/v1/approvals/"+pending[0].(map[string]any)["id"].(string)+"/decisions", approverA,
			map[string]any{"decision": "approve"}, map[string]string{"Idempotency-Key": uuid.NewString()}), 200, "")
		x := f.waitStatus(id, 60*time.Second, plannedTerminal...)
		// Signal tới đúng child workflow: không phải đợi lần tự kiểm tra sau hai phút.
		if d := time.Since(decided); x["status"] != "completed" || d > 30*time.Second {
			t.Fatalf("execution %v sau %s; task: %s", x["status"], d, f.taskStatuses(id))
		}
		if n := upstreamCalls("restart_container") - before; n != 1 {
			t.Errorf("restart_container chạy %d lần, muốn 1", n)
		}
		record(t, id, "fix", "planned_parent_approval", "planned_task_approval")
	})
}
