package registry

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"
	pgxv5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hqgroups/mcp/internal/api"
	"github.com/hqgroups/mcp/internal/mcp/registry/store"
	"github.com/hqgroups/mcp/internal/platform/tenancy"
	apperr "github.com/hqgroups/mcp/internal/shared/errors"
	"github.com/hqgroups/mcp/internal/shared/pagination"
)

// Phạm vi của một server.
const (
	ScopePlatform     = "platform"
	ScopeOrganization = "organization"
)

// Server là một MCP server trong Registry.
type Server struct {
	ID              uuid.UUID
	Scope           string
	Slug            string
	Name            string
	Description     *string
	Kind            string
	Transport       string
	Endpoint        *string
	AuthMethod      string
	Status          string
	HealthStatus    string
	HealthCheckedAt *time.Time
	ActiveVersion   *int
	PendingVersion  *int
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Tool là một tool của version đang hoạt động, kèm thuộc tính chính sách và
// thông tin để Gateway gọi upstream.
type Tool struct {
	ID              uuid.UUID
	ServerID        uuid.UUID
	ServerSlug      string
	ServerKind      string
	Endpoint        string
	AuthMethod      string
	Name            string
	Description     string
	InputSchema     json.RawMessage
	DefinitionHash  []byte
	Enabled         bool
	RiskLevel       string
	SideEffect      string
	DataSensitivity string
	Egress          bool
	Cacheable       bool
	Timeout         time.Duration
	MaxResultBytes  int
	// SecretID là secret chứa credential của server ngoài (auth_method org_secret).
	SecretID *uuid.UUID
}

// QualifiedName là tên tool trong server ảo tổng hợp của Gateway: "<server>.<tool>".
func (t Tool) QualifiedName() string { return t.ServerSlug + "." + t.Name }

var errNotFound = apperr.New(apperr.KindNotFound, "not_found", "tài nguyên không tồn tại")

func invalid(msg string) error { return apperr.New(apperr.KindUnprocessable, "validation_failed", msg) }
func internal(err error) error { return apperr.Wrap(err, apperr.KindInternal, "internal_error", "") }

const (
	sortCreated  = "-created_at"
	maxToolsRead = 2000
)

// Service là phần đọc của Registry. Tenant luôn lấy từ context; mỗi tenant thấy
// server của mình và server của platform.
type Service struct {
	db *pgxpool.Pool
	w  *Writes // nil: process này chỉ đọc Registry
}

// NewService tạo Service.
func NewService(db *pgxpool.Pool) *Service { return &Service{db: db} }

func (s *Service) servers(ctx context.Context, id *uuid.UUID, limit int, cursor string) ([]Server, string, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return nil, "", internal(tenancy.ErrNoTenant)
	}
	var after *time.Time
	var afterID *uuid.UUID
	if c, err := pagination.Decode(cursor, sortCreated); err != nil {
		return nil, "", err
	} else if c != nil {
		t, perr := time.Parse(time.RFC3339Nano, c.Value)
		if perr != nil {
			return nil, "", pagination.ErrInvalidCursor
		}
		after, afterID = &t, &c.ID
	}
	var rows []store.ListServersRow
	err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) (err error) {
		rows, err = store.New(tx).ListServers(ctx, store.ListServersParams{
			OrgID: &orgID, ID: id, AfterCreated: after, AfterID: afterID, RowLimit: int32(limit + 1)}) //nolint:gosec // limit ≤ 200
		return err
	})
	if err != nil {
		return nil, "", internal(err)
	}
	next := ""
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[limit-1]
		next = pagination.Encode(pagination.Cursor{Sort: sortCreated, ID: last.ID, Value: last.CreatedAt.UTC().Format(time.RFC3339Nano)})
	}
	out := make([]Server, len(rows))
	for i, r := range rows {
		sv := Server{ID: r.ID, Scope: ScopeOrganization, Slug: r.Slug, Name: r.Name, Description: r.Description, Kind: r.Kind,
			Transport: r.Transport, Endpoint: r.Endpoint, AuthMethod: r.AuthMethod, Status: r.Status,
			HealthStatus: r.HealthStatus, HealthCheckedAt: r.HealthCheckedAt,
			CreatedAt: r.CreatedAt.UTC(), UpdatedAt: r.UpdatedAt.UTC()}
		if r.OrgID == nil {
			sv.Scope = ScopePlatform
		}
		if r.ActiveVersion != nil {
			v := int(*r.ActiveVersion)
			sv.ActiveVersion = &v
		}
		if r.PendingVersion > 0 {
			v := int(r.PendingVersion)
			sv.PendingVersion = &v
		}
		out[i] = sv
	}
	return out, next, nil
}

// ListServers liệt kê server mà tenant thấy được, mới nhất trước.
func (s *Service) ListServers(ctx context.Context, limit int, cursor string) ([]Server, string, error) {
	return s.servers(ctx, nil, limit, cursor)
}

// GetServer trả về một server theo id.
func (s *Service) GetServer(ctx context.Context, id uuid.UUID) (Server, error) {
	out, _, err := s.servers(ctx, &id, 1, "")
	if err != nil {
		return Server{}, err
	}
	if len(out) == 0 {
		return Server{}, errNotFound
	}
	return out[0], nil
}

// ToolFilter lọc danh sách tool của version đang hoạt động.
type ToolFilter struct {
	ServerID  *uuid.UUID
	Enabled   *bool
	RiskLevel string
	ToolID    *uuid.UUID
}

// ActiveTools trả về tool của version đang hoạt động của các server đang hoạt động
// mà tenant thấy được. Tool của version chờ duyệt không bao giờ có mặt ở đây.
func (s *Service) ActiveTools(ctx context.Context, f ToolFilter) ([]Tool, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return nil, internal(tenancy.ErrNoTenant)
	}
	var risk *string
	if f.RiskLevel != "" {
		if !slices.Contains([]string{"read", "write", "destructive", "financial"}, f.RiskLevel) {
			return nil, apperr.New(apperr.KindInvalid, "invalid_parameter", "risk_level không hợp lệ")
		}
		risk = &f.RiskLevel
	}
	var rows []store.ListActiveToolsRow
	err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) (err error) {
		rows, err = store.New(tx).ListActiveTools(ctx, store.ListActiveToolsParams{
			OrgID: &orgID, ServerID: f.ServerID, Enabled: f.Enabled, RiskLevel: risk, ToolID: f.ToolID, RowLimit: maxToolsRead})
		return err
	})
	if err != nil {
		return nil, internal(err)
	}
	out := make([]Tool, len(rows))
	for i, r := range rows {
		t := Tool{ID: r.ID, ServerID: r.ServerID, ServerSlug: r.ServerSlug, ServerKind: r.ServerKind, AuthMethod: r.ServerAuthMethod,
			Name: r.Name, InputSchema: r.InputSchema, DefinitionHash: r.DefinitionHash, Enabled: r.Enabled,
			RiskLevel: r.RiskLevel, SideEffect: r.SideEffect, DataSensitivity: r.DataSensitivity, Egress: r.Egress,
			Cacheable: r.Cacheable, Timeout: time.Duration(r.TimeoutMs) * time.Millisecond, MaxResultBytes: int(r.MaxResultBytes)}
		if r.Description != nil {
			t.Description = *r.Description
		}
		if r.ServerEndpoint != nil {
			t.Endpoint = *r.ServerEndpoint
		}
		t.SecretID = r.ServerSecretID
		out[i] = t
	}
	return out, nil
}

// ValidateTools kiểm tra danh sách tool trong cấu hình của một Agent Version với
// Registry. Hiện thực catalog.ToolValidator của Agent Catalog.
//
// Mỗi mục phải trỏ tới một server đang hoạt động mà tenant thấy; mọi tên trong
// allow / deny phải là tool đang bật của version đang hoạt động ("*" là mọi tool
// đang bật). require_approval trong spec chỉ nhận "always" hoặc "policy", nên Agent
// không có cách nào nới lỏng mức duyệt của tool.
func (s *Service) ValidateTools(ctx context.Context, _ uuid.UUID, cfg api.AgentConfig) error {
	enabled := true
	for i, entry := range cfg.Tools {
		field := "tools[" + itoa(i) + "]"
		id := entry.ServerId
		tools, err := s.ActiveTools(ctx, ToolFilter{ServerID: &id, Enabled: &enabled})
		if err != nil {
			return err
		}
		if len(tools) == 0 {
			// Server không tồn tại, thuộc org khác, chưa hoạt động, hoặc chưa có tool
			// nào được bật: cùng một thông báo, không phân biệt.
			return invalid(field + ".server_id: server không tồn tại hoặc không có tool nào đang bật")
		}
		byName := map[string]Tool{}
		for _, t := range tools {
			byName[t.Name] = t
		}
		if len(entry.Allow) == 0 {
			return invalid(field + ".allow không được rỗng")
		}
		names := slices.Clone(entry.Allow)
		if entry.Deny != nil {
			names = append(names, *entry.Deny...)
		}
		for _, n := range names {
			if n == "*" {
				continue
			}
			if _, ok := byName[n]; !ok {
				return invalid(field + ": tool " + quote(n) + " không tồn tại hoặc chưa được bật trên server này")
			}
		}
	}
	return nil
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// ErrNoSuchTool báo tool không có trong tập mà principal được phép: Gateway trả
// cùng một lỗi cho "không tồn tại" và "không được phép".
var ErrNoSuchTool = errors.New("registry: no such tool")
