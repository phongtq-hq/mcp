package pricing

import (
	"context"
	"net/http"
	"net/url"
	"time"

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

const (
	permModelRead     = "model:read"
	permPricingRead   = "model_pricing:read"
	permPricingCreate = "model_pricing:create"
)

var errNotFound = apperr.New(apperr.KindNotFound, "not_found", "tài nguyên không tồn tại")

type handlers struct {
	svc   *Service
	authz Authorizer
}

// RegisterRoutes gắn các endpoint bảng giá (tiền tố /v1). Mọi route phải nằm sau
// middleware xác thực.
func RegisterRoutes(mux *http.ServeMux, svc *Service, authz Authorizer,
	idem func(authorize func(*http.Request) error, next http.HandlerFunc) http.HandlerFunc) {
	h := &handlers{svc: svc, authz: authz}
	mux.HandleFunc("GET /v1/models/{modelId}/pricing", wrap(h.list))
	mux.HandleFunc("POST /v1/models/{modelId}/pricing", idem(func(r *http.Request) error {
		_, err := h.model(r, permPricingCreate)
		return err
	}, wrap(h.create)))
}

func wrap(fn func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := fn(w, r); err != nil {
			httpx.WriteError(w, r, err)
		}
	}
}

func toAPI(p Pricing) api.ModelPricing {
	scope := api.ModelPricingScope(p.Scope)
	return api.ModelPricing{
		Id: p.ID, ModelId: p.ModelID, Scope: &scope, Version: p.Version, Currency: p.Currency,
		InputPerMtok: p.InputPerMtok, OutputPerMtok: p.OutputPerMtok,
		CachedInputPerMtok: p.CachedInputPerMtok, CacheWritePerMtok: p.CacheWritePerMtok,
		ReasoningPerMtok: p.ReasoningPerMtok, EffectiveFrom: p.EffectiveFrom, EffectiveTo: p.EffectiveTo, Source: p.Source,
	}
}

// model xác nhận model trong đường dẫn tồn tại trong tầm nhìn của tenant và
// principal đọc được nó (nếu không: 404), rồi kiểm permission cho hành động.
func (h *handlers) model(r *http.Request, permission string) (uuid.UUID, error) {
	id, err := uuid.Parse(r.PathValue("modelId"))
	if err != nil {
		return uuid.Nil, errNotFound
	}
	if _, err := h.svc.models.ModelScope(r.Context(), id); err != nil {
		return uuid.Nil, err
	}
	if err := h.authz.Require(r.Context(), permModelRead, nil); err != nil {
		if apperr.KindOf(err) == apperr.KindPermissionDenied {
			return uuid.Nil, errNotFound
		}
		return uuid.Nil, err
	}
	if err := h.authz.Require(r.Context(), permission, nil); err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

func (h *handlers) list(w http.ResponseWriter, r *http.Request) error {
	modelID, err := h.model(r, permPricingRead)
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
	p := ListParams{Limit: limit, Cursor: q.Get("cursor")}
	if v := q.Get("effective_at"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return apperr.New(apperr.KindInvalid, "invalid_parameter", "effective_at phải là thời điểm RFC 3339")
		}
		p.EffectiveAt = &t
	}
	prices, next, err := h.svc.ListPricing(r.Context(), modelID, p)
	if err != nil {
		return err
	}
	page := struct {
		Data       []api.ModelPricing `json:"data"`
		NextCursor *string            `json:"next_cursor"`
	}{Data: make([]api.ModelPricing, len(prices))}
	for i, pr := range prices {
		page.Data[i] = toAPI(pr)
	}
	if next != "" {
		page.NextCursor = &next
	}
	httpx.WriteJSON(w, http.StatusOK, page)
	return nil
}

func (h *handlers) create(w http.ResponseWriter, r *http.Request) error {
	modelID, err := h.model(r, permPricingCreate)
	if err != nil {
		return err
	}
	var body api.ModelPricingCreate
	if err := httpx.DecodeJSON(r, &body); err != nil {
		return err
	}
	// Thiếu effective_from: để giá trị không (zero), service hiểu là "ngay bây giờ".
	var from time.Time
	if body.EffectiveFrom != nil {
		from = *body.EffectiveFrom
	}
	p, err := h.svc.CreatePricing(r.Context(), modelID, Create{
		Currency: body.Currency, InputPerMtok: body.InputPerMtok, OutputPerMtok: body.OutputPerMtok,
		CachedInputPerMtok: body.CachedInputPerMtok, CacheWritePerMtok: body.CacheWritePerMtok,
		ReasoningPerMtok: body.ReasoningPerMtok, EffectiveFrom: from, Source: body.Source})
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusCreated, toAPI(p))
	return nil
}
