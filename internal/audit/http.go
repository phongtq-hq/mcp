package audit

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	pgxv5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hqgroups/mcp/internal/api"
	"github.com/hqgroups/mcp/internal/audit/store"
	apperr "github.com/hqgroups/mcp/internal/shared/errors"
	"github.com/hqgroups/mcp/internal/shared/httpx"
	"github.com/hqgroups/mcp/internal/shared/pagination"
	sharedpgx "github.com/hqgroups/mcp/internal/shared/pgx"
)

// Authorizer là thứ handler cần từ Authorization & Policy.
type Authorizer interface {
	Require(ctx context.Context, permission string, projectID *uuid.UUID) error
}

const (
	permAuditRead = "audit_log:read"
	sortOccurred  = "-occurred_at"
	// Không có khoảng thời gian thì mặc định 30 ngày gần nhất; khoảng tối đa 92 ngày,
	// để một truy vấn không quét toàn bộ lịch sử.
	defaultWindow = 30 * 24 * time.Hour
	maxWindow     = 92 * 24 * time.Hour
)

var actionPrefixRe = regexp.MustCompile(`^[a-z0-9_.]{1,64}$`)

// OrgFrom lấy tenant của request. Audit không import tenancy (tenancy ghi audit), nên
// hàm này được tiêm từ composition root.
type OrgFrom func(ctx context.Context) (uuid.UUID, bool)

type handlers struct {
	db    *pgxpool.Pool
	authz Authorizer
	org   OrgFrom
}

// RegisterRoutes gắn endpoint đọc audit trail. Chỉ đọc: không có endpoint ghi, sửa hay xoá.
func RegisterRoutes(mux *http.ServeMux, db *pgxpool.Pool, authz Authorizer, org OrgFrom) {
	h := &handlers{db: db, authz: authz, org: org}
	mux.HandleFunc("GET /v1/audit-logs", func(w http.ResponseWriter, r *http.Request) {
		if err := h.list(w, r); err != nil {
			httpx.WriteError(w, r, err)
		}
	})
}

func badParam(msg string) error { return apperr.New(apperr.KindInvalid, "invalid_parameter", msg) }

// TimeRange đọc from / to (RFC 3339). required = false thì mặc định là khoảng gần nhất.
func TimeRange(q map[string][]string, required bool, defaultWindow, maxWindow time.Duration) (from, to time.Time, err error) {
	get := func(k string) string {
		if v := q[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	to = time.Now().UTC()
	if v := get("to"); v != "" {
		if to, err = time.Parse(time.RFC3339, v); err != nil {
			return from, to, badParam("to phải là thời điểm RFC 3339")
		}
	} else if required {
		return from, to, badParam("from và to là bắt buộc")
	}
	from = to.Add(-defaultWindow)
	if v := get("from"); v != "" {
		if from, err = time.Parse(time.RFC3339, v); err != nil {
			return from, to, badParam("from phải là thời điểm RFC 3339")
		}
	} else if required {
		return from, to, badParam("from và to là bắt buộc")
	}
	if !from.Before(to) || to.Sub(from) > maxWindow {
		return from, to, badParam("khoảng thời gian phải dương và không quá 92 ngày")
	}
	return from.UTC(), to.UTC(), nil
}

func optUUID(q map[string][]string, key string) (*uuid.UUID, error) {
	v := q[key]
	if len(v) == 0 || v[0] == "" {
		return nil, nil
	}
	id, err := uuid.Parse(v[0])
	if err != nil {
		return nil, badParam(key + " phải là UUID")
	}
	return &id, nil
}

func (h *handlers) list(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	// Audit ghi hành vi của mọi thành viên: đọc nó là quyền cấp organization.
	if err := h.authz.Require(ctx, permAuditRead, nil); err != nil {
		return err
	}
	orgID, ok := h.org(ctx)
	if !ok {
		return apperr.New(apperr.KindInternal, "internal_error", "")
	}
	q := r.URL.Query()
	limit, err := pagination.Limit(q.Get("limit"))
	if err != nil {
		return err
	}
	from, to, err := TimeRange(q, false, defaultWindow, maxWindow)
	if err != nil {
		return err
	}
	p := store.ListAuditLogsParams{OrgID: orgID, FromTime: from, ToTime: to, RowLimit: int32(limit + 1)} //nolint:gosec // limit ≤ 200
	for key, dst := range map[string]**uuid.UUID{"actor_id": &p.ActorID, "resource_id": &p.ResourceID, "execution_id": &p.ExecutionID, "project_id": &p.ProjectID} {
		if *dst, err = optUUID(q, key); err != nil {
			return err
		}
	}
	if v := q.Get("actor_type"); v != "" {
		if v != ActorUser && v != ActorAPIKey && v != ActorAgent && v != ActorSystem {
			return badParam("actor_type không hợp lệ")
		}
		p.ActorType = &v
	}
	if v := q.Get("outcome"); v != "" {
		if v != OutcomeSuccess && v != OutcomeDenied && v != OutcomeError {
			return badParam("outcome không hợp lệ")
		}
		p.Outcome = &v
	}
	if v := q.Get("action"); v != "" {
		if !actionPrefixRe.MatchString(v) {
			return badParam("action chỉ gồm chữ thường, số, '_' và '.'")
		}
		// "_" là ký tự đại diện của LIKE: khớp nguyên văn.
		esc := strings.ReplaceAll(v, "_", `\_`)
		p.ActionPrefix = &esc
	}
	if v := q.Get("resource_type"); v != "" {
		if !actionPrefixRe.MatchString(v) {
			return badParam("resource_type không hợp lệ")
		}
		p.ResourceType = &v
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
	var rows []store.ListAuditLogsRow
	if err := sharedpgx.WithTenantTx(ctx, h.db, orgID, func(tx pgxv5.Tx) (err error) {
		rows, err = store.New(tx).ListAuditLogs(ctx, p)
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
	data := make([]api.AuditLog, len(rows))
	for i, row := range rows {
		details := map[string]any{}
		_ = json.Unmarshal(row.Details, &details)
		e := api.AuditLog{Id: row.ID, ProjectId: row.ProjectID, ActorType: api.AuditLogActorType(row.ActorType), ActorId: row.ActorID,
			OnBehalfOf: row.OnBehalfOf, Action: row.Action, ResourceType: row.ResourceType, ResourceId: row.ResourceID,
			Outcome: api.AuditLogOutcome(row.Outcome), ExecutionId: row.ExecutionID, Details: &details, TraceId: row.TraceID,
			OccurredAt: row.OccurredAt.UTC()}
		if row.Ip != "" {
			e.Ip = &row.Ip
		}
		data[i] = e
	}
	page["data"] = data
	httpx.WriteJSON(w, http.StatusOK, page)
	return nil
}
