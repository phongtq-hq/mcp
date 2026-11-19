// Package catalog quản lý định nghĩa Agent và các Agent Version bất biến (ADR-0006).
//
// Agent là định danh ổn định với lifecycle draft → active ⇄ disabled → archived.
// Toàn bộ cấu hình (prompt, model, tool, giới hạn) nằm trong version; sửa cấu hình
// là tạo version mới, publish và rollback là đổi con trỏ active_version.
package catalog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	pgxv5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hqgroups/mcp/internal/agent/catalog/store"
	"github.com/hqgroups/mcp/internal/agent/orchestrator"
	"github.com/hqgroups/mcp/internal/api"
	"github.com/hqgroups/mcp/internal/audit"
	"github.com/hqgroups/mcp/internal/platform/identity"
	"github.com/hqgroups/mcp/internal/platform/tenancy"
	apperr "github.com/hqgroups/mcp/internal/shared/errors"
	"github.com/hqgroups/mcp/internal/shared/httpx"
	"github.com/hqgroups/mcp/internal/shared/ids"
	"github.com/hqgroups/mcp/internal/shared/pagination"
)

// Trạng thái của Agent.
const (
	StatusDraft    = "draft"
	StatusActive   = "active"
	StatusDisabled = "disabled"
	StatusArchived = "archived"
)

// Trần của platform cho giới hạn trong một Agent Version. Agent chỉ được đặt
// thấp hơn. Các con số này là điểm khởi đầu, chưa dựa trên số đo.
const (
	MaxIterationsCap   = 100
	MaxToolCallsCap    = 500
	MaxTimeoutSeconds  = 24 * 60 * 60
	MaxOutputTokensCap = 200_000
	MaxDelegationDepth = orchestrator.MaxDelegationDepth
	// MaxDelegates là số Agent tối đa một Agent Version được giao việc cho.
	MaxDelegates   = 8
	maxBudgetUSD   = 1000 // trần budget cho một execution
	maxPromptRunes = 100_000
	maxNameRunes   = 200
	maxDescRunes   = 2000
)

// Agent là định nghĩa một Agent.
type Agent struct {
	ID            uuid.UUID
	ProjectID     uuid.UUID
	Slug          string
	Name          string
	Description   *string
	Status        string
	ActiveVersion *int
	LatestVersion int
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Version là một Agent Version bất biến.
type Version struct {
	ID           uuid.UUID
	AgentID      uuid.UUID
	Version      int
	SystemPrompt string
	Config       json.RawMessage
	ChangeNote   *string
	CreatedBy    *uuid.UUID
	CreatedAt    time.Time
}

// VersionInput là nội dung của một version mới.
type VersionInput struct {
	SystemPrompt string
	Config       api.AgentConfig
	ChangeNote   *string
	Activate     bool
}

// CreateInput là đầu vào tạo Agent; version đầu tiên được tạo cùng lúc.
type CreateInput struct {
	Slug        string
	Name        string
	Description *string
	Version     VersionInput
}

// UpdateInput sửa metadata hoặc bật / tắt Agent. Không sửa cấu hình.
type UpdateInput struct {
	Name        *string
	Description *string
	Status      *string // active | disabled
	IfUpdatedAt *time.Time
}

// ListParams là tham số liệt kê Agent trong một project.
type ListParams struct {
	Limit  int
	Cursor string
	Status string // rỗng = mọi trạng thái trừ archived
	Search string
	Sort   string // name | -name | created_at | -created_at
}

// Projects là thứ catalog cần từ Identity & Tenancy.
type Projects interface {
	GetProject(ctx context.Context, id uuid.UUID) (tenancy.Project, error)
}

// ToolValidator kiểm tra danh sách tool của một version với Tool Registry và
// quyền của project. Chưa có Registry thì nil, và version tham chiếu tool bị từ chối.
type ToolValidator interface {
	ValidateTools(ctx context.Context, projectID uuid.UUID, cfg api.AgentConfig) error
}

// ModelValidator kiểm tra model được ghim (policy fixed) tồn tại và dùng được.
type ModelValidator interface {
	ValidateModel(ctx context.Context, modelID uuid.UUID) error
}

var (
	slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)

	errNotFound        = apperr.New(apperr.KindNotFound, "not_found", "tài nguyên không tồn tại")
	errSlugTaken       = apperr.New(apperr.KindConflict, "slug_taken", "slug đã được dùng trong project này")
	errArchived        = apperr.New(apperr.KindConflict, "agent_archived", "agent đã được lưu trữ")
	errProjectArchived = apperr.New(apperr.KindConflict, "project_archived", "project đã được lưu trữ")
	errNoActiveVersion = apperr.New(apperr.KindConflict, "no_active_version", "agent chưa có version nào được publish; chọn active version trước")
)

func invalid(msg string) error { return apperr.New(apperr.KindUnprocessable, "validation_failed", msg) }
func internal(err error) error { return apperr.Wrap(err, apperr.KindInternal, "internal_error", "") }

// Service chứa nghiệp vụ Agent Catalog. Tenant luôn lấy từ context.
type Service struct {
	db       *pgxpool.Pool
	projects Projects
	tools    ToolValidator
	models   ModelValidator
}

// NewService tạo Service. tools và models có thể nil khi context tương ứng chưa có.
func NewService(db *pgxpool.Pool, projects Projects, tools ToolValidator, models ModelValidator) *Service {
	return &Service{db: db, projects: projects, tools: tools, models: models}
}

func validText(s string, max int, allowNewline bool) bool {
	if utf8.RuneCountInString(s) > max {
		return false
	}
	return !strings.ContainsFunc(s, func(r rune) bool {
		if allowNewline && (r == '\n' || r == '\r' || r == '\t') {
			return false
		}
		return unicode.IsControl(r)
	})
}

// validLabel dành cho văn bản hiển thị cho người duyệt (tên, mô tả): ngoài ký tự
// điều khiển còn từ chối ký tự định dạng vô hình (bidi override, zero-width).
func validLabel(s string, max int, allowNewline bool) bool {
	return validText(s, max, allowNewline) &&
		!strings.ContainsFunc(s, func(r rune) bool { return unicode.Is(unicode.Cf, r) })
}

var amountRe = regexp.MustCompile(`^[0-9]{1,10}(\.[0-9]{1,8})?$`)

func validateBudget(m *api.Money) error {
	if m == nil {
		return nil
	}
	if m.Currency != "USD" {
		return invalid("limits.budget.currency chỉ hỗ trợ USD")
	}
	amount, ok := new(big.Rat).SetString(m.Amount)
	if !amountRe.MatchString(m.Amount) || !ok || amount.Sign() <= 0 || amount.Cmp(big.NewRat(maxBudgetUSD, 1)) > 0 {
		return invalid("limits.budget.amount phải là số thập phân dương, tối đa 8 chữ số thập phân và không vượt " + strconv.Itoa(maxBudgetUSD))
	}
	return nil
}

func validateMeta(name *string, desc *string) error {
	if name != nil && (strings.TrimSpace(*name) == "" || !validLabel(*name, maxNameRunes, false)) {
		return invalid("name phải có từ 1 đến 200 ký tự, không chứa ký tự điều khiển hay ký tự định dạng vô hình")
	}
	if desc != nil && !validLabel(*desc, maxDescRunes, true) {
		return invalid("description tối đa 2000 ký tự, không chứa ký tự điều khiển hay ký tự định dạng vô hình")
	}
	return nil
}

// validateVersion kiểm tra nội dung một version. Kiểm tra ở đây là bắt buộc trước
// khi lưu: version đã lưu là bất biến và có thể được publish bất cứ lúc nào.
func (s *Service) validateVersion(ctx context.Context, projectID uuid.UUID, in VersionInput) error {
	if strings.TrimSpace(in.SystemPrompt) == "" || !validText(in.SystemPrompt, maxPromptRunes, true) {
		return invalid("system_prompt là bắt buộc, tối đa 100000 ký tự, không chứa ký tự điều khiển")
	}
	if in.ChangeNote != nil && !validLabel(*in.ChangeNote, maxDescRunes, true) {
		return invalid("change_note tối đa 2000 ký tự")
	}
	c := in.Config
	l := c.Limits
	if l.MaxIterations < 1 || l.MaxIterations > MaxIterationsCap {
		return invalid("limits.max_iterations phải từ 1 đến " + strconv.Itoa(MaxIterationsCap))
	}
	if l.MaxToolCalls < 1 || l.MaxToolCalls > MaxToolCallsCap {
		return invalid("limits.max_tool_calls phải từ 1 đến " + strconv.Itoa(MaxToolCallsCap))
	}
	if l.TimeoutSeconds < 1 || l.TimeoutSeconds > MaxTimeoutSeconds {
		return invalid("limits.timeout_seconds phải từ 1 đến " + strconv.Itoa(MaxTimeoutSeconds))
	}
	if err := validateBudget(l.Budget); err != nil {
		return err
	}
	if c.Role != nil && !validText(*c.Role, maxDescRunes, true) {
		return invalid("role tối đa 2000 ký tự")
	}
	if t := c.Model.DefaultTier; t != nil {
		switch *t {
		case "economy", "standard", "premium":
		default:
			return invalid("model.default_tier phải là economy, standard hoặc premium")
		}
	}

	switch c.Model.Policy {
	case "tiered":
		if c.Model.FixedModelId != nil {
			return invalid("model.fixed_model_id chỉ dùng với policy fixed")
		}
	case "fixed":
		if c.Model.FixedModelId == nil {
			return invalid("policy fixed cần model.fixed_model_id")
		}
		if s.models == nil {
			return invalid("policy fixed chưa dùng được: Model Catalog chưa được hiện thực")
		}
		if err := s.models.ValidateModel(ctx, *c.Model.FixedModelId); err != nil {
			return err
		}
	default:
		return invalid("model.policy phải là fixed hoặc tiered")
	}
	if t := c.Model.Temperature; t != nil && (*t < 0 || *t > 2) {
		return invalid("model.temperature phải từ 0 đến 2")
	}
	if m := c.Model.MaxOutputTokens; m != nil && (*m < 1 || *m > MaxOutputTokensCap) {
		return invalid("model.max_output_tokens phải từ 1 đến " + strconv.Itoa(MaxOutputTokensCap))
	}

	// Tool: fail-closed. Không có Registry để đối chiếu thì không nhận version có
	// tool, vì không kiểm được tool tồn tại, thuộc quyền project, và
	// require_approval chỉ chặt hơn mức rủi ro của tool.
	if len(c.Tools) > 0 {
		if s.tools == nil {
			return invalid("version tham chiếu tool chưa dùng được: Tool Registry chưa được hiện thực")
		}
		if err := s.tools.ValidateTools(ctx, projectID, c); err != nil {
			return err
		}
	}
	// Delegation (agent-runtime §5.4): Agent đích phải thuộc cùng project. Việc giao việc
	// lúc chạy còn được quyết định lại từng lần (độ sâu, chu trình, số task, budget).
	if d := c.Delegation; d != nil {
		if d.MaxDepth != nil && (*d.MaxDepth < 0 || *d.MaxDepth > MaxDelegationDepth) {
			return invalid("delegation.max_depth phải từ 0 đến " + strconv.Itoa(MaxDelegationDepth))
		}
		if d.CanDelegateTo != nil && len(*d.CanDelegateTo) > 0 {
			if len(*d.CanDelegateTo) > MaxDelegates {
				return invalid("delegation.can_delegate_to có tối đa " + strconv.Itoa(MaxDelegates) + " agent")
			}
			// max_depth mặc định là 0 (không giao việc): danh sách agent đích mà không có
			// độ sâu là cấu hình không có tác dụng, dễ bị hiểu nhầm là đã bật.
			if d.MaxDepth == nil || *d.MaxDepth < 1 {
				return invalid("delegation.max_depth phải từ 1 trở lên khi có can_delegate_to")
			}
			seen := map[uuid.UUID]bool{}
			for _, id := range *d.CanDelegateTo {
				if seen[id] {
					return invalid("delegation.can_delegate_to lặp lại một agent")
				}
				seen[id] = true
				// Agent của project khác (hay tenant khác) cho cùng câu trả lời với agent không tồn tại.
				a, err := s.Get(ctx, id)
				if err != nil && apperr.KindOf(err) != apperr.KindNotFound {
					return err
				}
				if err != nil || a.ProjectID != projectID || a.Status == "archived" {
					return invalid("delegation.can_delegate_to: agent " + id.String() + " không tồn tại trong project này")
				}
			}
		}
	}
	// Session và memory thuộc Phase 2: chỉ nhận giá trị không có tác dụng.
	if m := c.Memory; m != nil && m.SessionHistory != nil && *m.SessionHistory != "none" {
		return invalid("memory.session_history chỉ hỗ trợ none ở phase này")
	}
	if r := c.Runtime; r != nil && r.Isolation != nil && *r.Isolation != "shared" {
		return invalid("runtime.isolation chỉ hỗ trợ shared ở phase này")
	}
	return nil
}

// canonicalConfig trả về JSON chuẩn hoá của config (chỉ gồm trường đã biết) và
// hash của system_prompt + config, dùng làm định danh nội dung của version.
func canonicalConfig(in VersionInput) (cfg json.RawMessage, hash []byte, err error) {
	cfg, err = json.Marshal(in.Config)
	if err != nil {
		return nil, nil, err
	}
	h := sha256.New()
	h.Write([]byte(in.SystemPrompt))
	h.Write([]byte{0})
	h.Write(cfg)
	return cfg, h.Sum(nil), nil
}

func createdBy(ctx context.Context) *uuid.UUID {
	if p, ok := identity.PrincipalFrom(ctx); ok && p.Type == identity.PrincipalUser {
		return &p.ID
	}
	return nil
}

func isPg(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}

func (s *Service) insertVersion(ctx context.Context, q *store.Queries, orgID, agentID uuid.UUID, n int, in VersionInput) (store.AgentVersion, error) {
	cfg, hash, err := canonicalConfig(in)
	if err != nil {
		return store.AgentVersion{}, err
	}
	return q.InsertAgentVersion(ctx, store.InsertAgentVersionParams{
		ID: ids.New(), OrgID: orgID, AgentID: agentID, Version: int32(n), //nolint:gosec // số version nhỏ
		SystemPrompt: in.SystemPrompt, Config: cfg, ConfigHash: hash,
		ChangeNote: in.ChangeNote, CreatedBy: createdBy(ctx),
	})
}

// Create tạo Agent ở trạng thái draft cùng version 1 (chưa được publish).
func (s *Service) Create(ctx context.Context, projectID uuid.UUID, in CreateInput) (Agent, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return Agent{}, internal(tenancy.ErrNoTenant)
	}
	if !slugRe.MatchString(in.Slug) {
		return Agent{}, invalid("slug phải khớp ^[a-z0-9][a-z0-9-]{1,62}$")
	}
	if err := validateMeta(&in.Name, in.Description); err != nil {
		return Agent{}, err
	}
	project, err := s.projects.GetProject(ctx, projectID)
	if err != nil {
		return Agent{}, err
	}
	if project.ArchivedAt != nil {
		return Agent{}, errProjectArchived
	}
	if err := s.validateVersion(ctx, projectID, in.Version); err != nil {
		return Agent{}, err
	}

	id := ids.New()
	err = tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) error {
		q := store.New(tx)
		if err := q.InsertAgent(ctx, store.InsertAgentParams{
			ID: id, OrgID: orgID, ProjectID: projectID, Slug: in.Slug, Name: in.Name,
			Description: in.Description, CreatedBy: createdBy(ctx),
		}); err != nil {
			return err
		}
		if _, err := s.insertVersion(ctx, q, orgID, id, 1, in.Version); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{
			OrgID: orgID, ProjectID: &projectID, Action: "agent.create", ResourceType: "agent", ResourceID: &id,
			Outcome: audit.OutcomeSuccess, Details: map[string]string{"slug": in.Slug, "version": "1"},
		})
	})
	switch {
	case isPg(err, "23505"):
		return Agent{}, errSlugTaken
	case isPg(err, "23503"): // project biến mất giữa lúc kiểm tra và lúc ghi
		return Agent{}, errNotFound
	case err != nil:
		return Agent{}, internal(err)
	}
	return s.Get(ctx, id)
}

// Get trả về Agent theo id. Agent của tenant khác là "không tồn tại".
func (s *Service) Get(ctx context.Context, id uuid.UUID) (Agent, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return Agent{}, internal(tenancy.ErrNoTenant)
	}
	var row store.GetAgentRow
	err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) (err error) {
		row, err = store.New(tx).GetAgent(ctx, store.GetAgentParams{OrgID: orgID, ID: id})
		return err
	})
	if errors.Is(err, pgxv5.ErrNoRows) {
		return Agent{}, errNotFound
	}
	if err != nil {
		return Agent{}, internal(err)
	}
	return agentFromRow(row.Agent, row.ActiveVersion, row.LatestVersion), nil
}

// requireOpenProject từ chối thay đổi agent trong project đã lưu trữ.
//
// Phải gọi TRƯỚC khi mở transaction ghi, không phải bên trong: nó dùng một kết nối
// riêng, và xin kết nối thứ hai trong lúc đang giữ một transaction (kèm khoá dòng)
// làm các request đồng thời chờ nhau mãi khi pool cạn. Hệ quả là một project vừa
// bị lưu trữ đồng thời có thể lọt qua đúng một thay đổi; việc lưu trữ không đảo
// ngược được nên mọi thay đổi sau đó đều bị chặn.
func (s *Service) requireOpenProject(ctx context.Context, agentID uuid.UUID) error {
	a, err := s.Get(ctx, agentID)
	if err != nil {
		return err
	}
	if a.Status == StatusArchived {
		return errArchived
	}
	p, err := s.projects.GetProject(ctx, a.ProjectID)
	if err != nil {
		return err
	}
	if p.ArchivedAt != nil {
		return errProjectArchived
	}
	return nil
}

// mutate chạy fn với dòng agent đã khoá, trong một transaction. Mọi thay đổi
// trạng thái và việc tạo version đi qua đây nên được tuần tự hoá theo agent.
func (s *Service) mutate(ctx context.Context, id uuid.UUID, ifUpdatedAt *time.Time,
	fn func(tx pgxv5.Tx, q *store.Queries, orgID uuid.UUID, a store.LockAgentRow) error) error {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return internal(tenancy.ErrNoTenant)
	}
	err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) error {
		q := store.New(tx)
		a, err := q.LockAgent(ctx, store.LockAgentParams{OrgID: orgID, ID: id})
		if errors.Is(err, pgxv5.ErrNoRows) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		if ifUpdatedAt != nil && !a.UpdatedAt.Equal(*ifUpdatedAt) {
			return httpx.ErrPreconditionFailed
		}
		return fn(tx, q, orgID, a)
	})
	if err != nil {
		if _, ok := apperr.From(err); ok {
			return err
		}
		return internal(err)
	}
	return nil
}

// Update sửa tên, mô tả, hoặc chuyển giữa active và disabled.
func (s *Service) Update(ctx context.Context, id uuid.UUID, u UpdateInput) (Agent, error) {
	if err := validateMeta(u.Name, u.Description); err != nil {
		return Agent{}, err
	}
	if u.Status != nil && *u.Status != StatusActive && *u.Status != StatusDisabled {
		return Agent{}, invalid("status chỉ nhận active hoặc disabled")
	}
	if err := s.requireOpenProject(ctx, id); err != nil {
		return Agent{}, err
	}
	err := s.mutate(ctx, id, u.IfUpdatedAt, func(tx pgxv5.Tx, q *store.Queries, orgID uuid.UUID, a store.LockAgentRow) error {
		if a.Status == StatusArchived {
			return errArchived
		}
		var fields []string
		if u.Name != nil {
			fields = append(fields, "name")
		}
		if u.Description != nil {
			fields = append(fields, "description")
		}
		status := u.Status
		if status != nil && *status == a.Status {
			status = nil
		}
		if status != nil {
			// Agent chỉ rời draft bằng cách publish một version.
			if a.ActiveVersionID == nil {
				return errNoActiveVersion
			}
			fields = append(fields, "status")
		}
		if err := q.UpdateAgent(ctx, store.UpdateAgentParams{
			OrgID: orgID, ID: id, Name: u.Name, Description: u.Description, Status: status,
		}); err != nil {
			return err
		}
		details := map[string]string{"fields": strings.Join(fields, ",")}
		if status != nil {
			details["status"] = *status
		}
		return audit.Record(ctx, tx, audit.Event{
			OrgID: orgID, ProjectID: &a.ProjectID, Action: "agent.update", ResourceType: "agent", ResourceID: &id,
			Outcome: audit.OutcomeSuccess, Details: details,
		})
	})
	if err != nil {
		return Agent{}, err
	}
	return s.Get(ctx, id)
}

// Archive lưu trữ Agent. Lưu trữ lại là no-op.
func (s *Service) Archive(ctx context.Context, id uuid.UUID) error {
	return s.mutate(ctx, id, nil, func(tx pgxv5.Tx, q *store.Queries, orgID uuid.UUID, a store.LockAgentRow) error {
		if a.Status == StatusArchived {
			return nil
		}
		archived := StatusArchived
		if err := q.UpdateAgent(ctx, store.UpdateAgentParams{OrgID: orgID, ID: id, Status: &archived}); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{
			OrgID: orgID, ProjectID: &a.ProjectID, Action: "agent.archive", ResourceType: "agent", ResourceID: &id,
			Outcome: audit.OutcomeSuccess,
		})
	})
}

func (s *Service) activate(ctx context.Context, tx pgxv5.Tx, q *store.Queries, orgID uuid.UUID, a store.LockAgentRow, v store.AgentVersion) error {
	status := a.Status
	if status == StatusDraft {
		status = StatusActive // publish lần đầu đưa agent rời draft
	}
	if err := q.SetActiveVersion(ctx, store.SetActiveVersionParams{
		OrgID: orgID, ID: a.ID, ActiveVersionID: &v.ID, Status: status,
	}); err != nil {
		return err
	}
	return audit.Record(ctx, tx, audit.Event{
		OrgID: orgID, ProjectID: &a.ProjectID, Action: "agent.publish", ResourceType: "agent", ResourceID: &a.ID,
		Outcome: audit.OutcomeSuccess, Details: map[string]string{"version": strconv.Itoa(int(v.Version))},
	})
}

// SetActiveVersion publish hoặc rollback: chọn version đang hoạt động.
// Execution đang chạy giữ nguyên version nó bắt đầu.
func (s *Service) SetActiveVersion(ctx context.Context, id uuid.UUID, version int) (Agent, error) {
	if err := s.requireOpenProject(ctx, id); err != nil {
		return Agent{}, err
	}
	// Model ghim trong version có thể đã bị tắt hoặc ngừng hỗ trợ sau khi version
	// được tạo: kiểm lại trước khi đưa version vào hoạt động. Runtime vẫn phải kiểm
	// một lần nữa ở mỗi execution.
	if s.models != nil {
		v, err := s.GetVersion(ctx, id, version)
		if err != nil {
			return Agent{}, err
		}
		var cfg api.AgentConfig
		if err := json.Unmarshal(v.Config, &cfg); err != nil {
			return Agent{}, internal(err)
		}
		if cfg.Model.Policy == "fixed" && cfg.Model.FixedModelId != nil {
			if err := s.models.ValidateModel(ctx, *cfg.Model.FixedModelId); err != nil {
				return Agent{}, err
			}
		}
	}
	err := s.mutate(ctx, id, nil, func(tx pgxv5.Tx, q *store.Queries, orgID uuid.UUID, a store.LockAgentRow) error {
		if a.Status == StatusArchived {
			return errArchived
		}
		v, err := q.GetAgentVersion(ctx, store.GetAgentVersionParams{OrgID: orgID, AgentID: id, Version: int32(version)}) //nolint:gosec // đã chặn ở handler
		if errors.Is(err, pgxv5.ErrNoRows) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		if a.ActiveVersionID != nil && *a.ActiveVersionID == v.ID {
			return nil
		}
		return s.activate(ctx, tx, q, orgID, a, v)
	})
	if err != nil {
		return Agent{}, err
	}
	return s.Get(ctx, id)
}

// CreateVersion tạo version bất biến kế tiếp. projectID là project của agent
// (người gọi đã đọc agent để kiểm quyền).
func (s *Service) CreateVersion(ctx context.Context, id, projectID uuid.UUID, in VersionInput) (Version, error) {
	if err := s.requireOpenProject(ctx, id); err != nil {
		return Version{}, err
	}
	if err := s.validateVersion(ctx, projectID, in); err != nil {
		return Version{}, err
	}
	var out store.AgentVersion
	err := s.mutate(ctx, id, nil, func(tx pgxv5.Tx, q *store.Queries, orgID uuid.UUID, a store.LockAgentRow) error {
		if a.Status == StatusArchived {
			return errArchived
		}
		latest, err := q.GetLatestAgentVersion(ctx, store.GetLatestAgentVersionParams{OrgID: orgID, AgentID: id})
		if err != nil {
			return err
		}
		// Nội dung trùng version mới nhất: trả về version đó thay vì tạo bản sao.
		// Nhờ vậy gửi lại một request tạo version (kể cả khi Idempotency-Key bị
		// coi là bỏ dở) không sinh thêm version.
		if _, hash, herr := canonicalConfig(in); herr != nil {
			return herr
		} else if bytes.Equal(hash, latest.ConfigHash) {
			out = latest
			if in.Activate && (a.ActiveVersionID == nil || *a.ActiveVersionID != latest.ID) {
				return s.activate(ctx, tx, q, orgID, a, latest)
			}
			return nil
		}
		out, err = s.insertVersion(ctx, q, orgID, id, int(latest.Version)+1, in)
		if err != nil {
			return err
		}
		if err := audit.Record(ctx, tx, audit.Event{
			OrgID: orgID, ProjectID: &a.ProjectID, Action: "agent.version.create", ResourceType: "agent", ResourceID: &id,
			Outcome: audit.OutcomeSuccess, Details: map[string]string{"version": strconv.Itoa(int(out.Version))},
		}); err != nil {
			return err
		}
		if in.Activate {
			return s.activate(ctx, tx, q, orgID, a, out)
		}
		return nil
	})
	if err != nil {
		return Version{}, err
	}
	return versionFromRow(out), nil
}

// GetVersion trả về một version theo số.
func (s *Service) GetVersion(ctx context.Context, id uuid.UUID, version int) (Version, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return Version{}, internal(tenancy.ErrNoTenant)
	}
	var row store.AgentVersion
	err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) (err error) {
		row, err = store.New(tx).GetAgentVersion(ctx, store.GetAgentVersionParams{OrgID: orgID, AgentID: id, Version: int32(version)}) //nolint:gosec // đã chặn ở handler
		return err
	})
	if errors.Is(err, pgxv5.ErrNoRows) {
		return Version{}, errNotFound
	}
	if err != nil {
		return Version{}, internal(err)
	}
	return versionFromRow(row), nil
}

// ListVersions liệt kê version, mới nhất trước. cursor là số version cuối của trang trước.
func (s *Service) ListVersions(ctx context.Context, id uuid.UUID, limit int, cursor string) (versions []Version, next string, err error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return nil, "", internal(tenancy.ErrNoTenant)
	}
	var before *int32
	if cursor != "" {
		n, perr := strconv.ParseInt(cursor, 10, 32)
		if perr != nil || n < 1 {
			return nil, "", pagination.ErrInvalidCursor
		}
		b := int32(n)
		before = &b
	}
	var rows []store.AgentVersion
	err = tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) (err error) {
		rows, err = store.New(tx).ListAgentVersions(ctx, store.ListAgentVersionsParams{
			OrgID: orgID, AgentID: id, BeforeVersion: before, RowLimit: int32(limit + 1)}) //nolint:gosec // limit ≤ 200
		return err
	})
	if err != nil {
		return nil, "", internal(err)
	}
	if len(rows) > limit {
		rows = rows[:limit]
		next = strconv.Itoa(int(rows[len(rows)-1].Version))
	}
	versions = make([]Version, len(rows))
	for i, r := range rows {
		versions[i] = versionFromRow(r)
	}
	return versions, next, nil
}

// escapeLike biến chuỗi tìm kiếm của người dùng thành mẫu ILIKE "chứa", vô hiệu
// hoá ký tự đại diện do người dùng nhập.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(s) + "%"
}

// List liệt kê Agent của một project, phân trang keyset.
func (s *Service) List(ctx context.Context, projectID uuid.UUID, p ListParams) (agents []Agent, next string, err error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return nil, "", internal(tenancy.ErrNoTenant)
	}
	if p.Sort == "" {
		p.Sort = "name"
	}
	var status, search *string
	switch p.Status {
	case "":
	case StatusDraft, StatusActive, StatusDisabled, StatusArchived:
		status = &p.Status
	default:
		return nil, "", apperr.New(apperr.KindInvalid, "invalid_parameter", "status không hợp lệ")
	}
	if p.Search != "" {
		if !validText(p.Search, maxNameRunes, false) {
			return nil, "", apperr.New(apperr.KindInvalid, "invalid_parameter", "q không hợp lệ")
		}
		like := escapeLike(p.Search)
		search = &like
	}
	byName := strings.HasSuffix(p.Sort, "name")
	desc := strings.HasPrefix(p.Sort, "-")
	if key := strings.TrimPrefix(p.Sort, "-"); key != "name" && key != "created_at" {
		return nil, "", apperr.New(apperr.KindInvalid, "invalid_parameter",
			"sort phải là một trong: name, -name, created_at, -created_at")
	}
	cur, err := pagination.Decode(p.Cursor, p.Sort)
	if err != nil {
		return nil, "", err
	}
	var afterID *uuid.UUID
	var afterName *string
	var afterCreated *time.Time
	if cur != nil {
		afterID = &cur.ID
		if byName {
			afterName = &cur.Value
		} else {
			t, perr := time.Parse(time.RFC3339Nano, cur.Value)
			if perr != nil {
				return nil, "", pagination.ErrInvalidCursor
			}
			afterCreated = &t
		}
	}
	limit := int32(p.Limit + 1) //nolint:gosec // limit ≤ 200

	type row struct {
		a      store.Agent
		active *int32
		latest int32
	}
	var rows []row
	err = tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) error {
		q := store.New(tx)
		if byName {
			rs, err := q.ListAgentsByName(ctx, store.ListAgentsByNameParams{
				OrgID: orgID, ProjectID: projectID, Status: status, Search: search,
				AfterName: afterName, AfterID: afterID, Descending: desc, RowLimit: limit})
			for _, r := range rs {
				rows = append(rows, row{r.Agent, r.ActiveVersion, r.LatestVersion})
			}
			return err
		}
		rs, err := q.ListAgentsByCreated(ctx, store.ListAgentsByCreatedParams{
			OrgID: orgID, ProjectID: projectID, Status: status, Search: search,
			AfterCreated: afterCreated, AfterID: afterID, Descending: desc, RowLimit: limit})
		for _, r := range rs {
			rows = append(rows, row{r.Agent, r.ActiveVersion, r.LatestVersion})
		}
		return err
	})
	if err != nil {
		return nil, "", internal(err)
	}
	if len(rows) > p.Limit {
		rows = rows[:p.Limit]
		last := rows[len(rows)-1].a
		c := pagination.Cursor{Sort: p.Sort, ID: last.ID, Value: last.CreatedAt.UTC().Format(time.RFC3339Nano)}
		if byName {
			c.Value = last.Name
		}
		next = pagination.Encode(c)
	}
	agents = make([]Agent, len(rows))
	for i, r := range rows {
		agents[i] = agentFromRow(r.a, r.active, r.latest)
	}
	return agents, next, nil
}

func agentFromRow(a store.Agent, active *int32, latest int32) Agent {
	out := Agent{ID: a.ID, ProjectID: a.ProjectID, Slug: a.Slug, Name: a.Name, Description: a.Description,
		Status: a.Status, LatestVersion: int(latest), CreatedAt: a.CreatedAt.UTC(), UpdatedAt: a.UpdatedAt.UTC()}
	if active != nil {
		n := int(*active)
		out.ActiveVersion = &n
	}
	return out
}

func versionFromRow(v store.AgentVersion) Version {
	return Version{ID: v.ID, AgentID: v.AgentID, Version: int(v.Version), SystemPrompt: v.SystemPrompt,
		Config: v.Config, ChangeNote: v.ChangeNote, CreatedBy: v.CreatedBy, CreatedAt: v.CreatedAt.UTC()}
}

// Runnable là một Agent Version sẵn sàng để chạy.
type Runnable struct {
	Agent   Agent
	Version Version
	Config  api.AgentConfig
}

var errNotActive = apperr.New(apperr.KindConflict, "agent_not_active", "agent không ở trạng thái active")

// ListActive trả về các Agent đang hoạt động của một project, theo tên, tối đa limit.
func (s *Service) ListActive(ctx context.Context, projectID uuid.UUID, limit int) ([]Agent, error) {
	agents, _, err := s.List(ctx, projectID, ListParams{Limit: limit, Status: "active", Sort: "name"})
	return agents, err
}

// Runnable trả về version sẽ chạy của một Agent: version được chỉ định, hoặc version
// đang hoạt động. Người gọi kiểm trạng thái của Agent và quyền chạy version đó.
func (s *Service) Runnable(ctx context.Context, agentID uuid.UUID, version *int) (Runnable, error) {
	a, err := s.Get(ctx, agentID)
	if err != nil {
		return Runnable{}, err
	}
	n := a.ActiveVersion
	if version != nil {
		n = version
	}
	if n == nil {
		return Runnable{}, errNoActiveVersion
	}
	v, err := s.GetVersion(ctx, agentID, *n)
	if err != nil {
		return Runnable{}, err
	}
	out := Runnable{Agent: a, Version: v}
	if err := json.Unmarshal(v.Config, &out.Config); err != nil {
		return Runnable{}, internal(err)
	}
	return out, nil
}

// ToolRule là một mục trong danh sách tool của một Agent Version.
type ToolRule struct {
	ServerID uuid.UUID
	Allow    []string
	Deny     []string
	// RequireApproval: mục này đặt require_approval: always (chặt hơn policy của tool).
	RequireApproval bool
}

// ToolRules trả về danh sách tool mà một Agent Version được phép dùng, theo id của version.
// Trả thêm max_tool_calls của version: Gateway áp lại trần này trên số tool call nó đã cho chạy.
func (s *Service) ToolRules(ctx context.Context, versionID uuid.UUID) ([]ToolRule, int, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return nil, 0, internal(tenancy.ErrNoTenant)
	}
	var row store.GetAgentVersionByIDRow
	err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) (err error) {
		row, err = store.New(tx).GetAgentVersionByID(ctx, store.GetAgentVersionByIDParams{OrgID: orgID, ID: versionID})
		return err
	})
	if errors.Is(err, pgxv5.ErrNoRows) {
		return nil, 0, errNotFound
	}
	if err != nil {
		return nil, 0, internal(err)
	}
	// Agent bị tắt hoặc lưu trữ thì execution đang chạy của nó mất quyền gọi tool ngay.
	if row.AgentStatus != "active" {
		return nil, 0, errNotActive
	}
	var cfg api.AgentConfig
	if err := json.Unmarshal(row.Config, &cfg); err != nil {
		return nil, 0, internal(err)
	}
	rules := make([]ToolRule, 0, len(cfg.Tools))
	for _, t := range cfg.Tools {
		r := ToolRule{ServerID: t.ServerId, Allow: t.Allow}
		if t.Deny != nil {
			r.Deny = *t.Deny
		}
		r.RequireApproval = t.RequireApproval != nil && *t.RequireApproval == api.Always
		rules = append(rules, r)
	}
	return rules, cfg.Limits.MaxToolCalls, nil
}

// ModelPolicy là phần cấu hình model và giới hạn của một Agent Version mà Gateway
// dùng để ràng một lượt gọi model.
type ModelPolicy struct {
	ModelID         uuid.UUID // uuid.Nil: version không ghim model (policy tiered)
	MaxOutputTokens int       // 0 = Agent Version không đặt
	MaxIterations   int
}

// FixedModel trả về các giới hạn của một Agent Version theo id của nó, cùng model
// được ghim (model.policy: fixed). Version dùng tier có ModelID là uuid.Nil.
func (s *Service) FixedModel(ctx context.Context, versionID uuid.UUID) (ModelPolicy, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return ModelPolicy{}, internal(tenancy.ErrNoTenant)
	}
	var row store.GetAgentVersionByIDRow
	err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) (err error) {
		row, err = store.New(tx).GetAgentVersionByID(ctx, store.GetAgentVersionByIDParams{OrgID: orgID, ID: versionID})
		return err
	})
	if errors.Is(err, pgxv5.ErrNoRows) {
		return ModelPolicy{}, errNotFound
	}
	if err != nil {
		return ModelPolicy{}, internal(err)
	}
	if row.AgentStatus != "active" {
		return ModelPolicy{}, errNotActive
	}
	var cfg api.AgentConfig
	if err := json.Unmarshal(row.Config, &cfg); err != nil {
		return ModelPolicy{}, internal(err)
	}
	out := ModelPolicy{MaxIterations: cfg.Limits.MaxIterations}
	if cfg.Model.Policy == "fixed" && cfg.Model.FixedModelId != nil {
		out.ModelID = *cfg.Model.FixedModelId
	}
	if cfg.Model.MaxOutputTokens != nil {
		out.MaxOutputTokens = *cfg.Model.MaxOutputTokens
	}
	return out, nil
}

// ServerInUse cho biết có Agent đang hoạt động nào mà version đang hoạt động của nó
// tham chiếu MCP server này hay không.
func (s *Service) ServerInUse(ctx context.Context, serverID uuid.UUID) (bool, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return false, internal(tenancy.ErrNoTenant)
	}
	var n int32
	err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) (err error) {
		n, err = store.New(tx).ActiveAgentsUsingServer(ctx, store.ActiveAgentsUsingServerParams{OrgID: orgID, ServerID: serverID})
		return err
	})
	if err != nil {
		return false, internal(err)
	}
	return n > 0, nil
}
