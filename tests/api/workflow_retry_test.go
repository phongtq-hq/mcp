package api_test

import (
	"bytes"
	"context"
	"os"
	"strings"
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

// retryDef: check (thử tối đa attempts lần) → report.
func retryDef(a, b string, attempts int, onFailure string) map[string]any {
	check := map[string]any{"id": "check", "type": "agent", "agent_id": a, "goal": "GOAL-check", "max_attempts": attempts}
	if onFailure != "" {
		check["on_failure"] = onFailure
	}
	return map[string]any{"steps": []any{check,
		map[string]any{"id": "report", "type": "agent", "agent_id": b, "goal": "GOAL-report", "depends_on": []string{"check"}}}}
}

// TestWorkflowStepRetry: bước agent có max_attempts được chạy lại khi thất bại; kết quả
// của bước là kết quả của lần thử thành công (agent-runtime §5.12).
func TestWorkflowStepRetry(t *testing.T) {
	e := setup(t)
	adminA := e.tok("admin-a")
	f := newPlannedFixture(t, e, "retry-a")
	mon := f.agent("monitoring", nil, nil)
	rep := f.agent("reporter", nil, nil)
	// failFirst: số lượt đầu của Agent monitoring bị từ chối (task thất bại).
	script := func(failFirst int32) *atomic.Int32 {
		var calls atomic.Int32
		e.model.script(func(_ context.Context, _ int, req provider.Request) (provider.Response, error) {
			if agentOf(req) == "monitoring" {
				n := calls.Add(1)
				if n <= failFirst {
					return provider.Response{StopReason: provider.StopRefusal, ModelUsed: "sonnet"}, nil
				}
				return answer("KET-QUA-LAN-" + string(rune('0'+n))), nil
			}
			return answer("BAO-CAO"), nil
		})
		return &calls
	}
	run := func(slug string, def map[string]any) (map[string]any, string) {
		t.Helper()
		r := f.createWorkflow(adminA, slug, def)
		want(t, r, 201, "")
		id := r.json(t)["id"].(string)
		f.activate(id)
		started := f.runWorkflow(adminA, id)
		want(t, started, 202, "")
		x := f.waitRun(started.json(t)["id"].(string), 30*time.Second)
		return x, x["execution_id"].(string)
	}
	reportPrompt := func() string {
		for _, req := range e.model.seen() {
			if agentOf(req) == "reporter" {
				return req.Messages[0].Text
			}
		}
		return ""
	}

	t.Run("definition is validated", func(t *testing.T) {
		for name, def := range map[string]map[string]any{
			"quá số lần thử": retryDef(mon, rep, 4, ""),
			"số âm":          retryDef(mon, rep, -1, ""),
			"trên bước wait": {"steps": []any{map[string]any{"id": "a", "type": "agent", "agent_id": mon, "goal": "g"},
				map[string]any{"id": "w", "type": "wait", "seconds": 1, "max_attempts": 2, "depends_on": []string{"a"}}}},
			"id trùng với lần thử lại": {"steps": []any{map[string]any{"id": "a", "type": "agent", "agent_id": mon, "goal": "g", "max_attempts": 2},
				map[string]any{"id": "a-try2", "type": "agent", "agent_id": rep, "goal": "g"}}},
		} {
			if r := f.createWorkflow(adminA, "bad-retry", def); r.status != 422 {
				t.Errorf("%s: status %d, muốn 422: %s", name, r.status, r.body)
			}
		}
	})

	t.Run("a failed attempt is retried; dependents get the successful attempt's result", func(t *testing.T) {
		calls := script(1)
		x, execID := run("retry-second", retryDef(mon, rep, 3, ""))
		if got := f.taskStatuses(execID); x["status"] != "completed" || got != "check=failed check-try2=completed check-try3=skipped report=completed" {
			t.Fatalf("lần chạy %v; task: %s", x["status"], got)
		}
		if n := calls.Load(); n != 2 {
			t.Errorf("Agent của bước được gọi %d lượt, muốn 2", n)
		}
		p := reportPrompt()
		if !strings.Contains(p, `<task_result id="check-try2" status="completed">`) || !strings.Contains(p, "KET-QUA-LAN-2") || strings.Contains(p, `status="skipped"`) {
			t.Errorf("bước sau không nhận kết quả của lần thử thành công: %s", p)
		}
		// Lần thử lại thấy lần trước đã thất bại (dữ liệu, không phải chỉ dẫn).
		for _, req := range e.model.seen() {
			if agentOf(req) == "monitoring" && strings.Contains(req.Messages[0].Text, "<task_results>") &&
				!strings.Contains(req.Messages[0].Text, `<task_result id="check" status="failed">`) {
				t.Errorf("lần thử lại không thấy lần thử trước: %s", req.Messages[0].Text)
			}
		}
	})

	t.Run("no retry after success", func(t *testing.T) {
		calls := script(0)
		x, execID := run("retry-none", retryDef(mon, rep, 3, ""))
		if got := f.taskStatuses(execID); x["status"] != "completed" || got != "check=completed check-try2=skipped check-try3=skipped report=completed" {
			t.Fatalf("lần chạy %v; task: %s", x["status"], got)
		}
		if n := calls.Load(); n != 1 {
			t.Errorf("Agent của bước được gọi %d lượt, muốn 1", n)
		}
		if p := reportPrompt(); !strings.Contains(p, `<task_result id="check" status="completed">`) || !strings.Contains(p, "KET-QUA-LAN-1") {
			t.Errorf("bước sau: %s", p)
		}
	})

	t.Run("attempts are bounded; the step's on_failure applies to the last attempt", func(t *testing.T) {
		calls := script(99)
		x, execID := run("retry-exhausted", retryDef(mon, rep, 2, ""))
		if got := f.taskStatuses(execID); x["status"] != "failed" || got != "check=failed check-try2=failed report=skipped" {
			t.Fatalf("lần chạy %v; task: %s", x["status"], got)
		}
		if n := calls.Load(); n != 2 {
			t.Errorf("Agent của bước được gọi %d lượt, muốn đúng 2", n)
		}
		script(99)
		x, execID = run("retry-continue", retryDef(mon, rep, 2, "continue"))
		if got := f.taskStatuses(execID); x["status"] != "completed" || got != "check=failed check-try2=failed report=completed" {
			t.Fatalf("on_failure continue: lần chạy %v; task: %s", x["status"], got)
		}
	})
}

// TestWorkflowStepRetryNeverRepeatsWrites: lần thử lại là một task mới, nên khoá
// idempotency của tool không khử trùng được giữa hai lần thử. Một bước đã gọi tool ghi
// (Gateway đã giữ chỗ cho nó) không được chạy lại, dù nó kết thúc failed.
func TestWorkflowStepRetryNeverRepeatsWrites(t *testing.T) {
	e := setup(t)
	adminA := e.tok("admin-a")
	f := newPlannedFixture(t, e, "retry-write")
	writer := f.agent("monitoring", []string{"create_ticket", "list_containers"}, nil)
	rep := f.agent("reporter", nil, nil)
	start := func(slug, tool string) (map[string]any, string, *atomic.Int32) {
		t.Helper()
		var turns atomic.Int32
		e.model.script(func(_ context.Context, _ int, req provider.Request) (provider.Response, error) {
			if agentOf(req) != "monitoring" {
				return answer("BAO-CAO"), nil
			}
			turns.Add(1)
			// Lượt đầu của mỗi lần thử gọi tool; lượt sau (đã có kết quả tool) bị từ chối → task failed.
			if len(req.Messages) == 1 {
				return provider.Response{StopReason: provider.StopToolUse, ModelUsed: "sonnet",
					ToolCalls: []provider.ToolCall{{ID: "c1", Name: "infra." + tool, Arguments: []byte(`{"name":"x"}`)}}}, nil
			}
			return provider.Response{StopReason: provider.StopRefusal, ModelUsed: "sonnet"}, nil
		})
		r := f.createWorkflow(adminA, slug, retryDef(writer, rep, 3, "continue"))
		want(t, r, 201, "")
		id := r.json(t)["id"].(string)
		f.activate(id)
		started := f.runWorkflow(adminA, id)
		want(t, started, 202, "")
		x := f.waitRun(started.json(t)["id"].(string), 30*time.Second)
		return x, x["execution_id"].(string), &turns
	}

	// Tool ghi đã chạy ở lần thử đầu: không lần thử lại nào chạy, tool chạy đúng một lần.
	before := f.upstreamCalls("create_ticket")
	_, execID, turns := start("retry-after-write", "create_ticket")
	if got := f.taskStatuses(execID); got != "check=failed check-try2=failed check-try3=failed report=completed" {
		t.Fatalf("task: %s", got)
	}
	if n := f.upstreamCalls("create_ticket") - before; n != 1 {
		t.Fatalf("tool ghi chạy %d lần, muốn đúng 1", n)
	}
	if n := turns.Load(); n != 2 {
		t.Errorf("Agent của bước được gọi %d lượt, muốn 2 (chỉ lần thử đầu)", n)
	}
	// Tool chỉ đọc không cản việc thử lại: cả ba lần thử đều chạy.
	before = f.upstreamCalls("list_containers")
	_, execID, turns = start("retry-after-read", "list_containers")
	if got := f.taskStatuses(execID); got != "check=failed check-try2=failed check-try3=failed report=completed" {
		t.Fatalf("task (chỉ đọc): %s", got)
	}
	if n, calls := turns.Load(), f.upstreamCalls("list_containers")-before; n != 6 || calls != 3 {
		t.Errorf("bước chỉ đọc: %d lượt model, %d lần gọi tool; muốn 6 và 3", n, calls)
	}
}

// TestDurableWorkflowRetry: cùng luật trên Temporal. Lịch sử của workflow cha được phát
// lại và (HQ_UPDATE_REPLAY_FIXTURE=1) ghi thành lịch sử mẫu planned_parent_retry_v1.json.
func TestDurableWorkflowRetry(t *testing.T) {
	e := setup(t, true)
	adminA := e.tok("admin-a")
	f := newPlannedFixture(t, e, "retry-durable")
	mon := f.agent("monitoring", nil, nil)
	rep := f.agent("reporter", nil, nil)
	var calls atomic.Int32
	e.model.script(func(_ context.Context, _ int, req provider.Request) (provider.Response, error) {
		if agentOf(req) == "monitoring" && calls.Add(1) == 1 {
			return provider.Response{StopReason: provider.StopRefusal, ModelUsed: "sonnet"}, nil
		}
		return answer("KET-QUA-" + agentOf(req)), nil
	})
	e.startWorker(5 * time.Second)
	r := f.createWorkflow(adminA, "durable-retry", retryDef(mon, rep, 3, ""))
	want(t, r, 201, "")
	id := r.json(t)["id"].(string)
	f.activate(id)
	run := f.runWorkflow(adminA, id)
	want(t, run, 202, "")
	done := f.waitRun(run.json(t)["id"].(string), 90*time.Second)
	execID := done["execution_id"].(string)
	if got := f.taskStatuses(execID); done["status"] != "completed" || got != "check=failed check-try2=completed check-try3=skipped report=completed" {
		t.Fatalf("lần chạy %v; task: %s", done["status"], got)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("Agent của bước được gọi %d lượt, muốn 2", n)
	}
	h := e.closedHistoryOf(durable.WorkflowID(e.orgA, uuid.MustParse(execID)))
	raw, err := protojson.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"GOAL-", "AGENT:", "KET-QUA-", "GHI-CHU", "durable-retry"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Errorf("lịch sử chứa nội dung %q", secret)
		}
	}
	rp := worker.NewWorkflowReplayer()
	rp.RegisterWorkflowWithOptions(durable.PlanWorkflow, workflow.RegisterOptions{Name: durable.PlanRetryWorkflowName})
	if err := rp.ReplayWorkflowHistory(quiet, h); err != nil {
		t.Errorf("replay: %v", err)
	}
	if os.Getenv("HQ_UPDATE_REPLAY_FIXTURE") == "1" {
		if host, err := os.Hostname(); err == nil && host != "" {
			raw = bytes.ReplaceAll(raw, []byte(host), []byte("fixture-host"))
		}
		//nolint:gosec // lịch sử chỉ chứa tham chiếu, là dữ liệu test được commit
		if err := os.WriteFile("../../internal/agent/runtime/durable/testdata/planned_parent_retry_v1.json", raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
