package registry

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	pgxv5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/hqgroups/mcp/internal/audit"
	"github.com/hqgroups/mcp/internal/mcp/registry/store"
	"github.com/hqgroups/mcp/internal/platform/identity"
	"github.com/hqgroups/mcp/internal/platform/tenancy"
	apperr "github.com/hqgroups/mcp/internal/shared/errors"
	"github.com/hqgroups/mcp/internal/shared/httpx"
	"github.com/hqgroups/mcp/internal/shared/ids"
	"github.com/hqgroups/mcp/internal/shared/safehttp"
)

// Phần ghi của Registry cho tenant: đăng ký MCP server NGOÀI, discovery, duyệt
// version, phân loại tool (mcp-platform §5).
//
// Server ngoài không được tin: annotation của nó không bao giờ dùng để phân loại,
// tool mới phát hiện luôn ở mức an toàn nhất và bị tắt, version đầu tiên cũng phải
// được duyệt. Endpoint do tenant cung cấp đi qua SSRF guard lúc đăng ký và ở mỗi lần
// gọi. Process api không gọi MCP server: discovery do Gateway thực hiện và trả ảnh
// chụp về để api ghi.

// Discoverer chạy discovery cho một server của org (qua Gateway).
type Discoverer interface {
	Discover(ctx context.Context, serverID uuid.UUID) (Snapshot, error)
}

// SecretWriter là thứ Registry cần từ Secrets: ghi (không đọc) credential của server.
type SecretWriter interface {
	Put(ctx context.Context, tx pgxv5.Tx, orgID uuid.UUID, name, kind string, plaintext []byte, createdBy *uuid.UUID) (uuid.UUID, error)
}

// AgentUsage cho biết một server có đang được Agent Version đang hoạt động tham chiếu không.
type AgentUsage interface {
	ServerInUse(ctx context.Context, serverID uuid.UUID) (bool, error)
}

// Writes là phụ thuộc của phần ghi.
type Writes struct {
	Policy     safehttp.Policy
	Discoverer Discoverer   // nil: discovery trả 503
	Secrets    SecretWriter // nil: auth_method org_secret bị từ chối
	Agents     AgentUsage
}

// WithWrites bật phần ghi cho tenant.
func (s *Service) WithWrites(w Writes) *Service {
	s.w = &w
	return s
}

// RegisterExternal là đầu vào đăng ký một server ngoài.
type RegisterExternal struct {
	Slug        string
	Name        string
	Description *string
	Endpoint    string
	AuthMethod  string  // none | org_secret
	Token       *string // bearer token, bắt buộc với org_secret
}

// ServerUpdate sửa metadata, trạng thái hoặc credential của server.
type ServerUpdate struct {
	Name        *string
	Description *string
	Status      *string // active | disabled
	Token       *string
	IfUpdatedAt *time.Time
}

// Version là một ảnh chụp discovery của server.
type Version struct {
	ID              uuid.UUID
	ServerID        uuid.UUID
	Version         int
	ReportedVersion *string
	ProtocolVersion string
	ReviewStatus    string
	ReviewedBy      *uuid.UUID
	ReviewedAt      *time.Time
	DiscoveredAt    time.Time
	ToolCount       int
}

var (
	errSlugTaken  = apperr.New(apperr.KindConflict, "slug_taken", "slug đã được dùng")
	errReadOnly   = apperr.New(apperr.KindUnavailable, "registry_read_only", "process này chưa được cấu hình để ghi Registry")
	errInUse      = apperr.New(apperr.KindConflict, "server_in_use", "server đang được Agent Version đang hoạt động tham chiếu")
	errDisabled   = apperr.New(apperr.KindConflict, "server_disabled", "server đang bị tắt")
	errNoDiscover = apperr.New(apperr.KindUnavailable, "discovery_unavailable", "không chạy được discovery lúc này; thử lại sau").AsRetryable()
)

func validText(s string, max int) bool {
	return strings.TrimSpace(s) != "" && utf8.RuneCountInString(s) <= max && utf8.ValidString(s) &&
		!strings.ContainsFunc(s, func(r rune) bool { return r < ' ' && r != '\n' || r == 0x7f })
}

func userID(ctx context.Context) *uuid.UUID {
	if p, ok := identity.PrincipalFrom(ctx); ok && p.Type == identity.PrincipalUser {
		return &p.ID
	}
	return nil
}

// asAPI chuyển lỗi của các hàm Registry (lỗi nghiệp vụ dạng văn bản, hoặc lỗi
// database) thành lỗi API.
func asAPI(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := apperr.From(err); ok {
		return err
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return internal(err)
	}
	return apperr.New(apperr.KindConflict, "registry_conflict", strings.TrimPrefix(err.Error(), "registry: "))
}

func (s *Service) checkToken(method string, token *string) error {
	switch method {
	case "none":
		if token != nil {
			return invalid("credential chỉ dùng với auth_method org_secret")
		}
	case "org_secret":
		if s.w.Secrets == nil {
			return invalid("auth_method org_secret: process này chưa được cấu hình để lưu secret")
		}
		if token == nil || strings.TrimSpace(*token) == "" || len(*token) > 4096 ||
			strings.ContainsFunc(*token, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
			return invalid("credential.token là bắt buộc với auth_method org_secret (tối đa 4096 ký tự, không có khoảng trắng)")
		}
	default:
		return invalid("auth_method: chỉ hỗ trợ none và org_secret cho server ngoài")
	}
	return nil
}

func (s *Service) putToken(ctx context.Context, tx pgxv5.Tx, orgID, serverID uuid.UUID, token string) error {
	secretID, err := s.w.Secrets.Put(ctx, tx, orgID, "mcp-server/"+serverID.String(), "api_key", []byte(token), userID(ctx))
	if err != nil {
		return err
	}
	return store.New(tx).SetServerAuth(ctx, store.SetServerAuthParams{OrgID: &orgID, ID: serverID, SecretID: &secretID})
}

// Register đăng ký một MCP server ngoài cho org. Server chưa phục vụ tool nào cho tới
// khi có một lần discovery và version của nó được duyệt.
func (s *Service) Register(ctx context.Context, in RegisterExternal) (Server, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return Server{}, internal(tenancy.ErrNoTenant)
	}
	if s.w == nil {
		return Server{}, errReadOnly
	}
	if !slugRe.MatchString(in.Slug) {
		return Server{}, invalid("slug phải khớp ^[a-z0-9][a-z0-9-]{1,62}$")
	}
	if !validText(in.Name, 200) || (in.Description != nil && utf8.RuneCountInString(*in.Description) > 2000) {
		return Server{}, invalid("name phải có từ 1 đến 200 ký tự; description tối đa 2000")
	}
	if err := s.checkToken(in.AuthMethod, in.Token); err != nil {
		return Server{}, err
	}
	// SSRF guard lúc đăng ký: dạng URL và mọi địa chỉ nó phân giải tới. Thông báo
	// không nói địa chỉ nào bị từ chối.
	u, err := s.w.Policy.Check(ctx, in.Endpoint)
	if err != nil {
		return Server{}, invalid("endpoint không được chấp nhận: phải là URL https công khai, cổng được phép, không trỏ tới địa chỉ nội bộ")
	}
	// Khoá nhét vào query string sẽ nằm ở dạng rõ trong Registry, hiện ra cho mọi thành
	// viên đọc được server, và đi vào log: credential phải đi qua org_secret.
	if u.RawQuery != "" || u.ForceQuery {
		return Server{}, invalid("endpoint không được có query string; nếu server cần khoá, dùng auth_method org_secret")
	}
	endpoint := u.String()
	id := ids.New()
	err = tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) error {
		q := store.New(tx)
		if _, err := q.InsertServer(ctx, store.InsertServerParams{ID: id, OrgID: &orgID, Slug: in.Slug, Name: in.Name,
			Description: in.Description, Kind: KindExternal, Endpoint: &endpoint, AuthMethod: in.AuthMethod}); err != nil {
			return err
		}
		if in.Token != nil {
			if err := s.putToken(ctx, tx, orgID, id, *in.Token); err != nil {
				return err
			}
		}
		return audit.Record(ctx, tx, audit.Event{OrgID: orgID, Action: "mcp_server.register", ResourceType: "mcp_server", ResourceID: &id,
			Outcome: audit.OutcomeSuccess, Details: map[string]string{"slug": in.Slug, "kind": KindExternal, "host": u.Hostname(), "auth_method": in.AuthMethod}})
	})
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		return Server{}, errSlugTaken
	}
	if err != nil {
		return Server{}, internal(err)
	}
	return s.GetServer(ctx, id)
}

// orgServer đọc server của chính org; server của platform hoặc managed server không
// sửa được qua API tenant.
func (s *Service) orgServer(ctx context.Context, tx pgxv5.Tx, orgID, id uuid.UUID) (store.GetOrgServerRow, error) {
	row, err := store.New(tx).GetOrgServer(ctx, store.GetOrgServerParams{OrgID: &orgID, ID: id})
	if errors.Is(err, pgxv5.ErrNoRows) {
		return row, errNotFound
	}
	if err == nil && row.Kind != KindExternal {
		return row, apperr.New(apperr.KindConflict, "server_managed", "managed server do platform quản lý, không sửa được qua API")
	}
	return row, err
}

// Update sửa metadata, bật / tắt server, hoặc xoay credential.
func (s *Service) Update(ctx context.Context, id uuid.UUID, u ServerUpdate) (Server, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return Server{}, internal(tenancy.ErrNoTenant)
	}
	if s.w == nil {
		return Server{}, errReadOnly
	}
	if (u.Name != nil && !validText(*u.Name, 200)) || (u.Description != nil && utf8.RuneCountInString(*u.Description) > 2000) {
		return Server{}, invalid("name phải có từ 1 đến 200 ký tự; description tối đa 2000")
	}
	if u.Status != nil && *u.Status != "active" && *u.Status != "disabled" {
		return Server{}, invalid("status chỉ nhận active hoặc disabled")
	}
	err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) error {
		row, err := s.orgServer(ctx, tx, orgID, id)
		if err != nil {
			return err
		}
		var fields []string
		status := u.Status
		if status != nil {
			fields = append(fields, "status")
			// Bật lại server chưa có version nào được duyệt đưa nó về registered.
			if *status == "active" && row.ActiveVersionID == nil {
				registered := "registered"
				status = &registered
			}
		}
		if u.Name != nil {
			fields = append(fields, "name")
		}
		if u.Description != nil {
			fields = append(fields, "description")
		}
		if u.Token != nil {
			if err := s.checkToken("org_secret", u.Token); err != nil {
				return err
			}
			if row.AuthMethod != "org_secret" {
				return invalid("credential: server này dùng auth_method " + row.AuthMethod)
			}
			fields = append(fields, "credential")
		}
		// If-Match được kiểm trước mọi lần ghi khác của request này.
		n, err := store.New(tx).UpdateServerMeta(ctx, store.UpdateServerMetaParams{OrgID: &orgID, ID: id, Name: u.Name,
			Description: u.Description, Status: status, IfUpdatedAt: u.IfUpdatedAt})
		if err != nil {
			return err
		}
		if n == 0 {
			return httpx.ErrPreconditionFailed
		}
		if u.Token != nil {
			if err := s.putToken(ctx, tx, orgID, id, *u.Token); err != nil {
				return err
			}
		}
		return audit.Record(ctx, tx, audit.Event{OrgID: orgID, Action: "mcp_server.update", ResourceType: "mcp_server", ResourceID: &id,
			Outcome: audit.OutcomeSuccess, Details: map[string]string{"fields": strings.Join(fields, ",")}})
	})
	if err != nil {
		return Server{}, asAPI(err)
	}
	return s.GetServer(ctx, id)
}

// Archive lưu trữ server. Server đang được Agent Version đang hoạt động tham chiếu thì từ chối.
func (s *Service) Archive(ctx context.Context, id uuid.UUID) error {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return internal(tenancy.ErrNoTenant)
	}
	if s.w == nil {
		return errReadOnly
	}
	// Kiểm tra cần kết nối riêng: làm trước khi mở transaction.
	if s.w.Agents != nil {
		used, err := s.w.Agents.ServerInUse(ctx, id)
		if err != nil {
			return err
		}
		if used {
			return errInUse
		}
	}
	err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) error {
		if _, err := s.orgServer(ctx, tx, orgID, id); err != nil {
			return err
		}
		if _, err := store.New(tx).ArchiveServer(ctx, store.ArchiveServerParams{OrgID: &orgID, ID: id}); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{OrgID: orgID, Action: "mcp_server.archive", ResourceType: "mcp_server", ResourceID: &id,
			Outcome: audit.OutcomeSuccess})
	})
	return asAPI(err)
}

// DiscoverNow chạy discovery qua Gateway và ghi kết quả. Định nghĩa tool không đổi thì
// không ghi gì; có đổi thì tạo version chờ duyệt (kể cả version đầu tiên) và tạm khoá
// những tool của version đang hoạt động có định nghĩa đã đổi.
func (s *Service) DiscoverNow(ctx context.Context, id uuid.UUID) (ApplyResult, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return ApplyResult{}, internal(tenancy.ErrNoTenant)
	}
	if s.w == nil || s.w.Discoverer == nil {
		return ApplyResult{}, errNoDiscover
	}
	var slug string
	if err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) error {
		row, err := s.orgServer(ctx, tx, orgID, id)
		slug = row.Slug
		if err == nil && row.Status == "disabled" {
			return errDisabled
		}
		return err
	}); err != nil {
		return ApplyResult{}, asAPI(err)
	}
	// Lời gọi mạng nằm ngoài transaction.
	snap, err := s.w.Discoverer.Discover(ctx, id)
	if err != nil {
		return ApplyResult{}, err
	}
	var res ApplyResult
	err = tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) error {
		var err error
		if res, err = ApplyDiscovery(ctx, tx, &orgID, slug, snap, false); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{OrgID: orgID, Action: "mcp_server.discover", ResourceType: "mcp_server", ResourceID: &id,
			Outcome: audit.OutcomeSuccess, Details: map[string]string{"version": strconv.Itoa(res.Version), "changed": strconv.FormatBool(res.Changed),
				"pending": strconv.FormatBool(res.Pending), "tools": strconv.Itoa(len(snap.Tools)), "locked_tools": strings.Join(res.LockedTools, ",")}})
	})
	if err != nil {
		return ApplyResult{}, asAPI(err)
	}
	return res, nil
}

// Versions liệt kê version của một server của org, mới nhất trước.
func (s *Service) Versions(ctx context.Context, id uuid.UUID, reviewStatus string, limit, before int) (out []Version, next int, err error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return nil, 0, internal(tenancy.ErrNoTenant)
	}
	p := store.ListVersionsParams{ServerID: id, BeforeVersion: int32(before), RowLimit: int32(limit + 1)} //nolint:gosec // đã chặn ở handler
	if reviewStatus != "" {
		p.ReviewStatus = &reviewStatus
	}
	var rows []store.ListVersionsRow
	err = tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) error {
		if _, err := store.New(tx).GetOrgServer(ctx, store.GetOrgServerParams{OrgID: &orgID, ID: id}); err != nil {
			if errors.Is(err, pgxv5.ErrNoRows) {
				return errNotFound
			}
			return err
		}
		var err error
		rows, err = store.New(tx).ListVersions(ctx, p)
		return err
	})
	if err != nil {
		return nil, 0, asAPI(err)
	}
	if len(rows) > limit {
		rows = rows[:limit]
		next = int(rows[limit-1].Version)
	}
	out = make([]Version, len(rows))
	for i, r := range rows {
		out[i] = Version{ID: r.ID, ServerID: r.ServerID, Version: int(r.Version), ReportedVersion: r.ReportedVersion,
			ProtocolVersion: r.ProtocolVersion, ReviewStatus: r.ReviewStatus, ReviewedBy: r.ReviewedBy, ReviewedAt: r.ReviewedAt,
			DiscoveredAt: r.DiscoveredAt.UTC(), ToolCount: int(r.ToolCount)}
	}
	return out, next, nil
}

// ReviewVersion duyệt hoặc từ chối một version chờ duyệt. Duyệt thì version trở thành
// version đang hoạt động; tool của server ngoài vẫn bị tắt cho tới khi được phân loại.
func (s *Service) ReviewVersion(ctx context.Context, id uuid.UUID, version int, approve bool) error {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return internal(tenancy.ErrNoTenant)
	}
	if s.w == nil {
		return errReadOnly
	}
	err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) error {
		row, err := s.orgServer(ctx, tx, orgID, id)
		if err != nil {
			return err
		}
		if err := Review(ctx, tx, &orgID, row.Slug, version, approve, userID(ctx)); err != nil {
			return err
		}
		decision := "rejected"
		if approve {
			decision = "approved"
		}
		return audit.Record(ctx, tx, audit.Event{OrgID: orgID, Action: "mcp_server.review", ResourceType: "mcp_server", ResourceID: &id,
			Outcome: audit.OutcomeSuccess, Details: map[string]string{"version": strconv.Itoa(version), "decision": decision}})
	})
	return asAPI(err)
}

// GetTool trả về một tool của version đang hoạt động mà tenant thấy được.
func (s *Service) GetTool(ctx context.Context, id uuid.UUID) (Tool, error) {
	tools, err := s.ActiveTools(ctx, ToolFilter{ToolID: &id})
	if err != nil {
		return Tool{}, err
	}
	if len(tools) == 0 {
		return Tool{}, errNotFound
	}
	return tools[0], nil
}

var (
	riskLevels  = []string{"read", "write", "destructive", "financial"}
	sideEffects = []string{"none", "idempotent", "non_idempotent"}
	sensitivity = []string{"public", "internal", "sensitive"}
)

func oneOf(v *string, allowed []string) bool {
	if v == nil {
		return true
	}
	for _, a := range allowed {
		if *v == a {
			return true
		}
	}
	return false
}

// ClassifyTool đặt thuộc tính chính sách cho một tool của server ngoài thuộc org.
func (s *Service) ClassifyTool(ctx context.Context, id uuid.UUID, c Classification) (Tool, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return Tool{}, internal(tenancy.ErrNoTenant)
	}
	if s.w == nil {
		return Tool{}, errReadOnly
	}
	if !oneOf(c.RiskLevel, riskLevels) || !oneOf(c.SideEffect, sideEffects) || !oneOf(c.DataSensitivity, sensitivity) {
		return Tool{}, invalid("risk_level, side_effect hoặc data_sensitivity không hợp lệ")
	}
	tool, err := s.GetTool(ctx, id)
	if err != nil {
		return Tool{}, err
	}
	err = tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) error {
		row, err := s.orgServer(ctx, tx, orgID, tool.ServerID)
		if err != nil {
			return err
		}
		if err := Classify(ctx, tx, &orgID, row.Slug, tool.Name, c); err != nil {
			return err
		}
		details := map[string]string{"tool": tool.QualifiedName()}
		if c.Enabled != nil {
			details["enabled"] = strconv.FormatBool(*c.Enabled)
		}
		if c.RiskLevel != nil {
			details["risk_level"] = *c.RiskLevel
		}
		return audit.Record(ctx, tx, audit.Event{OrgID: orgID, Action: "mcp_tool.classify", ResourceType: "mcp_tool", ResourceID: &id,
			Outcome: audit.OutcomeSuccess, Details: details})
	})
	if err != nil {
		return Tool{}, asAPI(err)
	}
	return s.GetTool(ctx, id)
}

// OrgServerEndpoint là thông tin Gateway cần để gọi một server ngoài của org.
type OrgServerEndpoint struct {
	Slug       string
	Endpoint   string
	AuthMethod string
	SecretID   *uuid.UUID
}

// ExternalEndpoint trả về endpoint và cách xác thực của một server ngoài thuộc org
// của ctx. Dùng cho discovery ở Gateway.
func (s *Service) ExternalEndpoint(ctx context.Context, id uuid.UUID) (OrgServerEndpoint, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return OrgServerEndpoint{}, internal(tenancy.ErrNoTenant)
	}
	var row store.GetOrgServerRow
	err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) (err error) {
		row, err = s.orgServer(ctx, tx, orgID, id)
		return err
	})
	if err != nil {
		return OrgServerEndpoint{}, asAPI(err)
	}
	if row.Endpoint == nil || row.Status == "disabled" {
		return OrgServerEndpoint{}, errDisabled
	}
	return OrgServerEndpoint{Slug: row.Slug, Endpoint: *row.Endpoint, AuthMethod: row.AuthMethod, SecretID: row.SecretID}, nil
}
