// Package definitions là workflow do người dùng định nghĩa (ADR-0015): định nghĩa có
// phiên bản, và các lần chạy của nó.
//
// Một định nghĩa là DỮ LIỆU: một đồ thị không chu trình các bước, không có biến hay
// template. Mỗi lần chạy được biên dịch thành một kế hoạch và chạy như một execution
// `planned` của Agent Runtime; package này không có bộ diễn giải riêng. Nó chỉ giữ định
// nghĩa, biên dịch nó, và giữ bản chiếu của từng lần chạy (workflow_executions), lấy trạng
// thái từ execution của lần chạy đó khi được đọc.
package definitions

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	pgxv5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hqgroups/mcp/internal/agent/orchestrator"
	"github.com/hqgroups/mcp/internal/audit"
	"github.com/hqgroups/mcp/internal/platform/authz"
	"github.com/hqgroups/mcp/internal/platform/identity"
	"github.com/hqgroups/mcp/internal/platform/tenancy"
	apperr "github.com/hqgroups/mcp/internal/shared/errors"
	"github.com/hqgroups/mcp/internal/shared/ids"
	"github.com/hqgroups/mcp/internal/shared/pagination"
	"github.com/hqgroups/mcp/internal/workflow/definitions/store"
)

const (
	maxNameLen        = 200
	maxDescriptionLen = 2000
	maxNoteRunes      = 2000
	maxDefinitionSize = 256 << 10
	sortCreated       = "-created_at"

	// Trần số workflow chưa lưu trữ của một project và số workflow đang có lịch của một org.
	maxWorkflowsPerProject = 200
	maxScheduledPerOrg     = 50
	// scheduledGap là khoảng cách nhỏ nhất giữa hai lần chạy theo lịch của một workflow.
	// Lịch hợp lệ không dày hơn 5 phút; phần chênh là độ trễ của nơi kích hoạt. Trần này
	// được giữ ở đây, trong database, chứ không tin nơi kích hoạt.
	scheduledGap = 4 * time.Minute
	// maxScheduleFailures: sau ngần này lần lịch kích hoạt liên tiếp bị từ chối, workflow
	// tự về disabled (một workflow hỏng không sinh lần chạy lỗi và audit mãi mãi).
	maxScheduleFailures = 3
	// maxListSync là số lần chạy chưa kết thúc được đọc lại trạng thái trong một trang.
	maxListSync = 20

	stepAgent    = "agent"
	stepWait     = "wait"
	stepApproval = "approval"
	// defaultApprovalSeconds là thời hạn mặc định của một bước approval.
	defaultApprovalSeconds = 1800

	statusDraft    = "draft"
	statusActive   = "active"
	statusDisabled = "disabled"
	statusArchived = "archived"

	// Loại kích hoạt của một lần chạy.
	TriggerManual   = "manual"
	TriggerAPI      = "api"
	TriggerSchedule = "schedule"
)

var (
	errNotFound  = apperr.New(apperr.KindNotFound, "not_found", "tài nguyên không tồn tại")
	errNotActive = apperr.New(apperr.KindConflict, "workflow_not_active", "workflow không ở trạng thái active")
	errArchived  = apperr.New(apperr.KindConflict, "workflow_archived", "workflow đã được lưu trữ")
	errStale     = apperr.New(apperr.KindPreconditionFailed, "precondition_failed", "workflow đã thay đổi; đọc lại rồi thử lại")
	errFinished  = apperr.New(apperr.KindConflict, "execution_finished", "lần chạy đã kết thúc")
	// errNeedsUser: thay đổi làm người gọi thành người chạy của workflow, và người chạy phải là user.
	errNeedsUser = apperr.New(apperr.KindUnprocessable, "validation_failed",
		"chỉ user mới kích hoạt, hoặc sửa definition / schedule của workflow đang active: lần chạy theo lịch mang quyền của người đó")
	errRunUnchecked  = apperr.New(apperr.KindConflict, "workflow_changed", "workflow vừa đổi trạng thái; đọc lại rồi thử lại").AsRetryable()
	errWorkflowLimit = apperr.New(apperr.KindConflict, "workflow_limit_reached", "project đã có số workflow tối đa; lưu trữ bớt workflow")
	errScheduleLimit = apperr.New(apperr.KindConflict, "schedule_limit_reached", "organization đã có số workflow chạy theo lịch tối đa")
	errTooFrequent   = apperr.New(apperr.KindConflict, "schedule_too_frequent", "workflow vừa có một lần chạy theo lịch")
)

func invalid(msg string) error { return apperr.New(apperr.KindUnprocessable, "validation_failed", msg) }
func internal(err error) error { return apperr.Wrap(err, apperr.KindInternal, "internal_error", "") }

// Money là số tiền dạng chuỗi thập phân.
type Money struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

// Step là một bước của workflow.
type Step struct {
	ID        string     `json:"id"`
	Type      string     `json:"type"`
	DependsOn []string   `json:"depends_on,omitempty"`
	AgentID   *uuid.UUID `json:"agent_id,omitempty"`
	Goal      string     `json:"goal,omitempty"`
	Budget    *Money     `json:"budget,omitempty"`
	OnFailure string     `json:"on_failure,omitempty"`
	Seconds   int        `json:"seconds,omitempty"`
	// Bước approval: điều người duyệt được hỏi, số người duyệt, thời hạn duyệt. Với bước
	// agent, TimeoutSeconds là thời hạn riêng của bước.
	Message           string `json:"message,omitempty"`
	RequiredApprovals int    `json:"required_approvals,omitempty"`
	TimeoutSeconds    int    `json:"timeout_seconds,omitempty"`
	// MaxAttempts (chỉ bước agent; 0 hoặc 1 = không thử lại, tối đa 3): số lần chạy bước
	// khi nó kết thúc failed hoặc timed_out. Mỗi lần thử là một task riêng của kế hoạch
	// (id "<id>-try2", "<id>-try3") với budget riêng bằng budget của bước.
	MaxAttempts int `json:"max_attempts,omitempty"`
}

// attemptKey là khoá trong kế hoạch của lần thử thứ n (từ 1) của bước id.
func attemptKey(id string, n int) string {
	if n <= 1 {
		return id
	}
	return id + "-try" + strconv.Itoa(n)
}

// Definition là định nghĩa của một workflow.
type Definition struct {
	MaxParallelSteps int    `json:"max_parallel_steps,omitempty"`
	Steps            []Step `json:"steps"`
}

// ParseDefinition đọc một định nghĩa và kiểm cấu trúc của nó. Khoá lạ bị từ chối: một
// trường viết sai (hoặc một tính năng chưa có) không được lặng lẽ bỏ qua.
func ParseDefinition(raw json.RawMessage) (Definition, error) {
	var d Definition
	if len(raw) == 0 || len(raw) > maxDefinitionSize {
		return d, invalid("definition là bắt buộc và không vượt " + strconv.Itoa(maxDefinitionSize>>10) + " KiB")
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return Definition{}, invalid("definition không đúng định dạng: " + truncateErr(err))
	}
	if _, err := d.Plan(); err != nil {
		return Definition{}, err
	}
	return d, nil
}

func truncateErr(err error) string {
	s := err.Error()
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// Plan biên dịch định nghĩa thành kế hoạch của một execution `planned` và kiểm cấu trúc
// (phụ thuộc, chu trình, số bước, độ sâu). Việc Agent tồn tại, còn hoạt động và có budget
// được kiểm ở mỗi lần chạy, bởi Agent Runtime.
func (d Definition) Plan() (orchestrator.Plan, error) {
	plan := orchestrator.Plan{MaxParallel: d.MaxParallelSteps, Tasks: make([]orchestrator.Task, 0, len(d.Steps))}
	// Bước có thử lại biên dịch thành một chuỗi task; bước phụ thuộc vào nó phụ thuộc vào
	// LẦN THỬ CUỐI (kết quả của bước là kết quả của chuỗi, orchestrator.Effective).
	ids := make(map[string]bool, len(d.Steps))
	last := make(map[string]string, len(d.Steps))
	for _, s := range d.Steps {
		ids[s.ID] = true
	}
	for i, s := range d.Steps {
		at := "definition.steps[" + strconv.Itoa(i) + "]"
		if s.MaxAttempts < 0 || s.MaxAttempts > orchestrator.MaxAttempts || (s.MaxAttempts != 0 && s.Type != stepAgent) {
			return plan, invalid(at + ".max_attempts: chỉ bước agent, từ 1 đến " + strconv.Itoa(orchestrator.MaxAttempts))
		}
		// Khoá của lần thử lại là id + "-tryN": id phải còn chỗ cho hậu tố trong 32 ký tự.
		if s.MaxAttempts > 1 && len(attemptKey(s.ID, s.MaxAttempts)) > 32 {
			return plan, invalid(at + ": id của bước có max_attempts dài tối đa 27 ký tự")
		}
		last[s.ID] = attemptKey(s.ID, max(s.MaxAttempts, 1))
		for n := 2; n <= s.MaxAttempts; n++ {
			if ids[attemptKey(s.ID, n)] {
				return plan, invalid(at + ": id " + attemptKey(s.ID, n) + " trùng với lần thử lại của bước " + s.ID)
			}
		}
	}
	for i, s := range d.Steps {
		at := "definition.steps[" + strconv.Itoa(i) + "]"
		deps := make([]string, len(s.DependsOn))
		for k, dep := range s.DependsOn {
			deps[k] = dep
			if l, ok := last[dep]; ok {
				deps[k] = l
			}
		}
		t := orchestrator.Task{Key: s.ID, DependsOn: deps, OnFailure: s.OnFailure}
		switch s.Type {
		case stepAgent:
			if s.AgentID == nil {
				return plan, invalid(at + ": bước agent cần agent_id")
			}
			if s.Seconds != 0 {
				return plan, invalid(at + ": seconds chỉ dùng với bước wait")
			}
			if s.Message != "" || s.RequiredApprovals != 0 {
				return plan, invalid(at + ": message, required_approvals chỉ dùng với bước approval")
			}
			// timeout_seconds của bước agent: thời hạn riêng, chỉ chặt hơn của Agent Version.
			t.TimeoutSeconds = s.TimeoutSeconds
			t.Kind, t.AgentID, t.Goal = orchestrator.KindAgent, *s.AgentID, s.Goal
			if s.Budget != nil {
				if s.Budget.Currency != "USD" || s.Budget.Amount == "" {
					return plan, invalid(at + ".budget: cần amount và currency USD")
				}
				t.Budget = s.Budget.Amount
			}
		case stepWait:
			if s.AgentID != nil || s.Goal != "" || s.Budget != nil {
				return plan, invalid(at + ": bước wait không có agent_id, goal hay budget")
			}
			if s.Message != "" || s.RequiredApprovals != 0 || s.TimeoutSeconds != 0 {
				return plan, invalid(at + ": message, required_approvals, timeout_seconds chỉ dùng với bước approval")
			}
			t.Kind, t.WaitSeconds = orchestrator.KindWait, s.Seconds
		case stepApproval:
			if s.AgentID != nil || s.Goal != "" || s.Budget != nil || s.Seconds != 0 {
				return plan, invalid(at + ": bước approval không có agent_id, goal, budget hay seconds")
			}
			gate := orchestrator.Gate{Message: s.Message, Required: s.RequiredApprovals, TimeoutSeconds: s.TimeoutSeconds}
			if gate.Required == 0 {
				gate.Required = 1
			}
			if gate.TimeoutSeconds == 0 {
				gate.TimeoutSeconds = defaultApprovalSeconds
			}
			t.Kind, t.Gate = orchestrator.KindApproval, &gate
		case "tool":
			return plan, invalid(at + ": loại bước " + s.Type + " chưa được hỗ trợ")
		default:
			return plan, invalid(at + ".type phải là agent, wait hoặc approval")
		}
		// Các lần thử chưa phải cuối mang on_failure: continue (chúng không chặn và không
		// làm hỏng kế hoạch); lần thử cuối mang on_failure của bước.
		for n := 1; n <= max(s.MaxAttempts, 1); n++ {
			a := t
			a.Key = attemptKey(s.ID, n)
			if n > 1 {
				prev := attemptKey(s.ID, n-1)
				a.RetryOf, a.DependsOn = prev, append(slices.Clone(deps), prev)
			}
			if n < s.MaxAttempts {
				a.OnFailure = orchestrator.Continue
			}
			plan.Tasks = append(plan.Tasks, a)
		}
	}
	if len(plan.Tasks) > orchestrator.MaxTasks {
		return plan, invalid("definition: mỗi lần thử của một bước là một task; tổng " + strconv.Itoa(len(plan.Tasks)) +
			" task vượt trần " + strconv.Itoa(orchestrator.MaxTasks))
	}
	if _, err := orchestrator.Validate(plan); err != nil {
		// Thông báo của bộ kiểm kế hoạch nói về "plan.tasks": đổi sang tên của định nghĩa.
		return plan, invalid(strings.ReplaceAll(strings.ReplaceAll(err.Error(), "plan.tasks", "definition.steps"), "plan.", "definition."))
	}
	agents := 0
	for _, t := range plan.Tasks {
		if t.Kind == orchestrator.KindAgent {
			agents++
		}
	}
	if agents == 0 {
		return plan, invalid("definition phải có ít nhất một bước agent")
	}
	return plan, nil
}

// Workflow là một workflow do người dùng định nghĩa.
type Workflow struct {
	ID          uuid.UUID
	ProjectID   uuid.UUID
	Slug        string
	Name        string
	Description *string
	Version     int
	Definition  json.RawMessage
	Schedule    *string
	Status      string
	RunAs       *uuid.UUID
	// ScheduleFailures là số lần lịch kích hoạt liên tiếp bị từ chối.
	ScheduleFailures int
	// DefinitionByType / DefinitionBy: principal đã viết definition hiện tại. Người đó (hay
	// người tạo API key đó) không duyệt được cổng approval của các lần chạy definition này.
	DefinitionByType *string
	DefinitionBy     *uuid.UUID
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// Run là bản chiếu của một lần chạy workflow.
type Run struct {
	ID              uuid.UUID
	ProjectID       uuid.UUID
	WorkflowID      uuid.UUID
	WorkflowVersion int
	TriggerType     string
	Status          string
	Input           json.RawMessage
	Output          json.RawMessage
	Error           json.RawMessage
	ExecutionID     *uuid.UUID
	StartedAt       *time.Time
	FinishedAt      *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// ExecutionState là trạng thái của execution chạy một lần chạy workflow.
type ExecutionState struct {
	Status     string
	Output     json.RawMessage
	Error      json.RawMessage
	StartedAt  *time.Time
	FinishedAt *time.Time
}

// Executions là thứ Workflow cần từ Agent Runtime: tạo execution `planned` cho một lần
// chạy, đọc trạng thái và huỷ nó.
type Executions interface {
	StartPlanned(ctx context.Context, projectID, workflowExecutionID uuid.UUID, note string, plan orchestrator.Plan) (uuid.UUID, error)
	State(ctx context.Context, executionID uuid.UUID) (ExecutionState, error)
	Cancel(ctx context.Context, executionID uuid.UUID) error
	// ForRun trả về execution đã được tạo cho một lần chạy, nếu có.
	ForRun(ctx context.Context, workflowExecutionID uuid.UUID) (uuid.UUID, bool, error)
}

// Actors dựng ngữ cảnh "chạy dưới quyền một người dùng" cho lần chạy theo lịch
// (ADR-0015): không có request nào mang token của người đó, nên mọi điều kiện mà
// middleware xác thực áp cho một request được kiểm lại ở đây, mỗi lần lịch kích hoạt.
type Actors interface {
	// As trả về ctx mang tenant và principal của user, sau khi kiểm user, membership và
	// organization còn hoạt động.
	As(ctx context.Context, orgID, userID uuid.UUID) (context.Context, error)
	// Require kiểm một quyền của principal trong ctx.
	Require(ctx context.Context, permission string, projectID *uuid.UUID) error
}

// Projects là thứ Workflow cần từ Identity & Tenancy.
type Projects interface {
	GetProject(ctx context.Context, id uuid.UUID) (tenancy.Project, error)
}

// Scheduler đồng bộ lịch chạy của một workflow sang nơi kích hoạt nó (Temporal
// Schedules). nil: môi trường này không chạy theo lịch, và `schedule` bị từ chối.
type Scheduler interface {
	// Sync tạo, sửa hoặc xoá lịch của workflow theo trạng thái hiện tại của nó: có lịch
	// khi và chỉ khi workflow đang active và có biểu thức cron.
	Sync(ctx context.Context, orgID uuid.UUID, w Workflow) error
	// Valid kiểm một biểu thức cron.
	Valid(cron string) error
}

// Service quản lý workflow và các lần chạy của nó.
type Service struct {
	db         *pgxpool.Pool
	projects   Projects
	executions Executions
	scheduler  Scheduler
	actors     Actors
}

// WithActors bật lần chạy theo lịch: StartScheduled cần nó để chạy dưới quyền người đã
// kích hoạt workflow.
func (s *Service) WithActors(a Actors) *Service {
	s.actors = a
	return s
}

// NewService tạo Service. executions nil: process này không chạy được workflow.
func NewService(db *pgxpool.Pool, projects Projects, executions Executions, scheduler Scheduler) *Service {
	return &Service{db: db, projects: projects, executions: executions, scheduler: scheduler}
}

func fromRow(r store.Workflow) Workflow {
	return Workflow{ID: r.ID, ProjectID: r.ProjectID, Slug: r.Slug, Name: r.Name, Description: r.Description, Version: int(r.Version),
		Definition: r.Definition, Schedule: r.Schedule, Status: r.Status, RunAs: r.RunAs, ScheduleFailures: int(r.ScheduleFailures),
		DefinitionByType: r.DefinitionByType, DefinitionBy: r.DefinitionBy,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
}

func runFromRow(r store.WorkflowExecution) Run {
	return Run{ID: r.ID, ProjectID: r.ProjectID, WorkflowID: r.WorkflowID, WorkflowVersion: int(r.WorkflowVersion), TriggerType: r.TriggerType,
		Status: r.Status, Input: r.Input, Output: r.Output, Error: r.Error, ExecutionID: r.ExecutionID,
		StartedAt: r.StartedAt, FinishedAt: r.FinishedAt, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
}

func validText(s string, maxRunes int, required bool) bool {
	if !utf8.ValidString(s) || strings.ContainsRune(s, 0) || utf8.RuneCountInString(s) > maxRunes {
		return false
	}
	return !required || strings.TrimSpace(s) != ""
}

func (s *Service) validSchedule(cron *string) error {
	if cron == nil {
		return nil
	}
	if s.scheduler == nil {
		return invalid("schedule: môi trường này chưa được cấu hình để chạy workflow theo lịch")
	}
	if err := s.scheduler.Valid(*cron); err != nil {
		return invalid("schedule: " + err.Error())
	}
	return nil
}

// CreateInput là yêu cầu tạo workflow.
type CreateInput struct {
	Slug        string
	Name        string
	Description *string
	Definition  json.RawMessage
	Schedule    *string
}

// Create tạo một workflow ở trạng thái draft.
func (s *Service) Create(ctx context.Context, projectID uuid.UUID, in CreateInput) (Workflow, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	p, pok := identity.PrincipalFrom(ctx)
	if !ok || !pok {
		return Workflow{}, internal(tenancy.ErrNoTenant)
	}
	project, err := s.projects.GetProject(ctx, projectID)
	if err != nil {
		return Workflow{}, err
	}
	if project.ArchivedAt != nil {
		return Workflow{}, apperr.New(apperr.KindConflict, "project_archived", "project đã được lưu trữ")
	}
	if !validText(in.Name, maxNameLen, true) {
		return Workflow{}, invalid("name phải có từ 1 đến 200 ký tự")
	}
	if in.Description != nil && !validText(*in.Description, maxDescriptionLen, false) {
		return Workflow{}, invalid("description có tối đa 2000 ký tự")
	}
	def, err := ParseDefinition(in.Definition)
	if err != nil {
		return Workflow{}, err
	}
	if err := s.validSchedule(in.Schedule); err != nil {
		return Workflow{}, err
	}
	var createdBy *uuid.UUID
	if p.Type == identity.PrincipalUser {
		createdBy = &p.ID
	}
	id := ids.New()
	var row store.Workflow
	err = tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) error {
		q := store.New(tx)
		if err := q.LockWorkflowLimits(ctx, orgID); err != nil {
			return err
		}
		n, err := q.CountProjectWorkflows(ctx, store.CountProjectWorkflowsParams{OrgID: orgID, ProjectID: projectID})
		if err != nil {
			return err
		}
		if n >= maxWorkflowsPerProject {
			return errWorkflowLimit
		}
		row, err = q.InsertWorkflow(ctx, store.InsertWorkflowParams{ID: id, OrgID: orgID, ProjectID: projectID, Slug: in.Slug,
			Name: in.Name, Description: in.Description, Definition: mustJSON(def), CreatedBy: createdBy,
			DefinitionByType: &p.Type, DefinitionBy: &p.ID})
		if err != nil {
			return err
		}
		if in.Schedule != nil {
			// Lịch được lưu cùng workflow nhưng chỉ có hiệu lực khi workflow được kích hoạt.
			if row, err = q.UpdateWorkflow(ctx, store.UpdateWorkflowParams{OrgID: orgID, ID: id, Name: row.Name, Description: row.Description,
				Definition: row.Definition, Version: row.Version, Schedule: in.Schedule, Status: row.Status, RunAs: row.RunAs,
				DefinitionByType: row.DefinitionByType, DefinitionBy: row.DefinitionBy}); err != nil {
				return err
			}
		}
		return audit.Record(ctx, tx, audit.Event{OrgID: orgID, ProjectID: &projectID, Action: "workflow.create", ResourceType: "workflow",
			ResourceID: &id, Outcome: audit.OutcomeSuccess, Details: map[string]string{"slug": in.Slug, "steps": strconv.Itoa(len(def.Steps))}})
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return Workflow{}, apperr.New(apperr.KindConflict, "slug_taken", "slug đã được dùng trong project này")
		}
		if errors.Is(err, errWorkflowLimit) {
			return Workflow{}, err
		}
		return Workflow{}, internal(err)
	}
	return fromRow(row), nil
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

// Get trả về một workflow của tenant.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (Workflow, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return Workflow{}, internal(tenancy.ErrNoTenant)
	}
	var row store.Workflow
	err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) (err error) {
		row, err = store.New(tx).GetWorkflow(ctx, store.GetWorkflowParams{OrgID: orgID, ID: id})
		return err
	})
	if errors.Is(err, pgxv5.ErrNoRows) {
		return Workflow{}, errNotFound
	}
	if err != nil {
		return Workflow{}, internal(err)
	}
	return fromRow(row), nil
}

func after(cursor string) (*time.Time, *uuid.UUID, error) {
	c, err := pagination.Decode(cursor, sortCreated)
	if err != nil || c == nil {
		return nil, nil, err
	}
	at, err := time.Parse(time.RFC3339Nano, c.Value)
	if err != nil {
		return nil, nil, pagination.ErrInvalidCursor
	}
	return &at, &c.ID, nil
}

func nextCursor(at time.Time, id uuid.UUID) string {
	return pagination.Encode(pagination.Cursor{Sort: sortCreated, Value: at.UTC().Format(time.RFC3339Nano), ID: id})
}

// List liệt kê workflow của một project, mới nhất trước.
func (s *Service) List(ctx context.Context, projectID uuid.UUID, status string, limit int, cursor string) ([]Workflow, string, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return nil, "", internal(tenancy.ErrNoTenant)
	}
	at, id, err := after(cursor)
	if err != nil {
		return nil, "", err
	}
	params := store.ListWorkflowsParams{OrgID: orgID, ProjectID: projectID, AfterCreatedAt: at, AfterID: id, RowLimit: int32(limit + 1)} //nolint:gosec // limit ≤ 200
	if status != "" {
		params.Status = &status
	}
	var rows []store.Workflow
	if err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) (err error) {
		rows, err = store.New(tx).ListWorkflows(ctx, params)
		return err
	}); err != nil {
		return nil, "", internal(err)
	}
	next := ""
	if len(rows) > limit {
		rows = rows[:limit]
		next = nextCursor(rows[limit-1].CreatedAt, rows[limit-1].ID)
	}
	out := make([]Workflow, len(rows))
	for i, r := range rows {
		out[i] = fromRow(r)
	}
	return out, next, nil
}

// UpdateInput là thay đổi của một workflow; trường nil giữ nguyên.
type UpdateInput struct {
	Name        *string
	Description *string
	Definition  json.RawMessage
	// Schedule: SetSchedule = true thì đặt (hoặc xoá, khi Schedule là nil) lịch chạy.
	Schedule    *string
	SetSchedule bool
	Status      *string
	IfMatch     *time.Time
	// RunChecked: handler đã kiểm người gọi có quyền chạy workflow (workflow:run +
	// execution:create) hay không; RunDenied là kết quả khi không có.
	RunChecked bool
	RunDenied  error
}

// Update sửa một workflow. Đổi definition làm tăng version; lần chạy đang chạy giữ bản
// chụp của nó.
//
// Người chạy của workflow (run_as) là người chịu trách nhiệm về thứ nó chạy theo lịch:
// kích hoạt, và MỌI thay đổi definition / schedule của một workflow đang active, ghi
// người gọi làm người chạy. Người đó phải là user và tự có quyền chạy workflow. Nếu
// không, một người chỉ có workflow:write sẽ chạy được nội dung của mình dưới quyền
// người đã kích hoạt, và tự duyệt được hành động của nó.
func (s *Service) Update(ctx context.Context, id uuid.UUID, in UpdateInput) (Workflow, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	p, pok := identity.PrincipalFrom(ctx)
	if !ok || !pok {
		return Workflow{}, internal(tenancy.ErrNoTenant)
	}
	var def *Definition
	if in.Definition != nil {
		d, err := ParseDefinition(in.Definition)
		if err != nil {
			return Workflow{}, err
		}
		def = &d
	}
	if in.Name != nil && !validText(*in.Name, maxNameLen, true) {
		return Workflow{}, invalid("name phải có từ 1 đến 200 ký tự")
	}
	if in.Description != nil && !validText(*in.Description, maxDescriptionLen, false) {
		return Workflow{}, invalid("description có tối đa 2000 ký tự")
	}
	if in.SetSchedule {
		if err := s.validSchedule(in.Schedule); err != nil {
			return Workflow{}, err
		}
	}
	if in.Status != nil && *in.Status != statusActive && *in.Status != statusDisabled {
		return Workflow{}, invalid("status chỉ nhận active hoặc disabled")
	}
	var row store.Workflow
	err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) error {
		q := store.New(tx)
		cur, err := q.LockWorkflow(ctx, store.LockWorkflowParams{OrgID: orgID, ID: id})
		if err != nil {
			return err
		}
		if cur.Status == statusArchived {
			return errArchived
		}
		if in.IfMatch != nil && !cur.UpdatedAt.Equal(*in.IfMatch) {
			return errStale
		}
		upd := store.UpdateWorkflowParams{OrgID: orgID, ID: id, Name: cur.Name, Description: cur.Description, Definition: cur.Definition,
			Version: cur.Version, Schedule: cur.Schedule, Status: cur.Status, RunAs: cur.RunAs,
			DefinitionByType: cur.DefinitionByType, DefinitionBy: cur.DefinitionBy}
		changed := []string{}
		if in.Name != nil {
			upd.Name = *in.Name
			changed = append(changed, "name")
		}
		if in.Description != nil {
			upd.Description = in.Description
			changed = append(changed, "description")
		}
		if def != nil {
			next := mustJSON(*def)
			if string(next) != string(mustJSON(mustDefinition(cur.Definition))) {
				upd.Definition, upd.Version = next, cur.Version+1
				// Người viết definition mới: không duyệt được cổng approval của các lần chạy nó.
				upd.DefinitionByType, upd.DefinitionBy = &p.Type, &p.ID
				changed = append(changed, "definition")
			}
		}
		rerun := slices.Contains(changed, "definition") // thay đổi này đổi thứ workflow chạy theo lịch
		if in.SetSchedule && !equalText(in.Schedule, cur.Schedule) {
			upd.Schedule = in.Schedule
			changed = append(changed, "schedule")
			rerun = true
		}
		if in.Status != nil {
			if *in.Status != cur.Status {
				upd.Status = *in.Status
				changed = append(changed, "status")
			}
			// Kích hoạt một workflow đã active là nhận lại vai trò người chạy.
			rerun = rerun || *in.Status == statusActive
		}
		if upd.Status == statusActive && rerun {
			// Người chạy phải là một user: API key hết hạn và không tự duyệt được gì.
			if p.Type != identity.PrincipalUser {
				return errNeedsUser
			}
			if !in.RunChecked {
				return errRunUnchecked // workflow được kích hoạt giữa lúc handler đọc và lúc khoá
			}
			if in.RunDenied != nil {
				return in.RunDenied
			}
			if cur.RunAs == nil || *cur.RunAs != p.ID {
				changed = append(changed, "run_as")
			}
			upd.RunAs = &p.ID
		}
		if upd.Status == statusActive && upd.Schedule != nil && (cur.Status != statusActive || cur.Schedule == nil) {
			if err := q.LockWorkflowLimits(ctx, orgID); err != nil {
				return err
			}
			n, err := q.CountScheduledWorkflows(ctx, store.CountScheduledWorkflowsParams{OrgID: orgID, ExceptID: id})
			if err != nil {
				return err
			}
			if n >= maxScheduledPerOrg {
				return errScheduleLimit
			}
		}
		if row, err = q.UpdateWorkflow(ctx, upd); err != nil {
			return err
		}
		details := map[string]string{"fields": strings.Join(changed, ","), "version": strconv.Itoa(int(row.Version)), "status": row.Status}
		if row.RunAs != nil {
			details["run_as"] = row.RunAs.String()
		}
		if row.Schedule != nil {
			details["schedule"] = *row.Schedule
		}
		return audit.Record(ctx, tx, audit.Event{OrgID: orgID, ProjectID: &row.ProjectID, Action: "workflow.update", ResourceType: "workflow",
			ResourceID: &id, Outcome: audit.OutcomeSuccess, Details: details})
	})
	switch {
	case errors.Is(err, pgxv5.ErrNoRows):
		return Workflow{}, errNotFound
	case errors.Is(err, errArchived), errors.Is(err, errStale), errors.Is(err, errNeedsUser), errors.Is(err, errRunUnchecked),
		errors.Is(err, errScheduleLimit), in.RunDenied != nil && errors.Is(err, in.RunDenied):
		return Workflow{}, err
	case err != nil:
		return Workflow{}, internal(err)
	}
	w := fromRow(row)
	if err := s.syncSchedule(ctx, orgID, w); err != nil {
		return Workflow{}, err
	}
	return w, nil
}

func equalText(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func mustDefinition(raw json.RawMessage) Definition {
	var d Definition
	_ = json.Unmarshal(raw, &d)
	return d
}

// syncSchedule đưa lịch của workflow sang nơi kích hoạt. Trạng thái trong database là
// nguồn sự thật; lỗi ở đây được báo cho người gọi để họ thử lại (thao tác idempotent).
func (s *Service) syncSchedule(ctx context.Context, orgID uuid.UUID, w Workflow) error {
	if s.scheduler == nil {
		return nil
	}
	if err := s.scheduler.Sync(ctx, orgID, w); err != nil {
		return apperr.Wrap(err, apperr.KindUnavailable, "schedule_unavailable",
			"workflow đã được lưu nhưng chưa đồng bộ được lịch chạy; gửi lại yêu cầu").AsRetryable()
	}
	return nil
}

// Archive lưu trữ một workflow: nó không chạy được nữa và lịch của nó bị xoá.
func (s *Service) Archive(ctx context.Context, id uuid.UUID) error {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return internal(tenancy.ErrNoTenant)
	}
	var row store.Workflow
	err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) error {
		q := store.New(tx)
		cur, err := q.LockWorkflow(ctx, store.LockWorkflowParams{OrgID: orgID, ID: id})
		if err != nil {
			return err
		}
		row = cur
		if cur.Status == statusArchived {
			return nil
		}
		if row, err = q.UpdateWorkflow(ctx, store.UpdateWorkflowParams{OrgID: orgID, ID: id, Name: cur.Name, Description: cur.Description,
			Definition: cur.Definition, Version: cur.Version, Schedule: cur.Schedule, Status: statusArchived, RunAs: cur.RunAs,
			DefinitionByType: cur.DefinitionByType, DefinitionBy: cur.DefinitionBy}); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{OrgID: orgID, ProjectID: &row.ProjectID, Action: "workflow.archive", ResourceType: "workflow",
			ResourceID: &id, Outcome: audit.OutcomeSuccess})
	})
	if errors.Is(err, pgxv5.ErrNoRows) {
		return errNotFound
	}
	if err != nil {
		return internal(err)
	}
	return s.syncSchedule(ctx, orgID, fromRow(row))
}

// Start chạy một workflow dưới quyền của principal trong ctx. Chỉ workflow active chạy
// được. Bản chụp của định nghĩa được biên dịch thành kế hoạch và chạy như một execution
// `planned`: mọi kiểm tra của Agent Runtime (Agent còn hoạt động, model, giá, budget,
// trần đồng thời) áp dụng ở đây, mỗi lần chạy.
//
// key là khoá của lần kích hoạt theo lịch ("" với lần chạy thủ công). Lần gọi lại với
// cùng khoá không tạo lần chạy thứ hai: nó trả về lần chạy đã có, và nếu lần chạy đó bị
// bỏ dở trước khi có execution thì làm tiếp từ đúng lần chạy đó. Hai lần chạy theo lịch
// của một workflow cách nhau ít nhất scheduledGap.
func (s *Service) Start(ctx context.Context, id uuid.UUID, trigger, note, key string) (Run, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	p, pok := identity.PrincipalFrom(ctx)
	if !ok || !pok {
		return Run{}, internal(tenancy.ErrNoTenant)
	}
	if s.executions == nil {
		return Run{}, apperr.New(apperr.KindUnavailable, "runtime_unavailable", "process này chưa được cấu hình để chạy workflow")
	}
	if !validText(note, maxNoteRunes, false) {
		return Run{}, invalid("input.message có tối đa 2000 ký tự")
	}
	w, err := s.Get(ctx, id)
	if err != nil {
		return Run{}, err
	}
	if w.Status != statusActive {
		return Run{}, errNotActive
	}
	if _, err := ParseDefinition(w.Definition); err != nil {
		return Run{}, err
	}
	runID := ids.New()
	var input json.RawMessage
	if note != "" {
		input = mustJSON(map[string]string{"message": note})
	}
	var triggerKey *string
	if key != "" {
		triggerKey = &key
	}
	var row store.WorkflowExecution
	resumed := false
	err = tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) error {
		q := store.New(tx)
		if triggerKey != nil {
			// Khoá workflow: các lần kích hoạt của một lịch được xét lần lượt.
			cur, err := q.LockWorkflow(ctx, store.LockWorkflowParams{OrgID: orgID, ID: id})
			if err != nil {
				return err
			}
			if cur.Status != statusActive {
				return errNotActive
			}
			w = fromRow(cur)
			existing, err := q.GetWorkflowExecutionByTrigger(ctx, store.GetWorkflowExecutionByTriggerParams{OrgID: orgID, WorkflowID: id, TriggerKey: triggerKey})
			if err == nil {
				row, resumed = existing, true
				return nil
			}
			if !errors.Is(err, pgxv5.ErrNoRows) {
				return err
			}
			last, err := q.LatestScheduledRun(ctx, store.LatestScheduledRunParams{OrgID: orgID, WorkflowID: id, GapSeconds: scheduledGap.Seconds()})
			if err != nil && !errors.Is(err, pgxv5.ErrNoRows) {
				return err
			}
			if err == nil && last.Recent {
				return errTooFrequent
			}
		}
		var err error
		row, err = q.InsertWorkflowExecution(ctx, store.InsertWorkflowExecutionParams{ID: runID, OrgID: orgID, ProjectID: w.ProjectID,
			WorkflowID: w.ID, WorkflowVersion: int32(w.Version), DefinitionSnapshot: w.Definition, TriggerType: trigger, TriggeredBy: &p.ID, //nolint:gosec // version nhỏ
			DefinitionByType: w.DefinitionByType, DefinitionBy: w.DefinitionBy,
			Input: input, TriggerKey: triggerKey})
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{OrgID: orgID, ProjectID: &w.ProjectID, Action: "workflow.run", ResourceType: "workflow_execution",
			ResourceID: &runID, Outcome: audit.OutcomeSuccess,
			Details: map[string]string{"workflow_id": w.ID.String(), "version": strconv.Itoa(w.Version), "trigger": trigger}})
	})
	switch {
	case errors.Is(err, errNotActive), errors.Is(err, errTooFrequent):
		return Run{}, err
	case errors.Is(err, pgxv5.ErrNoRows):
		return Run{}, errNotFound
	case err != nil:
		return Run{}, internal(err)
	}
	if resumed {
		if terminal(row.Status) || row.ExecutionID != nil {
			return runFromRow(s.sync(ctx, orgID, row)), nil
		}
		// Lần chạy bị bỏ dở trước khi được gắn execution: execution đã được tạo thì gắn
		// vào; chưa có thì tạo tiếp cho đúng lần chạy này.
		execID, found, err := s.executions.ForRun(ctx, row.ID)
		if err != nil {
			return Run{}, err
		}
		if found {
			return s.link(ctx, orgID, row.ID, execID)
		}
	}
	// Kế hoạch được biên dịch từ bản chụp của lần chạy, không từ định nghĩa hiện tại.
	var plan orchestrator.Plan
	def, err := ParseDefinition(row.DefinitionSnapshot)
	if err == nil {
		plan, err = def.Plan()
	}
	if err != nil {
		s.closeFailed(ctx, orgID, row, err)
		return Run{}, err
	}
	message := "Lần chạy của workflow " + w.Slug + " (phiên bản " + strconv.Itoa(int(row.WorkflowVersion)) + ")."
	if note != "" {
		message += " Ghi chú: " + note
	}
	execID, startErr := s.executions.StartPlanned(ctx, row.ProjectID, row.ID, message, plan)
	if startErr != nil {
		if triggerKey != nil && transient(startErr) {
			// Lần kích hoạt theo lịch được thử lại với cùng khoá: để lần chạy mở cho lần thử
			// sau. StartScheduled đóng nó ở lần thử cuối.
			return Run{}, startErr
		}
		// Lần chạy đã được ghi nhưng không tạo được execution: đóng nó với đúng lý do.
		s.closeFailed(ctx, orgID, row, startErr)
		return Run{}, startErr
	}
	return s.link(ctx, orgID, row.ID, execID)
}

// transient báo một lỗi nên được thử lại: lỗi nội bộ, phụ thuộc tạm thời không dùng
// được, hoặc đang quá tải.
func transient(err error) bool {
	e, ok := apperr.From(err)
	return !ok || e.Kind == apperr.KindInternal || e.Kind == apperr.KindUnavailable || e.Kind == apperr.KindRateLimited
}

// link gắn execution vào lần chạy của nó. Lần chạy đã bị đóng trong lúc execution được
// tạo (adopt, sau unlinkedGrace) thì không ai theo dõi execution đó nữa: huỷ nó.
func (s *Service) link(ctx context.Context, orgID, runID, execID uuid.UUID) (Run, error) {
	var row store.WorkflowExecution
	err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) error {
		q := store.New(tx)
		if _, err := q.LinkWorkflowExecution(ctx, store.LinkWorkflowExecutionParams{OrgID: orgID, ID: runID, ExecutionID: &execID}); err != nil {
			return err
		}
		var err error
		row, err = q.GetWorkflowExecution(ctx, store.GetWorkflowExecutionParams{OrgID: orgID, ID: runID})
		return err
	})
	if err != nil {
		return Run{}, internal(err)
	}
	if row.ExecutionID == nil {
		slog.WarnContext(ctx, "workflow: lần chạy đã đóng trước khi gắn được execution; huỷ execution", "workflow_execution_id", runID, "execution_id", execID)
		if err := s.executions.Cancel(ctx, execID); err != nil {
			slog.ErrorContext(ctx, "workflow: không huỷ được execution của lần chạy đã đóng", "error", err, "execution_id", execID)
		}
	}
	return runFromRow(row), nil
}

// closeFailed đóng một lần chạy không tạo được execution, kèm lý do, và ghi audit.
func (s *Service) closeFailed(ctx context.Context, orgID uuid.UUID, run store.WorkflowExecution, cause error) {
	code, title := "internal_error", "không tạo được execution cho lần chạy"
	if e, ok := apperr.From(cause); ok && e.Kind != apperr.KindInternal {
		code, title = e.Code, e.Message
	}
	now := time.Now()
	problem := mustJSON(map[string]any{"type": "about:blank", "title": title, "status": 0, "code": code})
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) error {
		n, err := store.New(tx).SyncWorkflowExecution(ctx, store.SyncWorkflowExecutionParams{OrgID: orgID, ID: run.ID, Status: "failed", Error: problem, FinishedAt: &now})
		if err != nil || n == 0 {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{OrgID: orgID, ProjectID: &run.ProjectID, Action: "workflow.run_fail", ResourceType: "workflow_execution",
			ResourceID: &run.ID, Outcome: audit.OutcomeError, Details: map[string]string{"workflow_id": run.WorkflowID.String(), "error_code": code}})
	})
	if err != nil {
		slog.ErrorContext(ctx, "workflow: không đóng được lần chạy", "error", err, "workflow_execution_id", run.ID)
	}
}

func terminal(status string) bool {
	switch status {
	case "completed", "failed", "cancelled", "compensated", "budget_exceeded", "timed_out":
		return true
	}
	return false
}

// sync cập nhật bản chiếu từ execution của lần chạy. Lỗi đọc execution không làm hỏng
// việc đọc bản chiếu: trạng thái đã lưu được trả về.
func (s *Service) sync(ctx context.Context, orgID uuid.UUID, row store.WorkflowExecution) store.WorkflowExecution {
	if terminal(row.Status) || s.executions == nil {
		return row
	}
	if row.ExecutionID == nil {
		return s.adopt(ctx, orgID, row)
	}
	st, err := s.executions.State(ctx, *row.ExecutionID)
	if err != nil || st.Status == "" {
		return row
	}
	if st.Status == row.Status && !terminal(st.Status) {
		return row
	}
	_ = tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) error {
		q := store.New(tx)
		if _, err := q.SyncWorkflowExecution(ctx, store.SyncWorkflowExecutionParams{OrgID: orgID, ID: row.ID, Status: st.Status,
			Output: st.Output, Error: st.Error, StartedAt: st.StartedAt, FinishedAt: st.FinishedAt}); err != nil {
			return err
		}
		fresh, err := q.GetWorkflowExecution(ctx, store.GetWorkflowExecutionParams{OrgID: orgID, ID: row.ID})
		if err == nil {
			row = fresh
		}
		return err
	})
	return row
}

// unlinkedGrace là thời gian một lần chạy được phép chưa có execution: dài hơn hẳn thời
// gian Start cần để tạo execution, nên lần chạy còn thiếu sau đó là lần chạy bị bỏ dở
// (process dừng giữa hai transaction của Start).
const unlinkedGrace = 2 * time.Minute

// adopt sửa một lần chạy bị bỏ dở khi nó được đọc: execution của nó đã được tạo thì gắn
// vào; chưa có thì đóng lần chạy là failed. Nó không tạo execution mới: quyền của lúc
// kích hoạt không còn ở đây (lần kích hoạt theo lịch được thử lại thì làm tiếp ở Start).
func (s *Service) adopt(ctx context.Context, orgID uuid.UUID, row store.WorkflowExecution) store.WorkflowExecution {
	if time.Since(row.CreatedAt) < unlinkedGrace {
		return row
	}
	execID, found, err := s.executions.ForRun(ctx, row.ID)
	if err != nil {
		return row
	}
	if !found {
		s.closeFailed(ctx, orgID, row, apperr.New(apperr.KindUnavailable, "start_interrupted", "lần chạy bị gián đoạn trước khi execution được tạo"))
	}
	_ = tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) error {
		q := store.New(tx)
		if found {
			if _, err := q.LinkWorkflowExecution(ctx, store.LinkWorkflowExecutionParams{OrgID: orgID, ID: row.ID, ExecutionID: &execID}); err != nil {
				return err
			}
		}
		fresh, err := q.GetWorkflowExecution(ctx, store.GetWorkflowExecutionParams{OrgID: orgID, ID: row.ID})
		if err == nil {
			row = fresh
		}
		return err
	})
	if found && row.ExecutionID != nil {
		return s.sync(ctx, orgID, row)
	}
	return row
}

// StartScheduled tạo lần chạy cho một lần lịch kích hoạt (ADR-0015). Nó được gọi từ
// activity chạy trong process api, không từ một request: mọi thứ được đọc lại từ
// database, và lần chạy mang quyền của người chạy của workflow (run_as), kiểm lại ở đây.
//
// Trả lỗi chỉ khi nên thử lại (database hoặc phụ thuộc tạm thời không dùng được) và đây
// chưa phải lần thử cuối (final). Lần kích hoạt bị từ chối (workflow không còn active,
// người chạy mất quyền, định nghĩa không còn hợp lệ) được ghi audit và bỏ qua: thử lại
// không đổi được kết quả. Bị từ chối maxScheduleFailures lần liên tiếp thì workflow về
// disabled và lịch của nó bị xoá.
func (s *Service) StartScheduled(ctx context.Context, orgID, workflowID uuid.UUID, key string, final bool) error {
	if s.actors == nil || s.executions == nil {
		return internal(errors.New("definitions: process này không chạy được workflow theo lịch"))
	}
	if orgID == uuid.Nil || workflowID == uuid.Nil || key == "" || len(key) > 255 {
		return nil // tham chiếu hỏng: không có gì để thử lại
	}
	ctx = tenancy.WithOrg(ctx, orgID)
	w, err := s.Get(ctx, workflowID)
	if errors.Is(err, errNotFound) {
		// Tham chiếu đến từ Temporal và có thể nêu org bất kỳ: không ghi gì vào audit của
		// một org chỉ vì có người nhắc tới nó.
		slog.WarnContext(ctx, "workflow: lần kích hoạt theo lịch trỏ tới workflow không tồn tại", "org_id", orgID, "workflow_id", workflowID)
		return nil
	}
	if err != nil {
		return err
	}
	if w.Status != statusActive || w.Schedule == nil || w.RunAs == nil {
		// Lịch còn sót của một workflow không còn chạy theo lịch: dọn nó, không chạy gì.
		_ = s.syncSchedule(ctx, orgID, w)
		return s.skip(ctx, orgID, w, "workflow_not_scheduled")
	}
	// Chính sách "bỏ lần này khi lần trước chưa xong" của nơi kích hoạt chỉ thấy workflow
	// kích hoạt (xong trong vài giây), không thấy execution: kiểm ở đây.
	open, err := s.previousRunOpen(ctx, orgID, workflowID, key)
	if err != nil {
		return err
	}
	if open {
		return s.skip(ctx, orgID, w, "previous_run_active")
	}
	actx, err := s.actors.As(ctx, orgID, *w.RunAs)
	if err == nil {
		for _, perm := range []string{authz.WorkflowRun, authz.ExecutionCreate} {
			if err = s.actors.Require(actx, perm, &w.ProjectID); err != nil {
				break
			}
		}
	}
	if err == nil {
		_, err = s.Start(actx, workflowID, TriggerSchedule, "", key)
	}
	switch {
	case err == nil:
		if w.ScheduleFailures > 0 {
			_ = tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) error {
				return store.New(tx).ResetScheduleFailures(ctx, store.ResetScheduleFailuresParams{OrgID: orgID, ID: workflowID})
			})
		}
		return nil
	case transient(err):
		if !final {
			return err
		}
		// Hết lần thử: lần chạy còn mở (nếu đã được ghi) không ai làm tiếp nữa.
		s.closeUnstarted(ctx, orgID, workflowID, key)
		return s.skip(ctx, orgID, w, "start_failed")
	case errors.Is(err, errTooFrequent):
		return s.skip(ctx, orgID, w, errTooFrequent.Code)
	}
	reason := "refused"
	if e, ok := apperr.From(err); ok {
		reason = e.Code
	}
	return s.refuse(ctx, orgID, w, reason)
}

// skip ghi audit một lần kích hoạt theo lịch không chạy.
func (s *Service) skip(ctx context.Context, orgID uuid.UUID, w Workflow, reason string) error {
	err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) error {
		return audit.Record(ctx, tx, audit.Event{OrgID: orgID, ProjectID: &w.ProjectID, Action: "workflow.schedule_skip", ResourceType: "workflow",
			ResourceID: &w.ID, Outcome: audit.OutcomeDenied, Details: map[string]string{"reason": reason}})
	})
	if err != nil {
		return internal(err)
	}
	return nil
}

// refuse ghi một lần kích hoạt bị từ chối vì lý do thử lại không đổi được, và tắt
// workflow khi điều đó lặp lại liên tiếp.
func (s *Service) refuse(ctx context.Context, orgID uuid.UUID, w Workflow, reason string) error {
	var disabled *store.Workflow
	err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) error {
		q := store.New(tx)
		n, err := q.BumpScheduleFailures(ctx, store.BumpScheduleFailuresParams{OrgID: orgID, ID: w.ID})
		if err != nil && !errors.Is(err, pgxv5.ErrNoRows) {
			return err
		}
		if err == nil && n >= maxScheduleFailures {
			row, err := q.DisableWorkflow(ctx, store.DisableWorkflowParams{OrgID: orgID, ID: w.ID})
			if err != nil {
				return err
			}
			disabled = &row
		}
		if err := audit.Record(ctx, tx, audit.Event{OrgID: orgID, ProjectID: &w.ProjectID, Action: "workflow.schedule_skip", ResourceType: "workflow",
			ResourceID: &w.ID, Outcome: audit.OutcomeDenied, Details: map[string]string{"reason": reason}}); err != nil {
			return err
		}
		if disabled == nil {
			return nil
		}
		return audit.Record(ctx, tx, audit.Event{OrgID: orgID, ProjectID: &w.ProjectID, Action: "workflow.auto_disable", ResourceType: "workflow",
			ResourceID: &w.ID, Outcome: audit.OutcomeSuccess,
			Details: map[string]string{"reason": reason, "consecutive_failures": strconv.Itoa(maxScheduleFailures)}})
	})
	if err != nil {
		return internal(err)
	}
	if disabled != nil {
		// Lịch còn sót (nếu xoá lỗi) tự được dọn ở lần kích hoạt kế tiếp.
		_ = s.syncSchedule(ctx, orgID, fromRow(*disabled))
	}
	return nil
}

// previousRunOpen báo lần chạy theo lịch gần nhất của workflow (không phải của chính lần
// kích hoạt này) còn chưa kết thúc.
func (s *Service) previousRunOpen(ctx context.Context, orgID, workflowID uuid.UUID, key string) (bool, error) {
	var last store.LatestScheduledRunRow
	err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) (err error) {
		last, err = store.New(tx).LatestScheduledRun(ctx, store.LatestScheduledRunParams{OrgID: orgID, WorkflowID: workflowID, GapSeconds: scheduledGap.Seconds()})
		return err
	})
	if errors.Is(err, pgxv5.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, internal(err)
	}
	if terminal(last.Status) || (last.TriggerKey != nil && *last.TriggerKey == key) {
		return false, nil
	}
	run, err := s.GetRun(ctx, last.ID) // đọc lại trạng thái từ execution của nó
	if err != nil {
		return false, err
	}
	return !terminal(run.Status), nil
}

// closeUnstarted đóng lần chạy của một lần kích hoạt đã hết lần thử mà chưa có execution.
func (s *Service) closeUnstarted(ctx context.Context, orgID, workflowID uuid.UUID, key string) {
	var row store.WorkflowExecution
	err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) (err error) {
		row, err = store.New(tx).GetWorkflowExecutionByTrigger(ctx, store.GetWorkflowExecutionByTriggerParams{OrgID: orgID, WorkflowID: workflowID, TriggerKey: &key})
		return err
	})
	if err != nil || terminal(row.Status) || row.ExecutionID != nil {
		return
	}
	if _, found, err := s.executions.ForRun(ctx, row.ID); err != nil || found {
		return // execution đã có: lần đọc kế tiếp gắn nó vào
	}
	s.closeFailed(ctx, orgID, row, apperr.New(apperr.KindUnavailable, "start_failed", "không khởi chạy được execution cho lần chạy theo lịch"))
}

// LookupRun đọc một lần chạy của tenant đúng như đã lưu, không ghi gì. Handler dùng nó
// để kiểm quyền trước khi GetRun đọc lại trạng thái (việc đó có ghi).
func (s *Service) LookupRun(ctx context.Context, id uuid.UUID) (Run, error) {
	_, row, err := s.runRow(ctx, id)
	if err != nil {
		return Run{}, err
	}
	return runFromRow(row), nil
}

func (s *Service) runRow(ctx context.Context, id uuid.UUID) (uuid.UUID, store.WorkflowExecution, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return uuid.Nil, store.WorkflowExecution{}, internal(tenancy.ErrNoTenant)
	}
	var row store.WorkflowExecution
	err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) (err error) {
		row, err = store.New(tx).GetWorkflowExecution(ctx, store.GetWorkflowExecutionParams{OrgID: orgID, ID: id})
		return err
	})
	if errors.Is(err, pgxv5.ErrNoRows) {
		return orgID, row, errNotFound
	}
	if err != nil {
		return orgID, row, internal(err)
	}
	return orgID, row, nil
}

// GetRun trả về một lần chạy của tenant, với trạng thái lấy từ execution của nó.
func (s *Service) GetRun(ctx context.Context, id uuid.UUID) (Run, error) {
	orgID, row, err := s.runRow(ctx, id)
	if err != nil {
		return Run{}, err
	}
	return runFromRow(s.sync(ctx, orgID, row)), nil
}

func runFromListRow(r store.ListWorkflowExecutionsRow) Run {
	return Run{ID: r.ID, ProjectID: r.ProjectID, WorkflowID: r.WorkflowID, WorkflowVersion: int(r.WorkflowVersion), TriggerType: r.TriggerType,
		Status: r.Status, Input: r.Input, Output: r.Output, Error: r.Error, ExecutionID: r.ExecutionID,
		StartedAt: r.StartedAt, FinishedAt: r.FinishedAt, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
}

// ListRuns liệt kê các lần chạy của một workflow, mới nhất trước. Tối đa maxListSync lần
// chạy chưa kết thúc (các lần mới nhất) được đọc lại trạng thái; phần còn lại trả về
// trạng thái đã lưu.
func (s *Service) ListRuns(ctx context.Context, workflowID uuid.UUID, status string, limit int, cursor string) ([]Run, string, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return nil, "", internal(tenancy.ErrNoTenant)
	}
	at, id, err := after(cursor)
	if err != nil {
		return nil, "", err
	}
	params := store.ListWorkflowExecutionsParams{OrgID: orgID, WorkflowID: workflowID, AfterCreatedAt: at, AfterID: id, RowLimit: int32(limit + 1)} //nolint:gosec // limit ≤ 200
	if status != "" {
		params.Status = &status
	}
	var rows []store.ListWorkflowExecutionsRow
	if err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) (err error) {
		rows, err = store.New(tx).ListWorkflowExecutions(ctx, params)
		return err
	}); err != nil {
		return nil, "", internal(err)
	}
	next := ""
	if len(rows) > limit {
		rows = rows[:limit]
		next = nextCursor(rows[limit-1].CreatedAt, rows[limit-1].ID)
	}
	out := make([]Run, len(rows))
	synced := 0
	for i, r := range rows {
		out[i] = runFromListRow(r)
		if terminal(r.Status) || synced >= maxListSync {
			continue
		}
		synced++
		if fresh, err := s.GetRun(ctx, r.ID); err == nil {
			out[i] = fresh
		}
	}
	return out, next, nil
}

// CancelRun huỷ một lần chạy: huỷ execution của nó. Trạng thái của bản chiếu đổi khi
// execution thực sự dừng.
func (s *Service) CancelRun(ctx context.Context, id uuid.UUID) (Run, error) {
	run, err := s.GetRun(ctx, id)
	if err != nil {
		return Run{}, err
	}
	if terminal(run.Status) {
		return Run{}, errFinished
	}
	if run.ExecutionID == nil || s.executions == nil {
		return Run{}, apperr.New(apperr.KindConflict, "execution_not_started", "lần chạy chưa có execution để huỷ")
	}
	if err := s.executions.Cancel(ctx, *run.ExecutionID); err != nil {
		if e, ok := apperr.From(err); ok && e.Code == "execution_finished" {
			return s.GetRun(ctx, id)
		}
		return Run{}, err
	}
	return s.GetRun(ctx, id)
}
