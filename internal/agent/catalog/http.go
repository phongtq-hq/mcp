package catalog

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
	"github.com/hqgroups/mcp/internal/shared/pagination"
)

// Authorizer là thứ handler cần từ Authorization & Policy.
type Authorizer interface {
	Require(ctx context.Context, permission string, projectID *uuid.UUID) error
}

// Permission mà các endpoint ở đây yêu cầu. Giá trị phải có trong danh mục của authz.
const (
	permProjectRead  = "project:read"
	permAgentRead    = "agent:read"
	permAgentCreate  = "agent:create"
	permAgentUpdate  = "agent:update"
	permAgentPublish = "agent:publish"
	permAgentArchive = "agent:archive"
)

type handlers struct {
	svc   *Service
	authz Authorizer
}

// RegisterRoutes gắn các endpoint Agent (tiền tố /v1). Mọi route phải nằm sau
// middleware xác thực. idem bọc handler bằng xử lý Idempotency-Key.
func RegisterRoutes(mux *http.ServeMux, svc *Service, authz Authorizer,
	idem func(authorize func(*http.Request) error, next http.HandlerFunc) http.HandlerFunc) {
	h := &handlers{svc: svc, authz: authz}
	mux.HandleFunc("GET /v1/projects/{projectId}/agents", wrap(h.list))
	mux.HandleFunc("POST /v1/projects/{projectId}/agents", idem(func(r *http.Request) error {
		_, err := h.project(r, permAgentCreate)
		return err
	}, wrap(h.create)))
	mux.HandleFunc("GET /v1/agents/{agentId}", wrap(h.get))
	mux.HandleFunc("PATCH /v1/agents/{agentId}", wrap(h.update))
	mux.HandleFunc("DELETE /v1/agents/{agentId}", wrap(h.archive))
	mux.HandleFunc("GET /v1/agents/{agentId}/versions", wrap(h.listVersions))
	mux.HandleFunc("POST /v1/agents/{agentId}/versions", idem(func(r *http.Request) error {
		_, err := h.load(r, permAgentUpdate)
		return err
	}, wrap(h.createVersion)))
	mux.HandleFunc("GET /v1/agents/{agentId}/versions/{version}", wrap(h.getVersion))
	mux.HandleFunc("PUT /v1/agents/{agentId}/active-version", wrap(h.setActiveVersion))
}

func wrap(fn func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := fn(w, r); err != nil {
			httpx.WriteError(w, r, err)
		}
	}
}

func toAPIAgent(a Agent) api.Agent {
	return api.Agent{
		Id: &a.ID, ProjectId: &a.ProjectID, Slug: a.Slug, Name: a.Name, Description: a.Description,
		Status: api.AgentStatus(a.Status), ActiveVersion: a.ActiveVersion, LatestVersion: &a.LatestVersion,
		CreatedAt: &a.CreatedAt, UpdatedAt: &a.UpdatedAt,
	}
}

func toAPIVersion(v Version) api.AgentVersion {
	out := api.AgentVersion{
		Id: v.ID, AgentId: v.AgentID, Version: v.Version, SystemPrompt: v.SystemPrompt,
		ChangeNote: v.ChangeNote, CreatedBy: v.CreatedBy, CreatedAt: v.CreatedAt,
	}
	_ = json.Unmarshal(v.Config, &out.Config)
	return out
}

// requireHiding kiểm quyền đọc; thiếu quyền đọc thì tài nguyên "không tồn tại".
func (h *handlers) requireHiding(ctx context.Context, permission string, projectID uuid.UUID) error {
	if err := h.authz.Require(ctx, permission, &projectID); err != nil {
		if apperr.KindOf(err) == apperr.KindPermissionDenied {
			return errNotFound
		}
		return err
	}
	return nil
}

// project xác nhận project trong đường dẫn tồn tại trong tenant và principal
// đọc được nó, rồi kiểm permission cho hành động trên Agent.
func (h *handlers) project(r *http.Request, permission string) (uuid.UUID, error) {
	id, err := uuid.Parse(r.PathValue("projectId"))
	if err != nil {
		return uuid.Nil, errNotFound
	}
	if _, err := h.svc.projects.GetProject(r.Context(), id); err != nil {
		return uuid.Nil, err
	}
	if err := h.requireHiding(r.Context(), permProjectRead, id); err != nil {
		return uuid.Nil, err
	}
	if err := h.authz.Require(r.Context(), permission, &id); err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// load đọc Agent theo id rồi kiểm quyền: không tồn tại, thuộc tenant khác, hoặc
// không được đọc đều là 404; đọc được mà thiếu quyền cho hành động là 403.
func (h *handlers) load(r *http.Request, permissions ...string) (Agent, error) {
	id, err := uuid.Parse(r.PathValue("agentId"))
	if err != nil {
		return Agent{}, errNotFound
	}
	a, err := h.svc.Get(r.Context(), id)
	if err != nil {
		return Agent{}, err
	}
	if err := h.requireHiding(r.Context(), permAgentRead, a.ProjectID); err != nil {
		return Agent{}, err
	}
	for _, p := range permissions {
		if err := h.authz.Require(r.Context(), p, &a.ProjectID); err != nil {
			return Agent{}, err
		}
	}
	return a, nil
}

func writeAgent(w http.ResponseWriter, status int, a Agent) {
	w.Header().Set("ETag", httpx.ETag(a.UpdatedAt))
	httpx.WriteJSON(w, status, toAPIAgent(a))
}

func (h *handlers) list(w http.ResponseWriter, r *http.Request) error {
	projectID, err := h.project(r, permAgentRead)
	if err != nil {
		return err
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return apperr.New(apperr.KindInvalid, "invalid_parameter", "query string không hợp lệ")
	}
	limit, err := pagination.Limit(q.Get("limit"))
	if err != nil {
		return err
	}
	agents, next, err := h.svc.List(r.Context(), projectID, ListParams{
		Limit: limit, Cursor: q.Get("cursor"), Status: q.Get("status"), Search: q.Get("q"), Sort: q.Get("sort")})
	if err != nil {
		return err
	}
	page := struct {
		Data       []api.Agent `json:"data"`
		NextCursor *string     `json:"next_cursor"`
	}{Data: make([]api.Agent, len(agents))}
	for i, a := range agents {
		page.Data[i] = toAPIAgent(a)
	}
	if next != "" {
		page.NextCursor = &next
	}
	httpx.WriteJSON(w, http.StatusOK, page)
	return nil
}

func (h *handlers) create(w http.ResponseWriter, r *http.Request) error {
	projectID, err := h.project(r, permAgentCreate)
	if err != nil {
		return err
	}
	var body api.AgentCreate
	if err := httpx.DecodeJSON(r, &body); err != nil {
		return err
	}
	a, err := h.svc.Create(r.Context(), projectID, CreateInput{
		Slug: body.Slug, Name: body.Name, Description: body.Description,
		Version: VersionInput{SystemPrompt: body.SystemPrompt, Config: body.Config},
	})
	if err != nil {
		return err
	}
	w.Header().Set("Location", "/v1/agents/"+a.ID.String())
	writeAgent(w, http.StatusCreated, a)
	return nil
}

func (h *handlers) get(w http.ResponseWriter, r *http.Request) error {
	a, err := h.load(r)
	if err != nil {
		return err
	}
	writeAgent(w, http.StatusOK, a)
	return nil
}

func (h *handlers) update(w http.ResponseWriter, r *http.Request) error {
	a, err := h.load(r, permAgentUpdate)
	if err != nil {
		return err
	}
	var body api.AgentUpdate
	if err := httpx.DecodeJSON(r, &body); err != nil {
		return err
	}
	ifMatch, err := httpx.IfMatch(r)
	if err != nil {
		return err
	}
	u := UpdateInput{Name: body.Name, Description: body.Description, IfUpdatedAt: ifMatch}
	if body.Status != nil {
		s := string(*body.Status)
		u.Status = &s
		// Bật (lại) một agent là quyết định publish: người chỉ được viết version
		// không tự bật lại được agent đã bị tắt.
		if s == StatusActive {
			if err := h.authz.Require(r.Context(), permAgentPublish, &a.ProjectID); err != nil {
				return err
			}
		}
	}
	a, err = h.svc.Update(r.Context(), a.ID, u)
	if err != nil {
		return err
	}
	writeAgent(w, http.StatusOK, a)
	return nil
}

func (h *handlers) archive(w http.ResponseWriter, r *http.Request) error {
	a, err := h.load(r, permAgentArchive)
	if err != nil {
		return err
	}
	if err := h.svc.Archive(r.Context(), a.ID); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *handlers) listVersions(w http.ResponseWriter, r *http.Request) error {
	a, err := h.load(r)
	if err != nil {
		return err
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return apperr.New(apperr.KindInvalid, "invalid_parameter", "query string không hợp lệ")
	}
	limit, err := pagination.Limit(q.Get("limit"))
	if err != nil {
		return err
	}
	versions, next, err := h.svc.ListVersions(r.Context(), a.ID, limit, q.Get("cursor"))
	if err != nil {
		return err
	}
	page := struct {
		Data       []api.AgentVersion `json:"data"`
		NextCursor *string            `json:"next_cursor"`
	}{Data: make([]api.AgentVersion, len(versions))}
	for i, v := range versions {
		page.Data[i] = toAPIVersion(v)
	}
	if next != "" {
		page.NextCursor = &next
	}
	httpx.WriteJSON(w, http.StatusOK, page)
	return nil
}

func (h *handlers) createVersion(w http.ResponseWriter, r *http.Request) error {
	a, err := h.load(r, permAgentUpdate)
	if err != nil {
		return err
	}
	var body api.AgentVersionCreate
	if err := httpx.DecodeJSON(r, &body); err != nil {
		return err
	}
	in := VersionInput{SystemPrompt: body.SystemPrompt, Config: body.Config, ChangeNote: body.ChangeNote}
	if body.Activate != nil && *body.Activate {
		// Tạo và publish trong một bước cần cả quyền publish.
		if err := h.authz.Require(r.Context(), permAgentPublish, &a.ProjectID); err != nil {
			return err
		}
		in.Activate = true
	}
	v, err := h.svc.CreateVersion(r.Context(), a.ID, a.ProjectID, in)
	if err != nil {
		return err
	}
	w.Header().Set("Location", "/v1/agents/"+a.ID.String()+"/versions/"+strconv.Itoa(v.Version))
	httpx.WriteJSON(w, http.StatusCreated, toAPIVersion(v))
	return nil
}

func versionParam(s string) (int, error) {
	n, err := strconv.ParseInt(s, 10, 32)
	if err != nil || n < 1 {
		return 0, errNotFound
	}
	return int(n), nil
}

func (h *handlers) getVersion(w http.ResponseWriter, r *http.Request) error {
	a, err := h.load(r)
	if err != nil {
		return err
	}
	n, err := versionParam(r.PathValue("version"))
	if err != nil {
		return err
	}
	v, err := h.svc.GetVersion(r.Context(), a.ID, n)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, toAPIVersion(v))
	return nil
}

func (h *handlers) setActiveVersion(w http.ResponseWriter, r *http.Request) error {
	a, err := h.load(r, permAgentPublish)
	if err != nil {
		return err
	}
	var body api.SetActiveAgentVersionJSONBody
	if err := httpx.DecodeJSON(r, &body); err != nil {
		return err
	}
	if body.Version < 1 || body.Version > 1<<31-1 {
		return invalid("version phải là số nguyên dương")
	}
	a, err = h.svc.SetActiveVersion(r.Context(), a.ID, body.Version)
	if err != nil {
		return err
	}
	writeAgent(w, http.StatusOK, a)
	return nil
}
