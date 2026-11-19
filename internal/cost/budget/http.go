package budget

import (
	"context"
	"encoding/json"
	"net/http"
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
	permRead    = "budget:read"
	permWrite   = "budget:write"
	sortCreated = "-created_at"
)

func badParam(msg string) error { return apperr.New(apperr.KindInvalid, "invalid_parameter", msg) }

type handlers struct {
	svc   *Service
	authz Authorizer
}

// RegisterRoutes gắn các endpoint budget (tiền tố /v1). Mọi route phải nằm sau
// middleware xác thực. Budget là cấu hình cấp organization: quyền được kiểm ở cấp org.
func RegisterRoutes(mux *http.ServeMux, svc *Service, authz Authorizer,
	idem func(authorize func(*http.Request) error, next http.HandlerFunc) http.HandlerFunc) {
	h := &handlers{svc: svc, authz: authz}
	write := func(r *http.Request) error { return authz.Require(r.Context(), permWrite, nil) }
	mux.HandleFunc("GET /v1/budgets", wrap(h.list))
	mux.HandleFunc("POST /v1/budgets", idem(write, wrap(h.create)))
	mux.HandleFunc("GET /v1/budgets/{budgetId}", wrap(h.get))
	mux.HandleFunc("PATCH /v1/budgets/{budgetId}", wrap(h.update))
	mux.HandleFunc("DELETE /v1/budgets/{budgetId}", wrap(h.delete))
}

func wrap(fn func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := fn(w, r); err != nil {
			httpx.WriteError(w, r, err)
		}
	}
}

func toAPI(b Budget) api.Budget {
	out := api.Budget{Id: b.ID, ScopeType: api.BudgetScope(b.ScopeType), ScopeId: b.ScopeID, Period: api.BudgetPeriod(b.Period),
		Limit: api.Money{Amount: b.Limit, Currency: b.Currency}, SoftThresholdPct: b.SoftThresholdPct,
		HardAction: api.BudgetHardAction(b.HardAction), Enabled: b.Enabled, CreatedAt: &b.CreatedAt, UpdatedAt: &b.UpdatedAt}
	spent := api.Money{Amount: b.Spent, Currency: b.Currency}
	start := b.PeriodStart
	cur := struct {
		End            *time.Time `json:"end,omitempty"`
		Reserved       *api.Money `json:"reserved,omitempty"`
		Spent          *api.Money `json:"spent,omitempty"`
		Start          *time.Time `json:"start,omitempty"`
		UtilizationPct *float32   `json:"utilization_pct,omitempty"`
	}{End: b.PeriodEnd, Spent: &spent, Start: &start}
	// Tỉ lệ sử dụng chỉ để hiển thị; mọi quyết định dùng số nguyên.
	if limit, ok := toUnits(b.Limit); ok && limit > 0 {
		if s, ok := toUnits(b.Spent); ok {
			pct := float32(float64(s) * 100 / float64(limit))
			cur.UtilizationPct = &pct
		}
	}
	out.CurrentPeriod = &cur
	return out
}

func (h *handlers) id(r *http.Request) (uuid.UUID, error) {
	id, err := uuid.Parse(r.PathValue("budgetId"))
	if err != nil {
		return uuid.Nil, errNotFound
	}
	return id, nil
}

func (h *handlers) list(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	if err := h.authz.Require(ctx, permRead, nil); err != nil {
		return err
	}
	q := r.URL.Query()
	limit, err := pagination.Limit(q.Get("limit"))
	if err != nil {
		return err
	}
	f := ListFilter{Limit: limit}
	if v := q.Get("scope_type"); v != "" {
		f.ScopeType = &v
	}
	if v := q.Get("scope_id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			return badParam("scope_id phải là UUID")
		}
		f.ScopeID = &id
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
	data := make([]api.Budget, len(rows))
	for i, b := range rows {
		data[i] = toAPI(b)
	}
	page["data"] = data
	httpx.WriteJSON(w, http.StatusOK, page)
	return nil
}

func (h *handlers) create(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	if err := h.authz.Require(ctx, permWrite, nil); err != nil {
		return err
	}
	var body api.BudgetCreate
	if err := httpx.DecodeJSON(r, &body); err != nil {
		return err
	}
	in := Create{ScopeType: string(body.ScopeType), ScopeID: body.ScopeId, Period: string(body.Period),
		Limit: body.Limit.Amount, Currency: body.Limit.Currency, SoftThresholdPct: body.SoftThresholdPct}
	if body.HardAction != nil {
		in.HardAction = string(*body.HardAction)
	}
	b, err := h.svc.Create(ctx, in)
	if err != nil {
		return err
	}
	w.Header().Set("Location", "/v1/budgets/"+b.ID.String())
	w.Header().Set("ETag", httpx.ETag(b.UpdatedAt))
	httpx.WriteJSON(w, http.StatusCreated, toAPI(b))
	return nil
}

func (h *handlers) get(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	if err := h.authz.Require(ctx, permRead, nil); err != nil {
		return err
	}
	id, err := h.id(r)
	if err != nil {
		return err
	}
	b, err := h.svc.Get(ctx, id)
	if err != nil {
		return err
	}
	w.Header().Set("ETag", httpx.ETag(b.UpdatedAt))
	httpx.WriteJSON(w, http.StatusOK, toAPI(b))
	return nil
}

func (h *handlers) update(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	if err := h.authz.Require(ctx, permWrite, nil); err != nil {
		return err
	}
	id, err := h.id(r)
	if err != nil {
		return err
	}
	// soft_threshold_pct: thiếu = giữ nguyên, null = bỏ ngưỡng. Kiểu sinh ra không phân
	// biệt được hai trường hợp này nên đọc qua map.
	var raw map[string]any
	var body api.BudgetUpdate
	if err := httpx.DecodeJSON(r, &raw); err != nil {
		return err
	}
	if err := remarshal(raw, &body); err != nil {
		return err
	}
	// If-Match (tuỳ chọn): chỉ sửa nếu budget chưa đổi kể từ ETag client đang giữ.
	ifMatch, err := httpx.IfMatch(r)
	if err != nil {
		return err
	}
	u := Update{Enabled: body.Enabled, SoftThresholdPct: body.SoftThresholdPct, IfUpdatedAt: ifMatch}
	_, u.SetSoft = raw["soft_threshold_pct"]
	if body.Limit != nil {
		u.Limit, u.Currency = &body.Limit.Amount, &body.Limit.Currency
	}
	if body.HardAction != nil {
		a := string(*body.HardAction)
		u.HardAction = &a
	}
	b, err := h.svc.Update(ctx, id, u)
	if err != nil {
		return err
	}
	w.Header().Set("ETag", httpx.ETag(b.UpdatedAt))
	httpx.WriteJSON(w, http.StatusOK, toAPI(b))
	return nil
}

func (h *handlers) delete(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	if err := h.authz.Require(ctx, permWrite, nil); err != nil {
		return err
	}
	id, err := h.id(r)
	if err != nil {
		return err
	}
	if err := h.svc.Delete(ctx, id); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// remarshal đọc lại một map đã giải mã vào kiểu có cấu trúc.
func remarshal(raw map[string]any, dst any) error {
	b, err := json.Marshal(raw)
	if err == nil {
		err = json.Unmarshal(b, dst)
	}
	if err != nil {
		return apperr.New(apperr.KindInvalid, "invalid_json", "thân request không đúng định dạng")
	}
	return nil
}
