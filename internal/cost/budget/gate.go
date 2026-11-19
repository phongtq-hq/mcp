package budget

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"github.com/google/uuid"
	pgxv5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/hqgroups/mcp/internal/audit"
	"github.com/hqgroups/mcp/internal/cost/budget/store"
	"github.com/hqgroups/mcp/internal/cost/pricing"
	"github.com/hqgroups/mcp/internal/platform/tenancy"
	"github.com/hqgroups/mcp/internal/shared/ids"
)

func isNoRows(err error) bool { return errors.Is(err, pgxv5.ErrNoRows) }

func pgxUnique(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// Scope là phạm vi của một lời gọi model: nó tiêu vào budget của organization, của
// project, của agent, và của user khởi tạo (nếu là user).
type Scope struct {
	OrgID     uuid.UUID
	ProjectID uuid.UUID
	AgentID   uuid.UUID
	UserID    *uuid.UUID
}

// TaskBudget là budget của một task (limits.budget của Agent Version) và mức task đã
// tiêu, do bên gọi đọc từ dữ liệu thực thi của nó.
type TaskBudget struct {
	// ID là id của task; khoá bộ đếm của budget task dựng từ nó.
	ID    uuid.UUID
	Limit string
	Spent string
}

// Call là một lời gọi model sắp thực hiện.
type Call struct {
	Scope           Scope
	ModelID         uuid.UUID
	InputBytes      int
	MaxOutputTokens int
	// Task khác nil: lời gọi còn phải nằm trong budget của task.
	Task *TaskBudget
	// Key định danh lời gọi (ví dụ task + model). Lời gọi lặp lại cùng Key thay thế phần
	// đặt trước của lần trước, để một lần gọi chết giữa chừng rồi được thử lại không giữ
	// chỗ hai lần. Rỗng: mỗi lần đặt trước độc lập.
	Key string
}

// Reservation là phần đã đặt trước cho một lời gọi; phải được Settle đúng một lần.
type Reservation struct {
	id       string
	orgID    uuid.UUID
	buckets  []Bucket
	budgets  []applicable
	price    pricing.Pricing
	estimate int64
}

// Estimate là trần chi phí đã đặt trước, dạng chuỗi thập phân.
func (r *Reservation) Estimate() string { return FormatAmount(r.estimate) }

type applicable struct {
	id        uuid.UUID
	scopeType string
	scopeID   uuid.UUID
	period    string
	limit     int64
	softPct   int // 0 = không có ngưỡng mềm
	enforce   bool
}

func (a applicable) bucket(orgID uuid.UUID, now time.Time) Bucket {
	start, end := PeriodBounds(a.period, now)
	// Bộ đếm sống hết kỳ cộng hai ngày (usage ghi trễ); total được gia hạn mỗi lần dùng.
	expire := now.Add(400 * 24 * time.Hour)
	if end != nil {
		expire = end.Add(48 * time.Hour)
	}
	// Tiền tố org:{id}: theo quy ước Redis của platform; dấu ngoặc nhọn giữ mọi khoá của
	// một org trên cùng một slot để script nhiều khoá chạy được trên Redis Cluster.
	key := "org:{" + orgID.String() + "}:budget:" + a.id.String() + ":" + strconv.FormatInt(start.Unix(), 10)
	return Bucket{Key: key, Limit: a.limit, Enforce: a.enforce, ExpireAt: expire}
}

// load đọc các budget đang bật áp cho một phạm vi.
func (s *Service) load(ctx context.Context, sc Scope) ([]applicable, error) {
	var rows []store.ApplicableBudgetsRow
	err := tenancy.InTx(tenancy.WithOrg(ctx, sc.OrgID), s.db, func(tx pgxv5.Tx) (err error) {
		rows, err = store.New(tx).ApplicableBudgets(ctx, store.ApplicableBudgetsParams{OrgID: sc.OrgID, ProjectID: sc.ProjectID,
			AgentID: sc.AgentID, UserID: sc.UserID})
		return err
	})
	if err != nil {
		return nil, err
	}
	out := make([]applicable, 0, len(rows))
	for _, r := range rows {
		limit, ok := toUnits(r.LimitAmount)
		if !ok || r.Currency != "USD" {
			continue // chỉ USD được thực thi ở phase này
		}
		a := applicable{id: r.ID, scopeType: r.ScopeType, scopeID: r.ScopeID, period: r.Period, limit: limit, enforce: r.HardAction == ActionBlock}
		if r.SoftThresholdPct != nil {
			a.softPct = int(*r.SoftThresholdPct)
		}
		out = append(out, a)
	}
	return out, nil
}

// spentUnits đọc mức đã tiêu của một budget trong kỳ hiện tại từ ledger.
func (s *Service) spentUnits(ctx context.Context, orgID uuid.UUID, a applicable, now time.Time) (int64, error) {
	start, _ := PeriodBounds(a.period, now)
	var raw string
	err := tenancy.InTx(tenancy.WithOrg(ctx, orgID), s.db, func(tx pgxv5.Tx) (err error) {
		raw, err = spentSince(ctx, tx, orgID, a.scopeType, a.scopeID, start)
		return err
	})
	if err != nil {
		return 0, err
	}
	units, ok := toUnits(raw)
	if !ok {
		return 0, errors.New("budget: mức đã tiêu không đọc được")
	}
	return units, nil
}

// CheckNew kiểm trước khi nhận một execution mới: budget có hard_action = block đã
// chạm ngưỡng thì từ chối (HTTP 402). Dựa trên ledger; phần đang đặt trước của các
// execution đang chạy được tính ở từng lời gọi model.
func (s *Service) CheckNew(ctx context.Context, sc Scope) error {
	budgets, err := s.load(ctx, sc)
	if err != nil {
		return internal(err)
	}
	now := time.Now()
	for _, a := range budgets {
		if !a.enforce {
			continue
		}
		spent, err := s.spentUnits(ctx, sc.OrgID, a, now)
		if err != nil {
			return internal(err)
		}
		if spent >= a.limit {
			return Exceeded(a.scopeType)
		}
	}
	return nil
}

// taskBucketTTL: bộ đếm của budget task sống lâu hơn mọi task (thời hạn task tối đa
// tính bằng giờ); nó chỉ là bộ đếm nhanh, mức đã tiêu của task nằm ở dữ liệu thực thi.
const taskBucketTTL = 48 * time.Hour

// Reserve đặt trước trần chi phí của một lời gọi model ở mọi budget áp dụng: task,
// rồi agent / user / project / organization, nguyên tử. Bất kỳ budget nào không đủ thì
// trả lỗi budget_exceeded và không giữ lại gì. Reservation trả về phải được Settle.
//
// Service không có Counter (process không nối bộ đếm dùng chung) chỉ kiểm budget của
// task bằng mức đã tiêu do bên gọi đưa; budget theo phạm vi không được kiểm ở đó.
func (s *Service) Reserve(ctx context.Context, c Call) (*Reservation, error) {
	if s.prices == nil {
		return nil, internal(errors.New("budget: thiếu Prices"))
	}
	now := time.Now()
	price, err := s.prices.PriceAt(tenancy.WithOrg(ctx, c.Scope.OrgID), c.ModelID, now)
	if err != nil {
		return nil, err // model chưa có giá: không gọi (cost-management §3)
	}
	// Bộ đếm và ledger chỉ cộng USD: giá bằng tiền khác sẽ làm hai bên lệch nhau.
	if price.Currency != "USD" {
		return nil, invalid("giá của model không tính bằng USD; budget chưa quy đổi tiền tệ")
	}
	res := &Reservation{id: c.Key, orgID: c.Scope.OrgID, price: price, estimate: Estimate(price, c.InputBytes, c.MaxOutputTokens)}
	if res.id == "" {
		res.id = ids.New().String()
	}

	var taskLimit, taskSpent int64
	hasTask := c.Task != nil && c.Task.Limit != ""
	if hasTask {
		var ok1, ok2 bool
		taskLimit, ok1 = toUnits(c.Task.Limit)
		taskSpent, ok2 = toUnits(c.Task.Spent)
		if !ok1 || !ok2 {
			return nil, internal(errors.New("budget: budget của task không đọc được"))
		}
		// Mức đã tiêu do bên gọi đưa là cận dưới: vượt theo nó thì chắc chắn vượt.
		if taskSpent+res.estimate > taskLimit {
			return nil, Exceeded("execution")
		}
	}
	if s.counter == nil {
		return res, nil
	}

	res.budgets, err = s.load(ctx, c.Scope)
	if err != nil {
		return nil, internal(err)
	}
	blocking := false
	for _, a := range res.budgets {
		res.buckets = append(res.buckets, a.bucket(c.Scope.OrgID, now))
		blocking = blocking || a.enforce
	}
	// Budget của task cũng là một bucket trong cùng lần đặt trước: nhiều lượt gọi đồng
	// thời của một task không cùng lọt qua, và mức đã tiêu của nó do nơi gọi model đếm,
	// không chỉ dựa vào số do runtime ghi. Bucket chưa có thì nạp từ mức bên gọi đưa.
	taskIdx := -1
	if hasTask {
		taskIdx = len(res.buckets)
		res.budgets = append(res.budgets, applicable{id: c.Task.ID, scopeType: "execution", limit: taskLimit, enforce: true})
		res.buckets = append(res.buckets, Bucket{Key: "org:{" + c.Scope.OrgID.String() + "}:budget:task:" + c.Task.ID.String(),
			Limit: taskLimit, Enforce: true, ExpireAt: now.Add(taskBucketTTL)})
		blocking = true
	}
	if len(res.buckets) == 0 {
		return res, nil
	}
	// Bucket chưa có (kỳ mới, hoặc bộ đếm bị mất) được nạp từ ledger rồi thử lại, có trần.
	for attempt := 0; attempt < 3; attempt++ {
		got, err := s.counter.Reserve(ctx, res.id, res.buckets, res.estimate, reservationTTL)
		if err != nil {
			return s.unavailable(ctx, res, blocking, err)
		}
		if got.OK {
			return res, nil
		}
		if len(got.Missing) == 0 {
			return nil, Exceeded(res.budgets[got.Exceeded].scopeType)
		}
		for _, i := range got.Missing {
			spent := taskSpent
			if i != taskIdx {
				spent, err = s.spentUnits(ctx, c.Scope.OrgID, res.budgets[i], now)
			}
			if err == nil {
				err = s.counter.Seed(ctx, res.buckets[i], spent)
			}
			if err != nil {
				return s.unavailable(ctx, res, blocking, err)
			}
		}
	}
	return s.unavailable(ctx, res, blocking, errors.New("budget: bộ đếm không khởi tạo được"))
}

// unavailable: bộ đếm không dùng được. Có budget chặn cứng thì từ chối (fail-closed);
// chỉ có budget cảnh báo thì cho qua, không đặt trước.
func (s *Service) unavailable(ctx context.Context, res *Reservation, blocking bool, cause error) (*Reservation, error) {
	slog.ErrorContext(ctx, "budget: bộ đếm không sẵn sàng", "error", cause, "org_id", res.orgID, "blocking", blocking)
	if blocking {
		return nil, ErrUnavailable
	}
	res.buckets, res.budgets = nil, nil
	return res, nil
}

// Settle quyết toán một lời gọi: bỏ phần đặt trước và cộng chi phí thật (tính từ token
// provider báo) vào mọi bộ đếm. Lời gọi không có kết quả thì usage rỗng: hoàn toàn bộ.
// Lỗi ở đây không làm hỏng lời gọi đã xong: bộ đếm dựng lại được từ ledger.
func (s *Service) Settle(ctx context.Context, res *Reservation, u Usage) {
	if res == nil || len(res.buckets) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	actual := Actual(res.price, u)
	spent, err := s.counter.Settle(ctx, res.id, res.buckets, actual)
	if err != nil {
		// Thử lại một lần: bỏ lỡ lần quyết toán này thì bộ đếm thiếu khoản đó tới khi
		// được dựng lại từ ledger.
		time.Sleep(200 * time.Millisecond)
		spent, err = s.counter.Settle(ctx, res.id, res.buckets, actual)
	}
	if err != nil {
		slog.ErrorContext(ctx, "budget: không quyết toán được; bộ đếm lệch thấp so với chi phí thật cho tới khi dựng lại từ ledger",
			"error", err, "org_id", res.orgID, "unsettled", FormatAmount(actual))
		return
	}
	for i, a := range res.budgets {
		if a.softPct == 0 || a.scopeType == "execution" || i >= len(spent) || spent[i]*100 < a.limit*int64(a.softPct) {
			continue
		}
		s.alert(ctx, res.orgID, a, res.buckets[i], spent[i])
	}
}

// alert ghi một lần mỗi kỳ việc budget vượt ngưỡng mềm. Webhook / email thuộc phase sau.
func (s *Service) alert(ctx context.Context, orgID uuid.UUID, a applicable, b Bucket, spent int64) {
	first, err := s.counter.Once(ctx, b.Key+":soft", time.Until(b.ExpireAt))
	if err != nil || !first {
		return
	}
	slog.WarnContext(ctx, "budget: vượt ngưỡng mềm", "org_id", orgID, "budget_id", a.id, "scope_type", a.scopeType)
	ctx = audit.WithActor(tenancy.WithOrg(ctx, orgID), audit.Actor{Type: audit.ActorSystem})
	err = tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) error {
		return audit.Record(ctx, tx, audit.Event{OrgID: orgID, Action: "budget.soft_threshold", ResourceType: "budget", ResourceID: &a.id,
			Outcome: audit.OutcomeSuccess, Details: map[string]string{"scope_type": a.scopeType, "scope_id": a.scopeID.String(),
				"period": a.period, "limit": FormatAmount(a.limit), "spent": FormatAmount(spent), "threshold_pct": strconv.Itoa(a.softPct)}})
	})
	if err != nil {
		slog.ErrorContext(ctx, "budget: không ghi được audit ngưỡng mềm", "error", err)
	}
}
