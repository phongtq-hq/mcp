package tenancy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"

	"github.com/google/uuid"

	"github.com/hqgroups/mcp/internal/api"
	apperr "github.com/hqgroups/mcp/internal/shared/errors"
	"github.com/hqgroups/mcp/internal/shared/httpx"
)

// Authorizer là thứ handler cần từ Authorization & Policy.
type Authorizer interface {
	// Require kiểm tra principal của request có permission hay không.
	// projectID nil là hành động cấp org.
	Require(ctx context.Context, permission string, projectID *uuid.UUID) error
	// ReadableProjects cho biết principal đọc được mọi project của org (all),
	// hay chỉ một số project cụ thể. Không đọc được project nào thì trả lỗi.
	ReadableProjects(ctx context.Context) (all bool, projectIDs []uuid.UUID, err error)
}

// Permission mà các endpoint ở đây yêu cầu. Giá trị phải có trong danh mục của authz.
const (
	permOrganizationRead   = "organization:read"
	permOrganizationUpdate = "organization:update"
	permProjectRead        = "project:read"
	permProjectCreate      = "project:create"
	permProjectUpdate      = "project:update"
	permProjectArchive     = "project:archive"
)

type handlers struct {
	svc   *Service
	authz Authorizer
}

// Idempotent bọc một handler bằng xử lý Idempotency-Key. authorize kiểm quyền
// của request và chạy trước khi nhận key hay phát lại phản hồi cũ.
type Idempotent func(authorize func(*http.Request) error, next http.HandlerFunc) http.HandlerFunc

// RegisterRoutes gắn các endpoint Organization và Project (tiền tố /v1).
// Mọi route phải nằm sau middleware xác thực.
func RegisterRoutes(mux *http.ServeMux, svc *Service, authz Authorizer, idem Idempotent) {
	h := &handlers{svc: svc, authz: authz}
	mux.HandleFunc("GET /v1/organization", wrap(h.getOrganization))
	mux.HandleFunc("PATCH /v1/organization", wrap(h.updateOrganization))
	mux.HandleFunc("GET /v1/projects", wrap(h.listProjects))
	mux.HandleFunc("POST /v1/projects", idem(func(r *http.Request) error {
		return h.authz.Require(r.Context(), permProjectCreate, nil)
	}, wrap(h.createProject)))
	mux.HandleFunc("GET /v1/projects/{projectId}", wrap(h.getProject))
	mux.HandleFunc("PATCH /v1/projects/{projectId}", wrap(h.updateProject))
	mux.HandleFunc("DELETE /v1/projects/{projectId}", wrap(h.archiveProject))
}

func wrap(fn func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := fn(w, r); err != nil {
			httpx.WriteError(w, r, err)
		}
	}
}

// Chuyển giữa kiểu API (sinh từ openapi.yaml) và kiểu miền.

func settingsToRaw(m *map[string]any) json.RawMessage {
	if m == nil {
		return nil
	}
	b, _ := json.Marshal(*m)
	return b
}

func settingsFromRaw(raw json.RawMessage) *map[string]any {
	m := map[string]any{}
	_ = json.Unmarshal(raw, &m)
	return &m
}

func toAPIOrganization(o Organization) api.Organization {
	return api.Organization{
		Id: &o.ID, Slug: o.Slug, Name: o.Name, Status: api.OrganizationStatus(o.Status),
		Settings: settingsFromRaw(o.Settings), CreatedAt: &o.CreatedAt, UpdatedAt: &o.UpdatedAt,
	}
}

func toAPIProject(p Project) api.Project {
	return api.Project{
		Id: &p.ID, Slug: p.Slug, Name: p.Name, Environment: api.Environment(p.Environment),
		Settings: settingsFromRaw(p.Settings), ArchivedAt: p.ArchivedAt,
		CreatedAt: &p.CreatedAt, UpdatedAt: &p.UpdatedAt,
	}
}

func readUpdate(r *http.Request, name *string, settings *map[string]any) (Update, error) {
	ifMatch, err := httpx.IfMatch(r)
	if err != nil {
		return Update{}, err
	}
	return Update{Name: name, Settings: settingsToRaw(settings), IfUpdatedAt: ifMatch}, nil
}

func (h *handlers) getOrganization(w http.ResponseWriter, r *http.Request) error {
	if err := h.authz.Require(r.Context(), permOrganizationRead, nil); err != nil {
		return err
	}
	org, err := h.svc.GetOrganization(r.Context())
	if err != nil {
		return err
	}
	w.Header().Set("ETag", httpx.ETag(org.UpdatedAt))
	httpx.WriteJSON(w, http.StatusOK, toAPIOrganization(org))
	return nil
}

func (h *handlers) updateOrganization(w http.ResponseWriter, r *http.Request) error {
	if err := h.authz.Require(r.Context(), permOrganizationUpdate, nil); err != nil {
		return err
	}
	var body api.OrganizationUpdate
	if err := httpx.DecodeJSON(r, &body); err != nil {
		return err
	}
	u, err := readUpdate(r, body.Name, body.Settings)
	if err != nil {
		return err
	}
	org, err := h.svc.UpdateOrganization(r.Context(), u)
	if err != nil {
		return err
	}
	w.Header().Set("ETag", httpx.ETag(org.UpdatedAt))
	httpx.WriteJSON(w, http.StatusOK, toAPIOrganization(org))
	return nil
}

func (h *handlers) listProjects(w http.ResponseWriter, r *http.Request) error {
	all, only, err := h.authz.ReadableProjects(r.Context())
	if err != nil {
		return err
	}
	// url.ParseQuery thay cho r.URL.Query(): query string hỏng là lỗi 400,
	// không âm thầm bỏ tham số (một cursor hỏng sẽ thành "trang đầu").
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return apperr.New(apperr.KindInvalid, "invalid_parameter", "query string không hợp lệ")
	}
	p := ListParams{Cursor: q.Get("cursor"), Environment: q.Get("environment"), Sort: q.Get("sort")}
	if !all {
		p.OnlyIDs = only
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n == 0 {
			return apperr.New(apperr.KindInvalid, "invalid_parameter", "limit phải từ 1 đến 200")
		}
		p.Limit = n
	}
	projects, next, err := h.svc.ListProjects(r.Context(), p)
	if err != nil {
		return err
	}
	page := struct {
		Data       []api.Project `json:"data"`
		NextCursor *string       `json:"next_cursor"`
	}{Data: make([]api.Project, len(projects))}
	for i, pr := range projects {
		page.Data[i] = toAPIProject(pr)
	}
	if next != "" {
		page.NextCursor = &next
	}
	httpx.WriteJSON(w, http.StatusOK, page)
	return nil
}

func (h *handlers) createProject(w http.ResponseWriter, r *http.Request) error {
	if err := h.authz.Require(r.Context(), permProjectCreate, nil); err != nil {
		return err
	}
	var body api.ProjectCreate
	if err := httpx.DecodeJSON(r, &body); err != nil {
		return err
	}
	p, err := h.svc.CreateProject(r.Context(), ProjectCreate{
		Slug: body.Slug, Name: body.Name, Environment: string(body.Environment),
		Settings: settingsToRaw(body.Settings)})
	if err != nil {
		return err
	}
	w.Header().Set("ETag", httpx.ETag(p.UpdatedAt))
	w.Header().Set("Location", "/v1/projects/"+p.ID.String())
	httpx.WriteJSON(w, http.StatusCreated, toAPIProject(p))
	return nil
}

// loadProject đọc project theo id trong đường dẫn rồi mới kiểm tra quyền.
// Project không tồn tại, thuộc tenant khác, hoặc principal không được đọc đều
// là 404: không ai dò được sự tồn tại của thứ mình không có quyền xem. 403 chỉ
// dành cho trường hợp đọc được nhưng thiếu quyền cho hành động.
func (h *handlers) loadProject(r *http.Request, permission string) (Project, error) {
	id, err := uuid.Parse(r.PathValue("projectId"))
	if err != nil {
		return Project{}, errNotFound
	}
	p, err := h.svc.GetProject(r.Context(), id)
	if err != nil {
		return Project{}, err
	}
	if err := h.authz.Require(r.Context(), permProjectRead, &p.ID); err != nil {
		if apperr.KindOf(err) == apperr.KindPermissionDenied {
			return Project{}, errNotFound
		}
		return Project{}, err
	}
	if permission != permProjectRead {
		if err := h.authz.Require(r.Context(), permission, &p.ID); err != nil {
			return Project{}, err
		}
	}
	return p, nil
}

func (h *handlers) getProject(w http.ResponseWriter, r *http.Request) error {
	p, err := h.loadProject(r, permProjectRead)
	if err != nil {
		return err
	}
	w.Header().Set("ETag", httpx.ETag(p.UpdatedAt))
	httpx.WriteJSON(w, http.StatusOK, toAPIProject(p))
	return nil
}

func (h *handlers) updateProject(w http.ResponseWriter, r *http.Request) error {
	p, err := h.loadProject(r, permProjectUpdate)
	if err != nil {
		return err
	}
	var body api.ProjectUpdate
	if err := httpx.DecodeJSON(r, &body); err != nil {
		return err
	}
	u, err := readUpdate(r, body.Name, body.Settings)
	if err != nil {
		return err
	}
	p, err = h.svc.UpdateProject(r.Context(), p.ID, u)
	if err != nil {
		return err
	}
	w.Header().Set("ETag", httpx.ETag(p.UpdatedAt))
	httpx.WriteJSON(w, http.StatusOK, toAPIProject(p))
	return nil
}

func (h *handlers) archiveProject(w http.ResponseWriter, r *http.Request) error {
	p, err := h.loadProject(r, permProjectArchive)
	if err != nil {
		return err
	}
	if err := h.svc.ArchiveProject(r.Context(), p.ID); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
