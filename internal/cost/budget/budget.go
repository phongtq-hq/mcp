// Package budget là Budget Management (cost-management §4): cấu hình budget theo phạm
// vi và việc thực thi nó quanh mỗi lời gọi model theo kiểu đặt trước rồi quyết toán.
//
// Postgres là nguồn sự thật (bảng budgets là cấu hình, usage_records là mức đã tiêu).
// Counter là bộ đếm nhanh, dựng lại được từ ledger: bucket chưa có thì được nạp từ
// usage_records ở lần đặt trước đầu tiên. Package này không gọi LLM và không biết Agent
// hoạt động thế nào; nó chỉ nhận phạm vi của một lời gọi.
package budget

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/google/uuid"
	pgxv5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hqgroups/mcp/internal/audit"
	"github.com/hqgroups/mcp/internal/cost/budget/store"
	"github.com/hqgroups/mcp/internal/cost/pricing"
	"github.com/hqgroups/mcp/internal/platform/identity"
	"github.com/hqgroups/mcp/internal/platform/tenancy"
	apperr "github.com/hqgroups/mcp/internal/shared/errors"
	"github.com/hqgroups/mcp/internal/shared/ids"
)

const (
	ScopeOrganization = "organization"
	ScopeProject      = "project"
	ScopeUser         = "user"
	ScopeAgent        = "agent"

	PeriodDaily   = "daily"
	PeriodMonthly = "monthly"
	PeriodTotal   = "total"

	ActionBlock     = "block"
	ActionAlertOnly = "alert_only"

	// unitsPerCurrency: tiền được tính bằng số nguyên đơn vị 10^-8 (NUMERIC(18,8)).
	unitsPerCurrency = 100_000_000
	// maxLimitUnits: trần của một hạn mức (10 triệu USD). Bộ đếm Redis là số thực 64
	// bit, chính xác tới 2^53; trần này giữ mọi phép cộng dưới ngưỡng đó.
	maxLimitUnits = 10_000_000 * unitsPerCurrency
	// reservationTTL: phần đặt trước tự hoàn nếu không ai quyết toán (process gọi model
	// chết giữa chừng). Dài hơn thời hạn tối đa của một lời gọi model.
	reservationTTL = 12 * time.Minute
	// bytesPerToken: ước lượng thận trọng số byte của một token đầu vào (ước lượng thấp
	// số byte mỗi token = ước lượng cao số token = đặt trước nhiều hơn mức thật). Văn bản
	// thường khoảng 3–4 byte mỗi token; chuỗi ký hiệu, số, hex trong kết quả tool thì ít hơn.
	bytesPerToken = 2
)

var (
	scopes  = []string{ScopeOrganization, ScopeProject, ScopeUser, ScopeAgent}
	periods = []string{PeriodDaily, PeriodMonthly, PeriodTotal}
	actions = []string{ActionBlock, ActionAlertOnly}

	amountRe = regexp.MustCompile(`^\d{1,10}(\.\d{1,8})?$`)

	errNotFound = apperr.New(apperr.KindNotFound, "not_found", "tài nguyên không tồn tại")
	// ErrUnavailable: không kiểm được budget có hard_action = block (bộ đếm không sẵn
	// sàng). Fail-closed: lời gọi bị từ chối, thử lại được.
	ErrUnavailable = apperr.New(apperr.KindUnavailable, "budget_unavailable", "không kiểm tra được budget; thử lại sau").AsRetryable()
)

func invalid(msg string) error { return apperr.New(apperr.KindUnprocessable, "validation_failed", msg) }
func internal(err error) error { return apperr.Wrap(err, apperr.KindInternal, "internal_error", "") }

// CodeExceeded là mã lỗi khi một budget chạm ngưỡng cứng.
const CodeExceeded = "budget_exceeded"

// Exceeded tạo lỗi vượt budget. scope là phạm vi đã chạm ngưỡng (execution, organization…).
func Exceeded(scope string) error {
	return apperr.New(apperr.KindBudgetExceeded, CodeExceeded, "budget của "+scope+" đã chạm ngưỡng")
}

// ---- tiền ----

// ParseAmount đổi số thập phân dạng chuỗi sang đơn vị 10^-8. Không dùng float.
func ParseAmount(s string) (int64, bool) {
	if !amountRe.MatchString(s) {
		return 0, false
	}
	return toUnits(s)
}

// toUnits đổi một số thập phân không âm (kết quả của NUMERIC::text) sang đơn vị 10^-8,
// làm tròn lên.
func toUnits(s string) (int64, bool) {
	r, ok := new(big.Rat).SetString(s)
	if !ok || r.Sign() < 0 {
		return 0, false
	}
	r.Mul(r, big.NewRat(unitsPerCurrency, 1))
	q, m := new(big.Int).QuoRem(r.Num(), r.Denom(), new(big.Int))
	if m.Sign() != 0 {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsInt64() {
		return 0, false
	}
	return q.Int64(), true
}

// FormatAmount đổi đơn vị 10^-8 về chuỗi thập phân 8 chữ số.
func FormatAmount(units int64) string {
	sign := ""
	if units < 0 {
		sign, units = "-", -units
	}
	return fmt.Sprintf("%s%d.%08d", sign, units/unitsPerCurrency, units%unitsPerCurrency)
}

// costUnits = tokens × giá mỗi triệu token, làm tròn lên, tính bằng số nguyên lớn.
func costUnits(tokens int64, perMtok string) int64 {
	if tokens <= 0 || perMtok == "" {
		return 0
	}
	price, ok := new(big.Rat).SetString(perMtok)
	if !ok || price.Sign() <= 0 {
		return 0
	}
	r := new(big.Rat).Mul(price, big.NewRat(tokens*unitsPerCurrency/1, 1_000_000))
	q, m := new(big.Int).QuoRem(r.Num(), r.Denom(), new(big.Int))
	if m.Sign() != 0 {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsInt64() {
		return maxLimitUnits // giá trị vô lý: coi như vượt mọi hạn mức
	}
	return q.Int64()
}

func orElse(p *string, fallback string) string {
	if p != nil {
		return *p
	}
	return fallback
}

// Estimate là ước lượng trần chi phí của một lượt gọi (cost-management §4.2): token
// đầu vào ước lượng × giá đầu vào cao nhất (ghi cache thường đắt hơn input) cộng
// max_output_tokens × giá đầu ra cao nhất (output hoặc reasoning). Đây là ước lượng
// thận trọng, không phải bảo đảm: số token thật chỉ provider biết, nên một lượt vẫn
// có thể vượt phần đã đặt trước; phần vượt được cộng vào bộ đếm lúc quyết toán.
func Estimate(p pricing.Pricing, inputBytes, maxOutputTokens int) int64 {
	in := int64(inputBytes+bytesPerToken-1) / bytesPerToken
	inCost := max(costUnits(in, p.InputPerMtok), costUnits(in, orElse(p.CacheWritePerMtok, "")))
	out := int64(maxOutputTokens)
	return inCost + max(costUnits(out, p.OutputPerMtok), costUnits(out, orElse(p.ReasoningPerMtok, "")))
}

// Usage là số token thật của một lượt gọi, do provider báo.
type Usage struct {
	InputTokens, OutputTokens, CachedInputTokens, CacheWriteTokens, ReasoningTokens int64
}

// Actual là chi phí thật của một lượt gọi, cùng công thức với usage ledger.
func Actual(p pricing.Pricing, u Usage) int64 {
	return costUnits(u.InputTokens, p.InputPerMtok) + costUnits(u.OutputTokens, p.OutputPerMtok) +
		costUnits(u.CachedInputTokens, orElse(p.CachedInputPerMtok, p.InputPerMtok)) +
		costUnits(u.CacheWriteTokens, orElse(p.CacheWritePerMtok, p.InputPerMtok)) +
		costUnits(u.ReasoningTokens, orElse(p.ReasoningPerMtok, ""))
}

// ---- kỳ ----

// PeriodBounds trả về đầu kỳ và cuối kỳ (nil với total) chứa thời điểm at, theo UTC.
func PeriodBounds(period string, at time.Time) (start time.Time, end *time.Time) {
	at = at.UTC()
	switch period {
	case PeriodDaily:
		start = time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC)
		e := start.AddDate(0, 0, 1)
		return start, &e
	case PeriodMonthly:
		start = time.Date(at.Year(), at.Month(), 1, 0, 0, 0, 0, time.UTC)
		e := start.AddDate(0, 1, 0)
		return start, &e
	}
	return time.Unix(0, 0).UTC(), nil
}

// ---- cấu hình ----

// Budget là một hạn mức chi tiêu.
type Budget struct {
	ID               uuid.UUID
	ScopeType        string
	ScopeID          uuid.UUID
	Period           string
	Limit            string
	Currency         string
	SoftThresholdPct *int
	HardAction       string
	Enabled          bool
	CreatedAt        time.Time
	UpdatedAt        time.Time
	// Kỳ hiện tại, tính từ ledger.
	PeriodStart time.Time
	PeriodEnd   *time.Time
	Spent       string
}

// Create là đầu vào tạo budget.
type Create struct {
	ScopeType        string
	ScopeID          uuid.UUID
	Period           string
	Limit            string
	Currency         string
	SoftThresholdPct *int
	HardAction       string
}

// Update là đầu vào sửa budget. Trường nil thì giữ nguyên.
type Update struct {
	Limit      *string
	Currency   *string
	HardAction *string
	Enabled    *bool
	// SetSoft: có đổi ngưỡng mềm hay không (SoftThresholdPct nil = bỏ ngưỡng).
	SetSoft          bool
	SoftThresholdPct *int
	// IfUpdatedAt khác nil: chỉ sửa nếu budget chưa đổi kể từ thời điểm đó (If-Match).
	IfUpdatedAt *time.Time
}

// ErrPrecondition: budget đã đổi kể từ phiên bản client đang giữ.
var ErrPrecondition = apperr.New(apperr.KindPreconditionFailed, "precondition_failed", "tài nguyên đã thay đổi; đọc lại rồi thử lại")

// Scopes kiểm một phạm vi tồn tại trong organization của request.
type Scopes interface {
	ValidateScope(ctx context.Context, scopeType string, scopeID uuid.UUID) error
}

// Prices là thứ budget cần từ bảng giá.
type Prices interface {
	PriceAt(ctx context.Context, modelID uuid.UUID, at time.Time) (pricing.Pricing, error)
}

// Service quản lý budget và thực thi nó.
type Service struct {
	db      *pgxpool.Pool
	scopes  Scopes
	prices  Prices
	counter Counter
}

// NewService tạo Service. scopes chỉ cần cho việc tạo budget (process api); prices và
// counter chỉ cần cho việc thực thi quanh lời gọi model.
func NewService(db *pgxpool.Pool, scopes Scopes, prices Prices, counter Counter) *Service {
	return &Service{db: db, scopes: scopes, prices: prices, counter: counter}
}

func validLimit(amount, currency string) (int64, error) {
	if currency != "USD" {
		return 0, invalid("limit.currency chỉ hỗ trợ USD")
	}
	units, ok := ParseAmount(amount)
	if !ok || units > maxLimitUnits {
		return 0, invalid("limit.amount phải là số thập phân không âm, tối đa 8 chữ số thập phân và không vượt 10000000")
	}
	return units, nil
}

func validSoft(p *int) error {
	if p != nil && (*p < 1 || *p > 100) {
		return invalid("soft_threshold_pct phải từ 1 đến 100")
	}
	return nil
}

func soft32(p *int) *int32 {
	if p == nil {
		return nil
	}
	v := int32(*p) //nolint:gosec // đã chặn 1..100
	return &v
}

func softText(p *int) string {
	if p == nil {
		return ""
	}
	return strconv.Itoa(*p)
}

func softInt(p *int32) *int {
	if p == nil {
		return nil
	}
	v := int(*p)
	return &v
}

// spent trả về mức đã tiêu của một phạm vi kể từ since, theo ledger.
func spentSince(ctx context.Context, tx pgxv5.Tx, orgID uuid.UUID, scopeType string, scopeID uuid.UUID, since time.Time) (string, error) {
	q := store.New(tx)
	switch scopeType {
	case ScopeOrganization:
		return q.SpentSinceOrg(ctx, store.SpentSinceOrgParams{OrgID: orgID, Since: since})
	case ScopeProject:
		return q.SpentSinceProject(ctx, store.SpentSinceProjectParams{OrgID: orgID, ScopeID: scopeID, Since: since})
	case ScopeAgent:
		return q.SpentSinceAgent(ctx, store.SpentSinceAgentParams{OrgID: orgID, ScopeID: &scopeID, Since: since})
	case ScopeUser:
		return q.SpentSinceUser(ctx, store.SpentSinceUserParams{OrgID: orgID, ScopeID: &scopeID, Since: since})
	}
	return "", fmt.Errorf("budget: phạm vi không xác định: %q", scopeType)
}

func (s *Service) withUsage(ctx context.Context, tx pgxv5.Tx, orgID uuid.UUID, b Budget) (Budget, error) {
	b.PeriodStart, b.PeriodEnd = PeriodBounds(b.Period, time.Now())
	spent, err := spentSince(ctx, tx, orgID, b.ScopeType, b.ScopeID, b.PeriodStart)
	b.Spent = spent
	return b, err
}

func fromRow(id, scopeID uuid.UUID, scopeType, period, limit, currency string, softPct *int32, action string, enabled bool, created, updated time.Time) Budget {
	return Budget{ID: id, ScopeType: scopeType, ScopeID: scopeID, Period: period, Limit: limit, Currency: currency,
		SoftThresholdPct: softInt(softPct), HardAction: action, Enabled: enabled, CreatedAt: created.UTC(), UpdatedAt: updated.UTC()}
}

// Create tạo một budget. Mỗi (phạm vi, chu kỳ) chỉ có một budget.
func (s *Service) Create(ctx context.Context, in Create) (Budget, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return Budget{}, internal(tenancy.ErrNoTenant)
	}
	if !slices.Contains(scopes, in.ScopeType) {
		return Budget{}, invalid("scope_type phải là organization, project, user hoặc agent")
	}
	if !slices.Contains(periods, in.Period) {
		return Budget{}, invalid("period phải là daily, monthly hoặc total")
	}
	if in.HardAction == "" {
		in.HardAction = ActionBlock
	}
	if !slices.Contains(actions, in.HardAction) {
		return Budget{}, invalid("hard_action phải là block hoặc alert_only")
	}
	if _, err := validLimit(in.Limit, in.Currency); err != nil {
		return Budget{}, err
	}
	if err := validSoft(in.SoftThresholdPct); err != nil {
		return Budget{}, err
	}
	// Phạm vi phải tồn tại trong org: budget trỏ tới project / agent / user của org khác
	// vừa vô nghĩa vừa là kênh dò id.
	if in.ScopeType == ScopeOrganization {
		if in.ScopeID != orgID {
			return Budget{}, invalid("scope_id của budget organization phải là id của organization hiện tại")
		}
	} else if s.scopes == nil {
		return Budget{}, internal(fmt.Errorf("budget: thiếu Scopes"))
	} else if err := s.scopes.ValidateScope(ctx, in.ScopeType, in.ScopeID); err != nil {
		return Budget{}, err
	}
	var creator *uuid.UUID
	if p, ok := identity.PrincipalFrom(ctx); ok && p.Type == identity.PrincipalUser {
		creator = &p.ID
	}
	var out Budget
	err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) error {
		r, err := store.New(tx).InsertBudget(ctx, store.InsertBudgetParams{ID: ids.New(), OrgID: orgID, ScopeType: in.ScopeType,
			ScopeID: in.ScopeID, Period: in.Period, LimitAmount: in.Limit, Currency: in.Currency,
			SoftThresholdPct: soft32(in.SoftThresholdPct), HardAction: in.HardAction, CreatedBy: creator})
		if err != nil {
			return err
		}
		out = fromRow(r.ID, r.ScopeID, r.ScopeType, r.Period, r.LimitAmount, r.Currency, r.SoftThresholdPct, r.HardAction, r.Enabled, r.CreatedAt, r.UpdatedAt)
		if out, err = s.withUsage(ctx, tx, orgID, out); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{OrgID: orgID, Action: "budget.create", ResourceType: "budget", ResourceID: &out.ID,
			Outcome: audit.OutcomeSuccess, Details: map[string]string{"scope_type": out.ScopeType, "scope_id": out.ScopeID.String(),
				"period": out.Period, "limit": out.Limit, "hard_action": out.HardAction}})
	})
	if pgxUnique(err) {
		return Budget{}, apperr.New(apperr.KindConflict, "budget_exists", "phạm vi này đã có budget cho chu kỳ đó")
	}
	if err != nil {
		return Budget{}, internal(err)
	}
	return out, nil
}

// Get trả về một budget kèm mức sử dụng của kỳ hiện tại.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (Budget, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return Budget{}, internal(tenancy.ErrNoTenant)
	}
	var out Budget
	err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) error {
		r, err := store.New(tx).GetBudget(ctx, store.GetBudgetParams{OrgID: orgID, ID: id})
		if err != nil {
			return err
		}
		out = fromRow(r.ID, r.ScopeID, r.ScopeType, r.Period, r.LimitAmount, r.Currency, r.SoftThresholdPct, r.HardAction, r.Enabled, r.CreatedAt, r.UpdatedAt)
		out, err = s.withUsage(ctx, tx, orgID, out)
		return err
	})
	if isNoRows(err) {
		return Budget{}, errNotFound
	}
	if err != nil {
		return Budget{}, internal(err)
	}
	return out, nil
}

// ListFilter lọc danh sách budget.
type ListFilter struct {
	ScopeType    *string
	ScopeID      *uuid.UUID
	AfterCreated *time.Time
	AfterID      *uuid.UUID
	Limit        int
}

// List liệt kê budget của organization, mới nhất trước. Trả thêm một dòng so với
// Limit nếu còn trang sau.
func (s *Service) List(ctx context.Context, f ListFilter) ([]Budget, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return nil, internal(tenancy.ErrNoTenant)
	}
	if f.ScopeType != nil && !slices.Contains(scopes, *f.ScopeType) {
		return nil, apperr.New(apperr.KindInvalid, "invalid_parameter", "scope_type không hợp lệ")
	}
	var out []Budget
	err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) error {
		rows, err := store.New(tx).ListBudgets(ctx, store.ListBudgetsParams{OrgID: orgID, ScopeType: f.ScopeType, ScopeID: f.ScopeID,
			AfterCreated: f.AfterCreated, AfterID: f.AfterID, RowLimit: int32(f.Limit + 1)}) //nolint:gosec // limit ≤ 200
		if err != nil {
			return err
		}
		for _, r := range rows {
			b := fromRow(r.ID, r.ScopeID, r.ScopeType, r.Period, r.LimitAmount, r.Currency, r.SoftThresholdPct, r.HardAction, r.Enabled, r.CreatedAt, r.UpdatedAt)
			if b, err = s.withUsage(ctx, tx, orgID, b); err != nil {
				return err
			}
			out = append(out, b)
		}
		return nil
	})
	if err != nil {
		return nil, internal(err)
	}
	return out, nil
}

// Update sửa hạn mức, ngưỡng mềm, hành vi khi vượt, hoặc bật / tắt một budget.
func (s *Service) Update(ctx context.Context, id uuid.UUID, u Update) (Budget, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return Budget{}, internal(tenancy.ErrNoTenant)
	}
	if u.Limit != nil {
		currency := "USD"
		if u.Currency != nil {
			currency = *u.Currency
		}
		if _, err := validLimit(*u.Limit, currency); err != nil {
			return Budget{}, err
		}
	}
	if u.HardAction != nil && !slices.Contains(actions, *u.HardAction) {
		return Budget{}, invalid("hard_action phải là block hoặc alert_only")
	}
	if err := validSoft(u.SoftThresholdPct); err != nil {
		return Budget{}, err
	}
	var out Budget
	err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) error {
		q := store.New(tx)
		r, err := q.UpdateBudget(ctx, store.UpdateBudgetParams{OrgID: orgID, ID: id, LimitAmount: u.Limit,
			SetSoft: u.SetSoft, SoftThresholdPct: soft32(u.SoftThresholdPct), HardAction: u.HardAction, Enabled: u.Enabled,
			IfUpdatedAt: u.IfUpdatedAt})
		if isNoRows(err) && u.IfUpdatedAt != nil {
			// Không dòng nào khớp: budget không tồn tại, hoặc đã đổi (If-Match lệch).
			if _, gerr := q.GetBudget(ctx, store.GetBudgetParams{OrgID: orgID, ID: id}); gerr == nil {
				return ErrPrecondition
			}
		}
		if err != nil {
			return err
		}
		out = fromRow(r.ID, r.ScopeID, r.ScopeType, r.Period, r.LimitAmount, r.Currency, r.SoftThresholdPct, r.HardAction, r.Enabled, r.CreatedAt, r.UpdatedAt)
		if out, err = s.withUsage(ctx, tx, orgID, out); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{OrgID: orgID, Action: "budget.update", ResourceType: "budget", ResourceID: &id,
			Outcome: audit.OutcomeSuccess, Details: map[string]string{"limit": out.Limit, "hard_action": out.HardAction,
				"enabled": strconv.FormatBool(out.Enabled), "soft_threshold_pct": softText(out.SoftThresholdPct)}})
	})
	if errors.Is(err, ErrPrecondition) {
		return Budget{}, ErrPrecondition
	}
	if isNoRows(err) {
		return Budget{}, errNotFound
	}
	if err != nil {
		return Budget{}, internal(err)
	}
	return out, nil
}

// Delete xoá một budget. Lịch sử của nó nằm trong audit.
func (s *Service) Delete(ctx context.Context, id uuid.UUID) error {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return internal(tenancy.ErrNoTenant)
	}
	err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) error {
		// Budget bị xoá hẳn (cấu hình nhỏ, không có gì tham chiếu tới nó); nội dung của nó
		// lúc xoá được giữ trong audit.
		r, err := store.New(tx).DeleteBudget(ctx, store.DeleteBudgetParams{OrgID: orgID, ID: id})
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{OrgID: orgID, Action: "budget.delete", ResourceType: "budget", ResourceID: &id,
			Outcome: audit.OutcomeSuccess, Details: map[string]string{"scope_type": r.ScopeType, "scope_id": r.ScopeID.String(),
				"period": r.Period, "limit": r.LimitAmount}})
	})
	if isNoRows(err) {
		return errNotFound
	}
	if err != nil {
		return internal(err)
	}
	return nil
}
