// Package modelcatalog quản lý provider LLM và model (bounded context AI Provider).
//
// Platform và Agent tham chiếu model qua catalog này, không hardcode tên model
// (ADR-0010). Bảng giá thuộc context Cost (internal/cost/pricing).
//
// Phạm vi: provider và model có thể thuộc platform (mọi tenant đọc được, không sửa
// được qua API tenant) hoặc thuộc một org. Model luôn cùng phạm vi với provider của nó.
package modelcatalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	pgxv5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hqgroups/mcp/internal/ai/modelcatalog/store"
	"github.com/hqgroups/mcp/internal/audit"
	"github.com/hqgroups/mcp/internal/platform/identity"
	"github.com/hqgroups/mcp/internal/platform/tenancy"
	apperr "github.com/hqgroups/mcp/internal/shared/errors"
	"github.com/hqgroups/mcp/internal/shared/httpx"
	"github.com/hqgroups/mcp/internal/shared/ids"
	"github.com/hqgroups/mcp/internal/shared/pagination"
)

// Phạm vi của một provider hoặc model.
const (
	ScopePlatform     = "platform"
	ScopeOrganization = "organization"
)

// Provider là một nhà cung cấp LLM đã đăng ký.
type Provider struct {
	ID            uuid.UUID
	Scope         string
	Slug          string
	Adapter       string
	BaseURL       *string
	Enabled       bool
	HasCredential bool
	Settings      json.RawMessage
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Model là một model của một provider.
type Model struct {
	ID            uuid.UUID
	ProviderID    uuid.UUID
	Scope         string
	ProviderModel string
	DisplayName   string
	Tier          *string
	Capabilities  json.RawMessage
	Enabled       bool
	DeprecatedAt  *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// ProviderCreate là đầu vào tạo provider.
type ProviderCreate struct {
	Slug     string
	Adapter  string
	BaseURL  *string
	APIKey   *string
	Settings json.RawMessage
}

// ProviderUpdate sửa provider của org.
type ProviderUpdate struct {
	Enabled     *bool
	Settings    json.RawMessage
	BaseURL     *string
	APIKey      *string
	IfUpdatedAt *time.Time
}

// ModelCreate là đầu vào tạo model.
type ModelCreate struct {
	ProviderID    uuid.UUID
	ProviderModel string
	DisplayName   string
	Tier          *string
	Capabilities  json.RawMessage
}

// ModelUpdate sửa model của org.
type ModelUpdate struct {
	DisplayName  *string
	Tier         *string
	Capabilities json.RawMessage
	Enabled      *bool
	DeprecatedAt *time.Time
}

// ModelFilter lọc danh sách model.
type ModelFilter struct {
	ProviderID *uuid.UUID
	Tier       string
	Enabled    *bool
	Limit      int
	Cursor     string
}

var (
	slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)
	// Định danh model phía provider: chữ, số và vài dấu thường gặp; không khoảng trắng.
	providerModelRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@-]{0,199}$`)

	errNotFound        = apperr.New(apperr.KindNotFound, "not_found", "tài nguyên không tồn tại")
	errSlugTaken       = apperr.New(apperr.KindConflict, "slug_taken", "slug đã được dùng trong organization này")
	errModelExists     = apperr.New(apperr.KindConflict, "model_exists", "provider này đã có model với provider_model đó")
	errProviderInUse   = apperr.New(apperr.KindConflict, "provider_in_use", "provider còn model; tắt provider (enabled=false) thay vì xoá")
	errPlatformManaged = apperr.New(apperr.KindPermissionDenied, "platform_managed", "tài nguyên do platform quản lý, không sửa được từ organization")
)

const (
	maxNameRunes = 200
	maxJSONBytes = 64 << 10
	sortCreated  = "-created_at"
)

func invalid(msg string) error { return apperr.New(apperr.KindUnprocessable, "validation_failed", msg) }
func internal(err error) error { return apperr.Wrap(err, apperr.KindInternal, "internal_error", "") }

func isPg(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}

// Service chứa nghiệp vụ Model Catalog. Tenant luôn lấy từ context.
type Service struct {
	db          *pgxpool.Pool
	adapters    []string
	credentials Credentials
	secrets     SecretWriter
}

// NewService tạo Service. adapters là danh sách adapter mà process này thực sự có;
// provider chỉ được tạo với adapter trong danh sách (không nhận tên adapter tuỳ ý).
func NewService(db *pgxpool.Pool, adapters []string) *Service {
	return &Service{db: db, adapters: adapters}
}

// ErrCredentialInvalid là lỗi mà Credentials trả về khi provider từ chối khoá.
var ErrCredentialInvalid = errors.New("modelcatalog: provider từ chối khoá")

// Credentials kiểm khoá của provider với chính provider đó trước khi lưu.
type Credentials interface {
	// Supports cho biết adapter có dùng khoá do org cung cấp hay không.
	Supports(adapter string) bool
	// Verify trả ErrCredentialInvalid nếu provider từ chối khoá; lỗi khác nghĩa là
	// không kiểm tra được.
	Verify(ctx context.Context, adapter, apiKey string) error
}

// SecretWriter là thứ Model Catalog cần từ Secrets: ghi (không đọc) một secret.
type SecretWriter interface {
	Put(ctx context.Context, tx pgxv5.Tx, orgID uuid.UUID, name, kind string, plaintext []byte, createdBy *uuid.UUID) (uuid.UUID, error)
}

// WithCredentials bật việc nhận api_key cho provider: khoá được kiểm với provider,
// rồi mã hoá và lưu qua Secrets. Thiếu một trong hai thì api_key bị từ chối.
func (s *Service) WithCredentials(c Credentials, w SecretWriter) *Service {
	s.credentials, s.secrets = c, w
	return s
}

// checkCredential kiểm api_key trước khi mở transaction (lời gọi mạng không nằm
// trong transaction). Khoá sai thì không có gì được lưu.
func (s *Service) checkCredential(ctx context.Context, adapter string, apiKey *string) error {
	if apiKey == nil {
		return nil
	}
	if s.credentials == nil || s.secrets == nil {
		return invalid("api_key: process này chưa được cấu hình để lưu secret (HQ_SECRETS_PUBLIC_KEY)")
	}
	if !s.credentials.Supports(adapter) {
		return invalid("api_key: adapter " + adapter + " không dùng khoá do organization cung cấp")
	}
	err := s.credentials.Verify(ctx, adapter, *apiKey)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrCredentialInvalid):
		return invalid("api_key: provider từ chối khoá này; kiểm tra lại khoá và quyền của nó")
	default:
		// Không kết luận được khoá đúng hay sai: không lưu, và để người gọi thử lại.
		return apperr.Wrap(err, apperr.KindUnavailable, "provider_unavailable", "không kiểm tra được api_key với provider; thử lại sau").AsRetryable()
	}
}

// storeCredential mã hoá và lưu khoá trong tx, rồi gắn nó vào provider.
func (s *Service) storeCredential(ctx context.Context, tx pgxv5.Tx, orgID, providerID uuid.UUID, apiKey string) (store.InsertProviderRow, error) {
	var by *uuid.UUID
	if p, ok := identity.PrincipalFrom(ctx); ok && p.Type == "user" {
		by = &p.ID
	}
	secretID, err := s.secrets.Put(ctx, tx, orgID, "model-provider/"+providerID.String(), "api_key", []byte(strings.TrimSpace(apiKey)), by)
	if err != nil {
		return store.InsertProviderRow{}, err
	}
	row, err := store.New(tx).SetProviderSecret(ctx, store.SetProviderSecretParams{OrgID: &orgID, ID: providerID, SecretID: &secretID})
	return store.InsertProviderRow(row), err
}

func validateObject(field string, raw json.RawMessage) error {
	if raw == nil {
		return nil
	}
	if len(raw) > maxJSONBytes {
		return invalid(field + " vượt giới hạn kích thước")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return invalid(field + " phải là một JSON object")
	}
	if strings.Contains(string(raw), `\u0000`) {
		return invalid(field + " không được chứa ký tự U+0000")
	}
	return nil
}

var secretKeyRe = regexp.MustCompile(`(?i)(key|token|secret|password|passwd|credential)`)

// rejectSecretKeys từ chối settings có khoá trông như chứa bí mật. settings được
// lưu plaintext và trả lại cho mọi người đọc được provider; khoá API phải đi qua
// Secrets (chưa có), không qua đây.
func rejectSecretKeys(raw json.RawMessage) error {
	if raw == nil {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	var stack []bool // true = đang ở trong object
	expectKey := false
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil
		}
		switch v := tok.(type) {
		case json.Delim:
			switch v {
			case '{':
				stack = append(stack, true)
				expectKey = true
			case '[':
				stack = append(stack, false)
				expectKey = false
			default:
				stack = stack[:len(stack)-1]
				expectKey = len(stack) > 0 && stack[len(stack)-1]
			}
			continue
		case string:
			if expectKey {
				if secretKeyRe.MatchString(v) {
					return invalid("settings không được chứa khoá, token hay mật khẩu (trường " + strconv.Quote(v) + "); bí mật phải đi qua Secrets")
				}
				expectKey = false
				continue
			}
		}
		expectKey = len(stack) > 0 && stack[len(stack)-1]
	}
}

func validLabel(s string) bool {
	return strings.TrimSpace(s) != "" && utf8.RuneCountInString(s) <= maxNameRunes &&
		!strings.ContainsFunc(s, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) })
}

func validateTier(t *string) error {
	if t == nil {
		return nil
	}
	switch *t {
	case "economy", "standard", "premium":
		return nil
	}
	return invalid("tier phải là economy, standard hoặc premium")
}

// Các tính năng chưa có hạ tầng để làm an toàn bị từ chối thay vì nhận rồi bỏ qua.
func rejectUnsupported(baseURL *string) error {
	if baseURL != nil {
		return invalid("base_url chưa được hỗ trợ: cần kiểm tra chống SSRF trước khi platform gọi tới URL do người dùng cung cấp")
	}
	return nil
}

func orDefault(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return json.RawMessage(`{}`)
	}
	return raw
}

// CreateProvider thêm provider cho organization của request.
func (s *Service) CreateProvider(ctx context.Context, in ProviderCreate) (Provider, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return Provider{}, internal(tenancy.ErrNoTenant)
	}
	if !slugRe.MatchString(in.Slug) {
		return Provider{}, invalid("slug phải khớp ^[a-z0-9][a-z0-9-]{1,62}$")
	}
	if !slices.Contains(s.adapters, in.Adapter) {
		return Provider{}, invalid("adapter không được hỗ trợ ở môi trường này; hỗ trợ: " + strings.Join(s.adapters, ", "))
	}
	if err := rejectUnsupported(in.BaseURL); err != nil {
		return Provider{}, err
	}
	if err := validateObject("settings", in.Settings); err != nil {
		return Provider{}, err
	}
	if err := rejectSecretKeys(in.Settings); err != nil {
		return Provider{}, err
	}
	if err := s.checkCredential(ctx, in.Adapter, in.APIKey); err != nil {
		return Provider{}, err
	}
	var row store.InsertProviderRow
	err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) (err error) {
		row, err = store.New(tx).InsertProvider(ctx, store.InsertProviderParams{
			ID: ids.New(), OrgID: &orgID, Slug: in.Slug, Adapter: in.Adapter, Settings: orDefault(in.Settings)})
		if err != nil {
			return err
		}
		if in.APIKey != nil {
			if row, err = s.storeCredential(ctx, tx, orgID, row.ID, *in.APIKey); err != nil {
				return err
			}
		}
		return audit.Record(ctx, tx, audit.Event{
			OrgID: orgID, Action: "model_provider.create", ResourceType: "model_provider", ResourceID: &row.ID,
			Outcome: audit.OutcomeSuccess, Details: map[string]string{"slug": row.Slug, "adapter": row.Adapter,
				"credential": strconv.FormatBool(in.APIKey != nil)}})
	})
	if isPg(err, "23505") {
		return Provider{}, errSlugTaken
	}
	if err != nil {
		return Provider{}, internal(err)
	}
	return providerFromRow(row), nil
}

// GetProvider trả về provider của org hoặc của platform.
func (s *Service) GetProvider(ctx context.Context, id uuid.UUID) (Provider, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return Provider{}, internal(tenancy.ErrNoTenant)
	}
	var row store.GetProviderRow
	err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) (err error) {
		row, err = store.New(tx).GetProvider(ctx, store.GetProviderParams{ID: id, OrgID: &orgID})
		return err
	})
	if errors.Is(err, pgxv5.ErrNoRows) {
		return Provider{}, errNotFound
	}
	if err != nil {
		return Provider{}, internal(err)
	}
	return providerFromRow(store.InsertProviderRow(row)), nil
}

func decodeCreatedCursor(cursor string) (*time.Time, *uuid.UUID, error) {
	c, err := pagination.Decode(cursor, sortCreated)
	if err != nil || c == nil {
		return nil, nil, err
	}
	t, perr := time.Parse(time.RFC3339Nano, c.Value)
	if perr != nil {
		return nil, nil, pagination.ErrInvalidCursor
	}
	return &t, &c.ID, nil
}

func createdCursor(id uuid.UUID, at time.Time) string {
	return pagination.Encode(pagination.Cursor{Sort: sortCreated, ID: id, Value: at.UTC().Format(time.RFC3339Nano)})
}

// ListProviders liệt kê provider của org và của platform, mới nhất trước.
func (s *Service) ListProviders(ctx context.Context, limit int, cursor string) (out []Provider, next string, err error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return nil, "", internal(tenancy.ErrNoTenant)
	}
	after, afterID, err := decodeCreatedCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	var rows []store.ListProvidersRow
	err = tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) (err error) {
		rows, err = store.New(tx).ListProviders(ctx, store.ListProvidersParams{
			OrgID: &orgID, AfterCreated: after, AfterID: afterID, RowLimit: int32(limit + 1)}) //nolint:gosec // limit ≤ 200
		return err
	})
	if err != nil {
		return nil, "", internal(err)
	}
	if len(rows) > limit {
		rows = rows[:limit]
		next = createdCursor(rows[limit-1].ID, rows[limit-1].CreatedAt)
	}
	for _, r := range rows {
		out = append(out, providerFromRow(store.InsertProviderRow(r)))
	}
	return out, next, nil
}

// UpdateProvider sửa provider của org. Provider của platform chỉ đọc.
func (s *Service) UpdateProvider(ctx context.Context, id uuid.UUID, u ProviderUpdate) (Provider, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return Provider{}, internal(tenancy.ErrNoTenant)
	}
	if err := rejectUnsupported(u.BaseURL); err != nil {
		return Provider{}, err
	}
	if err := validateObject("settings", u.Settings); err != nil {
		return Provider{}, err
	}
	if err := rejectSecretKeys(u.Settings); err != nil {
		return Provider{}, err
	}
	current, err := s.GetProvider(ctx, id)
	if err != nil {
		return Provider{}, err
	}
	if current.Scope == ScopePlatform {
		return Provider{}, errPlatformManaged
	}
	if err := s.checkCredential(ctx, current.Adapter, u.APIKey); err != nil {
		return Provider{}, err
	}
	var row store.UpdateProviderRow
	err = tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) (err error) {
		row, err = store.New(tx).UpdateProvider(ctx, store.UpdateProviderParams{
			OrgID: &orgID, ID: id, Enabled: u.Enabled, Settings: u.Settings, IfUpdatedAt: u.IfUpdatedAt})
		if err != nil {
			return err
		}
		var fields []string
		if u.Enabled != nil {
			fields = append(fields, "enabled")
		}
		if u.Settings != nil {
			fields = append(fields, "settings")
		}
		if u.APIKey != nil {
			// Xoay khoá: một version mới của secret; version cũ không sửa được.
			updated, err := s.storeCredential(ctx, tx, orgID, id, *u.APIKey)
			if err != nil {
				return err
			}
			row = store.UpdateProviderRow(updated)
			fields = append(fields, "api_key")
		}
		return audit.Record(ctx, tx, audit.Event{
			OrgID: orgID, Action: "model_provider.update", ResourceType: "model_provider", ResourceID: &id,
			Outcome: audit.OutcomeSuccess, Details: map[string]string{"fields": strings.Join(fields, ",")}})
	})
	if errors.Is(err, pgxv5.ErrNoRows) {
		if u.IfUpdatedAt != nil {
			return Provider{}, httpx.ErrPreconditionFailed
		}
		return Provider{}, errNotFound
	}
	if err != nil {
		return Provider{}, internal(err)
	}
	return providerFromRow(store.InsertProviderRow(row)), nil
}

// DeleteProvider lưu trữ (xoá mềm) provider của org khi nó không còn model nào.
// Provider có model chỉ tắt được: model có thể đang được Agent Version hay bản ghi
// usage tham chiếu. Dòng được giữ lại để audit vẫn tra được.
func (s *Service) DeleteProvider(ctx context.Context, id uuid.UUID) error {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return internal(tenancy.ErrNoTenant)
	}
	current, err := s.GetProvider(ctx, id)
	if err != nil {
		return err
	}
	if current.Scope == ScopePlatform {
		return errPlatformManaged
	}
	err = tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) error {
		q := store.New(tx)
		n, err := q.CountModelsOfProvider(ctx, store.CountModelsOfProviderParams{ProviderID: id, OrgID: &orgID})
		if err != nil {
			return err
		}
		if n > 0 {
			return errProviderInUse
		}
		deleted, err := q.ArchiveProvider(ctx, store.ArchiveProviderParams{OrgID: &orgID, ID: id})
		if err != nil {
			return err
		}
		if deleted == 0 {
			return errNotFound
		}
		return audit.Record(ctx, tx, audit.Event{
			OrgID: orgID, Action: "model_provider.delete", ResourceType: "model_provider", ResourceID: &id,
			Outcome: audit.OutcomeSuccess, Details: map[string]string{"slug": current.Slug}})
	})
	switch {
	case isPg(err, "23503"): // một model vừa được thêm đồng thời
		return errProviderInUse
	case err != nil:
		if _, ok := apperr.From(err); ok {
			return err
		}
		return internal(err)
	}
	return nil
}

// CreateModel thêm model dưới một provider của org.
func (s *Service) CreateModel(ctx context.Context, in ModelCreate) (Model, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return Model{}, internal(tenancy.ErrNoTenant)
	}
	if !providerModelRe.MatchString(in.ProviderModel) {
		return Model{}, invalid("provider_model chỉ gồm chữ, số và các ký tự . _ : / @ -, tối đa 200 ký tự")
	}
	if !validLabel(in.DisplayName) {
		return Model{}, invalid("display_name phải có từ 1 đến 200 ký tự, không chứa ký tự điều khiển")
	}
	if err := validateTier(in.Tier); err != nil {
		return Model{}, err
	}
	if err := validateObject("capabilities", in.Capabilities); err != nil {
		return Model{}, err
	}
	p, err := s.GetProvider(ctx, in.ProviderID)
	if err != nil {
		if apperr.KindOf(err) == apperr.KindNotFound {
			return Model{}, invalid("provider_id không tồn tại")
		}
		return Model{}, err
	}
	if p.Scope == ScopePlatform {
		return Model{}, errPlatformManaged
	}
	var row store.InsertModelRow
	err = tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) (err error) {
		row, err = store.New(tx).InsertModel(ctx, store.InsertModelParams{
			ID: ids.New(), OrgID: &orgID, ProviderID: in.ProviderID, ProviderModel: in.ProviderModel,
			DisplayName: in.DisplayName, Tier: in.Tier, Capabilities: orDefault(in.Capabilities)})
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{
			OrgID: orgID, Action: "model.create", ResourceType: "model", ResourceID: &row.ID,
			Outcome: audit.OutcomeSuccess, Details: map[string]string{"provider_model": row.ProviderModel, "provider_id": in.ProviderID.String()}})
	})
	switch {
	case isPg(err, "23505"):
		return Model{}, errModelExists
	case isPg(err, "23503"):
		return Model{}, invalid("provider_id không tồn tại")
	case err != nil:
		return Model{}, internal(err)
	}
	return modelFromRow(row), nil
}

// GetModel trả về model của org hoặc của platform.
func (s *Service) GetModel(ctx context.Context, id uuid.UUID) (Model, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return Model{}, internal(tenancy.ErrNoTenant)
	}
	var row store.GetModelRow
	err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) (err error) {
		row, err = store.New(tx).GetModel(ctx, store.GetModelParams{ID: id, OrgID: &orgID})
		return err
	})
	if errors.Is(err, pgxv5.ErrNoRows) {
		return Model{}, errNotFound
	}
	if err != nil {
		return Model{}, internal(err)
	}
	return modelFromRow(store.InsertModelRow(row)), nil
}

// ListModels liệt kê model của org và của platform, mới nhất trước.
func (s *Service) ListModels(ctx context.Context, f ModelFilter) (out []Model, next string, err error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return nil, "", internal(tenancy.ErrNoTenant)
	}
	var tier *string
	if f.Tier != "" {
		if err := validateTier(&f.Tier); err != nil {
			return nil, "", apperr.New(apperr.KindInvalid, "invalid_parameter", "tier không hợp lệ")
		}
		tier = &f.Tier
	}
	after, afterID, err := decodeCreatedCursor(f.Cursor)
	if err != nil {
		return nil, "", err
	}
	var rows []store.ListModelsRow
	err = tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) (err error) {
		rows, err = store.New(tx).ListModels(ctx, store.ListModelsParams{
			OrgID: &orgID, ProviderID: f.ProviderID, Tier: tier, Enabled: f.Enabled,
			AfterCreated: after, AfterID: afterID, RowLimit: int32(f.Limit + 1)}) //nolint:gosec // limit ≤ 200
		return err
	})
	if err != nil {
		return nil, "", internal(err)
	}
	if len(rows) > f.Limit {
		rows = rows[:f.Limit]
		next = createdCursor(rows[f.Limit-1].ID, rows[f.Limit-1].CreatedAt)
	}
	for _, r := range rows {
		out = append(out, modelFromRow(store.InsertModelRow(r)))
	}
	return out, next, nil
}

// UpdateModel sửa model của org. Model của platform chỉ đọc.
func (s *Service) UpdateModel(ctx context.Context, id uuid.UUID, u ModelUpdate) (Model, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return Model{}, internal(tenancy.ErrNoTenant)
	}
	if u.DisplayName != nil && !validLabel(*u.DisplayName) {
		return Model{}, invalid("display_name phải có từ 1 đến 200 ký tự, không chứa ký tự điều khiển")
	}
	if err := validateTier(u.Tier); err != nil {
		return Model{}, err
	}
	if err := validateObject("capabilities", u.Capabilities); err != nil {
		return Model{}, err
	}
	current, err := s.GetModel(ctx, id)
	if err != nil {
		return Model{}, err
	}
	if current.Scope == ScopePlatform {
		return Model{}, errPlatformManaged
	}
	var row store.UpdateModelRow
	err = tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) (err error) {
		row, err = store.New(tx).UpdateModel(ctx, store.UpdateModelParams{
			OrgID: &orgID, ID: id, DisplayName: u.DisplayName, Tier: u.Tier, Capabilities: u.Capabilities,
			Enabled: u.Enabled, DeprecatedAt: u.DeprecatedAt})
		if err != nil {
			return err
		}
		var fields []string
		for name, set := range map[string]bool{"display_name": u.DisplayName != nil, "tier": u.Tier != nil,
			"capabilities": u.Capabilities != nil, "enabled": u.Enabled != nil, "deprecated_at": u.DeprecatedAt != nil} {
			if set {
				fields = append(fields, name)
			}
		}
		slices.Sort(fields)
		return audit.Record(ctx, tx, audit.Event{
			OrgID: orgID, Action: "model.update", ResourceType: "model", ResourceID: &id,
			Outcome: audit.OutcomeSuccess, Details: map[string]string{"fields": strings.Join(fields, ",")}})
	})
	if errors.Is(err, pgxv5.ErrNoRows) {
		return Model{}, errNotFound
	}
	if err != nil {
		return Model{}, internal(err)
	}
	return modelFromRow(store.InsertModelRow(row)), nil
}

// ErrModelNotFound: model không tồn tại trong tầm nhìn của tenant (khác với model tồn
// tại nhưng đang không dùng được).
var ErrModelNotFound = apperr.New(apperr.KindUnprocessable, "validation_failed", "model không tồn tại")

// Usable mô tả một model đã được kiểm là dùng được ngay lúc này.
type Usable struct {
	ID            uuid.UUID
	Adapter       string
	ProviderModel string
	// PlatformProvider: provider thuộc platform (dùng khoá của platform).
	PlatformProvider bool
	// HasCredential: provider của org đã có khoá được lưu.
	HasCredential bool
	// SecretID là secret chứa khoá của provider thuộc org (nil nếu chưa có).
	SecretID *uuid.UUID
	// Khả năng khai báo của model, cho ràng buộc cứng của Model Router.
	ToolCalling   bool
	ContextWindow int
}

// ResolveForUse trả về model nếu nó tồn tại trong tầm nhìn của tenant, đang bật,
// chưa bị ngừng hỗ trợ, provider đang bật và adapter có trong process này.
func (s *Service) ResolveForUse(ctx context.Context, id uuid.UUID) (Usable, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return Usable{}, internal(tenancy.ErrNoTenant)
	}
	var row store.GetModelForUseRow
	err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) (err error) {
		row, err = store.New(tx).GetModelForUse(ctx, store.GetModelForUseParams{ID: id, OrgID: &orgID})
		return err
	})
	switch {
	case errors.Is(err, pgxv5.ErrNoRows):
		return Usable{}, ErrModelNotFound
	case err != nil:
		return Usable{}, internal(err)
	case !row.Enabled:
		return Usable{}, invalid("model đang bị tắt")
	case row.DeprecatedAt != nil && !row.DeprecatedAt.After(time.Now()):
		return Usable{}, invalid("model đã ngừng hỗ trợ")
	case !row.ProviderEnabled:
		return Usable{}, invalid("provider của model đang bị tắt")
	case !slices.Contains(s.adapters, row.Adapter):
		return Usable{}, invalid("adapter của model không có ở môi trường này")
	}
	var caps struct {
		ToolCalling   bool `json:"tool_calling"`
		ContextWindow int  `json:"context_window"`
	}
	_ = json.Unmarshal(row.Capabilities, &caps) // capabilities đã được kiểm khi ghi
	return Usable{ID: row.ID, Adapter: row.Adapter, ProviderModel: row.ProviderModel,
		PlatformProvider: row.ProviderOrgID == nil, HasCredential: row.ProviderSecretID != nil, SecretID: row.ProviderSecretID,
		ToolCalling: caps.ToolCalling, ContextWindow: caps.ContextWindow}, nil
}

// ValidateModel kiểm tra model được ghim trong một Agent Version (policy fixed)
// dùng được. Hiện thực catalog.ModelValidator của Agent Catalog.
func (s *Service) ValidateModel(ctx context.Context, modelID uuid.UUID) error {
	_, err := s.ResolveForUse(ctx, modelID)
	if e, ok := apperr.From(err); ok && e.Kind == apperr.KindUnprocessable {
		return invalid("model.fixed_model_id: " + e.Message)
	}
	return err
}

// ModelScope cho biết model (trong tầm nhìn của tenant) thuộc platform hay thuộc org.
func (s *Service) ModelScope(ctx context.Context, modelID uuid.UUID) (platform bool, err error) {
	m, err := s.GetModel(ctx, modelID)
	if err != nil {
		return false, err
	}
	return m.Scope == ScopePlatform, nil
}

func scopeOf(orgID *uuid.UUID) string {
	if orgID == nil {
		return ScopePlatform
	}
	return ScopeOrganization
}

func providerFromRow(r store.InsertProviderRow) Provider {
	return Provider{ID: r.ID, Scope: scopeOf(r.OrgID), Slug: r.Slug, Adapter: r.Adapter, BaseURL: r.BaseUrl,
		Enabled: r.Enabled, HasCredential: r.SecretID != nil, Settings: r.Settings,
		CreatedAt: r.CreatedAt.UTC(), UpdatedAt: r.UpdatedAt.UTC()}
}

func modelFromRow(r store.InsertModelRow) Model {
	m := Model{ID: r.ID, ProviderID: r.ProviderID, Scope: scopeOf(r.OrgID), ProviderModel: r.ProviderModel,
		DisplayName: r.DisplayName, Tier: r.Tier, Capabilities: r.Capabilities, Enabled: r.Enabled,
		CreatedAt: r.CreatedAt.UTC(), UpdatedAt: r.UpdatedAt.UTC()}
	if r.DeprecatedAt != nil {
		t := r.DeprecatedAt.UTC()
		m.DeprecatedAt = &t
	}
	return m
}
