// Package pricing quản lý bảng giá model (bounded context Cost, ADR-0007).
//
// Giá là dữ liệu có phiên bản và khoảng hiệu lực; không có giá nào trong source
// code. Giá mới là dòng mới; dòng đang hiệu lực chỉ được đóng tại effective_from
// của dòng mới, không bao giờ bị sửa. Mỗi bản ghi usage tham chiếu phiên bản giá
// đã dùng để tính.
//
// Tiền là chuỗi thập phân (NUMERIC(18,8) trong database), không bao giờ là float.
package pricing

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	pgxv5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hqgroups/mcp/internal/audit"
	"github.com/hqgroups/mcp/internal/cost/pricing/store"
	"github.com/hqgroups/mcp/internal/platform/identity"
	"github.com/hqgroups/mcp/internal/platform/tenancy"
	apperr "github.com/hqgroups/mcp/internal/shared/errors"
	"github.com/hqgroups/mcp/internal/shared/ids"
	"github.com/hqgroups/mcp/internal/shared/pagination"
)

// Phạm vi của một dòng giá.
const (
	ScopeList         = "list"         // giá niêm yết của platform
	ScopeOrganization = "organization" // giá riêng của org
)

// Pricing là một phiên bản giá của một model. Đơn giá tính trên một triệu token.
type Pricing struct {
	ID                 uuid.UUID
	ModelID            uuid.UUID
	Scope              string
	Version            int
	Currency           string
	InputPerMtok       string
	OutputPerMtok      string
	CachedInputPerMtok *string
	CacheWritePerMtok  *string
	ReasoningPerMtok   *string
	EffectiveFrom      time.Time
	EffectiveTo        *time.Time
	Source             *string
}

// Create là đầu vào thêm một phiên bản giá riêng của org.
type Create struct {
	Currency           string
	InputPerMtok       string
	OutputPerMtok      string
	CachedInputPerMtok *string
	CacheWritePerMtok  *string
	ReasoningPerMtok   *string
	EffectiveFrom      time.Time
	Source             *string
}

// ListParams là tham số liệt kê lịch sử giá.
type ListParams struct {
	Limit       int
	Cursor      string
	EffectiveAt *time.Time
}

// Models là thứ pricing cần từ Model Catalog.
type Models interface {
	// ModelScope trả lỗi not found nếu tenant không thấy model; platform = true
	// nếu model thuộc platform.
	ModelScope(ctx context.Context, modelID uuid.UUID) (platform bool, err error)
}

var (
	amountRe   = regexp.MustCompile(`^[0-9]{1,10}(\.[0-9]{1,8})?$`)
	currencyRe = regexp.MustCompile(`^[A-Z]{3}$`)

	// ErrNoPrice báo model không có giá hiệu lực tại thời điểm được hỏi. Mặc định
	// của platform là từ chối gọi model như vậy: không biết giá thì budget vô nghĩa.
	ErrNoPrice = apperr.New(apperr.KindUnprocessable, "no_effective_price", "model không có giá hiệu lực tại thời điểm này")

	errPlatformManaged = apperr.New(apperr.KindPermissionDenied, "platform_managed",
		"giá của model thuộc platform do platform quản lý, không đặt được từ organization")

	errConflict = apperr.New(apperr.KindConflict, "pricing_conflict",
		"effective_from phải sau effective_from của giá đang hiệu lực và không chồng lên khoảng giá đã có")
)

const (
	sortFrom    = "-effective_from"
	maxSource   = 500
	maxBackdate = 5 * time.Minute
	maxSchedule = 365 * 24 * time.Hour
)

func invalid(msg string) error { return apperr.New(apperr.KindUnprocessable, "validation_failed", msg) }
func internal(err error) error { return apperr.Wrap(err, apperr.KindInternal, "internal_error", "") }

// Service chứa nghiệp vụ bảng giá. Tenant luôn lấy từ context.
type Service struct {
	db     *pgxpool.Pool
	models Models
}

// NewService tạo Service.
func NewService(db *pgxpool.Pool, models Models) *Service { return &Service{db: db, models: models} }

func validAmount(field string, v *string, required bool) error {
	if v == nil {
		if required {
			return invalid(field + " là bắt buộc")
		}
		return nil
	}
	if !amountRe.MatchString(*v) {
		return invalid(field + " phải là số thập phân không âm, tối đa 10 chữ số phần nguyên và 8 chữ số thập phân")
	}
	return nil
}

// CreatePricing thêm một phiên bản giá riêng của org cho model. Giá riêng đang mở
// của org (nếu có) được đóng tại effective_from của giá mới.
func (s *Service) CreatePricing(ctx context.Context, modelID uuid.UUID, in Create) (Pricing, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return Pricing{}, internal(tenancy.ErrNoTenant)
	}
	if !currencyRe.MatchString(in.Currency) {
		return Pricing{}, invalid("currency phải là mã 3 chữ cái in hoa, ví dụ USD")
	}
	for _, f := range []struct {
		name     string
		v        *string
		required bool
	}{
		{"input_per_mtok", &in.InputPerMtok, true}, {"output_per_mtok", &in.OutputPerMtok, true},
		{"cached_input_per_mtok", in.CachedInputPerMtok, false}, {"cache_write_per_mtok", in.CacheWritePerMtok, false},
		{"reasoning_per_mtok", in.ReasoningPerMtok, false},
	} {
		if err := validAmount(f.name, f.v, f.required); err != nil {
			return Pricing{}, err
		}
	}
	// Giá không viết lại được quá khứ: mọi phép tính lại chi phí theo thời điểm phát
	// sinh (đối soát, báo cáo) phải ra đúng con số đã dùng lúc ghi usage. Dung sai
	// lùi chỉ để bù lệch đồng hồ; thời điểm bắt đầu thực tế do database quyết định.
	now := time.Now()
	if in.EffectiveFrom.Before(now.Add(-maxBackdate)) {
		return Pricing{}, invalid("effective_from không được ở quá khứ: giá chỉ áp dụng từ hiện tại trở đi")
	}
	// Dòng giá là bất biến và giá sau phải bắt đầu sau giá trước, nên một mốc quá xa
	// sẽ khoá việc đổi giá cho tới mốc đó.
	if in.EffectiveFrom.After(now.Add(maxSchedule)) {
		return Pricing{}, invalid("effective_from không được xa hơn 1 năm trong tương lai")
	}
	if in.Source != nil && (len(*in.Source) > maxSource ||
		strings.ContainsFunc(*in.Source, func(r rune) bool { return unicode.IsControl(r) })) {
		return Pricing{}, invalid("source tối đa 500 ký tự, không chứa ký tự điều khiển")
	}
	// Trước khi mở transaction: kiểm tra model dùng kết nối riêng.
	platform, err := s.models.ModelScope(ctx, modelID)
	if err != nil {
		return Pricing{}, err
	}
	// Tenant chỉ đặt giá cho model của chính mình. Giá của model thuộc platform (giá
	// niêm yết và giá thoả thuận riêng) do platform quản lý: nếu tenant tự đặt được,
	// họ đặt giá 0 và mọi budget theo tiền mất tác dụng.
	if platform {
		return Pricing{}, errPlatformManaged
	}
	modelScope := orgID
	var createdBy *uuid.UUID
	if p, ok := identity.PrincipalFrom(ctx); ok && p.Type == identity.PrincipalUser {
		createdBy = &p.ID
	}
	var row store.InsertPricingRow
	var from time.Time
	closed := ""
	err = tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) error {
		q := store.New(tx)
		if err := q.LockPricingScope(ctx, "pricing:"+orgID.String()+":"+modelID.String()); err != nil {
			return err
		}
		// Không sớm hơn "bây giờ" của database, kể cả khi đồng hồ ứng dụng lệch.
		var err error
		from, err = q.EffectiveStart(ctx, in.EffectiveFrom.UTC().Truncate(time.Microsecond))
		if err != nil {
			return err
		}
		open, err := q.GetOpenPricing(ctx, store.GetOpenPricingParams{OrgID: &orgID, ModelID: modelID})
		switch {
		case err == nil:
			if !from.After(open.EffectiveFrom) {
				return errConflict
			}
			if err := q.ClosePricing(ctx, store.ClosePricingParams{OrgID: &orgID, ID: open.ID, EffectiveTo: &from}); err != nil {
				return err
			}
			closed = open.ID.String()
		case !errors.Is(err, pgxv5.ErrNoRows):
			return err
		}
		latest, err := q.MaxPricingVersion(ctx, store.MaxPricingVersionParams{OrgID: &orgID, ModelID: modelID})
		if err != nil {
			return err
		}
		row, err = q.InsertPricing(ctx, store.InsertPricingParams{
			ID: ids.New(), ModelID: modelID, OrgID: &orgID, ModelScopeOrgID: modelScope,
			Version: latest + 1, Currency: in.Currency,
			InputPerMtok: in.InputPerMtok, OutputPerMtok: in.OutputPerMtok,
			CachedInputPerMtok: in.CachedInputPerMtok, CacheWritePerMtok: in.CacheWritePerMtok,
			ReasoningPerMtok: in.ReasoningPerMtok, EffectiveFrom: from, Source: in.Source, CreatedBy: createdBy,
		})
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{
			OrgID: orgID, Action: "model_pricing.create", ResourceType: "model", ResourceID: &modelID,
			Outcome: audit.OutcomeSuccess,
			Details: map[string]string{
				"pricing_id": row.ID.String(), "closed_pricing_id": closed,
				"version": strconv.Itoa(int(row.Version)), "currency": row.Currency,
				"input_per_mtok": row.InputPerMtok, "output_per_mtok": row.OutputPerMtok,
				"cached_input_per_mtok": row.CachedInputPerMtok, "cache_write_per_mtok": row.CacheWritePerMtok,
				"reasoning_per_mtok": row.ReasoningPerMtok,
				"effective_from":     from.UTC().Format(time.RFC3339Nano),
			}})
	})
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &pgErr) && pgErr.Code == "23P01": // vi phạm EXCLUDE: chồng khoảng hiệu lực
		return Pricing{}, errConflict
	case errors.As(err, &pgErr) && pgErr.Code == "23503":
		return Pricing{}, apperr.New(apperr.KindNotFound, "not_found", "tài nguyên không tồn tại")
	case err != nil:
		if _, ok := apperr.From(err); ok {
			return Pricing{}, err
		}
		return Pricing{}, internal(err)
	}
	return fromRow(row), nil
}

// ListPricing trả về lịch sử giá của model (giá riêng của org và giá niêm yết), mới nhất trước.
func (s *Service) ListPricing(ctx context.Context, modelID uuid.UUID, p ListParams) (out []Pricing, next string, err error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return nil, "", internal(tenancy.ErrNoTenant)
	}
	var afterFrom *time.Time
	var afterID *uuid.UUID
	if c, cerr := pagination.Decode(p.Cursor, sortFrom); cerr != nil {
		return nil, "", cerr
	} else if c != nil {
		t, perr := time.Parse(time.RFC3339Nano, c.Value)
		if perr != nil {
			return nil, "", pagination.ErrInvalidCursor
		}
		afterFrom, afterID = &t, &c.ID
	}
	var rows []store.ListPricingRow
	err = tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) (err error) {
		rows, err = store.New(tx).ListPricing(ctx, store.ListPricingParams{
			ModelID: modelID, OrgID: &orgID, EffectiveAt: p.EffectiveAt,
			AfterFrom: afterFrom, AfterID: afterID, RowLimit: int32(p.Limit + 1)}) //nolint:gosec // limit ≤ 200
		return err
	})
	if err != nil {
		return nil, "", internal(err)
	}
	if len(rows) > p.Limit {
		rows = rows[:p.Limit]
		last := rows[p.Limit-1]
		next = pagination.Encode(pagination.Cursor{Sort: sortFrom, ID: last.ID, Value: last.EffectiveFrom.UTC().Format(time.RFC3339Nano)})
	}
	for _, r := range rows {
		out = append(out, fromRow(store.InsertPricingRow(r)))
	}
	return out, next, nil
}

// PriceAt trả về giá hiệu lực của model tại thời điểm at: ưu tiên giá riêng của
// org, sau đó giá niêm yết. Trả ErrNoPrice nếu không có.
func (s *Service) PriceAt(ctx context.Context, modelID uuid.UUID, at time.Time) (Pricing, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return Pricing{}, internal(tenancy.ErrNoTenant)
	}
	var row store.PriceAtRow
	err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) (err error) {
		row, err = store.New(tx).PriceAt(ctx, store.PriceAtParams{ModelID: modelID, OrgID: &orgID, At: at})
		return err
	})
	if errors.Is(err, pgxv5.ErrNoRows) {
		return Pricing{}, ErrNoPrice
	}
	if err != nil {
		return Pricing{}, internal(err)
	}
	return fromRow(store.InsertPricingRow(row)), nil
}

// ModelsWithCurrentPricing cho biết trong các model đã cho, model nào đang có giá hiệu lực.
func (s *Service) ModelsWithCurrentPricing(ctx context.Context, modelIDs []uuid.UUID) (map[uuid.UUID]bool, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return nil, internal(tenancy.ErrNoTenant)
	}
	out := make(map[uuid.UUID]bool, len(modelIDs))
	if len(modelIDs) == 0 {
		return out, nil
	}
	var priced []uuid.UUID
	err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) (err error) {
		priced, err = store.New(tx).ModelsWithCurrentPricing(ctx, store.ModelsWithCurrentPricingParams{ModelIds: modelIDs, OrgID: &orgID})
		return err
	})
	if err != nil {
		return nil, internal(err)
	}
	for _, id := range priced {
		out[id] = true
	}
	return out, nil
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func fromRow(r store.InsertPricingRow) Pricing {
	p := Pricing{ID: r.ID, ModelID: r.ModelID, Scope: ScopeOrganization, Version: int(r.Version), Currency: r.Currency,
		InputPerMtok: r.InputPerMtok, OutputPerMtok: r.OutputPerMtok,
		CachedInputPerMtok: optional(r.CachedInputPerMtok), CacheWritePerMtok: optional(r.CacheWritePerMtok),
		ReasoningPerMtok: optional(r.ReasoningPerMtok), EffectiveFrom: r.EffectiveFrom.UTC(), Source: r.Source}
	if r.OrgID == nil {
		p.Scope = ScopeList
	}
	if r.EffectiveTo != nil {
		t := r.EffectiveTo.UTC()
		p.EffectiveTo = &t
	}
	return p
}
