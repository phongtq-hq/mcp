package approval

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

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

const (
	permRead    = "approval:read"
	permDecide  = "approval:decide"
	sortCreated = "-created_at"
	idemHeader  = "Idempotency-Key"
)

func badParam(msg string) error { return apperr.New(apperr.KindInvalid, "invalid_parameter", msg) }

type handlers struct {
	svc   *Service
	authz Authorizer
}

// RegisterRoutes gắn các endpoint approval (tiền tố /v1). Mọi route phải nằm sau
// middleware xác thực. Quyền được kiểm theo project của approval.
func RegisterRoutes(mux *http.ServeMux, svc *Service, authz Authorizer,
	idem func(authorize func(*http.Request) error, next http.HandlerFunc) http.HandlerFunc) {
	h := &handlers{svc: svc, authz: authz}
	mux.HandleFunc("GET /v1/approvals", wrap(h.list))
	mux.HandleFunc("GET /v1/approvals/{approvalId}", wrap(h.get))
	mux.HandleFunc("POST /v1/approvals/{approvalId}/decisions", idem(func(r *http.Request) error {
		_, err := h.load(r, permDecide)
		return err
	}, wrap(h.decide)))
}

func wrap(fn func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := fn(w, r); err != nil {
			httpx.WriteError(w, r, err)
		}
	}
}

func toAPI(a Approval) api.Approval {
	out := api.Approval{Id: a.ID, ProjectId: a.ProjectID, ExecutionId: a.ExecutionID, TaskId: a.TaskID,
		SubjectType: api.ApprovalSubjectType(a.SubjectType), RiskLevel: api.RiskLevel(a.RiskLevel), Status: api.ApprovalStatus(a.Status),
		RequiredApprovals: a.Required, RequestedBy: a.RequestedBy, ExpiresAt: a.ExpiresAt, ResolvedAt: a.ResolvedAt, CreatedAt: a.CreatedAt}
	tool, server := a.Payload.ToolName, a.Payload.ServerName
	payload := struct {
		Actions        *[]map[string]interface{} `json:"actions,omitempty"`
		AgentRationale *string                   `json:"agent_rationale,omitempty"`
		Arguments      *map[string]interface{}   `json:"arguments,omitempty"`
		DryRunResult   *map[string]interface{}   `json:"dry_run_result,omitempty"`
		Message        *string                   `json:"message,omitempty"`
		PlanKey        *string                   `json:"plan_key,omitempty"`
		Redactions     *int                      `json:"redactions,omitempty"`
		ServerName     *string                   `json:"server_name,omitempty"`
		ToolName       *string                   `json:"tool_name,omitempty"`
	}{}
	if a.SubjectType == "workflow_step" {
		message, key := a.Payload.Message, a.Payload.PlanKey
		payload.Message, payload.PlanKey = &message, &key
	} else {
		payload.ToolName, payload.ServerName = &tool, &server
	}
	if n := a.Payload.Redactions; n > 0 {
		payload.Redactions = &n
	}
	var args map[string]interface{}
	if json.Unmarshal(a.Payload.Arguments, &args) == nil {
		payload.Arguments = &args
	}
	out.Payload = &payload
	decisions := make([]struct {
		Comment   *string                       `json:"comment,omitempty"`
		DecidedAt time.Time                     `json:"decided_at"`
		Decision  api.ApprovalDecisionsDecision `json:"decision"`
		UserId    uuid.UUID                     `json:"user_id"`
	}, len(a.Decisions))
	for i, d := range a.Decisions {
		decisions[i].UserId, decisions[i].DecidedAt = d.UserID, d.DecidedAt
		decisions[i].Decision = api.ApprovalDecisionsDecision(d.Decision)
		if d.Comment != "" {
			c := d.Comment
			decisions[i].Comment = &c
		}
	}
	out.Decisions = &decisions
	return out
}

// load đọc approval của tenant rồi kiểm quyền trên project của nó. Approval mà người gọi
// không đọc được (tenant khác, hoặc thiếu approval:read ở project đó) là 404: không để
// lộ sự tồn tại của nó. Quyền khác (approval:decide) thiếu thì 403.
func (h *handlers) load(r *http.Request, permission string) (Approval, error) {
	id, err := uuid.Parse(r.PathValue("approvalId"))
	if err != nil {
		return Approval{}, errNotFound
	}
	a, err := h.svc.Get(r.Context(), id)
	if err != nil {
		return Approval{}, err
	}
	if err := h.authz.Require(r.Context(), permRead, &a.ProjectID); err != nil {
		if apperr.KindOf(err) == apperr.KindPermissionDenied {
			return Approval{}, errNotFound
		}
		return Approval{}, err
	}
	if permission != permRead {
		if err := h.authz.Require(r.Context(), permission, &a.ProjectID); err != nil {
			return Approval{}, err
		}
	}
	return a, nil
}

func (h *handlers) get(w http.ResponseWriter, r *http.Request) error {
	a, err := h.load(r, permRead)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, toAPI(a))
	return nil
}

func (h *handlers) list(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	q := r.URL.Query()
	limit, err := pagination.Limit(q.Get("limit"))
	if err != nil {
		return err
	}
	f := ListFilter{Limit: limit}
	for key, dst := range map[string]**uuid.UUID{"project_id": &f.ProjectID, "execution_id": &f.ExecutionID} {
		if v := q.Get(key); v != "" {
			id, err := uuid.Parse(v)
			if err != nil {
				return badParam(key + " phải là UUID")
			}
			*dst = &id
		}
	}
	// Lọc theo project thì quyền ở project đó là đủ; không lọc thì cần quyền cấp organization.
	if err := h.authz.Require(ctx, permRead, f.ProjectID); err != nil {
		return err
	}
	if v := q.Get("status"); v != "" {
		if !api.ApprovalStatus(v).Valid() {
			return badParam("status không hợp lệ")
		}
		f.Status = &v
	}
	// assigned_to_me: approval đang chờ mà người gọi không phải người yêu cầu (người yêu
	// cầu không tự duyệt được). Quyền approval:decide theo từng project vẫn được kiểm khi quyết định.
	if q.Get("assigned_to_me") == "true" {
		pending := StatusPending
		f.Status = &pending
		if p, ok := identity.PrincipalFrom(ctx); ok {
			f.NotRequestedBy = &p.ID
		}
	}
	c, err := pagination.Decode(q.Get("cursor"), sortCreated)
	if err != nil {
		return err
	}
	if c != nil {
		at, err := time.Parse(time.RFC3339Nano, c.Value)
		if err != nil {
			return pagination.ErrInvalidCursor
		}
		f.AfterCreated, f.AfterID = &at, &c.ID
	}
	rows, err := h.svc.List(ctx, f)
	if err != nil {
		return err
	}
	page := map[string]any{"next_cursor": nil}
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[limit-1]
		page["next_cursor"] = pagination.Encode(pagination.Cursor{Sort: sortCreated, Value: last.CreatedAt.Format(time.RFC3339Nano), ID: last.ID})
	}
	data := make([]api.Approval, len(rows))
	for i, a := range rows {
		data[i] = toAPI(a)
	}
	page["data"] = data
	httpx.WriteJSON(w, http.StatusOK, page)
	return nil
}

func (h *handlers) decide(w http.ResponseWriter, r *http.Request) error {
	a, err := h.load(r, permDecide)
	if err != nil {
		return err
	}
	// Quyết định cho phép một hành động có tác động thật: bắt buộc có Idempotency-Key.
	if r.Header.Get(idemHeader) == "" {
		return apperr.New(apperr.KindInvalid, "idempotency_key_required", "quyết định approval cần header Idempotency-Key")
	}
	var body struct {
		Decision string  `json:"decision"`
		Comment  *string `json:"comment"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		return err
	}
	comment := ""
	if body.Comment != nil {
		comment = *body.Comment
	}
	out, err := h.svc.Decide(r.Context(), a.ID, body.Decision, comment)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, toAPI(out))
	return nil
}
