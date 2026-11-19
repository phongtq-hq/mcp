// Package authz là điểm quyết định quyền duy nhất của platform (ADR-0009).
//
// Phase 1: RBAC. Role là tập permission; role binding gắn role cho principal ở
// phạm vi org hoặc project. Điều kiện ABAC và require_approval đến ở phase sau;
// binding có điều kiện hiện bị bỏ qua (từ chối), không bị coi là vô điều kiện.
package authz

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	pgxv5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hqgroups/mcp/internal/audit"
	"github.com/hqgroups/mcp/internal/platform/authz/store"
	"github.com/hqgroups/mcp/internal/platform/identity"
	"github.com/hqgroups/mcp/internal/platform/tenancy"
	apperr "github.com/hqgroups/mcp/internal/shared/errors"
)

// Danh mục permission, dạng resource:action. Là hằng số trong code vì thêm
// permission luôn đi kèm code kiểm tra nó.
const (
	OrganizationRead   = "organization:read"
	OrganizationUpdate = "organization:update"
	ProjectRead        = "project:read"
	ProjectCreate      = "project:create"
	ProjectUpdate      = "project:update"
	ProjectArchive     = "project:archive"
	AgentRead          = "agent:read"
	AgentCreate        = "agent:create"
	AgentUpdate        = "agent:update"  // sửa tên, mô tả, bật / tắt, tạo version mới
	AgentPublish       = "agent:publish" // chọn version đang hoạt động
	AgentArchive       = "agent:archive"

	ModelProviderRead   = "model_provider:read"
	ModelProviderCreate = "model_provider:create"
	ModelProviderUpdate = "model_provider:update"
	ModelProviderDelete = "model_provider:delete"
	// ModelProviderSetCredential: đặt hoặc xoay API key của provider.
	ModelProviderSetCredential = "model_provider:set_credential"
	ModelRead                  = "model:read"
	ModelCreate                = "model:create"
	ModelUpdate                = "model:update"
	ModelPricingRead           = "model_pricing:read"
	ModelPricingCreate         = "model_pricing:create"
	McpServerRead              = "mcp_server:read"
	McpToolCall                = "mcp_tool:call"
	McpServerCreate            = "mcp_server:create"
	McpServerUpdate            = "mcp_server:update"
	McpServerDelete            = "mcp_server:delete"
	McpServerDiscover          = "mcp_server:discover"
	McpServerReview            = "mcp_server:review"
	McpToolUpdate              = "mcp_tool:update"

	BudgetRead  = "budget:read"
	BudgetWrite = "budget:write"

	ApprovalRead   = "approval:read"
	ApprovalDecide = "approval:decide" // duyệt hoặc từ chối; người yêu cầu không tự duyệt

	AuditLogRead = "audit_log:read"
	UsageRead    = "usage:read"

	APIKeyRead   = "api_key:read"
	APIKeyCreate = "api_key:create"
	APIKeyRevoke = "api_key:revoke"

	NotificationWebhookRead  = "notification_webhook:read"
	NotificationWebhookWrite = "notification_webhook:write"

	WorkflowRead  = "workflow:read"
	WorkflowWrite = "workflow:write"
	WorkflowRun   = "workflow:run"

	ExecutionRead        = "execution:read"
	ExecutionCreate      = "execution:create"
	ExecutionCancel      = "execution:cancel"
	ExecutionResume      = "execution:resume"       // cho chạy tiếp một execution do người khác tạm dừng
	ExecutionReadContent = "execution:read_content" // nội dung prompt và kết quả tool trong step
)

var catalog = []string{
	OrganizationRead, OrganizationUpdate,
	ProjectRead, ProjectCreate, ProjectUpdate, ProjectArchive,
	AgentRead, AgentCreate, AgentUpdate, AgentPublish, AgentArchive,
	ModelProviderRead, ModelProviderCreate, ModelProviderUpdate, ModelProviderDelete, ModelProviderSetCredential,
	ModelRead, ModelCreate, ModelUpdate, ModelPricingRead, ModelPricingCreate,
	McpServerRead, McpToolCall,
	McpServerCreate, McpServerUpdate, McpServerDelete, McpServerDiscover, McpServerReview, McpToolUpdate,
	ExecutionRead, ExecutionCreate, ExecutionCancel, ExecutionResume, ExecutionReadContent,
	APIKeyRead, APIKeyCreate, APIKeyRevoke,
	AuditLogRead, UsageRead, BudgetRead, BudgetWrite, ApprovalRead, ApprovalDecide,
	WorkflowRead, WorkflowWrite, WorkflowRun,
	NotificationWebhookRead, NotificationWebhookWrite,
}

// orgWideReads là các quyền ĐỌC tài nguyên cấp org mà mọi thành viên có role mang
// permission đó đều dùng được, kể cả khi role chỉ được gắn ở phạm vi một project:
// người cấu hình agent trong một project vẫn cần xem thông tin org và danh mục model.
//
// Chỉ gồm thứ cần để cấu hình agent. Cấu hình provider và lịch sử giá riêng của
// org không nằm ở đây: chúng cần role gắn ở phạm vi org.
var orgWideReads = []string{OrganizationRead, ModelRead, McpServerRead}

// Effect là kết quả của một quyết định.
type Effect int

const (
	Deny Effect = iota
	Allow
	RequireApproval // chưa được trả về ở Phase 1
)

// Decision là quyết định kèm quy tắc đã khớp, để giải thích được vì sao.
type Decision struct {
	Effect Effect
	Rule   string
}

// Principal là chủ thể xin quyền.
type Principal struct {
	Type string
	ID   uuid.UUID
}

// Resource là đối tượng của hành động. ProjectID nil nghĩa là tài nguyên cấp org.
type Resource struct {
	ProjectID *uuid.UUID
}

// Engine đánh giá quyền từ role binding trong database.
type Engine struct {
	db *pgxpool.Pool

	mu         sync.Mutex
	recentDeny map[denyKey]time.Time
}

// NewEngine tạo Engine.
func NewEngine(db *pgxpool.Pool) *Engine {
	return &Engine{db: db, recentDeny: map[denyKey]time.Time{}}
}

type denyKey struct {
	org, principal uuid.UUID
	action         string
	project        uuid.UUID
}

const (
	denyAuditWindow = time.Minute
	denyCacheMax    = 10_000
)

// shouldAuditDeny gộp các lần từ chối giống hệt nhau trong một cửa sổ thời gian:
// lần đầu được ghi audit, các lần lặp lại thì không. Nếu không, một thành viên
// chưa có quyền làm phình bảng audit (chỉ thêm, không xoá được) và giữ khoá ghi
// audit của org bằng cách gửi request lặp lại. Mọi lần vẫn bị từ chối như nhau.
// Bộ nhớ đệm theo process: nhiều replica ghi tối đa mỗi replica một bản mỗi cửa sổ.
func (e *Engine) shouldAuditDeny(k denyKey) bool {
	now := time.Now()
	e.mu.Lock()
	defer e.mu.Unlock()
	if last, ok := e.recentDeny[k]; ok && now.Sub(last) < denyAuditWindow {
		return false
	}
	if len(e.recentDeny) >= denyCacheMax {
		clear(e.recentDeny)
	}
	e.recentDeny[k] = now
	return true
}

// Decide trả lời principal có được thực hiện action trên resource hay không.
// Tenant lấy từ context. attributes dành cho điều kiện ABAC (chưa dùng).
func (e *Engine) Decide(ctx context.Context, p Principal, action string, res Resource, _ map[string]any) (Decision, error) {
	if !slices.Contains(catalog, action) {
		return Decision{Effect: Deny, Rule: "unknown_permission"}, nil
	}
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return Decision{}, tenancy.ErrNoTenant
	}

	grants, err := e.grants(ctx, orgID, p)
	if err != nil {
		return Decision{}, err
	}

	anyScope := slices.Contains(orgWideReads, action)
	for _, g := range grants {
		// Binding toàn org áp dụng cho mọi tài nguyên; binding theo project chỉ
		// áp dụng cho đúng project đó, không cho tài nguyên cấp org khác.
		inScope := anyScope || g.ProjectID == nil || (res.ProjectID != nil && *g.ProjectID == *res.ProjectID)
		if inScope && slices.Contains(g.Permissions, action) {
			return Decision{Effect: Allow, Rule: "role:" + g.RoleName}, nil
		}
	}
	return Decision{Effect: Deny, Rule: "no_matching_grant"}, nil
}

func (e *Engine) grants(ctx context.Context, orgID uuid.UUID, p Principal) ([]store.ListGrantsForPrincipalRow, error) {
	var grants []store.ListGrantsForPrincipalRow
	err := tenancy.InTx(ctx, e.db, func(tx pgxv5.Tx) error {
		var err error
		grants, err = store.New(tx).ListGrantsForPrincipal(ctx, store.ListGrantsForPrincipalParams{
			OrgID: orgID, PrincipalType: p.Type, PrincipalID: p.ID,
		})
		return err
	})
	return grants, err
}

// ReadableProjects trả lời principal của request đọc được những project nào:
// all = true nếu có quyền đọc toàn org; ngược lại ids là các project được gắn
// quyền đọc riêng. Không có quyền đọc project nào thì trả lỗi 403.
func (e *Engine) ReadableProjects(ctx context.Context) (all bool, projectIDs []uuid.UUID, err error) {
	p, ok := identity.PrincipalFrom(ctx)
	if !ok {
		return false, nil, apperr.New(apperr.KindUnauthenticated, "unauthenticated", "thiếu thông tin xác thực")
	}
	grants, err := e.grants(ctx, p.OrgID, Principal{Type: p.Type, ID: p.ID})
	if err != nil {
		return false, nil, apperr.Wrap(err, apperr.KindInternal, "internal_error", "")
	}
	for _, g := range grants {
		if !slices.Contains(g.Permissions, ProjectRead) {
			continue
		}
		if g.ProjectID == nil {
			return true, nil, nil
		}
		if !slices.Contains(projectIDs, *g.ProjectID) {
			projectIDs = append(projectIDs, *g.ProjectID)
		}
	}
	if len(projectIDs) == 0 {
		return false, nil, e.deny(ctx, p, ProjectRead, nil, "no_matching_grant")
	}
	return false, projectIDs, nil
}

// deny ghi quyết định từ chối vào audit trail kèm quy tắc đã khớp (security §4.2)
// và trả lỗi 403. Không ghi được audit thì trả lỗi nội bộ: từ chối vẫn có hiệu lực.
func (e *Engine) deny(ctx context.Context, p identity.Principal, action string, projectID *uuid.UUID, rule string) error {
	denied := apperr.New(apperr.KindPermissionDenied, "permission_denied", "không đủ quyền thực hiện hành động này")
	k := denyKey{org: p.OrgID, principal: p.ID, action: action}
	if projectID != nil {
		k.project = *projectID
	}
	if !e.shouldAuditDeny(k) {
		return denied
	}
	resourceType, _, _ := strings.Cut(action, ":")
	err := tenancy.InTx(ctx, e.db, func(tx pgxv5.Tx) error {
		return audit.Record(ctx, tx, audit.Event{
			OrgID: p.OrgID, ProjectID: projectID, Action: "authz.deny",
			ResourceType: resourceType, ResourceID: projectID, Outcome: audit.OutcomeDenied,
			Details: map[string]string{"permission": action, "rule": rule},
		})
	})
	if err != nil {
		return apperr.Wrap(err, apperr.KindInternal, "internal_error", "")
	}
	return denied
}

// Require kiểm tra quyền cho principal của request hiện tại; trả lỗi 403 nếu
// không được phép. projectID nil nghĩa là hành động cấp org.
func (e *Engine) Require(ctx context.Context, action string, projectID *uuid.UUID) error {
	p, ok := identity.PrincipalFrom(ctx)
	if !ok {
		return apperr.New(apperr.KindUnauthenticated, "unauthenticated", "thiếu thông tin xác thực")
	}
	d, err := e.Decide(ctx, Principal{Type: p.Type, ID: p.ID}, action, Resource{ProjectID: projectID}, nil)
	if err != nil {
		return apperr.Wrap(err, apperr.KindInternal, "internal_error", "")
	}
	if d.Effect != Allow {
		return e.deny(ctx, p, action, projectID, d.Rule)
	}
	return nil
}

// Missing trả về những permission trong danh sách mà principal của request không có
// ở phạm vi đã cho (projectID nil = toàn org). Dùng khi một principal trao quyền cho
// principal khác (API key): quyền được trao không được vượt quyền của người trao.
// Không ghi audit: đây là phép so sánh, không phải một lần từ chối truy cập.
func (e *Engine) Missing(ctx context.Context, permissions []string, projectID *uuid.UUID) ([]string, error) {
	p, ok := identity.PrincipalFrom(ctx)
	if !ok {
		return nil, apperr.New(apperr.KindUnauthenticated, "unauthenticated", "thiếu thông tin xác thực")
	}
	grants, err := e.grants(ctx, p.OrgID, Principal{Type: p.Type, ID: p.ID})
	if err != nil {
		return nil, err
	}
	var missing []string
	for _, perm := range permissions {
		held := false
		for _, g := range grants {
			// Binding toàn org phủ mọi phạm vi; binding theo project chỉ phủ đúng project đó.
			if (g.ProjectID == nil || (projectID != nil && *g.ProjectID == *projectID)) && slices.Contains(g.Permissions, perm) {
				held = true
				break
			}
		}
		if !held {
			missing = append(missing, perm)
		}
	}
	return missing, nil
}
