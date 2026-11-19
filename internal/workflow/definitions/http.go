package definitions

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"

	"github.com/google/uuid"

	"github.com/hqgroups/mcp/internal/api"
	"github.com/hqgroups/mcp/internal/platform/identity"
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
	permProjectRead   = "project:read"
	permRead          = "workflow:read"
	permWrite         = "workflow:write"
	permRun           = "workflow:run"
	permExecCreate    = "execution:create"
	permExecRead      = "execution:read"
	permExecCancel    = "execution:cancel"
	idempotencyHeader = "Idempotency-Key"
)

type handlers struct {
	svc   *Service
	authz Authorizer
}

// RegisterRoutes gắn các endpoint Workflow (tiền tố /v1). Mọi route phải nằm sau
// middleware xác thực.
func RegisterRoutes(mux *http.ServeMux, svc *Service, authz Authorizer,
	idem func(authorize func(*http.Request) error, next http.HandlerFunc) http.HandlerFunc) {
	h := &handlers{svc: svc, authz: authz}
	mux.HandleFunc("GET /v1/projects/{projectId}/workflows", wrap(h.list))
	mux.HandleFunc("POST /v1/projects/{projectId}/workflows", idem(func(r *http.Request) error {
		_, err := h.project(r, permWrite)
		return err
	}, wrap(h.create)))
	mux.HandleFunc("GET /v1/workflows/{workflowId}", wrap(h.get))
	mux.HandleFunc("PATCH /v1/workflows/{workflowId}", wrap(h.update))
	mux.HandleFunc("DELETE /v1/workflows/{workflowId}", wrap(h.archive))
	mux.HandleFunc("GET /v1/workflows/{workflowId}/executions", wrap(h.listRuns))
	mux.HandleFunc("POST /v1/workflows/{workflowId}/executions", idem(func(r *http.Request) error {
		_, err := h.load(r, permRun, permExecCreate)
		return err
	}, wrap(h.start)))
	mux.HandleFunc("GET /v1/workflow-executions/{workflowExecutionId}", wrap(h.getRun))
	mux.HandleFunc("POST /v1/workflow-executions/{workflowExecutionId}/cancel", wrap(h.cancelRun))
}

func wrap(fn func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := fn(w, r); err != nil {
			httpx.WriteError(w, r, err)
		}
	}
}

func (h *handlers) requireHiding(ctx context.Context, permission string, projectID uuid.UUID) error {
	if err := h.authz.Require(ctx, permission, &projectID); err != nil {
		if apperr.KindOf(err) == apperr.KindPermissionDenied {
			return errNotFound
		}
		return err
	}
	return nil
}

// project xác nhận project trong đường dẫn tồn tại trong tenant và principal đọc được
// nó, rồi kiểm permission cho hành động.
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

// load đọc workflow theo id rồi kiểm quyền: không tồn tại, thuộc tenant khác, hoặc
// không được đọc đều là 404; đọc được mà thiếu quyền cho hành động là 403.
func (h *handlers) load(r *http.Request, permissions ...string) (Workflow, error) {
	id, err := uuid.Parse(r.PathValue("workflowId"))
	if err != nil {
		return Workflow{}, errNotFound
	}
	w, err := h.svc.Get(r.Context(), id)
	if err != nil {
		return Workflow{}, err
	}
	if err := h.requireHiding(r.Context(), permRead, w.ProjectID); err != nil {
		return Workflow{}, err
	}
	for _, p := range permissions {
		if err := h.authz.Require(r.Context(), p, &w.ProjectID); err != nil {
			return Workflow{}, err
		}
	}
	return w, nil
}

func (h *handlers) loadRun(r *http.Request, permissions ...string) (Run, error) {
	id, err := uuid.Parse(r.PathValue("workflowExecutionId"))
	if err != nil {
		return Run{}, errNotFound
	}
	// Đọc không ghi gì trước, kiểm quyền, rồi mới đọc lại trạng thái từ execution (việc
	// đó ghi vào database): người không được đọc lần chạy không kích được lần ghi nào.
	run, err := h.svc.LookupRun(r.Context(), id)
	if err != nil {
		return Run{}, err
	}
	if err := h.requireHiding(r.Context(), permRead, run.ProjectID); err != nil {
		return Run{}, err
	}
	for _, p := range permissions {
		if err := h.authz.Require(r.Context(), p, &run.ProjectID); err != nil {
			return Run{}, err
		}
	}
	return h.svc.GetRun(r.Context(), id)
}

func object(raw json.RawMessage) *map[string]interface{} {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]interface{}
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return nil
	}
	return &m
}

func toAPI(w Workflow) api.Workflow {
	out := api.Workflow{Id: w.ID, ProjectId: w.ProjectID, Slug: w.Slug, Name: w.Name, Description: w.Description, Version: &w.Version,
		Schedule: w.Schedule, Status: api.WorkflowStatus(w.Status), CreatedAt: &w.CreatedAt, UpdatedAt: &w.UpdatedAt}
	_ = json.Unmarshal(w.Definition, &out.Definition)
	return out
}

func toAPIRun(run Run) api.WorkflowExecution {
	out := api.WorkflowExecution{Id: run.ID, WorkflowId: run.WorkflowID, WorkflowVersion: run.WorkflowVersion,
		TriggerType: api.WorkflowExecutionTriggerType(run.TriggerType), Status: api.WorkflowExecutionStatus(run.Status),
		Input: object(run.Input), Output: object(run.Output), ExecutionId: run.ExecutionID,
		StartedAt: run.StartedAt, FinishedAt: run.FinishedAt, CreatedAt: &run.CreatedAt, UpdatedAt: &run.UpdatedAt}
	if len(run.Error) > 0 {
		var p api.Problem
		if json.Unmarshal(run.Error, &p) == nil {
			out.Error = &p
		}
	}
	return out
}

func writePage(w http.ResponseWriter, data any, next string) {
	page := map[string]any{"data": data, "next_cursor": nil}
	if next != "" {
		page["next_cursor"] = next
	}
	httpx.WriteJSON(w, http.StatusOK, page)
}

func badParam(msg string) error { return apperr.New(apperr.KindInvalid, "invalid_parameter", msg) }

var workflowStatuses = map[string]bool{statusDraft: true, statusActive: true, statusDisabled: true, statusArchived: true}

func (h *handlers) list(w http.ResponseWriter, r *http.Request) error {
	projectID, err := h.project(r, permRead)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	limit, err := pagination.Limit(q.Get("limit"))
	if err != nil {
		return err
	}
	status := q.Get("status")
	if status != "" && !workflowStatuses[status] {
		return badParam("status không hợp lệ")
	}
	items, next, err := h.svc.List(r.Context(), projectID, status, limit, q.Get("cursor"))
	if err != nil {
		return err
	}
	data := make([]api.Workflow, len(items))
	for i, it := range items {
		data[i] = toAPI(it)
	}
	writePage(w, data, next)
	return nil
}

var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)

// rawBody đọc body ở dạng các trường thô: định nghĩa được kiểm bằng bộ đọc chặt của
// package (khoá lạ bị từ chối), không qua kiểu sinh từ OpenAPI.
func rawBody(r *http.Request) (map[string]json.RawMessage, error) {
	var body map[string]json.RawMessage
	if err := httpx.DecodeJSON(r, &body); err != nil {
		return nil, err
	}
	return body, nil
}

func stringField(body map[string]json.RawMessage, key string) (*string, bool, error) {
	raw, ok := body[key]
	if !ok {
		return nil, false, nil
	}
	if string(raw) == "null" {
		return nil, true, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, true, invalid(key + " phải là chuỗi")
	}
	return &s, true, nil
}

func (h *handlers) create(w http.ResponseWriter, r *http.Request) error {
	projectID, err := h.project(r, permWrite)
	if err != nil {
		return err
	}
	body, err := rawBody(r)
	if err != nil {
		return err
	}
	for k := range body {
		switch k {
		case "slug", "name", "description", "definition", "schedule":
		default:
			return invalid("trường không được hỗ trợ: " + k)
		}
	}
	in := CreateInput{Definition: body["definition"]}
	slug, _, err := stringField(body, "slug")
	if err != nil {
		return err
	}
	name, _, err := stringField(body, "name")
	if err != nil {
		return err
	}
	if slug == nil || name == nil {
		return invalid("slug và name là bắt buộc")
	}
	in.Slug, in.Name = *slug, *name
	if !slugPattern.MatchString(in.Slug) {
		return invalid("slug phải khớp ^[a-z0-9][a-z0-9-]{1,62}$")
	}
	if in.Description, _, err = stringField(body, "description"); err != nil {
		return err
	}
	if in.Schedule, _, err = stringField(body, "schedule"); err != nil {
		return err
	}
	out, err := h.svc.Create(r.Context(), projectID, in)
	if err != nil {
		return err
	}
	w.Header().Set("ETag", httpx.ETag(out.UpdatedAt))
	w.Header().Set("Location", "/v1/workflows/"+out.ID.String())
	httpx.WriteJSON(w, http.StatusCreated, toAPI(out))
	return nil
}

func (h *handlers) get(w http.ResponseWriter, r *http.Request) error {
	out, err := h.load(r)
	if err != nil {
		return err
	}
	w.Header().Set("ETag", httpx.ETag(out.UpdatedAt))
	httpx.WriteJSON(w, http.StatusOK, toAPI(out))
	return nil
}

func (h *handlers) update(w http.ResponseWriter, r *http.Request) error {
	cur, err := h.load(r, permWrite)
	if err != nil {
		return err
	}
	ifMatch, err := httpx.IfMatch(r)
	if err != nil {
		return err
	}
	body, err := rawBody(r)
	if err != nil {
		return err
	}
	for k := range body {
		switch k {
		case "name", "description", "definition", "schedule", "status":
		default:
			return invalid("trường không được hỗ trợ: " + k)
		}
	}
	in := UpdateInput{IfMatch: ifMatch, Definition: body["definition"]}
	if in.Name, _, err = stringField(body, "name"); err != nil {
		return err
	}
	if in.Description, _, err = stringField(body, "description"); err != nil {
		return err
	}
	if in.Schedule, in.SetSchedule, err = stringField(body, "schedule"); err != nil {
		return err
	}
	if in.Status, _, err = stringField(body, "status"); err != nil {
		return err
	}
	// Kích hoạt workflow, hoặc đổi thứ một workflow đang active chạy, là cho phép nó chạy
	// dưới quyền của mình, kể cả theo lịch: người làm việc đó phải tự có quyền chạy
	// workflow và tạo execution. Service quyết định thay đổi có cần quyền này không (dưới
	// khoá của dòng); ở đây chỉ kiểm khi request có thể cần tới nó.
	if (in.Status != nil && *in.Status == statusActive) || (cur.Status == statusActive && (in.Definition != nil || in.SetSchedule)) {
		in.RunChecked = true
		for _, p := range []string{permRun, permExecCreate} {
			if err := h.authz.Require(r.Context(), p, &cur.ProjectID); err != nil {
				in.RunDenied = err
				break
			}
		}
	}
	out, err := h.svc.Update(r.Context(), cur.ID, in)
	if err != nil {
		return err
	}
	w.Header().Set("ETag", httpx.ETag(out.UpdatedAt))
	httpx.WriteJSON(w, http.StatusOK, toAPI(out))
	return nil
}

func (h *handlers) archive(w http.ResponseWriter, r *http.Request) error {
	cur, err := h.load(r, permWrite)
	if err != nil {
		return err
	}
	if err := h.svc.Archive(r.Context(), cur.ID); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *handlers) start(w http.ResponseWriter, r *http.Request) error {
	cur, err := h.load(r, permRun, permExecCreate)
	if err != nil {
		return err
	}
	// Một lần chạy gọi model và tốn tiền: bắt buộc có Idempotency-Key.
	if r.Header.Get(idempotencyHeader) == "" {
		return apperr.New(apperr.KindInvalid, "idempotency_key_required", "chạy workflow cần header Idempotency-Key")
	}
	var body struct {
		Input *struct {
			Message string `json:"message"`
		} `json:"input"`
	}
	if r.ContentLength != 0 {
		if err := httpx.DecodeJSON(r, &body); err != nil {
			return err
		}
	}
	note := ""
	if body.Input != nil {
		note = body.Input.Message
	}
	trigger := TriggerManual
	if p, ok := identity.PrincipalFrom(r.Context()); ok && p.Type != identity.PrincipalUser {
		trigger = TriggerAPI
	}
	run, err := h.svc.Start(r.Context(), cur.ID, trigger, note, "")
	if err != nil {
		return err
	}
	w.Header().Set("Location", "/v1/workflow-executions/"+run.ID.String())
	httpx.WriteJSON(w, http.StatusAccepted, toAPIRun(run))
	return nil
}

var runStatuses = map[string]bool{"queued": true, "running": true, "waiting_approval": true, "paused": true, "completed": true,
	"failed": true, "cancelled": true, "budget_exceeded": true, "timed_out": true}

func (h *handlers) listRuns(w http.ResponseWriter, r *http.Request) error {
	// Lần chạy mang input / output của execution: cần cả quyền đọc execution.
	cur, err := h.load(r, permExecRead)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	limit, err := pagination.Limit(q.Get("limit"))
	if err != nil {
		return err
	}
	status := q.Get("status")
	if status != "" && !runStatuses[status] {
		return badParam("status không hợp lệ")
	}
	items, next, err := h.svc.ListRuns(r.Context(), cur.ID, status, limit, q.Get("cursor"))
	if err != nil {
		return err
	}
	data := make([]api.WorkflowExecution, len(items))
	for i, it := range items {
		data[i] = toAPIRun(it)
	}
	writePage(w, data, next)
	return nil
}

func (h *handlers) getRun(w http.ResponseWriter, r *http.Request) error {
	run, err := h.loadRun(r, permExecRead)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, toAPIRun(run))
	return nil
}

func (h *handlers) cancelRun(w http.ResponseWriter, r *http.Request) error {
	run, err := h.loadRun(r, permRun, permExecCancel)
	if err != nil {
		return err
	}
	if run, err = h.svc.CancelRun(r.Context(), run.ID); err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusAccepted, toAPIRun(run))
	return nil
}
