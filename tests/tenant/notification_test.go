package tenant_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	pgxv5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/hqgroups/mcp/internal/platform/tenancy"
	"github.com/hqgroups/mcp/internal/shared/ids"
	"github.com/hqgroups/mcp/internal/testutil/pgtest"
)

// TestNotificationOutbox: outbox của thông báo webhook (migration 00035) — trigger xếp
// thông báo, cách ly tenant, quyền theo role, và hàm notification_claim là đường đọc
// xuyên tenant duy nhất, chỉ của gateway.
func TestNotificationOutbox(t *testing.T) {
	db := pgtest.Start(t)
	ctx := context.Background()
	orgA, orgB, projA, projB := ids.New(), ids.New(), ids.New(), ids.New()
	seed := func(sql string, args ...any) {
		t.Helper()
		if _, err := db.Admin.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := db.Admin.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	q := func(id uuid.UUID) string { return "'" + id.String() + "'" }
	seed(`INSERT INTO organizations (id, slug, name) VALUES ($1,'org-a','A'), ($2,'org-b','B')`, orgA, orgB)
	seed(`INSERT INTO projects (id, org_id, slug, name) VALUES ($1,$2,'p','P'), ($3,$4,'p','P')`, projA, orgA, projB, orgB)
	approval := func(org, project uuid.UUID) uuid.UUID {
		t.Helper()
		id := ids.New()
		seed(`INSERT INTO approvals (id, org_id, project_id, subject_type, payload, risk_level, requested_by_type, expires_at)
		      VALUES ($1,$2,$3,'remediation_plan','{}','write','system', now() + interval '1 hour')`, id, org, project)
		return id
	}
	// Chưa có webhook: approval không xếp thông báo.
	approval(orgA, projA)
	if n := count(`SELECT count(*) FROM notification_deliveries`); n != 0 {
		t.Fatalf("có %d thông báo khi chưa cấu hình webhook", n)
	}
	for _, o := range []uuid.UUID{orgA, orgB} {
		sec := ids.New()
		seed(`INSERT INTO secrets (id, org_id, name, kind, ciphertext, wrapped_dek, kek_ref) VALUES ($1,$2,'hook','webhook_signing_key','\x00','\x00','test')`, sec, o)
		seed(`INSERT INTO notification_webhooks (org_id, url, secret_id) VALUES ($1,'https://hooks.example.test/h',$2)`, o, sec)
	}
	// Secret của org khác không dùng được làm khoá ký (khoá ngoại ghép).
	if _, err := db.Admin.Exec(ctx, `UPDATE notification_webhooks SET secret_id = (SELECT id FROM secrets WHERE org_id = $2) WHERE org_id = $1`, orgA, orgB); err == nil {
		t.Fatal("webhook của org A trỏ được tới secret của org B")
	}
	var as []uuid.UUID
	for range 6 {
		as = append(as, approval(orgA, projA))
	}
	apB := approval(orgB, projB)
	if a, b := count(`SELECT count(*) FROM notification_deliveries WHERE org_id = $1`, orgA), count(`SELECT count(*) FROM notification_deliveries WHERE org_id = $1 AND approval_id = $2`, orgB, apB); a != 6 || b != 1 {
		t.Fatalf("thông báo: A = %d (muốn 6), B = %d (muốn 1)", a, b)
	}

	t.Run("tenant isolation under application roles", func(t *testing.T) {
		for _, role := range []string{"app_api", "app_gateway"} {
			err := tenancy.InTx(tenancy.WithOrg(ctx, orgA), db.PoolAs(t, role, 1), func(tx pgxv5.Tx) error {
				for _, table := range []string{"notification_webhooks", "notification_deliveries"} {
					var n, foreign int
					if err := tx.QueryRow(ctx, "SELECT count(*), count(*) FILTER (WHERE org_id = $1) FROM "+table, orgB).Scan(&n, &foreign); err != nil {
						return err
					}
					if n == 0 || foreign != 0 {
						t.Errorf("%s đọc %s: %d dòng, %d của org khác", role, table, n, foreign)
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		// Không đặt tenant thì không thấy gì.
		err := pgxv5.BeginFunc(ctx, db.PoolAs(t, "app_gateway", 1), func(tx pgxv5.Tx) error {
			var n int
			if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM notification_deliveries) + (SELECT count(*) FROM notification_webhooks)`).Scan(&n); err != nil {
				return err
			}
			if n != 0 {
				t.Errorf("gateway đọc thẳng được %d dòng khi không có tenant", n)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	t.Run("privileges by role", func(t *testing.T) {
		insert := `INSERT INTO notification_deliveries (org_id, event_type, approval_id) VALUES (` + q(orgA) + `,'approval.requested',` + q(as[0]) + `)`
		for role, stmts := range map[string][]string{
			"app_api": {
				`SELECT * FROM notification_claim(1)`,
				insert,
				`UPDATE notification_deliveries SET status = 'delivered', finished_at = now()`,
				`DELETE FROM notification_deliveries`,
			},
			"app_gateway": {
				insert,
				`DELETE FROM notification_deliveries`,
				`UPDATE notification_deliveries SET attempts = 0`,
				`UPDATE notification_deliveries SET next_attempt_at = now()`,
				`UPDATE notification_webhooks SET url = 'https://evil.example.test/h'`,
				`UPDATE notification_webhooks SET secret_id = secret_id`,
				`DELETE FROM notification_webhooks`,
				`INSERT INTO notification_webhooks (org_id, url, secret_id) VALUES (` + q(orgA) + `,'https://x.example.test/h',` + q(ids.New()) + `)`,
			},
			"app_worker": {
				`SELECT * FROM notification_claim(1)`,
				`SELECT 1 FROM notification_deliveries LIMIT 1`,
				`SELECT 1 FROM notification_webhooks LIMIT 1`,
				insert,
			},
		} {
			err := tenancy.InTx(tenancy.WithOrg(ctx, orgA), db.PoolAs(t, role, 1), func(tx pgxv5.Tx) error {
				for _, stmt := range stmts {
					if _, err := tx.Exec(ctx, "SAVEPOINT s"); err != nil {
						return err
					}
					_, err := tx.Exec(ctx, stmt)
					var pgErr *pgconn.PgError
					if err == nil || !errors.As(err, &pgErr) || pgErr.Code != insufficientPrivilege {
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
	})

	t.Run("claim leases rows across orgs, at most four per org per batch", func(t *testing.T) {
		gw := db.PoolAs(t, "app_gateway", 1)
		claim := func(limit int) map[uuid.UUID]int {
			t.Helper()
			per := map[uuid.UUID]int{}
			err := pgxv5.BeginFunc(ctx, gw, func(tx pgxv5.Tx) error {
				rows, err := tx.Query(ctx, `SELECT org_id, attempts FROM notification_claim($1)`, limit)
				if err != nil {
					return err
				}
				defer rows.Close()
				for rows.Next() {
					var org uuid.UUID
					var attempts int
					if err := rows.Scan(&org, &attempts); err != nil {
						return err
					}
					if attempts != 1 {
						t.Errorf("attempts = %d sau lần nhận đầu", attempts)
					}
					per[org]++
				}
				return rows.Err()
			})
			if err != nil {
				t.Fatal(err)
			}
			return per
		}
		got := claim(64)
		if got[orgA] != 4 || got[orgB] != 1 {
			t.Fatalf("lô đầu: %v, muốn A = 4, B = 1", got)
		}
		// Dòng đã thuê không được nhận lại trước hạn; hai dòng còn lại của A thì có.
		if got = claim(64); got[orgA] != 2 || got[orgB] != 0 {
			t.Fatalf("lô hai: %v, muốn A = 2, B = 0", got)
		}
		if got = claim(64); len(got) != 0 {
			t.Fatalf("lô ba: %v, muốn rỗng", got)
		}
		for _, bad := range []int{0, 65} {
			if _, err := gw.Exec(ctx, `SELECT notification_claim($1)`, bad); err == nil {
				t.Errorf("notification_claim(%d) được chấp nhận", bad)
			}
		}
		// Hết lượt thử: hàm tự đóng dòng thay vì trả nó.
		seed(`UPDATE notification_deliveries SET attempts = 6, next_attempt_at = now() - interval '1 second' WHERE org_id = $1`, orgB)
		if got = claim(64); got[orgB] != 0 {
			t.Fatalf("dòng đã hết lượt vẫn được trả: %v", got)
		}
		if n := count(`SELECT count(*) FROM notification_deliveries WHERE org_id = $1 AND status = 'failed' AND last_error_code = 'attempts_exhausted'`, orgB); n != 1 {
			t.Fatalf("dòng hết lượt chưa được đóng failed (%d)", n)
		}
	})

	t.Run("lifecycle guard holds for every role", func(t *testing.T) {
		for _, stmt := range []string{
			`UPDATE notification_deliveries SET status = 'pending', finished_at = NULL WHERE status = 'failed'`,
			`UPDATE notification_deliveries SET attempts = 0 WHERE status = 'pending'`,
			`UPDATE notification_deliveries SET approval_id = ` + q(apB) + ` WHERE org_id = ` + q(orgA),
			`INSERT INTO notification_deliveries (org_id, event_type, approval_id, status, finished_at) VALUES (` + q(orgB) + `,'approval.requested',` + q(approval(orgB, projB)) + `,'delivered', now())`,
		} {
			_, err := db.Admin.Exec(ctx, stmt)
			var pgErr *pgconn.PgError
			if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "23001" {
				t.Errorf("%q → %v, muốn restrict_violation", stmt, err)
			}
		}
	})

	t.Run("an org with a deep backlog does not starve other orgs", func(t *testing.T) {
		// Org A: 100 thông báo tới hạn, cũ hơn mọi dòng của org B.
		for range 100 {
			approval(orgA, projA)
		}
		seed(`UPDATE notification_deliveries SET next_attempt_at = now() - interval '1 hour' WHERE org_id = $1 AND status = 'pending'`, orgA)
		seed(`UPDATE notification_webhooks SET enabled = true WHERE org_id = $1`, orgB)
		approval(orgB, projB)
		var a, b int
		err := pgxv5.BeginFunc(ctx, db.PoolAs(t, "app_gateway", 1), func(tx pgxv5.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE org_id = $1), count(*) FILTER (WHERE org_id = $2) FROM notification_claim(16)`, orgA, orgB).Scan(&a, &b)
		})
		if err != nil {
			t.Fatal(err)
		}
		// B còn dòng chờ từ các ca trước: mọi dòng của B phải nằm trong lô đầu.
		if a != 4 || b < 1 {
			t.Fatalf("lô đầu: A = %d, B = %d; muốn A = 4, B ≥ 1", a, b)
		}
	})

	t.Run("a disabled webhook and a full outbox enqueue nothing", func(t *testing.T) {
		seed(`UPDATE notification_webhooks SET enabled = false WHERE org_id = $1`, orgB)
		before := count(`SELECT count(*) FROM notification_deliveries WHERE org_id = $1`, orgB)
		approval(orgB, projB)
		if n := count(`SELECT count(*) FROM notification_deliveries WHERE org_id = $1`, orgB); n != before {
			t.Fatalf("webhook đã tắt vẫn xếp thông báo (%d → %d)", before, n)
		}
		// Outbox của org A đầy (500 dòng chờ): approval vẫn được tạo, chỉ không có thông báo.
		for range 420 {
			approval(orgA, projA)
		}
		if n := count(`SELECT count(*) FROM notification_deliveries WHERE org_id = $1 AND status = 'pending'`, orgA); n != 500 {
			t.Fatalf("org A có %d thông báo chờ, muốn đúng trần 500", n)
		}
	})
}
