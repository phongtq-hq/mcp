// Package durable chạy vòng lặp của một execution trên Temporal (ADR-0002): vòng lặp
// là một workflow, mỗi bước có I/O là một activity. Execution tiếp tục đúng chỗ sau
// khi process worker khởi động lại.
//
// Workflow dùng đúng hàm runtime.Run mà cách chạy trong process dùng; ở đây chỉ có
// phần nối các bước với Temporal. Lịch sử workflow chỉ chứa tham chiếu (số thứ tự
// step, tên tool, kích thước): prompt, arguments và kết quả tool nằm trong
// execution_steps và được activity tự đọc.
package durable

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	tlog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/hqgroups/mcp/internal/agent/orchestrator"
	"github.com/hqgroups/mcp/internal/agent/runtime"
	apperr "github.com/hqgroups/mcp/internal/shared/errors"
)

const (
	// TaskQueue là hàng đợi của shared runtime. Isolated runtime dùng hàng đợi riêng,
	// cùng code (agent-runtime §7).
	TaskQueue = "agent-shared"
	// WorkflowName là tên ổn định của workflow: đổi tên hàm Go không đổi tên này.
	WorkflowName = "AgentExecution"
	// PlanWorkflowName là workflow cha của một execution chế độ planned; mỗi task của nó
	// là một child workflow TaskWorkflowName (agent-runtime §5.3).
	PlanWorkflowName = "PlannedExecution"
	// PlanRetryWorkflowName là CÙNG workflow cha, dưới một tên khác, cho kế hoạch có lần
	// thử lại (retry_of). Worker phát hành trước khi có luật thử lại không đăng ký tên này:
	// gặp kế hoạch như vậy nó không chạy được (workflow task chờ worker mới) thay vì chạy
	// sai, tức là chạy lại vô điều kiện một bước đã thành công.
	PlanRetryWorkflowName = "PlannedExecutionRetry"
	TaskWorkflowName      = "AgentTask"

	// SignalApproval là signal api gửi khi một approval của execution được quyết định.
	SignalApproval = "approval-resolved"
	// SignalResume là signal api gửi khi yêu cầu tạm dừng của execution được gỡ.
	SignalResume = "execution-resumed"
	// changeControl là mốc versioning của workflow (workflow.GetVersion): từ version 1,
	// vòng lặp đọc yêu cầu huỷ VÀ yêu cầu tạm dừng bằng một local activity (Control) thay
	// cho Cancelled. Execution bắt đầu trước mốc này phát lại theo đường cũ và không tạm
	// dừng được. Đổi nghĩa của một version đã phát hành là phá execution đang chạy: thêm
	// version mới thay vì sửa.
	changeControl = "control"
	// approvalRecheck: khoảng tối đa giữa hai lần workflow tự đọc lại trạng thái approval.
	approvalRecheck = 2 * time.Minute

	modelAttempts = 3
	toolAttempts  = 3
	// Lề sau deadline của task để workflow còn kịp ghi trạng thái kết thúc.
	finishGrace = 3 * time.Minute

	errNonRetryable = "NonRetryable"
	errRetryable    = "Retryable"

	maxActivities = 32
	// Trần vòng lặp Agent đồng thời của một org trên shared runtime: execution đơn Agent
	// chiếm một suất, execution planned chiếm số task nó chạy song song.
	maxPerOrg = 8
)

// graceAt là lề sau hạn của task cho workflow ở một độ sâu trong cây giao việc (task gốc
// và task của kế hoạch ở độ sâu 0; workflow cha của kế hoạch ở -1). Workflow ở trên có
// lề dài hơn workflow nó chờ một finishGrace, để task con kịp ghi trạng thái kết thúc
// trước khi workflow của task cha bị Temporal kết thúc.
func graceAt(depth int) time.Duration {
	levels := orchestrator.MaxDelegationDepth + 1 - depth
	if levels < 1 {
		levels = 1
	}
	return time.Duration(levels) * finishGrace
}

// WorkflowID là định danh workflow của một execution. Có org_id để phân biệt tenant
// ngay ở Temporal.
func WorkflowID(orgID, executionID uuid.UUID) string {
	return "execution/" + orgID.String() + "/" + executionID.String()
}

// TaskWorkflowID là định danh child workflow của một task trong execution chế độ planned.
func TaskWorkflowID(orgID, executionID, taskID uuid.UUID) string {
	return WorkflowID(orgID, executionID) + "/task/" + taskID.String()
}

// Activities là các bước có I/O của vòng lặp, chạy trong process worker.
type Activities struct {
	Steps *runtime.Steps
}

// classify chuyển lỗi của một bước thành lỗi activity. Lịch sử workflow chỉ được chứa
// tham chiếu (ADR-0002), nên thông báo lỗi gốc (có thể mang địa chỉ nội bộ, chi tiết
// của database hay provider) chỉ vào log; Temporal nhận một mã cố định và việc có nên
// thử lại hay không.
func classify(ctx context.Context, step string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err // thông báo cố định của thư viện chuẩn; SDK dựa vào chúng để nhận biết huỷ
	}
	slog.WarnContext(ctx, "durable: bước lỗi", "step", step, "error", err)
	if runtime.Permanent(err) {
		return temporal.NewNonRetryableApplicationError("step_rejected", errNonRetryable, nil)
	}
	if e, ok := apperr.From(err); ok && !apperr.IsRetryable(e) && e.Kind != apperr.KindInternal {
		return temporal.NewNonRetryableApplicationError(e.Code, errNonRetryable, nil)
	}
	return temporal.NewApplicationError("step_failed", errRetryable)
}

// heartbeat báo cho Temporal biết activity còn sống trong lúc chờ một lời gọi dài
// (model, tool). Nhờ nó, worker chết thì activity được giao lại sau HeartbeatTimeout,
// và yêu cầu huỷ tới được activity đang chạy.
func heartbeat(ctx context.Context) (stop func()) {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(3 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				activity.RecordHeartbeat(ctx)
			}
		}
	}()
	return func() { close(done) }
}

// Begin chuyển execution sang running.
func (a *Activities) Begin(ctx context.Context, task runtime.TaskRef) error {
	return classify(ctx, "begin", a.Steps.Begin(ctx, task))
}

// ListTools trả về tên các tool của task.
func (a *Activities) ListTools(ctx context.Context, task runtime.TaskRef) ([]string, error) {
	names, err := a.Steps.ListTools(ctx, task)
	return names, classify(ctx, "list_tools", err)
}

// Cancelled đọc yêu cầu huỷ từ database.
func (a *Activities) Cancelled(ctx context.Context, task runtime.TaskRef) (bool, error) {
	cancelled, err := a.Steps.Cancelled(ctx, task)
	return cancelled, classify(ctx, "cancelled", err)
}

// CallModel thực hiện một lượt của model.
func (a *Activities) CallModel(ctx context.Context, in runtime.CallModelInput) (runtime.Turn, error) {
	defer heartbeat(ctx)()
	last := activity.GetInfo(ctx).Attempt >= modelAttempts
	turn, err := a.Steps.CallModel(ctx, in, runtime.ModelRetry{LastAttempt: last})
	return turn, classify(ctx, "call_model", err)
}

// AuthorizeToolCall quyết định và ghi lại việc từ chối một tool call.
func (a *Activities) AuthorizeToolCall(ctx context.Context, in runtime.AuthorizeInput) (runtime.Authorization, error) {
	auth, err := a.Steps.AuthorizeToolCall(ctx, in)
	return auth, classify(ctx, "authorize_tool_call", err)
}

// CallTool chạy một tool call qua MCP Gateway.
func (a *Activities) CallTool(ctx context.Context, in runtime.CallToolInput) (runtime.ToolOutcome, error) {
	defer heartbeat(ctx)()
	// Lần thử cuối của activity phải đóng step dứt khoát (kể cả là "không xác định"):
	// vòng lặp không được đi tiếp với một tool call còn mở.
	out, err := a.Steps.CallTool(ctx, in, runtime.ToolRetry{LastAttempt: activity.GetInfo(ctx).Attempt >= toolAttempts})
	return out, classify(ctx, "call_tool", err)
}

// Control đọc yêu cầu huỷ và yêu cầu tạm dừng của execution từ database.
func (a *Activities) Control(ctx context.Context, task runtime.TaskRef) (runtime.Control, error) {
	c, err := a.Steps.Control(ctx, task)
	return c, classify(ctx, "control", err)
}

// BeginPause chuyển task sang paused.
func (a *Activities) BeginPause(ctx context.Context, task runtime.TaskRef) error {
	return classify(ctx, "begin_pause", a.Steps.BeginPause(ctx, task))
}

// EndPause đưa task trở lại running.
func (a *Activities) EndPause(ctx context.Context, task runtime.TaskRef) error {
	return classify(ctx, "end_pause", a.Steps.EndPause(ctx, task))
}

// BeginWait chuyển task sang waiting_approval.
func (a *Activities) BeginWait(ctx context.Context, task runtime.TaskRef) error {
	return classify(ctx, "begin_wait", a.Steps.BeginWait(ctx, task))
}

// ApprovalPending đọc approval mà task đang chờ còn chờ quyết định hay không.
func (a *Activities) ApprovalPending(ctx context.Context, in runtime.AwaitInput) (bool, error) {
	pending, err := a.Steps.ApprovalPending(ctx, in)
	return pending, classify(ctx, "approval_pending", err)
}

// EndWait đưa task trở lại running.
func (a *Activities) EndWait(ctx context.Context, task runtime.TaskRef) error {
	return classify(ctx, "end_wait", a.Steps.EndWait(ctx, task))
}

// Finish đóng execution ở trạng thái kết thúc.
func (a *Activities) Finish(ctx context.Context, task runtime.TaskRef, out runtime.Outcome) error {
	return classify(ctx, "finish", a.Steps.Finish(ctx, task, out))
}

// BeginPlan chuyển execution chế độ planned sang running.
func (a *Activities) BeginPlan(ctx context.Context, exec runtime.TaskRef) error {
	return classify(ctx, "begin_plan", a.Steps.BeginPlan(ctx, exec))
}

// BeginTask chuyển một task của execution nhiều task sang running.
func (a *Activities) BeginTask(ctx context.Context, task runtime.TaskRef) error {
	return classify(ctx, "begin_task", a.Steps.BeginTask(ctx, task))
}

// FinishTask đóng một task của execution nhiều task.
func (a *Activities) FinishTask(ctx context.Context, task runtime.TaskRef, out runtime.Outcome) error {
	return classify(ctx, "finish_task", a.Steps.FinishTask(ctx, task, out))
}

// SkipTask ghi nhận một task không được chạy.
func (a *Activities) SkipTask(ctx context.Context, task runtime.TaskRef) error {
	return classify(ctx, "skip_task", a.Steps.SkipTask(ctx, task))
}

// BeginPlanStep chuyển một nút chờ của kế hoạch sang running.
func (a *Activities) BeginPlanStep(ctx context.Context, exec runtime.TaskRef, key string) error {
	return classify(ctx, "begin_plan_step", a.Steps.BeginPlanStep(ctx, exec, key))
}

// SkipPlanStep ghi nhận một nút chờ không được chạy.
func (a *Activities) SkipPlanStep(ctx context.Context, exec runtime.TaskRef, key string) error {
	return classify(ctx, "skip_plan_step", a.Steps.SkipPlanStep(ctx, exec, key))
}

// PlanApprovalPending đọc approval của một nút approval còn chờ quyết định hay không.
func (a *Activities) PlanApprovalPending(ctx context.Context, exec runtime.TaskRef, key string) (bool, error) {
	pending, err := a.Steps.PlanApprovalPending(ctx, exec, key)
	return pending, classify(ctx, "plan_approval_pending", err)
}

// ClosePlanApproval kết thúc một nút approval và trả về trạng thái của nút (đọc từ database).
func (a *Activities) ClosePlanApproval(ctx context.Context, exec runtime.TaskRef, key, stop string) (string, error) {
	status, err := a.Steps.ClosePlanApproval(ctx, exec, key, stop)
	return status, classify(ctx, "close_plan_approval", err)
}

// FinishPlanStep đóng một nút chờ của kế hoạch.
func (a *Activities) FinishPlanStep(ctx context.Context, exec runtime.TaskRef, key, status string) error {
	return classify(ctx, "finish_plan_step", a.Steps.FinishPlanStep(ctx, exec, key, status))
}

// FinishPlan đóng execution chế độ planned với kết quả tổng hợp.
func (a *Activities) FinishPlan(ctx context.Context, exec runtime.TaskRef, hint runtime.PlanStop) error {
	return classify(ctx, "finish_plan", a.Steps.FinishPlan(ctx, exec, hint))
}

// DelegateSpec dựng cấu hình chạy của một task con (delegation) từ database.
func (a *Activities) DelegateSpec(ctx context.Context, in runtime.AwaitTaskInput) (runtime.PlanTask, error) {
	pt, err := a.Steps.DelegateSpec(ctx, in)
	return pt, classify(ctx, "delegate_spec", err)
}

// Tham chiếu tới activity theo tên phương thức; không cần giá trị của receiver.
var acts *Activities

func shortActivity(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{InitialInterval: time.Second, BackoffCoefficient: 2, MaximumAttempts: 5,
			NonRetryableErrorTypes: []string{errNonRetryable}},
	})
}

// longActivity là tuỳ chọn cho bước có lời gọi ra ngoài (model, tool). budget là thời
// gian còn lại của task: nó chặn TOÀN BỘ bước, gồm thời gian chờ trong hàng đợi và mọi
// lần thử lại, để bước không kéo workflow qua hạn của task.
func longActivity(ctx workflow.Context, budget, perAttempt time.Duration, attempts int32) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		ScheduleToCloseTimeout: budget,
		StartToCloseTimeout:    min(budget, perAttempt),
		// Worker chết giữa một lời gọi dài: activity được giao lại sau khoảng này.
		HeartbeatTimeout: 15 * time.Second,
		// Khi workflow bị huỷ, chờ activity dừng hẳn rồi mới đi tiếp: trạng thái kết thúc
		// chỉ được ghi sau khi bước đang chạy đã ghi xong phần của nó (usage của lượt
		// model đã có kết quả).
		WaitForCancellation: true,
		RetryPolicy: &temporal.RetryPolicy{InitialInterval: time.Second, BackoffCoefficient: 2, MaximumInterval: 20 * time.Second,
			MaximumAttempts: attempts, NonRetryableErrorTypes: []string{errNonRetryable}},
	})
}

// finishActivity: trạng thái kết thúc phải được ghi. Thử lại tới khi hết lề sau hạn
// của task, không giới hạn số lần, để database chập chờn không biến task đã xong thành
// timed_out.
func finishActivity(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		ScheduleToCloseTimeout: finishGrace - 15*time.Second,
		StartToCloseTimeout:    20 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{InitialInterval: time.Second, BackoffCoefficient: 2, MaximumInterval: 10 * time.Second,
			NonRetryableErrorTypes: []string{errNonRetryable}},
	})
}

// executor nối runtime.Executor với các activity. Mọi phương thức chỉ dùng API của
// workflow SDK: không đọc đồng hồ thật, không I/O.
type executor struct {
	ctx      workflow.Context
	deadline time.Time
	// control: workflow này đọc yêu cầu huỷ và tạm dừng bằng activity Control (version ≥ 1
	// của changeControl). paused là yêu cầu tạm dừng theo lần đọc gần nhất.
	control bool
	paused  bool
}

// newExecutor tạo executor cho một workflow. GetVersion ghi mốc version vào lịch sử của
// execution mới; khi phát lại một lịch sử cũ (không có mốc) nó trả về version mặc định,
// và executor đi đúng chuỗi lệnh cũ.
func newExecutor(ctx workflow.Context, deadline time.Time) *executor {
	v := workflow.GetVersion(ctx, changeControl, workflow.DefaultVersion, 1)
	return &executor{ctx: ctx, deadline: deadline, control: v >= 1}
}

func controlOptions(ctx workflow.Context) workflow.Context {
	return workflow.WithLocalActivityOptions(ctx, workflow.LocalActivityOptions{
		StartToCloseTimeout: 10 * time.Second,
		// Vòng lặp gọi bước này trước mỗi lượt model và tool call: database chập chờn vài
		// giây không được làm hỏng task.
		RetryPolicy: &temporal.RetryPolicy{InitialInterval: 500 * time.Millisecond, BackoffCoefficient: 2, MaximumAttempts: 6},
	})
}

func (e *executor) remaining() time.Duration {
	d := e.deadline.Sub(workflow.Now(e.ctx))
	if d < time.Second {
		d = time.Second
	}
	return d
}

func (e *executor) Stopped() runtime.StopKind {
	if e.ctx.Err() != nil {
		return runtime.StopCancelled // workflow bị huỷ: yêu cầu huỷ của người dùng lan xuống
	}
	if !workflow.Now(e.ctx).Before(e.deadline) {
		return runtime.StopDeadline
	}
	return runtime.NotStopped
}

func (e *executor) ListTools(task runtime.TaskRef) (names []string, err error) {
	err = workflow.ExecuteActivity(longActivity(e.ctx, e.remaining(), time.Minute, 3), acts.ListTools, task).Get(e.ctx, &names)
	return names, err
}

func (e *executor) Cancelled(task runtime.TaskRef) (cancelled bool, err error) {
	if e.ctx.Err() != nil {
		return true, nil
	}
	if e.control {
		var c runtime.Control
		err = workflow.ExecuteLocalActivity(controlOptions(e.ctx), acts.Control, task).Get(e.ctx, &c)
		e.paused = err == nil && c.Paused && !c.Cancelled
		return c.Cancelled, err
	}
	// Local activity: một lần đọc database ngắn, không cần đi qua hàng đợi.
	lctx := workflow.WithLocalActivityOptions(e.ctx, workflow.LocalActivityOptions{
		StartToCloseTimeout: 10 * time.Second,
		// Vòng lặp gọi bước này trước mỗi lượt model và tool call: database chập chờn vài
		// giây không được làm hỏng task.
		RetryPolicy: &temporal.RetryPolicy{InitialInterval: 500 * time.Millisecond, BackoffCoefficient: 2, MaximumAttempts: 6},
	})
	err = workflow.ExecuteLocalActivity(lctx, acts.Cancelled, task).Get(e.ctx, &cancelled)
	return cancelled, err
}

func (e *executor) CallModel(in runtime.CallModelInput) (turn runtime.Turn, err error) {
	err = workflow.ExecuteActivity(longActivity(e.ctx, e.remaining(), 15*time.Minute, modelAttempts), acts.CallModel, in).Get(e.ctx, &turn)
	return turn, err
}

func (e *executor) AuthorizeToolCall(in runtime.AuthorizeInput) (auth runtime.Authorization, err error) {
	if in.Offered {
		// Quyết định cho phép ở phía runtime không có I/O; quyết định thật thuộc về
		// Gateway ở bước CallTool. Không cần một activity cho nó.
		return runtime.Authorization{Allowed: true, Rule: "agent_version_allows"}, nil
	}
	err = workflow.ExecuteActivity(shortActivity(e.ctx), acts.AuthorizeToolCall, in).Get(e.ctx, &auth)
	return auth, err
}

func (e *executor) CallTool(in runtime.CallToolInput) (out runtime.ToolOutcome, err error) {
	err = workflow.ExecuteActivity(longActivity(e.ctx, e.remaining(), 11*time.Minute, toolAttempts), acts.CallTool, in).Get(e.ctx, &out)
	return out, err
}

// Paused trả về yêu cầu tạm dừng theo lần đọc gần nhất của Cancelled. Không phát lệnh nào.
func (e *executor) Paused(runtime.TaskRef) bool { return e.control && e.paused && e.ctx.Err() == nil }

// AwaitResume giữ task ở trạng thái paused tới khi yêu cầu tạm dừng được gỡ. Như chờ
// approval: workflow ngủ trên một signal (api gửi khi resume) và một timer làm lưới an
// toàn; trạng thái thật luôn đọc từ database. Việc chờ bị chặn bởi hạn của task; huỷ
// workflow kết thúc nó ngay.
func (e *executor) AwaitResume(task runtime.TaskRef) error {
	if err := workflow.ExecuteActivity(shortActivity(e.ctx), acts.BeginPause, task).Get(e.ctx, nil); err != nil {
		if e.ctx.Err() != nil {
			return nil
		}
		return err
	}
	signals := workflow.GetSignalChannel(e.ctx, SignalResume)
	var werr error
	for e.Stopped() == runtime.NotStopped {
		// Bỏ hết signal tồn đọng trước khi đọc trạng thái: mỗi signal thừa không được làm
		// vòng chờ quay thêm một lần.
		for signals.ReceiveAsync(nil) {
		}
		var c runtime.Control
		// Không đọc được database trong lúc tạm dừng (kể cả sau mọi lần thử) thì coi như
		// vẫn tạm dừng và đọc lại ở chu kỳ sau: một execution tạm dừng lâu không được hỏng
		// vì database chập chờn. Vòng chờ vẫn bị chặn bởi hạn của task.
		if err := workflow.ExecuteLocalActivity(controlOptions(e.ctx), acts.Control, task).Get(e.ctx, &c); err == nil && (!c.Paused || c.Cancelled) {
			break
		}
		tctx, cancelTimer := workflow.WithCancel(e.ctx)
		sel := workflow.NewSelector(e.ctx)
		sel.AddFuture(workflow.NewTimer(tctx, min(approvalRecheck, e.remaining())), func(workflow.Future) {})
		sel.AddReceive(signals, func(c workflow.ReceiveChannel, _ bool) { c.Receive(e.ctx, nil) })
		sel.Select(e.ctx)
		cancelTimer()
	}
	e.paused = false
	if e.ctx.Err() != nil {
		werr = nil // bị huỷ trong lúc tạm dừng: vòng lặp tự kết thúc theo Stopped()
	}
	// Task trở lại running kể cả khi workflow đã bị huỷ, để nó được đóng đúng trạng thái.
	dctx, cancel := workflow.NewDisconnectedContext(e.ctx)
	defer cancel()
	if err := workflow.ExecuteActivity(shortActivity(dctx), acts.EndPause, task).Get(dctx, nil); err != nil && werr == nil {
		werr = err
	}
	return werr
}

// AwaitApproval chờ approval của một tool call có quyết định. Workflow ngủ trên một
// signal (api gửi khi người duyệt quyết định) và một timer: timer là lưới an toàn khi
// signal bị lỡ, và là cách thấy approval hết hạn. Trạng thái thật luôn đọc từ database.
// Việc chờ bị chặn bởi hạn của task; huỷ workflow kết thúc nó ngay.
func (e *executor) AwaitApproval(in runtime.AwaitInput) error {
	if err := workflow.ExecuteActivity(shortActivity(e.ctx), acts.BeginWait, in.Task).Get(e.ctx, nil); err != nil {
		if e.ctx.Err() != nil {
			return nil
		}
		return err
	}
	lctx := workflow.WithLocalActivityOptions(e.ctx, workflow.LocalActivityOptions{
		StartToCloseTimeout: 10 * time.Second,
		RetryPolicy:         &temporal.RetryPolicy{InitialInterval: 500 * time.Millisecond, BackoffCoefficient: 2, MaximumAttempts: 6},
	})
	signals := workflow.GetSignalChannel(e.ctx, SignalApproval)
	var werr error
	for e.Stopped() == runtime.NotStopped {
		pending := false
		if werr = workflow.ExecuteLocalActivity(lctx, acts.ApprovalPending, in).Get(e.ctx, &pending); werr != nil || !pending {
			break
		}
		tctx, cancelTimer := workflow.WithCancel(e.ctx)
		sel := workflow.NewSelector(e.ctx)
		sel.AddFuture(workflow.NewTimer(tctx, min(approvalRecheck, e.remaining())), func(workflow.Future) {})
		sel.AddReceive(signals, func(c workflow.ReceiveChannel, _ bool) { c.Receive(e.ctx, nil) })
		sel.Select(e.ctx)
		cancelTimer()
	}
	if e.ctx.Err() != nil {
		werr = nil // bị huỷ trong lúc chờ: vòng lặp tự kết thúc theo Stopped()
	}
	// Task trở lại running kể cả khi workflow đã bị huỷ, để Finish đóng nó đúng trạng thái.
	dctx, cancel := workflow.NewDisconnectedContext(e.ctx)
	defer cancel()
	if err := workflow.ExecuteActivity(shortActivity(dctx), acts.EndWait, in.Task).Get(dctx, nil); err != nil && werr == nil {
		werr = err
	}
	return werr
}

// AwaitTask chạy task con của một lần giao việc như một child workflow, tới khi nó kết
// thúc. Cấu hình của task con lấy từ database (activity DelegateSpec), không từ tham
// chiếu. Huỷ workflow này lan xuống task con. Kết quả của task con không đi qua lịch
// sử: bước CallTool được gọi lại và đọc nó từ database.
func (e *executor) AwaitTask(in runtime.AwaitTaskInput) error {
	var pt runtime.PlanTask
	if err := workflow.ExecuteActivity(shortActivity(e.ctx), acts.DelegateSpec, in).Get(e.ctx, &pt); err != nil {
		if e.ctx.Err() != nil {
			return nil
		}
		return err
	}
	// Task con không sống quá hạn của task cha.
	if e.deadline.Before(pt.Spec.Task.Deadline) {
		pt.Spec.Task.Deadline = e.deadline
	}
	task := pt.Spec.Task
	cctx := workflow.WithChildOptions(e.ctx, workflow.ChildWorkflowOptions{
		WorkflowID: TaskWorkflowID(task.OrgID, task.ExecutionID, task.TaskID),
		TaskQueue:  TaskQueue,
		// Lề giảm theo độ sâu: task con luôn kịp đóng trước khi workflow của task cha hết
		// lề của chính nó.
		WorkflowExecutionTimeout: e.remaining() + graceAt(pt.Depth),
		WaitForCancellation:      true,
		ParentClosePolicy:        enums.PARENT_CLOSE_POLICY_REQUEST_CANCEL,
		WorkflowIDReusePolicy:    enums.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
		TypedSearchAttributes: temporal.NewSearchAttributes(attrOrg.ValueSet(task.OrgID.String()),
			attrProject.ValueSet(task.ProjectID.String())),
	})
	var out runtime.Outcome
	if err := workflow.ExecuteChildWorkflow(cctx, TaskWorkflowName, pt).Get(e.ctx, &out); err != nil && e.ctx.Err() == nil {
		// Child workflow hỏng mà không đóng được task của nó: bước CallTool gọi lại sẽ thấy
		// task con chưa kết thúc và vòng lặp dừng task cha; không đoán kết quả.
		workflow.GetLogger(e.ctx).Warn("durable: child workflow của task con lỗi", "task_id", task.TaskID.String())
	}
	return nil
}

// Workflow là vòng lặp của một execution đơn Agent.
func Workflow(ctx workflow.Context, spec runtime.Spec) (runtime.Outcome, error) {
	out := runtime.Outcome{Status: runtime.StatusFailed, ErrorCode: runtime.CodeInternal, ErrorMessage: "lỗi nội bộ khi chạy task"}
	if err := workflow.ExecuteActivity(shortActivity(ctx), acts.Begin, spec.Task).Get(ctx, nil); err == nil {
		out = runtime.Run(newExecutor(ctx, spec.Task.Deadline), spec)
	} else if ctx.Err() != nil {
		out = runtime.Outcome{Status: runtime.StatusCancelled}
	}
	// Trạng thái kết thúc luôn được ghi, kể cả khi workflow đã bị huỷ.
	dctx, cancel := workflow.NewDisconnectedContext(ctx)
	defer cancel()
	if err := workflow.ExecuteActivity(finishActivity(dctx), acts.Finish, spec.Task, out).Get(dctx, nil); err != nil {
		return out, err
	}
	return out, nil
}

// TaskWorkflow là vòng lặp của MỘT task trong execution chế độ planned: cùng
// runtime.Run như execution đơn Agent, nhưng chỉ mở và đóng task của nó; execution do
// workflow cha đóng.
func TaskWorkflow(ctx workflow.Context, pt runtime.PlanTask) (runtime.Outcome, error) {
	spec := pt.Spec
	// Hạn của task tính từ lúc nó thực sự bắt đầu, và không quá hạn của execution.
	spec.Task.Deadline = pt.TaskDeadline(workflow.Now(ctx))
	out := runtime.Outcome{Status: runtime.StatusFailed, ErrorCode: runtime.CodeInternal, ErrorMessage: "lỗi nội bộ khi chạy task"}
	if err := workflow.ExecuteActivity(shortActivity(ctx), acts.BeginTask, spec.Task).Get(ctx, nil); err == nil {
		out = runtime.Run(newExecutor(ctx, spec.Task.Deadline), spec)
	} else if ctx.Err() != nil {
		out = runtime.Outcome{Status: runtime.StatusCancelled}
	}
	// Task luôn được đóng, kể cả khi nó không bắt đầu được: không để nó "queued" trong
	// lúc các task phụ thuộc chạy. (Task đã bị bỏ qua hay đã đóng thì bước này không đổi gì.)
	dctx, cancel := workflow.NewDisconnectedContext(ctx)
	defer cancel()
	if err := workflow.ExecuteActivity(finishActivity(dctx), acts.FinishTask, spec.Task, out).Get(dctx, nil); err != nil {
		return out, err
	}
	return out, nil
}

// planEngine nối orchestrator.Engine với child workflow. Chỉ dùng API của workflow SDK.
type planEngine struct {
	ctx workflow.Context
	// cctx là context các child workflow chạy dưới: huỷ nó (Halt) hoặc huỷ workflow cha
	// đều gửi yêu cầu huỷ tới mọi task đang chạy.
	cctx    workflow.Context
	cancel  workflow.CancelFunc
	plan    runtime.PlanSpec
	running []workflow.Future // theo chỉ số nút; nil = không chạy
	// wakers: kênh đánh thức của từng nút approval đang chờ (theo chỉ số nút). Một coroutine
	// duy nhất nhận signal "approval-resolved" rồi đánh thức MỌI nút đang chờ: signal chỉ
	// được một nơi nhận, nên để từng nút tự nhận thì nút khác lỡ quyết định của nó.
	wakers      []workflow.Channel
	dispatching bool
}

// dispatch khởi chạy (một lần) coroutine phân phát signal approval cho các nút đang chờ.
func (e *planEngine) dispatch() {
	if e.dispatching {
		return
	}
	e.dispatching = true
	workflow.Go(e.cctx, func(ctx workflow.Context) {
		signals := workflow.GetSignalChannel(ctx, SignalApproval)
		for ctx.Err() == nil {
			sel := workflow.NewSelector(ctx)
			sel.AddReceive(signals, func(c workflow.ReceiveChannel, _ bool) {
				c.Receive(ctx, nil)
				for _, w := range e.wakers {
					if w != nil {
						w.SendAsync(true) // kênh đệm 1: nút đã có một lần đánh thức chờ sẵn thì thôi
					}
				}
			})
			sel.AddReceive(ctx.Done(), func(workflow.ReceiveChannel, bool) {})
			sel.Select(ctx)
		}
	})
}

// startWait chạy một nút chờ: một timer của workflow giữa hai activity ghi trạng thái.
// Huỷ kế hoạch (Halt) hoặc huỷ workflow kết thúc timer ngay. Timer không ngủ quá hạn của
// execution: chạm hạn thì nút là timed_out và kế hoạch dừng như mọi task quá hạn.
func (e *planEngine) startWait(pt runtime.PlanTask) workflow.Future {
	fut, set := workflow.NewFuture(e.ctx)
	// Coroutine chạy dưới context của các task (bị huỷ khi kế hoạch dừng); mọi lời gọi
	// chặn trong coroutine phải dùng context của chính nó.
	workflow.Go(e.cctx, func(ctx workflow.Context) {
		status := runtime.StatusCompleted
		if err := workflow.ExecuteActivity(shortActivity(ctx), acts.BeginPlanStep, e.plan.Exec, pt.Key).Get(ctx, nil); err != nil {
			status = runtime.StatusCancelled
		} else {
			sleep, capped := time.Duration(pt.WaitSeconds)*time.Second, false
			if left := e.plan.Exec.Deadline.Sub(workflow.Now(ctx)); left < sleep {
				sleep, capped = max(left, 0), true
			}
			switch err := workflow.NewTimer(ctx, sleep).Get(ctx, nil); {
			case err != nil:
				status = runtime.StatusCancelled
				if !workflow.Now(ctx).Before(e.plan.Exec.Deadline) {
					status = runtime.StatusTimedOut
				}
			case capped:
				status = runtime.StatusTimedOut
			}
		}
		dctx, cancel := workflow.NewDisconnectedContext(ctx)
		defer cancel()
		// Trạng thái của nút là thứ database đã ghi: không ghi được "completed" (chưa chờ đủ
		// theo đồng hồ của database, hoặc database lỗi) thì nút không hoàn thành.
		if err := workflow.ExecuteActivity(shortActivity(dctx), acts.FinishPlanStep, e.plan.Exec, pt.Key, status).Get(dctx, nil); err != nil {
			status = runtime.StatusFailed
		}
		set.Set(runtime.Outcome{Status: status}, nil)
	})
	return fut
}

// startApproval chạy một nút approval: tạo approval của nút (BeginPlanStep), rồi ngủ
// tới khi được đánh thức (api gửi signal "approval-resolved" khi có quyết định; xem
// dispatch) hoặc tới một timer. Timer là lưới an toàn khi signal bị lỡ, và là cách thấy
// approval hết hạn. Trạng thái thật luôn đọc từ database; kết quả của nút do
// ClosePlanApproval (database) trả về, không do workflow quyết định. Đọc trạng thái lỗi
// thì chờ tiếp: một lần đọc lỗi không được làm cổng đang chờ người duyệt bị đóng.
func (e *planEngine) startApproval(i int, pt runtime.PlanTask) workflow.Future {
	fut, set := workflow.NewFuture(e.ctx)
	e.dispatch()
	workflow.Go(e.cctx, func(ctx workflow.Context) {
		stop := ""
		if err := workflow.ExecuteActivity(shortActivity(ctx), acts.BeginPlanStep, e.plan.Exec, pt.Key).Get(ctx, nil); err != nil {
			stop = runtime.StatusCancelled
		} else {
			lctx := workflow.WithLocalActivityOptions(ctx, workflow.LocalActivityOptions{
				StartToCloseTimeout: 10 * time.Second,
				RetryPolicy:         &temporal.RetryPolicy{InitialInterval: 500 * time.Millisecond, BackoffCoefficient: 2, MaximumAttempts: 6},
			})
			wake := workflow.NewBufferedChannel(ctx, 1)
			e.wakers[i] = wake
			for ctx.Err() == nil && workflow.Now(ctx).Before(e.plan.Exec.Deadline) {
				pending := false
				if err := workflow.ExecuteLocalActivity(lctx, acts.PlanApprovalPending, e.plan.Exec, pt.Key).Get(ctx, &pending); err == nil && !pending {
					break
				}
				tctx, cancelTimer := workflow.WithCancel(ctx)
				sel := workflow.NewSelector(ctx)
				sel.AddFuture(workflow.NewTimer(tctx, min(approvalRecheck, max(e.plan.Exec.Deadline.Sub(workflow.Now(ctx)), time.Second))), func(workflow.Future) {})
				sel.AddReceive(wake, func(c workflow.ReceiveChannel, _ bool) { c.Receive(ctx, nil) })
				sel.Select(ctx)
				cancelTimer()
			}
			e.wakers[i] = nil
			switch {
			case !workflow.Now(ctx).Before(e.plan.Exec.Deadline):
				stop = runtime.StatusTimedOut
			case ctx.Err() != nil:
				stop = runtime.StatusCancelled
			}
		}
		dctx, cancel := workflow.NewDisconnectedContext(ctx)
		defer cancel()
		status := runtime.StatusFailed
		if err := workflow.ExecuteActivity(shortActivity(dctx), acts.ClosePlanApproval, e.plan.Exec, pt.Key, stop).Get(dctx, &status); err != nil {
			status = runtime.StatusFailed
		}
		set.Set(runtime.Outcome{Status: status}, nil)
	})
	return fut
}

func (e *planEngine) Start(i int) {
	switch pt := e.plan.Tasks[i]; pt.Kind {
	case orchestrator.KindWait:
		e.running[i] = e.startWait(pt)
		return
	case orchestrator.KindApproval:
		e.running[i] = e.startApproval(i, pt)
		return
	}
	task := e.plan.Tasks[i].Spec.Task
	remaining := e.plan.Exec.Deadline.Sub(workflow.Now(e.ctx))
	if remaining < time.Second {
		remaining = time.Second
	}
	cctx := workflow.WithChildOptions(e.cctx, workflow.ChildWorkflowOptions{
		WorkflowID:               TaskWorkflowID(task.OrgID, task.ExecutionID, task.TaskID),
		TaskQueue:                TaskQueue,
		WorkflowExecutionTimeout: remaining + graceAt(0),
		// Chờ task dừng hẳn (đã ghi trạng thái kết thúc của nó) rồi mới coi là xong.
		WaitForCancellation:   true,
		ParentClosePolicy:     enums.PARENT_CLOSE_POLICY_REQUEST_CANCEL,
		WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
		TypedSearchAttributes: temporal.NewSearchAttributes(attrOrg.ValueSet(task.OrgID.String()),
			attrProject.ValueSet(task.ProjectID.String())),
	})
	e.running[i] = workflow.ExecuteChildWorkflow(cctx, TaskWorkflowName, e.plan.Tasks[i])
}

func (e *planEngine) Wait() (done int, status string) {
	done = -1
	sel := workflow.NewSelector(e.ctx)
	for i, f := range e.running {
		if f == nil {
			continue
		}
		sel.AddFuture(f, func(f workflow.Future) {
			var out runtime.Outcome
			switch err := f.Get(e.ctx, &out); {
			case err == nil:
				status = out.Status
			case temporal.IsCanceledError(err) || e.cctx.Err() != nil:
				status = runtime.StatusCancelled
			default:
				// Child workflow hỏng (hết hạn ở Temporal, không ghi được trạng thái): task
				// không hoàn thành; FinishPlan đóng nó trong database.
				status = runtime.StatusFailed
			}
			done = i
		})
	}
	sel.Select(e.ctx)
	if done >= 0 {
		e.running[done] = nil
	}
	return done, status
}

func (e *planEngine) Skip(i int) {
	// Ghi cả khi execution đã bị huỷ; lỗi ở đây không chặn kế hoạch (FinishPlan đóng
	// mọi task còn mở).
	dctx, cancel := workflow.NewDisconnectedContext(e.ctx)
	defer cancel()
	if pt := e.plan.Tasks[i]; orchestrator.IsStep(pt.Kind) {
		_ = workflow.ExecuteActivity(shortActivity(dctx), acts.SkipPlanStep, e.plan.Exec, pt.Key).Get(dctx, nil)
		return
	}
	_ = workflow.ExecuteActivity(shortActivity(dctx), acts.SkipTask, e.plan.Tasks[i].Spec.Task).Get(dctx, nil)
}

func (e *planEngine) Halt() { e.cancel() }

func (e *planEngine) Stopped() bool {
	return e.ctx.Err() != nil || !workflow.Now(e.ctx).Before(e.plan.Exec.Deadline)
}

// drain chờ mọi child workflow còn chạy kết thúc.
func (e *planEngine) drain() {
	for {
		busy := false
		for _, f := range e.running {
			busy = busy || f != nil
		}
		if !busy {
			return
		}
		if i, _ := e.Wait(); i < 0 {
			return
		}
	}
}

// PlanWorkflow là workflow cha của một execution chế độ planned: thân là
// orchestrator.Run, mỗi task là một child workflow. Lịch sử chỉ chứa tham chiếu (định
// danh, giới hạn, trạng thái); goal và kết quả của task nằm trong database.
func PlanWorkflow(ctx workflow.Context, plan runtime.PlanSpec) error {
	if err := workflow.ExecuteActivity(shortActivity(ctx), acts.BeginPlan, plan.Exec).Get(ctx, nil); err == nil {
		cctx, cancel := workflow.WithCancel(ctx)
		en := &planEngine{ctx: ctx, cctx: cctx, cancel: cancel, plan: plan, running: make([]workflow.Future, len(plan.Tasks)),
			wakers: make([]workflow.Channel, len(plan.Tasks))}
		orchestrator.Run(en, plan.Nodes(), plan.MaxParallel)
		// Không task nào còn chạy sau khi execution đã đóng.
		cancel()
		en.drain()
	}
	hint := runtime.PlanStop{Cancelled: ctx.Err() != nil, Deadline: !workflow.Now(ctx).Before(plan.Exec.Deadline)}
	// Trạng thái kết thúc luôn được ghi, kể cả khi workflow đã bị huỷ.
	dctx, cancel := workflow.NewDisconnectedContext(ctx)
	defer cancel()
	return workflow.ExecuteActivity(finishActivity(dctx), acts.FinishPlan, plan.Exec, hint).Get(dctx, nil)
}

// Search attribute của workflow: lọc và kiểm tra theo tenant ngay ở Temporal
// (security §5). Phải được đăng ký trong namespace trước khi chạy (dev server:
// --search-attribute OrgId=Keyword --search-attribute ProjectId=Keyword).
var (
	attrOrg     = temporal.NewSearchAttributeKeyKeyword("OrgId")
	attrProject = temporal.NewSearchAttributeKeyKeyword("ProjectId")
)

// DialOptions là cách nối tới Temporal. Việc tự vận hành hay dùng dịch vụ quản lý còn
// chưa chốt (ADR chưa có), nên phần xác thực nằm cả ở đây, không rải trong code.
type DialOptions struct {
	Address   string
	Namespace string
	// TLS khác nil thì kết nối được mã hoá (và xác thực hai chiều nếu có chứng chỉ client).
	TLS *tls.Config
	// APIKey, nếu có, được gửi ở mỗi request.
	APIKey string
	// Identity ghi vào lịch sử workflow; rỗng thì SDK dùng pid@hostname.
	Identity string
	Logger   *slog.Logger
}

// Dial nối tới Temporal. Lỗi nếu không tới được: process không lên thay vì nhận
// execution mà không chạy được.
func Dial(ctx context.Context, o DialOptions) (client.Client, error) {
	if o.Namespace == "" {
		o.Namespace = client.DefaultNamespace
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	opts := client.Options{HostPort: o.Address, Namespace: o.Namespace, Identity: o.Identity, Logger: tlog.NewStructuredLogger(o.Logger)}
	if o.TLS != nil {
		opts.ConnectionOptions.TLS = o.TLS
	}
	if o.APIKey != "" {
		opts.Credentials = client.NewAPIKeyStaticCredentials(o.APIKey)
	}
	c, err := client.DialContext(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("durable: không nối được Temporal: %w", err)
	}
	return c, nil
}

// DialFromEnv đọc cấu hình kết nối từ biến môi trường: HQ_TEMPORAL_ADDRESS,
// HQ_TEMPORAL_NAMESPACE, HQ_TEMPORAL_TLS=1, HQ_TEMPORAL_TLS_CERT_FILE / _KEY_FILE
// (mTLS), HQ_TEMPORAL_API_KEY. Ngoài local, kết nối không TLS bị từ chối: ai ghi được
// vào Temporal thì điều khiển được worker.
func DialFromEnv(ctx context.Context, getenv func(string) string, local bool) (client.Client, error) {
	o := DialOptions{Address: getenv("HQ_TEMPORAL_ADDRESS"), Namespace: getenv("HQ_TEMPORAL_NAMESPACE"), APIKey: getenv("HQ_TEMPORAL_API_KEY")}
	cert, key := getenv("HQ_TEMPORAL_TLS_CERT_FILE"), getenv("HQ_TEMPORAL_TLS_KEY_FILE")
	if getenv("HQ_TEMPORAL_TLS") == "1" || cert != "" {
		o.TLS = &tls.Config{MinVersion: tls.VersionTLS12}
		if cert != "" {
			pair, err := tls.LoadX509KeyPair(cert, key)
			if err != nil {
				return nil, fmt.Errorf("durable: chứng chỉ client của Temporal: %w", err)
			}
			o.TLS.Certificates = []tls.Certificate{pair}
		}
	}
	if o.TLS == nil && !local {
		return nil, errors.New("durable: kết nối Temporal không TLS chỉ được phép khi HQ_ENV=local (đặt HQ_TEMPORAL_TLS=1)")
	}
	return Dial(ctx, o)
}

// NewWorker tạo worker của shared runtime. tune chỉnh tuỳ chọn (dùng trong test).
func NewWorker(c client.Client, steps *runtime.Steps, tune ...func(*worker.Options)) worker.Worker {
	opts := worker.Options{
		// Trần của một process worker: mỗi vòng lặp Agent (execution đơn Agent, hoặc một
		// task của execution planned) có nhiều nhất một activity dài (model hoặc tool) tại
		// một thời điểm; trần theo org (maxPerOrg) đếm theo vòng lặp, không theo execution.
		MaxConcurrentActivityExecutionSize:     maxActivities,
		MaxConcurrentWorkflowTaskExecutionSize: 16,
		// Khi dừng, activity đang chạy có chừng này thời gian để ghi xong step; chưa
		// xong thì Temporal giao lại cho worker khác sau HeartbeatTimeout.
		WorkerStopTimeout: 20 * time.Second,
	}
	for _, fn := range tune {
		fn(&opts)
	}
	w := worker.New(c, TaskQueue, opts)
	w.RegisterWorkflowWithOptions(Workflow, workflow.RegisterOptions{Name: WorkflowName})
	w.RegisterWorkflowWithOptions(PlanWorkflow, workflow.RegisterOptions{Name: PlanWorkflowName})
	w.RegisterWorkflowWithOptions(PlanWorkflow, workflow.RegisterOptions{Name: PlanRetryWorkflowName})
	w.RegisterWorkflowWithOptions(TaskWorkflow, workflow.RegisterOptions{Name: TaskWorkflowName})
	w.RegisterActivity(&Activities{Steps: steps})
	return w
}

// Starter khởi chạy và huỷ workflow của execution từ process api.
type Starter struct {
	Client client.Client
}

// Admit hiện thực runtime.Runner. Process api không giữ chỗ trong bộ nhớ: trần theo
// org do Service đếm trong database (OrgLimit), trần của process do worker đặt.
func (s *Starter) Admit(uuid.UUID) error { return nil }

// Abort hiện thực runtime.Runner.
func (s *Starter) Abort(uuid.UUID) {}

// OrgLimit hiện thực runtime.OrgLimiter.
func (s *Starter) OrgLimit() int { return maxPerOrg }

// Start hiện thực runtime.Runner.
func (s *Starter) Start(ctx context.Context, spec runtime.Spec) error {
	timeout := time.Until(spec.Task.Deadline) + graceAt(0)
	_, err := s.Client.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        WorkflowID(spec.Task.OrgID, spec.Task.ExecutionID),
		TaskQueue: TaskQueue,
		// Hết hạn thì Temporal kết thúc workflow; execution khi đó được đóng timed_out
		// ở lần đọc kế tiếp. Bình thường workflow tự đóng trước đó.
		WorkflowExecutionTimeout: timeout,
		// Một execution chỉ có một workflow, kể cả khi lời gọi này bị lặp lại.
		WorkflowIDReusePolicy:                    enums.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
		WorkflowExecutionErrorWhenAlreadyStarted: false,
		TypedSearchAttributes: temporal.NewSearchAttributes(attrOrg.ValueSet(spec.Task.OrgID.String()),
			attrProject.ValueSet(spec.Task.ProjectID.String())),
	}, WorkflowName, spec)
	if err != nil {
		return fmt.Errorf("durable: không khởi chạy được workflow: %w", err)
	}
	return nil
}

// StartPlan hiện thực runtime.Runner: workflow cha của một execution chế độ planned.
func (s *Starter) StartPlan(ctx context.Context, plan runtime.PlanSpec) error {
	exec := plan.Exec
	name := PlanWorkflowName
	for _, t := range plan.Tasks {
		if t.RetryOf != nil {
			name = PlanRetryWorkflowName
			break
		}
	}
	_, err := s.Client.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        WorkflowID(exec.OrgID, exec.ExecutionID),
		TaskQueue: TaskQueue,
		// Thêm một lề so với child workflow: workflow cha đóng execution sau khi task
		// cuối đã ghi xong trạng thái của nó.
		WorkflowExecutionTimeout:                 time.Until(exec.Deadline) + graceAt(-1),
		WorkflowIDReusePolicy:                    enums.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
		WorkflowExecutionErrorWhenAlreadyStarted: false,
		TypedSearchAttributes: temporal.NewSearchAttributes(attrOrg.ValueSet(exec.OrgID.String()),
			attrProject.ValueSet(exec.ProjectID.String())),
	}, name, plan)
	if err != nil {
		return fmt.Errorf("durable: không khởi chạy được workflow: %w", err)
	}
	return nil
}

// ApprovalResolved hiện thực runtime.Runner: đánh thức workflow đang chờ approval. Với
// execution đơn Agent đó là workflow của execution; với execution chế độ planned đó là
// child workflow của task. Lỡ signal không sao: workflow tự đọc lại trạng thái theo chu kỳ.
func (s *Starter) ApprovalResolved(ctx context.Context, orgID, executionID, taskID uuid.UUID) {
	targets := []string{WorkflowID(orgID, executionID)}
	if taskID != uuid.Nil {
		targets = append(targets, TaskWorkflowID(orgID, executionID, taskID))
	}
	for _, id := range targets {
		err := s.Client.SignalWorkflow(ctx, id, "", SignalApproval, nil)
		var notFound *serviceerror.NotFound
		if err != nil && !errors.As(err, &notFound) {
			slog.WarnContext(ctx, "durable: không gửi được signal approval tới workflow", "error", err, "execution_id", executionID)
		}
	}
}

// Resumed hiện thực runtime.Runner: đánh thức các workflow đang tạm dừng của execution
// (workflow của execution và child workflow của từng task còn mở). Lỡ signal không sao:
// workflow tự đọc lại yêu cầu theo chu kỳ.
func (s *Starter) Resumed(ctx context.Context, orgID, executionID uuid.UUID, tasks []uuid.UUID) {
	targets := []string{WorkflowID(orgID, executionID)}
	for _, id := range tasks {
		targets = append(targets, TaskWorkflowID(orgID, executionID, id))
	}
	for _, id := range targets {
		err := s.Client.SignalWorkflow(ctx, id, "", SignalResume, nil)
		var notFound *serviceerror.NotFound
		if err != nil && !errors.As(err, &notFound) {
			slog.WarnContext(ctx, "durable: không gửi được signal resume tới workflow", "error", err, "execution_id", executionID)
		}
	}
}

// Cancel hiện thực runtime.Runner: huỷ workflow để các activity đang chạy dừng sớm.
// Yêu cầu huỷ đã nằm trong database; workflow cũng đọc nó trước mỗi bước, nên lỗi ở
// đây không chặn việc huỷ.
func (s *Starter) Cancel(ctx context.Context, task runtime.TaskRef) {
	err := s.Client.CancelWorkflow(ctx, WorkflowID(task.OrgID, task.ExecutionID), "")
	var notFound *serviceerror.NotFound
	if err != nil && !errors.As(err, &notFound) {
		slog.WarnContext(ctx, "durable: không gửi được yêu cầu huỷ tới workflow", "error", err, "execution_id", task.ExecutionID)
	}
}
