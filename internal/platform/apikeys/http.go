package apikeys

import (
	"context"
	"net/http"

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

type handlers struct {
	svc   *Service
	authz Authorizer
}

// RegisterRoutes gắn các endpoint API key (tiền tố /v1), sau middleware xác thực.
// POST không được bọc Idempotency-Key: phản hồi chứa giá trị của khoá và không được
// lưu lại để phát lại.
func RegisterRoutes(mux *http.ServeMux, svc *Service, authz Authorizer) {
	h := &handlers{svc: svc, authz: authz}
	mux.HandleFunc("GET /v1/api-keys", wrap(h.list))
	mux.HandleFunc("POST /v1/api-keys", wrap(h.create))
	mux.HandleFunc("DELETE /v1/api-keys/{apiKeyId}", wrap(h.revoke))
}

func wrap(fn func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := fn(w, r); err != nil {
			httpx.WriteError(w, r, err)
		}
	}
}

func toAPI(k Key) api.ApiKey {
	return api.ApiKey{Id: k.ID, Name: k.Name, Prefix: Prefix + k.Prefix, Role: k.Role, ProjectId: k.ProjectID, CreatedBy: &k.CreatedBy,
		ExpiresAt: k.ExpiresAt, RevokedAt: k.RevokedAt, LastUsedAt: k.LastUsedAt, CreatedAt: k.CreatedAt}
}

func (h *handlers) create(w http.ResponseWriter, r *http.Request) error {
	var body api.ApiKeyCreate
	if err := httpx.DecodeJSON(r, &body); err != nil {
		return err
	}
	// Project phải tồn tại trong tenant trước khi nó được dùng làm phạm vi kiểm quyền:
	// nếu không, mỗi UUID bịa ra là một dòng audit từ chối mới.
	if body.ProjectId != nil {
		if _, err := h.svc.projects.GetProject(r.Context(), *body.ProjectId); err != nil {
			if apperr.KindOf(err) == apperr.KindNotFound {
				return invalid("project_id không tồn tại")
			}
			return err
		}
	}
	// Khoá phạm vi project cần quyền ở project đó; khoá phạm vi org cần quyền toàn org.
	if err := h.authz.Require(r.Context(), permKeyCreate, body.ProjectId); err != nil {
		return err
	}
	k, token, err := h.svc.Create(r.Context(), CreateInput{Name: body.Name, ProjectID: body.ProjectId, Role: body.Role, ExpiresInDays: body.ExpiresInDays})
	if err != nil {
		return err
	}
	meta := toAPI(k)
	// Phản hồi duy nhất chứa giá trị của khoá: không cho proxy hay trình duyệt lưu lại.
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusCreated, api.ApiKeyCreated{Id: meta.Id, Name: meta.Name, Prefix: meta.Prefix, Role: meta.Role,
		ProjectId: meta.ProjectId, CreatedBy: meta.CreatedBy, ExpiresAt: meta.ExpiresAt, CreatedAt: meta.CreatedAt, Key: &token})
	return nil
}

func (h *handlers) list(w http.ResponseWriter, r *http.Request) error {
	if err := h.authz.Require(r.Context(), permKeyRead, nil); err != nil {
		return err
	}
	limit, err := pagination.Limit(r.URL.Query().Get("limit"))
	if err != nil {
		return err
	}
	keys, next, err := h.svc.List(r.Context(), limit, r.URL.Query().Get("cursor"))
	if err != nil {
		return err
	}
	data := make([]api.ApiKey, len(keys))
	for i, k := range keys {
		data[i] = toAPI(k)
	}
	page := map[string]any{"data": data, "next_cursor": nil}
	if next != "" {
		page["next_cursor"] = next
	}
	httpx.WriteJSON(w, http.StatusOK, page)
	return nil
}

func (h *handlers) revoke(w http.ResponseWriter, r *http.Request) error {
	id, err := uuid.Parse(r.PathValue("apiKeyId"))
	if err != nil {
		return errNotFound
	}
	k, err := h.svc.Get(r.Context(), id)
	if err != nil {
		return err
	}
	// Không đọc được khoá ở phạm vi của nó thì khoá "không tồn tại".
	if err := h.authz.Require(r.Context(), permKeyRead, k.ProjectID); err != nil {
		if apperr.KindOf(err) == apperr.KindPermissionDenied {
			return errNotFound
		}
		return err
	}
	if err := h.authz.Require(r.Context(), permKeyRevoke, k.ProjectID); err != nil {
		return err
	}
	if err := h.svc.Revoke(r.Context(), id); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
