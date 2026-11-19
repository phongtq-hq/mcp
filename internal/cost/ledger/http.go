package ledger

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"
	pgxv5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hqgroups/mcp/internal/api"
	"github.com/hqgroups/mcp/internal/cost/ledger/store"
	"github.com/hqgroups/mcp/internal/platform/tenancy"
	apperr "github.com/hqgroups/mcp/internal/shared/errors"
	"github.com/hqgroups/mcp/internal/shared/httpx"
	"github.com/hqgroups/mcp/internal/shared/pagination"
)

// Authorizer là thứ handler cần từ Authorization & Policy.
type Authorizer interface {
	Require(ctx context.Context, permission string, projectID *uuid.UUID) error
}

const (
	permUsageRead = "usage:read"
	sortOccurred  = "-occurred_at"
	maxWindow     = 92 * 24 * time.Hour
)

func badParam(msg string) error { return apperr.New(apperr.KindInvalid, "invalid_parameter", msg) }

// RegisterRoutes gắn các endpoint đọc usage (bản ghi thô, tổng hợp, chuỗi thời gian; tiền tố
// /v1), sau middleware xác thực.
func RegisterRoutes(mux *http.ServeMux, db *pgxpool.Pool, authz Authorizer, projects Projects) {
	mux.HandleFunc("GET /v1/usage/summary", func(w http.ResponseWriter, r *http.Request) {
		if err := summary(w, r, db, authz, projects); err != nil {
			httpx.WriteError(w, r, err)
		}
	})
	mux.HandleFunc("GET /v1/usage/timeseries", func(w http.ResponseWriter, r *http.Request) {
		if err := timeseries(w, r, db, authz, projects, time.Now); err != nil {
			httpx.WriteError(w, r, err)
		}
	})
	mux.HandleFunc("GET /v1/usage/records", func(w http.ResponseWriter, r *http.Request) {
		if err := list(w, r, db, authz); err != nil {
			httpx.WriteError(w, r, err)
		}
	})
}

func list(w http.ResponseWriter, r *http.Request, db *pgxpool.Pool, authz Authorizer) error {
	ctx := r.Context()
	// Usage của cả organization: quyền cấp org.
	if err := authz.Require(ctx, permUsageRead, nil); err != nil {
		return err
	}
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return apperr.New(apperr.KindInternal, "internal_error", "")
	}
	q := r.URL.Query()
	limit, err := pagination.Limit(q.Get("limit"))
	if err != nil {
		return err
	}
	from, err1 := time.Parse(time.RFC3339, q.Get("from"))
	to, err2 := time.Parse(time.RFC3339, q.Get("to"))
	if err1 != nil || err2 != nil {
		return badParam("from và to là bắt buộc, dạng RFC 3339")
	}
	if !from.Before(to) || to.Sub(from) > maxWindow {
		return badParam("khoảng thời gian phải dương và không quá 92 ngày")
	}
	p := store.ListUsageParams{OrgID: orgID, FromTime: from.UTC(), ToTime: to.UTC(), RowLimit: int32(limit + 1)} //nolint:gosec // limit ≤ 200
	for key, dst := range map[string]**uuid.UUID{"execution_id": &p.ExecutionID, "agent_id": &p.AgentID, "model_id": &p.ModelID} {
		if v := q.Get(key); v != "" {
			id, err := uuid.Parse(v)
			if err != nil {
				return badParam(key + " phải là UUID")
			}
			*dst = &id
		}
	}
	c, err := pagination.Decode(q.Get("cursor"), sortOccurred)
	if err != nil {
		return err
	}
	if c != nil {
		at, err := time.Parse(time.RFC3339Nano, c.Value)
		if err != nil {
			return pagination.ErrInvalidCursor
		}
		p.AfterTime, p.AfterID = &at, &c.ID
	}
	var rows []store.ListUsageRow
	if err := tenancy.InTx(ctx, db, func(tx pgxv5.Tx) (err error) {
		rows, err = store.New(tx).ListUsage(ctx, p)
		return err
	}); err != nil {
		return apperr.Wrap(err, apperr.KindInternal, "internal_error", "")
	}
	page := map[string]any{"next_cursor": nil}
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[limit-1]
		page["next_cursor"] = pagination.Encode(pagination.Cursor{Sort: sortOccurred, Value: last.OccurredAt.UTC().Format(time.RFC3339Nano), ID: last.ID})
	}
	data := make([]api.UsageRecord, len(rows))
	for i, row := range rows {
		in, out, cached, write, reason := int(row.InputTokens), int(row.OutputTokens), int(row.CachedInputTokens), int(row.CacheWriteTokens), int(row.ReasoningTokens)
		projectID := row.ProjectID
		u := api.UsageRecord{Id: row.ID, Kind: api.UsageRecordKind(row.Kind), Purpose: row.Purpose, ProjectId: &projectID, UserId: row.UserID,
			AgentId: row.AgentID, ExecutionId: row.ExecutionID, TaskId: row.TaskID, WorkflowId: row.WorkflowID, ProviderId: row.ProviderID,
			ModelId: row.ModelID, ToolId: row.ToolID, InputTokens: &in, OutputTokens: &out, CachedInputTokens: &cached,
			CacheWriteTokens: &write, ReasoningTokens: &reason, PricingId: row.PricingID, OccurredAt: row.OccurredAt.UTC()}
		if row.EstimatedCost != "" {
			u.EstimatedCost = &api.Money{Amount: row.EstimatedCost, Currency: row.Currency}
		}
		data[i] = u
	}
	page["data"] = data
	httpx.WriteJSON(w, http.StatusOK, page)
	return nil
}
