// Package schedule nối lịch chạy của workflow do người dùng định nghĩa với Temporal
// Schedules (ADR-0015 mục 4). Nó là nơi duy nhất của context Workflow import Temporal SDK.
//
// Một workflow active có biểu thức cron có đúng một Schedule. Khi lịch kích hoạt,
// Temporal khởi chạy workflow Run trên task queue TaskQueue, hàng đợi chỉ process api
// poll; Run gọi đúng một activity, và activity đó (chạy trong api) tạo lần chạy qua
// definitions.Service.StartScheduled. Lịch sử của Run chỉ chứa định danh của org và
// workflow; api đọc lại mọi thứ khác từ database.
package schedule

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/robfig/cron"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/hqgroups/mcp/internal/workflow/definitions"
)

const (
	// TaskQueue là hàng đợi chỉ process api poll: việc tạo lần chạy cần quyền của api.
	TaskQueue = "api-internal"
	// WorkflowName là tên workflow được lịch khởi chạy.
	WorkflowName = "ScheduledWorkflowRun"
	// ActivityName là tên activity tạo lần chạy.
	ActivityName = "StartScheduledRun"
	// MinInterval là khoảng cách nhỏ nhất giữa hai lần kích hoạt của một lịch.
	MinInterval = 5 * time.Minute

	maxCronLen = 100
	// runTimeout chặn trên toàn bộ một lần kích hoạt (kể cả các lần thử lại của activity).
	runTimeout = 15 * time.Minute
	// maxAttempts là số lần activity tạo lần chạy được thử.
	maxAttempts = 5
	// stopTimeout là thời gian worker chờ activity đang chạy khi process dừng.
	stopTimeout = 30 * time.Second
)

// runID là workflow ID của các lần kích hoạt của một lịch; Temporal nối thêm
// "-<thời điểm kích hoạt>" cho từng lần.
func runID(orgID, workflowID uuid.UUID) string {
	return "workflow-run/" + orgID.String() + "/" + workflowID.String()
}

// ID trả về định danh Schedule của một workflow. org nằm trong định danh như mọi
// workflow ID khác của platform.
func ID(orgID, workflowID uuid.UUID) string {
	return "workflow-schedule/" + orgID.String() + "/" + workflowID.String()
}

// Ref là tham chiếu đi qua Temporal: chỉ định danh.
type Ref struct {
	OrgID      uuid.UUID `json:"org_id"`
	WorkflowID uuid.UUID `json:"workflow_id"`
}

// Run là workflow được lịch khởi chạy: đúng một activity. Khoá của lần kích hoạt là
// workflow ID của chính nó (Temporal gắn thời điểm kích hoạt vào ID), nên activity được
// thử lại, hay Run được chạy lại, không tạo lần chạy thứ hai.
func Run(ctx workflow.Context, ref Ref) error {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 2 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{InitialInterval: 5 * time.Second, BackoffCoefficient: 2, MaximumInterval: time.Minute,
			MaximumAttempts: maxAttempts},
	})
	return workflow.ExecuteActivity(ctx, ActivityName, ref).Get(ctx, nil)
}

// Runner là thứ activity cần từ context Workflow. final: đây là lần thử cuối của lần
// kích hoạt này, lỗi tạm thời không còn được thử lại.
type Runner interface {
	StartScheduled(ctx context.Context, orgID, workflowID uuid.UUID, key string, final bool) error
}

type activities struct{ runner Runner }

// StartScheduledRun tạo lần chạy cho một lần kích hoạt. Khoá của lần kích hoạt lấy từ
// workflow ID mà Temporal báo cho activity, không từ tham số; workflow ID không có dạng
// của một lần kích hoạt của đúng workflow trong tham chiếu thì bị bỏ qua. Lỗi trả về
// không mang nội dung: lịch sử của Temporal chỉ thấy một mã cố định.
func (a activities) StartScheduledRun(ctx context.Context, ref Ref) error {
	info := activity.GetInfo(ctx)
	key := info.WorkflowExecution.ID
	if !ValidKey(ref, key) {
		return nil // không phải lần kích hoạt của lịch này: không có gì để thử lại
	}
	if err := a.runner.StartScheduled(ctx, ref.OrgID, ref.WorkflowID, key, info.Attempt >= maxAttempts); err != nil {
		return temporal.NewApplicationError("không tạo được lần chạy theo lịch", "schedule_start_failed")
	}
	return nil
}

// ValidKey báo key có dạng workflow ID của một lần kích hoạt của lịch trong ref:
// "workflow-run/<org>/<workflow>-<thời điểm RFC 3339>".
func ValidKey(ref Ref, key string) bool {
	at, ok := strings.CutPrefix(key, runID(ref.OrgID, ref.WorkflowID)+"-")
	if !ok || ref.OrgID == uuid.Nil || ref.WorkflowID == uuid.Nil {
		return false
	}
	_, err := time.Parse(time.RFC3339, at)
	return err == nil
}

// Start chạy worker của hàng đợi api-internal trong process này; hàm trả về dừng nó
// (chờ activity đang chạy xong). Gọi stop trước khi đóng client.
func Start(c client.Client, runner Runner) (stop func(), err error) {
	w := NewWorker(c, runner)
	if err := w.Start(); err != nil {
		return nil, fmt.Errorf("schedule: khởi động worker %s: %w", TaskQueue, err)
	}
	return w.Stop, nil
}

// NewWorker tạo worker của hàng đợi api-internal. Người gọi Start và Stop nó.
func NewWorker(c client.Client, runner Runner) worker.Worker {
	w := worker.New(c, TaskQueue, worker.Options{MaxConcurrentActivityExecutionSize: 4, MaxConcurrentWorkflowTaskExecutionSize: 4,
		WorkerStopTimeout: stopTimeout})
	w.RegisterWorkflowWithOptions(Run, workflow.RegisterOptions{Name: WorkflowName})
	w.RegisterActivityWithOptions(activities{runner: runner}.StartScheduledRun, activity.RegisterOptions{Name: ActivityName})
	return w
}

// Scheduler hiện thực definitions.Scheduler trên Temporal Schedules.
type Scheduler struct{ Client client.Client }

var cronField = regexp.MustCompile(`^[0-9*/,-]+$`)

// Valid kiểm một biểu thức cron: đúng 5 trường số (phút giờ ngày tháng thứ, giờ UTC),
// không có tên, macro (@hourly) hay múi giờ, và không kích hoạt dày hơn MinInterval.
func (Scheduler) Valid(expr string) error { return Valid(expr) }

// Valid là phần thuần của Scheduler.Valid.
func Valid(expr string) error {
	if len(expr) > maxCronLen {
		return fmt.Errorf("biểu thức cron dài quá %d ký tự", maxCronLen)
	}
	fields := strings.Fields(expr)
	if len(fields) != 5 || strings.Join(fields, " ") != expr {
		return errors.New("biểu thức cron phải có đúng 5 trường cách nhau một dấu cách: phút giờ ngày tháng thứ")
	}
	for _, f := range fields {
		if !cronField.MatchString(f) {
			return errors.New("biểu thức cron chỉ nhận số và các ký tự * / , -")
		}
	}
	sched, err := cron.ParseStandard(expr)
	if err != nil {
		return errors.New("biểu thức cron không hợp lệ")
	}
	// Mẫu cron lặp lại theo chu kỳ: duyệt một số lần kích hoạt hữu hạn từ một mốc cố định.
	at := sched.Next(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	if at.IsZero() {
		return errors.New("biểu thức cron không bao giờ kích hoạt")
	}
	for range 512 {
		next := sched.Next(at)
		if next.IsZero() {
			break
		}
		if next.Sub(at) < MinInterval {
			return fmt.Errorf("lịch kích hoạt dày hơn mỗi %d phút một lần", int(MinInterval/time.Minute))
		}
		at = next
	}
	return nil
}

// Sync đưa lịch của workflow về đúng trạng thái: có Schedule khi và chỉ khi workflow
// active và có biểu thức cron. Idempotent.
func (s Scheduler) Sync(ctx context.Context, orgID uuid.UUID, w definitions.Workflow) error {
	id := ID(orgID, w.ID)
	h := s.Client.ScheduleClient().GetHandle(ctx, id)
	var notFound *serviceerror.NotFound
	if w.Status != "active" || w.Schedule == nil {
		if err := h.Delete(ctx); err != nil && !errors.As(err, &notFound) {
			return fmt.Errorf("schedule: xoá lịch: %w", err)
		}
		return nil
	}
	if err := Valid(*w.Schedule); err != nil {
		return err
	}
	spec := client.ScheduleSpec{CronExpressions: []string{*w.Schedule}}
	err := h.Update(ctx, client.ScheduleUpdateOptions{DoUpdate: func(in client.ScheduleUpdateInput) (*client.ScheduleUpdate, error) {
		sched := in.Description.Schedule
		sched.Spec = &spec
		return &client.ScheduleUpdate{Schedule: &sched}, nil
	}})
	if err == nil {
		return nil
	}
	if !errors.As(err, &notFound) {
		return fmt.Errorf("schedule: sửa lịch: %w", err)
	}
	_, err = s.Client.ScheduleClient().Create(ctx, client.ScheduleOptions{
		ID:   id,
		Spec: spec,
		Action: &client.ScheduleWorkflowAction{
			ID:                       runID(orgID, w.ID),
			Workflow:                 WorkflowName,
			Args:                     []any{Ref{OrgID: orgID, WorkflowID: w.ID}},
			TaskQueue:                TaskQueue,
			WorkflowExecutionTimeout: runTimeout,
			TypedSearchAttributes: temporal.NewSearchAttributes(temporal.NewSearchAttributeKeyKeyword("OrgId").ValueSet(orgID.String()),
				temporal.NewSearchAttributeKeyKeyword("ProjectId").ValueSet(w.ProjectID.String())),
		},
		// Lần trước chưa xong thì bỏ lần này; lịch bị lỡ quá một phút (Temporal hoặc api
		// ngừng) không được chạy bù dồn dập.
		Overlap:       enums.SCHEDULE_OVERLAP_POLICY_SKIP,
		CatchupWindow: time.Minute,
	})
	if err != nil && !errors.Is(err, temporal.ErrScheduleAlreadyRunning) {
		return fmt.Errorf("schedule: tạo lịch: %w", err)
	}
	return nil
}
