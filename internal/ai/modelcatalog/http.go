package modelcatalog

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

// PricingStatus cho biết model nào đang có giá hiệu lực (từ context Cost).
type PricingStatus interface {
	ModelsWithCurrentPricing(ctx context.Context, modelIDs []uuid.UUID) (map[uuid.UUID]bool, error)
}

// Permission mà các endpoint ở đây yêu cầu. Giá trị phải có trong danh mục của authz.
const (
	permProviderRead   = "model_provider:read"
	permProviderCreate = "model_provider:create"
	permProviderUpdate = "model_provider:update"
	permProviderDelete = "model_provider:delete"
	// Đặt hoặc xoay khoá của provider là quyền riêng, tách khỏi việc sửa cấu hình.
	permProviderCredential = "model_provider:set_credential"
	permModelRead          = "model:read"
	permModelCreate        = "model:create"
	permModelUpdate        = "model:update"
)

type handlers struct {
	svc     *Service
	authz   Authorizer
	pricing PricingStatus
}

// RegisterRoutes gắn các endpoint Model Provider và Model (tiền tố /v1). Mọi route
// phải nằm sau middleware xác thực.
func RegisterRoutes(mux *http.ServeMux, svc *Service, authz Authorizer, pricing PricingStatus,
	idem func(authorize func(*http.Request) error, next http.HandlerFunc) http.HandlerFunc) {
	h := &handlers{svc: svc, authz: authz, pricing: pricing}
	require := func(perm string) func(*http.Request) error {
		return func(r *http.Request) error { return authz.Require(r.Context(), perm, nil) }
	}
	mux.HandleFunc("GET /v1/model-providers", wrap(h.listProviders))
	mux.HandleFunc("POST /v1/model-providers", idem(require(permProviderCreate), wrap(h.createProvider)))
	mux.HandleFunc("GET /v1/model-providers/{providerId}", wrap(h.getProvider))
	mux.HandleFunc("PATCH /v1/model-providers/{providerId}", wrap(h.updateProvider))
	mux.HandleFunc("DELETE /v1/model-providers/{providerId}", wrap(h.deleteProvider))
	mux.HandleFunc("GET /v1/models", wrap(h.listModels))
	mux.HandleFunc("POST /v1/models", idem(require(permModelCreate), wrap(h.createModel)))
	mux.HandleFunc("GET /v1/models/{modelId}", wrap(h.getModel))
	mux.HandleFunc("PATCH /v1/models/{modelId}", wrap(h.updateModel))
}

func wrap(fn func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := fn(w, r); err != nil {
			httpx.WriteError(w, r, err)
		}
	}
}

func rawObject(m *map[string]any) json.RawMessage {
	if m == nil {
		return nil
	}
	b, _ := json.Marshal(*m)
	return b
}

func toAPIProvider(p Provider) api.ModelProvider {
	scope := api.ModelProviderScope(p.Scope)
	out := api.ModelProvider{
		Id: p.ID, Slug: p.Slug, Adapter: p.Adapter, Enabled: p.Enabled,
		HasCredential: &p.HasCredential, Scope: &scope,
		CreatedAt: &p.CreatedAt, UpdatedAt: &p.UpdatedAt,
	}
	// Cấu hình vận hành của provider thuộc platform (endpoint nội bộ, tham số) không
	// trả cho tenant: họ chỉ cần biết provider tồn tại và dùng adapter nào.
	if p.Scope != ScopePlatform {
		settings := map[string]any{}
		_ = json.Unmarshal(p.Settings, &settings)
		out.Settings = &settings
		out.BaseUrl = p.BaseURL
	}
	return out
}

func toAPIModel(m Model, hasPricing bool) api.Model {
	out := api.Model{
		Id: m.ID, ProviderId: m.ProviderID, ProviderModel: m.ProviderModel, DisplayName: m.DisplayName,
		Enabled: m.Enabled, DeprecatedAt: m.DeprecatedAt, HasCurrentPricing: &hasPricing,
		CreatedAt: &m.CreatedAt, UpdatedAt: &m.UpdatedAt,
	}
	if m.Tier != nil {
		t := api.ModelTier(*m.Tier)
		out.Tier = &t
	}
	var caps api.Model_Capabilities
	if json.Unmarshal(m.Capabilities, &caps) == nil {
		out.Capabilities = &caps
	}
	return out
}

func parseQuery(r *http.Request) (url.Values, int, error) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, 0, apperr.New(apperr.KindInvalid, "invalid_parameter", "query string không hợp lệ")
	}
	limit, err := pagination.Limit(q.Get("limit"))
	return q, limit, err
}

// hideDenied biến "không có quyền đọc" thành "không tồn tại" cho tài nguyên theo id.
func hideDenied(err error) error {
	if apperr.KindOf(err) == apperr.KindPermissionDenied {
		return errNotFound
	}
	return err
}

func (h *handlers) hasPricing(ctx context.Context, ids ...uuid.UUID) (map[uuid.UUID]bool, error) {
	if h.pricing == nil {
		return map[uuid.UUID]bool{}, nil
	}
	return h.pricing.ModelsWithCurrentPricing(ctx, ids)
}

func (h *handlers) listProviders(w http.ResponseWriter, r *http.Request) error {
	if err := h.authz.Require(r.Context(), permProviderRead, nil); err != nil {
		return err
	}
	q, limit, err := parseQuery(r)
	if err != nil {
		return err
	}
	providers, next, err := h.svc.ListProviders(r.Context(), limit, q.Get("cursor"))
	if err != nil {
		return err
	}
	page := struct {
		Data       []api.ModelProvider `json:"data"`
		NextCursor *string             `json:"next_cursor"`
	}{Data: make([]api.ModelProvider, len(providers))}
	for i, p := range providers {
		page.Data[i] = toAPIProvider(p)
	}
	if next != "" {
		page.NextCursor = &next
	}
	httpx.WriteJSON(w, http.StatusOK, page)
	return nil
}

func (h *handlers) createProvider(w http.ResponseWriter, r *http.Request) error {
	if err := h.authz.Require(r.Context(), permProviderCreate, nil); err != nil {
		return err
	}
	var body api.ModelProviderCreate
	if err := httpx.DecodeJSON(r, &body); err != nil {
		return err
	}
	if body.ApiKey != nil {
		if err := h.authz.Require(r.Context(), permProviderCredential, nil); err != nil {
			return err
		}
	}
	p, err := h.svc.CreateProvider(r.Context(), ProviderCreate{
		Slug: body.Slug, Adapter: body.Adapter, BaseURL: body.BaseUrl, APIKey: body.ApiKey, Settings: rawObject(body.Settings)})
	if err != nil {
		return err
	}
	w.Header().Set("ETag", httpx.ETag(p.UpdatedAt))
	w.Header().Set("Location", "/v1/model-providers/"+p.ID.String())
	httpx.WriteJSON(w, http.StatusCreated, toAPIProvider(p))
	return nil
}

func (h *handlers) loadProvider(r *http.Request, permission string) (Provider, error) {
	id, err := uuid.Parse(r.PathValue("providerId"))
	if err != nil {
		return Provider{}, errNotFound
	}
	p, err := h.svc.GetProvider(r.Context(), id)
	if err != nil {
		return Provider{}, err
	}
	if err := h.authz.Require(r.Context(), permProviderRead, nil); err != nil {
		return Provider{}, hideDenied(err)
	}
	if permission != permProviderRead {
		if err := h.authz.Require(r.Context(), permission, nil); err != nil {
			return Provider{}, err
		}
	}
	return p, nil
}

func (h *handlers) getProvider(w http.ResponseWriter, r *http.Request) error {
	p, err := h.loadProvider(r, permProviderRead)
	if err != nil {
		return err
	}
	w.Header().Set("ETag", httpx.ETag(p.UpdatedAt))
	httpx.WriteJSON(w, http.StatusOK, toAPIProvider(p))
	return nil
}

func (h *handlers) updateProvider(w http.ResponseWriter, r *http.Request) error {
	p, err := h.loadProvider(r, permProviderUpdate)
	if err != nil {
		return err
	}
	var body api.ModelProviderUpdate
	if err := httpx.DecodeJSON(r, &body); err != nil {
		return err
	}
	ifMatch, err := httpx.IfMatch(r)
	if err != nil {
		return err
	}
	if body.ApiKey != nil {
		if err := h.authz.Require(r.Context(), permProviderCredential, nil); err != nil {
			return err
		}
	}
	p, err = h.svc.UpdateProvider(r.Context(), p.ID, ProviderUpdate{
		Enabled: body.Enabled, Settings: rawObject(body.Settings), BaseURL: body.BaseUrl, APIKey: body.ApiKey, IfUpdatedAt: ifMatch})
	if err != nil {
		return err
	}
	w.Header().Set("ETag", httpx.ETag(p.UpdatedAt))
	httpx.WriteJSON(w, http.StatusOK, toAPIProvider(p))
	return nil
}

func (h *handlers) deleteProvider(w http.ResponseWriter, r *http.Request) error {
	p, err := h.loadProvider(r, permProviderDelete)
	if err != nil {
		return err
	}
	if err := h.svc.DeleteProvider(r.Context(), p.ID); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *handlers) listModels(w http.ResponseWriter, r *http.Request) error {
	if err := h.authz.Require(r.Context(), permModelRead, nil); err != nil {
		return err
	}
	q, limit, err := parseQuery(r)
	if err != nil {
		return err
	}
	f := ModelFilter{Limit: limit, Cursor: q.Get("cursor"), Tier: q.Get("tier")}
	if v := q.Get("provider_id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			return apperr.New(apperr.KindInvalid, "invalid_parameter", "provider_id không hợp lệ")
		}
		f.ProviderID = &id
	}
	if v := q.Get("enabled"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return apperr.New(apperr.KindInvalid, "invalid_parameter", "enabled phải là true hoặc false")
		}
		f.Enabled = &b
	}
	models, next, err := h.svc.ListModels(r.Context(), f)
	if err != nil {
		return err
	}
	ids := make([]uuid.UUID, len(models))
	for i, m := range models {
		ids[i] = m.ID
	}
	priced, err := h.hasPricing(r.Context(), ids...)
	if err != nil {
		return err
	}
	page := struct {
		Data       []api.Model `json:"data"`
		NextCursor *string     `json:"next_cursor"`
	}{Data: make([]api.Model, len(models))}
	for i, m := range models {
		page.Data[i] = toAPIModel(m, priced[m.ID])
	}
	if next != "" {
		page.NextCursor = &next
	}
	httpx.WriteJSON(w, http.StatusOK, page)
	return nil
}

func (h *handlers) writeModel(w http.ResponseWriter, r *http.Request, status int, m Model) error {
	priced, err := h.hasPricing(r.Context(), m.ID)
	if err != nil {
		return err
	}
	w.Header().Set("ETag", httpx.ETag(m.UpdatedAt))
	httpx.WriteJSON(w, status, toAPIModel(m, priced[m.ID]))
	return nil
}

func (h *handlers) createModel(w http.ResponseWriter, r *http.Request) error {
	if err := h.authz.Require(r.Context(), permModelCreate, nil); err != nil {
		return err
	}
	var body api.ModelCreate
	if err := httpx.DecodeJSON(r, &body); err != nil {
		return err
	}
	in := ModelCreate{ProviderID: body.ProviderId, ProviderModel: body.ProviderModel, DisplayName: body.DisplayName,
		Capabilities: rawObject(body.Capabilities)}
	if body.Tier != nil {
		t := string(*body.Tier)
		in.Tier = &t
	}
	m, err := h.svc.CreateModel(r.Context(), in)
	if err != nil {
		return err
	}
	w.Header().Set("Location", "/v1/models/"+m.ID.String())
	return h.writeModel(w, r, http.StatusCreated, m)
}

func (h *handlers) loadModel(r *http.Request, permission string) (Model, error) {
	id, err := uuid.Parse(r.PathValue("modelId"))
	if err != nil {
		return Model{}, errNotFound
	}
	m, err := h.svc.GetModel(r.Context(), id)
	if err != nil {
		return Model{}, err
	}
	if err := h.authz.Require(r.Context(), permModelRead, nil); err != nil {
		return Model{}, hideDenied(err)
	}
	if permission != permModelRead {
		if err := h.authz.Require(r.Context(), permission, nil); err != nil {
			return Model{}, err
		}
	}
	return m, nil
}

func (h *handlers) getModel(w http.ResponseWriter, r *http.Request) error {
	m, err := h.loadModel(r, permModelRead)
	if err != nil {
		return err
	}
	return h.writeModel(w, r, http.StatusOK, m)
}

func (h *handlers) updateModel(w http.ResponseWriter, r *http.Request) error {
	m, err := h.loadModel(r, permModelUpdate)
	if err != nil {
		return err
	}
	var body api.ModelUpdate
	if err := httpx.DecodeJSON(r, &body); err != nil {
		return err
	}
	ifMatch, err := httpx.IfMatch(r)
	if err != nil {
		return err
	}
	u := ModelUpdate{DisplayName: body.DisplayName, Capabilities: rawObject(body.Capabilities),
		Enabled: body.Enabled, DeprecatedAt: body.DeprecatedAt, IfUpdatedAt: ifMatch}
	if body.Tier != nil {
		t := string(*body.Tier)
		u.Tier = &t
	}
	m, err = h.svc.UpdateModel(r.Context(), m.ID, u)
	if err != nil {
		return err
	}
	return h.writeModel(w, r, http.StatusOK, m)
}
