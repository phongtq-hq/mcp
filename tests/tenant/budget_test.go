package tenant_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	pgxv5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/hqgroups/mcp/internal/cost/budget"
	"github.com/hqgroups/mcp/internal/cost/pricing"
	"github.com/hqgroups/mcp/internal/platform/tenancy"
	apperr "github.com/hqgroups/mcp/internal/shared/errors"
	"github.com/hqgroups/mcp/internal/shared/ids"
	"github.com/hqgroups/mcp/internal/testutil/pgtest"
)

// TestBudgetsIsolation: bảng budgets cách ly theo tenant, và mỗi role chỉ có quyền nó cần.
func TestBudgetsIsolation(t *testing.T) {
	db := pgtest.Start(t)
	ctx := context.Background()
	orgA, orgB := ids.New(), ids.New()
	seed := func(sql string, args ...any) {
		t.Helper()
		if _, err := db.Admin.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	seed(`INSERT INTO organizations (id, slug, name) VALUES ($1,'a','A'), ($2,'b','B')`, orgA, orgB)
	budgets := map[uuid.UUID]uuid.UUID{orgA: ids.New(), orgB: ids.New()}
	for org, id := range budgets {
		seed(`INSERT INTO budgets (id, org_id, scope_type, scope_id, period, limit_amount) VALUES ($1,$2,'organization',$2,'monthly',10)`, id, org)
	}
	q := func(id uuid.UUID) string { return "'" + id.String() + "'" }

	api := db.PoolAs(t, "app_api", 1)
	err := tenancy.InTx(tenancy.WithOrg(ctx, orgA), api, func(tx pgxv5.Tx) error {
		var n, foreign int
		if err := tx.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE org_id = $1) FROM budgets`, orgB).Scan(&n, &foreign); err != nil {
			return err
		}
		if n != 1 || foreign != 0 {
			t.Errorf("org A thấy %d budget (%d của org B), muốn 1 và 0", n, foreign)
		}
		for name, stmt := range map[string]string{
			"sửa budget của org B":     `UPDATE budgets SET limit_amount = 0 WHERE id = ` + q(budgets[orgB]) + ` RETURNING id`,
			"xoá budget của org B":     `DELETE FROM budgets WHERE id = ` + q(budgets[orgB]) + ` RETURNING id`,
			"tạo budget khai org B":    `INSERT INTO budgets (org_id, scope_type, scope_id, period, limit_amount) VALUES (` + q(orgB) + `,'organization',` + q(orgB) + `,'daily',1) RETURNING id`,
			"budget org trỏ tới org B": `INSERT INTO budgets (org_id, scope_type, scope_id, period, limit_amount) VALUES (` + q(orgA) + `,'organization',` + q(orgB) + `,'daily',1) RETURNING id`,
			"trùng phạm vi và chu kỳ":  `INSERT INTO budgets (org_id, scope_type, scope_id, period, limit_amount) VALUES (` + q(orgA) + `,'organization',` + q(orgA) + `,'monthly',1) RETURNING id`,
			"hạn mức âm":               `UPDATE budgets SET limit_amount = -1 WHERE id = ` + q(budgets[orgA]) + ` RETURNING id`,
			"đổi phạm vi của budget":   `UPDATE budgets SET scope_type = 'project' WHERE id = ` + q(budgets[orgA]) + ` RETURNING id`,
			"đổi chu kỳ của budget":    `UPDATE budgets SET period = 'total' WHERE id = ` + q(budgets[orgA]) + ` RETURNING id`,
			"chuyển budget sang org B": `UPDATE budgets SET org_id = ` + q(orgB) + ` WHERE id = ` + q(budgets[orgA]) + ` RETURNING id`,
		} {
			if _, err := tx.Exec(ctx, "SAVEPOINT s"); err != nil {
				return err
			}
			rows, err := tx.Query(ctx, stmt)
			affected := 0
			if err == nil {
				for rows.Next() {
					affected++
				}
				err = rows.Err()
				rows.Close()
			}
			if err == nil && affected > 0 {
				t.Errorf("%s: tác động %d dòng", name, affected)
			}
			if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT s"); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// gateway chỉ đọc budget của org trong context; không ghi được, và chỉ đọc được các
	// cột chi phí của usage.
	err = tenancy.InTx(tenancy.WithOrg(ctx, orgA), db.PoolAs(t, "app_gateway", 1), func(tx pgxv5.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM budgets`).Scan(&n); err != nil || n != 1 {
			t.Errorf("app_gateway: đọc budget: %d dòng, err %v", n, err)
		}
		for _, stmt := range []string{
			`UPDATE budgets SET enabled = false`,
			`DELETE FROM budgets`,
			`INSERT INTO budgets (org_id, scope_type, scope_id, period, limit_amount) VALUES (` + q(orgA) + `,'organization',` + q(orgA) + `,'daily',1)`,
			`SELECT input_tokens FROM usage_records LIMIT 1`,
			`SELECT task_id FROM usage_records LIMIT 1`,
		} {
			if _, err := tx.Exec(ctx, "SAVEPOINT s"); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, stmt)
			var pgErr *pgconn.PgError
			if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "42501" {
				t.Errorf("app_gateway: %q → %v, muốn bị từ chối vì thiếu quyền", stmt, err)
			}
			if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT s"); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// worker không đọc được cấu hình budget hay phân bố chi tiêu (00018); app_admin chỉ đọc.
	for role, stmts := range map[string][]string{
		"app_worker": {`SELECT id FROM budgets LIMIT 1`, `SELECT project_id FROM usage_records LIMIT 1`, `SELECT occurred_at FROM usage_records LIMIT 1`},
		"app_admin":  {`UPDATE budgets SET enabled = false`, `DELETE FROM budgets`},
	} {
		err := tenancy.InTx(tenancy.WithOrg(ctx, orgA), db.PoolAs(t, role, 1), func(tx pgxv5.Tx) error {
			for _, stmt := range stmts {
				if _, err := tx.Exec(ctx, "SAVEPOINT s"); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, stmt)
				var pgErr *pgconn.PgError
				if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "42501" {
					t.Errorf("%s: %q → %v, muốn bị từ chối vì thiếu quyền", role, stmt, err)
				}
				if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT s"); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

type fixedPrice struct{}

func (fixedPrice) PriceAt(context.Context, uuid.UUID, time.Time) (pricing.Pricing, error) {
	return pricing.Pricing{Currency: "USD", InputPerMtok: "3", OutputPerMtok: "15"}, nil
}

type otherCurrency struct{}

func (otherCurrency) PriceAt(context.Context, uuid.UUID, time.Time) (pricing.Pricing, error) {
	return pricing.Pricing{Currency: "EUR", InputPerMtok: "3", OutputPerMtok: "15"}, nil
}

// brokenCounter là bộ đếm không sẵn sàng (Redis mất).
type brokenCounter struct{}

var errDown = errors.New("redis: connection refused")

func (brokenCounter) Reserve(context.Context, string, []budget.Bucket, int64, time.Duration) (budget.Reserved, error) {
	return budget.Reserved{}, errDown
}
func (brokenCounter) Seed(context.Context, budget.Bucket, int64) error { return errDown }
func (brokenCounter) Settle(context.Context, string, []budget.Bucket, int64) ([]int64, error) {
	return nil, errDown
}
func (brokenCounter) Once(context.Context, string, time.Duration) (bool, error) {
	return false, errDown
}

// TestBudgetGate: việc đặt trước dưới role của Gateway — dựng lại bộ đếm từ ledger,
// fail-closed khi bộ đếm không sẵn sàng, và cách ly giữa các org.
func TestBudgetGate(t *testing.T) {
	db := pgtest.Start(t)
	ctx := context.Background()
	orgA, orgB := ids.New(), ids.New()
	projA, projB, agent, model := ids.New(), ids.New(), ids.New(), ids.New()
	seed := func(sql string, args ...any) {
		t.Helper()
		if _, err := db.Admin.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	seed(`INSERT INTO organizations (id, slug, name) VALUES ($1,'a','A'), ($2,'b','B')`, orgA, orgB)
	seed(`INSERT INTO projects (id, org_id, slug, name) VALUES ($1,$2,'p','P'), ($3,$4,'p','P')`, projA, orgA, projB, orgB)
	gw := db.PoolAs(t, "app_gateway", 2)
	call := func(org, project uuid.UUID) budget.Call {
		// 3000 byte ≈ 1500 token vào (0.0045) + 100 token ra (0.0015) = 0.006 USD.
		return budget.Call{Scope: budget.Scope{OrgID: org, ProjectID: project, AgentID: agent}, ModelID: model, InputBytes: 3000, MaxOutputTokens: 100}
	}
	code := func(err error) string {
		if e, ok := apperr.From(err); ok {
			return e.Code
		}
		return ""
	}

	// Org A đã tiêu 0.006 trong ledger; budget tháng 0.01 của org A.
	seed(`INSERT INTO usage_records (org_id, project_id, kind, estimated_cost) VALUES ($1,$2,'llm',0.006)`, orgA, projA)
	// Org B tiêu rất nhiều: không được tính vào bộ đếm của org A.
	seed(`INSERT INTO usage_records (org_id, project_id, kind, estimated_cost) VALUES ($1,$2,'llm',100)`, orgB, projB)
	seed(`INSERT INTO budgets (org_id, scope_type, scope_id, period, limit_amount) VALUES ($1,'organization',$1,'monthly',0.01)`, orgA)

	t.Run("a fresh counter is rebuilt from the ledger of the same tenant", func(t *testing.T) {
		svc := budget.NewService(gw, nil, fixedPrice{}, budget.NewMemoryCounter())
		// 0.006 (ledger) + 0.006 > 0.01: bị từ chối ngay ở lần đầu, dù bộ đếm vừa được tạo.
		if _, err := svc.Reserve(ctx, call(orgA, projA)); code(err) != budget.CodeExceeded {
			t.Errorf("bộ đếm mới không dựng lại từ ledger: err = %v", err)
		}
		// Org B không có budget: lời gọi của nó không bị ảnh hưởng bởi budget của org A.
		res, err := svc.Reserve(ctx, call(orgB, projB))
		if err != nil {
			t.Fatalf("org B: %v", err)
		}
		svc.Settle(ctx, res, budget.Usage{InputTokens: 1000})
		// Lời gọi nhỏ hơn thì qua: 0.006 + (0.00045 + 0.0015) ≤ 0.01. Đặt trước giữ chỗ cho tới khi quyết toán.
		small := call(orgA, projA)
		small.InputBytes = 300
		first, err := svc.Reserve(ctx, small)
		if err != nil {
			t.Fatalf("lời gọi nhỏ: %v", err)
		}
		if _, err := svc.Reserve(ctx, small); err != nil {
			t.Fatalf("lời gọi nhỏ thứ hai: %v", err)
		}
		if _, err := svc.Reserve(ctx, small); code(err) != budget.CodeExceeded {
			t.Errorf("lời gọi thứ ba phải bị chặn bởi hai phần đang đặt trước: %v", err)
		}
		// Quyết toán lượt đầu với mức thật nhỏ hơn phần đặt trước: chỗ được hoàn lại.
		svc.Settle(ctx, first, budget.Usage{InputTokens: 10})
		if _, err := svc.Reserve(ctx, small); err != nil {
			t.Errorf("sau khi quyết toán vẫn bị chặn: %v", err)
		}
		// Budget của task: mức đã tiêu cộng trần ước lượng vượt hạn mức thì chặn.
		over := call(orgB, projB)
		over.Task = &budget.TaskBudget{ID: ids.New(), Limit: "0.01", Spent: "0.009"}
		if _, err := svc.Reserve(ctx, over); code(err) != budget.CodeExceeded {
			t.Errorf("budget của task: %v", err)
		}
		// Hai lượt gọi đồng thời của một task không cùng lọt qua budget của nó, kể cả khi
		// mức đã tiêu bên gọi đưa (do runtime ghi) chưa kịp tăng.
		task := &budget.TaskBudget{ID: ids.New(), Limit: "0.008", Spent: "0"}
		one, two := call(orgB, projB), call(orgB, projB)
		one.Task, two.Task = task, task
		held, err := svc.Reserve(ctx, one)
		if err != nil {
			t.Fatalf("lượt đầu của task: %v", err)
		}
		if _, err := svc.Reserve(ctx, two); code(err) != budget.CodeExceeded {
			t.Errorf("lượt thứ hai đồng thời của task phải bị chặn: %v", err)
		}
		// Mức tiêu thật được bộ đếm ghi nhận: lượt sau bị chặn dù bên gọi vẫn báo đã tiêu 0.
		svc.Settle(ctx, held, budget.Usage{InputTokens: 1500}) // 0.0045
		if _, err := svc.Reserve(ctx, two); code(err) != budget.CodeExceeded {
			t.Errorf("sau khi tiêu 0.0045 của 0.008, lượt cần thêm ~0.006 phải bị chặn: %v", err)
		}
		// Gọi lại cùng Key (lần trước chết giữa chừng): thay thế phần đặt trước cũ, không giữ chỗ hai lần.
		retry := call(orgB, projB)
		retry.Key = "task-x:model-y"
		retry.Task = &budget.TaskBudget{ID: ids.New(), Limit: "0.008", Spent: "0"}
		for i := 0; i < 3; i++ {
			if _, err := svc.Reserve(ctx, retry); err != nil {
				t.Fatalf("gọi lại cùng Key lần %d: %v", i+1, err)
			}
		}
		// Giá không tính bằng USD bị từ chối: bộ đếm và ledger chỉ cộng USD.
		if _, err := budget.NewService(gw, nil, otherCurrency{}, budget.NewMemoryCounter()).Reserve(ctx, call(orgB, projB)); err == nil {
			t.Error("giá không phải USD được chấp nhận")
		}
		// Không có bộ đếm (process worker): chỉ budget của task được kiểm, bằng mức bên gọi đưa.
		noCounter := budget.NewService(gw, nil, fixedPrice{}, nil)
		if _, err := noCounter.Reserve(ctx, call(orgA, projA)); err != nil {
			t.Errorf("không có bộ đếm mà vẫn kiểm budget theo phạm vi: %v", err)
		}
		if _, err := noCounter.Reserve(ctx, over); code(err) != budget.CodeExceeded {
			t.Errorf("không có bộ đếm: budget của task: %v", err)
		}
	})

	t.Run("fail-closed when the counter is unavailable and a blocking budget applies", func(t *testing.T) {
		svc := budget.NewService(gw, nil, fixedPrice{}, brokenCounter{})
		_, err := svc.Reserve(ctx, call(orgA, projA))
		if e, ok := apperr.From(err); !ok || e.Code != "budget_unavailable" || !apperr.IsRetryable(err) {
			t.Errorf("budget chặn cứng + bộ đếm hỏng: err = %v, muốn budget_unavailable (thử lại được)", err)
		}
		// Org B không có budget nào: bộ đếm không được dùng tới, lời gọi qua.
		if _, err := svc.Reserve(ctx, call(orgB, projB)); err != nil {
			t.Errorf("org không có budget bị chặn vì bộ đếm hỏng: %v", err)
		}
		// Chỉ có budget cảnh báo: cho qua (không chặn execution vì một bộ đếm phụ trợ hỏng).
		seed(`UPDATE budgets SET hard_action = 'alert_only' WHERE org_id = $1`, orgA)
		res, err := svc.Reserve(ctx, call(orgA, projA))
		if err != nil {
			t.Errorf("budget chỉ cảnh báo + bộ đếm hỏng: %v", err)
		}
		svc.Settle(ctx, res, budget.Usage{}) // không panic, không lỗi
	})
}
