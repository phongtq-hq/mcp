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
	"unicode/utf8"

	"github.com/google/uuid"
	pgxv5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/trace"

	"github.com/hqgroups/mcp/internal/agent/catalog"
	"github.com/hqgroups/mcp/internal/agent/orchestrator"
	"github.com/hqgroups/mcp/internal/agent/runtime/store"
	"github.com/hqgroups/mcp/internal/ai/modelcatalog"
	"github.com/hqgroups/mcp/internal/audit"
	"github.com/hqgroups/mcp/internal/cost/budget"
	"github.com/hqgroups/mcp/internal/cost/pricing"
	"github.com/hqgroups/mcp/internal/platform/identity"
	"github.com/hqgroups/mcp/internal/platform/tenancy"
	apperr "github.com/hqgroups/mcp/internal/shared/errors"
	"github.com/hqgroups/mcp/internal/shared/ids"
	"github.com/hqgroups/mcp/internal/shared/pagination"
)

const (
	maxMessageRunes        = 100_000
	defaultMaxOutputTokens = 4096
	// Trần số execution chạy đồng thời trong một process. Phase 1 chạy vòng lặp
	// trong process api; vượt trần thì từ chối thay vì xếp hàng không giới hạn.
	defaultMaxConcurrent = 16
	// Trần theo org, để một tenant không chiếm hết chỗ của process.
	maxConcurrentPerOrg = 4
	maxTasksRead        = 200
	// Phần cửa sổ ngữ cảnh tối thiểu dành cho hội thoại khi chọn model theo tier.
	minPromptTokens = 4096
	// Trần độ dài chuỗi model của một task (model chính + dự phòng).
	maxModelChain = 4
	sortCreated   = "-created_at"
)

// Execution là một lần xử lý một yêu cầu.
type Execution struct {
	ID            uuid.UUID
	ProjectID     uuid.UUID
	InitiatorType string
	InitiatorID   *uuid.UUID
	Mode          string
	Status        string
	Input         json.RawMessage
	Output        json.RawMessage
	Error         json.RawMessage
	EstimatedCost string
	Currency      string
	TraceID       *string
	StartedAt     *time.Time
	FinishedAt    *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
	// PauseRequestedAt khác nil: execution đang có yêu cầu tạm dừng (kể cả khi trạng thái
	// chưa là paused); PausedByType / PausedBy là người đã yêu cầu.
	PauseRequestedAt *time.Time
	PausedByType     *string
	PausedBy         *uuid.UUID
}

// Task là một đơn vị công việc trong execution, gắn với đúng một Agent Version.
type Task struct {
	ID            uuid.UUID
	ExecutionID   uuid.UUID
	ProjectID     uuid.UUID
	ParentTaskID  *uuid.UUID
	AgentID       uuid.UUID
	AgentVersion  int
	Depth         int
	Goal          string
	Status        string
	Output        json.RawMessage
	Error         json.RawMessage
	EstimatedCost string
	Currency      string
	// PlanKey, DependsOn, OnFailure: vị trí của task trong kế hoạch (execution planned).
	PlanKey       *string
	DependsOn     []uuid.UUID
	OnFailure     string
	OutputSummary *string
	// Budget là budget của task ("" = không có).
	Budget     string
	Iterations int
	ToolCalls  int
	StartedAt  *time.Time
	FinishedAt *time.Time
	CreatedAt  time.Time
}

// Step là một hành động nguyên tử trong task.
type Step struct {
	ID         uuid.UUID
	TaskID     uuid.UUID
	StepSeq    int
	Kind       string
	Status     string
	ModelID    *uuid.UUID
	Routing    json.RawMessage
	ToolID     *uuid.UUID
	ToolName   *string
	DurationMs *int
	Request    json.RawMessage
	Response   json.RawMessage
	Error      json.RawMessage
	CreatedAt  time.Time
}

// CreateInput là yêu cầu chạy một Agent.
type CreateInput struct {
	Mode           string
	AgentID        *uuid.UUID
	AgentVersion   *int
	Message        string
	TimeoutSeconds *int
	// Plan là kế hoạch của execution chế độ planned; Budget (USD) là trần chi phí của nó.
	Plan   *orchestrator.Plan
	Budget *string
	// proposal đánh dấu execution này là một lần lập kế hoạch (ProposePlan).
	proposal *proposalMark
	// WorkflowExecutionID khác nil: execution này là một lần chạy workflow do người dùng
	// định nghĩa (ADR-0015). Chỉ code Go đặt được; handler của API không đọc nó từ request.
	WorkflowExecutionID *uuid.UUID
	// planFrom khác nil: kế hoạch được chạy thẳng từ đề xuất của execution lập kế hoạch
	// này (RunProposal). Nguồn gốc của kế hoạch khi đó là model, không phải người gọi.
	planFrom *uuid.UUID
	// MayRunAnyVersion: người gọi có quyền publish. Thiếu quyền này thì chỉ chạy được
	// version đang hoạt động: version chưa publish (hoặc đã bị rollback) không được
	// chạy bởi người chỉ có quyền sửa Agent.
	MayRunAnyVersion bool
}

// Prices là thứ runtime cần từ bảng giá.
type Prices interface {
	PriceAt(ctx context.Context, modelID uuid.UUID, at time.Time) (pricing.Pricing, error)
}

// Agents là thứ runtime cần từ Agent Catalog.
type Agents interface {
	Get(ctx context.Context, agentID uuid.UUID) (catalog.Agent, error)
	// ListActive trả về các Agent đang hoạt động của project, theo tên, tối đa limit.
	ListActive(ctx context.Context, projectID uuid.UUID, limit int) ([]catalog.Agent, error)
	Runnable(ctx context.Context, agentID uuid.UUID, version *int) (catalog.Runnable, error)
}

// Models là thứ runtime cần từ Model Catalog.
type Models interface {
	ResolveForUse(ctx context.Context, id uuid.UUID) (modelcatalog.Usable, error)
}

// Projects là thứ runtime cần từ Identity & Tenancy.
type Projects interface {
	GetProject(ctx context.Context, id uuid.UUID) (tenancy.Project, error)
}

var (
	errNotFound = apperr.New(apperr.KindNotFound, "not_found", "tài nguyên không tồn tại")
	errBusy     = apperr.New(apperr.KindRateLimited, "rate_limited", "đang có quá nhiều execution chạy; thử lại sau").AsRetryable()
	errFinished = apperr.New(apperr.KindConflict, "execution_finished", "execution đã kết thúc")
)

func invalid(msg string) error { return apperr.New(apperr.KindUnprocessable, "validation_failed", msg) }
func internal(err error) error { return apperr.Wrap(err, apperr.KindInternal, "internal_error", "") }

// Runner khởi chạy vòng lặp của một execution đã được ghi vào database.
type Runner interface {
	// Admit giữ chỗ cho một execution của org; trả lỗi nếu đang quá tải. Sau Admit phải
	// gọi đúng một trong Start hoặc Abort.
	Admit(orgID uuid.UUID) error
	Abort(orgID uuid.UUID)
	// Start khởi chạy vòng lặp và trả về ngay.
	Start(ctx context.Context, spec Spec) error
	// StartPlan khởi chạy một execution chế độ planned (nhiều task) và trả về ngay.
	StartPlan(ctx context.Context, plan PlanSpec) error
	// Cancel báo cho vòng lặp đang chạy biết đã có yêu cầu huỷ. Yêu cầu huỷ luôn được
	// ghi vào database trước; lời gọi này chỉ để vòng lặp dừng sớm hơn.
	Cancel(ctx context.Context, task TaskRef)
	// ApprovalResolved báo một approval của execution vừa có quyết định, để vòng lặp
	// đang chờ nó thức dậy sớm. Vòng lặp vẫn tự đọc lại trạng thái theo chu kỳ.
	// taskID là task của approval (uuid.Nil nếu không có).
	ApprovalResolved(ctx context.Context, orgID, executionID, taskID uuid.UUID)
	// Resumed báo yêu cầu tạm dừng của execution vừa được gỡ, để các vòng lặp đang tạm
	// dừng thức dậy sớm. tasks là các task còn mở của execution. Vòng lặp vẫn tự đọc lại
	// yêu cầu theo chu kỳ, nên lỡ thông báo này không sao.
	Resumed(ctx context.Context, orgID, executionID uuid.UUID, tasks []uuid.UUID)
}

// ExecutionApprovals là thứ runtime cần từ Approval khi một execution bị huỷ: mọi
// approval còn chờ (hoặc đã duyệt nhưng chưa dùng) của nó bị huỷ theo.
type ExecutionApprovals interface {
	CancelForExecution(ctx context.Context, executionID uuid.UUID) error
}

// ApprovalResolved chuyển thông báo "approval đã có quyết định" tới Runner.
func (s *Service) ApprovalResolved(ctx context.Context, orgID, executionID, taskID uuid.UUID) {
	if s.runner != nil {
		s.runner.ApprovalResolved(ctx, orgID, executionID, taskID)
	}
}

// OrgLimiter là Runner muốn Service chặn số execution đồng thời của một org bằng cách
// đếm trong database.
type OrgLimiter interface {
	OrgLimit() int
}

// Route là chuỗi model Model Router chọn cho một lời gọi.
type Route struct {
	Tier   string
	Models []modelcatalog.Usable
	// Skipped mô tả các model trong chuỗi của tier bị bỏ qua, cho thông báo lỗi.
	Skipped []string
}

// BudgetScope là phạm vi một execution tiêu vào: budget của organization, project,
// agent và user khởi tạo.
type BudgetScope struct {
	OrgID     uuid.UUID
	ProjectID uuid.UUID
	AgentID   uuid.UUID
	UserID    *uuid.UUID
}

// Budgets là thứ runtime cần từ Budget Management khi nhận một execution mới: lỗi
// budget_exceeded (HTTP 402) nếu một budget chặn cứng của phạm vi đã chạm ngưỡng.
type Budgets interface {
	CheckNew(ctx context.Context, scope BudgetScope) error
}

// Router là thứ runtime cần từ Model Router: chuỗi model của một tier trong project.
// tier nil nghĩa là tier mặc định cho một bước của Agent.
type Router interface {
	// minContext là cửa sổ ngữ cảnh tối thiểu (token) một model phải có.
	Route(ctx context.Context, projectID uuid.UUID, tier *string, needTools bool, minContext int) (Route, error)
}

// eligible kiểm một model với ràng buộc của môi trường chạy vòng lặp.
func (s *Service) eligible(ctx context.Context, usable modelcatalog.Usable) error {
	if !slices.Contains(s.adapters, usable.Adapter) {
		return invalid("adapter của model không có ở môi trường này")
	}
	// Adapter có khoá nằm ở Gateway: provider của org phải có khoá của chính org, nó
	// không dùng khoá của platform.
	if slices.Contains(s.remote, usable.Adapter) && !usable.PlatformProvider && !usable.HasCredential {
		return invalid("provider của model chưa có api_key")
	}
	// Model chưa có giá hiệu lực thì không gọi (cost-management §3): chi phí của
	// execution phải ước tính được. Số cộng dồn của execution tính bằng USD.
	price, err := s.prices.PriceAt(ctx, usable.ID, time.Now())
	switch {
	case errors.Is(err, pricing.ErrNoPrice):
		return invalid("model chưa có giá hiệu lực")
	case err != nil:
		return err
	case price.Currency != "USD":
		return invalid("giá của model không tính bằng USD; phase này chưa quy đổi tiền tệ")
	}
	return nil
}

// Service tạo và truy vấn execution; vòng lặp do Runner chạy.
type Service struct {
	db       *pgxpool.Pool
	agents   Agents
	models   Models
	projects Projects
	prices   Prices
	runner   Runner
	router   Router
	budgets  Budgets
	approval ExecutionApprovals
	// orchestration đọc cấu hình chế độ planned của project. nil: planned không dùng được.
	orchestration Orchestration
	adapters      []string
	remote        []string
}

// Config là phụ thuộc của Service. Runner nil nghĩa là process này không chạy được
// execution (tạo execution trả 503).
type Config struct {
	Agents   Agents
	Models   Models
	Projects Projects
	Prices   Prices
	Runner   Runner
	// Router chọn model cho Agent Version dùng model.policy tiered. nil thì các version
	// đó không chạy được.
	Router Router
	// Budgets kiểm budget theo phạm vi khi nhận execution mới. nil: không kiểm.
	Budgets Budgets
	// Approvals huỷ approval của execution bị huỷ. nil: môi trường không có approval.
	Approvals ExecutionApprovals
	// Orchestration đọc cấu hình chế độ planned của project. nil: planned bị từ chối.
	Orchestration Orchestration
	// Adapters là mọi adapter LLM mà môi trường chạy vòng lặp phục vụ được.
	// RemoteAdapters là những adapter trong đó có khoá nằm ở Gateway (anthropic):
	// provider của org dùng chúng phải có khoá của chính org.
	Adapters       []string
	RemoteAdapters []string
}

// NewService tạo Service.
func NewService(db *pgxpool.Pool, cfg Config) *Service {
	return &Service{db: db, agents: cfg.Agents, models: cfg.Models, projects: cfg.Projects, prices: cfg.Prices,
		runner: cfg.Runner, router: cfg.Router, budgets: cfg.Budgets, approval: cfg.Approvals, orchestration: cfg.Orchestration, adapters: cfg.Adapters, remote: cfg.RemoteAdapters}
}

// LocalRunner chạy vòng lặp ngay trong process này, mỗi execution một goroutine. Dùng
// cho phát triển cục bộ và test; không bền vững qua restart (execution mồ côi quá hạn
// được đóng timed_out khi có người đọc). Môi trường thật dùng Temporal.
type LocalRunner struct {
	base  context.Context //nolint:containedctx // vòng đời của process: execution nền dừng khi process dừng
	steps *Steps
	slots chan struct{}

	mu     sync.Mutex
	perOrg map[uuid.UUID]int
	wg     sync.WaitGroup
}

// NewLocalRunner tạo LocalRunner. base là context của process: khi nó bị huỷ, các
// execution đang chạy dừng với trạng thái failed (interrupted).
func NewLocalRunner(base context.Context, steps *Steps) *LocalRunner {
	return &LocalRunner{base: base, steps: steps, slots: make(chan struct{}, defaultMaxConcurrent), perOrg: map[uuid.UUID]int{}}
}

// Wait chờ các execution nền đóng xong (sau khi context của process bị huỷ), tối đa
// tới khi ctx hết hạn. Gọi trong chuỗi shutdown để execution không bị bỏ lại "running".
func (r *LocalRunner) Wait(ctx context.Context) {
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// Admit hiện thực Runner: trần đồng thời của process và của từng org.
func (r *LocalRunner) Admit(orgID uuid.UUID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.perOrg[orgID] >= maxConcurrentPerOrg {
		return errBusy
	}
	select {
	case r.slots <- struct{}{}:
		r.perOrg[orgID]++
		return nil
	default:
		return errBusy
	}
}

// Abort hiện thực Runner.
func (r *LocalRunner) Abort(orgID uuid.UUID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	<-r.slots
	if r.perOrg[orgID]--; r.perOrg[orgID] <= 0 {
		delete(r.perOrg, orgID)
	}
}

// Cancel hiện thực Runner: vòng lặp trong process đọc yêu cầu huỷ từ database trước mỗi bước.
func (r *LocalRunner) Cancel(context.Context, TaskRef) {}

// Resumed hiện thực Runner: vòng lặp trong process tự đọc lại yêu cầu tạm dừng.
func (r *LocalRunner) Resumed(context.Context, uuid.UUID, uuid.UUID, []uuid.UUID) {}

// ApprovalResolved hiện thực Runner: vòng lặp trong process tự đọc trạng thái approval.
func (r *LocalRunner) ApprovalResolved(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) {}

// Start hiện thực Runner.
func (r *LocalRunner) Start(_ context.Context, spec Spec) error {
	r.wg.Add(1)
	go r.run(spec)
	return nil
}

// run là chủ sở hữu của một execution nền: nó dừng khi task xong, hết hạn, bị huỷ,
// hoặc khi process dừng, và luôn đóng execution ở một trạng thái kết thúc.
func (r *LocalRunner) run(spec Spec) {
	defer r.wg.Done()
	task := spec.Task
	defer r.Abort(task.OrgID)
	ctx, cancel := context.WithDeadline(r.base, task.Deadline)
	defer cancel()

	out := Outcome{Status: StatusFailed, ErrorCode: CodeInternal, ErrorMessage: "lỗi nội bộ khi chạy task"}
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				slog.ErrorContext(ctx, "runtime: panic trong execution", "panic", rec, "execution_id", task.ExecutionID,
					"stack", string(debug.Stack()))
			}
		}()
		if err := r.steps.Begin(ctx, task); err != nil {
			slog.ErrorContext(ctx, "runtime: không bắt đầu được execution", "error", err, "execution_id", task.ExecutionID)
			return
		}
		out = Run(localExecutor{ctx: ctx, steps: r.steps}, spec)
	}()
	if err := r.steps.Finish(r.base, task, out); err != nil {
		slog.ErrorContext(ctx, "runtime: không đóng được execution", "error", err, "execution_id", task.ExecutionID)
	}
}

func joinIDs(ids []uuid.UUID) string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return strings.Join(out, ",")
}

func numeric(n pgtype.Numeric) string {
	v, err := n.Value()
	if s, ok := v.(string); err == nil && ok {
		return s
	}
	return "0"
}

func executionFromRow(r store.AgentExecution) Execution {
	return Execution{ID: r.ID, ProjectID: r.ProjectID, InitiatorType: r.InitiatorType, InitiatorID: r.InitiatorID,
		Mode: r.Mode, Status: r.Status, Input: r.Input, Output: r.Output, Error: r.Error,
		EstimatedCost: numeric(r.EstimatedCost), Currency: r.Currency, TraceID: r.TraceID,
		StartedAt: r.StartedAt, FinishedAt: r.FinishedAt, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		PauseRequestedAt: r.PauseRequestedAt, PausedByType: r.PauseRequestedByType, PausedBy: r.PauseRequestedBy}
}

// prepared là một task đã được kiểm và sẵn sàng ghi: Agent Version, chuỗi model và
// các giới hạn của nó.
type prepared struct {
	run     catalog.Runnable
	chain   []modelcatalog.Usable
	tier    string
	skipped []string
	maxOut  int
	timeout time.Duration
}

func (p prepared) chainIDs() []uuid.UUID {
	out := make([]uuid.UUID, len(p.chain))
	for i, u := range p.chain {
		out[i] = u.ID
	}
	return out
}

// spec dựng Spec của task từ cấu hình đã kiểm.
func (p prepared) spec(task TaskRef) Spec {
	task.AgentID, task.AgentVersionID = p.run.Agent.ID, p.run.Version.ID
	// Ngân sách hội thoại theo cửa sổ ngữ cảnh NHỎ NHẤT của chuỗi model: lượt nào cũng
	// có thể rơi vào model dự phòng.
	window := 0
	for _, u := range p.chain {
		if u.ContextWindow > 0 && (window == 0 || u.ContextWindow < window) {
			window = u.ContextWindow
		}
	}
	sp := Spec{Task: task, Tier: p.tier, MaxOutputTokens: p.maxOut, ContextBudgetBytes: contextBudget(window, p.maxOut),
		Model:  ModelRef{ID: p.chain[0].ID, Adapter: p.chain[0].Adapter, ProviderModel: p.chain[0].ProviderModel},
		Limits: Limits{MaxIterations: p.run.Config.Limits.MaxIterations, MaxToolCalls: p.run.Config.Limits.MaxToolCalls}}
	for _, u := range p.chain[1:] {
		sp.Fallbacks = append(sp.Fallbacks, ModelRef{ID: u.ID, Adapter: u.Adapter, ProviderModel: u.ProviderModel})
	}
	return sp
}

// prepare kiểm một Agent cho một task của project: Agent thuộc project và đang hoạt
// động, version được phép chạy, và chọn chuỗi model của task (model được ghim, hoặc
// chuỗi của tier do Model Router chọn). Chuỗi cố định từ lúc tạo task.
func (s *Service) prepare(ctx context.Context, projectID, agentID uuid.UUID, version *int, mayRunAnyVersion bool) (prepared, error) {
	var out prepared
	// Project được xác nhận TRƯỚC mọi lỗi khác: Agent của project khác (kể cả chưa có
	// version đang hoạt động) cho đúng câu trả lời của Agent không tồn tại.
	agent, err := s.agents.Get(ctx, agentID)
	if err != nil {
		if apperr.KindOf(err) == apperr.KindNotFound {
			return out, invalid("agent_id / agent_version không tồn tại trong project này")
		}
		return out, err
	}
	if agent.ProjectID != projectID {
		return out, invalid("agent_id / agent_version không tồn tại trong project này")
	}
	run, err := s.agents.Runnable(ctx, agentID, version)
	if err != nil {
		if apperr.KindOf(err) == apperr.KindNotFound {
			return out, invalid("agent_id / agent_version không tồn tại trong project này")
		}
		return out, err
	}
	if run.Agent.ProjectID != projectID {
		return out, invalid("agent_id / agent_version không tồn tại trong project này")
	}
	// Trạng thái được kiểm sau project: agent của project khác không lộ ra qua mã lỗi.
	if run.Agent.Status != "active" {
		return out, apperr.New(apperr.KindConflict, "agent_not_active", "agent không ở trạng thái active")
	}
	if (run.Agent.ActiveVersion == nil || *run.Agent.ActiveVersion != run.Version.Version) && !mayRunAnyVersion {
		return out, apperr.New(apperr.KindPermissionDenied, "permission_denied",
			"chạy version không phải version đang hoạt động cần quyền agent:publish")
	}
	out.run = run
	cfg := run.Config
	out.maxOut = defaultMaxOutputTokens
	if cfg.Model.MaxOutputTokens != nil && *cfg.Model.MaxOutputTokens > 0 {
		out.maxOut = *cfg.Model.MaxOutputTokens
	}
	switch {
	case cfg.Model.Policy == "fixed" && cfg.Model.FixedModelId != nil:
		usable, err := s.models.ResolveForUse(ctx, *cfg.Model.FixedModelId)
		if err != nil {
			return out, err
		}
		if err := s.eligible(ctx, usable); err != nil {
			return out, err
		}
		out.chain = []modelcatalog.Usable{usable}
	case cfg.Model.Policy == "tiered":
		if s.router == nil {
			return out, invalid("agent version dùng model.policy tiered nhưng Model Router chưa được cấu hình ở môi trường này")
		}
		var want *string
		if cfg.Model.DefaultTier != nil {
			t := string(*cfg.Model.DefaultTier)
			want = &t
		}
		// Sàn của cửa sổ ngữ cảnh: phần trả lời tối đa cộng một khoảng tối thiểu cho hội
		// thoại. Ước lượng chính xác theo từng lượt cần đếm token, chưa làm.
		route, err := s.router.Route(ctx, projectID, want, len(cfg.Tools) > 0, out.maxOut+minPromptTokens)
		if err != nil {
			return out, err
		}
		out.tier = route.Tier
		out.skipped = route.Skipped
		for _, u := range route.Models {
			// Ràng buộc của môi trường chạy: adapter, khoá của provider, giá hiệu lực.
			if err := s.eligible(ctx, u); err != nil {
				e, ok := apperr.From(err)
				if !ok || e.Kind != apperr.KindUnprocessable {
					return out, err
				}
				out.skipped = append(out.skipped, u.ID.String()+": "+e.Message)
				continue
			}
			out.chain = append(out.chain, u)
		}
		if len(out.chain) == 0 {
			return out, invalid("không có model nào của tier " + out.tier + " dùng được (" + strings.Join(out.skipped, "; ") + ")")
		}
		if len(out.chain) > maxModelChain {
			out.chain = out.chain[:maxModelChain]
		}
	default:
		return out, invalid("model.policy của agent version không hợp lệ")
	}
	out.timeout = time.Duration(cfg.Limits.TimeoutSeconds) * time.Second
	if out.timeout <= 0 || cfg.Limits.MaxIterations <= 0 {
		return out, invalid("agent version thiếu giới hạn timeout / max_iterations")
	}
	return out, nil
}

// budgetFloor từ chối một budget nhỏ hơn trần chi phí của riêng phần trả lời một lượt:
// lượt đầu tiên chắc chắn bị chặn, nên báo ngay thay vì nhận execution rồi dừng mà chưa
// làm gì.
func (s *Service) budgetFloor(ctx context.Context, p prepared, amount string) error {
	price, err := s.prices.PriceAt(ctx, p.chain[0].ID, time.Now())
	if err != nil {
		return err
	}
	if limit, ok := budget.ParseAmount(amount); ok && limit < budget.Estimate(price, 0, p.maxOut) {
		return invalid("budget của task (limits.budget của agent version, hoặc plan.tasks[].budget) nhỏ hơn trần chi phí của một lượt gọi model (max_output_tokens × giá output)")
	}
	return nil
}

// checkBudgets: không nhận execution mới khi một budget chặn cứng của phạm vi đã chạm
// ngưỡng (cost-management §4.3).
func (s *Service) checkBudgets(ctx context.Context, orgID, projectID uuid.UUID, p identity.Principal, agentIDs ...uuid.UUID) error {
	if s.budgets == nil {
		return nil
	}
	seen := map[uuid.UUID]bool{}
	for _, id := range agentIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		scope := BudgetScope{OrgID: orgID, ProjectID: projectID, AgentID: id}
		if p.Type == identity.PrincipalUser {
			scope.UserID = &p.ID
		}
		if err := s.budgets.CheckNew(ctx, scope); err != nil {
			return err
		}
	}
	return nil
}

// admitInTx áp trần execution đồng thời của org khi Runner không tự giữ chỗ trong bộ
// nhớ (Temporal: nhiều process api, vòng lặp chạy ở worker): trần được đếm trong
// database, dưới một khoá theo org để các request đồng thời không cùng lọt qua.
//
// Đơn vị của trần là vòng lặp Agent chạy đồng thời: slots là số suất execution mới
// chiếm (1 với execution đơn Agent; số task chạy song song với execution planned).
func (s *Service) admitInTx(ctx context.Context, q *store.Queries, orgID uuid.UUID, slots int) error {
	lim, ok := s.runner.(OrgLimiter)
	if !ok {
		return nil
	}
	if err := q.LockOrgAdmission(ctx, orgID); err != nil {
		return err
	}
	used, err := q.CountActiveSlots(ctx, orgID)
	if err != nil {
		return err
	}
	if used+int64(slots) > int64(lim.OrgLimit()) {
		return errBusy
	}
	return nil
}

func traceIDOf(ctx context.Context) *string {
	if sc := trace.SpanContextFromContext(ctx); sc.HasTraceID() {
		t := sc.TraceID().String()
		return &t
	}
	return nil
}

// Create kiểm tra yêu cầu, ghi execution + task ở trạng thái queued rồi chạy vòng
// lặp ở nền. Trả về ngay; người gọi theo dõi bằng Get.
func (s *Service) Create(ctx context.Context, projectID uuid.UUID, in CreateInput) (Execution, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	p, pok := identity.PrincipalFrom(ctx)
	if !ok || !pok {
		return Execution{}, internal(tenancy.ErrNoTenant)
	}
	if s.runner == nil {
		return Execution{}, apperr.New(apperr.KindUnavailable, "runtime_unavailable", "process này chưa được cấu hình để chạy execution")
	}
	if in.Mode != ModeSingleAgent && in.Mode != ModePlanned {
		return Execution{}, invalid("mode: phase này chỉ hỗ trợ single_agent và planned")
	}
	if in.Message == "" || utf8.RuneCountInString(in.Message) > maxMessageRunes || !utf8.ValidString(in.Message) ||
		strings.ContainsRune(in.Message, 0) {
		return Execution{}, invalid("input.message phải có từ 1 đến 100000 ký tự")
	}
	project, err := s.projects.GetProject(ctx, projectID)
	if err != nil {
		return Execution{}, err
	}
	if project.ArchivedAt != nil {
		return Execution{}, apperr.New(apperr.KindConflict, "project_archived", "project đã được lưu trữ")
	}
	if in.Mode == ModePlanned {
		return s.createPlanned(ctx, orgID, p, projectID, in)
	}
	if in.Plan != nil {
		return Execution{}, invalid("plan chỉ dùng với mode planned")
	}
	if in.Budget != nil {
		return Execution{}, invalid("budget chỉ dùng với mode planned; execution đơn Agent dùng limits.budget của agent version")
	}
	if in.AgentID == nil {
		return Execution{}, invalid("agent_id là bắt buộc với mode single_agent")
	}
	prep, err := s.prepare(ctx, projectID, *in.AgentID, in.AgentVersion, in.MayRunAnyVersion)
	if err != nil {
		return Execution{}, err
	}
	run, cfg := prep.run, prep.run.Config
	// Lần lập kế hoạch: điều kiện của Agent lập kế hoạch được kiểm trên ĐÚNG version sắp
	// chạy (version đang hoạt động có thể đã đổi từ lúc ProposePlan kiểm).
	if in.proposal != nil && !plannerEligible(run) {
		return Execution{}, errPlannerTools
	}
	timeout := prep.timeout
	if in.TimeoutSeconds != nil {
		// Yêu cầu chỉ được đặt hạn chặt hơn Agent Version, không nới ra.
		if req := time.Duration(*in.TimeoutSeconds) * time.Second; req <= 0 || req > timeout {
			return Execution{}, invalid("timeout_seconds phải dương và không vượt giới hạn của agent version")
		}
		timeout = time.Duration(*in.TimeoutSeconds) * time.Second
	}
	// Budget của chính execution là limits.budget của Agent Version, chụp vào execution
	// và task bên dưới.
	if err := s.checkBudgets(ctx, orgID, projectID, p, run.Agent.ID); err != nil {
		return Execution{}, err
	}
	var budgetLimit *string
	if b := cfg.Limits.Budget; b != nil && b.Currency == "USD" {
		budgetLimit = &b.Amount
		if err := s.budgetFloor(ctx, prep, b.Amount); err != nil {
			return Execution{}, err
		}
	}
	chainIDs := prep.chainIDs()
	delegation, err := s.buildDelegation(ctx, projectID, []prepared{prep})
	if err != nil {
		return Execution{}, err
	}
	// Giữ chỗ trước khi ghi: không có execution queued nào mà không ai chạy.
	if err := s.runner.Admit(orgID); err != nil {
		return Execution{}, err
	}
	started := false
	defer func() {
		if !started {
			s.runner.Abort(orgID)
		}
	}()

	deadline := time.Now().Add(timeout)
	execID, taskID := ids.New(), ids.New()
	var row store.AgentExecution
	err = tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) error {
		q := store.New(tx)
		if err := s.admitInTx(ctx, q, orgID, 1); err != nil {
			return err
		}
		var err error
		row, err = q.InsertExecution(ctx, store.InsertExecutionParams{
			ID: execID, OrgID: orgID, ProjectID: projectID, InitiatorType: p.Type, InitiatorID: &p.ID,
			Mode: in.Mode, Input: mustJSON(map[string]string{"message": in.Message}), TraceID: traceIDOf(ctx), DeadlineAt: deadline,
			BudgetLimit: budgetLimit,
		})
		if err != nil {
			return err
		}
		if err := delegation.insert(ctx, q, orgID, execID); err != nil {
			return err
		}
		if m := in.proposal; m != nil {
			if err := q.InsertPlanProposal(ctx, store.InsertPlanProposalParams{OrgID: orgID, ExecutionID: execID, ProjectID: projectID,
				PlannerAgentID: m.PlannerAgentID, AgentIds: m.AgentIDs, RequestedByType: p.Type, RequestedBy: p.ID}); err != nil {
				return err
			}
		}
		if err := q.InsertTask(ctx, store.InsertTaskParams{ID: taskID, OrgID: orgID, ExecutionID: execID,
			AgentID: run.Agent.ID, AgentVersionID: run.Version.ID, Goal: in.Message, SystemPrompt: &run.Version.SystemPrompt, ModelChain: chainIDs, BudgetLimit: budgetLimit}); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{
			OrgID: orgID, ProjectID: &projectID, Action: "execution.create", ResourceType: "execution", ResourceID: &execID, ExecutionID: &execID,
			Outcome: audit.OutcomeSuccess,
			Details: map[string]string{"agent_id": run.Agent.ID.String(), "agent_version": strconv.Itoa(run.Version.Version), "mode": in.Mode,
				// Quyết định của Model Router: tier, chuỗi model đã chọn và model bị loại.
				"model_tier": prep.tier, "model_chain": joinIDs(chainIDs), "models_skipped": truncate(strings.Join(prep.skipped, "; "), 1000),
				// Agent đích của delegation không dùng được lúc tạo: giao việc cho chúng sẽ bị từ chối.
				"delegation_targets": strconv.Itoa(len(delegation.Targets)), "delegation_unavailable": joinIDs(delegation.Unavailable),
				// Lần lập kế hoạch được ghi rõ là như vậy, kèm các Agent mà kế hoạch được dùng.
				"purpose": in.proposal.purpose(), "plan_agent_ids": joinIDs(in.proposal.agentIDs())}})
	})
	if errors.Is(err, errBusy) {
		return Execution{}, errBusy
	}
	if err != nil {
		return Execution{}, internal(err)
	}

	spec := prep.spec(TaskRef{OrgID: orgID, ProjectID: projectID, ExecutionID: execID, TaskID: taskID,
		InitiatorType: p.Type, InitiatorID: p.ID, Deadline: deadline,
		// Lề cho lệch đồng hồ giữa process và database.
		Since: time.Now().Add(-10 * time.Minute)})
	started = true
	if err := s.runner.Start(ctx, spec); err != nil {
		// Execution đã được ghi nhưng không khởi chạy được: đóng nó lại thay vì để treo.
		s.abandon(ctx, spec.Task)
		return Execution{}, apperr.Wrap(err, apperr.KindUnavailable, "runtime_unavailable", "không khởi chạy được execution; thử lại sau").AsRetryable()
	}
	return executionFromRow(row), nil
}

// maxPlanTimeout là trần thời hạn của một execution chế độ planned.
const maxPlanTimeout = 2 * time.Hour

// Orchestration là thứ runtime cần từ cấu hình của project cho chế độ planned.
type Orchestration interface {
	PlanSettings(ctx context.Context, projectID uuid.UUID) (orchestrator.Settings, error)
}

// taskError gắn vị trí của task trong kế hoạch vào lỗi kiểm tra của nó.
func taskError(i int, err error) error {
	if e, ok := apperr.From(err); ok && e.Kind == apperr.KindUnprocessable {
		return invalid("plan.tasks[" + strconv.Itoa(i) + "]: " + e.Message)
	}
	return err
}

// createPlanned nhận một execution chế độ planned (agent-runtime §5): kế hoạch là dữ
// liệu, được kiểm toàn bộ trước khi ghi bất cứ thứ gì; kế hoạch không hợp lệ bị từ
// chối, không được sửa giúp. Mọi task được ghi ngay ở trạng thái queued cùng execution,
// với Agent Version, chuỗi model và budget chốt lúc này.
func (s *Service) createPlanned(ctx context.Context, orgID uuid.UUID, p identity.Principal, projectID uuid.UUID, in CreateInput) (Execution, error) {
	if in.AgentID != nil || in.AgentVersion != nil {
		return Execution{}, invalid("agent_id / agent_version không dùng với mode planned; Agent nằm trong từng task của plan")
	}
	if in.Plan == nil {
		return Execution{}, invalid("plan là bắt buộc với mode planned; để một agent lập kế hoạch đề xuất kế hoạch, dùng POST /v1/projects/{id}/plan-proposals")
	}
	if s.orchestration == nil {
		return Execution{}, apperr.New(apperr.KindUnprocessable, "planned_not_enabled", "chế độ planned chưa được bật cho project này")
	}
	// Chế độ planned phải được bật tường minh theo project (agent-runtime §5.1). Cấu
	// hình hỏng thì dừng, không hiểu thành "bật".
	settings, err := s.orchestration.PlanSettings(ctx, projectID)
	if err != nil {
		return Execution{}, err
	}
	if !settings.PlannedEnabled {
		return Execution{}, apperr.New(apperr.KindUnprocessable, "planned_not_enabled", "chế độ planned chưa được bật cho project này")
	}
	nodes, err := orchestrator.Validate(*in.Plan)
	if err != nil {
		return Execution{}, invalid(err.Error())
	}
	parallel := settings.Parallel()
	if in.Plan.MaxParallel > 0 {
		if in.Plan.MaxParallel > parallel {
			return Execution{}, invalid("plan.max_parallel_tasks vượt cấu hình của project (" + strconv.Itoa(parallel) + ")")
		}
		parallel = in.Plan.MaxParallel
	}

	// Từng task: Agent, chuỗi model, budget. Budget của task là phần kế hoạch cấp cho nó
	// (chỉ được chặt hơn limits.budget của Agent Version), hoặc limits.budget.
	// Trần chi phí của execution, nếu người gọi đặt.
	var execLimit int64
	var execBudget *string
	if in.Budget != nil {
		limit, ok := budget.ParseAmount(*in.Budget)
		if !ok || limit <= 0 {
			return Execution{}, invalid("budget phải là số tiền dương (USD)")
		}
		execLimit, execBudget = limit, in.Budget
	}
	n := len(in.Plan.Tasks)
	preps := make([]prepared, n)
	budgets := make([]*string, n)
	seconds := make([]int64, n)
	agentIDs := make([]uuid.UUID, n)
	var allocated int64
	agents := 0
	for i, t := range in.Plan.Tasks {
		// Nút chờ và nút approval không có Agent, model hay budget: chúng chỉ chiếm thời gian.
		if t.Kind == orchestrator.KindWait {
			seconds[i] = int64(t.WaitSeconds)
			continue
		}
		if t.Kind == orchestrator.KindApproval {
			seconds[i] = int64(t.Gate.TimeoutSeconds)
			continue
		}
		agents++
		prep, err := s.prepare(ctx, projectID, t.AgentID, t.AgentVersion, in.MayRunAnyVersion)
		if err != nil {
			return Execution{}, taskError(i, err)
		}
		preps[i], agentIDs[i], seconds[i] = prep, prep.run.Agent.ID, int64(prep.timeout/time.Second)
		// Thời hạn riêng của bước chỉ được chặt hơn thời hạn của Agent Version.
		if t.TimeoutSeconds > 0 {
			if int64(t.TimeoutSeconds) > seconds[i] {
				return Execution{}, invalid("plan.tasks[" + strconv.Itoa(i) + "]: thời hạn riêng vượt thời hạn của agent version (" + strconv.FormatInt(seconds[i], 10) + " giây)")
			}
			seconds[i] = int64(t.TimeoutSeconds)
		}
		var own *string
		if b := prep.run.Config.Limits.Budget; b != nil && b.Currency == "USD" {
			own = &b.Amount
		}
		if t.Budget != "" {
			want, ok := budget.ParseAmount(t.Budget)
			if !ok || want <= 0 {
				return Execution{}, invalid("plan.tasks[" + strconv.Itoa(i) + "].budget phải là số tiền dương (USD)")
			}
			if own != nil {
				if limit, ok := budget.ParseAmount(*own); ok && want > limit {
					return Execution{}, invalid("plan.tasks[" + strconv.Itoa(i) + "].budget vượt limits.budget của agent version")
				}
			}
			amount := t.Budget
			own = &amount
		}
		// Mọi task của một execution nhiều Agent phải có trần chi phí (roadmap: multi-agent
		// không có trần chi phí là rủi ro tài chính thật).
		if own == nil {
			return Execution{}, invalid("plan.tasks[" + strconv.Itoa(i) + "] không có budget: mỗi task của mode planned cần budget (plan.tasks[].budget hoặc limits.budget của agent version)")
		}
		if err := s.budgetFloor(ctx, prep, *own); err != nil {
			return Execution{}, taskError(i, err)
		}
		v, ok := budget.ParseAmount(*own)
		if !ok || v <= 0 {
			return Execution{}, invalid("plan.tasks[" + strconv.Itoa(i) + "]: budget của task không hợp lệ")
		}
		// Tổng phần cấp cho các task không vượt budget của execution. So sánh theo phần
		// còn lại, không cộng trước: tổng của nhiều số lớn có thể tràn int64 và lọt qua.
		if execBudget != nil {
			if v > execLimit-allocated {
				return Execution{}, invalid("tổng budget của các task vượt budget của execution")
			}
			allocated += v
		}
		budgets[i] = own
	}
	// Hạn của execution: thời gian kế hoạch cần khi mọi task dùng hết hạn của nó, với
	// đúng số task được chạy song song.
	timeout := time.Duration(orchestrator.Makespan(nodes, seconds, parallel)) * time.Second
	if timeout > maxPlanTimeout {
		timeout = maxPlanTimeout
	}
	if in.TimeoutSeconds != nil {
		req := time.Duration(*in.TimeoutSeconds) * time.Second
		if req <= 0 || req > timeout {
			return Execution{}, invalid("timeout_seconds phải dương và không vượt tổng thời hạn của kế hoạch (" + strconv.Itoa(int(timeout/time.Second)) + " giây)")
		}
		timeout = req
	}
	if agents == 0 {
		return Execution{}, invalid("kế hoạch phải có ít nhất một bước giao cho agent")
	}
	// Từ đây agentIDs chỉ gồm Agent của các nút Agent (nút chờ không có).
	used := make([]uuid.UUID, 0, agents)
	roots := make([]prepared, 0, agents)
	for i, t := range in.Plan.Tasks {
		if !orchestrator.IsStep(t.Kind) {
			used = append(used, agentIDs[i])
			roots = append(roots, preps[i])
		}
	}
	if err := s.checkBudgets(ctx, orgID, projectID, p, used...); err != nil {
		return Execution{}, err
	}
	delegation, err := s.buildDelegation(ctx, projectID, roots)
	if err != nil {
		return Execution{}, err
	}

	if err := s.runner.Admit(orgID); err != nil {
		return Execution{}, err
	}
	started := false
	defer func() {
		if !started {
			s.runner.Abort(orgID)
		}
	}()

	workflowID := ""
	if in.WorkflowExecutionID != nil {
		workflowID = in.WorkflowExecutionID.String()
	}
	planSource, proposalID := PlanSourceCaller, ""
	if in.planFrom != nil {
		planSource, proposalID = PlanSourceModel, in.planFrom.String()
	}
	deadline := time.Now().Add(timeout)
	execID := ids.New()
	taskIDs := make([]uuid.UUID, n)
	stored := storedPlan{MaxParallelTasks: parallel, Tasks: make([]storedTask, n)}
	for i, t := range in.Plan.Tasks {
		if orchestrator.IsStep(t.Kind) {
			stored.Tasks[i] = storedTask{Key: t.Key, Kind: t.Kind, WaitSeconds: t.WaitSeconds, DependsOn: t.DependsOn, OnFailure: nodes[i].OnFailure}
			continue
		}
		taskIDs[i] = ids.New()
		stored.Tasks[i] = storedTask{Key: t.Key, TaskID: taskIDs[i], AgentID: agentIDs[i], DependsOn: t.DependsOn, OnFailure: nodes[i].OnFailure,
			RetryOf: t.RetryOf}
	}
	// Dấu vết của từng task: Agent Version, tier / chuỗi model đã chọn và budget của nó.
	planAudit := make([]string, n)
	for i, t := range in.Plan.Tasks {
		if t.Kind == orchestrator.KindWait {
			planAudit[i] = t.Key + ": wait=" + strconv.Itoa(t.WaitSeconds) + "s"
			continue
		}
		if t.Kind == orchestrator.KindApproval {
			planAudit[i] = t.Key + ": approval required=" + strconv.Itoa(t.Gate.Required) + " timeout=" + strconv.Itoa(t.Gate.TimeoutSeconds) + "s"
			continue
		}
		planAudit[i] = t.Key + ": agent=" + agentIDs[i].String() + " version=" + strconv.Itoa(preps[i].run.Version.Version) +
			" tier=" + preps[i].tier + " models=" + joinIDs(preps[i].chainIDs()) + " budget=" + *budgets[i]
	}
	var row store.AgentExecution
	err = tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) error {
		q := store.New(tx)
		if err := s.admitInTx(ctx, q, orgID, min(parallel, agents)); err != nil {
			return err
		}
		var err error
		row, err = q.InsertExecution(ctx, store.InsertExecutionParams{
			ID: execID, OrgID: orgID, ProjectID: projectID, InitiatorType: p.Type, InitiatorID: &p.ID,
			Mode: in.Mode, Input: mustJSON(map[string]string{"message": in.Message}), TraceID: traceIDOf(ctx), DeadlineAt: deadline,
			BudgetLimit: execBudget, Plan: mustJSON(stored), PlanSource: &planSource, WorkflowExecutionID: in.WorkflowExecutionID,
		})
		if err != nil {
			return err
		}
		if err := delegation.insert(ctx, q, orgID, execID); err != nil {
			return err
		}
		for i, t := range in.Plan.Tasks {
			if orchestrator.IsStep(t.Kind) {
				config := mustJSON(map[string]int{"seconds": t.WaitSeconds})
				if t.Kind == orchestrator.KindApproval {
					config = mustJSON(map[string]any{"message": t.Gate.Message, "required_approvals": t.Gate.Required, "timeout_seconds": t.Gate.TimeoutSeconds})
				}
				if err := q.InsertPlanStep(ctx, store.InsertPlanStepParams{OrgID: orgID, ExecutionID: execID, PlanKey: t.Key,
					Kind: t.Kind, Config: config}); err != nil {
					return err
				}
				continue
			}
			// depends_on của task chỉ gồm các task của Agent: đó là những bước nó thấy kết quả.
			deps := make([]uuid.UUID, 0, len(nodes[i].DependsOn))
			for _, j := range nodes[i].DependsOn {
				if !orchestrator.IsStep(in.Plan.Tasks[j].Kind) {
					deps = append(deps, taskIDs[j])
				}
			}
			key, onFailure := t.Key, nodes[i].OnFailure
			if err := q.InsertTask(ctx, store.InsertTaskParams{ID: taskIDs[i], OrgID: orgID, ExecutionID: execID,
				AgentID: agentIDs[i], AgentVersionID: preps[i].run.Version.ID, Goal: t.Goal, SystemPrompt: &preps[i].run.Version.SystemPrompt,
				ModelChain: preps[i].chainIDs(), BudgetLimit: budgets[i], PlanKey: &key, DependsOn: deps, OnFailure: &onFailure}); err != nil {
				return err
			}
		}
		return audit.Record(ctx, tx, audit.Event{
			OrgID: orgID, ProjectID: &projectID, Action: "execution.create", ResourceType: "execution", ResourceID: &execID, ExecutionID: &execID,
			Outcome: audit.OutcomeSuccess,
			Details: map[string]string{"mode": in.Mode, "tasks": strconv.Itoa(n), "max_parallel_tasks": strconv.Itoa(parallel),
				"agents": joinIDs(used), "plan": truncate(strings.Join(planAudit, "; "), 4000), "workflow_execution_id": workflowID,
				// Kế hoạch do ai viết: người gọi, hay model (và từ đề xuất nào).
				"plan_source": planSource, "proposal_execution_id": proposalID,
				"delegation_targets": strconv.Itoa(len(delegation.Targets)), "delegation_unavailable": joinIDs(delegation.Unavailable)}})
	})
	if errors.Is(err, errBusy) {
		return Execution{}, errBusy
	}
	if err != nil {
		return Execution{}, internal(err)
	}

	execRef := TaskRef{OrgID: orgID, ProjectID: projectID, ExecutionID: execID, InitiatorType: p.Type, InitiatorID: p.ID,
		Deadline: deadline, Since: time.Now().Add(-10 * time.Minute)}
	plan := PlanSpec{Exec: execRef, MaxParallel: parallel, Tasks: make([]PlanTask, n)}
	for i, t := range in.Plan.Tasks {
		if orchestrator.IsStep(t.Kind) {
			// Nội dung của nút approval (thông điệp, số người duyệt) không đi qua nơi chạy kế
			// hoạch: nó nằm ở execution_plan_steps và được đọc ở đó.
			plan.Tasks[i] = PlanTask{Key: t.Key, Kind: t.Kind, WaitSeconds: t.WaitSeconds, Spec: Spec{Task: execRef},
				DependsOn: nodes[i].DependsOn, OnFailure: nodes[i].OnFailure}
			continue
		}
		ref := execRef
		ref.TaskID = taskIDs[i]
		plan.Tasks[i] = PlanTask{Key: t.Key, Spec: preps[i].spec(ref), DependsOn: nodes[i].DependsOn, OnFailure: nodes[i].OnFailure,
			RetryOf: nodes[i].RetryOf, TimeoutSeconds: int(seconds[i])}
	}
	started = true
	if err := s.runner.StartPlan(ctx, plan); err != nil {
		s.abandon(ctx, execRef)
		return Execution{}, apperr.Wrap(err, apperr.KindUnavailable, "runtime_unavailable", "không khởi chạy được execution; thử lại sau").AsRetryable()
	}
	return executionFromRow(row), nil
}

func active(status string) bool {
	return status == "queued" || status == "running" || status == StatusWaitingApproval || status == StatusPaused
}

// Get trả về một execution của tenant. Execution mồ côi (quá hạn mà vẫn "running" vì
// process chạy nó đã dừng) được đóng lại ở đây.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (Execution, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return Execution{}, internal(tenancy.ErrNoTenant)
	}
	var row store.AgentExecution
	err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) error {
		q := store.New(tx)
		var err error
		if row, err = q.GetExecution(ctx, store.GetExecutionParams{OrgID: orgID, ID: id}); err != nil {
			return err
		}
		if !active(row.Status) || !row.DeadlineAt.Before(time.Now().Add(-time.Minute)) {
			return nil
		}
		n, err := q.ExpireExecution(ctx, store.ExpireExecutionParams{OrgID: orgID, ID: id})
		if err != nil || n == 0 {
			return err
		}
		if err := q.FinishTasks(ctx, store.FinishTasksParams{OrgID: orgID, ExecutionID: id, Status: StatusTimedOut}); err != nil {
			return err
		}
		if err := q.CloseOpenPlanApprovals(ctx, id); err != nil {
			return err
		}
		if err := q.FinishOpenPlanSteps(ctx, store.FinishOpenPlanStepsParams{OrgID: orgID, ExecutionID: id, Status: StatusTimedOut}); err != nil {
			return err
		}
		if err := q.CloseOpenStepsOfExecution(ctx, store.CloseOpenStepsOfExecutionParams{OrgID: orgID, ExecutionID: id, Since: row.CreatedAt,
			Error: mustJSON(stepError{Code: CodeInterrupted, Message: "bước bị ngắt trước khi có kết quả"})}); err != nil {
			return err
		}
		if err := audit.Record(ctx, tx, audit.Event{OrgID: orgID, ProjectID: &row.ProjectID, Action: "execution.finish",
			ResourceType: "execution", ResourceID: &id, ExecutionID: &id, Outcome: audit.OutcomeError,
			Details: map[string]string{"status": StatusTimedOut, "error_code": "orphaned"}}); err != nil {
			return err
		}
		row, err = q.GetExecution(ctx, store.GetExecutionParams{OrgID: orgID, ID: id})
		return err
	})
	if errors.Is(err, pgxv5.ErrNoRows) {
		return Execution{}, errNotFound
	}
	if err != nil {
		return Execution{}, internal(err)
	}
	return executionFromRow(row), nil
}

// ForWorkflowRun trả về execution đã được tạo cho một lần chạy workflow, nếu có.
func (s *Service) ForWorkflowRun(ctx context.Context, workflowExecutionID uuid.UUID) (uuid.UUID, bool, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return uuid.Nil, false, internal(tenancy.ErrNoTenant)
	}
	var id uuid.UUID
	err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) (err error) {
		id, err = store.New(tx).GetExecutionIDForWorkflowRun(ctx, store.GetExecutionIDForWorkflowRunParams{OrgID: orgID, WorkflowExecutionID: &workflowExecutionID})
		return err
	})
	if errors.Is(err, pgxv5.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, internal(err)
	}
	return id, true, nil
}

// Cancel ghi yêu cầu huỷ. Vòng lặp dừng ở lần kiểm tra kế tiếp; trạng thái chỉ
// chuyển sang cancelled khi nó thực sự dừng.
func (s *Service) Cancel(ctx context.Context, id uuid.UUID) (Execution, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return Execution{}, internal(tenancy.ErrNoTenant)
	}
	var row store.AgentExecution
	err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) error {
		q := store.New(tx)
		n, err := q.RequestCancel(ctx, store.RequestCancelParams{OrgID: orgID, ID: id})
		if err != nil {
			return err
		}
		if row, err = q.GetExecution(ctx, store.GetExecutionParams{OrgID: orgID, ID: id}); err != nil {
			return err
		}
		if n == 0 {
			return errFinished
		}
		return audit.Record(ctx, tx, audit.Event{OrgID: orgID, ProjectID: &row.ProjectID, Action: "execution.cancel",
			ResourceType: "execution", ResourceID: &id, ExecutionID: &id, Outcome: audit.OutcomeSuccess})
	})
	switch {
	case errors.Is(err, pgxv5.ErrNoRows):
		return Execution{}, errNotFound
	case errors.Is(err, errFinished):
		return Execution{}, errFinished
	case err != nil:
		return Execution{}, internal(err)
	}
	// Execution bị huỷ: không hành động nào của nó còn được thực thi, kể cả hành động
	// đang chờ duyệt hoặc đã được duyệt nhưng chưa chạy.
	if s.approval != nil {
		if err := s.approval.CancelForExecution(ctx, id); err != nil {
			slog.ErrorContext(ctx, "runtime: không huỷ được approval của execution", "error", err, "execution_id", id)
		}
	}
	if s.runner != nil {
		s.runner.Cancel(ctx, TaskRef{OrgID: orgID, ProjectID: row.ProjectID, ExecutionID: id})
	}
	return executionFromRow(row), nil
}

// Pause ghi yêu cầu tạm dừng. Mỗi task của execution dừng ở ranh giới bước kế tiếp của
// nó (bước đang chạy được chạy xong); execution chỉ sang paused khi vòng lặp thực sự
// dừng. Thời gian tạm dừng vẫn tính vào hạn của execution. Idempotent: yêu cầu lặp lại
// không đổi gì, không ghi audit.
//
// Tạm dừng dựa vào sự hợp tác của vòng lặp: nó là công cụ vận hành, KHÔNG phải kiểm
// soát an toàn. Muốn chắc chắn không bước nào chạy nữa thì huỷ execution (Gateway tự
// thực thi yêu cầu huỷ).
func (s *Service) Pause(ctx context.Context, id uuid.UUID) (Execution, error) {
	return s.setPause(ctx, id, true)
}

// Resume gỡ yêu cầu tạm dừng và đánh thức các vòng lặp đang dừng. Idempotent.
func (s *Service) Resume(ctx context.Context, id uuid.UUID) (Execution, error) {
	return s.setPause(ctx, id, false)
}

func (s *Service) setPause(ctx context.Context, id uuid.UUID, pause bool) (Execution, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return Execution{}, internal(tenancy.ErrNoTenant)
	}
	p, ok := identity.PrincipalFrom(ctx)
	if !ok {
		return Execution{}, internal(tenancy.ErrNoTenant)
	}
	var row store.AgentExecution
	var open []uuid.UUID
	changed := false
	err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) error {
		q := store.New(tx)
		var n int64
		var err error
		action := "execution.resume"
		if pause {
			action = "execution.pause"
			by := p.Type
			n, err = q.RequestPause(ctx, store.RequestPauseParams{OrgID: orgID, ID: id, ByType: &by, ByID: &p.ID})
		} else {
			n, err = q.RequestResume(ctx, store.RequestResumeParams{OrgID: orgID, ID: id})
		}
		if err != nil {
			return err
		}
		if row, err = q.GetExecution(ctx, store.GetExecutionParams{OrgID: orgID, ID: id}); err != nil {
			return err
		}
		if !active(row.Status) {
			return errFinished
		}
		if n == 0 {
			return nil // đã ở đúng trạng thái được yêu cầu: không đổi gì, không audit, không signal
		}
		changed = true
		if !pause {
			if open, err = q.ListOpenTaskIDs(ctx, store.ListOpenTaskIDsParams{OrgID: orgID, ExecutionID: id}); err != nil {
				return err
			}
		}
		return audit.Record(ctx, tx, audit.Event{OrgID: orgID, ProjectID: &row.ProjectID, Action: action,
			ResourceType: "execution", ResourceID: &id, ExecutionID: &id, Outcome: audit.OutcomeSuccess})
	})
	switch {
	case errors.Is(err, pgxv5.ErrNoRows):
		return Execution{}, errNotFound
	case errors.Is(err, errFinished):
		return Execution{}, errFinished
	case err != nil:
		return Execution{}, internal(err)
	}
	if !pause && changed && s.runner != nil {
		s.runner.Resumed(ctx, orgID, id, open)
	}
	return executionFromRow(row), nil
}

// List liệt kê execution của một project, mới nhất trước.
func (s *Service) List(ctx context.Context, projectID uuid.UUID, status string, limit int, cursor string) (out []Execution, next string, err error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return nil, "", internal(tenancy.ErrNoTenant)
	}
	c, err := pagination.Decode(cursor, sortCreated)
	if err != nil {
		return nil, "", err
	}
	params := store.ListExecutionsParams{OrgID: orgID, ProjectID: projectID, RowLimit: int32(limit + 1)} //nolint:gosec // limit ≤ 200
	if status != "" {
		params.Status = &status
	}
	if c != nil {
		at, err := time.Parse(time.RFC3339Nano, c.Value)
		if err != nil {
			return nil, "", pagination.ErrInvalidCursor
		}
		params.AfterCreatedAt, params.AfterID = &at, &c.ID
	}
	var rows []store.AgentExecution
	if err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) (err error) {
		rows, err = store.New(tx).ListExecutions(ctx, params)
		return err
	}); err != nil {
		return nil, "", internal(err)
	}
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[limit-1]
		next = pagination.Encode(pagination.Cursor{Sort: sortCreated, Value: last.CreatedAt.UTC().Format(time.RFC3339Nano), ID: last.ID})
	}
	out = make([]Execution, len(rows))
	for i, r := range rows {
		out[i] = executionFromRow(r)
	}
	return out, next, nil
}

// Tasks liệt kê task của một execution.
func (s *Service) Tasks(ctx context.Context, executionID uuid.UUID) ([]Task, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return nil, internal(tenancy.ErrNoTenant)
	}
	var rows []store.ListTasksRow
	if err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) (err error) {
		rows, err = store.New(tx).ListTasks(ctx, store.ListTasksParams{OrgID: orgID, ExecutionID: executionID, RowLimit: maxTasksRead})
		return err
	}); err != nil {
		return nil, internal(err)
	}
	out := make([]Task, len(rows))
	for i, r := range rows {
		out[i] = Task{ID: r.ID, ExecutionID: r.ExecutionID, ParentTaskID: r.ParentTaskID, AgentID: r.AgentID,
			AgentVersion: int(r.AgentVersion), Depth: int(r.Depth), Goal: r.Goal, Status: r.Status, Output: r.Output, Error: r.Error,
			EstimatedCost: numeric(r.EstimatedCost), Currency: "USD", Iterations: int(r.Iterations), ToolCalls: int(r.ToolCalls),
			StartedAt: r.StartedAt, FinishedAt: r.FinishedAt, CreatedAt: r.CreatedAt,
			PlanKey: r.PlanKey, DependsOn: r.DependsOn, OnFailure: r.OnFailure, OutputSummary: r.OutputSummary, Budget: r.BudgetLimit}
	}
	return out, nil
}

// GetTask trả về một task của tenant.
func (s *Service) GetTask(ctx context.Context, id uuid.UUID) (Task, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return Task{}, internal(tenancy.ErrNoTenant)
	}
	var r store.GetTaskRow
	err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) (err error) {
		r, err = store.New(tx).GetTask(ctx, store.GetTaskParams{OrgID: orgID, ID: id})
		return err
	})
	if errors.Is(err, pgxv5.ErrNoRows) {
		return Task{}, errNotFound
	}
	if err != nil {
		return Task{}, internal(err)
	}
	return Task{ID: r.ID, ExecutionID: r.ExecutionID, ProjectID: r.ProjectID, ParentTaskID: r.ParentTaskID, AgentID: r.AgentID,
		AgentVersion: int(r.AgentVersion), Depth: int(r.Depth), Goal: r.Goal, Status: r.Status, Output: r.Output, Error: r.Error,
		EstimatedCost: numeric(r.EstimatedCost), Currency: "USD", Iterations: int(r.Iterations), ToolCalls: int(r.ToolCalls),
		StartedAt: r.StartedAt, FinishedAt: r.FinishedAt, CreatedAt: r.CreatedAt,
		PlanKey: r.PlanKey, DependsOn: r.DependsOn, OnFailure: r.OnFailure, OutputSummary: r.OutputSummary, Budget: r.BudgetLimit}, nil
}

// Steps liệt kê step của một task theo step_seq. afterSeq là step_seq cuối của trang trước.
func (s *Service) Steps(ctx context.Context, task Task, kind string, includeContent bool, limit, afterSeq int) (out []Step, next int, err error) {
	taskID := task.ID
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return nil, 0, internal(tenancy.ErrNoTenant)
	}
	params := store.ListStepsParams{OrgID: orgID, TaskID: taskID, AfterSeq: int32(afterSeq), Since: task.CreatedAt, IncludeContent: includeContent, RowLimit: int32(limit + 1)} //nolint:gosec // đã chặn ở handler
	if kind != "" {
		params.Kind = &kind
	}
	var rows []store.ListStepsRow
	if err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) (err error) {
		rows, err = store.New(tx).ListSteps(ctx, params)
		return err
	}); err != nil {
		return nil, 0, internal(err)
	}
	if len(rows) > limit {
		rows = rows[:limit]
		next = int(rows[limit-1].StepSeq)
	}
	out = make([]Step, len(rows))
	for i, r := range rows {
		st := Step{ID: r.ID, TaskID: r.TaskID, StepSeq: int(r.StepSeq), Kind: r.Kind, Status: r.Status, ModelID: r.ModelID,
			Routing: r.Routing, ToolID: r.ToolID, ToolName: r.ToolName, Request: r.Request, Response: r.Response, Error: r.Error, CreatedAt: r.CreatedAt}
		if r.DurationMs != nil {
			d := int(*r.DurationMs)
			st.DurationMs = &d
		}
		out[i] = st
	}
	return out, next, nil
}

// TaskReader cho Gateway biết một task còn chạy hay không.
type TaskReader struct{ db *pgxpool.Pool }

// NewTaskReader tạo TaskReader.
func NewTaskReader(db *pgxpool.Pool) *TaskReader { return &TaskReader{db: db} }

// ActiveTask là một task đang chạy, chưa bị yêu cầu huỷ và chưa quá hạn.
type ActiveTask struct {
	ExecutionID    uuid.UUID
	ProjectID      uuid.UUID
	AgentVersionID uuid.UUID
	InitiatorType  string
	InitiatorID    uuid.UUID
	// Iterations là số lượt model đã hoàn tất của task.
	Iterations int
	// ModelChain là các model task được phép gọi (chọn lúc tạo task). nil với task tạo
	// trước khi có Model Router.
	ModelChain []uuid.UUID
	AgentID    uuid.UUID
	// Deadline là hạn của execution.
	Deadline time.Time
	// BudgetLimit là budget của task (rỗng = không có); EstimatedCost là mức đã tiêu,
	// gồm cả phần các task con của nó (delegation) đã tiêu.
	BudgetLimit   string
	EstimatedCost string
	// Depth là độ sâu của task trong cây giao việc (0 = task gốc / task của kế hoạch).
	Depth int
	// ModelPlanned: task thuộc một kế hoạch do model đề xuất và api chạy thẳng từ đề xuất.
	ModelPlanned bool
}

// ErrTaskNotActive báo task không tồn tại trong tenant hoặc không còn chạy.
var ErrTaskNotActive = apperr.New(apperr.KindPermissionDenied, "task_not_active", "task không còn chạy")

// ActiveTask trả về task nếu nó còn chạy.
func (t *TaskReader) ActiveTask(ctx context.Context, taskID uuid.UUID) (ActiveTask, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return ActiveTask{}, internal(tenancy.ErrNoTenant)
	}
	var r store.GetActiveTaskRow
	err := tenancy.InTx(ctx, t.db, func(tx pgxv5.Tx) (err error) {
		r, err = store.New(tx).GetActiveTask(ctx, store.GetActiveTaskParams{OrgID: orgID, ID: taskID})
		return err
	})
	if errors.Is(err, pgxv5.ErrNoRows) {
		return ActiveTask{}, ErrTaskNotActive
	}
	if err != nil {
		return ActiveTask{}, internal(err)
	}
	out := ActiveTask{Deadline: r.DeadlineAt, ModelChain: r.ModelChain, AgentID: r.AgentID, BudgetLimit: r.BudgetLimit, EstimatedCost: r.EstimatedCost, ExecutionID: r.ExecutionID, ProjectID: r.ProjectID, AgentVersionID: r.AgentVersionID, InitiatorType: r.InitiatorType,
		Iterations: int(r.Iterations), Depth: int(r.Depth), ModelPlanned: r.ModelPlanned}
	if r.InitiatorID != nil {
		out.InitiatorID = *r.InitiatorID
	}
	return out, nil
}

// abandon đóng một execution vừa được ghi nhưng không khởi chạy được.
func (s *Service) abandon(ctx context.Context, task TaskRef) {
	ctx, cancel := finishCtx(ctx)
	defer cancel()
	errJSON := mustJSON(map[string]any{"type": "about:blank", "title": "không khởi chạy được execution", "status": 0, "code": "not_started"})
	err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) error {
		q := store.New(tx)
		if _, err := q.FinishExecution(ctx, store.FinishExecutionParams{OrgID: task.OrgID, ID: task.ExecutionID, Status: StatusFailed, Error: errJSON}); err != nil {
			return err
		}
		if err := q.FinishTasks(ctx, store.FinishTasksParams{OrgID: task.OrgID, ExecutionID: task.ExecutionID, Status: StatusFailed, Error: errJSON}); err != nil {
			return err
		}
		if err := q.CloseOpenPlanApprovals(ctx, task.ExecutionID); err != nil {
			return err
		}
		if err := q.FinishOpenPlanSteps(ctx, store.FinishOpenPlanStepsParams{OrgID: task.OrgID, ExecutionID: task.ExecutionID, Status: StatusFailed}); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{OrgID: task.OrgID, ProjectID: &task.ProjectID, Action: "execution.finish",
			ResourceType: "execution", ResourceID: &task.ExecutionID, ExecutionID: &task.ExecutionID, Outcome: audit.OutcomeError,
			Details: map[string]string{"status": StatusFailed, "error_code": "not_started"}})
	})
	if err != nil {
		slog.ErrorContext(ctx, "runtime: không đóng được execution không khởi chạy", "error", err, "execution_id", task.ExecutionID)
	}
}
