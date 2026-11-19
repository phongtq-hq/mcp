package tenancy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	pgxv5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hqgroups/mcp/internal/audit"
	"github.com/hqgroups/mcp/internal/platform/tenancy/store"
	apperr "github.com/hqgroups/mcp/internal/shared/errors"
	"github.com/hqgroups/mcp/internal/shared/httpx"
	"github.com/hqgroups/mcp/internal/shared/ids"
)

// Organization là tenant.
type Organization struct {
	ID        uuid.UUID       `json:"id"`
	Slug      string          `json:"slug"`
	Name      string          `json:"name"`
	Status    string          `json:"status"`
	Settings  json.RawMessage `json:"settings"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// Project là đơn vị chứa tài nguyên trong một organization.
type Project struct {
	ID          uuid.UUID       `json:"id"`
	Slug        string          `json:"slug"`
	Name        string          `json:"name"`
	Environment string          `json:"environment"`
	Settings    json.RawMessage `json:"settings"`
	ArchivedAt  *time.Time      `json:"archived_at"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

// Update là thay đổi từng phần cho organization hoặc project.
// IfUpdatedAt khác nil thì chỉ áp dụng khi tài nguyên chưa đổi kể từ thời điểm đó.
type Update struct {
	Name        *string
	Settings    json.RawMessage
	IfUpdatedAt *time.Time
}

// ProjectCreate là đầu vào tạo project.
type ProjectCreate struct {
	Slug        string
	Name        string
	Environment string
	Settings    json.RawMessage
}

// ListParams là tham số liệt kê project.
type ListParams struct {
	Limit       int
	Cursor      string
	Environment string // rỗng = mọi môi trường
	Sort        string // name | -name | created_at | -created_at
	// OnlyIDs khác nil thì chỉ trả về các project này (principal không có quyền
	// đọc toàn org). nil = không giới hạn.
	OnlyIDs []uuid.UUID
}

const (
	DefaultLimit = 50
	MaxLimit     = 200
	maxNameLen   = 200
	maxSettings  = 64 << 10
)

var (
	slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)

	errNotFound       = apperr.New(apperr.KindNotFound, "not_found", "tài nguyên không tồn tại")
	errSlugTaken      = apperr.New(apperr.KindConflict, "slug_taken", "slug đã được dùng trong organization này")
	errArchived       = apperr.New(apperr.KindConflict, "project_archived", "project đã được lưu trữ")
	errInvalidCursor  = apperr.New(apperr.KindInvalid, "invalid_cursor", "cursor không hợp lệ hoặc không khớp với cách sắp xếp")
	errUniqueViolated = "23505"
)

func invalid(msg string) error {
	return apperr.New(apperr.KindUnprocessable, "validation_failed", msg)
}

func internal(err error) error {
	return apperr.Wrap(err, apperr.KindInternal, "internal_error", "")
}

// Service chứa nghiệp vụ Organization và Project. Tenant luôn lấy từ context.
type Service struct {
	db *pgxpool.Pool
	// settingsCheck kiểm phần settings thuộc về context khác (ví dụ model_routing của
	// Model Router) trước khi ghi. nil thì không kiểm gì thêm.
	settingsCheck func(ctx context.Context, settings json.RawMessage) (map[string]string, error)
}

// WithSettingsValidator đặt hàm kiểm settings của organization và project trước khi
// ghi. Tenancy không biết nội dung các khoá của context khác; bên sở hữu khoá kiểm nó.
// Hàm trả về thêm chi tiết cho audit của lần ghi (ví dụ chuỗi model của từng tier).
func (s *Service) WithSettingsValidator(check func(ctx context.Context, settings json.RawMessage) (map[string]string, error)) *Service {
	s.settingsCheck = check
	return s
}

func (s *Service) checkSettings(ctx context.Context, settings json.RawMessage) (map[string]string, error) {
	if s.settingsCheck == nil || settings == nil {
		return nil, nil
	}
	return s.settingsCheck(ctx, settings)
}

// withDetails gộp chi tiết của validator vào chi tiết audit của lần ghi.
func withDetails(base, extra map[string]string) map[string]string {
	for k, v := range extra {
		base[k] = v
	}
	return base
}

// NewService tạo Service.
func NewService(db *pgxpool.Pool) *Service { return &Service{db: db} }

func validateName(name string) error {
	if strings.TrimSpace(name) == "" || utf8.RuneCountInString(name) > maxNameLen {
		return invalid("name phải có từ 1 đến 200 ký tự")
	}
	// Cc: ký tự điều khiển. Cf: ký tự định dạng vô hình (bidi override, zero-width)
	// làm tên hiển thị khác với nội dung thật.
	if strings.ContainsFunc(name, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }) {
		return invalid("name không được chứa ký tự điều khiển hoặc ký tự định dạng vô hình")
	}
	return nil
}

func validateSettings(s json.RawMessage) error {
	if s == nil {
		return nil
	}
	if len(s) > maxSettings {
		return invalid("settings vượt giới hạn kích thước")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(s, &obj); err != nil || obj == nil {
		return invalid("settings phải là một JSON object")
	}
	// PostgreSQL jsonb không lưu được U+0000 trong chuỗi (kể cả tên khoá).
	dec := json.NewDecoder(bytes.NewReader(s))
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		if str, ok := tok.(string); ok && strings.ContainsRune(str, 0) {
			return invalid("settings không được chứa ký tự U+0000")
		}
	}
	return nil
}

// changed liệt kê tên các trường được sửa, cho audit. Không ghi giá trị:
// settings có thể chứa dữ liệu không nên nằm trong audit trail.
func (u Update) changed() map[string]string {
	var fields []string
	if u.Name != nil {
		fields = append(fields, "name")
	}
	if u.Settings != nil {
		fields = append(fields, "settings")
	}
	return map[string]string{"fields": strings.Join(fields, ",")}
}

func (u Update) validate() error {
	if u.Name != nil {
		if err := validateName(*u.Name); err != nil {
			return err
		}
	}
	return validateSettings(u.Settings)
}

// GetOrganization trả về organization của request.
func (s *Service) GetOrganization(ctx context.Context) (Organization, error) {
	orgID, ok := OrgFrom(ctx)
	if !ok {
		return Organization{}, internal(ErrNoTenant)
	}
	var row store.Organization
	err := InTx(ctx, s.db, func(tx pgxv5.Tx) (err error) {
		row, err = store.New(tx).GetOrganization(ctx, orgID)
		return err
	})
	if errors.Is(err, pgxv5.ErrNoRows) {
		return Organization{}, errNotFound
	}
	if err != nil {
		return Organization{}, internal(err)
	}
	return orgFromRow(row), nil
}

// UpdateOrganization sửa organization của request.
func (s *Service) UpdateOrganization(ctx context.Context, u Update) (Organization, error) {
	orgID, ok := OrgFrom(ctx)
	if !ok {
		return Organization{}, internal(ErrNoTenant)
	}
	if err := u.validate(); err != nil {
		return Organization{}, err
	}
	extra, err := s.checkSettings(ctx, u.Settings)
	if err != nil {
		return Organization{}, err
	}
	var row store.Organization
	err = InTx(ctx, s.db, func(tx pgxv5.Tx) (err error) {
		row, err = store.New(tx).UpdateOrganization(ctx, store.UpdateOrganizationParams{
			ID: orgID, Name: u.Name, Settings: u.Settings, IfUpdatedAt: u.IfUpdatedAt,
		})
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{
			OrgID: orgID, Action: "organization.update", ResourceType: "organization", ResourceID: &orgID,
			Outcome: audit.OutcomeSuccess, Details: withDetails(u.changed(), extra),
		})
	})
	if errors.Is(err, pgxv5.ErrNoRows) {
		if u.IfUpdatedAt != nil {
			return Organization{}, httpx.ErrPreconditionFailed
		}
		return Organization{}, errNotFound
	}
	if err != nil {
		return Organization{}, internal(err)
	}
	return orgFromRow(row), nil
}

// CreateProject tạo project trong organization của request.
func (s *Service) CreateProject(ctx context.Context, in ProjectCreate) (Project, error) {
	orgID, ok := OrgFrom(ctx)
	if !ok {
		return Project{}, internal(ErrNoTenant)
	}
	if !slugRe.MatchString(in.Slug) {
		return Project{}, invalid("slug phải khớp ^[a-z0-9][a-z0-9-]{1,62}$")
	}
	if err := validateName(in.Name); err != nil {
		return Project{}, err
	}
	if in.Environment != "prod" && in.Environment != "non_prod" {
		return Project{}, invalid("environment phải là prod hoặc non_prod")
	}
	if err := validateSettings(in.Settings); err != nil {
		return Project{}, err
	}
	extra, err := s.checkSettings(ctx, in.Settings)
	if err != nil {
		return Project{}, err
	}
	if in.Settings == nil {
		in.Settings = json.RawMessage(`{}`)
	}
	var row store.Project
	err = InTx(ctx, s.db, func(tx pgxv5.Tx) (err error) {
		row, err = store.New(tx).CreateProject(ctx, store.CreateProjectParams{
			ID: ids.New(), OrgID: orgID, Slug: in.Slug, Name: in.Name,
			Environment: in.Environment, Settings: in.Settings,
		})
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{
			OrgID: orgID, ProjectID: &row.ID, Action: "project.create", ResourceType: "project", ResourceID: &row.ID,
			Outcome: audit.OutcomeSuccess, Details: withDetails(map[string]string{"slug": row.Slug, "environment": row.Environment}, extra),
		})
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == errUniqueViolated {
		return Project{}, errSlugTaken
	}
	if err != nil {
		return Project{}, internal(err)
	}
	return projectFromRow(row), nil
}

// GetProject trả về project theo id. Project của tenant khác là "không tồn tại".
func (s *Service) GetProject(ctx context.Context, id uuid.UUID) (Project, error) {
	orgID, ok := OrgFrom(ctx)
	if !ok {
		return Project{}, internal(ErrNoTenant)
	}
	var row store.Project
	err := InTx(ctx, s.db, func(tx pgxv5.Tx) (err error) {
		row, err = store.New(tx).GetProject(ctx, store.GetProjectParams{OrgID: orgID, ID: id})
		return err
	})
	if errors.Is(err, pgxv5.ErrNoRows) {
		return Project{}, errNotFound
	}
	if err != nil {
		return Project{}, internal(err)
	}
	return projectFromRow(row), nil
}

// UpdateProject sửa tên hoặc settings. environment không đổi được sau khi tạo.
func (s *Service) UpdateProject(ctx context.Context, id uuid.UUID, u Update) (Project, error) {
	orgID, ok := OrgFrom(ctx)
	if !ok {
		return Project{}, internal(ErrNoTenant)
	}
	if err := u.validate(); err != nil {
		return Project{}, err
	}
	extra, err := s.checkSettings(ctx, u.Settings)
	if err != nil {
		return Project{}, err
	}
	var row store.Project
	var current *store.Project
	err = InTx(ctx, s.db, func(tx pgxv5.Tx) error {
		q := store.New(tx)
		var err error
		row, err = q.UpdateProject(ctx, store.UpdateProjectParams{
			OrgID: orgID, ID: id, Name: u.Name, Settings: u.Settings, IfUpdatedAt: u.IfUpdatedAt,
		})
		if errors.Is(err, pgxv5.ErrNoRows) {
			// Không dòng nào được sửa: phân biệt không tồn tại / đã lưu trữ / ETag lệch.
			cur, gerr := q.GetProject(ctx, store.GetProjectParams{OrgID: orgID, ID: id})
			if gerr == nil {
				current = &cur
			} else if !errors.Is(gerr, pgxv5.ErrNoRows) {
				return gerr
			}
		}
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{
			OrgID: orgID, ProjectID: &id, Action: "project.update", ResourceType: "project", ResourceID: &id,
			Outcome: audit.OutcomeSuccess, Details: withDetails(u.changed(), extra),
		})
	})
	if errors.Is(err, pgxv5.ErrNoRows) {
		switch {
		case current == nil:
			return Project{}, errNotFound
		case current.ArchivedAt != nil:
			return Project{}, errArchived
		default:
			return Project{}, httpx.ErrPreconditionFailed
		}
	}
	if err != nil {
		return Project{}, internal(err)
	}
	return projectFromRow(row), nil
}

// ArchiveProject xoá mềm project. Lưu trữ lại một project đã lưu trữ là no-op.
// Người gọi đã xác nhận project tồn tại trong tenant.
func (s *Service) ArchiveProject(ctx context.Context, id uuid.UUID) error {
	orgID, ok := OrgFrom(ctx)
	if !ok {
		return internal(ErrNoTenant)
	}
	err := InTx(ctx, s.db, func(tx pgxv5.Tx) error {
		n, err := store.New(tx).ArchiveProject(ctx, store.ArchiveProjectParams{OrgID: orgID, ID: id})
		if err != nil || n == 0 { // đã lưu trữ từ trước: không có thay đổi để ghi
			return err
		}
		return audit.Record(ctx, tx, audit.Event{
			OrgID: orgID, ProjectID: &id, Action: "project.archive", ResourceType: "project", ResourceID: &id,
			Outcome: audit.OutcomeSuccess,
		})
	})
	if err != nil {
		return internal(err)
	}
	return nil
}

type cursor struct {
	Sort  string    `json:"s"`
	Value string    `json:"v"`
	ID    uuid.UUID `json:"id"`
}

func encodeCursor(c cursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s, sort string) (*cursor, error) {
	if s == "" {
		return nil, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, errInvalidCursor
	}
	var c cursor
	if err := json.Unmarshal(b, &c); err != nil || c.Sort != sort || c.ID == uuid.Nil ||
		strings.ContainsRune(c.Value, 0) || len(c.Value) > 4*maxNameLen {
		return nil, errInvalidCursor
	}
	return &c, nil
}

// ListProjects liệt kê project chưa lưu trữ, phân trang keyset.
// next rỗng nghĩa là hết.
func (s *Service) ListProjects(ctx context.Context, p ListParams) (projects []Project, next string, err error) {
	orgID, ok := OrgFrom(ctx)
	if !ok {
		return nil, "", internal(ErrNoTenant)
	}
	if p.Sort == "" {
		p.Sort = "-created_at"
	}
	if p.Limit == 0 {
		p.Limit = DefaultLimit
	}
	if p.Limit < 1 || p.Limit > MaxLimit {
		return nil, "", apperr.New(apperr.KindInvalid, "invalid_parameter", "limit phải từ 1 đến 200")
	}
	var env *string
	switch p.Environment {
	case "":
	case "prod", "non_prod":
		env = &p.Environment
	default:
		return nil, "", apperr.New(apperr.KindInvalid, "invalid_parameter", "environment phải là prod hoặc non_prod")
	}
	cur, err := decodeCursor(p.Cursor, p.Sort)
	if err != nil {
		return nil, "", err
	}
	var afterID *uuid.UUID
	var afterName *string
	var afterCreated *time.Time
	if cur != nil {
		afterID = &cur.ID
		if strings.HasSuffix(p.Sort, "name") {
			afterName = &cur.Value
		} else {
			t, perr := time.Parse(time.RFC3339Nano, cur.Value)
			if perr != nil {
				return nil, "", errInvalidCursor
			}
			afterCreated = &t
		}
	}
	// Lấy thừa một dòng để biết còn trang sau hay không.
	limit := int32(p.Limit + 1) //nolint:gosec // đã chặn trong [1, MaxLimit]

	var rows []store.Project
	err = InTx(ctx, s.db, func(tx pgxv5.Tx) (err error) {
		q := store.New(tx)
		switch p.Sort {
		case "-created_at":
			rows, err = q.ListProjectsByCreatedDesc(ctx, store.ListProjectsByCreatedDescParams{
				OrgID: orgID, Environment: env, OnlyIds: p.OnlyIDs, AfterCreated: afterCreated, AfterID: afterID, RowLimit: limit})
		case "created_at":
			rows, err = q.ListProjectsByCreatedAsc(ctx, store.ListProjectsByCreatedAscParams{
				OrgID: orgID, Environment: env, OnlyIds: p.OnlyIDs, AfterCreated: afterCreated, AfterID: afterID, RowLimit: limit})
		case "name":
			rows, err = q.ListProjectsByNameAsc(ctx, store.ListProjectsByNameAscParams{
				OrgID: orgID, Environment: env, OnlyIds: p.OnlyIDs, AfterName: afterName, AfterID: afterID, RowLimit: limit})
		case "-name":
			rows, err = q.ListProjectsByNameDesc(ctx, store.ListProjectsByNameDescParams{
				OrgID: orgID, Environment: env, OnlyIds: p.OnlyIDs, AfterName: afterName, AfterID: afterID, RowLimit: limit})
		default:
			err = apperr.New(apperr.KindInvalid, "invalid_parameter",
				"sort phải là một trong: name, -name, created_at, -created_at")
		}
		return err
	})
	if err != nil {
		if _, ok := apperr.From(err); ok {
			return nil, "", err
		}
		return nil, "", internal(err)
	}

	if len(rows) > p.Limit {
		rows = rows[:p.Limit]
		last := rows[len(rows)-1]
		c := cursor{Sort: p.Sort, ID: last.ID, Value: last.CreatedAt.UTC().Format(time.RFC3339Nano)}
		if strings.HasSuffix(p.Sort, "name") {
			c.Value = last.Name
		}
		next = encodeCursor(c)
	}
	projects = make([]Project, len(rows))
	for i, r := range rows {
		projects[i] = projectFromRow(r)
	}
	return projects, next, nil
}

func orgFromRow(r store.Organization) Organization {
	return Organization{ID: r.ID, Slug: r.Slug, Name: r.Name, Status: r.Status,
		Settings: r.Settings, CreatedAt: r.CreatedAt.UTC(), UpdatedAt: r.UpdatedAt.UTC()}
}

func projectFromRow(r store.Project) Project {
	return Project{ID: r.ID, Slug: r.Slug, Name: r.Name, Environment: r.Environment,
		Settings: r.Settings, ArchivedAt: utcPtr(r.ArchivedAt), CreatedAt: r.CreatedAt.UTC(), UpdatedAt: r.UpdatedAt.UTC()}
}

// Thời gian trả ra API luôn ở UTC.
func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}
