package api_test

import (
	"bytes"
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/hqgroups/mcp/internal/agent/runtime/durable"
	"github.com/hqgroups/mcp/internal/ai/provider"
)

// TestDurableDelegation chạy delegation trên Temporal thật: task con là child workflow
// của workflow đang giao việc. Worker chết giữa lúc task con chạy thì execution tiếp tục
// mà không tạo task con thứ hai; huỷ lan xuống task con; lịch sử chỉ chứa tham chiếu.
func TestDurableDelegation(t *testing.T) {
	e := setup(t, true)
	ctx := context.Background()
	adminA := e.tok("admin-a")
	f := newPlannedFixture(t, e, "delegation-durable")
	db := f.agent("database", []string{"list_containers"}, nil)
	f.agent("lead", nil, delegating(1, db))
	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := e.db.Admin.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	record := func(t *testing.T, id string, names ...string) {
		t.Helper()
		tasks := f.allTasks(id)
		ids := []string{durable.WorkflowID(e.orgA, uuid.MustParse(id)),
			durable.TaskWorkflowID(e.orgA, uuid.MustParse(id), uuid.MustParse(tasks[len(tasks)-1]["id"].(string)))}
		rp := worker.NewWorkflowReplayer()
		rp.RegisterWorkflowWithOptions(durable.Workflow, workflow.RegisterOptions{Name: durable.WorkflowName})
		rp.RegisterWorkflowWithOptions(durable.TaskWorkflow, workflow.RegisterOptions{Name: durable.TaskWorkflowName})
		for i, name := range names {
			h := e.historyOf(ids[i])
			raw, err := protojson.Marshal(h)
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"GOAL-", "AGENT:", "DB-KHOE", "LEAD-XONG", "Kiểm tra production", "result of list_containers"} {
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

	t.Run("worker dies while the delegated task runs; exactly one child is created and the parent continues", func(t *testing.T) {
		var killed atomic.Bool
		entered := make(chan struct{}, 1)
		e.model.script(func(ctx context.Context, _ int, req provider.Request) (provider.Response, error) {
			switch {
			case agentOf(req) == "database" && hasToolResult(req):
				return answer("DB-KHOE-MANH"), nil
			case agentOf(req) == "database" && !killed.Load():
				entered <- struct{}{}
				<-ctx.Done() // lượt đầu của task con treo tới khi worker bị dừng
				return provider.Response{}, ctx.Err()
			case agentOf(req) == "database":
				return toolUse("infra.list_containers"), nil
			case !hasToolResult(req):
				return delegateCall("database", "GOAL-CHO-DATABASE"), nil
			}
			return answer("LEAD-XONG"), nil
		})
		kill := e.startWorker(500 * time.Millisecond)
		id := f.single(adminA, "lead")
		select {
		case <-entered:
		case <-time.After(40 * time.Second):
			t.Fatal("task con không chạy")
		}
		killed.Store(true)
		kill()
		e.startWorker(5 * time.Second)
		x := f.waitStatus(id, 120*time.Second, plannedTerminal...)
		if x["status"] != "completed" || x["output"].(map[string]any)["message"] != "LEAD-XONG" {
			t.Fatalf("execution: %v %v", x["status"], x["error"])
		}
		tasks := f.allTasks(id)
		if len(tasks) != 2 || tasks[1]["status"] != "completed" || tasks[1]["parent_task_id"] != tasks[0]["id"] {
			t.Fatalf("task: %v", tasks)
		}
		// Task cha: lượt model, bước giao việc, lượt model. Task con: lượt model, tool, lượt model.
		if n := count(`SELECT count(*) FROM execution_steps WHERE execution_id = $1 AND status = 'succeeded'`, id); n != 6 {
			t.Errorf("số step thành công = %d, muốn 6", n)
		}
		if n := count(`SELECT count(*) FROM (SELECT task_id, step_seq FROM execution_steps WHERE execution_id = $1
			GROUP BY task_id, step_seq HAVING count(*) > 1) d`, id); n != 0 {
			t.Errorf("có %d step bị ghi trùng", n)
		}
		if n := count(`SELECT count(*) FROM usage_records WHERE execution_id = $1`, id); n != 4 {
			t.Errorf("usage_records = %d, muốn 4", n)
		}
		record(t, id, "delegation_parent", "delegation_child")
	})

	t.Run("cancel reaches the delegated child workflow", func(t *testing.T) {
		e.stopWorkers()
		e.startWorker(5 * time.Second)
		entered := make(chan struct{}, 1)
		var interrupted atomic.Bool
		e.model.script(func(ctx context.Context, _ int, req provider.Request) (provider.Response, error) {
			if agentOf(req) == "database" {
				entered <- struct{}{}
				<-ctx.Done()
				interrupted.Store(true)
				return provider.Response{}, ctx.Err()
			}
			return delegateCall("database", "GOAL-DAI"), nil
		})
		id := f.single(adminA, "lead")
		select {
		case <-entered:
		case <-time.After(40 * time.Second):
			t.Fatal("task con không chạy")
		}
		began := time.Now()
		want(t, e.do("POST", "/v1/executions/"+id+"/cancel", adminA, nil, nil), 202, "")
		x := f.waitStatus(id, 30*time.Second, plannedTerminal...)
		tasks := f.allTasks(id)
		if d := time.Since(began); x["status"] != "cancelled" || d > 20*time.Second || !interrupted.Load() ||
			len(tasks) != 2 || tasks[0]["status"] != "cancelled" || tasks[1]["status"] != "cancelled" {
			t.Fatalf("execution %v sau %s; task: %v", x["status"], d, tasks)
		}
		record(t, id, "delegation_parent_cancelled", "delegation_child_cancelled")
	})
}
