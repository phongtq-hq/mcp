package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	pgxv5 "github.com/jackc/pgx/v5"

	"github.com/hqgroups/mcp/internal/agent/orchestrator"
	"github.com/hqgroups/mcp/internal/agent/runtime/store"
	"github.com/hqgroups/mcp/internal/ai/provider"
	"github.com/hqgroups/mcp/internal/audit"
	"github.com/hqgroups/mcp/internal/platform/tenancy"
)

// Execution chế độ planned (agent-runtime §5): một execution, nhiều task chạy theo kế
// hoạch. Mỗi task vẫn là đúng vòng lặp Run của một Agent; phần ở đây là các bước có
// I/O quanh nó (mở / đóng từng task, đóng execution với kết quả tổng hợp) và cách chạy
// kế hoạch trong process. Trên Temporal, kế hoạch là workflow cha và mỗi task là một
// child workflow (package durable).

const (
	// ModeSingleAgent và ModePlanned là các chế độ execution đã hiện thực.
	ModeSingleAgent = "single_agent"
	ModePlanned     = "planned"
	// Nguồn gốc của kế hoạch một execution planned.
	PlanSourceCaller = "caller" // người gọi viết và gửi
	PlanSourceModel  = "model"  // Agent lập kế hoạch đề xuất, api chạy thẳng từ đề xuất

	// maxSummaryRunes là trần của output_summary: phần kết quả của một task mà các task
	// phụ thuộc nó thấy. Tóm tắt bằng model thuộc lát "quản lý context"; hiện là cắt.
	maxSummaryRunes = 4000
	// maxPlanMessageRunes là trần phần kết quả của một task trong output của execution.
	maxPlanMessageRunes = 100_000
)

// PlanTask là một task của kế hoạch ở dạng tham chiếu: không có goal hay prompt.
type PlanTask struct {
	Key  string `json:"key"`
	Spec Spec   `json:"spec"`
	// Kind là loại nút (rỗng = task của Agent; orchestrator.KindWait = nút chờ, khi đó
	// WaitSeconds là thời gian chờ và Spec chỉ mang tham chiếu tới execution).
	Kind        string `json:"kind,omitempty"`
	WaitSeconds int    `json:"wait_seconds,omitempty"`
	// DependsOn là chỉ số (trong PlanSpec.Tasks) của các task phải kết thúc trước.
	DependsOn []int  `json:"depends_on,omitempty"`
	OnFailure string `json:"on_failure"`
	// RetryOf (nil = không) là chỉ số của lần thử trước của cùng một bước; xem
	// orchestrator.Node.RetryOf.
	RetryOf *int `json:"retry_of,omitempty"`
	// TimeoutSeconds là thời hạn của riêng task, tính từ lúc nó bắt đầu chạy; task cũng
	// không sống quá hạn của execution (Spec.Task.Deadline).
	TimeoutSeconds int `json:"timeout_seconds"`
	// Depth là độ sâu của task trong cây giao việc (0 = task gốc / task của kế hoạch).
	Depth int `json:"depth,omitempty"`
}

// PlanSpec là mọi thứ cần để chạy một execution chế độ planned.
type PlanSpec struct {
	// Exec định danh execution (TaskID, AgentID, AgentVersionID để trống).
	Exec        TaskRef    `json:"exec"`
	Tasks       []PlanTask `json:"tasks"`
	MaxParallel int        `json:"max_parallel"`
}

// Nodes trả về kế hoạch ở dạng bộ lập lịch cần.
func (p PlanSpec) Nodes() []orchestrator.Node {
	nodes := make([]orchestrator.Node, len(p.Tasks))
	for i, t := range p.Tasks {
		nodes[i] = orchestrator.Node{DependsOn: t.DependsOn, OnFailure: t.OnFailure, RetryOf: t.RetryOf}
	}
	return nodes
}

// TaskDeadline là hạn của một task bắt đầu chạy lúc now.
func (t PlanTask) TaskDeadline(now time.Time) time.Time {
	d := t.Spec.Task.Deadline
	if t.TimeoutSeconds > 0 {
		if own := now.Add(time.Duration(t.TimeoutSeconds) * time.Second); own.Before(d) {
			d = own
		}
	}
	return d
}

// storedPlan là kế hoạch đã kiểm, chụp vào agent_executions.plan lúc tạo execution.
type storedPlan struct {
	MaxParallelTasks int          `json:"max_parallel_tasks"`
	Tasks            []storedTask `json:"tasks"`
}

type storedTask struct {
	Key string `json:"id"`
	// Kind rỗng: task của Agent (TaskID, AgentID). KindWait: nút chờ (WaitSeconds), trạng
	// thái của nó nằm ở execution_plan_steps.
	Kind        string    `json:"kind,omitempty"`
	WaitSeconds int       `json:"wait_seconds,omitempty"`
	TaskID      uuid.UUID `json:"task_id"`
	AgentID     uuid.UUID `json:"agent_id"`
	DependsOn   []string  `json:"depends_on,omitempty"`
	OnFailure   string    `json:"on_failure"`
	// RetryOf là khoá của lần thử trước của cùng một bước (rỗng = không phải lần thử lại).
	RetryOf string `json:"retry_of,omitempty"`
}

// index trả về vị trí của nút mang khoá key (-1 nếu không có).
func (p storedPlan) index(key string) int {
	return slices.IndexFunc(p.Tasks, func(t storedTask) bool { return t.Key == key })
}

// effective trả về "kết quả của bước" cho nút j và vị trí của nút mang kết quả đó: một
// nút thử lại bị bỏ qua mang kết quả của lần thử trước nó (orchestrator.Effective). status
// đọc trạng thái hiện tại của một nút; ok = false khi không đọc được.
func (p storedPlan) effective(status func(storedTask) (string, bool), j int) (st string, at int, ok bool) {
	// Một lần thử nào đó của bước đã hoàn thành thì bước đã hoàn thành, dù nút thử lại sau
	// nó ở trạng thái gì (chưa kịp được bỏ qua khi execution dừng, hoặc việc ghi "bỏ qua"
	// bị lỗi): lần thử lại của một bước đã thành công không bao giờ được chạy.
	for k, hops := j, 0; k >= 0 && k < len(p.Tasks) && hops < len(p.Tasks); hops++ {
		if s, ok := status(p.Tasks[k]); ok && s == StatusCompleted {
			return s, k, true
		}
		if p.Tasks[k].RetryOf == "" {
			break
		}
		k = p.index(p.Tasks[k].RetryOf)
	}
	for range p.Tasks {
		if j < 0 || j >= len(p.Tasks) {
			return "", -1, false
		}
		st, ok = status(p.Tasks[j])
		if !ok || st != orchestrator.Skipped || p.Tasks[j].RetryOf == "" {
			return st, j, ok
		}
		j = p.index(p.Tasks[j].RetryOf)
	}
	return "", -1, false
}

func (p storedPlan) nodes() ([]orchestrator.Node, bool) {
	index := make(map[string]int, len(p.Tasks))
	for i, t := range p.Tasks {
		index[t.Key] = i
	}
	nodes := make([]orchestrator.Node, len(p.Tasks))
	for i, t := range p.Tasks {
		nodes[i].OnFailure = t.OnFailure
		if t.RetryOf != "" {
			j, ok := index[t.RetryOf]
			if !ok {
				return nil, false
			}
			nodes[i].RetryOf = &j
		}
		for _, k := range t.DependsOn {
			j, ok := index[k]
			if !ok {
				return nil, false
			}
			nodes[i].DependsOn = append(nodes[i].DependsOn, j)
		}
	}
	return nodes, true
}

func terminalStatus(s string) bool {
	return slices.Contains([]string{StatusCompleted, StatusFailed, StatusCancelled, StatusTimedOut, StatusBudgetExceeded}, s)
}

// outcomeError dựng lỗi (RFC 9457) của một task / execution không hoàn thành. Thông
// báo do platform sinh từ mã lỗi, không lấy văn bản từ tham chiếu đi qua Temporal.
func outcomeError(status, code string) json.RawMessage {
	if status == StatusCompleted || status == StatusCancelled {
		return nil
	}
	if code == "" {
		code = status
	}
	title, ok := outcomeMessages[code]
	if !ok {
		code, title = CodeInternal, outcomeMessages[CodeInternal]
	}
	return mustJSON(map[string]any{"type": "about:blank", "title": title, "status": 0, "code": code})
}

// finalText đọc trả lời của lượt model gần nhất của task ("" nếu chưa có).
func finalText(ctx context.Context, q *store.Queries, task TaskRef, out Outcome) (string, error) {
	if out.FinalStepSeq <= 0 {
		return "", nil
	}
	step, err := q.GetStep(ctx, store.GetStepParams{OrgID: task.OrgID, TaskID: task.TaskID, StepSeq: int32(out.FinalStepSeq), Since: task.Since}) //nolint:gosec // step_seq nhỏ
	if errors.Is(err, pgxv5.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	// Chỉ trả lời của một lượt model đã thành công: tham chiếu trỏ vào step khác (kết
	// quả tool cũng có trường text) không được thành output của task.
	var resp provider.Response
	if step.Kind != kindLLM || step.Status != stepSucceeded || json.Unmarshal(step.Response, &resp) != nil {
		return "", nil
	}
	return resp.Text, nil
}

// lockExecution khoá execution và đối chiếu tham chiếu (đi qua Temporal) với dòng thật.
func lockExecution(ctx context.Context, q *store.Queries, ref TaskRef) (store.LockExecutionRow, error) {
	exec, err := q.LockExecution(ctx, store.LockExecutionParams{OrgID: ref.OrgID, ID: ref.ExecutionID})
	if errors.Is(err, pgxv5.ErrNoRows) {
		return exec, errTaskMismatch
	}
	if err != nil {
		return exec, err
	}
	if exec.ProjectID != ref.ProjectID || exec.InitiatorType != ref.InitiatorType ||
		exec.InitiatorID == nil || *exec.InitiatorID != ref.InitiatorID {
		return exec, errTaskMismatch
	}
	return exec, nil
}

// syncWaiting đặt trạng thái của execution theo các task của nó: waiting_approval khi
// có ít nhất một task đang chờ duyệt, hoặc một nút approval của kế hoạch đang chờ người duyệt; không thì paused khi có task đang tạm dừng; không
// thì running. Gọi dưới khoá của execution. Execution đã kết thúc không bị đụng tới.
func syncWaiting(ctx context.Context, q *store.Queries, orgID, executionID uuid.UUID) error {
	held, err := q.CountHeldTasks(ctx, store.CountHeldTasksParams{OrgID: orgID, ExecutionID: executionID})
	if err != nil {
		return err
	}
	gates, err := q.CountWaitingPlanSteps(ctx, store.CountWaitingPlanStepsParams{OrgID: orgID, ExecutionID: executionID})
	if err != nil {
		return err
	}
	to := "running"
	switch {
	case held.Waiting > 0 || gates > 0:
		to = StatusWaitingApproval
	case held.Paused > 0:
		to = StatusPaused
	}
	return q.SetExecutionHeld(ctx, store.SetExecutionHeldParams{OrgID: orgID, ID: executionID, ToStatus: to})
}

// BeginPlan chuyển execution chế độ planned sang running.
func (s *Steps) BeginPlan(ctx context.Context, exec TaskRef) error {
	ctx = taskContext(ctx, exec)
	return tenancy.InTx(ctx, s.DB, func(tx pgxv5.Tx) error {
		q := store.New(tx)
		row, err := lockExecution(ctx, q, exec)
		if err != nil {
			return err
		}
		switch row.Status {
		case "queued":
			_, err := q.StartExecution(ctx, store.StartExecutionParams{OrgID: exec.OrgID, ID: exec.ExecutionID})
			return err
		case "running", StatusWaitingApproval, StatusPaused:
			return nil // bước này chạy lại
		}
		return errTaskClosed
	})
}

// BeginTask chuyển một task của execution nhiều task sang running. Task chỉ bắt đầu
// khi execution của nó còn chạy.
func (s *Steps) BeginTask(ctx context.Context, task TaskRef) error {
	ctx = taskContext(ctx, task)
	return tenancy.InTx(ctx, s.DB, func(tx pgxv5.Tx) error {
		q := store.New(tx)
		exec, err := lockExecution(ctx, q, task)
		if err != nil {
			return err
		}
		if exec.Status != "running" && exec.Status != StatusWaitingApproval && exec.Status != StatusPaused {
			return errTaskClosed
		}
		// Thứ tự chạy đến từ workflow (tham chiếu): database quyết định task có được bắt
		// đầu hay không. Execution phải là planned, chưa bị huỷ, còn hạn; mọi task nó phụ
		// thuộc phải đã kết thúc và không chặn nó.
		head, err := q.GetExecutionPlan(ctx, store.GetExecutionPlanParams{OrgID: task.OrgID, ID: task.ExecutionID})
		if err != nil {
			return err
		}
		if head.CancelRequestedAt != nil || !time.Now().Before(head.DeadlineAt) {
			return errTaskClosed
		}
		self, err := q.GetTaskContext(ctx, store.GetTaskContextParams{OrgID: task.OrgID, ID: task.TaskID})
		if errors.Is(err, pgxv5.ErrNoRows) || (err == nil && self.ExecutionID != task.ExecutionID) {
			return errTaskMismatch
		}
		if err != nil {
			return err
		}
		// Task được mở bằng bước này là task của một kế hoạch, hoặc task con của một lần
		// giao việc (do agent_task_delegate tạo). Task gốc của execution đơn Agent thì không.
		if head.Mode != ModePlanned && self.ParentTaskID == nil {
			return errTaskMismatch
		}
		// Task của kế hoạch: phụ thuộc (task, nút chờ, cổng, luật thử lại) kiểm theo kế hoạch
		// đã chụp trong database.
		inPlan := false
		if head.Mode == ModePlanned && self.ParentTaskID == nil {
			var plan storedPlan
			if json.Unmarshal(head.Plan, &plan) != nil {
				return errTaskMismatch
			}
			if i := slices.IndexFunc(plan.Tasks, func(t storedTask) bool { return t.Kind == "" && t.TaskID == task.TaskID }); i >= 0 {
				inPlan = true
				if err := planDepsDone(ctx, q, task, plan, i); err != nil {
					return err
				}
			}
		}
		if !inPlan && len(self.DependsOn) > 0 {
			deps, err := q.ListDependencyResults(ctx, store.ListDependencyResultsParams{OrgID: task.OrgID, ExecutionID: task.ExecutionID, Ids: self.DependsOn})
			if err != nil {
				return err
			}
			if len(deps) != len(self.DependsOn) {
				return errTaskMismatch
			}
			for _, d := range deps {
				done := terminalStatus(d.Status) || d.Status == orchestrator.Skipped
				if !done || (d.Status != StatusCompleted && d.OnFailure != orchestrator.Continue) {
					return errTaskClosed
				}
			}
		}
		n, err := q.StartPlanTask(ctx, store.StartPlanTaskParams{OrgID: task.OrgID, ID: task.TaskID, ExecutionID: task.ExecutionID})
		if err != nil || n > 0 {
			return err
		}
		locked, err := q.LockTask(ctx, store.LockTaskParams{OrgID: task.OrgID, ID: task.TaskID})
		if errors.Is(err, pgxv5.ErrNoRows) || (err == nil && locked.ExecutionID != task.ExecutionID) {
			return errTaskMismatch
		}
		if err != nil {
			return err
		}
		if locked.Status != "running" && locked.Status != StatusWaitingApproval && locked.Status != StatusPaused {
			return errTaskClosed // đã kết thúc hoặc bị bỏ qua: không chạy
		}
		return nil
	})
}

// FinishTask đóng MỘT task của execution nhiều task ở trạng thái kết thúc, kèm trả lời
// cuối và tóm tắt của nó. Execution không bị đóng ở đây.
func (s *Steps) FinishTask(ctx context.Context, task TaskRef, out Outcome) error {
	ctx, cancel := finishCtx(taskContext(ctx, task))
	defer cancel()
	if !terminalStatus(out.Status) {
		return errTaskMismatch
	}
	errJSON := outcomeError(out.Status, out.ErrorCode)
	return tenancy.InTx(ctx, s.DB, func(tx pgxv5.Tx) error {
		q := store.New(tx)
		exec, err := lockExecution(ctx, q, task)
		if err != nil {
			return err
		}
		locked, err := q.LockTask(ctx, store.LockTaskParams{OrgID: task.OrgID, ID: task.TaskID})
		if errors.Is(err, pgxv5.ErrNoRows) || (err == nil && locked.ExecutionID != task.ExecutionID) {
			return errTaskMismatch
		}
		if err != nil {
			return err
		}
		if err := q.CloseOpenSteps(ctx, store.CloseOpenStepsParams{OrgID: task.OrgID, TaskID: task.TaskID, Since: task.Since,
			Error: mustJSON(stepError{Code: CodeInterrupted, Message: "bước bị ngắt trước khi có kết quả"})}); err != nil {
			return err
		}
		text, err := finalText(ctx, q, task, out)
		if err != nil {
			return err
		}
		var output json.RawMessage
		var summary *string
		if text != "" {
			output = mustJSON(map[string]string{"message": text})
			sum := truncate(text, maxSummaryRunes)
			summary = &sum
		}
		// Task không bắt đầu được vì execution đã có yêu cầu huỷ là bị huỷ, không phải lỗi.
		status := out.Status
		if status == StatusFailed && out.ErrorCode == CodeInternal && out.Iterations == 0 && exec.CancelRequestedAt != nil {
			status, errJSON = StatusCancelled, nil
		}
		n, err := q.FinishTask(ctx, store.FinishTaskParams{OrgID: task.OrgID, ID: task.TaskID, ExecutionID: task.ExecutionID,
			Status: status, Output: output, OutputSummary: summary, Error: errJSON})
		if err != nil || n == 0 {
			return err // n == 0: đã được đóng ở nơi khác, hoặc bước này chạy lại
		}
		// Task con (delegation) còn mở của task này: không ai chạy chúng nữa. Đóng chúng và
		// step đang mở của chúng, để chúng không còn là task "đang chạy" với Gateway và
		// không giữ execution ở waiting_approval.
		orphans, err := q.CloseOpenDescendants(ctx, store.CloseOpenDescendantsParams{OrgID: task.OrgID, ID: &task.TaskID})
		if err != nil {
			return err
		}
		if len(orphans) > 0 {
			if err := q.CloseOpenStepsOfTasks(ctx, store.CloseOpenStepsOfTasksParams{OrgID: task.OrgID, TaskIds: orphans, Since: task.Since,
				Error: mustJSON(stepError{Code: CodeInterrupted, Message: "bước bị ngắt trước khi có kết quả"})}); err != nil {
				return err
			}
		}
		// Task này có thể là task duy nhất đang chờ duyệt của execution.
		if err := syncWaiting(ctx, q, task.OrgID, task.ExecutionID); err != nil {
			return err
		}

		outcome := audit.OutcomeSuccess
		if out.Status != StatusCompleted {
			outcome = audit.OutcomeError
		}
		return audit.Record(ctx, tx, audit.Event{
			OrgID: task.OrgID, ProjectID: &task.ProjectID, Action: "task.finish", ResourceType: "task",
			ResourceID: &task.TaskID, ExecutionID: &task.ExecutionID, Outcome: outcome,
			Details: map[string]string{"status": status, "error_code": out.ErrorCode, "agent_id": locked.AgentID.String(),
				"iterations": strconv.Itoa(out.Iterations), "tool_calls": strconv.Itoa(out.ToolCalls)}})
	})
}

// SkipTask ghi nhận một task không được chạy (một task nó phụ thuộc không hoàn thành).
// Chỉ task còn queued mới bỏ qua được.
func (s *Steps) SkipTask(ctx context.Context, task TaskRef) error {
	ctx, cancel := finishCtx(taskContext(ctx, task))
	defer cancel()
	return tenancy.InTx(ctx, s.DB, func(tx pgxv5.Tx) error {
		q := store.New(tx)
		if _, err := lockExecution(ctx, q, task); err != nil {
			return err
		}
		n, err := q.SkipTask(ctx, store.SkipTaskParams{OrgID: task.OrgID, ID: task.TaskID, ExecutionID: task.ExecutionID})
		if err != nil || n == 0 {
			return err
		}
		reason := "dependency_not_completed"
		// Nút thử lại bị bỏ qua vì lần thử trước đã thành công: không phải một thất bại.
		if head, err := q.GetExecutionPlan(ctx, store.GetExecutionPlanParams{OrgID: task.OrgID, ID: task.ExecutionID}); err == nil {
			var plan storedPlan
			if json.Unmarshal(head.Plan, &plan) == nil {
				i := slices.IndexFunc(plan.Tasks, func(t storedTask) bool { return t.Kind == "" && t.TaskID == task.TaskID })
				if i >= 0 && plan.Tasks[i].RetryOf != "" {
					if rows, err := q.ListPlanTasks(ctx, store.ListPlanTasksParams{OrgID: task.OrgID, ExecutionID: task.ExecutionID}); err == nil {
						st, _, _ := plan.effective(func(t storedTask) (string, bool) {
							for _, r := range rows {
								if r.ID == t.TaskID {
									return r.Status, true
								}
							}
							return "", false
						}, i)
						if st == StatusCompleted {
							reason = "retry_not_needed"
						}
					}
				}
			}
		}
		return audit.Record(ctx, tx, audit.Event{
			OrgID: task.OrgID, ProjectID: &task.ProjectID, Action: "task.skip", ResourceType: "task",
			ResourceID: &task.TaskID, ExecutionID: &task.ExecutionID, Outcome: audit.OutcomeSuccess,
			Details: map[string]string{"reason": reason}})
	})
}

// planDepsDone kiểm trong database rằng mọi bước mà nút thứ i của kế hoạch phụ thuộc đã
// kết thúc và không chặn nó (hoàn thành, hoặc on_failure của bước đó là continue).
// waitOnly: chỉ xét các nút chờ (BeginTask đã kiểm các task của Agent qua depends_on).
func planDepsDone(ctx context.Context, q *store.Queries, exec TaskRef, plan storedPlan, i int) error {
	if len(plan.Tasks[i].DependsOn) == 0 {
		return nil
	}
	steps, err := q.ListPlanSteps(ctx, store.ListPlanStepsParams{OrgID: exec.OrgID, ExecutionID: exec.ExecutionID})
	if err != nil {
		return err
	}
	stepStatus := make(map[string]string, len(steps))
	for _, r := range steps {
		stepStatus[r.PlanKey] = r.Status
	}
	tasks, err := q.ListPlanTasks(ctx, store.ListPlanTasksParams{OrgID: exec.OrgID, ExecutionID: exec.ExecutionID})
	if err != nil {
		return err
	}
	byID := make(map[uuid.UUID]string, len(tasks))
	for _, r := range tasks {
		byID[r.ID] = r.Status
	}
	status := func(t storedTask) (string, bool) {
		if orchestrator.IsStep(t.Kind) {
			st, ok := stepStatus[t.Key]
			return st, ok
		}
		st, ok := byID[t.TaskID]
		return st, ok
	}
	for _, key := range plan.Tasks[i].DependsOn {
		j := plan.index(key)
		if j < 0 {
			return errTaskMismatch
		}
		dep := plan.Tasks[j]
		// Lần thử lại chỉ chạy khi lần thử trước của nó đã chạy và thất bại (failed /
		// timed_out): nơi chạy kế hoạch không tự chạy lại được một bước đã thành công.
		if plan.Tasks[i].RetryOf == key {
			st, ok := status(dep)
			if !ok {
				return errTaskMismatch
			}
			if st != StatusFailed && st != StatusTimedOut {
				return errTaskClosed
			}
			// ... và khi MỌI lần thử trước của bước chắc chắn chưa gây tác động (không
			// tool ghi nào được Gateway giữ chỗ, không kết quả không xác định, không giao
			// việc): lần thử lại là task mới, khoá idempotency của tool không khử trùng
			// được giữa hai lần thử. Duyệt cả chuỗi: một lần thử bị từ chối ở đây kết thúc
			// mà không chạy, và không được "rửa" lịch sử cho lần sau.
			for k, hops := j, 0; k >= 0 && hops < len(plan.Tasks); hops++ {
				safe, err := q.TaskRetrySafe(ctx, plan.Tasks[k].TaskID)
				if err != nil {
					return err
				}
				if !safe {
					return errRetryUnsafe
				}
				if plan.Tasks[k].RetryOf == "" {
					break
				}
				k = plan.index(plan.Tasks[k].RetryOf)
			}
			continue
		}
		st, _, ok := plan.effective(status, j)
		if !ok {
			return errTaskMismatch
		}
		done := terminalStatus(st) || st == orchestrator.Skipped
		if !done || (st != StatusCompleted && dep.OnFailure != orchestrator.Continue) {
			return errTaskClosed
		}
	}
	return gatesOpen(ctx, q, exec, plan.Tasks[i].Key)
}

// gatesOpen kiểm trong database rằng mọi cổng approval mà một nút của kế hoạch phụ thuộc,
// trực tiếp hay gián tiếp, đã mở. Một cổng bị từ chối không mở lại được bởi on_failure:
// continue của một bước ở giữa.
func gatesOpen(ctx context.Context, q *store.Queries, exec TaskRef, key string) error {
	open, err := q.PlanNodeGatesOpen(ctx, store.PlanNodeGatesOpenParams{ExecutionID: exec.ExecutionID, PlanKey: key})
	if err != nil {
		return err
	}
	if !open {
		return errTaskClosed
	}
	return nil
}

// planStep tìm nút không phải Agent (wait, approval) theo khoá trong kế hoạch đã lưu của execution.
func planStep(head store.GetExecutionPlanRow, key string) (storedPlan, int, error) {
	var plan storedPlan
	if head.Mode != ModePlanned || json.Unmarshal(head.Plan, &plan) != nil {
		return plan, -1, errTaskMismatch
	}
	i := slices.IndexFunc(plan.Tasks, func(t storedTask) bool { return orchestrator.IsStep(t.Kind) && t.Key == key })
	if i < 0 {
		return plan, -1, errTaskMismatch
	}
	return plan, i, nil
}

func planStepStatus(ctx context.Context, q *store.Queries, exec TaskRef, key string) (string, error) {
	steps, err := q.ListPlanSteps(ctx, store.ListPlanStepsParams{OrgID: exec.OrgID, ExecutionID: exec.ExecutionID})
	if err != nil {
		return "", err
	}
	for _, r := range steps {
		if r.PlanKey == key {
			return r.Status, nil
		}
	}
	return "", errTaskMismatch
}

// BeginPlanStep chuyển một nút không phải Agent của kế hoạch (nút chờ) sang running. Như
// BeginTask, thứ tự chạy đến từ nơi chạy kế hoạch (tham chiếu) và database quyết định:
// execution còn chạy, chưa bị huỷ, còn hạn; nút có trong kế hoạch đã lưu; mọi bước nó
// phụ thuộc đã kết thúc và không chặn nó.
func (s *Steps) BeginPlanStep(ctx context.Context, exec TaskRef, key string) error {
	ctx = taskContext(ctx, exec)
	return tenancy.InTx(ctx, s.DB, func(tx pgxv5.Tx) error {
		q := store.New(tx)
		row, err := lockExecution(ctx, q, exec)
		if err != nil {
			return err
		}
		if (row.Status != "running" && row.Status != StatusWaitingApproval && row.Status != StatusPaused) || row.CancelRequestedAt != nil {
			return errTaskClosed
		}
		head, err := q.GetExecutionPlan(ctx, store.GetExecutionPlanParams{OrgID: exec.OrgID, ID: exec.ExecutionID})
		if err != nil {
			return err
		}
		if !time.Now().Before(head.DeadlineAt) {
			return errTaskClosed
		}
		plan, i, err := planStep(head, key)
		if err != nil {
			return err
		}
		if err := planDepsDone(ctx, q, exec, plan, i); err != nil {
			return err
		}
		n, err := q.StartPlanStep(ctx, store.StartPlanStepParams{OrgID: exec.OrgID, ExecutionID: exec.ExecutionID, PlanKey: key})
		if err != nil {
			return err
		}
		if n == 0 {
			st, err := planStepStatus(ctx, q, exec, key)
			if err != nil {
				return err
			}
			if st != "running" {
				return errTaskClosed // đã kết thúc hoặc bị bỏ qua: không chạy
			}
		}
		if plan.Tasks[i].Kind != orchestrator.KindApproval {
			return nil
		}
		// Nút approval: approval của nó được tạo ở đây, bởi hàm trong database (nội dung
		// lấy từ cấu hình đã chụp của nút). Gọi lại trả về approval đã có.
		id, err := q.RequestPlanApproval(ctx, store.RequestPlanApprovalParams{ExecutionID: exec.ExecutionID, PlanKey: key})
		if err != nil {
			return err
		}
		if id == uuid.Nil {
			return errTaskClosed
		}
		return syncWaiting(ctx, q, exec.OrgID, exec.ExecutionID)
	})
}

// PlanApprovalPending cho biết approval của một nút approval còn chờ quyết định hay không.
func (s *Steps) PlanApprovalPending(ctx context.Context, exec TaskRef, key string) (bool, error) {
	ctx = taskContext(ctx, exec)
	pending := false
	err := tenancy.InTx(ctx, s.DB, func(tx pgxv5.Tx) error {
		st, err := store.New(tx).PlanApprovalStatus(ctx, store.PlanApprovalStatusParams{ExecutionID: exec.ExecutionID, PlanKey: key})
		pending = st == "pending"
		return err
	})
	return pending, err
}

// ClosePlanApproval kết thúc một nút approval và trả về trạng thái của nút. Kết quả do
// database quyết định và ghi (hàm plan_step_close_approval), không do nơi chạy kế hoạch:
// nút chỉ "completed" khi approval của nó đã đủ người duyệt (approval được tiêu thụ,
// dùng một lần); bị từ chối hoặc hết hạn là "failed"; còn chờ khi kế hoạch dừng thì
// approval bị huỷ. stop (cancelled / timed_out, hoặc rỗng) là gợi ý của nơi gọi rằng kế
// hoạch đang dừng: nó chỉ có thể làm cổng đóng lại. Approval còn chờ mà không có lý do
// dừng thì trả lỗi thử lại được và không đổi gì.
func (s *Steps) ClosePlanApproval(ctx context.Context, exec TaskRef, key string, stop string) (string, error) {
	ctx, cancel := finishCtx(taskContext(ctx, exec))
	defer cancel()
	final := StatusCancelled
	err := tenancy.InTx(ctx, s.DB, func(tx pgxv5.Tx) error {
		q := store.New(tx)
		if _, err := lockExecution(ctx, q, exec); err != nil {
			return err
		}
		var err error
		final, err = closePlanApproval(ctx, q, exec, key, stop)
		return err
	})
	return final, err
}

// errGatePending: approval của cổng còn chờ và không có lý do dừng. Nơi gọi chờ tiếp
// (lỗi thử lại được), không coi cổng là đã đóng.
var errGatePending = errors.New("runtime: approval của cổng còn chờ quyết định")

func closePlanApproval(ctx context.Context, q *store.Queries, exec TaskRef, key, stop string) (string, error) {
	if stop != StatusCancelled && stop != StatusTimedOut {
		stop = ""
	}
	final, err := q.ClosePlanApproval(ctx, store.ClosePlanApprovalParams{ExecutionID: exec.ExecutionID, PlanKey: key, Stop: stop})
	if err != nil {
		return "", err
	}
	switch final {
	case "":
		return "", errTaskMismatch
	case "pending":
		return "", errGatePending
	}
	return final, syncWaiting(ctx, q, exec.OrgID, exec.ExecutionID)
}

// errWaitNotElapsed: nút chờ chưa chờ đủ theo đồng hồ của database.
var errWaitNotElapsed = errors.New("runtime: nút chờ chưa chờ đủ thời gian của nó")

// FinishPlanStep đóng một nút không phải Agent của kế hoạch. "completed" chỉ được nhận
// khi nút đã chạy và đã chờ đủ thời gian của nó (kiểm trong database).
func (s *Steps) FinishPlanStep(ctx context.Context, exec TaskRef, key, status string) error {
	ctx, cancel := finishCtx(taskContext(ctx, exec))
	defer cancel()
	if !slices.Contains([]string{StatusCompleted, StatusFailed, StatusCancelled, StatusTimedOut}, status) {
		return errTaskMismatch
	}
	return tenancy.InTx(ctx, s.DB, func(tx pgxv5.Tx) error {
		q := store.New(tx)
		if _, err := lockExecution(ctx, q, exec); err != nil {
			return err
		}
		// Nút approval không nhận trạng thái từ nơi gọi: kết quả của nó là của approval.
		head, err := q.GetExecutionPlan(ctx, store.GetExecutionPlanParams{OrgID: exec.OrgID, ID: exec.ExecutionID})
		if err != nil {
			return err
		}
		plan, i, err := planStep(head, key)
		if err != nil {
			return err
		}
		if plan.Tasks[i].Kind == orchestrator.KindApproval {
			_, err := closePlanApproval(ctx, q, exec, key, status)
			return err
		}
		n, err := q.FinishPlanStep(ctx, store.FinishPlanStepParams{OrgID: exec.OrgID, ExecutionID: exec.ExecutionID, PlanKey: key, Status: status})
		if err != nil || n > 0 {
			return err
		}
		st, err := planStepStatus(ctx, q, exec, key)
		if err != nil {
			return err
		}
		if st == "queued" || st == "running" {
			// "completed" cho một nút chưa chạy hoặc chưa chờ đủ. Lỗi thử lại được: độ lệch
			// đồng hồ nhỏ tự hết sau vài lần thử; nút chưa chạy thì hết lần thử và không đóng.
			return errWaitNotElapsed
		}
		return nil // đã được đóng ở nơi khác, hoặc bước này chạy lại
	})
}

// SkipPlanStep ghi nhận một nút chờ không được chạy (một bước nó phụ thuộc không hoàn
// thành). Chỉ nút còn queued mới bỏ qua được.
func (s *Steps) SkipPlanStep(ctx context.Context, exec TaskRef, key string) error {
	ctx, cancel := finishCtx(taskContext(ctx, exec))
	defer cancel()
	return tenancy.InTx(ctx, s.DB, func(tx pgxv5.Tx) error {
		q := store.New(tx)
		if _, err := lockExecution(ctx, q, exec); err != nil {
			return err
		}
		_, err := q.SkipPlanStep(ctx, store.SkipPlanStepParams{OrgID: exec.OrgID, ExecutionID: exec.ExecutionID, PlanKey: key})
		return err
	})
}

// PlanStop là gợi ý của nơi chạy kế hoạch về lý do nó dừng. Database vẫn là nguồn sự
// thật cho yêu cầu huỷ và hạn của execution.
type PlanStop struct {
	Cancelled bool `json:"cancelled,omitempty"`
	Deadline  bool `json:"deadline,omitempty"`
}

// FinishPlan đóng execution chế độ planned: task còn mở bị đóng theo, trạng thái của
// execution được suy ra từ trạng thái các task TRONG DATABASE (không từ tham chiếu), và
// output là kết quả tổng hợp deterministic: kết quả của các task cuối của kế hoạch kèm
// trạng thái và tóm tắt của từng task.
func (s *Steps) FinishPlan(ctx context.Context, exec TaskRef, hint PlanStop) error {
	ctx, cancel := finishCtx(taskContext(ctx, exec))
	defer cancel()
	return tenancy.InTx(ctx, s.DB, func(tx pgxv5.Tx) error {
		q := store.New(tx)
		locked, err := lockExecution(ctx, q, exec)
		if err != nil {
			return err
		}
		if locked.Status != "queued" && locked.Status != "running" && locked.Status != StatusWaitingApproval && locked.Status != StatusPaused {
			return nil // đã được đóng ở nơi khác (hết hạn), hoặc bước này chạy lại
		}
		head, err := q.GetExecutionPlan(ctx, store.GetExecutionPlanParams{OrgID: exec.OrgID, ID: exec.ExecutionID})
		if err != nil {
			return err
		}
		var plan storedPlan
		if head.Mode != ModePlanned || json.Unmarshal(head.Plan, &plan) != nil || len(plan.Tasks) == 0 {
			return errTaskMismatch
		}
		nodes, ok := plan.nodes()
		if !ok {
			return errTaskMismatch
		}
		stop := orchestrator.NotStopped
		switch {
		case head.CancelRequestedAt != nil || hint.Cancelled:
			stop = orchestrator.StopCancelled
		case hint.Deadline || !time.Now().Before(head.DeadlineAt):
			stop = orchestrator.StopDeadline
		}
		// Task còn mở: nơi chạy kế hoạch đã dừng nên không ai chạy chúng nữa.
		closing := StatusCancelled
		if stop == orchestrator.StopDeadline {
			closing = StatusTimedOut
		}
		if err := q.FinishTasks(ctx, store.FinishTasksParams{OrgID: exec.OrgID, ExecutionID: exec.ExecutionID,
			Status: closing, Error: outcomeError(closing, "")}); err != nil {
			return err
		}
		if err := q.CloseOpenStepsOfExecution(ctx, store.CloseOpenStepsOfExecutionParams{OrgID: exec.OrgID, ExecutionID: exec.ExecutionID,
			Since: exec.Since, Error: mustJSON(stepError{Code: CodeInterrupted, Message: "bước bị ngắt trước khi có kết quả"})}); err != nil {
			return err
		}
		// Cổng approval còn mở: approval của nó bị huỷ, không nhận quyết định nữa.
		if err := q.CloseOpenPlanApprovals(ctx, exec.ExecutionID); err != nil {
			return err
		}
		if err := q.FinishOpenPlanSteps(ctx, store.FinishOpenPlanStepsParams{OrgID: exec.OrgID, ExecutionID: exec.ExecutionID, Status: closing}); err != nil {
			return err
		}
		rows, err := q.ListPlanTasks(ctx, store.ListPlanTasksParams{OrgID: exec.OrgID, ExecutionID: exec.ExecutionID})
		if err != nil {
			return err
		}
		stepRows, err := q.ListPlanSteps(ctx, store.ListPlanStepsParams{OrgID: exec.OrgID, ExecutionID: exec.ExecutionID})
		if err != nil {
			return err
		}
		stepStatus := make(map[string]string, len(stepRows))
		for _, r := range stepRows {
			stepStatus[r.PlanKey] = r.Status
		}
		byID := make(map[uuid.UUID]store.ListPlanTasksRow, len(rows))
		for _, r := range rows {
			byID[r.ID] = r
		}
		statuses := make([]string, len(plan.Tasks))
		counts := map[string]int{}
		type taskResult struct {
			Key     string     `json:"id"`
			Kind    string     `json:"kind,omitempty"`
			TaskID  *uuid.UUID `json:"task_id,omitempty"`
			Status  string     `json:"status"`
			Summary string     `json:"summary,omitempty"`
		}
		results := make([]taskResult, len(plan.Tasks))
		for i, t := range plan.Tasks {
			if orchestrator.IsStep(t.Kind) {
				st, ok := stepStatus[t.Key]
				if !ok {
					return errTaskMismatch
				}
				statuses[i] = st
				counts[st]++
				results[i] = taskResult{Key: t.Key, Kind: t.Kind, Status: st}
				continue
			}
			taskID := t.TaskID
			r, ok := byID[t.TaskID]
			if !ok {
				return errTaskMismatch
			}
			statuses[i] = r.Status
			counts[r.Status]++
			results[i] = taskResult{Key: t.Key, TaskID: &taskID, Status: r.Status, Summary: r.OutputSummary}
		}
		status, code := orchestrator.Aggregate(nodes, statuses, stop)

		// Kết quả của kế hoạch là kết quả của các task không còn task nào phụ thuộc vào.
		var parts []string
		sinks := orchestrator.Sinks(nodes)
		for _, i := range sinks {
			var out struct {
				Message string `json:"message"`
			}
			// Bước có thử lại: kết quả nằm ở lần thử đã thành công, không phải ở nút cuối.
			st, at, ok := plan.effective(func(t storedTask) (string, bool) {
				k := plan.index(t.Key)
				return statuses[k], k >= 0
			}, i)
			if !ok || orchestrator.IsStep(plan.Tasks[at].Kind) || st != StatusCompleted ||
				json.Unmarshal(byID[plan.Tasks[at].TaskID].Output, &out) != nil || out.Message == "" {
				continue
			}
			msg := truncate(out.Message, maxPlanMessageRunes)
			if len(sinks) > 1 {
				msg = "## " + plan.Tasks[i].Key + "\n\n" + msg
			}
			parts = append(parts, msg)
		}
		output := map[string]any{"tasks": results}
		if len(parts) > 0 {
			output["message"] = strings.Join(parts, "\n\n")
		}
		n, err := q.FinishExecution(ctx, store.FinishExecutionParams{OrgID: exec.OrgID, ID: exec.ExecutionID,
			Status: status, Output: mustJSON(output), Error: outcomeError(status, code)})
		if err != nil || n == 0 {
			return err
		}
		outcome := audit.OutcomeSuccess
		if status != StatusCompleted {
			outcome = audit.OutcomeError
		}
		summary := make([]string, 0, len(counts))
		for _, st := range []string{StatusCompleted, StatusFailed, StatusCancelled, StatusTimedOut, StatusBudgetExceeded, orchestrator.Skipped} {
			if counts[st] > 0 {
				summary = append(summary, st+"="+strconv.Itoa(counts[st]))
			}
		}
		return audit.Record(ctx, tx, audit.Event{
			OrgID: exec.OrgID, ProjectID: &exec.ProjectID, Action: "execution.finish", ResourceType: "execution",
			ResourceID: &exec.ExecutionID, ExecutionID: &exec.ExecutionID, Outcome: outcome,
			Details: map[string]string{"status": status, "error_code": code, "mode": ModePlanned, "tasks": strings.Join(summary, ",")}})
	})
}

// errPlanHalted là lý do context của các task bị huỷ khi kế hoạch dừng (một task
// fail_execution thất bại): task đang chạy kết thúc là cancelled, không phải interrupted.
var errPlanHalted = errors.New("runtime: kế hoạch đã dừng")

// StartPlan hiện thực Runner: chạy kế hoạch ngay trong process này.
func (r *LocalRunner) StartPlan(_ context.Context, plan PlanSpec) error {
	r.wg.Add(1)
	go r.runPlan(plan)
	return nil
}

// runPlan là chủ sở hữu của một execution chế độ planned chạy nền: nó dừng khi kế
// hoạch xong, hết hạn, bị huỷ, hoặc khi process dừng, và luôn đóng execution.
func (r *LocalRunner) runPlan(plan PlanSpec) {
	defer r.wg.Done()
	exec := plan.Exec
	defer r.Abort(exec.OrgID)
	ctx, cancel := context.WithDeadline(r.base, exec.Deadline)
	defer cancel()
	hctx, halt := context.WithCancelCause(ctx)
	defer halt(errPlanHalted)

	en := &localEngine{r: r, ctx: ctx, taskCtx: hctx, halt: halt, plan: plan, done: make(chan localResult, len(plan.Tasks))}
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				slog.ErrorContext(ctx, "runtime: panic khi chạy kế hoạch", "panic", rec, "execution_id", exec.ExecutionID,
					"stack", string(debug.Stack()))
			}
		}()
		if err := r.steps.BeginPlan(ctx, exec); err != nil {
			slog.ErrorContext(ctx, "runtime: không bắt đầu được execution", "error", err, "execution_id", exec.ExecutionID)
			return
		}
		orchestrator.Run(en, plan.Nodes(), plan.MaxParallel)
	}()
	// Không task nào còn chạy sau khi execution đã đóng.
	halt(errPlanHalted)
	en.wg.Wait()
	hint := PlanStop{Deadline: errors.Is(ctx.Err(), context.DeadlineExceeded)}
	if err := r.steps.FinishPlan(r.base, exec, hint); err != nil {
		slog.ErrorContext(ctx, "runtime: không đóng được execution", "error", err, "execution_id", exec.ExecutionID)
	}
}

type localResult struct {
	i      int
	status string
}

// localEngine chạy các task của kế hoạch, mỗi task một goroutine thuộc về runPlan.
type localEngine struct {
	r *LocalRunner
	// ctx là context của execution; taskCtx là context các task chạy dưới, bị huỷ khi
	// kế hoạch dừng.
	ctx     context.Context //nolint:containedctx // vòng đời của một execution chạy trong process
	taskCtx context.Context //nolint:containedctx // như trên
	halt    context.CancelCauseFunc
	plan    PlanSpec
	done    chan localResult
	wg      sync.WaitGroup
}

func (e *localEngine) Start(i int) {
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		switch pt := e.plan.Tasks[i]; pt.Kind {
		case orchestrator.KindWait:
			e.done <- localResult{i: i, status: e.runWait(pt)}
			return
		case orchestrator.KindApproval:
			e.done <- localResult{i: i, status: e.runApproval(pt)}
			return
		}
		e.done <- localResult{i: i, status: e.runTask(e.plan.Tasks[i])}
	}()
}

// waitCancelPoll là nhịp nút chờ chạy trong process đọc yêu cầu huỷ từ database.
const waitCancelPoll = 2 * time.Second

// runWait chạy một nút chờ: ngủ đủ thời gian, hoặc dừng khi kế hoạch dừng / execution hết hạn.
func (e *localEngine) runWait(pt PlanTask) string {
	exec := e.plan.Exec
	status := StatusCompleted
	if err := e.r.steps.BeginPlanStep(e.taskCtx, exec, pt.Key); err != nil {
		slog.WarnContext(e.ctx, "runtime: không bắt đầu được nút chờ", "error", err, "plan_key", pt.Key)
		status = StatusCancelled
	} else {
		t := time.NewTimer(time.Duration(pt.WaitSeconds) * time.Second)
		defer t.Stop()
		// Không vòng lặp Agent nào đọc yêu cầu huỷ thay cho nút chờ: nó tự đọc theo nhịp.
		poll := time.NewTicker(waitCancelPoll)
		defer poll.Stop()
	wait:
		for {
			select {
			case <-t.C:
				break wait
			case <-poll.C:
				if cancelled, err := e.r.steps.Cancelled(e.taskCtx, TaskRef{OrgID: exec.OrgID, ProjectID: exec.ProjectID, ExecutionID: exec.ExecutionID}); err == nil && cancelled {
					status = StatusCancelled
					break wait
				}
			case <-e.taskCtx.Done():
				status = StatusCancelled
				if errors.Is(e.ctx.Err(), context.DeadlineExceeded) {
					status = StatusTimedOut
				}
				break wait
			}
		}
	}
	if err := e.r.steps.FinishPlanStep(e.r.base, exec, pt.Key, status); err != nil {
		// Trạng thái của nút là thứ database đã ghi: không ghi được "completed" thì nút
		// không hoàn thành, và các bước phụ thuộc không được coi là đã mở.
		slog.ErrorContext(e.ctx, "runtime: không đóng được nút chờ", "error", err, "plan_key", pt.Key)
		return StatusFailed
	}
	return status
}

// runApproval chạy một nút approval: tạo approval của nút rồi chờ nó có quyết định, hết
// hạn, hoặc kế hoạch dừng. Kết quả của nút do ClosePlanApproval đọc từ database.
func (e *localEngine) runApproval(pt PlanTask) string {
	exec := e.plan.Exec
	stop := ""
	if err := e.r.steps.BeginPlanStep(e.taskCtx, exec, pt.Key); err != nil {
		slog.WarnContext(e.ctx, "runtime: không bắt đầu được nút approval", "error", err, "plan_key", pt.Key)
		stop = StatusCancelled
	} else {
		poll := time.NewTicker(time.Second)
		defer poll.Stop()
	wait:
		for {
			select {
			case <-poll.C:
				// Approval hết hạn không còn pending: vòng chờ luôn kết thúc trong hạn của nó.
				pending, err := e.r.steps.PlanApprovalPending(e.taskCtx, exec, pt.Key)
				if err == nil && !pending {
					break wait
				}
				if cancelled, err := e.r.steps.Cancelled(e.taskCtx, TaskRef{OrgID: exec.OrgID, ProjectID: exec.ProjectID, ExecutionID: exec.ExecutionID}); err == nil && cancelled {
					stop = StatusCancelled
					break wait
				}
			case <-e.taskCtx.Done():
				stop = StatusCancelled
				if errors.Is(e.ctx.Err(), context.DeadlineExceeded) {
					stop = StatusTimedOut
				}
				break wait
			}
		}
	}
	status, err := e.r.steps.ClosePlanApproval(e.r.base, exec, pt.Key, stop)
	if err != nil {
		// Không đóng được (kể cả khi approval còn chờ): kế hoạch coi nút là thất bại;
		// FinishPlan đóng nút và huỷ approval của nó.
		slog.ErrorContext(e.ctx, "runtime: không đóng được nút approval", "error", err, "plan_key", pt.Key)
		return StatusFailed
	}
	return status
}

func (e *localEngine) runTask(pt PlanTask) string {
	spec := pt.Spec
	spec.Task.Deadline = pt.TaskDeadline(time.Now())
	task := spec.Task
	ctx, cancel := context.WithDeadline(e.taskCtx, task.Deadline)
	defer cancel()
	out := Outcome{Status: StatusFailed, ErrorCode: CodeInternal, ErrorMessage: "lỗi nội bộ khi chạy task"}
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				slog.ErrorContext(ctx, "runtime: panic trong task", "panic", rec, "task_id", task.TaskID, "stack", string(debug.Stack()))
			}
		}()
		if err := e.r.steps.BeginTask(ctx, task); err != nil {
			slog.WarnContext(ctx, "runtime: không bắt đầu được task", "error", err, "task_id", task.TaskID)
			if ctx.Err() != nil {
				out = Outcome{Status: StatusCancelled}
			}
			return
		}
		out = Run(localExecutor{ctx: ctx, steps: e.r.steps}, spec)
	}()
	// Task luôn được đóng ở đây, kể cả khi nó không bắt đầu được: không để nó "queued"
	// trong lúc các task phụ thuộc chạy. (Task đã bị bỏ qua hay đã đóng thì không đổi.)
	if err := e.r.steps.FinishTask(e.r.base, task, out); err != nil {
		slog.ErrorContext(ctx, "runtime: không đóng được task", "error", err, "task_id", task.TaskID)
	}
	return out.Status
}

func (e *localEngine) Wait() (int, string) {
	res := <-e.done
	return res.i, res.status
}

func (e *localEngine) Skip(i int) {
	if pt := e.plan.Tasks[i]; orchestrator.IsStep(pt.Kind) {
		if err := e.r.steps.SkipPlanStep(e.r.base, e.plan.Exec, pt.Key); err != nil {
			slog.ErrorContext(e.ctx, "runtime: không ghi được nút chờ bị bỏ qua", "error", err, "plan_key", pt.Key)
		}
		return
	}
	if err := e.r.steps.SkipTask(e.r.base, e.plan.Tasks[i].Spec.Task); err != nil {
		slog.ErrorContext(e.ctx, "runtime: không ghi được task bị bỏ qua", "error", err, "task_id", e.plan.Tasks[i].Spec.Task.TaskID)
	}
}

func (e *localEngine) Halt() { e.halt(errPlanHalted) }

func (e *localEngine) Stopped() bool {
	if e.ctx.Err() != nil {
		return true
	}
	cancelled, err := e.r.steps.Cancelled(e.ctx, e.plan.Exec)
	// Một lần đọc lỗi không được làm hỏng cả kế hoạch: từng task tự đọc yêu cầu huỷ
	// trước mỗi bước của nó, và BeginTask từ chối task của execution đã bị huỷ.
	return err == nil && cancelled
}
