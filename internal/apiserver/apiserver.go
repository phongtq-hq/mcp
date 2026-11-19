// Package apiserver là composition root của REST Management API: nối các
// bounded context lại thành một http.Handler. Không chứa logic nghiệp vụ.
package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hqgroups/mcp/internal/agent/catalog"
	"github.com/hqgroups/mcp/internal/agent/orchestrator"
	"github.com/hqgroups/mcp/internal/agent/runtime"
	"github.com/hqgroups/mcp/internal/ai/modelcatalog"
	"github.com/hqgroups/mcp/internal/ai/provider"
	"github.com/hqgroups/mcp/internal/ai/provider/anthropic"
	"github.com/hqgroups/mcp/internal/ai/router"
	"github.com/hqgroups/mcp/internal/audit"
	"github.com/hqgroups/mcp/internal/cost/budget"
	"github.com/hqgroups/mcp/internal/cost/ledger"
	"github.com/hqgroups/mcp/internal/cost/pricing"
	mcpclient "github.com/hqgroups/mcp/internal/mcp/client"
	"github.com/hqgroups/mcp/internal/mcp/gateway"
	"github.com/hqgroups/mcp/internal/mcp/registry"
	"github.com/hqgroups/mcp/internal/platform/apikeys"
	"github.com/hqgroups/mcp/internal/platform/authz"
	"github.com/hqgroups/mcp/internal/platform/idempotency"
	"github.com/hqgroups/mcp/internal/platform/identity"
	"github.com/hqgroups/mcp/internal/platform/secrets"
	"github.com/hqgroups/mcp/internal/platform/tenancy"
	"github.com/hqgroups/mcp/internal/runtimewire"
	apperr "github.com/hqgroups/mcp/internal/shared/errors"
	"github.com/hqgroups/mcp/internal/shared/exectoken"
	"github.com/hqgroups/mcp/internal/shared/safehttp"
	"github.com/hqgroups/mcp/internal/workflow/approval"
	"github.com/hqgroups/mcp/internal/workflow/definitions"
	"github.com/hqgroups/mcp/internal/workflow/notify"
)

// Config là cấu hình của API server.
type Config struct {
	// Adapters là các adapter LLM mà process này có. Provider chỉ được đăng ký với
	// adapter trong danh sách này.
	Adapters []string
	// Providers là adapter LLM theo tên, dùng để chạy execution.
	Providers map[string]provider.Provider
	// GatewayURL và ExecutionTokenKey cho phép Agent Runtime gọi tool qua MCP Gateway
	// (ADR-0004). Thiếu một trong hai thì process này không chạy được execution.
	GatewayURL        string
	ExecutionTokenKey []byte
	// ServiceTokenKey ký service token gửi Gateway (uỷ thác discovery). Chỉ api và
	// gateway giữ khoá này; worker thì không. Rỗng thì discovery không dùng được.
	ServiceTokenKey []byte
	// BudgetCounter là bộ đếm budget cho adapter chạy trong process (chế độ
	// InProcessRuntime). nil: bộ đếm trong bộ nhớ của process này.
	BudgetCounter budget.Counter
	// WorkflowScheduler đồng bộ lịch chạy của workflow (Temporal Schedules). nil: `schedule`
	// của workflow bị từ chối.
	WorkflowScheduler definitions.Scheduler
	// ScheduleHost khởi chạy nơi nhận các lần lịch kích hoạt (worker của hàng đợi
	// api-internal, ADR-0015) và trao cho nó đường tạo lần chạy. nil cùng với
	// WorkflowScheduler nil: không có lần chạy theo lịch.
	ScheduleHost func(ScheduledRuns) error
	// InProcessRuntime cho phép chạy vòng lặp ngay trong process này khi Runner là nil.
	// Không bền vững qua restart: chỉ dành cho phát triển cục bộ và test.
	InProcessRuntime bool
	// SecretSealer cho phép lưu secret (chỉ mã hoá). nil thì api_key của provider bị từ chối.
	SecretSealer secrets.Sealer
	// Credentials kiểm khoá của provider trước khi lưu. nil = kiểm với provider thật.
	Credentials modelcatalog.Credentials
	// Runner khởi chạy vòng lặp ở nơi khác (Temporal). nil = chạy ngay trong process
	// này, không bền vững qua restart; chỉ dành cho phát triển cục bộ và test.
	Runner runtime.Runner
	// UpstreamPolicy là chính sách SSRF cho endpoint MCP do tenant đăng ký.
	UpstreamPolicy safehttp.Policy
}

// discoverer uỷ discovery của một MCP server ngoài cho Gateway: process api không gọi
// MCP server. Mỗi lần gọi dùng một service token chỉ áp dụng cho đúng server đó.
type discoverer struct {
	gw     *mcpclient.Gateway
	tokens *exectoken.Issuer
}

func (d discoverer) Discover(ctx context.Context, serverID uuid.UUID) (registry.Snapshot, error) {
	p, ok := identity.PrincipalFrom(ctx)
	if !ok {
		return registry.Snapshot{}, apperr.New(apperr.KindUnauthenticated, "unauthenticated", "thiếu thông tin xác thực")
	}
	token, err := d.tokens.SignService(exectoken.ServiceClaims{OrgID: p.OrgID, Purpose: gateway.PurposeDiscovery, Resource: serverID,
		ActorType: p.Type, ActorID: p.ID}, time.Now())
	if err != nil {
		return registry.Snapshot{}, apperr.Wrap(err, apperr.KindInternal, "internal_error", "")
	}
	body, status, err := d.gw.Discover(ctx, token, serverID)
	if err != nil {
		return registry.Snapshot{}, apperr.Wrap(err, apperr.KindUnavailable, "discovery_unavailable", "không gọi được Gateway để chạy discovery").AsRetryable()
	}
	if status != http.StatusOK {
		var problem struct {
			Code   string `json:"code"`
			Detail string `json:"detail"`
		}
		_ = json.Unmarshal(body, &problem)
		if status == http.StatusUnprocessableEntity && problem.Detail != "" {
			return registry.Snapshot{}, apperr.New(apperr.KindUnprocessable, problem.Code, problem.Detail)
		}
		if status == http.StatusTooManyRequests || status >= 500 {
			return registry.Snapshot{}, apperr.New(apperr.KindUnavailable, "discovery_unavailable", "Gateway không chạy được discovery lúc này").AsRetryable()
		}
		return registry.Snapshot{}, apperr.New(apperr.KindConflict, "discovery_failed", "server không chạy discovery được ở trạng thái hiện tại")
	}
	var wire registry.WireSnapshot
	if err := json.Unmarshal(body, &wire); err != nil {
		return registry.Snapshot{}, apperr.New(apperr.KindInternal, "internal_error", "")
	}
	// Không tin dữ liệu đã được Gateway kiểm: kiểm và tính lại hash ở đây.
	snap, err := registry.FromWire(wire)
	if err != nil {
		return registry.Snapshot{}, apperr.New(apperr.KindUnprocessable, "discovery_failed", "định nghĩa tool của server không hợp lệ: "+err.Error())
	}
	return snap, nil
}

// AdapterAnthropic là adapter có khoá nằm ở Gateway: process api không giữ khoá LLM
// nào, nên model dùng adapter này được gọi qua Gateway.
const AdapterAnthropic = anthropic.Name

// providerCredentials kiểm khoá với chính provider.
type providerCredentials struct{}

func (providerCredentials) Supports(adapter string) bool { return adapter == AdapterAnthropic }

func (providerCredentials) Verify(ctx context.Context, adapter, apiKey string) error {
	if adapter != AdapterAnthropic {
		return modelcatalog.ErrCredentialInvalid
	}
	err := anthropic.Verifier{}.Verify(ctx, apiKey)
	if errors.Is(err, anthropic.ErrInvalidKey) {
		return modelcatalog.ErrCredentialInvalid
	}
	return err
}

// routingSettings đưa settings của organization và project cho Model Router.
type routingSettings struct{ t *tenancy.Service }

func (r routingSettings) OrganizationSettings(ctx context.Context) (json.RawMessage, error) {
	o, err := r.t.GetOrganization(ctx)
	return o.Settings, err
}

func (r routingSettings) ProjectSettings(ctx context.Context, projectID uuid.UUID) (json.RawMessage, error) {
	p, err := r.t.GetProject(ctx, projectID)
	return p.Settings, err
}

// budgetScopes kiểm phạm vi của một budget tồn tại trong organization của request.
type budgetScopes struct {
	projects *tenancy.Service
	agents   *catalog.Service
	users    *identity.ActiveChecker
}

func (b budgetScopes) ValidateScope(ctx context.Context, scopeType string, id uuid.UUID) error {
	missing := apperr.New(apperr.KindUnprocessable, "validation_failed", "scope_id không tồn tại trong organization này")
	var err error
	switch scopeType {
	case budget.ScopeProject:
		_, err = b.projects.GetProject(ctx, id)
	case budget.ScopeAgent:
		_, err = b.agents.Get(ctx, id)
	case budget.ScopeUser:
		orgID, ok := tenancy.OrgFrom(ctx)
		if !ok {
			return apperr.New(apperr.KindInternal, "internal_error", "")
		}
		if b.users.RequireActive(ctx, id, orgID) != nil {
			return missing // user của org khác, không tồn tại, hay đã bị vô hiệu hoá: cùng một câu trả lời
		}
		return nil
	default:
		return missing
	}
	if apperr.KindOf(err) == apperr.KindNotFound {
		return missing
	}
	return err
}

// executionBudgets nối Budget Management vào interface của runtime.
type executionBudgets struct{ b *budget.Service }

func (e executionBudgets) CheckNew(ctx context.Context, s runtime.BudgetScope) error {
	return e.b.CheckNew(ctx, budget.Scope{OrgID: s.OrgID, ProjectID: s.ProjectID, AgentID: s.AgentID, UserID: s.UserID})
}

// planSettings đọc cấu hình chế độ planned (khoá `orchestration`) của một project.
type planSettings struct{ t *tenancy.Service }

func (p planSettings) PlanSettings(ctx context.Context, projectID uuid.UUID) (orchestrator.Settings, error) {
	project, err := p.t.GetProject(ctx, projectID)
	if err != nil {
		return orchestrator.Settings{}, err
	}
	s, err := orchestrator.ParseSettings(project.Settings)
	if err != nil {
		// Cấu hình đã lưu bị hỏng: dừng, không hiểu thành "bật" hay "tắt".
		return orchestrator.Settings{}, apperr.New(apperr.KindUnprocessable, "orchestration_config_invalid", err.Error())
	}
	return s, nil
}

// settingsValidator kiểm các khoá của settings mà từng context sở hữu, trước khi ghi:
// model_routing (Model Router) và orchestration (Orchestrator).
func settingsValidator(routes *router.Service) func(ctx context.Context, settings json.RawMessage) (map[string]string, error) {
	return func(ctx context.Context, settings json.RawMessage) (map[string]string, error) {
		details, err := routes.ValidateSettings(ctx, settings)
		if err != nil {
			return nil, err
		}
		o, err := orchestrator.ParseSettings(settings)
		if err != nil {
			return nil, apperr.InvalidField("settings", err.Error())
		}
		if o.PlannedEnabled || o.PlannerAgentID != nil {
			if details == nil {
				details = map[string]string{}
			}
			details["orchestration_planned_enabled"] = strconv.FormatBool(o.PlannedEnabled)
			details["orchestration_max_parallel_tasks"] = strconv.Itoa(o.Parallel())
			// Đổi Agent lập kế hoạch là đổi thứ viết ra kế hoạch của cả project: luôn để lại dấu vết.
			if o.PlannerAgentID != nil {
				details["orchestration_planner_agent_id"] = o.PlannerAgentID.String()
			}
		}
		return details, nil
	}
}

// ScheduledRuns là đường tạo lần chạy cho một lần lịch kích hoạt.
type ScheduledRuns interface {
	StartScheduled(ctx context.Context, orgID, workflowID uuid.UUID, key string, final bool) error
}

// workflowRuns nối Agent Runtime vào interface của Workflow: một lần chạy workflow là
// một execution `planned` (ADR-0015).
type workflowRuns struct{ r *runtime.Service }

func (w workflowRuns) StartPlanned(ctx context.Context, projectID, workflowExecutionID uuid.UUID, note string, plan orchestrator.Plan) (uuid.UUID, error) {
	e, err := w.r.Create(ctx, projectID, runtime.CreateInput{Mode: runtime.ModePlanned, Message: note, Plan: &plan,
		WorkflowExecutionID: &workflowExecutionID})
	return e.ID, err
}

func (w workflowRuns) State(ctx context.Context, executionID uuid.UUID) (definitions.ExecutionState, error) {
	e, err := w.r.Get(ctx, executionID)
	if err != nil {
		return definitions.ExecutionState{}, err
	}
	return definitions.ExecutionState{Status: e.Status, Output: e.Output, Error: e.Error, StartedAt: e.StartedAt, FinishedAt: e.FinishedAt}, nil
}

func (w workflowRuns) Cancel(ctx context.Context, executionID uuid.UUID) error {
	_, err := w.r.Cancel(ctx, executionID)
	return err
}

func (w workflowRuns) ForRun(ctx context.Context, workflowExecutionID uuid.UUID) (uuid.UUID, bool, error) {
	return w.r.ForWorkflowRun(ctx, workflowExecutionID)
}

// scheduleActors dựng ngữ cảnh "chạy dưới quyền người đã kích hoạt workflow" cho lần
// chạy theo lịch: cùng các điều kiện mà middleware xác thực áp cho một request.
type scheduleActors struct {
	active *identity.ActiveChecker
	authz  *authz.Engine
}

func (a scheduleActors) As(ctx context.Context, orgID, userID uuid.UUID) (context.Context, error) {
	if err := a.active.RequireActive(ctx, userID, orgID); err != nil {
		return nil, err
	}
	ctx = tenancy.WithOrg(ctx, orgID)
	return identity.WithPrincipal(ctx, identity.Principal{Type: identity.PrincipalUser, ID: userID, OrgID: orgID}), nil
}

func (a scheduleActors) Require(ctx context.Context, permission string, projectID *uuid.UUID) error {
	return a.authz.Require(ctx, permission, projectID)
}

// modelRouter nối Model Router vào interface của runtime.
type modelRouter struct{ r *router.Service }

func (m modelRouter) Route(ctx context.Context, projectID uuid.UUID, tier *string, needTools bool, minContext int) (runtime.Route, error) {
	// Vòng lặp của Agent là purpose agent_step; Agent Version có thể đặt tier khác.
	want := router.TierFor(router.PurposeAgentStep)
	if tier != nil {
		want = *tier
	}
	d, err := m.r.Route(ctx, projectID, want, router.Requirements{ToolCalling: needTools, MinContextTokens: minContext})
	if err != nil {
		return runtime.Route{}, err
	}
	out := runtime.Route{Tier: d.Tier, Models: d.Models}
	for _, s := range d.Skipped {
		out.Skipped = append(out.Skipped, s.ModelID.String()+": "+s.Reason)
	}
	return out, nil
}

// New trả về handler cho mọi đường dẫn dưới /v1/. Mọi route đều yêu cầu xác thực.
// ctx là vòng đời của process: execution chạy nền dừng khi nó bị huỷ.
//
// Hàm wait trả về phải được gọi khi process dừng (sau khi ctx bị huỷ): nó chờ các
// execution nền ghi xong trạng thái kết thúc.
func New(ctx context.Context, db *pgxpool.Pool, verifier identity.TokenVerifier, cfg Config) (h http.Handler, wait func(context.Context), err error) {
	mux := http.NewServeMux()
	idem := idempotency.New(db)
	authorizer := authz.NewEngine(db)
	projects := tenancy.NewService(db)
	// Có đường tới Gateway thì dùng được adapter có khoá nằm ở đó.
	viaGateway := cfg.GatewayURL != "" && len(cfg.ExecutionTokenKey) > 0
	adapters := slices.Clone(cfg.Adapters)
	if viaGateway && !slices.Contains(adapters, AdapterAnthropic) {
		adapters = append(adapters, AdapterAnthropic)
	}
	models := modelcatalog.NewService(db, adapters)
	if cfg.SecretSealer != nil {
		creds := cfg.Credentials
		if creds == nil {
			creds = providerCredentials{}
		}
		models.WithCredentials(creds, secrets.NewWriter(cfg.SecretSealer))
	}
	prices := pricing.NewService(db, models)

	tenancy.RegisterRoutes(mux, projects, authorizer, idem.Wrap)
	modelcatalog.RegisterRoutes(mux, models, authorizer, prices, idem.Wrap)
	pricing.RegisterRoutes(mux, prices, authorizer, idem.Wrap)
	tools := registry.NewService(db)
	registry.RegisterRoutes(mux, tools, authorizer)
	// Agent Version được kiểm với Tool Registry và Model Catalog trước khi lưu.
	agents := catalog.NewService(db, projects, tools, models)
	catalog.RegisterRoutes(mux, agents, authorizer, idem.Wrap)
	// Tenant tự quản lý MCP server ngoài. Discovery được uỷ cho Gateway (bên dưới).
	writes := registry.Writes{Policy: cfg.UpstreamPolicy, Agents: agents}
	if cfg.SecretSealer != nil {
		writes.Secrets = secrets.NewWriter(cfg.SecretSealer)
	}

	// Vòng lặp Agent chạy trên Temporal (process worker) khi có cfg.Runner. Không có thì
	// chỉ khi cfg.InProcessRuntime được bật mới chạy ngay trong process này (phát triển
	// cục bộ, test); còn lại tạo execution trả 503. Cả hai cách, tool call và
	// lượt model có khoá ở Gateway đều đi qua Gateway bằng execution token; process api
	// không nối tới MCP server nào.
	// Model Router: ánh xạ tier → model nằm trong settings của organization / project,
	// nên chính router kiểm khoá model_routing khi settings được ghi.
	routes := router.NewService(routingSettings{projects}, models)
	projects.WithSettingsValidator(settingsValidator(routes))
	// Budget: api quản lý cấu hình và từ chối execution mới khi một budget chặn cứng đã
	// chạm ngưỡng; việc đặt trước / quyết toán từng lượt gọi nằm ở nơi gọi model.
	budgets := budget.NewService(db, budgetScopes{projects: projects, agents: agents, users: identity.NewActiveChecker(db)}, prices, nil)
	budget.RegisterRoutes(mux, budgets, authorizer, idem.Wrap)
	// Approval: api nhận quyết định của người duyệt và huỷ approval của execution bị huỷ;
	// Gateway là nơi tạo và tiêu thụ approval.
	approvals := approval.NewService(db)
	approval.RegisterRoutes(mux, approvals, authorizer, idem.Wrap)
	rc := runtime.Config{Agents: agents, Models: models, Projects: projects, Prices: prices, Adapters: adapters,
		Router: modelRouter{routes}, Budgets: executionBudgets{budgets}, Approvals: approvals,
		Orchestration: planSettings{projects}}
	wait = func(context.Context) {}
	if viaGateway {
		rc.RemoteAdapters = []string{AdapterAnthropic}
		switch {
		case cfg.Runner != nil:
			rc.Runner = cfg.Runner
		case cfg.InProcessRuntime:
			// Vòng lặp chạy trong process này (local, test): adapter trong process được
			// kiểm đủ budget, với bộ đếm trong bộ nhớ nếu không được trao bộ đếm dùng chung.
			counter := cfg.BudgetCounter
			if counter == nil {
				counter = budget.NewMemoryCounter()
			}
			steps, err := runtimewire.New(db, cfg.GatewayURL, cfg.ExecutionTokenKey, cfg.Providers, counter)
			if err != nil {
				return nil, nil, err
			}
			local := runtime.NewLocalRunner(ctx, steps)
			rc.Runner, wait = local, local.Wait
		}
	}
	// Discovery của MCP server ngoài được uỷ cho Gateway bằng service token, ký bằng
	// khoá riêng mà worker không giữ.
	if cfg.GatewayURL != "" && len(cfg.ServiceTokenKey) > 0 {
		gw, err := mcpclient.NewGateway(cfg.GatewayURL)
		if err != nil {
			return nil, nil, err
		}
		svc, err := exectoken.New(cfg.ServiceTokenKey)
		if err != nil {
			return nil, nil, err
		}
		writes.Discoverer = discoverer{gw: gw, tokens: svc}
	}
	tools.WithWrites(writes)
	registry.RegisterWriteRoutes(mux, tools, authorizer, idem.Wrap)
	executions := runtime.NewService(db, rc)
	// Quyết định của người duyệt đánh thức vòng lặp đang chờ nó.
	approvals.WithNotifier(executions)
	runtime.RegisterRoutes(mux, executions, authorizer, idem.Wrap)
	// Workflow do người dùng định nghĩa: mỗi lần chạy là một execution `planned`.
	workflows := definitions.NewService(db, projects, workflowRuns{executions}, cfg.WorkflowScheduler).
		WithActors(scheduleActors{active: identity.NewActiveChecker(db), authz: authorizer})
	definitions.RegisterRoutes(mux, workflows, authorizer, idem.Wrap)
	if cfg.ScheduleHost != nil {
		if err := cfg.ScheduleHost(workflows); err != nil {
			return nil, nil, err
		}
	}
	audit.RegisterRoutes(mux, db, authorizer, tenancy.OrgFrom)
	ledger.RegisterRoutes(mux, db, authorizer, projectExists{projects})
	// Webhook thông báo: api chỉ cấu hình và mã hoá khoá ký; gateway gửi.
	var hookSecrets notify.SecretWriter
	if cfg.SecretSealer != nil {
		hookSecrets = secrets.NewWriter(cfg.SecretSealer)
	}
	notify.RegisterRoutes(mux, notify.NewService(db, hookSecrets, cfg.UpstreamPolicy), authorizer)
	keys := apikeys.NewService(db, authorizer, projects)
	apikeys.RegisterRoutes(mux, keys, authorizer)
	return identity.NewAuthenticator(verifier, db).WithAPIKeys(keys).Middleware(mux), wait, nil
}

// projectExists nối Identity & Tenancy vào interface của báo cáo usage.
type projectExists struct{ p *tenancy.Service }

func (e projectExists) ProjectExists(ctx context.Context, id uuid.UUID) error {
	_, err := e.p.GetProject(ctx, id)
	return err
}
