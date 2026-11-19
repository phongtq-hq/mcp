package notify

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/hqgroups/mcp/internal/api"
	"github.com/hqgroups/mcp/internal/shared/httpx"
	"github.com/hqgroups/mcp/internal/shared/pagination"
)

const (
	permRead  = "notification_webhook:read"
	permWrite = "notification_webhook:write"
)

// Authorizer là thứ handler cần từ Authorization & Policy.
type Authorizer interface {
	Require(ctx context.Context, permission string, projectID *uuid.UUID) error
}

type handlers struct {
	svc   *Service
	authz Authorizer
}

// RegisterRoutes gắn các endpoint webhook thông báo (tiền tố /v1), sau middleware xác
// thực. PUT không được bọc Idempotency-Key: phản hồi chứa khoá ký và không được lưu
// lại để phát lại.
func RegisterRoutes(mux *http.ServeMux, svc *Service, authz Authorizer) {
	h := &handlers{svc: svc, authz: authz}
	mux.HandleFunc("GET /v1/notification-webhook", wrap(h.get))
	mux.HandleFunc("PUT /v1/notification-webhook", wrap(h.put))
	mux.HandleFunc("DELETE /v1/notification-webhook", wrap(h.delete))
	mux.HandleFunc("GET /v1/notification-webhook/deliveries", wrap(h.deliveries))
}

func wrap(fn func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := fn(w, r); err != nil {
			httpx.WriteError(w, r, err)
		}
	}
}

func toAPI(wh Webhook) api.NotificationWebhook {
	out := api.NotificationWebhook{Url: wh.URL, Enabled: wh.Enabled, ConsecutiveFailures: wh.ConsecutiveFailures,
		CreatedBy: wh.CreatedBy, CreatedAt: wh.CreatedAt, UpdatedAt: wh.UpdatedAt}
	if wh.DisabledReason != nil {
		r := api.NotificationWebhookDisabledReason(*wh.DisabledReason)
		out.DisabledReason = &r
	}
	return out
}

func (h *handlers) get(w http.ResponseWriter, r *http.Request) error {
	if err := h.authz.Require(r.Context(), permRead, nil); err != nil {
		return err
	}
	wh, err := h.svc.Get(r.Context())
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, toAPI(wh))
	return nil
}

func (h *handlers) put(w http.ResponseWriter, r *http.Request) error {
	if err := h.authz.Require(r.Context(), permWrite, nil); err != nil {
		return err
	}
	var body api.NotificationWebhookPut
	if err := httpx.DecodeJSON(r, &body); err != nil {
		return err
	}
	wh, secret, err := h.svc.Put(r.Context(), body.Url)
	if err != nil {
		return err
	}
	meta := toAPI(wh)
	// Phản hồi duy nhất chứa khoá ký: không cho proxy hay trình duyệt lưu lại.
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, api.NotificationWebhookCreated{Url: meta.Url, Enabled: meta.Enabled,
		ConsecutiveFailures: meta.ConsecutiveFailures, DisabledReason: (*api.NotificationWebhookCreatedDisabledReason)(wh.DisabledReason),
		CreatedBy: meta.CreatedBy, CreatedAt: meta.CreatedAt, UpdatedAt: meta.UpdatedAt, SigningSecret: &secret})
	return nil
}

func (h *handlers) delete(w http.ResponseWriter, r *http.Request) error {
	if err := h.authz.Require(r.Context(), permWrite, nil); err != nil {
		return err
	}
	if err := h.svc.Delete(r.Context()); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *handlers) deliveries(w http.ResponseWriter, r *http.Request) error {
	if err := h.authz.Require(r.Context(), permRead, nil); err != nil {
		return err
	}
	limit, err := pagination.Limit(r.URL.Query().Get("limit"))
	if err != nil {
		return err
	}
	rows, next, err := h.svc.ListDeliveries(r.Context(), limit, r.URL.Query().Get("cursor"))
	if err != nil {
		return err
	}
	data := make([]api.NotificationDelivery, len(rows))
	for i, d := range rows {
		item := api.NotificationDelivery{Id: d.ID, EventType: api.NotificationDeliveryEventType(d.EventType), ApprovalId: d.ApprovalID,
			Status: api.NotificationDeliveryStatus(d.Status), Attempts: d.Attempts, NextAttemptAt: d.NextAttemptAt,
			LastHttpStatus: d.LastHTTPStatus, CreatedAt: d.CreatedAt, FinishedAt: d.FinishedAt}
		if d.LastErrorCode != nil {
			c := api.NotificationDeliveryLastErrorCode(*d.LastErrorCode)
			item.LastErrorCode = &c
		}
		data[i] = item
	}
	page := map[string]any{"data": data, "next_cursor": nil}
	if next != "" {
		page["next_cursor"] = next
	}
	httpx.WriteJSON(w, http.StatusOK, page)
	return nil
}
