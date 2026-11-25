package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/hqgroups/mcp/internal/agent/orchestrator"
	"github.com/hqgroups/mcp/internal/api"
	"github.com/hqgroups/mcp/internal/platform/identity"
	apperr "github.com/hqgroups/mcp/internal/shared/errors"
	"github.com/hqgroups/mcp/internal/shared/httpx"
	"github.com/hqgroups/mcp/internal/shared/pagination"
)

// Authorizer là thứ handler cần từ Authorization & Policy.
type Authorizer interface {
	Require(ctx context.Context, permission string, projectID *uuid.UUID) error
	// Missing trả về permission trong danh sách mà principal không có, không ghi audit.
	Missing(ctx context.Context, permissions []string, projectID *uuid.UUID) ([]string, error)
}

// Permission mà các endpoint ở đây yêu cầu. Giá trị phải có trong danh mục của authz.
const (
	permProjectRead   = "project:read"
	permExecRead      = "execution:read"
	permExecCreate    = "execution:create"
	permExecCancel    = "execution:cancel"
	permExecResume    = "execution:resume"
	permExecReadFull  = "execution:read_content"
	permAgentPublish  = "agent:publish"
	idempotencyHeader = "Idempotency-Key"
)

type handlers struct {
	svc   *Service
	authz Authorizer
	// streams đếm luồng sự kiện đang mở của process này.
	streams eventStreams
}

// RegisterRoutes gắn các endpoint Execution (tiền tố /v1). Mọi route phải nằm sau
// middleware xác thực.
func RegisterRoutes(mux *http.ServeMux, svc *Service, authz Authorizer,
	idem func(authorize func(*http.Request) error, next http.HandlerFunc) http.HandlerFunc) {
	h := &handlers{svc: svc, authz: authz}
	mux.HandleFunc("GET /v1/projects/{projectId}/executions", wrap(h.list))
	mux.HandleFunc("POST /v1/projects/{projectId}/executions", idem(func(r *http.Request) error {
		_, err := h.project(r, permExecCreate)
		return err
	}, wrap(h.create)))
	mux.HandleFunc("POST /v1/projects/{projectId}/plan-proposals", idem(func(r *http.Request) error {
		_, err := h.project(r, permExecCreate)
		return err
	}, wrap(h.proposePlan)))
	mux.HandleFunc("GET /v1/executions/{executionId}/plan", wrap(h.planProposal))
	mux.HandleFunc("POST /v1/executions/{executionId}/plan/run", idem(func(r *http.Request) error {
		_, err := h.load(r, permExecCreate)
		return err
	}, wrap(h.runProposal)))
	mux.HandleFunc("GET /v1/executions/{executionId}", wrap(h.get))
	mux.HandleFunc("POST /v1/executions/{executionId}/cancel", wrap(h.cancel))
	mux.HandleFunc("POST /v1/executions/{executionId}/pause", wrap(h.pause))
	mux.HandleFunc("POST /v1/executions/{executionId}/resume", wrap(h.resume))
	mux.HandleFunc("GET /v1/executions/{executionId}/events", wrap(h.events))
	mux.HandleFunc("GET /v1/executions/{executionId}/tasks", wrap(h.tasks))
	mux.HandleFunc("GET /v1/tasks/{taskId}", wrap(h.task))
	mux.HandleFunc("GET /v1/tasks/{taskId}/steps", wrap(h.steps))
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

// project xác nhận project trong đường dẫn tồn tại trong tenant và principal đọc
// được nó, rồi kiểm permission cho hành động.
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

// load đọc execution theo id rồi kiểm quyền: không tồn tại, thuộc tenant khác, hoặc
// không được đọc đều là 404; đọc được mà thiếu quyền cho hành động là 403.
func (h *handlers) load(r *http.Request, permissions ...string) (Execution, error) {
	id, err := uuid.Parse(r.PathValue("executionId"))
	if err != nil {
		return Execution{}, errNotFound
	}
	e, err := h.svc.Get(r.Context(), id)
	if err != nil {
		return Execution{}, err
	}
	if err := h.requireHiding(r.Context(), permExecRead, e.ProjectID); err != nil {
		return Execution{}, err
	}
	for _, p := range permissions {
		if err := h.authz.Require(r.Context(), p, &e.ProjectID); err != nil {
			return Execution{}, err
		}
	}
	return e, nil
}

func (h *handlers) loadTask(r *http.Request, permissions ...string) (Task, error) {
	id, err := uuid.Parse(r.PathValue("taskId"))
	if err != nil {
		return Task{}, errNotFound
	}
	t, err := h.svc.GetTask(r.Context(), id)
	if err != nil {
		return Task{}, err
	}
	if err := h.requireHiding(r.Context(), permExecRead, t.ProjectID); err != nil {
		return Task{}, err
	}
	for _, p := range permissions {
		if err := h.authz.Require(r.Context(), p, &t.ProjectID); err != nil {
			return Task{}, err
		}
	}
	return t, nil
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

func problem(raw json.RawMessage) *api.Problem {
	if len(raw) == 0 {
		return nil
	}
	var p api.Problem
	if json.Unmarshal(raw, &p) != nil {
		return nil
	}
	return &p
}

// canReadContent: principal đọc được NỘI DUNG (input, output, goal) của execution trong
// project này. execution:read chỉ cho xem trạng thái, chi phí và định danh: câu trả lời
// của Agent có thể trích dữ liệu nhạy cảm nó đã đọc (giá vốn…), nên người chỉ được xem
// (viewer, approver) không thấy nó (review bảo mật 2026-11-25). Không ghi audit: đây là
// lọc trường, không phải từ chối truy cập.
func (h *handlers) canReadContent(ctx context.Context, projectID uuid.UUID) (bool, error) {
	missing, err := h.authz.Missing(ctx, []string{permExecReadFull}, &projectID)
	return len(missing) == 0, err
}

// writeExecution ghi một execution ra phản hồi, bỏ nội dung nếu người gọi thiếu
// execution:read_content (mọi endpoint trả execution đi qua đây).
func (h *handlers) writeExecution(w http.ResponseWriter, r *http.Request, status int, e Execution) error {
	full, err := h.canReadContent(r.Context(), e.ProjectID)
	if err != nil {
		return err
	}
	out := toAPIExecution(e)
	if !full {
		out = redactExecution(out)
	}
	httpx.WriteJSON(w, status, out)
	return nil
}

// redactExecution / redactTask bỏ nội dung khỏi bản trả về cho người thiếu
// execution:read_content.
func redactExecution(e api.Execution) api.Execution {
	e.Input, e.Output = nil, nil
	return e
}

func redactTask(t api.Task) api.Task {
	t.Goal, t.Output, t.OutputSummary = "", nil, nil
	return t
}

func toAPIExecution(e Execution) api.Execution {
	out := api.Execution{
		Id: e.ID, ProjectId: e.ProjectID, Mode: api.ExecutionMode(e.Mode), Status: api.ExecutionStatus(e.Status),
		Input: object(e.Input), Output: object(e.Output), Error: problem(e.Error),
		EstimatedCost: api.Money{Amount: e.EstimatedCost, Currency: e.Currency},
		TraceId:       e.TraceID, StartedAt: e.StartedAt, FinishedAt: e.FinishedAt, CreatedAt: &e.CreatedAt, UpdatedAt: &e.UpdatedAt,
		PauseRequestedAt: e.PauseRequestedAt,
	}
	typ := api.ExecutionInitiatorType(e.InitiatorType)
	out.Initiator = &struct {
		Id   *uuid.UUID                  `json:"id,omitempty"`
		Type *api.ExecutionInitiatorType `json:"type,omitempty"`
	}{Id: e.InitiatorID, Type: &typ}
	return out
}

func toAPITask(t Task) api.Task {
	out := api.Task{
		Id: t.ID, ExecutionId: t.ExecutionID, ParentTaskId: t.ParentTaskID, AgentId: t.AgentID, AgentVersion: t.AgentVersion,
		Depth: t.Depth, Goal: t.Goal, Status: api.TaskStatus(t.Status), Output: object(t.Output), Error: problem(t.Error),
		EstimatedCost: &api.Money{Amount: t.EstimatedCost, Currency: t.Currency},
		Iterations:    &t.Iterations, ToolCalls: &t.ToolCalls, StartedAt: t.StartedAt, FinishedAt: t.FinishedAt,
		PlanKey: t.PlanKey, OutputSummary: t.OutputSummary,
	}
	deps := t.DependsOn
	if deps == nil {
		deps = []uuid.UUID{}
	}
	out.DependsOn = &deps
	onFailure := api.TaskOnFailure(t.OnFailure)
	out.OnFailure = &onFailure
	if t.Budget != "" {
		out.Budget = &api.Money{Amount: t.Budget, Currency: t.Currency}
	}
	return out
}

func toAPIStep(s Step) api.Step {
	return api.Step{
		Id: s.ID, TaskId: s.TaskID, StepSeq: s.StepSeq, Kind: api.StepKind(s.Kind), Status: api.StepStatus(s.Status),
		ModelId: s.ModelID, Routing: object(s.Routing), ToolId: s.ToolID, ToolName: s.ToolName, DurationMs: s.DurationMs,
		Request: object(s.Request), Response: object(s.Response), Error: object(s.Error), CreatedAt: s.CreatedAt,
	}
}

var validStatus = map[string]bool{"queued": true, "running": true, "waiting_approval": true, "paused": true,
	"completed": true, "failed": true, "cancelled": true, "budget_exceeded": true, "timed_out": true}

var validKind = map[string]bool{"llm_call": true, "tool_call": true, "approval_wait": true, "delegation": true, "summarization": true}

func badParam(msg string) error { return apperr.New(apperr.KindInvalid, "invalid_parameter", msg) }

func (h *handlers) create(w http.ResponseWriter, r *http.Request) error {
	projectID, err := h.project(r, permExecCreate)
	if err != nil {
		return err
	}
	// Tạo execution tốn tiền và không tự khử trùng: bắt buộc có Idempotency-Key.
	if r.Header.Get(idempotencyHeader) == "" {
		return apperr.New(apperr.KindInvalid, "idempotency_key_required", "tạo execution cần header Idempotency-Key")
	}
	var body api.ExecutionCreate
	if err := httpx.DecodeJSON(r, &body); err != nil {
		return err
	}
	switch {
	case body.CallbackUrl != nil:
		return invalid("callback_url: chưa hỗ trợ ở phase này")
	case body.SessionId != nil:
		return invalid("session_id: chưa hỗ trợ ở phase này")
	case body.Tool != nil:
		return invalid("tool: mode direct_tool chưa hỗ trợ ở phase này")
	case body.Input.Context != nil:
		return invalid("input.context: chưa hỗ trợ ở phase này")
	}
	in := CreateInput{
		Mode: string(body.Mode), AgentID: body.AgentId, AgentVersion: body.AgentVersion,
		Message: body.Input.Message, TimeoutSeconds: body.TimeoutSeconds,
		MayRunAnyVersion: h.authz.Require(r.Context(), permAgentPublish, &projectID) == nil,
	}
	if body.Budget != nil {
		if body.Budget.Currency != "USD" {
			return invalid("budget.currency: phase này chỉ hỗ trợ USD")
		}
		in.Budget = &body.Budget.Amount
	}
	if body.Plan != nil {
		plan := orchestrator.Plan{Tasks: make([]orchestrator.Task, len(body.Plan.Tasks))}
		if body.Plan.MaxParallelTasks != nil {
			if *body.Plan.MaxParallelTasks < 1 {
				return invalid("plan.max_parallel_tasks phải từ 1 trở lên")
			}
			plan.MaxParallel = *body.Plan.MaxParallelTasks
		}
		for i, t := range body.Plan.Tasks {
			task := orchestrator.Task{Key: t.Id, AgentID: t.AgentId, AgentVersion: t.AgentVersion, Goal: t.Goal}
			if t.DependsOn != nil {
				task.DependsOn = *t.DependsOn
			}
			if t.OnFailure != nil {
				task.OnFailure = string(*t.OnFailure)
			}
			if t.Budget != nil {
				if t.Budget.Currency != "USD" {
					return invalid("plan.tasks[" + strconv.Itoa(i) + "].budget.currency: phase này chỉ hỗ trợ USD")
				}
				if task.Budget = t.Budget.Amount; task.Budget == "" {
					return invalid("plan.tasks[" + strconv.Itoa(i) + "].budget.amount là bắt buộc")
				}
			}
			plan.Tasks[i] = task
		}
		in.Plan = &plan
	}
	e, err := h.svc.Create(r.Context(), projectID, in)
	if err != nil {
		return err
	}
	w.Header().Set("Location", "/v1/executions/"+e.ID.String())
	return h.writeExecution(w, r, http.StatusAccepted, e)
}

func (h *handlers) get(w http.ResponseWriter, r *http.Request) error {
	e, err := h.load(r)
	if err != nil {
		return err
	}
	full, err := h.canReadContent(r.Context(), e.ProjectID)
	if err != nil {
		return err
	}
	out := toAPIExecution(e)
	if !full {
		out = redactExecution(out)
	}
	httpx.WriteJSON(w, http.StatusOK, out)
	return nil
}

func (h *handlers) cancel(w http.ResponseWriter, r *http.Request) error {
	e, err := h.load(r, permExecCancel)
	if err != nil {
		return err
	}
	if e, err = h.svc.Cancel(r.Context(), e.ID); err != nil {
		return err
	}
	return h.writeExecution(w, r, http.StatusAccepted, e)
}

// proposePlan chạy Agent lập kế hoạch của project; kết quả là một đề xuất, không task
// nào của kế hoạch được tạo.
func (h *handlers) proposePlan(w http.ResponseWriter, r *http.Request) error {
	projectID, err := h.project(r, permExecCreate)
	if err != nil {
		return err
	}
	// Gọi model tốn tiền và không tự khử trùng: bắt buộc có Idempotency-Key.
	if r.Header.Get(idempotencyHeader) == "" {
		return apperr.New(apperr.KindInvalid, "idempotency_key_required", "đề xuất kế hoạch cần header Idempotency-Key")
	}
	var body api.PlanProposalCreate
	if err := httpx.DecodeJSON(r, &body); err != nil {
		return err
	}
	in := ProposalInput{Message: body.Input.Message}
	if body.AgentIds != nil {
		if len(*body.AgentIds) > maxPlannerAgents {
			return invalid("agent_ids có tối đa " + strconv.Itoa(maxPlannerAgents) + " agent")
		}
		in.AgentIDs = *body.AgentIds
	}
	e, err := h.svc.ProposePlan(r.Context(), projectID, in)
	if err != nil {
		return err
	}
	w.Header().Set("Location", "/v1/executions/"+e.ID.String())
	return h.writeExecution(w, r, http.StatusAccepted, e)
}

// runProposal chạy kế hoạch mà một execution lập kế hoạch đã đề xuất, ngay ở phía api:
// platform ghi nhận kế hoạch do model viết và áp mức sàn tương ứng.
func (h *handlers) runProposal(w http.ResponseWriter, r *http.Request) error {
	e, err := h.load(r, permExecCreate)
	if err != nil {
		return err
	}
	if r.Header.Get(idempotencyHeader) == "" {
		return apperr.New(apperr.KindInvalid, "idempotency_key_required", "chạy kế hoạch cần header Idempotency-Key")
	}
	var body api.PlanProposalRun
	if r.ContentLength != 0 {
		if err := httpx.DecodeJSON(r, &body); err != nil {
			return err
		}
	}
	opts := RunOptions{TimeoutSeconds: body.TimeoutSeconds,
		MayRunAnyVersion: h.authz.Require(r.Context(), permAgentPublish, &e.ProjectID) == nil}
	if body.MaxParallelTasks != nil {
		if *body.MaxParallelTasks < 1 {
			return invalid("max_parallel_tasks phải từ 1 trở lên")
		}
		opts.MaxParallel = *body.MaxParallelTasks
	}
	if body.Budget != nil {
		if body.Budget.Currency != "USD" {
			return invalid("budget.currency: phase này chỉ hỗ trợ USD")
		}
		opts.Budget = &body.Budget.Amount
	}
	out, err := h.svc.RunProposal(r.Context(), e, opts)
	if err != nil {
		return err
	}
	w.Header().Set("Location", "/v1/executions/"+out.ID.String())
	return h.writeExecution(w, r, http.StatusAccepted, out)
}

// planProposal trả về kế hoạch mà một execution lập kế hoạch đã đề xuất, đã kiểm bằng code.
func (h *handlers) planProposal(w http.ResponseWriter, r *http.Request) error {
	e, err := h.load(r)
	if err != nil {
		return err
	}
	p, err := h.svc.PlanProposal(r.Context(), e)
	if err != nil {
		return err
	}
	out := api.PlanProposal{Status: api.PlanProposalStatus(p.Status), Problems: p.Problems}
	if out.Problems == nil {
		out.Problems = []string{}
	}
	if p.Status == "ready" {
		plan := api.ExecutionPlan{Tasks: make([]api.ExecutionPlanTask, len(p.Plan.Tasks))}
		for i, t := range p.Plan.Tasks {
			deps := t.DependsOn
			if deps == nil {
				deps = []string{}
			}
			task := api.ExecutionPlanTask{Id: t.Key, AgentId: t.AgentID, Goal: t.Goal, DependsOn: &deps}
			if t.OnFailure != "" {
				f := api.ExecutionPlanTaskOnFailure(t.OnFailure)
				task.OnFailure = &f
			}
			plan.Tasks[i] = task
		}
		out.Plan = &plan
	}
	httpx.WriteJSON(w, http.StatusOK, out)
	return nil
}

// pause và resume dùng quyền execution:cancel: cùng là quyền điều khiển một execution
// đang chạy. Cả hai idempotent.
func (h *handlers) pause(w http.ResponseWriter, r *http.Request) error {
	e, err := h.load(r, permExecCancel)
	if err != nil {
		return err
	}
	if e, err = h.svc.Pause(r.Context(), e.ID); err != nil {
		return err
	}
	return h.writeExecution(w, r, http.StatusAccepted, e)
}

// resume: người đã yêu cầu tạm dừng tự gỡ được yêu cầu của mình. Gỡ yêu cầu của người
// khác là cho chạy lại thứ mà người đó đã chặn (ví dụ quản trị viên tạm dừng để điều
// tra): cần thêm quyền execution:resume.
func (h *handlers) resume(w http.ResponseWriter, r *http.Request) error {
	e, err := h.load(r, permExecCancel)
	if err != nil {
		return err
	}
	if p, ok := identity.PrincipalFrom(r.Context()); e.PausedBy != nil && (!ok || e.PausedByType == nil || *e.PausedByType != p.Type || *e.PausedBy != p.ID) {
		if err := h.authz.Require(r.Context(), permExecResume, &e.ProjectID); err != nil {
			return err
		}
	}
	if e, err = h.svc.Resume(r.Context(), e.ID); err != nil {
		return err
	}
	return h.writeExecution(w, r, http.StatusAccepted, e)
}

func (h *handlers) list(w http.ResponseWriter, r *http.Request) error {
	projectID, err := h.project(r, permExecRead)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	limit, err := pagination.Limit(q.Get("limit"))
	if err != nil {
		return err
	}
	status := q.Get("status")
	if status != "" && !validStatus[status] {
		return badParam("status không hợp lệ")
	}
	items, next, err := h.svc.List(r.Context(), projectID, status, limit, q.Get("cursor"))
	if err != nil {
		return err
	}
	full, err := h.canReadContent(r.Context(), projectID)
	if err != nil {
		return err
	}
	data := make([]api.Execution, len(items))
	for i, e := range items {
		data[i] = toAPIExecution(e)
		if !full {
			data[i] = redactExecution(data[i])
		}
	}
	writePage(w, data, next)
	return nil
}

func writePage(w http.ResponseWriter, data any, next string) {
	page := map[string]any{"data": data, "next_cursor": nil}
	if next != "" {
		page["next_cursor"] = next
	}
	httpx.WriteJSON(w, http.StatusOK, page)
}

func (h *handlers) tasks(w http.ResponseWriter, r *http.Request) error {
	e, err := h.load(r)
	if err != nil {
		return err
	}
	items, err := h.svc.Tasks(r.Context(), e.ID)
	if err != nil {
		return err
	}
	full, err := h.canReadContent(r.Context(), e.ProjectID)
	if err != nil {
		return err
	}
	data := make([]api.Task, len(items))
	for i, t := range items {
		data[i] = toAPITask(t)
		if !full {
			data[i] = redactTask(data[i])
		}
	}
	writePage(w, data, "")
	return nil
}

func (h *handlers) task(w http.ResponseWriter, r *http.Request) error {
	t, err := h.loadTask(r)
	if err != nil {
		return err
	}
	full, err := h.canReadContent(r.Context(), t.ProjectID)
	if err != nil {
		return err
	}
	out := toAPITask(t)
	if !full {
		out = redactTask(out)
	}
	httpx.WriteJSON(w, http.StatusOK, out)
	return nil
}

func (h *handlers) steps(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	include := q.Get("include_content") == "true"
	if v := q.Get("include_content"); v != "" && v != "true" && v != "false" {
		return badParam("include_content phải là true hoặc false")
	}
	perms := []string{}
	if include {
		// Nội dung prompt và kết quả tool cần quyền riêng.
		perms = append(perms, permExecReadFull)
	}
	t, err := h.loadTask(r, perms...)
	if err != nil {
		return err
	}
	limit, err := pagination.Limit(q.Get("limit"))
	if err != nil {
		return err
	}
	kind := q.Get("kind")
	if kind != "" && !validKind[kind] {
		return badParam("kind không hợp lệ")
	}
	after := 0
	if c := q.Get("cursor"); c != "" {
		if after, err = strconv.Atoi(c); err != nil || after < 1 || after > 1<<30 {
			return pagination.ErrInvalidCursor
		}
	}
	items, next, err := h.svc.Steps(r.Context(), t, kind, include, limit, after)
	if err != nil {
		return err
	}
	data := make([]api.Step, len(items))
	for i, s := range items {
		data[i] = toAPIStep(s)
	}
	cursor := ""
	if next > 0 {
		cursor = strconv.Itoa(next)
	}
	writePage(w, data, cursor)
	return nil
}
