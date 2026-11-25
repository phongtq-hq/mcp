// Package gateway là MCP Gateway: điểm thực thi chính sách và proxy tới MCP server
// (mcp-platform §2, ADR-0004). Nó là MCP server đối với client và là MCP client đối
// với các server upstream. Mọi tool call của platform đi qua đây.
//
// Pipeline của một tools/call, đúng thứ tự:
//
//  1. AuthN        (middleware xác thực, trước handler này)
//  2. Tenant + rate limit
//  3. Resolve      tên tool → tool đã duyệt của version đang hoạt động; tool ngoài
//     tập được phép của principal thì không tồn tại đối với họ
//  4. Validate     arguments theo JSON Schema ĐÃ GHIM trong Registry
//  5. Policy       allow / deny / require_approval theo ma trận rủi ro (policy.go)
//  6. Credential   ngữ cảnh do Gateway ký cho managed server; KHÔNG chuyển tiếp token của client
//  7. Upstream     gọi với timeout của tool
//  8. Hậu xử lý    che secret, cắt theo max_result_bytes, ghi audit
//
// Gateway không chứa logic nghiệp vụ và không giữ trạng thái theo session.
package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/google/uuid"
	pgxv5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hqgroups/mcp/internal/audit"
	"github.com/hqgroups/mcp/internal/mcp/gateway/store"
	"github.com/hqgroups/mcp/internal/mcp/registry"
	"github.com/hqgroups/mcp/internal/platform/identity"
	"github.com/hqgroups/mcp/internal/platform/tenancy"
	apperr "github.com/hqgroups/mcp/internal/shared/errors"
	"github.com/hqgroups/mcp/internal/shared/exectoken"
	"github.com/hqgroups/mcp/internal/shared/httpx"
	"github.com/hqgroups/mcp/internal/shared/safehttp"
	"github.com/hqgroups/mcp/internal/shared/signedctx"
)

const (
	permProjectRead = "project:read"
	permToolCall    = "mcp_tool:call"

	maxArgumentBytes = 64 << 10
	maxRequestBytes  = 1 << 20
	// Trần phản hồi của upstream trước khi vào bộ nhớ của Gateway. Lớn hơn
	// max_result_bytes lớn nhất (4 MiB) để còn chỗ cho phần bao JSON.
	maxUpstreamBytes = 8 << 20
	auditTimeout     = 5 * time.Second
	defaultRateLimit = 120 // tool call và list mỗi phút cho mỗi principal
)

// Authorizer là thứ Gateway cần từ Authorization & Policy.
type Authorizer interface {
	Require(ctx context.Context, permission string, projectID *uuid.UUID) error
}

// Projects là thứ Gateway cần từ Identity & Tenancy.
type Projects interface {
	GetProject(ctx context.Context, id uuid.UUID) (tenancy.Project, error)
}

// Tools là thứ Gateway cần từ Registry.
type Tools interface {
	ActiveTools(ctx context.Context, f registry.ToolFilter) ([]registry.Tool, error)
}

// Gateway xử lý endpoint MCP của một project.
type Gateway struct {
	db       *pgxpool.Pool
	tools    Tools
	authz    Authorizer
	projects Projects
	signer   *signedctx.Signer
	// limits giới hạn tần suất theo org, principal và lớp request. Mặc định trong bộ
	// nhớ; production dùng Redis (SetLimiter) để các replica chia chung hạn mức.
	limits   Limiter
	upstream http.RoundTripper
	mcp      *mcp.StreamableHTTPHandler

	// Đường dành cho Agent nội bộ; nil nếu chưa bật (EnableInternal).
	tokens *exectoken.Issuer
	// approvals nil: tool cần duyệt bị từ chối.
	approvals Approvals
	// svcTokens kiểm service token (discovery do api uỷ thác). Khoá của nó KHÁC khoá
	// execution token: worker giữ khoá execution token nên không được ký service token.
	svcTokens *exectoken.Issuer
	tasks     Tasks
	agents    AgentTools
	users     Initiators
	llm       *LLMConfig
	ext       *External
	// extTransport là transport có bảo vệ SSRF, dùng chung cho mọi server ngoài.
	extTransport http.RoundTripper
}

// ExternalServers là thứ Gateway cần từ Registry để chạy discovery cho server ngoài.
type ExternalServers interface {
	ExternalEndpoint(ctx context.Context, serverID uuid.UUID) (registry.OrgServerEndpoint, error)
}

// External bật việc gọi MCP server NGOÀI do tenant đăng ký. Mọi kết nối tới chúng đi
// qua SSRF guard, và credential của server được Gateway giải mã từ Secrets.
type External struct {
	Policy  safehttp.Policy
	Secrets SecretReader // nil: chỉ gọi được server không cần credential
	Servers ExternalServers
}

// EnableExternal bật server ngoài.
func (g *Gateway) EnableExternal(e External) {
	g.ext = &e
	g.extTransport = e.Policy.Transport()
}

// bearerTransport gắn credential của MỘT server ngoài vào request tới server đó.
type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if b.token != "" {
		r.Header.Set("Authorization", "Bearer "+b.token)
	}
	return b.base.RoundTrip(r)
}

// externalClient dựng HTTP client cho một server ngoài: SSRF guard ở mỗi request và
// mỗi kết nối, không theo redirect, giới hạn kích thước phản hồi, kèm credential của
// server nếu có. Token của client và ngữ cảnh ký của platform không đi tới server ngoài.
func (g *Gateway) externalClient(ctx context.Context, authMethod string, secretID *uuid.UUID, purpose string) (*http.Client, error) {
	if g.ext == nil {
		return nil, errors.New("gateway: server ngoài chưa được bật")
	}
	token := ""
	switch authMethod {
	case "none":
	case "org_secret":
		if secretID == nil || g.ext.Secrets == nil {
			return nil, errors.New("gateway: server thiếu credential")
		}
		secret, err := g.ext.Secrets.Get(ctx, *secretID, purpose)
		if err != nil {
			return nil, err
		}
		token = string(secret)
	default:
		return nil, errors.New("gateway: cách xác thực của server chưa được hỗ trợ")
	}
	return &http.Client{
		Transport:     bearerTransport{token: token, base: g.ext.Policy.RoundTripper(g.extTransport, maxUpstreamBytes)},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

// ActiveTask là task mà một execution token nói tới, nếu nó còn chạy.
type ActiveTask struct {
	ExecutionID    uuid.UUID
	ProjectID      uuid.UUID
	AgentVersionID uuid.UUID
	InitiatorType  string
	InitiatorID    uuid.UUID
	// Iterations là số lượt model đã hoàn tất của task.
	Iterations int
	// Environment là môi trường của project của task; Deadline là hạn của task.
	Environment string
	Deadline    time.Time
	// ModelChain là các model task được phép gọi, chọn lúc tạo task. nil với task cũ:
	// khi đó chỉ model được ghim của Agent Version được phục vụ.
	ModelChain []uuid.UUID
	AgentID    uuid.UUID
	// Depth là độ sâu của task trong cây giao việc (0 = task gốc / task của kế hoạch).
	Depth int
	// ModelPlanned: task thuộc một kế hoạch do model đề xuất.
	ModelPlanned bool
	// BudgetLimit là budget của task (rỗng = không có); EstimatedCost là mức đã tiêu.
	BudgetLimit   string
	EstimatedCost string
}

// Initiators là thứ Gateway cần từ Identity: người khởi tạo execution còn được phục vụ.
type Initiators interface {
	// RequireActive trả lỗi nếu người khởi tạo (user hoặc API key) không còn được phục vụ.
	RequireActive(ctx context.Context, p identity.Principal) error
	// Owner trả về con người đứng sau principal: chính user, hoặc user đã tạo API key.
	Owner(ctx context.Context, p identity.Principal) (uuid.UUID, error)
}

// Tasks là thứ Gateway cần từ Agent Runtime: task còn chạy, chưa bị huỷ, chưa quá hạn.
type Tasks interface {
	ActiveTask(ctx context.Context, taskID uuid.UUID) (ActiveTask, error)
}

// ToolRule là một mục trong danh sách tool của Agent Version.
type ToolRule struct {
	ServerID uuid.UUID
	Allow    []string // tên tool, hoặc "*"
	Deny     []string
	// RequireApproval: Agent Version đặt require_approval: always cho các tool của mục này.
	RequireApproval bool
}

// ApprovalRequest là một tool call cần được duyệt trước khi chạy.
type ApprovalRequest struct {
	ProjectID, ExecutionID, TaskID, AgentID uuid.UUID
	StepSeq                                 int
	ToolID                                  uuid.UUID
	ToolName, ServerName, RiskLevel         string
	// ArgsHash là hash của arguments đã chuẩn hoá; Arguments là bản đã che secret cho
	// người duyệt xem.
	ArgsHash  []byte
	Arguments json.RawMessage
	// Redactions là số chỗ trong Arguments đã bị che: người duyệt phải biết mình không
	// thấy toàn bộ hành động.
	Redactions      int
	RequestedByType string
	RequestedBy     uuid.UUID
	// RequestedByOwner là con người đứng sau người yêu cầu (người tạo API key, hoặc chính
	// user): người này không được tự duyệt.
	RequestedByOwner uuid.UUID
	ExpiresAt        time.Time
}

// ApprovalState là trạng thái approval của một tool call.
type ApprovalState struct {
	ID     uuid.UUID
	Status string
	// Granted: hành động này đã được duyệt và approval đã được tiêu thụ cho chính nó.
	Granted bool
}

// Approvals là thứ Gateway cần từ Approval: hỏi một tool call đã được duyệt chưa (tạo
// approval nếu chưa có, tiêu thụ nó nếu đã duyệt). Approval gắn với đúng (task, step,
// tool, hash arguments).
type Approvals interface {
	Resolve(ctx context.Context, r ApprovalRequest) (ApprovalState, error)
}

// AgentTools là thứ Gateway cần từ Agent Catalog.
type AgentTools interface {
	// ToolRules trả về các mục tool của Agent Version và max_tool_calls của nó.
	ToolRules(ctx context.Context, agentVersionID uuid.UUID) ([]ToolRule, int, error)
}

// caller là bên gọi của một request MCP sau khi đã xác thực và kiểm quyền vào project.
type caller struct {
	principal identity.Principal
	projectID uuid.UUID
	// Chỉ có với Agent nội bộ.
	executionID *uuid.UUID
	taskID      *uuid.UUID
	// allow thu hẹp thêm tập tool (cấu hình Agent Version); nil = không thu hẹp.
	allow func(registry.Tool) bool
	// environment là môi trường của project (prod | non_prod).
	environment string
	// Chỉ có với Agent nội bộ: Agent của task, hạn của task, và việc Agent Version có
	// đòi duyệt tool này hay không.
	agentID         uuid.UUID
	deadline        time.Time
	requireApproval func(registry.Tool) bool
	// maxToolCalls là trần tool call của Agent Version (0 = không biết / không áp).
	maxToolCalls int
	// delegated: task được một Agent khác giao việc (độ sâu > 0 trong cây giao việc).
	// modelPlanned: task của một kế hoạch do model đề xuất.
	delegated    bool
	modelPlanned bool
}

// New tạo Gateway.
func New(db *pgxpool.Pool, tools Tools, authz Authorizer, projects Projects, signer *signedctx.Signer) *Gateway {
	g := &Gateway{db: db, tools: tools, authz: authz, projects: projects, signer: signer,
		limits:   newMemoryLimiter(),
		upstream: http.DefaultTransport}
	g.mcp = mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		s, _ := r.Context().Value(serverKey{}).(*mcp.Server)
		return s
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, MaxRequestBodyBytes: maxRequestBytes})
	return g
}

type serverKey struct{}

// RegisterRoutes gắn endpoint MCP cho client ngoài. Route phải nằm sau middleware
// xác thực OIDC.
func (g *Gateway) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/mcp/v1/projects/{project}", g.serve)
}

// EnableInternal bật đường dành cho Agent nội bộ (execution token, ADR-0004 mục 4).
// EnableApprovals bật việc phê duyệt cho tool call cần duyệt.
func (g *Gateway) EnableApprovals(a Approvals) { g.approvals = a }

// EnableServiceTokens bật các route nội bộ xác thực bằng service token của process api.
func (g *Gateway) EnableServiceTokens(tokens *exectoken.Issuer) { g.svcTokens = tokens }

func (g *Gateway) EnableInternal(tokens *exectoken.Issuer, tasks Tasks, agents AgentTools, users Initiators) {
	g.tokens, g.tasks, g.agents, g.users = tokens, tasks, agents, users
}

// RegisterInternalRoutes gắn endpoint MCP cho Agent nội bộ. Route này KHÔNG nằm sau
// middleware OIDC: nó chỉ nhận execution token, và token OIDC không dùng được ở đây.
func (g *Gateway) RegisterInternalRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/mcp/internal/v1/tasks/{task}", g.serveTask)
}

var errUnauthenticated = apperr.New(apperr.KindUnauthenticated, "unauthenticated", "thiếu thông tin xác thực")

// serveTask phục vụ một task của Agent nội bộ. Token chỉ định danh task; quyền được
// quyết định lại ở đây cho mỗi request: task còn chạy, người khởi tạo còn quyền gọi
// tool trong project, và tool nằm trong cấu hình của Agent Version.
func (g *Gateway) serveTask(w http.ResponseWriter, r *http.Request) {
	ctx, p, task, taskID, ok := g.authenticateTask(w, r, classMCP)
	if !ok {
		return
	}
	// Quyền của execution không vượt quyền hiện tại của người khởi tạo.
	if err := g.authz.Require(ctx, permToolCall, &task.ProjectID); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	rules, maxToolCalls, err := g.agents.ToolRules(ctx, task.AgentVersionID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	g.serveTools(w, r.WithContext(ctx), caller{principal: p, projectID: task.ProjectID,
		executionID: &task.ExecutionID, taskID: &taskID, environment: task.Environment,
		agentID: task.AgentID, deadline: task.Deadline, maxToolCalls: maxToolCalls, delegated: task.Depth > 0, modelPlanned: task.ModelPlanned,
		allow:           func(t registry.Tool) bool { return allowedByRules(rules, t) },
		requireApproval: func(t registry.Tool) bool { return approvalRequiredByRules(rules, t) }})
}

// authenticateTask xác thực một request của Agent nội bộ bằng execution token và
// kiểm lại trạng thái đã lưu: task còn chạy, token khớp execution / project / Agent
// Version / người khởi tạo, người khởi tạo còn được phục vụ, project chưa lưu trữ.
// Nó đã ghi phản hồi lỗi nếu trả ok = false.
func (g *Gateway) authenticateTask(w http.ResponseWriter, r *http.Request, class string) (context.Context, identity.Principal, ActiveTask, uuid.UUID, bool) {
	fail := func(err error) (context.Context, identity.Principal, ActiveTask, uuid.UUID, bool) {
		httpx.WriteError(w, r, err)
		return nil, identity.Principal{}, ActiveTask{}, uuid.Nil, false
	}
	if g.tokens == nil {
		return fail(errNotFound)
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	claims, err := g.tokens.Verify(token, time.Now())
	if !ok || err != nil {
		return fail(errUnauthenticated)
	}
	if id, err := uuid.Parse(r.PathValue("task")); err != nil || id != claims.TaskID {
		return fail(errNotFound)
	}
	// Execution do người dùng hoặc API key khởi tạo; loại khác chưa có đường kiểm quyền.
	if claims.InitiatorType != identity.PrincipalUser && claims.InitiatorType != identity.PrincipalAPIKey {
		return fail(errUnauthenticated)
	}
	p := identity.Principal{Type: claims.InitiatorType, ID: claims.InitiatorID, OrgID: claims.OrgID}
	ctx := tenancy.WithOrg(identity.WithPrincipal(r.Context(), p), claims.OrgID)
	// Hành động của Agent được quy về người khởi tạo trong audit.
	ctx = audit.WithActor(ctx, audit.Actor{Type: p.Type, ID: &p.ID})
	// Execution không sống lâu hơn quyền của người khởi tạo: user bị vô hiệu hoá,
	// rời org, hoặc org bị đình chỉ thì tool call và lượt gọi model dừng ngay.
	if err := g.users.RequireActive(ctx, p); err != nil {
		return fail(errUnauthenticated)
	}
	if !g.limit(w, r, p, class, false) {
		return nil, identity.Principal{}, ActiveTask{}, uuid.Nil, false
	}
	task, err := g.tasks.ActiveTask(ctx, claims.TaskID)
	if err != nil {
		return fail(err)
	}
	// Token phải khớp với trạng thái đã lưu: nó không tự nói được mình thuộc project
	// hay Agent Version nào.
	if task.ExecutionID != claims.ExecutionID || task.ProjectID != claims.ProjectID || task.AgentVersionID != claims.AgentVersionID ||
		task.InitiatorType != claims.InitiatorType || task.InitiatorID != claims.InitiatorID {
		return fail(errUnauthenticated)
	}
	project, err := g.projects.GetProject(ctx, task.ProjectID)
	if err != nil {
		return fail(err)
	}
	if project.ArchivedAt != nil {
		return fail(apperr.New(apperr.KindConflict, "project_archived", "project đã được lưu trữ"))
	}
	task.Environment = project.Environment
	return ctx, p, task, claims.TaskID, true
}

// approvalRequiredByRules: Agent Version đòi duyệt tool này (require_approval: always ở
// một mục của đúng server cho phép nó). Cấu hình của Agent chỉ làm chặt hơn policy.
func approvalRequiredByRules(rules []ToolRule, t registry.Tool) bool {
	for _, r := range rules {
		if r.ServerID == t.ServerID && r.RequireApproval && (slices.Contains(r.Allow, "*") || slices.Contains(r.Allow, t.Name)) {
			return true
		}
	}
	return false
}

// allowedByRules: tool được phép nếu có một mục của đúng server cho phép nó và
// KHÔNG mục nào của server đó cấm nó. Deny thắng trên mọi mục cùng server, và
// "*" trong deny cấm tất cả. Agent Version không có mục nào thì không có tool nào.
func allowedByRules(rules []ToolRule, t registry.Tool) bool {
	allowed := false
	for _, r := range rules {
		if r.ServerID != t.ServerID {
			continue
		}
		if slices.Contains(r.Deny, t.Name) || slices.Contains(r.Deny, "*") {
			return false
		}
		if slices.Contains(r.Allow, "*") || slices.Contains(r.Allow, t.Name) {
			allowed = true
		}
	}
	return allowed
}

var errNotFound = apperr.New(apperr.KindNotFound, "not_found", "tài nguyên không tồn tại")

// serve chạy bước 2 và 3 cho mọi request MCP tới một project, rồi trao cho SDK một
// server chỉ chứa đúng những tool principal được phép.
func (g *Gateway) serve(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, ok := identity.PrincipalFrom(ctx)
	if !ok {
		httpx.WriteError(w, r, apperr.New(apperr.KindUnauthenticated, "unauthenticated", "thiếu thông tin xác thực"))
		return
	}
	if !g.limit(w, r, p, classMCP, true) {
		return
	}
	projectID, err := uuid.Parse(r.PathValue("project"))
	if err != nil {
		httpx.WriteError(w, r, errNotFound)
		return
	}
	project, err := g.projects.GetProject(ctx, projectID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if err := g.authz.Require(ctx, permProjectRead, &projectID); err != nil {
		if apperr.KindOf(err) == apperr.KindPermissionDenied {
			err = errNotFound // project không đọc được thì không tồn tại
		}
		httpx.WriteError(w, r, err)
		return
	}
	if project.ArchivedAt != nil {
		httpx.WriteError(w, r, apperr.New(apperr.KindConflict, "project_archived", "project đã được lưu trữ"))
		return
	}
	if err := g.authz.Require(ctx, permToolCall, &projectID); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	// Client bên ngoài không thuộc execution nào: tool sensitive không tồn tại với nó (mức
	// sàn ở decide vẫn từ chối nếu có đường nào khác tới đây).
	g.serveTools(w, r, caller{principal: p, projectID: projectID, environment: project.Environment,
		allow: func(t registry.Tool) bool { return t.DataSensitivity != sensitivitySensitive }})
}

// serveTools trao cho SDK một server chỉ chứa đúng những tool bên gọi được phép.
func (g *Gateway) serveTools(w http.ResponseWriter, r *http.Request, c caller) {
	ctx := r.Context()
	enabled := true
	// Chỉ server dùng được ở project của lời gọi (chính sách project, migration 00041).
	tools, err := g.tools.ActiveTools(ctx, registry.ToolFilter{Enabled: &enabled, ProjectID: &c.projectID})
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}

	// Server được dựng cho từng request: không có trạng thái theo session, và danh
	// sách tool luôn phản ánh Registry và quyền tại thời điểm gọi.
	server := mcp.NewServer(&mcp.Implementation{Name: "hq-mcp-gateway", Version: "1"}, nil)
	known := map[string]bool{}
	bySlug := map[string]uuid.UUID{}
	for _, t := range tools {
		tool := t
		// Slug là không gian tên chung của platform và org. Registry từ chối slug trùng
		// khi đăng ký; nếu vẫn trùng, server đứng trước (của platform) thắng và server
		// kia không được phục vụ, thay vì để tool của bên này che tool của bên kia.
		if owner, ok := bySlug[tool.ServerSlug]; ok && owner != tool.ServerID {
			continue
		}
		bySlug[tool.ServerSlug] = tool.ServerID
		if c.allow != nil && !c.allow(tool) {
			continue
		}
		known[tool.QualifiedName()] = true
		server.AddTool(&mcp.Tool{
			Name:        tool.QualifiedName(),
			Description: tool.Description,
			InputSchema: tool.InputSchema,
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: tool.RiskLevel == "read"},
		}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return g.call(ctx, c, tool, req.Params.Arguments, req.Params.GetMeta()), nil
		})
	}
	// Gọi tool không có trong tập được phép (không tồn tại, đang tắt, bị khoá) cũng là
	// một quyết định từ chối và phải để lại dấu vết.
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if cr, ok := req.(*mcp.CallToolRequest); ok && method == "tools/call" && cr.Params != nil && !known[cr.Params.Name] {
				name := cr.Params.Name
				if len(name) > 128 {
					name = name[:128]
				}
				sum := sha256.Sum256(cr.Params.Arguments)
				if err := g.audit(ctx, c, nil, strings.ToValidUTF8(name, "?"), "", hex.EncodeToString(sum[:]),
					outcome{result: audit.OutcomeDenied, decision: "deny", rule: "tool_not_available"}, 0); err != nil {
					slog.ErrorContext(ctx, "gateway: không ghi được audit", "error", err)
				}
			}
			return next(ctx, method, req)
		}
	})
	g.mcp.ServeHTTP(w, r.WithContext(context.WithValue(ctx, serverKey{}, server)))
}

// outcome mô tả kết quả của một tool call cho audit.
type outcome struct {
	result   string // audit.OutcomeSuccess | OutcomeDenied | OutcomeError
	decision string // allow | deny | require_approval
	// approvalID là approval liên quan tới quyết định, nếu có.
	approvalID *uuid.UUID
	// executed: một tool có giữ chỗ (ghi, hoặc đã qua approval) đã được gửi tới upstream.
	executed bool
	rule     string
	bytes    int
	// items là tổng số phần tử của các mảng ở cấp cao nhất của kết quả (số container,
	// dòng log, SKU…): không chứa nội dung, nhưng đủ để ước lượng phạm vi một lần đọc
	// khi điều tra rò rỉ. -1: kết quả không phải một đối tượng JSON.
	items     int
	truncated bool
	redacted  int
}

func toolError(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: msg}}}
}

// Khoá trong _meta của tools/call trên đường nội bộ, và mã trong _meta của kết quả mà
// runtime dựa vào. Client MCP bên ngoài không dùng các khoá này.
const (
	MetaStepSeq        = "hq/step_seq"        // request: step_seq của tool call trong task
	MetaIdempotencyKey = "hq/idempotency_key" // gửi upstream: task_id:step_seq
	MetaCode           = "hq/code"            // kết quả: mã của quyết định
	MetaApprovalID     = "hq/approval_id"     // kết quả: approval liên quan

	CodeApprovalRequired = "approval_required" // đang chờ người duyệt
	CodeApprovalDenied   = "approval_denied"   // bị từ chối, hết hạn, bị huỷ, hoặc không khớp hành động
	CodeDenied           = "denied"            // policy từ chối
	CodeOutcomeUnknown   = "outcome_unknown"   // lần gọi trước có thể đã thực thi; không chạy lại

	// approvalTTL là thời hạn của một approval; nó cũng không vượt quá hạn của task.
	approvalTTL = 30 * time.Minute
)

// toolResult dựng kết quả lỗi có mã cho runtime.
func toolResult(msg, code string, approvalID *uuid.UUID) *mcp.CallToolResult {
	res := toolError(msg)
	res.Meta = mcp.Meta{MetaCode: code}
	if approvalID != nil {
		res.Meta[MetaApprovalID] = approvalID.String()
	}
	return res
}

// stepSeqOf đọc step_seq từ _meta của request (số nguyên dương).
func stepSeqOf(meta map[string]any) (int, bool) {
	f, ok := meta[MetaStepSeq].(float64)
	if !ok || f < 1 || f > 1<<20 || f != float64(int(f)) {
		return 0, false
	}
	return int(f), true
}

// call chạy bước 4–8 cho một tool call.
func (g *Gateway) call(ctx context.Context, c caller, tool registry.Tool, rawArgs json.RawMessage, meta map[string]any) *mcp.CallToolResult {
	p, projectID := c.principal, c.projectID
	start := time.Now()
	argsHash := sha256.Sum256(rawArgs)
	finish := func(res *mcp.CallToolResult, o outcome) *mcp.CallToolResult {
		// Audit là một phần của tool call: không ghi được thì không trả kết quả.
		if err := g.audit(ctx, c, &tool.ID, tool.QualifiedName(), tool.RiskLevel, hex.EncodeToString(argsHash[:]), o, time.Since(start)); err != nil {
			slog.ErrorContext(ctx, "gateway: không ghi được audit cho tool call", "error", err, "tool", tool.QualifiedName())
			if o.executed {
				// Tool có tác dụng phụ đã được gửi đi nhưng không ghi được audit: bên gọi
				// phải biết là KHÔNG được chạy lại hành động này, chứ không phải "thất bại".
				return toolResult("Tool đã được gửi đi nhưng không ghi được audit; không xác định được kết quả.", CodeOutcomeUnknown, o.approvalID)
			}
			return toolError("Không ghi được audit cho tool call; kết quả không được trả về.")
		}
		return res
	}

	// 4. Validate theo schema đã ghim.
	args, err := validateArguments(tool.InputSchema, rawArgs)
	if err != nil {
		return finish(toolError("Arguments không hợp lệ: "+err.Error()),
			outcome{result: audit.OutcomeError, decision: effectDeny, rule: "invalid_arguments"})
	}
	// Từ đây hash là của arguments đã chuẩn hoá (khoá được sắp xếp): cùng một hành động
	// luôn cho cùng một hash, dù bên gọi viết JSON theo thứ tự nào. Approval gắn với nó.
	canonical, err := json.Marshal(args)
	if err != nil {
		return finish(toolError("Arguments không hợp lệ."), outcome{result: audit.OutcomeError, decision: effectDeny, rule: "invalid_arguments"})
	}
	argsHash = sha256.Sum256(canonical)

	// 5. Policy. Gateway luôn tự quyết định, không tin rằng bên gọi đã kiểm tra.
	internal := c.taskID != nil && c.executionID != nil
	sensitive := tool.DataSensitivity == sensitivitySensitive
	egress := tool.Egress || tool.ServerKind == registry.KindExternal
	readSensitive := false
	if internal && egress && tool.Enabled {
		if readSensitive, err = g.hasSensitiveRead(ctx, p.OrgID, *c.executionID); err != nil {
			slog.ErrorContext(ctx, "gateway: không đọc được trạng thái dữ liệu nhạy cảm của execution", "error", err)
			return finish(toolResult("Không kiểm tra được chính sách dữ liệu; tool không được chạy.", CodeDenied, nil),
				outcome{result: audit.OutcomeError, decision: effectDeny, rule: "data_flow_unavailable"})
		}
	}
	d := decide(policyInput{Enabled: tool.Enabled, Risk: tool.RiskLevel, Environment: c.environment, Internal: internal,
		AgentRequiresApproval: c.requireApproval != nil && c.requireApproval(tool), Delegated: c.delegated, ModelPlanned: c.modelPlanned,
		Sensitive: sensitive, Egress: egress, ReadSensitive: readSensitive})
	if d.Effect == effectDeny {
		return finish(toolResult("Tool này không được phép chạy trong ngữ cảnh hiện tại.", CodeDenied, nil),
			outcome{result: audit.OutcomeDenied, decision: effectDeny, rule: d.Rule})
	}
	// Tool không chỉ đọc, và mọi tool cần duyệt, phải gắn với đúng một bước của task:
	// approval và việc chống chạy trùng đều khoá theo (task, step_seq).
	stepSeq, hasStep := stepSeqOf(meta)
	guarded := tool.RiskLevel != riskRead || d.Effect == effectRequireApproval
	if guarded && !hasStep {
		return finish(toolResult("Tool call thiếu tham chiếu tới bước của task.", CodeDenied, nil),
			outcome{result: audit.OutcomeDenied, decision: effectDeny, rule: "missing_step_reference"})
	}
	var approvalID *uuid.UUID
	if d.Effect == effectRequireApproval {
		if g.approvals == nil {
			return finish(toolResult("Tool này cần được phê duyệt, và môi trường này chưa bật phê duyệt.", CodeDenied, nil),
				outcome{result: audit.OutcomeDenied, decision: effectDeny, rule: "approval_unavailable"})
		}
		// Người duyệt thấy arguments, trừ secret lọt vào đó. Việc che làm trên MỘT BẢN SAO:
		// arguments được thực thi phải đúng là bản đã được băm và duyệt.
		var copyOf any
		if err := json.Unmarshal(canonical, &copyOf); err != nil {
			return finish(toolResult("Arguments không hợp lệ.", CodeDenied, nil), outcome{result: audit.OutcomeError, decision: effectDeny, rule: "invalid_arguments"})
		}
		redactions := 0
		shown, _ := json.Marshal(redactValue(copyOf, &redactions, 0))
		// Người yêu cầu là con người đứng sau execution: với API key, đó là người tạo key.
		owner, err := g.users.Owner(ctx, p)
		if err != nil {
			slog.ErrorContext(ctx, "gateway: không xác định được người đứng sau người khởi tạo", "error", err)
			return finish(toolResult("Không kiểm tra được phê duyệt; tool không được chạy.", CodeDenied, nil),
				outcome{result: audit.OutcomeError, decision: effectDeny, rule: "approval_unavailable"})
		}
		expires := time.Now().Add(approvalTTL)
		if !c.deadline.IsZero() && c.deadline.Before(expires) {
			expires = c.deadline // approval không sống lâu hơn task đang chờ nó
		}
		st, err := g.approvals.Resolve(ctx, ApprovalRequest{ProjectID: projectID, ExecutionID: *c.executionID, TaskID: *c.taskID,
			AgentID: c.agentID, StepSeq: stepSeq, ToolID: tool.ID, ToolName: tool.QualifiedName(), ServerName: tool.ServerSlug,
			RiskLevel: tool.RiskLevel, ArgsHash: argsHash[:], Arguments: shown, Redactions: redactions,
			RequestedByType: p.Type, RequestedBy: p.ID, RequestedByOwner: owner, ExpiresAt: expires})
		if err != nil {
			slog.ErrorContext(ctx, "gateway: không kiểm được approval", "error", err, "tool", tool.QualifiedName())
			return finish(toolResult("Không kiểm tra được phê duyệt; tool không được chạy.", CodeDenied, nil),
				outcome{result: audit.OutcomeError, decision: effectDeny, rule: "approval_unavailable"})
		}
		if st.ID != uuid.Nil {
			approvalID = &st.ID
		}
		switch {
		case st.Granted:
			// đã duyệt, approval đã được tiêu thụ cho đúng hành động này: đi tiếp
		case st.Status == "pending":
			return finish(toolResult("Tool call đang chờ phê duyệt.", CodeApprovalRequired, approvalID),
				outcome{result: audit.OutcomeDenied, decision: effectRequireApproval, rule: d.Rule, approvalID: approvalID})
		default:
			// rejected, expired, cancelled, hoặc approval của bước này là cho hành động khác.
			return finish(toolResult("Tool call không được phê duyệt ("+st.Status+").", CodeApprovalDenied, approvalID),
				outcome{result: audit.OutcomeDenied, decision: effectDeny, rule: "approval_" + st.Status, approvalID: approvalID})
		}
	}
	allowed := outcome{decision: effectAllow, rule: d.Rule, approvalID: approvalID}
	failed := func(msg string) *mcp.CallToolResult {
		o := allowed
		o.result = audit.OutcomeError
		return finish(toolError(msg), o)
	}

	// 6. Credential. Chỉ managed server với ngữ cảnh do Gateway ký; token của client
	// không bao giờ đi xuống upstream.
	var hc *http.Client
	switch {
	case tool.Endpoint == "":
	case tool.ServerKind == registry.KindManaged && tool.AuthMethod == "service_token":
		sc := signedctx.Context{Kind: signedctx.KindCall, Audience: tool.ServerSlug, OrgID: p.OrgID, ProjectID: projectID,
			Initiator: p.Type + ":" + p.ID.String(), ExecutionID: c.executionID}
		hc = &http.Client{
			Transport: &signedctx.Transport{Signer: g.signer, Context: sc, Base: limitTransport{g.upstream}},
			// Ngữ cảnh ký không đi theo redirect sang host khác.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	case tool.ServerKind == registry.KindExternal && g.ext != nil:
		// Server ngoài: SSRF guard được áp lại ở mỗi lần gọi; credential là của riêng
		// server đó, lấy từ Secrets.
		client, err := g.externalClient(ctx, tool.AuthMethod, tool.SecretID, "mcp_tool_call")
		if err != nil {
			slog.ErrorContext(ctx, "gateway: không dựng được client cho server ngoài", "error", err, "tool", tool.QualifiedName())
			return failed("Tool chưa dùng được: lỗi cấu hình credential của server.")
		}
		hc = client
	}
	if hc == nil {
		return finish(toolResult("Tool chưa dùng được: loại server này chưa được hỗ trợ.", CodeDenied, nil),
			outcome{result: audit.OutcomeDenied, decision: effectDeny, rule: "unsupported_upstream"})
	}

	// Tool không chỉ đọc: giữ quyền thực thi cho đúng bước này TRƯỚC khi gọi upstream.
	// Bước bị ngắt rồi chạy lại (worker chết, mất kết nối) gặp dòng đã có: lần trước có
	// thể đã thực thi, nên không gọi lại — trừ khi tool được khai là idempotent và đây
	// đúng là cùng một hành động, khi đó upstream nhận lại cùng idempotency key.
	//
	// Tool đã qua approval cũng được giữ chỗ như vậy, kể cả tool chỉ đọc: một approval
	// cho phép đúng một lần thực thi.
	var upstreamMeta mcp.Meta
	if guarded {
		first, same, total, err := g.claim(ctx, p.OrgID, *c.taskID, stepSeq, tool.ID, argsHash[:], c.maxToolCalls)
		if err != nil {
			slog.ErrorContext(ctx, "gateway: không giữ được quyền thực thi tool call", "error", err, "tool", tool.QualifiedName())
			return failed("Không ghi nhận được tool call; tool không được chạy.")
		}
		// Trần số tool call của Agent Version được áp lại ở đây, trên số lần Gateway đã
		// cho chạy: nó không dựa vào việc runtime tự đếm.
		if first && c.maxToolCalls > 0 && total > c.maxToolCalls {
			o := allowed
			o.result, o.decision, o.rule = audit.OutcomeDenied, effectDeny, "max_tool_calls"
			return finish(toolResult("Task đã dùng hết số tool call được phép.", CodeDenied, nil), o)
		}
		// Chạy lại chỉ khi tool được khai idempotent, là cùng một hành động, VÀ server là
		// managed (nó tôn trọng idempotency key); server ngoài có thể bỏ qua khoá đó.
		rerunnable := same && tool.SideEffect == "idempotent" && tool.ServerKind == registry.KindManaged && approvalID == nil
		if !first && !rerunnable {
			o := allowed
			o.result, o.decision, o.rule = audit.OutcomeDenied, effectDeny, "outcome_unknown"
			return finish(toolResult("Lần gọi trước của bước này có thể đã thực thi; tool không được chạy lại.", CodeOutcomeUnknown, approvalID), o)
		}
		upstreamMeta = mcp.Meta{MetaIdempotencyKey: c.taskID.String() + ":" + strconv.Itoa(stepSeq)}
		allowed.executed = true
	}

	// Execution sắp đọc dữ liệu sensitive: ghi lại TRƯỚC khi gọi upstream, để mọi tool
	// egress về sau của execution này cần người duyệt. Không ghi được thì không đọc.
	if internal && sensitive {
		if err := g.markSensitiveRead(ctx, p.OrgID, *c.executionID, tool.ID); err != nil {
			slog.ErrorContext(ctx, "gateway: không ghi được việc đọc dữ liệu nhạy cảm", "error", err, "tool", tool.QualifiedName())
			return failed("Không ghi nhận được việc đọc dữ liệu nhạy cảm; tool không được chạy.")
		}
	}

	// 7. Upstream.
	res, err := g.callUpstream(ctx, tool, hc, args, upstreamMeta)
	if err != nil {
		slog.WarnContext(ctx, "gateway: upstream lỗi", "error", err, "tool", tool.QualifiedName())
		msg := "MCP server không phản hồi; thử lại sau."
		if errors.Is(err, context.DeadlineExceeded) {
			msg = "Tool chạy quá thời hạn cho phép."
		}
		if guarded {
			// Không biết upstream đã thực thi hay chưa: không đoán, không thử lại.
			o := allowed
			o.result, o.rule = audit.OutcomeError, "outcome_unknown"
			return finish(toolResult(msg+" Không xác định được tool đã chạy hay chưa.", CodeOutcomeUnknown, approvalID), o)
		}
		return failed(msg)
	}

	// 8. Hậu xử lý.
	out, o := sanitize(res, tool.MaxResultBytes)
	o.decision, o.rule, o.approvalID, o.executed = effectAllow, d.Rule, approvalID, allowed.executed
	o.result = audit.OutcomeSuccess
	if out.IsError {
		o.result = audit.OutcomeError
	}
	return finish(out, o)
}

// claim giữ quyền thực thi một tool call cho (task, step). first = đây là lần đầu;
// same = dòng đã có là của đúng tool và đúng arguments này; total = số bước của task đã
// được giữ chỗ (gồm cả lần này).
func (g *Gateway) claim(ctx context.Context, orgID, taskID uuid.UUID, stepSeq int, toolID uuid.UUID, argsHash []byte, limit int) (first, same bool, total int, err error) {
	err = tenancy.InTx(tenancy.WithOrg(ctx, orgID), g.db, func(tx pgxv5.Tx) error {
		q := store.New(tx)
		// Đếm trước khi giữ chỗ: task đã chạm trần thì không ghi thêm dòng nào.
		n, err := q.CountToolClaims(ctx, store.CountToolClaimsParams{OrgID: orgID, TaskID: taskID})
		if err != nil {
			return err
		}
		prior, err := q.GetToolClaim(ctx, store.GetToolClaimParams{OrgID: orgID, TaskID: taskID, StepSeq: int32(stepSeq)}) //nolint:gosec // đã chặn ở stepSeqOf
		switch {
		case err == nil:
			same, total = prior.ToolID == toolID && bytes.Equal(prior.ArgsHash, argsHash), int(n)
			return nil
		case !errors.Is(err, pgxv5.ErrNoRows):
			return err
		}
		total = int(n) + 1
		if limit > 0 && total > limit {
			first = true // lần đầu của bước này, nhưng không được ghi: bên gọi từ chối theo total
			return nil
		}
		inserted, err := q.ClaimToolCall(ctx, store.ClaimToolCallParams{OrgID: orgID, TaskID: taskID, StepSeq: int32(stepSeq), ToolID: toolID, ArgsHash: argsHash}) //nolint:gosec // đã chặn ở stepSeqOf
		if err != nil {
			return err
		}
		if inserted == 1 {
			first = true
			return nil
		}
		// Một lần gọi đồng thời vừa giữ chỗ bước này.
		prior, err = q.GetToolClaim(ctx, store.GetToolClaimParams{OrgID: orgID, TaskID: taskID, StepSeq: int32(stepSeq)}) //nolint:gosec // đã chặn ở stepSeqOf
		if err != nil {
			return err
		}
		same = prior.ToolID == toolID && bytes.Equal(prior.ArgsHash, argsHash)
		return nil
	})
	return first, same, total, err
}

// sensitivitySensitive là mức data_sensitivity của dữ liệu nhạy cảm (mcp_tools).
const sensitivitySensitive = "sensitive"

func (g *Gateway) hasSensitiveRead(ctx context.Context, orgID, executionID uuid.UUID) (bool, error) {
	var ok bool
	err := tenancy.InTx(tenancy.WithOrg(ctx, orgID), g.db, func(tx pgxv5.Tx) error {
		var err error
		ok, err = store.New(tx).HasSensitiveRead(ctx, store.HasSensitiveReadParams{OrgID: orgID, ExecutionID: executionID})
		return err
	})
	return ok, err
}

func (g *Gateway) markSensitiveRead(ctx context.Context, orgID, executionID, toolID uuid.UUID) error {
	return tenancy.InTx(tenancy.WithOrg(ctx, orgID), g.db, func(tx pgxv5.Tx) error {
		return store.New(tx).MarkSensitiveRead(ctx, store.MarkSensitiveReadParams{OrgID: orgID, ExecutionID: executionID, ToolID: toolID})
	})
}

// maxExactInt là 2^53: số nguyên lớn hơn không biểu diễn chính xác được bằng float64.
var maxExactInt = new(big.Int).Lsh(big.NewInt(1), 53)

// exactNumbers từ chối JSON có số nguyên vượt ±2^53.
func exactNumbers(raw json.RawMessage) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil // hết dữ liệu (cú pháp đã được kiểm trước đó)
		}
		n, ok := tok.(json.Number)
		if !ok || strings.ContainsAny(string(n), ".eE") {
			continue // không phải số, hoặc số thực: không có giá trị nguyên chính xác để giữ
		}
		v, ok := new(big.Int).SetString(string(n), 10)
		if !ok || v.CmpAbs(maxExactInt) > 0 {
			return errors.New("chứa số nguyên vượt quá độ chính xác được hỗ trợ (±2^53)")
		}
	}
}

// validateArguments kiểm tra arguments theo JSON Schema đã ghim trong Registry,
// không theo schema upstream trả về lúc gọi.
func validateArguments(schema json.RawMessage, raw json.RawMessage) (map[string]any, error) {
	if len(raw) > maxArgumentBytes {
		return nil, errors.New("vượt giới hạn kích thước")
	}
	args := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil || args == nil {
			return nil, errors.New("phải là một JSON object")
		}
		// Số nguyên ngoài ±2^53 bị làm tròn khi giải mã: tool sẽ chạy với giá trị khác giá
		// trị được yêu cầu (và được duyệt). Từ chối thay vì âm thầm đổi giá trị.
		if err := exactNumbers(raw); err != nil {
			return nil, err
		}
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(schema, &s); err != nil {
		return nil, errors.New("schema của tool không đọc được")
	}
	resolved, err := s.Resolve(nil)
	if err != nil {
		return nil, errors.New("schema của tool không dùng được")
	}
	if err := resolved.Validate(args); err != nil {
		return nil, err
	}
	return args, nil
}

func (g *Gateway) callUpstream(ctx context.Context, tool registry.Tool, hc *http.Client, args map[string]any, meta mcp.Meta) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, tool.Timeout)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "hq-mcp-gateway", Version: "1"}, nil)
	sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: tool.Endpoint, HTTPClient: hc, DisableStandaloneSSE: true, MaxRetries: -1,
	}, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = sess.Close() }()
	return sess.CallTool(ctx, &mcp.CallToolParams{Meta: meta, Name: tool.Name, Arguments: args})
}

// sanitize biến kết quả của upstream thành thứ an toàn để trả về: chỉ giữ văn bản,
// che mẫu secret, và cắt theo giới hạn của tool. Kết quả tool là dữ liệu không tin
// cậy kể cả từ managed server.
func sanitize(res *mcp.CallToolResult, maxBytes int) (*mcp.CallToolResult, outcome) {
	var o outcome
	var b strings.Builder
	dropped := 0
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(tc.Text)
			continue
		}
		dropped++ // ảnh, audio, resource nhúng: chưa có chính sách cho chúng
	}
	text := b.String()
	// SDK thường trả cùng một dữ liệu ở cả dạng văn bản lẫn dạng có cấu trúc; chỉ giữ
	// một bản. Nếu không có văn bản thì dùng bản có cấu trúc.
	if text == "" && res.StructuredContent != nil {
		if j, err := json.Marshal(res.StructuredContent); err == nil {
			text = string(j)
		}
	}
	// NUL không có nghĩa trong văn bản và làm hỏng nơi lưu (jsonb) ở phía sau.
	text = strings.ReplaceAll(strings.ToValidUTF8(text, "?"), "\x00", "")
	text, o.redacted = redactText(text)
	o.items = countItems(text)
	if len(text) > maxBytes {
		text = strings.ToValidUTF8(text[:maxBytes], "")
		o.truncated = true
		text += fmt.Sprintf("\n[kết quả bị cắt ở %d byte]", maxBytes)
	}
	if dropped > 0 {
		text += fmt.Sprintf("\n[%d phần nội dung không phải văn bản đã bị bỏ]", dropped)
	}
	o.bytes = len(text)
	return &mcp.CallToolResult{IsError: res.IsError, Content: []mcp.Content{&mcp.TextContent{Text: text}}}, o
}

func (g *Gateway) audit(ctx context.Context, c caller, toolID *uuid.UUID, toolName, risk, argsHash string, o outcome, took time.Duration) error {
	p, projectID := c.principal, c.projectID
	details := map[string]string{}
	if c.executionID != nil {
		details["execution_id"], details["task_id"] = c.executionID.String(), c.taskID.String()
	}
	if o.approvalID != nil {
		details["approval_id"] = o.approvalID.String()
	}
	// Audit vẫn phải được ghi khi request đã bị huỷ ở phía client, nhưng không chờ vô
	// hạn: audit_append tuần tự hoá theo org, và mỗi lần chờ giữ một kết nối của pool.
	// Hết hạn thì tool call thất bại (fail-closed).
	ctx, cancel := context.WithTimeout(tenancy.WithOrg(context.WithoutCancel(ctx), p.OrgID), auditTimeout)
	defer cancel()
	return tenancy.InTx(ctx, g.db, func(tx pgxv5.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = '3s'"); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{
			OrgID: p.OrgID, ProjectID: &projectID, Action: "mcp_tool.call", ResourceType: "mcp_tool", ResourceID: toolID,
			ExecutionID: c.executionID,
			Outcome:     o.result,
			Details: merge(details, map[string]string{
				"tool": toolName, "risk_level": risk,
				"arguments_sha256": argsHash, // arguments đầy đủ không vào audit
				"decision":         o.decision, "rule": o.rule,
				"duration_ms":  strconv.FormatInt(took.Milliseconds(), 10),
				"result_bytes": strconv.Itoa(o.bytes), "truncated": strconv.FormatBool(o.truncated),
				"redactions": strconv.Itoa(o.redacted), "result_items": strconv.Itoa(o.items),
			})})
	})
}

// countItems đếm phần tử của các mảng ở cấp cao nhất của một đối tượng JSON; -1 nếu văn
// bản không phải một đối tượng JSON.
func countItems(text string) int {
	var top map[string]json.RawMessage
	if json.Unmarshal([]byte(text), &top) != nil {
		return -1
	}
	n := 0
	for _, v := range top {
		var arr []json.RawMessage
		if json.Unmarshal(v, &arr) == nil {
			n += len(arr)
		}
	}
	return n
}

func merge(a, b map[string]string) map[string]string {
	for k, v := range a {
		b[k] = v
	}
	return b
}

// limitTransport chặn phản hồi upstream vượt maxUpstreamBytes: SDK đọc cả body vào
// bộ nhớ, và kết quả tool là dữ liệu không tin cậy kể cả từ managed server.
type limitTransport struct{ base http.RoundTripper }

func (t limitTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	resp.Body = &limitedBody{ReadCloser: resp.Body, left: maxUpstreamBytes}
	return resp, nil
}

type limitedBody struct {
	io.ReadCloser
	left int64
}

var errUpstreamTooLarge = errors.New("gateway: phản hồi của MCP server vượt giới hạn kích thước")

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		return 0, errUpstreamTooLarge
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.ReadCloser.Read(p)
	b.left -= int64(n)
	return n, err
}

// contextForAudit: audit vẫn được ghi khi request đã bị huỷ, nhưng không chờ vô hạn.
func contextForAudit(ctx context.Context, orgID uuid.UUID) (context.Context, context.CancelFunc) {
	return context.WithTimeout(tenancy.WithOrg(context.WithoutCancel(ctx), orgID), auditTimeout)
}
