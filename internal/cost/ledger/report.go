package ledger

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	pgxv5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hqgroups/mcp/internal/cost/ledger/store"
	"github.com/hqgroups/mcp/internal/platform/tenancy"
	apperr "github.com/hqgroups/mcp/internal/shared/errors"
	"github.com/hqgroups/mcp/internal/shared/httpx"
)

// Báo cáo usage (cost-management §3): tổng hợp theo chiều và chuỗi thời gian. Mọi con số
// là ƯỚC TÍNH từ usage ledger theo giá hiệu lực lúc ghi; tiền được cộng bằng NUMERIC
// trong database và đi qua code ở dạng chuỗi thập phân, không qua float.

const (
	maxGroups      = 500
	maxPoints      = 5000
	maxMonthWindow = 400 * 24 * time.Hour
	usd            = "USD"
)

var (
	summaryDims = map[string]bool{"project": true, "user": true, "agent": true, "model": true, "provider": true,
		"workflow": true, "tool": true, "purpose": true}
	seriesDims = map[string]bool{"project": true, "agent": true, "model": true, "provider": true}
)

// Projects là thứ báo cáo cần từ Identity & Tenancy: project có tồn tại trong tenant không.
type Projects interface {
	// ProjectExists trả lỗi not_found khi project không có trong tenant của ctx.
	ProjectExists(ctx context.Context, id uuid.UUID) error
}

// Báo cáo là truy vấn tổng hợp nặng trên cả organization: mỗi process chạy tối đa
// maxConcurrentReports báo cáo cùng lúc (quá thì 429), và mỗi câu lệnh có hạn thời gian ở
// database, để vài request khoảng rộng không chiếm hết pool kết nối của mọi tenant.
const (
	maxConcurrentReports = 4
	reportStatementLimit = "15s"
)

var (
	reportSlots = make(chan struct{}, maxConcurrentReports)
	errBusy     = apperr.New(apperr.KindRateLimited, "rate_limited", "đang có quá nhiều báo cáo usage chạy; thử lại sau").AsRetryable()
)

func acquire() (release func(), err error) {
	select {
	case reportSlots <- struct{}{}:
		return func() { <-reportSlots }, nil
	default:
		return nil, errBusy
	}
}

// bound đặt hạn thời gian cho các câu lệnh của transaction báo cáo.
func bound(ctx context.Context, tx pgxv5.Tx) error {
	_, err := tx.Exec(ctx, "SET LOCAL statement_timeout = '"+reportStatementLimit+"'")
	return err
}

type money struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

type totals struct {
	Calls             int64  `json:"calls"`
	Executions        int64  `json:"executions"`
	InputTokens       int64  `json:"input_tokens"`
	OutputTokens      int64  `json:"output_tokens"`
	CachedInputTokens int64  `json:"cached_input_tokens"`
	UnpricedCalls     int64  `json:"unpriced_calls"`
	EstimatedCost     money  `json:"estimated_cost"`
	AvgCostPerExec    *money `json:"avg_cost_per_execution"`
}

func totalsOf(r store.SummarizeUsageRow) totals {
	t := totals{Calls: r.Calls, Executions: r.Executions, InputTokens: r.InputTokens, OutputTokens: r.OutputTokens,
		CachedInputTokens: r.CachedInputTokens, UnpricedCalls: r.UnpricedCalls, EstimatedCost: money{r.EstimatedCost, usd}}
	if r.AvgCostPerExecution != "" {
		t.AvgCostPerExec = &money{r.AvgCostPerExecution, usd}
	}
	return t
}

// window đọc from / to (RFC 3339) và kiểm khoảng thời gian.
func window(r *http.Request, maxSpan time.Duration, what string) (from, to time.Time, err error) {
	q := r.URL.Query()
	from, err1 := time.Parse(time.RFC3339, q.Get("from"))
	to, err2 := time.Parse(time.RFC3339, q.Get("to"))
	if err1 != nil || err2 != nil {
		return from, to, badParam("from và to là bắt buộc, dạng RFC 3339")
	}
	if !from.Before(to) || to.Sub(from) > maxSpan {
		return from, to, badParam("khoảng thời gian phải dương và không quá " + what)
	}
	return from.UTC(), to.UTC(), nil
}

func optionalID(r *http.Request, key string) (*uuid.UUID, error) {
	v := r.URL.Query().Get(key)
	if v == "" {
		return nil, nil //nolint:nilnil // không có bộ lọc
	}
	id, err := uuid.Parse(v)
	if err != nil {
		return nil, badParam(key + " phải là UUID")
	}
	return &id, nil
}

// authorize: báo cáo của cả organization cần usage:read cấp org; báo cáo đã lọc theo một
// project thì quyền usage:read trên project đó là đủ, trừ khi báo cáo tách hay lọc theo
// người dùng (orgWide): chi tiêu của từng người là dữ liệu cấp organization.
//
// Project được đọc trong tenant TRƯỚC khi kiểm quyền (không có thì 404): một project_id
// bịa ra không đi vào quyết định phân quyền, nên không sinh dòng audit từ chối cho từng
// UUID ngẫu nhiên.
func authorize(r *http.Request, authz Authorizer, projects Projects, projectID *uuid.UUID, orgWide bool) (uuid.UUID, error) {
	if projectID != nil {
		if err := projects.ProjectExists(r.Context(), *projectID); err != nil {
			return uuid.Nil, err
		}
	}
	scope := projectID
	if orgWide {
		scope = nil
	}
	if err := authz.Require(r.Context(), permUsageRead, scope); err != nil {
		return uuid.Nil, err
	}
	orgID, ok := tenancy.OrgFrom(r.Context())
	if !ok {
		return uuid.Nil, apperr.New(apperr.KindInternal, "internal_error", "")
	}
	return orgID, nil
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func summary(w http.ResponseWriter, r *http.Request, db *pgxpool.Pool, authz Authorizer, projects Projects) error {
	ctx := r.Context()
	projectID, err := optionalID(r, "project_id")
	if err != nil {
		return err
	}
	var dims []string
	if v := r.URL.Query().Get("group_by"); v != "" {
		dims = strings.Split(v, ",")
	}
	if len(dims) > 2 || (len(dims) == 2 && dims[0] == dims[1]) {
		return badParam("group_by nhận tối đa 2 chiều khác nhau")
	}
	for _, d := range dims {
		if !summaryDims[d] {
			return badParam("group_by không hợp lệ: " + d)
		}
	}
	byUser := slices.Contains(dims, "user") || r.URL.Query().Get("user_id") != ""
	orgID, err := authorize(r, authz, projects, projectID, byUser)
	if err != nil {
		return err
	}
	from, to, err := window(r, maxWindow, "92 ngày")
	if err != nil {
		return err
	}
	release, err := acquire()
	if err != nil {
		return err
	}
	defer release()
	p := store.SummarizeUsageParams{OrgID: orgID, FromTime: from, ToTime: to, ProjectID: projectID, RowLimit: 1}
	for key, dst := range map[string]**uuid.UUID{"agent_id": &p.AgentID, "model_id": &p.ModelID, "user_id": &p.UserID} {
		if *dst, err = optionalID(r, key); err != nil {
			return err
		}
	}
	var total []store.SummarizeUsageRow
	var groups []store.SummarizeUsageRow
	err = tenancy.InTx(ctx, db, func(tx pgxv5.Tx) error {
		if err := bound(ctx, tx); err != nil {
			return err
		}
		q := store.New(tx)
		var err error
		if total, err = q.SummarizeUsage(ctx, p); err != nil || len(dims) == 0 {
			return err
		}
		g := p
		g.Dim1, g.RowLimit = dims[0], maxGroups+1
		if len(dims) == 2 {
			g.Dim2 = dims[1]
		}
		groups, err = q.SummarizeUsage(ctx, g)
		return err
	})
	if err != nil {
		return apperr.Wrap(err, apperr.KindInternal, "internal_error", "")
	}
	type group struct {
		totals
		Key map[string]*string `json:"key"`
	}
	out := struct {
		From      time.Time `json:"from"`
		To        time.Time `json:"to"`
		CostBasis string    `json:"cost_basis"`
		Totals    totals    `json:"totals"`
		Truncated bool      `json:"truncated"`
		Groups    []group   `json:"groups"`
	}{From: from, To: to, CostBasis: "estimated", Totals: totals{EstimatedCost: money{"0.00000000", usd}}, Groups: []group{}}
	if len(total) == 1 {
		out.Totals = totalsOf(total[0])
	}
	if len(groups) > maxGroups {
		groups, out.Truncated = groups[:maxGroups], true
	}
	for _, g := range groups {
		key := map[string]*string{dims[0]: nullable(g.Key1)}
		if len(dims) == 2 {
			key[dims[1]] = nullable(g.Key2)
		}
		out.Groups = append(out.Groups, group{totals: totalsOf(g), Key: key})
	}
	httpx.WriteJSON(w, http.StatusOK, out)
	return nil
}

func timeseries(w http.ResponseWriter, r *http.Request, db *pgxpool.Pool, authz Authorizer, projects Projects, now func() time.Time) error {
	ctx := r.Context()
	projectID, err := optionalID(r, "project_id")
	if err != nil {
		return err
	}
	orgID, err := authorize(r, authz, projects, projectID, false)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	interval := q.Get("interval")
	if interval == "" {
		interval = "day"
	}
	span, what := maxWindow, "92 ngày với interval=day"
	switch interval {
	case "day":
	case "month":
		span, what = maxMonthWindow, "400 ngày với interval=month"
	default:
		return badParam("interval phải là day hoặc month")
	}
	from, to, err := window(r, span, what)
	if err != nil {
		return err
	}
	dim := q.Get("group_by")
	if dim != "" && !seriesDims[dim] {
		return badParam("group_by không hợp lệ: " + dim)
	}
	agentID, err := optionalID(r, "agent_id")
	if err != nil {
		return err
	}
	forecast := false
	switch q.Get("include_forecast") {
	case "", "false":
	case "true":
		forecast = true
	default:
		return badParam("include_forecast phải là true hoặc false")
	}
	release, err := acquire()
	if err != nil {
		return err
	}
	defer release()
	var rows []store.UsageTimeseriesRow
	var fc store.ForecastMonthRow
	at := now().UTC()
	monthStart := time.Date(at.Year(), at.Month(), 1, 0, 0, 0, 0, time.UTC)
	monthEnd := monthStart.AddDate(0, 1, 0)
	err = tenancy.InTx(ctx, db, func(tx pgxv5.Tx) error {
		if err := bound(ctx, tx); err != nil {
			return err
		}
		qs := store.New(tx)
		var err error
		rows, err = qs.UsageTimeseries(ctx, store.UsageTimeseriesParams{OrgID: orgID, Bucket: interval, Dim: dim, FromTime: from, ToTime: to,
			ProjectID: projectID, AgentID: agentID, RowLimit: maxPoints + 1})
		if err != nil || !forecast {
			return err
		}
		fc, err = qs.ForecastMonth(ctx, store.ForecastMonthParams{OrgID: orgID, MonthStart: monthStart, NowTime: at,
			// Ngày trọn cuối cùng của tháng là hôm qua (UTC); ngày đầu tháng thì chưa có ngày trọn nào.
			LastFullDay: time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -1), MonthEnd: monthEnd,
			ProjectID: projectID, AgentID: agentID})
		return err
	})
	if err != nil {
		return apperr.Wrap(err, apperr.KindInternal, "internal_error", "")
	}
	if len(rows) > maxPoints {
		return apperr.New(apperr.KindUnprocessable, "too_many_points", "kết quả vượt 5000 điểm; thu hẹp khoảng thời gian hoặc bỏ group_by")
	}
	type point struct {
		PeriodStart   time.Time `json:"period_start"`
		InputTokens   int64     `json:"input_tokens"`
		OutputTokens  int64     `json:"output_tokens"`
		EstimatedCost money     `json:"estimated_cost"`
	}
	type series struct {
		Key    *string `json:"key"`
		Points []point `json:"points"`
	}
	type projection struct {
		Method        string    `json:"method"`
		PeriodEnd     time.Time `json:"period_end"`
		ProjectedCost money     `json:"projected_cost"`
		Low           money     `json:"low"`
		High          money     `json:"high"`
	}
	out := struct {
		Interval  string      `json:"interval"`
		CostBasis string      `json:"cost_basis"`
		Series    []series    `json:"series"`
		Forecast  *projection `json:"forecast"`
	}{Interval: interval, CostBasis: "estimated", Series: []series{}}
	// Các dòng đã được sắp theo (key, period_start): mỗi lần đổi key là một chuỗi mới.
	for i, row := range rows {
		if i == 0 || row.Key != rows[i-1].Key {
			out.Series = append(out.Series, series{Key: nullable(row.Key)})
		}
		s := &out.Series[len(out.Series)-1]
		s.Points = append(s.Points, point{PeriodStart: row.PeriodStart.UTC(), InputTokens: row.InputTokens, OutputTokens: row.OutputTokens,
			EstimatedCost: money{row.EstimatedCost, usd}})
	}
	if forecast && fc.FullDays > 0 {
		out.Forecast = &projection{Method: "linear_run_rate", PeriodEnd: monthEnd, ProjectedCost: money{fc.Projected, usd},
			Low: money{fc.Low, usd}, High: money{fc.High, usd}}
	}
	httpx.WriteJSON(w, http.StatusOK, out)
	return nil
}
