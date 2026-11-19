// Package apikeys quản lý API key của platform: xác thực cho máy và CI thay cho đăng
// nhập OIDC (security §4.1). Khoá chỉ hiển thị một lần lúc tạo; database chỉ giữ
// SHA-256 của nó. Quyền của khoá là một role dựng sẵn, gắn ở phạm vi org hoặc một
// project, và không bao giờ vượt quyền của người tạo.
package apikeys

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	pgxv5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hqgroups/mcp/internal/audit"
	"github.com/hqgroups/mcp/internal/platform/apikeys/store"
	"github.com/hqgroups/mcp/internal/platform/identity"
	"github.com/hqgroups/mcp/internal/platform/tenancy"
	apperr "github.com/hqgroups/mcp/internal/shared/errors"
	"github.com/hqgroups/mcp/internal/shared/ids"
	"github.com/hqgroups/mcp/internal/shared/pagination"
	sharedpgx "github.com/hqgroups/mcp/internal/shared/pgx"
)

const (
	// Prefix là dấu nhận biết khoá của platform trong header Authorization.
	Prefix          = identity.APIKeyPrefix
	lookupLen       = 12
	secretBytes     = 32
	defaultDays     = 90
	maxDays         = 365
	maxNameRunes    = 100
	sortCreated     = "-created_at"
	permKeyRead     = "api_key:read"
	permKeyCreate   = "api_key:create"
	permKeyRevoke   = "api_key:revoke"
	permCredential  = "model_provider:set_credential"
	permKeyWildcard = "api_key:"
)

// Key là metadata của một API key. Giá trị của khoá không nằm ở đây.
type Key struct {
	ID         uuid.UUID
	ProjectID  *uuid.UUID
	Name       string
	Prefix     string
	Role       string
	CreatedBy  uuid.UUID
	ExpiresAt  time.Time
	RevokedAt  *time.Time
	LastUsedAt *time.Time
	CreatedAt  time.Time
}

// CreateInput là yêu cầu tạo khoá.
type CreateInput struct {
	Name          string
	ProjectID     *uuid.UUID
	Role          string
	ExpiresInDays *int
}

// Permissions là thứ package này cần từ Authorization & Policy.
type Permissions interface {
	// Missing trả về những permission mà principal của request KHÔNG có ở phạm vi đã cho.
	Missing(ctx context.Context, permissions []string, projectID *uuid.UUID) ([]string, error)
}

// Projects là thứ package này cần từ Identity & Tenancy.
type Projects interface {
	GetProject(ctx context.Context, id uuid.UUID) (tenancy.Project, error)
}

var (
	errNotFound = apperr.New(apperr.KindNotFound, "not_found", "tài nguyên không tồn tại")
	errUnauth   = apperr.New(apperr.KindUnauthenticated, "unauthenticated", "thiếu thông tin xác thực hoặc thông tin không hợp lệ")
)

func invalid(msg string) error { return apperr.New(apperr.KindUnprocessable, "validation_failed", msg) }
func internal(err error) error { return apperr.Wrap(err, apperr.KindInternal, "internal_error", "") }

// Service tạo, liệt kê, thu hồi và xác thực API key.
type Service struct {
	db       *pgxpool.Pool
	perms    Permissions
	projects Projects
	creators *identity.ActiveChecker
}

// NewService tạo Service. perms và projects chỉ cần cho việc tạo khoá (process api);
// process chỉ xác thực (gateway) truyền nil.
func NewService(db *pgxpool.Pool, perms Permissions, projects Projects) *Service {
	return &Service{db: db, perms: perms, projects: projects, creators: identity.NewActiveChecker(db)}
}

var lookupEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// generate tạo một khoá mới: "hqk_<12 ký tự tra cứu>_<43 ký tự bí mật>".
func generate() (token, lookup string, hash []byte, err error) {
	raw := make([]byte, 8+secretBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", "", nil, err
	}
	lookup = lookupEncoding.EncodeToString(raw[:8])[:lookupLen]
	token = Prefix + lookup + "_" + base64.RawURLEncoding.EncodeToString(raw[8:])
	sum := sha256.Sum256([]byte(token))
	return token, lookup, sum[:], nil
}

func keyFromRow(r store.InsertKeyRow) Key {
	return Key{ID: r.ID, ProjectID: r.ProjectID, Name: r.Name, Prefix: r.KeyPrefix, Role: r.RoleName, CreatedBy: r.CreatedBy,
		ExpiresAt: r.ExpiresAt.UTC(), RevokedAt: r.RevokedAt, LastUsedAt: r.LastUsedAt, CreatedAt: r.CreatedAt.UTC()}
}

// Create tạo một khoá và trả về giá trị của nó ĐÚNG MỘT LẦN. Chỉ người dùng tạo
// được khoá (khoá không tạo ra khoá), và role của khoá không được chứa permission
// nào mà người tạo không có ở cùng phạm vi.
func (s *Service) Create(ctx context.Context, in CreateInput) (Key, string, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	p, pok := identity.PrincipalFrom(ctx)
	if !ok || !pok {
		return Key{}, "", internal(tenancy.ErrNoTenant)
	}
	if p.Type != identity.PrincipalUser {
		return Key{}, "", apperr.New(apperr.KindPermissionDenied, "permission_denied", "chỉ người dùng tạo được API key")
	}
	name := strings.TrimSpace(in.Name)
	if name == "" || utf8.RuneCountInString(name) > maxNameRunes || strings.ContainsFunc(name, func(r rune) bool { return r < ' ' || r == 0x7f }) {
		return Key{}, "", invalid("name phải có từ 1 đến 100 ký tự, không chứa ký tự điều khiển")
	}
	days := defaultDays
	if in.ExpiresInDays != nil {
		days = *in.ExpiresInDays
	}
	if days < 1 || days > maxDays {
		return Key{}, "", invalid("expires_in_days phải từ 1 đến 365")
	}
	if in.ProjectID != nil {
		project, err := s.projects.GetProject(ctx, *in.ProjectID)
		if err != nil {
			if apperr.KindOf(err) == apperr.KindNotFound {
				return Key{}, "", invalid("project_id không tồn tại")
			}
			return Key{}, "", err
		}
		if project.ArchivedAt != nil {
			return Key{}, "", invalid("project đã được lưu trữ")
		}
	}

	// Role phải là role dựng sẵn; quyền của nó được so với quyền của người tạo.
	var permissions []string
	err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) (err error) {
		permissions, err = store.New(tx).BuiltinRolePermissions(ctx, in.Role)
		return err
	})
	if errors.Is(err, pgxv5.ErrNoRows) {
		return Key{}, "", invalid("role không tồn tại")
	}
	if err != nil {
		return Key{}, "", internal(err)
	}
	for _, perm := range permissions {
		// Khoá máy không quản lý khoá khác và không đặt credential của provider.
		if strings.HasPrefix(perm, permKeyWildcard) || perm == permCredential {
			return Key{}, "", invalid("role " + in.Role + " không gán được cho API key: nó chứa quyền quản lý khoá hoặc credential")
		}
	}
	missing, err := s.perms.Missing(ctx, permissions, in.ProjectID)
	if err != nil {
		return Key{}, "", internal(err)
	}
	if len(missing) > 0 {
		return Key{}, "", apperr.New(apperr.KindPermissionDenied, "permission_denied",
			"không tạo được khoá có quyền vượt quyền của chính bạn ở phạm vi này")
	}

	var (
		row   store.InsertKeyRow
		token string
	)
	// Prefix tra cứu là ngẫu nhiên và duy nhất toàn cục: trùng thì sinh khoá khác.
	for attempt := 0; attempt < 3; attempt++ {
		var lookup string
		var hash []byte
		if token, lookup, hash, err = generate(); err != nil {
			return Key{}, "", internal(err)
		}
		row, err = s.insert(ctx, orgID, p.ID, in, name, lookup, hash, days)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
			break
		}
	}
	if err != nil {
		return Key{}, "", internal(err)
	}
	return keyFromRow(row), token, nil
}

func (s *Service) insert(ctx context.Context, orgID, userID uuid.UUID, in CreateInput, name, lookup string, hash []byte, days int) (row store.InsertKeyRow, err error) {
	p := identity.Principal{ID: userID}
	err = tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) error {
		q := store.New(tx)
		var err error
		row, err = q.InsertKey(ctx, store.InsertKeyParams{ID: ids.New(), OrgID: orgID, ProjectID: in.ProjectID, Name: name,
			KeyPrefix: lookup, KeyHash: hash, RoleName: in.Role, CreatedBy: p.ID, ExpiresAt: time.Now().AddDate(0, 0, days)})
		if err != nil {
			return err
		}
		if err := q.BindRole(ctx, store.BindRoleParams{KeyID: row.ID, CreatedBy: p.ID}); err != nil {
			return err
		}
		details := map[string]string{"name": name, "role": in.Role, "prefix": lookup, "expires_at": row.ExpiresAt.UTC().Format(time.RFC3339)}
		return audit.Record(ctx, tx, audit.Event{OrgID: orgID, ProjectID: in.ProjectID, Action: "api_key.create",
			ResourceType: "api_key", ResourceID: &row.ID, Outcome: audit.OutcomeSuccess, Details: details})
	})
	return row, err
}

// Get trả về metadata của một khoá trong tenant.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (Key, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return Key{}, internal(tenancy.ErrNoTenant)
	}
	var row store.GetKeyRow
	err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) (err error) {
		row, err = store.New(tx).GetKey(ctx, store.GetKeyParams{OrgID: orgID, ID: id})
		return err
	})
	if errors.Is(err, pgxv5.ErrNoRows) {
		return Key{}, errNotFound
	}
	if err != nil {
		return Key{}, internal(err)
	}
	return keyFromRow(store.InsertKeyRow(row)), nil
}

// List liệt kê khoá của org, mới nhất trước.
func (s *Service) List(ctx context.Context, limit int, cursor string) (out []Key, next string, err error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return nil, "", internal(tenancy.ErrNoTenant)
	}
	c, err := pagination.Decode(cursor, sortCreated)
	if err != nil {
		return nil, "", err
	}
	params := store.ListKeysParams{OrgID: orgID, RowLimit: int32(limit + 1)} //nolint:gosec // limit ≤ 200
	if c != nil {
		at, err := time.Parse(time.RFC3339Nano, c.Value)
		if err != nil {
			return nil, "", pagination.ErrInvalidCursor
		}
		params.AfterCreatedAt, params.AfterID = &at, &c.ID
	}
	var rows []store.ListKeysRow
	if err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) (err error) {
		rows, err = store.New(tx).ListKeys(ctx, params)
		return err
	}); err != nil {
		return nil, "", internal(err)
	}
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[limit-1]
		next = pagination.Encode(pagination.Cursor{Sort: sortCreated, Value: last.CreatedAt.UTC().Format(time.RFC3339Nano), ID: last.ID})
	}
	out = make([]Key, len(rows))
	for i, r := range rows {
		out[i] = keyFromRow(store.InsertKeyRow(r))
	}
	return out, next, nil
}

// Revoke thu hồi khoá. Thu hồi lại khoá đã thu hồi là no-op.
func (s *Service) Revoke(ctx context.Context, id uuid.UUID) error {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return internal(tenancy.ErrNoTenant)
	}
	err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) error {
		q := store.New(tx)
		row, err := q.GetKey(ctx, store.GetKeyParams{OrgID: orgID, ID: id})
		if err != nil {
			return err
		}
		n, err := q.RevokeKey(ctx, store.RevokeKeyParams{OrgID: orgID, ID: id})
		if err != nil || n == 0 {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{OrgID: orgID, ProjectID: row.ProjectID, Action: "api_key.revoke",
			ResourceType: "api_key", ResourceID: &id, Outcome: audit.OutcomeSuccess, Details: map[string]string{"prefix": row.KeyPrefix}})
	})
	if errors.Is(err, pgxv5.ErrNoRows) {
		return errNotFound
	}
	if err != nil {
		return internal(err)
	}
	return nil
}

// IsKey cho biết một bearer token có dạng API key của platform hay không.
func IsKey(token string) bool { return strings.HasPrefix(token, Prefix) }

// Authenticate kiểm một API key và trả về principal của nó. Mọi lý do thất bại
// (sai dạng, không tồn tại, sai bí mật, đã thu hồi, hết hạn, org bị đình chỉ) cho
// cùng một lỗi 401.
func (s *Service) Authenticate(ctx context.Context, token string) (identity.Principal, error) {
	rest, ok := strings.CutPrefix(token, Prefix)
	lookup, _, ok2 := strings.Cut(rest, "_")
	if !ok || !ok2 || len(lookup) != lookupLen || len(token) > 128 {
		return identity.Principal{}, errUnauth
	}
	// Transaction này chỉ đặt prefix đang tra: policy key_lookup cho đọc đúng một dòng.
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return identity.Principal{}, internal(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.api_key_prefix', $1, true)", lookup); err != nil {
		return identity.Principal{}, internal(err)
	}
	row, err := store.New(tx).LookupByPrefix(ctx, lookup)
	sum := sha256.Sum256([]byte(token))
	switch {
	case errors.Is(err, pgxv5.ErrNoRows):
		// So sánh với hash giả để thời gian không phân biệt prefix có tồn tại hay không.
		subtle.ConstantTimeCompare(sum[:], make([]byte, sha256.Size))
		return identity.Principal{}, errUnauth
	case err != nil:
		return identity.Principal{}, internal(err)
	}
	_ = tx.Rollback(ctx)
	if subtle.ConstantTimeCompare(sum[:], row.KeyHash) != 1 || row.RevokedAt != nil || !row.ExpiresAt.After(time.Now()) {
		return identity.Principal{}, errUnauth
	}
	// Khoá không sống lâu hơn người tạo ra nó: người tạo bị vô hiệu hoá hoặc rời org
	// thì khoá ngừng hoạt động, như token của chính họ. Phép kiểm này gồm cả việc
	// organization còn hoạt động.
	if err := s.creators.RequireActive(ctx, row.CreatedBy, row.OrgID); err != nil {
		if apperr.KindOf(err) == apperr.KindInternal {
			return identity.Principal{}, err
		}
		return identity.Principal{}, errUnauth
	}
	if err := sharedpgx.WithTenantTx(ctx, s.db, row.OrgID, func(tx pgxv5.Tx) error {
		return store.New(tx).TouchKey(ctx, store.TouchKeyParams{OrgID: row.OrgID, ID: row.ID})
	}); err != nil {
		return identity.Principal{}, internal(err)
	}
	return identity.Principal{Type: identity.PrincipalAPIKey, ID: row.ID, OrgID: row.OrgID}, nil
}

// Usable cho biết khoá (trong tenant của ctx) còn dùng được: chưa thu hồi, chưa hết
// hạn, và người tạo ra nó còn hoạt động trong org.
// Owner trả về user đã tạo một API key của tenant. Hành động do key thực hiện được quy
// về người này ở những chỗ cần biết "con người nào đứng sau" (ví dụ: người yêu cầu một
// approval không được tự duyệt nó).
func (s *Service) Owner(ctx context.Context, id uuid.UUID) (uuid.UUID, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return uuid.Nil, tenancy.ErrNoTenant
	}
	var row store.KeyUsableRow
	err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) (err error) {
		row, err = store.New(tx).KeyUsable(ctx, store.KeyUsableParams{OrgID: orgID, ID: id})
		return err
	})
	return row.CreatedBy, err
}

func (s *Service) Usable(ctx context.Context, id uuid.UUID) (bool, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return false, tenancy.ErrNoTenant
	}
	var row store.KeyUsableRow
	err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) (err error) {
		row, err = store.New(tx).KeyUsable(ctx, store.KeyUsableParams{OrgID: orgID, ID: id})
		return err
	})
	if errors.Is(err, pgxv5.ErrNoRows) || (err == nil && !row.Usable) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := s.creators.RequireActive(ctx, row.CreatedBy, orgID); err != nil {
		if apperr.KindOf(err) == apperr.KindInternal {
			return false, err
		}
		return false, nil
	}
	return true, nil
}
