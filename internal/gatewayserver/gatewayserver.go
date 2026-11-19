// Package gatewayserver là composition root của process gateway: nối xác thực,
// phân quyền, Registry và MCP Gateway thành một http.Handler. Không chứa logic
// nghiệp vụ.
package gatewayserver

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/hqgroups/mcp/internal/agent/catalog"
	"github.com/hqgroups/mcp/internal/agent/runtime"
	"github.com/hqgroups/mcp/internal/ai/modelcatalog"
	"github.com/hqgroups/mcp/internal/ai/provider"
	"github.com/hqgroups/mcp/internal/ai/provider/anthropic"
	"github.com/hqgroups/mcp/internal/cost/budget"
	"github.com/hqgroups/mcp/internal/cost/pricing"
	"github.com/hqgroups/mcp/internal/mcp/gateway"
	"github.com/hqgroups/mcp/internal/mcp/registry"
	"github.com/hqgroups/mcp/internal/platform/apikeys"
	"github.com/hqgroups/mcp/internal/platform/authz"
	"github.com/hqgroups/mcp/internal/platform/identity"
	"github.com/hqgroups/mcp/internal/platform/secrets"
	"github.com/hqgroups/mcp/internal/platform/tenancy"
	"github.com/hqgroups/mcp/internal/shared/exectoken"
	"github.com/hqgroups/mcp/internal/shared/safehttp"
	"github.com/hqgroups/mcp/internal/shared/signedctx"
	"github.com/hqgroups/mcp/internal/workflow/approval"
	"github.com/hqgroups/mcp/internal/workflow/notify"
)

const metadataPath = "/.well-known/oauth-protected-resource"

// Config là cấu hình của gateway.
type Config struct {
	// PublicURL là URL công khai của gateway (không có dấu / cuối). Dùng làm
	// định danh resource trong metadata OAuth. Rỗng thì không công bố metadata.
	PublicURL string
	// Issuer là authorization server phát token cho gateway.
	Issuer string
	// ExecutionTokenKey bật đường dành cho Agent nội bộ (execution token). Rỗng thì
	// endpoint nội bộ không tồn tại.
	ExecutionTokenKey []byte
	// ServiceTokenKey kiểm service token của process api (discovery). Phải khác
	// ExecutionTokenKey; rỗng thì route discovery nội bộ tắt.
	ServiceTokenKey []byte
	// SecretOpener cho phép Gateway giải mã secret của org (khoá LLM). nil thì chỉ
	// dùng được khoá của platform.
	SecretOpener secrets.Opener
	// PlatformLLMKeys là khoá LLM của platform theo adapter, cho provider phạm vi platform.
	PlatformLLMKeys map[string]string
	// LLMFactory dựng adapter LLM từ một khoá. nil = adapter thật.
	LLMFactory gateway.ProviderFactory
	// UpstreamPolicy là chính sách SSRF cho MCP server ngoài do tenant đăng ký.
	UpstreamPolicy safehttp.Policy
	// Limiter là rate limit dùng chung giữa các replica (Redis). nil = trong bộ nhớ của process.
	Limiter gateway.Limiter
	// BudgetCounter là bộ đếm budget dùng chung giữa các replica (Redis). nil = trong bộ
	// nhớ của process (chỉ đúng khi có một replica: phát triển cục bộ và test).
	BudgetCounter budget.Counter
}

// initiators kiểm người khởi tạo một execution còn được phục vụ: user còn hoạt động
// và còn là thành viên, hoặc API key chưa bị thu hồi và chưa hết hạn; org còn hoạt động.
type initiators struct {
	users *identity.ActiveChecker
	keys  *apikeys.Service
}

func (i initiators) RequireActive(ctx context.Context, p identity.Principal) error {
	if p.Type == identity.PrincipalUser {
		return i.users.RequireActive(ctx, p.ID, p.OrgID)
	}
	usable, err := i.keys.Usable(ctx, p.ID)
	if err != nil {
		return err
	}
	if !usable {
		return errors.New("gateway: API key của người khởi tạo không còn dùng được")
	}
	return i.users.RequireOrgActive(ctx, p.OrgID)
}

// Owner trả về con người đứng sau một principal: chính user, hoặc người tạo API key.
func (i initiators) Owner(ctx context.Context, p identity.Principal) (uuid.UUID, error) {
	if p.Type == identity.PrincipalUser {
		return p.ID, nil
	}
	return i.keys.Owner(ctx, p.ID)
}

// models nối Agent Catalog và Model Catalog vào interface của Gateway.
type models struct {
	agents *catalog.Service
	models *modelcatalog.Service
}

func (m models) AgentModel(ctx context.Context, versionID uuid.UUID) (gateway.AgentModel, error) {
	p, err := m.agents.FixedModel(ctx, versionID)
	return gateway.AgentModel{ModelID: p.ModelID, MaxOutputTokens: p.MaxOutputTokens, MaxIterations: p.MaxIterations}, err
}

func (m models) Route(ctx context.Context, modelID uuid.UUID) (gateway.ModelRoute, error) {
	u, err := m.models.ResolveForUse(ctx, modelID)
	if err != nil {
		return gateway.ModelRoute{}, err
	}
	return gateway.ModelRoute{Adapter: u.Adapter, ProviderModel: u.ProviderModel, Platform: u.PlatformProvider, SecretID: u.SecretID}, nil
}

// approvals nối Approval vào interface của Gateway.
type approvals struct{ s *approval.Service }

func (a approvals) Resolve(ctx context.Context, r gateway.ApprovalRequest) (gateway.ApprovalState, error) {
	st, err := a.s.Resolve(ctx, approval.Request{ProjectID: r.ProjectID, ExecutionID: r.ExecutionID, TaskID: r.TaskID, StepSeq: r.StepSeq,
		ToolID: r.ToolID, ToolName: r.ToolName, ServerName: r.ServerName, RiskLevel: r.RiskLevel, ArgsHash: r.ArgsHash,
		Arguments: r.Arguments, Redactions: r.Redactions, AgentID: r.AgentID, RequestedByType: r.RequestedByType,
		RequestedBy: r.RequestedBy, RequestedByOwner: r.RequestedByOwner, ExpiresAt: r.ExpiresAt})
	return gateway.ApprovalState{ID: st.ID, Status: st.Status, Granted: st.Granted}, err
}

// modelBudget nối Budget Management vào interface của Gateway.
type modelBudget struct{ b *budget.Service }

func (m modelBudget) Reserve(ctx context.Context, c gateway.BudgetCall) (func(context.Context, provider.Usage), error) {
	call := budget.Call{Scope: budget.Scope{OrgID: c.OrgID, ProjectID: c.ProjectID, AgentID: c.AgentID, UserID: c.UserID},
		ModelID: c.ModelID, InputBytes: c.InputBytes, MaxOutputTokens: c.MaxOutputTokens}
	// Một task chỉ có một lượt gọi đang chạy cho mỗi model: lần gọi lại (sau khi lần
	// trước chết giữa chừng) thay thế phần đặt trước cũ thay vì giữ chỗ hai lần.
	call.Key = c.TaskID.String() + ":" + c.ModelID.String()
	if c.TaskLimit != "" {
		call.Task = &budget.TaskBudget{ID: c.TaskID, Limit: c.TaskLimit, Spent: c.TaskSpent}
	}
	res, err := m.b.Reserve(ctx, call)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, u provider.Usage) {
		m.b.Settle(ctx, res, budget.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
			CachedInputTokens: u.CachedInputTokens, CacheWriteTokens: u.CacheWriteTokens, ReasoningTokens: u.ReasoningTokens})
	}, nil
}

func defaultLLMFactory(adapter, apiKey string) (provider.Provider, error) {
	if adapter != anthropic.Name {
		return nil, fmt.Errorf("gateway: adapter %q không có ở process này", adapter)
	}
	return anthropic.New(anthropic.Config{APIKey: apiKey})
}

type tasks struct{ r *runtime.TaskReader }

func (t tasks) ActiveTask(ctx context.Context, id uuid.UUID) (gateway.ActiveTask, error) {
	a, err := t.r.ActiveTask(ctx, id)
	return gateway.ActiveTask{ExecutionID: a.ExecutionID, ProjectID: a.ProjectID, AgentVersionID: a.AgentVersionID,
		InitiatorType: a.InitiatorType, InitiatorID: a.InitiatorID, Iterations: a.Iterations, ModelChain: a.ModelChain,
		AgentID: a.AgentID, BudgetLimit: a.BudgetLimit, EstimatedCost: a.EstimatedCost, Deadline: a.Deadline, Depth: a.Depth, ModelPlanned: a.ModelPlanned}, err
}

type agentTools struct{ c *catalog.Service }

func (a agentTools) ToolRules(ctx context.Context, versionID uuid.UUID) ([]gateway.ToolRule, int, error) {
	rules, maxToolCalls, err := a.c.ToolRules(ctx, versionID)
	if err != nil {
		return nil, 0, err
	}
	out := make([]gateway.ToolRule, len(rules))
	for i, r := range rules {
		out[i] = gateway.ToolRule{ServerID: r.ServerID, Allow: r.Allow, Deny: r.Deny, RequireApproval: r.RequireApproval}
	}
	return out, maxToolCalls, nil
}

// Register gắn endpoint MCP và metadata OAuth vào mux.
func Register(mux *http.ServeMux, db *pgxpool.Pool, verifier identity.TokenVerifier, signer *signedctx.Signer, cfg Config) error {
	g := gateway.New(db, registry.NewService(db), authz.NewEngine(db), tenancy.NewService(db), signer)
	if cfg.Limiter != nil {
		g.SetLimiter(cfg.Limiter)
	}
	inner := http.NewServeMux()
	g.RegisterRoutes(inner)
	// Gateway không tạo user: người chưa từng đăng nhập vào platform không có quyền gì.
	keys := apikeys.NewService(db, nil, nil)
	h := identity.NewAuthenticator(verifier, db).WithoutProvisioning().WithAPIKeys(keys).Middleware(inner)
	if cfg.PublicURL != "" {
		mux.Handle(metadataPath, auth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
			Resource:               cfg.PublicURL,
			AuthorizationServers:   []string{cfg.Issuer},
			BearerMethodsSupported: []string{"header"},
		}))
		h = challenge(h, cfg.PublicURL+metadataPath)
	}
	mux.Handle("/mcp/", h)

	if len(cfg.ServiceTokenKey) > 0 {
		if subtle.ConstantTimeCompare(cfg.ServiceTokenKey, cfg.ExecutionTokenKey) == 1 {
			return errors.New("gatewayserver: khoá service token không được trùng khoá execution token")
		}
		svc, err := exectoken.New(cfg.ServiceTokenKey)
		if err != nil {
			return err
		}
		g.EnableServiceTokens(svc)
	}
	if len(cfg.ExecutionTokenKey) > 0 {
		tokens, err := exectoken.New(cfg.ExecutionTokenKey)
		if err != nil {
			return err
		}
		agents := catalog.NewService(db, nil, nil, nil)
		g.EnableInternal(tokens, tasks{runtime.NewTaskReader(db)}, agentTools{agents}, initiators{users: identity.NewActiveChecker(db), keys: keys})
		// Tool cần duyệt: Gateway tạo approval và tiêu thụ nó ngay trước khi thực thi.
		g.EnableApprovals(approvals{approval.NewService(db)})
		// Đường gọi model: Gateway là process duy nhất giữ khoá của LLM provider.
		llm := gateway.LLMConfig{
			Models:       models{agents: agents, models: modelcatalog.NewService(db, []string{anthropic.Name})},
			PlatformKeys: cfg.PlatformLLMKeys, Factory: cfg.LLMFactory,
		}
		if llm.Factory == nil {
			llm.Factory = defaultLLMFactory
		}
		// Budget được thực thi ở nơi tiêu tiền. Bộ đếm dùng chung giữa các replica khi có
		// Redis; mất bộ đếm thì dựng lại từ usage ledger.
		counter := cfg.BudgetCounter
		if counter == nil {
			counter = budget.NewMemoryCounter()
		}
		catalog := modelcatalog.NewService(db, []string{anthropic.Name})
		llm.Budget = modelBudget{budget.NewService(db, nil, pricing.NewService(db, catalog), counter)}
		ext := gateway.External{Policy: cfg.UpstreamPolicy, Servers: registry.NewService(db)}
		if cfg.SecretOpener != nil {
			reader := secrets.NewReader(db, cfg.SecretOpener)
			llm.Secrets, ext.Secrets = reader, reader
		}
		// MCP server ngoài: gọi qua SSRF guard, kèm credential riêng của từng server.
		g.EnableExternal(ext)
		regMux := http.NewServeMux()
		g.RegisterRegistryRoutes(regMux)
		mux.Handle("/registry/internal/", regMux)
		g.EnableLLM(llm)
		llmMux := http.NewServeMux()
		g.RegisterLLMRoutes(llmMux)
		mux.Handle("/llm/internal/", llmMux)
		// Đường nội bộ không qua middleware OIDC: nó chỉ nhận execution token. Pattern
		// dài hơn "/mcp/" nên ServeMux chọn nó trước.
		internal := http.NewServeMux()
		g.RegisterInternalRoutes(internal)
		mux.Handle("/mcp/internal/", internal)
	}
	return nil
}

// challenge thêm WWW-Authenticate trỏ tới metadata vào phản hồi 401, để MCP client
// tìm được authorization server (RFC 9728).
func challenge(next http.Handler, metadataURL string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&challengeWriter{ResponseWriter: w, value: fmt.Sprintf("Bearer resource_metadata=%q", metadataURL)}, r)
	})
}

type challengeWriter struct {
	http.ResponseWriter
	value string
}

func (w *challengeWriter) WriteHeader(status int) {
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", w.value)
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *challengeWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// NewNotifier dựng bộ gửi thông báo webhook (security §4.7). Trả về nil khi process
// không có khoá mở secret: không ký được thì không gửi. Người gọi chạy Run trong một
// goroutine gắn với context của process.
func NewNotifier(db *pgxpool.Pool, cfg Config) *notify.Dispatcher {
	if cfg.SecretOpener == nil {
		return nil
	}
	return notify.NewDispatcher(db, secrets.NewReader(db, cfg.SecretOpener), cfg.UpstreamPolicy, nil)
}
