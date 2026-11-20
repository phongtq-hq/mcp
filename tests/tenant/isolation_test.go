// Package tenant_test kiểm tra cách ly tenant ở tầng database (ADR-0005).
// Đây là khung cho bộ test cross-tenant: mỗi bảng thuộc tenant mới phải được
// thêm vào đây, và mỗi endpoint mới phải có test truy cập chéo tương ứng.
package tenant_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	pgxv5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hqgroups/mcp/internal/adminops"
	"github.com/hqgroups/mcp/internal/audit"
	"github.com/hqgroups/mcp/internal/platform/tenancy"
	"github.com/hqgroups/mcp/internal/shared/ids"
	sharedpgx "github.com/hqgroups/mcp/internal/shared/pgx"
	"github.com/hqgroups/mcp/internal/testutil/pgtest"
)

const insufficientPrivilege = "42501" // cũng là mã của vi phạm RLS WITH CHECK

type fixture struct {
	admin      *pgxpool.Pool
	app        *pgxpool.Pool // role app_api, một connection để lộ rò rỉ tenant giữa các lần dùng
	orgA, orgB uuid.UUID
	projectA   uuid.UUID
}

func setup(t *testing.T) fixture {
	t.Helper()
	db := pgtest.Start(t)
	ctx := context.Background()
	f := fixture{
		admin: db.Admin,
		app:   db.PoolAs(t, "app_api", 1),
		orgA:  ids.New(), orgB: ids.New(), projectA: ids.New(),
	}
	for _, o := range []struct {
		id   uuid.UUID
		slug string
	}{{f.orgA, "org-a"}, {f.orgB, "org-b"}} {
		if _, err := f.admin.Exec(ctx,
			`INSERT INTO organizations (id, slug, name) VALUES ($1, $2, $2)`, o.id, o.slug); err != nil {
			t.Fatalf("seed org: %v", err)
		}
	}
	err := tenancy.InTx(tenancy.WithOrg(ctx, f.orgA), f.app, func(tx pgxv5.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO projects (id, org_id, slug, name) VALUES ($1, $2, 'p1', 'P1')`, f.projectA, f.orgA)
		return err
	})
	if err != nil {
		t.Fatalf("seed project as org A: %v", err)
	}
	return f
}

func countProjects(t *testing.T, ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgxv5.Row
}) int {
	t.Helper()
	var n int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM projects`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func TestTenantIsolation(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	asA := tenancy.WithOrg(ctx, f.orgA)
	asB := tenancy.WithOrg(ctx, f.orgB)

	t.Run("owner sees own rows", func(t *testing.T) {
		_ = tenancy.InTx(asA, f.app, func(tx pgxv5.Tx) error {
			if n := countProjects(t, ctx, tx); n != 1 {
				t.Errorf("org A thấy %d project, muốn 1", n)
			}
			return nil
		})
	})

	t.Run("other tenant sees nothing", func(t *testing.T) {
		_ = tenancy.InTx(asB, f.app, func(tx pgxv5.Tx) error {
			if n := countProjects(t, ctx, tx); n != 0 {
				t.Errorf("org B thấy %d project của org A, muốn 0", n)
			}
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM organizations`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 1 {
				t.Errorf("org B thấy %d organization, muốn đúng 1 (chính nó)", n)
			}
			return nil
		})
	})

	t.Run("other tenant cannot update or archive", func(t *testing.T) {
		_ = tenancy.InTx(asB, f.app, func(tx pgxv5.Tx) error {
			tag, err := tx.Exec(ctx, `UPDATE projects SET name = 'hijacked' WHERE id = $1`, f.projectA)
			if err != nil {
				t.Fatal(err)
			}
			if tag.RowsAffected() != 0 {
				t.Errorf("org B sửa được %d dòng của org A", tag.RowsAffected())
			}
			return nil
		})
		var name string
		if err := f.admin.QueryRow(ctx, `SELECT name FROM projects WHERE id = $1`, f.projectA).Scan(&name); err != nil {
			t.Fatal(err)
		}
		if name != "P1" {
			t.Errorf("tên project bị đổi thành %q", name)
		}
	})

	t.Run("cannot insert row for another tenant", func(t *testing.T) {
		err := tenancy.InTx(asB, f.app, func(tx pgxv5.Tx) error {
			_, err := tx.Exec(ctx,
				`INSERT INTO projects (org_id, slug, name) VALUES ($1, 'evil', 'Evil')`, f.orgA)
			return err
		})
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != insufficientPrivilege {
			t.Errorf("muốn lỗi RLS %s, nhận %v", insufficientPrivilege, err)
		}
	})

	t.Run("cannot move own row to another tenant", func(t *testing.T) {
		err := tenancy.InTx(asA, f.app, func(tx pgxv5.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE projects SET org_id = $1 WHERE id = $2`, f.orgB, f.projectA)
			return err
		})
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != insufficientPrivilege {
			t.Errorf("muốn lỗi RLS %s, nhận %v", insufficientPrivilege, err)
		}
	})

	t.Run("no tenant set means no rows, not all rows", func(t *testing.T) {
		if n := countProjects(t, ctx, f.app); n != 0 {
			t.Errorf("truy vấn không đặt tenant thấy %d dòng, muốn 0", n)
		}
	})

	t.Run("tenant does not leak across pooled connection", func(t *testing.T) {
		// Pool chỉ có một connection: truy vấn này dùng lại đúng connection vừa
		// phục vụ org A. app.org_id phải đã hết hiệu lực cùng transaction.
		_ = tenancy.InTx(asA, f.app, func(tx pgxv5.Tx) error { return nil })
		if n := countProjects(t, ctx, f.app); n != 0 {
			t.Errorf("tenant rò sang lần dùng connection kế tiếp: thấy %d dòng", n)
		}
	})

	t.Run("rollback on error", func(t *testing.T) {
		boom := errors.New("boom")
		err := tenancy.InTx(asA, f.app, func(tx pgxv5.Tx) error {
			if _, err := tx.Exec(ctx,
				`INSERT INTO projects (org_id, slug, name) VALUES ($1, 'tmp', 'Tmp')`, f.orgA); err != nil {
				t.Fatal(err)
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("muốn lỗi boom, nhận %v", err)
		}
		if n := countProjects(t, ctx, f.admin); n != 1 {
			t.Errorf("sau rollback có %d project, muốn 1", n)
		}
	})

	t.Run("missing tenant in context is refused", func(t *testing.T) {
		called := false
		err := tenancy.InTx(ctx, f.app, func(pgxv5.Tx) error { called = true; return nil })
		if !errors.Is(err, tenancy.ErrNoTenant) || called {
			t.Errorf("muốn ErrNoTenant và không chạy fn; nhận err=%v called=%v", err, called)
		}
	})
}

func TestApplicationRolesCannotBypassRLS(t *testing.T) {
	db := pgtest.Start(t)
	ctx := context.Background()

	rows, err := db.Admin.Query(ctx,
		`SELECT rolname, rolbypassrls, rolsuper FROM pg_roles
		 WHERE rolname IN ('app_api','app_gateway','app_worker') ORDER BY rolname`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var name string
		var bypass, super bool
		if err := rows.Scan(&name, &bypass, &super); err != nil {
			t.Fatal(err)
		}
		seen++
		if bypass || super {
			t.Errorf("role %s có bypassrls=%v superuser=%v", name, bypass, super)
		}
	}
	if seen != 3 {
		t.Errorf("thấy %d role ứng dụng, muốn 3", seen)
	}
}

// Mọi bảng trong schema public có cột org_id phải bật và FORCE RLS và có policy.
// Test này bắt migration mới quên cách ly tenant.
func TestEveryTenantTableHasForcedRLS(t *testing.T) {
	db := pgtest.Start(t)
	ctx := context.Background()

	rows, err := db.Admin.Query(ctx, `
		SELECT c.relname, c.relrowsecurity, c.relforcerowsecurity,
		       (SELECT count(*) FROM pg_policy p WHERE p.polrelid = c.oid)
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind IN ('r','p')
		  AND (c.relname = 'organizations' OR EXISTS (
		        SELECT 1 FROM pg_attribute a
		        WHERE a.attrelid = c.oid AND a.attname = 'org_id' AND NOT a.attisdropped))
		ORDER BY c.relname`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var name string
		var enabled, forced bool
		var policies int
		if err := rows.Scan(&name, &enabled, &forced, &policies); err != nil {
			t.Fatal(err)
		}
		seen++
		if !enabled || !forced || policies == 0 {
			t.Errorf("bảng %s: rls=%v force=%v policies=%d", name, enabled, forced, policies)
		}
	}
	if seen < 2 {
		t.Errorf("chỉ thấy %d bảng tenant; truy vấn kiểm tra có thể đã sai", seen)
	}
}

// Policy own_memberships: ở bước xác thực (chỉ đặt app.user_id, chưa có tenant)
// một user đọc được membership của chính mình ở mọi org, và không gì khác.
func TestUserScopedTransactionSeesOnlyOwnMemberships(t *testing.T) {
	db := pgtest.Start(t)
	ctx := context.Background()
	app := db.PoolAs(t, "app_api", 1)
	orgA, orgB, alice, bob := ids.New(), ids.New(), ids.New(), ids.New()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO organizations (id, slug, name) VALUES ($1,'a','A'), ($2,'b','B')`, []any{orgA, orgB}},
		{`INSERT INTO users (id, idp_issuer, idp_subject, email) VALUES ($1,'iss','alice','a@x'), ($2,'iss','bob','b@x')`, []any{alice, bob}},
		{`INSERT INTO memberships (org_id, user_id) VALUES ($1,$3), ($2,$3), ($2,$4)`, []any{orgA, orgB, alice, bob}},
		{`INSERT INTO projects (org_id, slug, name) VALUES ($1,'p','P')`, []any{orgA}},
		{`INSERT INTO role_bindings (org_id, role_id, principal_type, principal_id)
		  SELECT $1, id, 'user', $2 FROM roles WHERE name = 'org_admin'`, []any{orgA, alice}},
	} {
		if _, err := db.Admin.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	err := sharedpgx.WithUserTx(ctx, app, alice, func(tx pgxv5.Tx) error {
		count := func(table string) (n int) {
			if err := tx.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
				t.Fatalf("count %s: %v", table, err)
			}
			return n
		}
		if n := count("memberships"); n != 2 {
			t.Errorf("alice thấy %d membership, muốn 2 (của chính mình, không có của bob)", n)
		}
		for _, table := range []string{"projects", "organizations", "role_bindings"} {
			if n := count(table); n != 0 {
				t.Errorf("transaction chỉ có user thấy %d dòng trong %s, muốn 0", n, table)
			}
		}
		// Chỉ được ĐỌC: không sửa, không xoá được membership qua policy này.
		for _, stmt := range []string{
			`UPDATE memberships SET status = 'removed' WHERE user_id = $1`,
			`DELETE FROM memberships WHERE user_id = $1`,
		} {
			if _, err := tx.Exec(ctx, "SAVEPOINT s"); err != nil {
				t.Fatal(err)
			}
			tag, err := tx.Exec(ctx, stmt, alice)
			if err == nil && tag.RowsAffected() != 0 {
				t.Errorf("%q tác động %d dòng trong transaction chỉ có user", stmt, tag.RowsAffected())
			}
			if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT s"); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Role dựng sẵn: tenant đọc được, không ghi được.
	err = tenancy.InTx(tenancy.WithOrg(ctx, orgA), app, func(tx pgxv5.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM roles WHERE org_id IS NULL`).Scan(&n); err != nil {
			return err
		}
		if n < 7 {
			t.Errorf("tenant thấy %d role dựng sẵn, muốn ít nhất 7", n)
		}
		// Câu lệnh bị từ chối làm transaction bị huỷ, nên trả lỗi ra để rollback.
		tag, err := tx.Exec(ctx, `UPDATE roles SET permissions = '{organization:update}' WHERE name = 'viewer'`)
		if err == nil && tag.RowsAffected() != 0 {
			t.Error("role ứng dụng sửa được role dựng sẵn")
		}
		return errRollback
	})
	if !errors.Is(err, errRollback) {
		t.Fatal(err)
	}
	var perms []string
	if err := db.Admin.QueryRow(ctx, `SELECT permissions FROM roles WHERE name = 'viewer'`).Scan(&perms); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(perms, "organization:update") {
		t.Errorf("permissions của viewer bị đổi: %v", perms)
	}
}

var errRollback = errors.New("rollback")

// Cách ly tenant trên các bảng phân quyền, và các ràng buộc mà RLS không tự chặn được.
func TestAuthzTablesTenantIsolation(t *testing.T) {
	db := pgtest.Start(t)
	ctx := context.Background()
	app := db.PoolAs(t, "app_api", 1)
	orgA, orgB, user, roleB, projB := ids.New(), ids.New(), ids.New(), ids.New(), ids.New()
	seed := func(sql string, args ...any) {
		t.Helper()
		if _, err := db.Admin.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	seed(`INSERT INTO organizations (id, slug, name) VALUES ($1,'a','A'), ($2,'b','B')`, orgA, orgB)
	seed(`INSERT INTO users (id, idp_issuer, idp_subject, email) VALUES ($1,'iss','u','u@x')`, user)
	seed(`INSERT INTO memberships (org_id, user_id) VALUES ($1,$2)`, orgB, user)
	seed(`INSERT INTO roles (id, org_id, name, permissions) VALUES ($1,$2,'b_custom','{organization:update}')`, roleB, orgB)
	seed(`INSERT INTO projects (id, org_id, slug, name) VALUES ($1,$2,'pb','PB')`, projB, orgB)
	seed(`INSERT INTO role_bindings (org_id, role_id, role_scope_org_id, principal_type, principal_id)
	      VALUES ($1,$2,$1,'user',$3)`, orgB, roleB, user)

	// Org A không thấy membership, role tuỳ biến hay binding nào của org B.
	err := tenancy.InTx(tenancy.WithOrg(ctx, orgA), app, func(tx pgxv5.Tx) error {
		for table, where := range map[string]string{
			"memberships": "true", "role_bindings": "true", "roles": "org_id IS NOT NULL",
		} {
			var n int
			if err := tx.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE "+where).Scan(&n); err != nil {
				return err
			}
			if n != 0 {
				t.Errorf("org A thấy %d dòng của org B trong %s", n, table)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Ràng buộc ở mức database, kiểm bằng app_admin (không chịu RLS, không bị chặn
	// bởi GRANT): đây là lớp còn lại khi code và RLS đều sai.
	admin := func(name, sql string, args ...any) {
		t.Helper()
		tx, err := db.Admin.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_admin"); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, sql, args...); err == nil {
			t.Errorf("%s: database chấp nhận, muốn bị từ chối", name)
		}
	}
	admin("binding của org A trỏ tới role tuỳ biến của org B (khai phạm vi platform)",
		`INSERT INTO role_bindings (org_id, role_id, principal_type, principal_id) VALUES ($1,$2,'user',$3)`,
		orgA, roleB, user)
	admin("binding của org A trỏ tới role tuỳ biến của org B (khai phạm vi org B)",
		`INSERT INTO role_bindings (org_id, role_id, role_scope_org_id, principal_type, principal_id) VALUES ($1,$2,$3,'user',$4)`,
		orgA, roleB, orgB, user)
	admin("binding của org A trỏ tới project của org B",
		`INSERT INTO role_bindings (org_id, role_id, principal_type, principal_id, project_id)
		 SELECT $1, id, 'user', $2, $3 FROM roles WHERE name = 'viewer'`, orgA, user, projB)

	// Quyền theo cột: tenant không tự đổi được status / slug của organization,
	// environment của project, hay bất cứ gì trên users.
	err = tenancy.InTx(tenancy.WithOrg(ctx, orgB), app, func(tx pgxv5.Tx) error {
		for _, stmt := range []string{
			`UPDATE organizations SET status = 'active'`,
			`UPDATE organizations SET slug = 'renamed'`,
			`UPDATE projects SET environment = 'prod'`,
			`UPDATE users SET status = 'disabled'`,
			`INSERT INTO memberships (org_id, user_id) SELECT id, '` + user.String() + `' FROM organizations`,
			`DELETE FROM role_bindings`,
		} {
			if _, err := tx.Exec(ctx, "SAVEPOINT s"); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, stmt); err == nil {
				t.Errorf("app_api chạy được: %s", stmt)
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

func TestRequireRLSEnforced(t *testing.T) {
	db := pgtest.Start(t)
	ctx := context.Background()
	if err := sharedpgx.RequireRLSEnforced(ctx, db.Admin); err == nil {
		t.Error("kết nối superuser phải bị từ chối")
	}
	if err := sharedpgx.RequireRLSEnforced(ctx, db.PoolAs(t, "app_admin", 1)); err == nil {
		t.Error("role BYPASSRLS phải bị từ chối")
	}
	for _, role := range []string{"app_api", "app_gateway", "app_worker"} {
		if err := sharedpgx.RequireRLSEnforced(ctx, db.PoolAs(t, role, 1)); err != nil {
			t.Errorf("%s: %v", role, err)
		}
	}
}

// audit_logs chỉ thêm, và chỉ qua audit_append: role ứng dụng không ghi trực tiếp,
// không sửa, không xoá được, kể cả qua phân vùng.
func TestAuditLogIsAppendOnlyAndTenantScoped(t *testing.T) {
	db := pgtest.Start(t)
	ctx := context.Background()
	app := db.PoolAs(t, "app_api", 1)
	orgA, orgB := ids.New(), ids.New()
	if _, err := db.Admin.Exec(ctx, `INSERT INTO organizations (id, slug, name) VALUES ($1,'a','A'), ($2,'b','B')`, orgA, orgB); err != nil {
		t.Fatal(err)
	}
	record := func(org uuid.UUID) {
		t.Helper()
		err := tenancy.InTx(tenancy.WithOrg(ctx, org), app, func(tx pgxv5.Tx) error {
			return audit.Record(ctx, tx, audit.Event{OrgID: org, Action: "test.event", ResourceType: "test", Outcome: audit.OutcomeSuccess})
		})
		if err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	record(orgA)
	record(orgA)
	record(orgB)

	// Thời điểm do database đặt, không ở tương lai, và không nằm ở phân vùng mặc định.
	var n, inDefault int
	var notFuture bool
	if err := db.Admin.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE tableoid = 'audit_logs_default'::regclass),
		bool_and(occurred_at <= clock_timestamp()) FROM audit_logs`).Scan(&n, &inDefault, &notFuture); err != nil {
		t.Fatal(err)
	}
	if n != 3 || inDefault != 0 || !notFuture {
		t.Errorf("n=%d inDefault=%d notFuture=%v", n, inDefault, notFuture)
	}

	var part string
	if err := db.Admin.QueryRow(ctx, `SELECT tableoid::regclass::text FROM audit_logs LIMIT 1`).Scan(&part); err != nil {
		t.Fatal(err)
	}
	err := tenancy.InTx(tenancy.WithOrg(ctx, orgA), app, func(tx pgxv5.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM audit_logs`).Scan(&n); err != nil {
			return err
		}
		if n != 2 {
			t.Errorf("org A thấy %d bản ghi audit, muốn 2 (không thấy của org B)", n)
		}
		if got, err := audit.VerifyChain(ctx, tx, orgA); err != nil || got != 2 {
			t.Errorf("verify: n=%d err=%v", got, err)
		}
		insert := `INSERT INTO audit_logs (id, org_id, actor_type, action, resource_type, outcome, hash, occurred_at)
			VALUES (gen_random_uuid(), '%s', 'system', 'forged', 'x', 'success', '\x00', now())`
		for _, stmt := range []string{
			`UPDATE audit_logs SET action = 'x'`,
			`DELETE FROM audit_logs`,
			`TRUNCATE audit_logs`,
			`UPDATE ` + part + ` SET action = 'x'`,
			`DELETE FROM ` + part,
			`SELECT 1 FROM ` + part,
			// Ghi trực tiếp, kể cả vào chính org của mình: chỉ audit_append được ghi.
			fmt.Sprintf(insert, orgA),
			fmt.Sprintf(insert, orgB),
			// Ghi qua hàm cho tenant khác với tenant của transaction.
			`SELECT audit_append('` + orgB.String() + `', NULL, 'system', NULL, NULL, 'forged', 'x', NULL, 'success', NULL, '{}', NULL, NULL)`,
			// Hàm quản trị phân vùng không dành cho role ứng dụng.
			`SELECT audit_logs_create_partition('2040-01-01')`,
		} {
			if _, err := tx.Exec(ctx, "SAVEPOINT s"); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, stmt); err == nil {
				t.Errorf("app_api chạy được: %s", stmt)
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

// Job chạy chậm: dòng đã rơi vào phân vùng mặc định không được làm hỏng việc tạo
// phân vùng của tháng đó.
func TestAuditPartitionCreationMovesRowsOutOfDefault(t *testing.T) {
	db := pgtest.Start(t)
	ctx := context.Background()
	org := ids.New()
	exec := func(sql string, args ...any) error {
		_, err := db.Admin.Exec(ctx, sql, args...)
		return err
	}
	if err := exec(`INSERT INTO organizations (id, slug, name) VALUES ($1,'a','A')`, org); err != nil {
		t.Fatal(err)
	}
	// Chỉ owner mới ghi trực tiếp được; mô phỏng một dòng ngoài mọi phân vùng đã tạo.
	if err := exec(`INSERT INTO audit_logs (id, org_id, actor_type, action, resource_type, outcome, hash, occurred_at)
		VALUES (gen_random_uuid(), $1, 'system', 'late', 'x', 'success', '\x00', '2040-06-15T00:00:00Z')`, org); err != nil {
		t.Fatal(err)
	}
	if err := exec(`SELECT audit_logs_create_partition('2040-06-15')`); err == nil {
		t.Error("ngày không phải đầu tháng phải bị từ chối")
	}
	if err := exec(`SELECT audit_logs_create_partition('2040-06-01')`); err != nil {
		t.Fatalf("tạo phân vùng khi default đã có dòng của tháng đó: %v", err)
	}
	if err := exec(`SELECT audit_logs_create_partition('2040-06-01')`); err != nil {
		t.Errorf("gọi lại phải là no-op: %v", err)
	}
	var where string
	var forced bool
	var policies int
	if err := db.Admin.QueryRow(ctx, `SELECT a.tableoid::regclass::text, c.relforcerowsecurity,
		(SELECT count(*) FROM pg_policy p WHERE p.polrelid = c.oid)
		FROM audit_logs a JOIN pg_class c ON c.oid = a.tableoid WHERE a.action = 'late'`).Scan(&where, &forced, &policies); err != nil {
		t.Fatal(err)
	}
	if where != "audit_logs_2040_06" || !forced || policies != 1 {
		t.Errorf("dòng nằm ở %s, force=%v, policies=%d", where, forced, policies)
	}
}

// Cách ly tenant và ràng buộc database trên agents, agent_versions, idempotency_keys.
func TestAgentAndIdempotencyTablesTenantIsolation(t *testing.T) {
	db := pgtest.Start(t)
	ctx := context.Background()
	app := db.PoolAs(t, "app_api", 1)
	orgA, orgB := ids.New(), ids.New()
	projA, projB, agentA, agentA2, agentB := ids.New(), ids.New(), ids.New(), ids.New(), ids.New()
	verA, verA2, verB := ids.New(), ids.New(), ids.New()
	seed := func(sql string, args ...any) {
		t.Helper()
		if _, err := db.Admin.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	seed(`INSERT INTO organizations (id, slug, name) VALUES ($1,'a','A'), ($2,'b','B')`, orgA, orgB)
	seed(`INSERT INTO projects (id, org_id, slug, name) VALUES ($1,$3,'pa','PA'), ($2,$4,'pb','PB')`, projA, projB, orgA, orgB)
	seed(`INSERT INTO agents (id, org_id, project_id, slug, name) VALUES ($1,$4,$6,'a1','A1'), ($2,$4,$6,'a2','A2'), ($3,$5,$7,'b1','B1')`,
		agentA, agentA2, agentB, orgA, orgB, projA, projB)
	seed(`INSERT INTO agent_versions (id, org_id, agent_id, version, system_prompt, config, config_hash) VALUES
		($1,$4,$6,1,'pa','{}','\x00'), ($2,$4,$7,1,'pa2','{}','\x00'), ($3,$5,$8,1,'SECRET PROMPT B','{}','\x00')`,
		verA, verA2, verB, orgA, orgB, agentA, agentA2, agentB)
	seed(`INSERT INTO idempotency_keys (org_id, principal_id, key, request_hash, status, expires_at)
		VALUES ($1, gen_random_uuid(), 'k', '\x00', 'in_progress', now() + interval '1 hour')`, orgB)

	q := func(s string) string { return "'" + s + "'" }
	a, b := q(orgA.String()), q(orgB.String())
	err := tenancy.InTx(tenancy.WithOrg(ctx, orgA), app, func(tx pgxv5.Tx) error {
		for table, want := range map[string]int{"agents": 2, "agent_versions": 2, "idempotency_keys": 0} {
			var n int
			if err := tx.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
				return err
			}
			if n != want {
				t.Errorf("org A thấy %d dòng trong %s, muốn %d", n, table, want)
			}
		}
		ver := `INSERT INTO agent_versions (org_id, agent_id, version, system_prompt, config, config_hash) VALUES (%s, %s, 2, 'p', '{}', '\x00') RETURNING id`
		for _, stmt := range []string{
			// ghi chéo tenant
			`UPDATE agents SET name = 'pwned' WHERE id = ` + q(agentB.String()) + ` RETURNING id`,
			`INSERT INTO agents (org_id, project_id, slug, name) VALUES (` + b + `,` + q(projB.String()) + `,'x','X') RETURNING id`,
			fmt.Sprintf(ver, b, q(agentB.String())),
			`INSERT INTO idempotency_keys (org_id, principal_id, key, request_hash, status, expires_at)
			 VALUES (` + b + `, gen_random_uuid(), 'x', '\x00', 'in_progress', now()) RETURNING key`,
			// khoá ngoại ghép: agent của org A trỏ project của org B; version của org A trỏ agent của org B
			`INSERT INTO agents (org_id, project_id, slug, name) VALUES (` + a + `,` + q(projB.String()) + `,'x','X') RETURNING id`,
			fmt.Sprintf(ver, a, q(agentB.String())),
			// active_version phải là version của chính agent
			`UPDATE agents SET active_version_id = ` + q(verA2.String()) + `, status = 'active' WHERE id = ` + q(agentA.String()) + ` RETURNING id`,
			`UPDATE agents SET active_version_id = ` + q(verB.String()) + `, status = 'active' WHERE id = ` + q(agentA.String()) + ` RETURNING id`,
			// trạng thái active cần có version đang hoạt động
			`UPDATE agents SET status = 'active' WHERE id = ` + q(agentA.String()) + ` RETURNING id`,
			// quyền theo cột và tính bất biến
			`UPDATE agents SET project_id = project_id RETURNING id`,
			`UPDATE agents SET slug = 'renamed' RETURNING id`,
			`UPDATE agent_versions SET system_prompt = 'x' RETURNING id`,
			`DELETE FROM agent_versions RETURNING id`,
			`DELETE FROM agents RETURNING id`,
			`TRUNCATE agent_versions CASCADE`,
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
			if err == nil && (affected > 0 || strings.HasPrefix(stmt, "TRUNCATE")) {
				t.Errorf("app_api trong org A chạy được (%d dòng): %s", affected, stmt)
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
	// TRUNCATE bị chặn cả với owner: không ai xoá sạch version (và agents qua CASCADE) được.
	if _, err := db.Admin.Exec(ctx, `TRUNCATE agent_versions CASCADE`); err == nil {
		t.Error("owner TRUNCATE được agent_versions")
	}
}

// Ràng buộc phạm vi của Model Catalog ở mức database: kiểm tra khoá ngoại không
// chịu RLS, nên chỉ các ràng buộc này ngăn được tham chiếu chéo tenant.
func TestModelCatalogScopeConstraints(t *testing.T) {
	db := pgtest.Start(t)
	ctx := context.Background()
	app := db.PoolAs(t, "app_api", 1)
	orgA, orgB := ids.New(), ids.New()
	provA, provB, provP, modelA, modelB, modelP := ids.New(), ids.New(), ids.New(), ids.New(), ids.New(), ids.New()
	seed := func(sql string, args ...any) {
		t.Helper()
		if _, err := db.Admin.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	seed(`INSERT INTO organizations (id, slug, name) VALUES ($1,'a','A'), ($2,'b','B')`, orgA, orgB)
	seed(`INSERT INTO model_providers (id, org_id, slug, adapter) VALUES ($1,$4,'pa','x'), ($2,$5,'pb','x'), ($3,NULL,'pp','x')`, provA, provB, provP, orgA, orgB)
	seed(`INSERT INTO models (id, org_id, provider_id, provider_model, display_name) VALUES ($1,$4,$6,'m','MA'), ($2,$5,$7,'m','MB'), ($3,NULL,$8,'m','MP')`,
		modelA, modelB, modelP, orgA, orgB, provA, provB, provP)
	seed(`INSERT INTO model_pricing (model_id, org_id, model_scope_org_id, version, input_per_mtok, output_per_mtok, effective_from) VALUES ($1,$2,$2,1,1,1,'2026-01-01')`, modelB, orgB)

	// Dưới app_admin (không chịu RLS, không bị GRANT chặn): chỉ còn ràng buộc của bảng.
	reject := func(name, sql string, args ...any) {
		t.Helper()
		tx, err := db.Admin.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_admin"); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, sql, args...); err == nil {
			t.Errorf("%s: database chấp nhận, muốn bị từ chối", name)
		}
	}
	reject("model của org A dưới provider của org B",
		`INSERT INTO models (org_id, provider_id, provider_model, display_name) VALUES ($1,$2,'x','X')`, orgA, provB)
	reject("model của org A dưới provider của platform",
		`INSERT INTO models (org_id, provider_id, provider_model, display_name) VALUES ($1,$2,'x','X')`, orgA, provP)
	reject("model của platform dưới provider của org",
		`INSERT INTO models (org_id, provider_id, provider_model, display_name) VALUES (NULL,$1,'x','X')`, provA)
	reject("giá của org A cho model của org B (khai phạm vi platform)",
		`INSERT INTO model_pricing (model_id, org_id, version, input_per_mtok, output_per_mtok, effective_from) VALUES ($1,$2,1,1,1,'2026-01-01')`, modelB, orgA)
	reject("giá của org A cho model của org B (khai phạm vi org B)",
		`INSERT INTO model_pricing (model_id, org_id, model_scope_org_id, version, input_per_mtok, output_per_mtok, effective_from) VALUES ($1,$2,$3,1,1,1,'2026-01-01')`, modelB, orgA, orgB)
	reject("giá niêm yết của platform cho model của org",
		`INSERT INTO model_pricing (model_id, org_id, model_scope_org_id, version, input_per_mtok, output_per_mtok, effective_from) VALUES ($1,NULL,$2,1,1,1,'2026-01-01')`, modelA, orgA)
	reject("hai giá chồng khoảng hiệu lực",
		`INSERT INTO model_pricing (model_id, org_id, model_scope_org_id, version, input_per_mtok, output_per_mtok, effective_from) VALUES ($1,$2,$2,2,1,1,'2026-06-01')`, modelB, orgB)
	reject("giá NaN",
		`INSERT INTO model_pricing (model_id, org_id, model_scope_org_id, version, input_per_mtok, output_per_mtok, effective_from) VALUES ($1,$2,$2,1,'NaN',1,'2026-01-01')`, modelA, orgA)
	reject("currency sai dạng",
		`INSERT INTO model_pricing (model_id, org_id, model_scope_org_id, version, currency, input_per_mtok, output_per_mtok, effective_from) VALUES ($1,$2,$2,1,'u',1,1,'2026-01-01')`, modelA, orgA)
	reject("trùng version (khoảng không chồng)",
		`INSERT INTO model_pricing (model_id, org_id, model_scope_org_id, version, input_per_mtok, output_per_mtok, effective_from, effective_to) VALUES ($1,$2,$2,1,1,1,'2020-01-01','2020-02-01')`, modelB, orgB)
	reject("đóng giá lùi về quá khứ",
		`UPDATE model_pricing SET effective_to = '2026-03-01' WHERE model_id = $1 AND effective_to IS NULL`, modelB)
	reject("giá âm",
		`INSERT INTO model_pricing (model_id, org_id, model_scope_org_id, version, input_per_mtok, output_per_mtok, effective_from) VALUES ($1,$2,$2,1,-1,1,'2026-01-01')`, modelA, orgA)

	// Dưới app_api trong tenant A: thấy catalog của mình và của platform, không thấy
	// của org B, và không ghi được vào dòng của platform hay của org khác.
	err := tenancy.InTx(tenancy.WithOrg(ctx, orgA), app, func(tx pgxv5.Tx) error {
		for table, want := range map[string]int{"model_providers": 2, "models": 2, "model_pricing": 0} {
			var n int
			if err := tx.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
				return err
			}
			if n != want {
				t.Errorf("org A thấy %d dòng trong %s, muốn %d", n, table, want)
			}
		}
		for _, stmt := range []string{
			`UPDATE model_providers SET enabled = false WHERE org_id IS NULL RETURNING id`,
			`UPDATE models SET enabled = false WHERE org_id IS NULL RETURNING id`,
			`UPDATE models SET enabled = false WHERE id = '` + modelB.String() + `' RETURNING id`,
			`INSERT INTO model_providers (org_id, slug, adapter) VALUES (NULL, 'fake-platform', 'x') RETURNING id`,
			`INSERT INTO model_providers (org_id, slug, adapter) VALUES ('` + orgB.String() + `', 'x', 'x') RETURNING id`,
			`INSERT INTO model_pricing (model_id, org_id, version, input_per_mtok, output_per_mtok, effective_from)
			 VALUES ('` + modelP.String() + `', NULL, 9, 0, 0, '2026-01-01') RETURNING id`,
			`UPDATE models SET provider_id = provider_id RETURNING id`,
			`UPDATE model_providers SET adapter = 'other' RETURNING id`,
			`DELETE FROM models RETURNING id`,
			`DELETE FROM model_providers RETURNING id`,
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
				t.Errorf("app_api trong org A chạy được (%d dòng): %s", affected, stmt)
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

// Cách ly tenant trên MCP Registry: tenant thấy server của mình và của platform,
// không thấy của org khác; role ứng dụng (api, gateway) chỉ đọc.
func TestMCPRegistryIsolation(t *testing.T) {
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
	servers, versions := map[string]uuid.UUID{}, map[string]uuid.UUID{}
	for slug, org := range map[string]*uuid.UUID{"sa": &orgA, "sb": &orgB, "sp": nil} {
		sid, vid := ids.New(), ids.New()
		servers[slug] = sid
		// Tool chỉ thêm được trong transaction tạo version của nó.
		tx, err := db.Admin.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO mcp_servers (id, org_id, slug, name, kind, transport, endpoint, auth_method) VALUES ($1,$2,$3,'S','managed','streamable_http','http://x','service_token')`, []any{sid, org, slug}},
			{`INSERT INTO mcp_server_versions (id, org_id, server_id, version, protocol_version, snapshot, snapshot_hash, review_status) VALUES ($1,$2,$3,1,'p','{}','\x00','approved')`, []any{vid, org, sid}},
			{`INSERT INTO mcp_tools (org_id, server_id, server_version_id, name, input_schema, definition_hash) VALUES ($1,$2,$3,'t','{"type":"object"}','\x00')`, []any{org, sid, vid}},
			{`UPDATE mcp_servers SET active_version_id = $2, status = 'active' WHERE id = $1`, []any{sid, vid}},
		} {
			if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		versions[slug] = vid
	}

	// Khoá ngoại không chịu RLS: ràng buộc của bảng phải tự chặn tham chiếu chéo phạm vi.
	pending := ids.New()
	seed(`INSERT INTO mcp_server_versions (id, org_id, server_id, version, protocol_version, snapshot, snapshot_hash) VALUES ($1,$2,$3,2,'p','{}','\x00')`, pending, orgA, servers["sa"])
	for name, stmt := range map[string]string{
		"tool chèn vào version đã duyệt":                `INSERT INTO mcp_tools (org_id, server_id, server_version_id, name, input_schema, definition_hash, enabled, risk_level) VALUES ('` + orgA.String() + `','` + servers["sa"].String() + `','` + versions["sa"].String() + `','exec','{"type":"object"}','\x00',true,'read')`,
		"tool của org A dưới server của org B":          `INSERT INTO mcp_tools (org_id, server_id, server_version_id, name, input_schema, definition_hash) VALUES ('` + orgA.String() + `','` + servers["sb"].String() + `','` + versions["sb"].String() + `','x','{"type":"object"}','\x00')`,
		"version chưa duyệt làm version hoạt động":      `UPDATE mcp_servers SET active_version_id = '` + pending.String() + `' WHERE id = '` + servers["sa"].String() + `'`,
		"version của server khác làm version hoạt động": `UPDATE mcp_servers SET active_version_id = '` + versions["sb"].String() + `' WHERE id = '` + servers["sa"].String() + `'`,
		"sửa ảnh chụp của version":                      `UPDATE mcp_server_versions SET snapshot = '{"x":1}' WHERE id = '` + versions["sa"].String() + `'`,
		"đổi kết quả duyệt":                             `UPDATE mcp_server_versions SET review_status = 'rejected' WHERE id = '` + versions["sa"].String() + `'`,
		"xoá version":                                   `DELETE FROM mcp_server_versions WHERE id = '` + pending.String() + `'`,
		"version của org A cho server của org B":        `INSERT INTO mcp_server_versions (org_id, server_id, version, protocol_version, snapshot, snapshot_hash) VALUES ('` + orgA.String() + `','` + servers["sb"].String() + `',2,'p','{}','\x00')`,
		"version của org A cho server platform":         `INSERT INTO mcp_server_versions (org_id, server_id, version, protocol_version, snapshot, snapshot_hash) VALUES ('` + orgA.String() + `','` + servers["sp"].String() + `',3,'p','{}','\x00')`,
		"version platform cho server của org":           `INSERT INTO mcp_server_versions (org_id, server_id, version, protocol_version, snapshot, snapshot_hash) VALUES (NULL,'` + servers["sa"].String() + `',2,'p','{}','\x00')`,
	} {
		tx, err := db.Admin.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = tx.Exec(ctx, "SET LOCAL ROLE app_admin")
		if _, err := tx.Exec(ctx, stmt); err == nil {
			t.Errorf("%s: database chấp nhận", name)
		}
		_ = tx.Rollback(ctx)
	}

	// Endpoint của server NGOÀI (do tenant đăng ký) không đổi được, kể cả với app_admin.
	// Migration 00037 cố ý cho app_admin đổi endpoint của managed server (đường
	// `hqadmin mcp-server set-endpoint`, có test ở tests/api); trigger của migration 00038
	// giữ luật cho server ngoài. Server ngoài được tạo trong chính transaction này rồi
	// rollback, để các phép đếm bên dưới không đổi.
	func() {
		tx, err := db.Admin.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		external := ids.New()
		if _, err := tx.Exec(ctx, `INSERT INTO mcp_servers (id, org_id, slug, name, kind, transport, endpoint, auth_method) VALUES ($1,$2,'ea','E','external','streamable_http','https://x','org_secret')`, external, orgA); err != nil {
			t.Fatalf("tạo server ngoài: %v", err)
		}
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_admin"); err != nil {
			t.Fatal(err)
		}
		// Phải bị chặn bởi trigger (luật theo loại server), không phải vì thiếu quyền trên cột.
		_, err = tx.Exec(ctx, `UPDATE mcp_servers SET endpoint = 'https://evil' WHERE id = $1`, external)
		if err == nil {
			t.Error("đổi endpoint của server ngoài: database chấp nhận")
		} else if !strings.Contains(err.Error(), "không đổi được") {
			t.Errorf("đổi endpoint của server ngoài bị từ chối vì lý do khác với trigger: %v", err)
		}
	}()

	for _, role := range []string{"app_api", "app_gateway"} {
		err := tenancy.InTx(tenancy.WithOrg(ctx, orgA), db.PoolAs(t, role, 1), func(tx pgxv5.Tx) error {
			for _, table := range []string{"mcp_servers", "mcp_server_versions", "mcp_tools"} {
				var n, foreign int
				if err := tx.QueryRow(ctx, "SELECT count(*), count(*) FILTER (WHERE org_id = $1) FROM "+table, orgB).Scan(&n, &foreign); err != nil {
					return err
				}
				want := 2
				if table == "mcp_server_versions" {
					want = 3 // thêm version chờ duyệt của org A
				}
				if n != want || foreign != 0 {
					t.Errorf("%s: org A thấy %d dòng trong %s (%d của org B), muốn %d và 0", role, n, table, foreign, want)
				}
			}
			for _, stmt := range []string{
				`UPDATE mcp_tools SET enabled = true, risk_level = 'read'`,
				`UPDATE mcp_servers SET endpoint = 'http://evil'`,
				`INSERT INTO mcp_servers (org_id, slug, name, kind, transport, auth_method) VALUES ('` + orgA.String() + `','zz','Z','managed','streamable_http','none')`,
				`DELETE FROM mcp_tools`,
				`DELETE FROM mcp_server_versions`,
			} {
				if _, err := tx.Exec(ctx, "SAVEPOINT s"); err != nil {
					return err
				}
				// Dữ liệu mẫu chỉ có managed server: api (từ 00012) ghi được server NGOÀI, nhưng
				// với managed server thì câu lệnh phải lỗi hoặc không tác động dòng nào.
				if tag, err := tx.Exec(ctx, stmt); err == nil && tag.RowsAffected() > 0 {
					t.Errorf("%s ghi được vào Registry (%d dòng): %s", role, tag.RowsAffected(), stmt)
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

// Cách ly tenant và bất biến trên các bảng thực thi: agent_executions, agent_tasks,
// execution_steps, usage_records.
func TestExecutionTablesIsolation(t *testing.T) {
	db := pgtest.Start(t)
	ctx := context.Background()
	app := db.PoolAs(t, "app_api", 1)
	orgA, orgB := ids.New(), ids.New()
	type fixture struct{ project, agent, version, exec, task uuid.UUID }
	fx := map[uuid.UUID]fixture{}
	seed := func(sql string, args ...any) {
		t.Helper()
		if _, err := db.Admin.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	seed(`INSERT INTO organizations (id, slug, name) VALUES ($1,'a','A'), ($2,'b','B')`, orgA, orgB)
	for _, org := range []uuid.UUID{orgA, orgB} {
		f := fixture{ids.New(), ids.New(), ids.New(), ids.New(), ids.New()}
		fx[org] = f
		seed(`INSERT INTO projects (id, org_id, slug, name) VALUES ($1,$2,'p','P')`, f.project, org)
		seed(`INSERT INTO agents (id, org_id, project_id, slug, name) VALUES ($1,$2,$3,'a','A')`, f.agent, org, f.project)
		seed(`INSERT INTO agent_versions (id, org_id, agent_id, version, system_prompt, config, config_hash) VALUES ($1,$2,$3,1,'p','{}','\x00')`, f.version, org, f.agent)
		seed(`INSERT INTO agent_executions (id, org_id, project_id, initiator_type, mode, status, input, deadline_at)
		      VALUES ($1,$2,$3,'user','single_agent','running','{"message":"SECRET"}', now() + interval '1 hour')`, f.exec, org, f.project)
		seed(`INSERT INTO agent_tasks (id, org_id, execution_id, agent_id, agent_version_id, goal, status) VALUES ($1,$2,$3,$4,$5,'g','running')`,
			f.task, org, f.exec, f.agent, f.version)
		seed(`INSERT INTO execution_steps (org_id, execution_id, task_id, step_seq, kind, status, request) VALUES ($1,$2,$3,1,'llm_call','started','{"prompt":"SECRET"}')`,
			org, f.exec, f.task)
		seed(`INSERT INTO usage_records (org_id, project_id, execution_id, task_id, step_seq, kind, input_tokens, estimated_cost) VALUES ($1,$2,$3,$4,1,'llm',100,0.5)`,
			org, f.project, f.exec, f.task)
	}
	a, b := fx[orgA], fx[orgB]
	q := func(id uuid.UUID) string { return "'" + id.String() + "'" }

	err := tenancy.InTx(tenancy.WithOrg(ctx, orgA), app, func(tx pgxv5.Tx) error {
		for _, table := range []string{"agent_executions", "agent_tasks", "execution_steps", "usage_records"} {
			var n, foreign int
			if err := tx.QueryRow(ctx, "SELECT count(*), count(*) FILTER (WHERE org_id = $1) FROM "+table, orgB).Scan(&n, &foreign); err != nil {
				return err
			}
			if n != 1 || foreign != 0 {
				t.Errorf("org A thấy %d dòng trong %s (%d của org B), muốn 1 và 0", n, table, foreign)
			}
		}
		// Mỗi câu phải không tác động dòng nào hoặc bị từ chối.
		for name, stmt := range map[string]string{
			"sửa execution của org B":             `UPDATE agent_executions SET status = 'cancelled' WHERE id = ` + q(b.exec) + ` RETURNING id`,
			"sửa task của org B":                  `UPDATE agent_tasks SET status = 'failed' WHERE id = ` + q(b.task) + ` RETURNING id`,
			"hoàn tất step của org B":             `UPDATE execution_steps SET status = 'succeeded' WHERE task_id = ` + q(b.task) + ` RETURNING id`,
			"execution khai org B":                `INSERT INTO agent_executions (org_id, project_id, initiator_type, mode, input, deadline_at) VALUES (` + q(orgB) + `,` + q(b.project) + `,'user','single_agent','{}', now()) RETURNING id`,
			"execution của org A trong project B": `INSERT INTO agent_executions (org_id, project_id, initiator_type, mode, input, deadline_at) VALUES (` + q(orgA) + `,` + q(b.project) + `,'user','single_agent','{}', now()) RETURNING id`,
			"task của org A trong execution B":    `INSERT INTO agent_tasks (org_id, execution_id, agent_id, agent_version_id, goal) VALUES (` + q(orgA) + `,` + q(b.exec) + `,` + q(a.agent) + `,` + q(a.version) + `,'g') RETURNING id`,
			"task của org A chạy agent của B":     `INSERT INTO agent_tasks (org_id, execution_id, agent_id, agent_version_id, goal) VALUES (` + q(orgA) + `,` + q(a.exec) + `,` + q(b.agent) + `,` + q(b.version) + `,'g') RETURNING id`,
			"task với version của agent khác":     `INSERT INTO agent_tasks (org_id, execution_id, agent_id, agent_version_id, goal) VALUES (` + q(orgA) + `,` + q(a.exec) + `,` + q(a.agent) + `,` + q(b.version) + `,'g') RETURNING id`,
			"step khai org B":                     `INSERT INTO execution_steps (org_id, execution_id, task_id, step_seq, kind, status) VALUES (` + q(orgB) + `,` + q(b.exec) + `,` + q(b.task) + `,9,'llm_call','started') RETURNING id`,
			"usage khai org B":                    `INSERT INTO usage_records (org_id, project_id, kind) VALUES (` + q(orgB) + `,` + q(b.project) + `,'llm') RETURNING id`,
			"sửa usage":                           `UPDATE usage_records SET estimated_cost = 0 RETURNING id`,
			"xoá usage":                           `DELETE FROM usage_records RETURNING id`,
			"xoá step":                            `DELETE FROM execution_steps RETURNING id`,
			"xoá execution":                       `DELETE FROM agent_executions RETURNING id`,
			"đổi input của execution":             `UPDATE agent_executions SET input = '{}' WHERE id = ` + q(a.exec) + ` RETURNING id`,
			"đổi nội dung yêu cầu của step":       `UPDATE execution_steps SET status = 'succeeded', request = '{}' WHERE task_id = ` + q(a.task) + ` RETURNING id`,
			"đổi chuỗi model của task":            `UPDATE agent_tasks SET model_chain = ARRAY[gen_random_uuid()] WHERE id = ` + q(a.task) + ` RETURNING id`,
			"đổi system prompt của task":          `UPDATE agent_tasks SET system_prompt = 'x' WHERE id = ` + q(a.task) + ` RETURNING id`,
			"chuỗi model rỗng":                    `INSERT INTO agent_tasks (org_id, execution_id, agent_id, agent_version_id, goal, model_chain) VALUES (` + q(orgA) + `,` + q(a.exec) + `,` + q(a.agent) + `,` + q(a.version) + `,'g','{}') RETURNING id`,
			"chuỗi model quá dài":                 `INSERT INTO agent_tasks (org_id, execution_id, agent_id, agent_version_id, goal, model_chain) VALUES (` + q(orgA) + `,` + q(a.exec) + `,` + q(a.agent) + `,` + q(a.version) + `,'g', ARRAY[gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),gen_random_uuid()]) RETURNING id`,
			"chuỗi model có phần tử NULL":         `INSERT INTO agent_tasks (org_id, execution_id, agent_id, agent_version_id, goal, model_chain) VALUES (` + q(orgA) + `,` + q(a.exec) + `,` + q(a.agent) + `,` + q(a.version) + `,'g', ARRAY[NULL]::uuid[]) RETURNING id`,
			"giá âm":                              `UPDATE agent_executions SET estimated_cost = -1 WHERE id = ` + q(a.exec) + ` RETURNING id`,
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
				t.Errorf("%s: database chấp nhận (%d dòng)", name, affected)
			}
			if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT s"); err != nil {
				return err
			}
		}

		// Step hoàn tất đúng một lần; execution đã kết thúc không đổi nữa.
		if _, err := tx.Exec(ctx, `UPDATE execution_steps SET status = 'succeeded', response = '{}', duration_ms = 5 WHERE task_id = $1 AND step_seq = 1`, a.task); err != nil {
			t.Errorf("hoàn tất step: %v", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE agent_executions SET status = 'completed', finished_at = now() WHERE id = $1`, a.exec); err != nil {
			t.Errorf("đóng execution: %v", err)
		}
		for name, stmt := range map[string]string{
			"hoàn tất step lần hai":         `UPDATE execution_steps SET status = 'failed' WHERE task_id = ` + q(a.task),
			"mở lại execution đã kết thúc":  `UPDATE agent_executions SET status = 'running' WHERE id = ` + q(a.exec),
			"đổi kết quả execution đã xong": `UPDATE agent_executions SET output = '{"x":1}' WHERE id = ` + q(a.exec),
		} {
			if _, err := tx.Exec(ctx, "SAVEPOINT s"); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, stmt); err == nil {
				t.Errorf("%s: database chấp nhận", name)
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

	// Gateway chỉ đọc execution / task (để kiểm task còn chạy). Quyền của app_worker
	// (từ migration 00013, Phase 2) được kiểm ở TestWorkerRolePrivileges.
	for role, readable := range map[string]bool{"app_gateway": true} {
		err := tenancy.InTx(tenancy.WithOrg(ctx, orgA), db.PoolAs(t, role, 1), func(tx pgxv5.Tx) error {
			for stmt, wantOK := range map[string]bool{
				`SELECT count(*) FROM agent_tasks`:                readable,
				`SELECT count(*) FROM agent_executions`:           readable,
				`SELECT count(*) FROM execution_steps`:            false,
				`SELECT model_chain, iterations FROM agent_tasks`: readable,
				`SELECT system_prompt FROM agent_tasks`:           false,
				`SELECT goal FROM agent_tasks`:                    false,
				`SELECT output FROM agent_tasks`:                  false,
				// Từ migration 00017 Gateway đọc được các cột chi phí của usage (dựng lại bộ
				// đếm budget từ ledger), nhưng không đọc được token hay tham chiếu step.
				`SELECT sum(estimated_cost) FROM usage_records`:                                                            readable,
				`SELECT input_tokens FROM usage_records`:                                                                   false,
				`SELECT task_id, step_seq FROM usage_records`:                                                              false,
				`UPDATE agent_executions SET cancel_requested_at = now()`:                                                  false,
				`UPDATE agent_tasks SET status = 'completed'`:                                                              false,
				`INSERT INTO usage_records (org_id, project_id, kind) VALUES (` + q(orgA) + `,` + q(a.project) + `,'llm')`: false,
			} {
				if _, err := tx.Exec(ctx, "SAVEPOINT s"); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, stmt)
				if (err == nil) != wantOK {
					t.Errorf("%s: %q → err = %v, muốn thành công = %v", role, stmt, err, wantOK)
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

// Cách ly tenant và phân quyền theo process trên secrets và api_keys.
func TestSecretsAndAPIKeysIsolation(t *testing.T) {
	db := pgtest.Start(t)
	ctx := context.Background()
	orgA, orgB, userA := ids.New(), ids.New(), ids.New()
	secretA, secretB, keyA, keyB := ids.New(), ids.New(), ids.New(), ids.New()
	seed := func(sql string, args ...any) {
		t.Helper()
		if _, err := db.Admin.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	seed(`INSERT INTO organizations (id, slug, name) VALUES ($1,'a','A'), ($2,'b','B')`, orgA, orgB)
	seed(`INSERT INTO users (id, idp_issuer, idp_subject, email) VALUES ($1,'iss','u','u@example.test')`, userA)
	seed(`INSERT INTO secrets (id, org_id, name, kind, ciphertext, wrapped_dek, kek_ref) VALUES
		($1,$3,'s','api_key','\x0a','\x0b','local'), ($2,$4,'s','api_key','\x0c','\x0d','local')`, secretA, secretB, orgA, orgB)
	seed(`INSERT INTO api_keys (id, org_id, name, key_prefix, key_hash, role_name, created_by, expires_at) VALUES
		($1,$3,'ka','aaaaaaaaaaaa',$5,'viewer',$6, now() + interval '1 day'),
		($2,$4,'kb','bbbbbbbbbbbb',$5,'viewer',$6, now() + interval '1 day')`, keyA, keyB, orgA, orgB, make([]byte, 32), userA)
	q := func(id uuid.UUID) string { return "'" + id.String() + "'" }

	type check struct {
		stmt   string
		wantOK bool
		rows   int // số dòng mong đợi khi wantOK; -1 = không kiểm
	}
	run := func(role string, checks []check) {
		t.Helper()
		err := tenancy.InTx(tenancy.WithOrg(ctx, orgA), db.PoolAs(t, role, 1), func(tx pgxv5.Tx) error {
			for _, c := range checks {
				if _, err := tx.Exec(ctx, "SAVEPOINT s"); err != nil {
					return err
				}
				rows, err := tx.Query(ctx, c.stmt)
				n := 0
				if err == nil {
					for rows.Next() {
						n++
					}
					err = rows.Err()
					rows.Close()
				}
				if (err == nil) != c.wantOK || (c.wantOK && c.rows >= 0 && n != c.rows) {
					t.Errorf("%s: %q → err = %v, %d dòng; muốn thành công = %v, %d dòng", role, c.stmt, err, n, c.wantOK, c.rows)
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

	// api: ghi được secret và đọc metadata của org mình, không đọc được bản mã, không sửa, không xoá.
	run("app_api", []check{
		{`SELECT id, name, kind, version, kek_ref FROM secrets`, true, 1},
		{`SELECT ciphertext FROM secrets`, false, 0},
		{`SELECT wrapped_dek FROM secrets`, false, 0},
		{`SELECT * FROM secrets`, false, 0},
		{`INSERT INTO secrets (org_id, name, kind, ciphertext, wrapped_dek, kek_ref) VALUES (` + q(orgA) + `,'n','api_key','\x01','\x02','local') RETURNING id`, true, 1},
		{`INSERT INTO secrets (org_id, name, kind, ciphertext, wrapped_dek, kek_ref) VALUES (` + q(orgB) + `,'n','api_key','\x01','\x02','local') RETURNING id`, false, 0},
		{`UPDATE secrets SET name = 'x' RETURNING id`, false, 0},
		{`DELETE FROM secrets RETURNING id`, false, 0},
		{`UPDATE model_providers SET secret_id = ` + q(secretB) + ` RETURNING id`, true, 0},
		// api_keys: chỉ thấy khoá của org mình; chỉ thu hồi được, không sửa được.
		{`SELECT id FROM api_keys`, true, 1},
		{`UPDATE api_keys SET revoked_at = now() WHERE id = ` + q(keyB) + ` RETURNING id`, true, 0},
		{`UPDATE api_keys SET revoked_at = now() WHERE id = ` + q(keyA) + ` RETURNING id`, true, 1},
		{`UPDATE api_keys SET key_hash = '\x00' RETURNING id`, false, 0},
		{`UPDATE api_keys SET role_name = 'org_admin' RETURNING id`, false, 0},
		{`UPDATE api_keys SET expires_at = now() + interval '10 years' RETURNING id`, false, 0},
		{`DELETE FROM api_keys RETURNING id`, false, 0},
		{`INSERT INTO api_keys (org_id, name, key_prefix, key_hash, role_name, created_by, expires_at) VALUES (` + q(orgB) + `,'x','cccccccccccc','\x` + strings.Repeat("00", 32) + `','viewer',` + q(userA) + `, now() + interval '1 day') RETURNING id`, false, 0},
		// Gắn quyền chỉ qua hàm, chỉ cho khoá của chính org.
		{`INSERT INTO role_bindings (org_id, role_id, principal_type, principal_id) SELECT ` + q(orgA) + `, id, 'user', ` + q(userA) + ` FROM roles WHERE name = 'org_admin' RETURNING id`, false, 0},
		// Đường vòng tới bản mã cũng bị quyền theo cột chặn.
		{`INSERT INTO secrets (org_id, name, kind, ciphertext, wrapped_dek, kek_ref) VALUES (` + q(orgA) + `,'n2','api_key','\x01','\x02','local') RETURNING ciphertext`, false, 0},
		{`SELECT secrets FROM secrets`, false, 0},
		{`SELECT to_jsonb(s) FROM secrets s`, false, 0},
		{`COPY secrets TO STDOUT`, false, 0},
		// Hàm gắn role: chỉ trong transaction tạo khoá, role lấy từ dòng khoá, mỗi khoá một lần.
		{`SELECT api_key_bind_role(` + q(keyA) + `, ` + q(userA) + `)`, false, 0}, // khoá tạo ở transaction khác
		{`SELECT api_key_bind_role(` + q(keyB) + `, ` + q(userA) + `)`, false, 0},
		{`WITH k AS (INSERT INTO api_keys (org_id, name, key_prefix, key_hash, role_name, created_by, expires_at) VALUES (` + q(orgA) + `,'n','dddddddddddd','\x` + strings.Repeat("00", 32) + `','viewer',` + q(userA) + `, now() + interval '1 day') RETURNING id) SELECT id FROM k`, true, 1},
		{`INSERT INTO api_keys (id, org_id, name, key_prefix, key_hash, role_name, created_by, expires_at) VALUES ('00000000-0000-7000-8000-000000000001',` + q(orgA) + `,'n','eeeeeeeeeeee','\x` + strings.Repeat("00", 32) + `','viewer',` + q(userA) + `, now() + interval '1 day');
		  SELECT api_key_bind_role('00000000-0000-7000-8000-000000000001', ` + q(userA) + `); SELECT api_key_bind_role('00000000-0000-7000-8000-000000000001', ` + q(userA) + `)`, false, 0}, // gắn lần hai
		{`INSERT INTO api_keys (id, org_id, name, key_prefix, key_hash, role_name, created_by, expires_at) VALUES ('00000000-0000-7000-8000-000000000002',` + q(orgA) + `,'n','ffffffffffff','\x` + strings.Repeat("00", 32) + `','org_admin',` + q(userA) + `, now() + interval '1 day');
		  SELECT api_key_bind_role('00000000-0000-7000-8000-000000000002', ` + q(userA) + `)`, false, 0}, // role quản lý khoá
	})
	// gateway: đọc được bản mã của org mình (để giải mã), không ghi được gì.
	run("app_gateway", []check{
		{`SELECT ciphertext, wrapped_dek FROM secrets`, true, 1},
		{`SELECT ciphertext FROM secrets WHERE id = ` + q(secretB), true, 0},
		{`INSERT INTO secrets (org_id, name, kind, ciphertext, wrapped_dek, kek_ref) VALUES (` + q(orgA) + `,'n','api_key','\x01','\x02','local') RETURNING id`, false, 0},
		{`SELECT id FROM api_keys`, true, 1},
		{`UPDATE api_keys SET revoked_at = now() RETURNING id`, false, 0},
		{`SELECT api_key_bind_role(` + q(keyA) + `, ` + q(userA) + `)`, false, 0},
	})
	run("app_worker", []check{
		{`SELECT id FROM secrets`, false, 0},
		{`SELECT id FROM api_keys`, false, 0},
	})

	// Bước xác thực: không đặt tenant, chỉ đặt prefix đang tra → đúng một dòng, của bất kỳ org nào.
	app := db.PoolAs(t, "app_api", 1)
	tx, err := app.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM api_keys`).Scan(&n); err != nil || n != 0 {
		t.Errorf("không đặt gì mà thấy %d khoá (%v)", n, err)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.api_key_prefix', 'bbbbbbbbbbbb', true)`); err != nil {
		t.Fatal(err)
	}
	var got uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM api_keys`).Scan(&got); err != nil || got != keyB {
		t.Errorf("tra theo prefix: %v %v", got, err)
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM secrets`).Scan(&n); err != nil || n != 0 {
		t.Errorf("transaction tra khoá thấy %d secret (%v)", n, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE api_keys SET revoked_at = now()`); err != nil {
		t.Errorf("update trong transaction tra khoá: %v", err)
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM api_keys WHERE revoked_at IS NOT NULL`).Scan(&n); err != nil || n != 0 {
		t.Errorf("transaction tra khoá sửa được %d khoá (%v)", n, err)
	}
}

// Job bảo trì: tạo phân vùng tháng sắp tới và dọn idempotency key hết hạn của mọi org.
func TestMaintenance(t *testing.T) {
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
	for i, org := range []uuid.UUID{orgA, orgB} {
		seed(`INSERT INTO idempotency_keys (org_id, principal_id, key, request_hash, status, expires_at) VALUES
			($1, gen_random_uuid(), $2, '\x00', 'in_progress', now() - interval '1 hour'),
			($1, gen_random_uuid(), $3, '\x00', 'in_progress', now() + interval '1 hour')`, org, fmt.Sprintf("old-%d", i), fmt.Sprintf("live-%d", i))
	}
	conn, err := db.Admin.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := db.Admin.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	parts := func() int {
		return count(`SELECT count(*) FROM pg_inherits i JOIN pg_class p ON p.oid = i.inhparent
			WHERE p.relname IN ('audit_logs','execution_steps','usage_records')`)
	}

	// Chạy như thể đã qua 30 tháng: migration chỉ tạo sẵn 24 tháng.
	future := time.Now().AddDate(0, 30, 0)
	before := parts()
	res, err := adminops.RunMaintenance(ctx, conn.Conn(), 3, future)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Months) != 4 || parts() != before+12 || res.IdempotencyKeysDeleted != 2 || res.AuditRowsInDefault != 0 {
		t.Errorf("kết quả: %+v, phân vùng %d → %d", res, before, parts())
	}
	if n := count(`SELECT count(*) FROM idempotency_keys`); n != 2 {
		t.Errorf("còn %d idempotency key, muốn 2 (chưa hết hạn)", n)
	}
	// Ghi được vào tháng vừa có phân vùng, dưới role ứng dụng và vẫn chịu RLS.
	at := time.Date(future.UTC().Year(), future.UTC().Month(), 15, 0, 0, 0, 0, time.UTC)
	err = tenancy.InTx(tenancy.WithOrg(ctx, orgA), db.PoolAs(t, "app_api", 1), func(tx pgxv5.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO execution_steps (org_id, execution_id, task_id, step_seq, kind, status, created_at)
			VALUES ($1, gen_random_uuid(), gen_random_uuid(), 1, 'llm_call', 'started', $2)`, orgA, at); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO usage_records (org_id, project_id, kind, occurred_at) VALUES ($1, gen_random_uuid(), 'llm', $2)`, orgA, at); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "SAVEPOINT s"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO execution_steps (org_id, execution_id, task_id, step_seq, kind, status, created_at)
			VALUES ($1, gen_random_uuid(), gen_random_uuid(), 1, 'llm_call', 'started', $2)`, orgB, at); err == nil {
			t.Error("phân vùng mới không chịu RLS: ghi được dòng của org khác")
		}
		_, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT s")
		return err
	})
	if err != nil {
		t.Fatalf("ghi vào phân vùng mới: %v", err)
	}
	// Chạy lại không đổi gì.
	again, err := adminops.RunMaintenance(ctx, conn.Conn(), 3, future)
	if err != nil || parts() != before+12 || again.IdempotencyKeysDeleted != 0 {
		t.Errorf("chạy lại: %+v %v, phân vùng %d", again, err, parts())
	}
	if _, err := adminops.RunMaintenance(ctx, conn.Conn(), 0, future); err == nil {
		t.Error("months = 0 được chấp nhận")
	}
}

// Role của api chỉ ghi được MCP server NGOÀI: lớp chặn ở database, độc lập với code.
func TestRegistryTenantWritePolicies(t *testing.T) {
	db := pgtest.Start(t)
	ctx := context.Background()
	orgA, orgB := ids.New(), ids.New()
	managed, managedVersion, platform, extSrv := ids.New(), ids.New(), ids.New(), ids.New()
	if _, err := db.Admin.Exec(ctx, `INSERT INTO organizations (id, slug, name) VALUES ($1,'a','A'), ($2,'b','B')`, orgA, orgB); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO mcp_servers (id, org_id, slug, name, kind, transport, endpoint, auth_method) VALUES ($1,$2,'docker','D','managed','streamable_http','http://x','service_token')`, []any{managed, orgA}},
		{`INSERT INTO mcp_servers (id, org_id, slug, name, kind, transport, endpoint, auth_method) VALUES ($1,NULL,'shared','S','managed','streamable_http','http://x','service_token')`, []any{platform}},
		{`INSERT INTO mcp_server_versions (id, org_id, server_id, version, protocol_version, snapshot, snapshot_hash, review_status) VALUES ($1,$2,$3,1,'p','[]','\x00','approved')`, []any{managedVersion, orgA, managed}},
		{`INSERT INTO mcp_tools (org_id, server_id, server_version_id, name, input_schema, definition_hash, enabled, risk_level) VALUES ($1,$2,$3,'t','{"type":"object"}','\x00',true,'read')`, []any{orgA, managed, managedVersion}},
		{`UPDATE mcp_servers SET active_version_id = $2, status = 'active' WHERE id = $1`, []any{managed, managedVersion}},
		{`INSERT INTO mcp_servers (id, org_id, slug, name, kind, transport, endpoint, auth_method) VALUES ($1,$2,'ext','E','external','streamable_http','https://x','org_secret')`, []any{extSrv, orgA}},
	} {
		if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	q := func(id uuid.UUID) string { return "'" + id.String() + "'" }
	server := func(kind, transport, auth, slug string) string {
		return `INSERT INTO mcp_servers (org_id, slug, name, kind, transport, endpoint, auth_method) VALUES (` + q(orgA) + `,'` + slug + `','N','` + kind + `','` + transport + `','https://x','` + auth + `') RETURNING id`
	}
	err = tenancy.InTx(tenancy.WithOrg(ctx, orgA), db.PoolAs(t, "app_api", 1), func(tx pgxv5.Tx) error {
		for name, c := range map[string]struct {
			stmt string
			rows int // -1 = phải lỗi
		}{
			"server ngoài":                      {server("external", "streamable_http", "org_secret", "ok-ext"), 1},
			"managed server":                    {server("managed", "streamable_http", "service_token", "fake-managed"), -1},
			"server ngoài mang service_token":   {server("external", "streamable_http", "service_token", "ext-signed"), -1},
			"server stdio":                      {server("external", "stdio", "none", "ext-stdio"), -1},
			"slug trùng server platform":        {server("external", "streamable_http", "none", "shared"), -1},
			"server của org khác":               {`INSERT INTO mcp_servers (org_id, slug, name, kind, transport, endpoint, auth_method) VALUES (` + q(orgB) + `,'x','N','external','streamable_http','https://x','none') RETURNING id`, -1},
			"server của platform":               {`INSERT INTO mcp_servers (org_id, slug, name, kind, transport, endpoint, auth_method) VALUES (NULL,'x','N','external','streamable_http','https://x','none') RETURNING id`, -1},
			"tắt managed server":                {`UPDATE mcp_servers SET status = 'disabled' WHERE id = ` + q(managed) + ` RETURNING id`, 0},
			"đổi tên server platform":           {`UPDATE mcp_servers SET name = 'x' WHERE id = ` + q(platform) + ` RETURNING id`, 0},
			"phân loại tool của managed server": {`UPDATE mcp_tools SET enabled = false WHERE server_id = ` + q(managed) + ` RETURNING id`, 0},
			"đổi kiểu server":                   {`UPDATE mcp_servers SET kind = 'managed' RETURNING id`, -1},
			"đổi endpoint":                      {`UPDATE mcp_servers SET endpoint = 'https://evil' RETURNING id`, -1},
			"version đã duyệt cho server ngoài": {`INSERT INTO mcp_server_versions (org_id, server_id, version, protocol_version, snapshot, snapshot_hash, review_status)
				VALUES (` + q(orgA) + `,` + q(extSrv) + `, 1, 'p', '[]', '\x00', 'approved') RETURNING id`, -1},
			"version chờ duyệt cho server ngoài": {`INSERT INTO mcp_server_versions (org_id, server_id, version, protocol_version, snapshot, snapshot_hash)
				VALUES (` + q(orgA) + `,` + q(extSrv) + `, 1, 'p', '[]', '\x00') RETURNING id`, 1},
			"credential trỏ tới secret không tồn tại": {`UPDATE mcp_servers SET secret_id = gen_random_uuid() WHERE id = ` + q(extSrv) + ` RETURNING id`, -1},
			"đổi tên server ngoài của mình":           {`UPDATE mcp_servers SET name = 'x' WHERE id = ` + q(extSrv) + ` RETURNING id`, 1},
			"version cho managed server":              {`INSERT INTO mcp_server_versions (org_id, server_id, version, protocol_version, snapshot, snapshot_hash) VALUES (` + q(orgA) + `,` + q(managed) + `,2,'p','[]','\x00') RETURNING id`, -1},
			"xoá server":                              {`DELETE FROM mcp_servers RETURNING id`, -1},
		} {
			if _, err := tx.Exec(ctx, "SAVEPOINT s"); err != nil {
				return err
			}
			rows, err := tx.Query(ctx, c.stmt)
			n := 0
			if err == nil {
				for rows.Next() {
					n++
				}
				err = rows.Err()
				rows.Close()
			}
			if (c.rows < 0) != (err != nil) || (c.rows >= 0 && n != c.rows) {
				t.Errorf("%s: err = %v, %d dòng; muốn %d", name, err, n, c.rows)
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

// TestWorkerRolePrivileges: role app_worker (process chạy vòng lặp trên Temporal) chỉ
// có đúng quyền các bước cần, bị RLS giới hạn theo org, và không chạm được secret,
// Registry, API key hay việc tạo execution.
func TestWorkerRolePrivileges(t *testing.T) {
	db := pgtest.Start(t)
	ctx := context.Background()
	worker := db.PoolAs(t, "app_worker", 1)
	orgA, orgB := ids.New(), ids.New()
	type fixture struct{ project, agent, version, exec, task uuid.UUID }
	fx := map[uuid.UUID]fixture{}
	seed := func(sql string, args ...any) {
		t.Helper()
		if _, err := db.Admin.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	seed(`INSERT INTO organizations (id, slug, name) VALUES ($1,'a','A'), ($2,'b','B')`, orgA, orgB)
	for _, org := range []uuid.UUID{orgA, orgB} {
		f := fixture{ids.New(), ids.New(), ids.New(), ids.New(), ids.New()}
		fx[org] = f
		seed(`INSERT INTO projects (id, org_id, slug, name) VALUES ($1,$2,'p','P')`, f.project, org)
		seed(`INSERT INTO agents (id, org_id, project_id, slug, name) VALUES ($1,$2,$3,'a','A')`, f.agent, org, f.project)
		seed(`INSERT INTO agent_versions (id, org_id, agent_id, version, system_prompt, config, config_hash) VALUES ($1,$2,$3,1,'prompt','{}','\x00')`, f.version, org, f.agent)
		seed(`INSERT INTO agent_executions (id, org_id, project_id, initiator_type, mode, status, input, deadline_at)
		      VALUES ($1,$2,$3,'user','single_agent','running','{}', now() + interval '1 hour')`, f.exec, org, f.project)
		seed(`INSERT INTO agent_tasks (id, org_id, execution_id, agent_id, agent_version_id, goal, status) VALUES ($1,$2,$3,$4,$5,'g','running')`,
			f.task, org, f.exec, f.agent, f.version)
		seed(`INSERT INTO execution_steps (org_id, execution_id, task_id, step_seq, kind, status) VALUES ($1,$2,$3,1,'llm_call','started')`, org, f.exec, f.task)
	}
	a, b := fx[orgA], fx[orgB]
	q := func(id uuid.UUID) string { return "'" + id.String() + "'" }

	err := tenancy.InTx(tenancy.WithOrg(ctx, orgA), worker, func(tx pgxv5.Tx) error {
		// Được phép: những gì các bước của vòng lặp làm, trong org của chính nó.
		for name, stmt := range map[string]string{
			"đọc system prompt đã chụp":       `SELECT system_prompt FROM agent_tasks WHERE id = ` + q(a.task),
			"mở step":                         `INSERT INTO execution_steps (org_id, execution_id, task_id, step_seq, kind, status) VALUES (` + q(orgA) + `,` + q(a.exec) + `,` + q(a.task) + `,2,'tool_call','started') RETURNING id`,
			"hoàn tất step kèm model đã dùng": `UPDATE execution_steps SET status = 'succeeded', response = '{}', model_id = gen_random_uuid() WHERE task_id = ` + q(a.task) + ` AND step_seq = 1 RETURNING id`,
			"ghi usage":                       `INSERT INTO usage_records (org_id, project_id, execution_id, task_id, step_seq, kind) VALUES (` + q(orgA) + `,` + q(a.project) + `,` + q(a.exec) + `,` + q(a.task) + `,1,'llm') RETURNING estimated_cost, currency`,
			"cộng dồn task":                   `UPDATE agent_tasks SET iterations = iterations + 1, estimated_cost = estimated_cost + 0.5 WHERE id = ` + q(a.task) + ` RETURNING id`,
			"khoá dòng task":                  `SELECT id FROM agent_tasks WHERE id = ` + q(a.task) + ` FOR NO KEY UPDATE`,
			"đóng execution":                  `UPDATE agent_executions SET status = 'completed', finished_at = now() WHERE id = ` + q(a.exec) + ` RETURNING id`,
			"đọc yêu cầu huỷ":                 `SELECT cancel_requested_at FROM agent_executions WHERE id = ` + q(a.exec),
			"đọc bảng giá":                    `SELECT count(*) FROM model_pricing`,
			"đọc provider của model":          `SELECT id, provider_id FROM models LIMIT 1`,
		} {
			if _, err := tx.Exec(ctx, "SAVEPOINT s"); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, stmt); err != nil {
				t.Errorf("%s: bị từ chối: %v", name, err)
				if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT s"); err != nil {
					return err
				}
			}
		}
		// Những ca này phải bị từ chối vì role không có quyền, không chỉ vì RLS lọc hết dòng.
		noGrant := map[string]bool{"tạo execution": true, "tạo task": true, "ghi yêu cầu huỷ": true, "đổi input của execution": true,
			"đổi goal của task": true, "đổi chuỗi model của task": true, "đổi system prompt của task": true, "đọc agent version": true, "đọc toàn bộ usage": true, "đổi kế hoạch của execution": true, "đổi phụ thuộc của task": true, "đổi on_failure của task": true, "tự tạm dừng execution": true, "tự gỡ tạm dừng": true, "sửa usage": true, "xoá step": true, "đọc secret": true,
			"đọc API key": true, "đọc budget": true, "đổi budget của task": true, "đổi budget của execution": true, "đọc MCP server": true, "đọc audit": true, "đặt giá model": true}
		// Không được phép: mỗi câu phải bị từ chối hoặc không tác động / không thấy dòng nào.
		for name, stmt := range map[string]string{
			"tạo execution":              `INSERT INTO agent_executions (org_id, project_id, initiator_type, mode, input, deadline_at) VALUES (` + q(orgA) + `,` + q(a.project) + `,'user','single_agent','{}', now()) RETURNING id`,
			"tạo task":                   `INSERT INTO agent_tasks (org_id, execution_id, agent_id, agent_version_id, goal) VALUES (` + q(orgA) + `,` + q(a.exec) + `,` + q(a.agent) + `,` + q(a.version) + `,'g') RETURNING id`,
			"ghi yêu cầu huỷ":            `UPDATE agent_executions SET cancel_requested_at = now() WHERE id = ` + q(a.exec) + ` RETURNING id`,
			"đổi input của execution":    `UPDATE agent_executions SET input = '{}' WHERE id = ` + q(a.exec) + ` RETURNING id`,
			"đổi goal của task":          `UPDATE agent_tasks SET goal = 'x' WHERE id = ` + q(a.task) + ` RETURNING id`,
			"đổi chuỗi model của task":   `UPDATE agent_tasks SET model_chain = ARRAY[gen_random_uuid()] WHERE id = ` + q(a.task) + ` RETURNING id`,
			"đổi system prompt của task": `UPDATE agent_tasks SET system_prompt = 'x' WHERE id = ` + q(a.task) + ` RETURNING id`,
			"đọc agent version":          `SELECT id FROM agent_versions LIMIT 1`,
			"đọc toàn bộ usage":          `SELECT id FROM usage_records LIMIT 1`,
			// Từ 00021 worker được ghi output_summary khi đóng một task; cấu trúc của kế hoạch
			// (task nào chạy, thấy kết quả của task nào) thì không.
			"đổi kế hoạch của execution": `UPDATE agent_executions SET plan = '{}' WHERE id = ` + q(a.exec) + ` RETURNING id`,
			"đổi phụ thuộc của task":     `UPDATE agent_tasks SET depends_on = '{}' WHERE id = ` + q(a.task) + ` RETURNING id`,
			"đổi on_failure của task":    `UPDATE agent_tasks SET on_failure = 'continue' WHERE id = ` + q(a.task) + ` RETURNING id`,
			// Pause / resume (00025): chỉ process api ghi yêu cầu tạm dừng; vòng lặp chỉ đọc.
			"tự tạm dừng execution":       `UPDATE agent_executions SET pause_requested_at = now() WHERE id = ` + q(a.exec) + ` RETURNING id`,
			"tự gỡ tạm dừng":              `UPDATE agent_executions SET pause_requested_at = NULL WHERE id = ` + q(a.exec) + ` RETURNING id`,
			"sửa usage":                   `UPDATE usage_records SET estimated_cost = 0 RETURNING id`,
			"xoá step":                    `DELETE FROM execution_steps RETURNING id`,
			"đọc secret":                  `SELECT id FROM secrets LIMIT 1`,
			"đọc budget":                  `SELECT id FROM budgets LIMIT 1`,
			"đổi budget của task":         `UPDATE agent_tasks SET budget_limit = 999 WHERE id = ` + q(a.task) + ` RETURNING id`,
			"đổi budget của execution":    `UPDATE agent_executions SET budget_limit = 999 WHERE id = ` + q(a.exec) + ` RETURNING id`,
			"đọc API key":                 `SELECT id FROM api_keys LIMIT 1`,
			"đọc MCP server":              `SELECT id FROM mcp_servers LIMIT 1`,
			"đọc audit":                   `SELECT id FROM audit_logs LIMIT 1`,
			"đặt giá model":               `INSERT INTO model_pricing (model_id, currency, input_per_mtok, output_per_mtok) SELECT id, 'USD', 1, 1 FROM models LIMIT 1 RETURNING id`,
			"đọc step của org B":          `SELECT id FROM execution_steps WHERE task_id = ` + q(b.task),
			"hoàn tất step của org B":     `UPDATE execution_steps SET status = 'succeeded' WHERE task_id = ` + q(b.task) + ` RETURNING id`,
			"đóng execution của org B":    `UPDATE agent_executions SET status = 'completed' WHERE id = ` + q(b.exec) + ` RETURNING id`,
			"đọc system prompt của org B": `SELECT system_prompt FROM agent_versions WHERE id = ` + q(b.version),
			"step khai org B":             `INSERT INTO execution_steps (org_id, execution_id, task_id, step_seq, kind, status) VALUES (` + q(orgB) + `,` + q(b.exec) + `,` + q(b.task) + `,9,'llm_call','started') RETURNING id`,
			"usage khai org B":            `INSERT INTO usage_records (org_id, project_id, kind) VALUES (` + q(orgB) + `,` + q(b.project) + `,'llm') RETURNING id`,
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
			var pgErr *pgconn.PgError
			switch {
			case err == nil && affected > 0:
				t.Errorf("%s: tác động / thấy %d dòng, muốn bị từ chối", name, affected)
			case err == nil && noGrant[name]:
				t.Errorf("%s: câu lệnh chạy được (0 dòng); muốn bị từ chối vì thiếu quyền", name)
			case err != nil && (!errors.As(err, &pgErr) || pgErr.Code != "42501"):
				// Lỗi khác "thiếu quyền" (sai tên bảng, sai cột) nghĩa là ca test viết sai.
				t.Errorf("%s: lỗi không phải thiếu quyền: %v", name, err)
			}
			if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT s"); err != nil {
				return err
			}
		}
		// Chặn ở mức dòng, không phụ thuộc quyền của role: worker có quyền ghi các bảng
		// này nhưng không được phá bất biến của chúng. Mỗi câu phải bị trigger từ chối.
		for name, stmt := range map[string]string{
			"step trùng step_seq":            `INSERT INTO execution_steps (org_id, execution_id, task_id, step_seq, kind, status) VALUES (` + q(orgA) + `,` + q(a.exec) + `,` + q(a.task) + `,1,'llm_call','started')`,
			"step mới ở trạng thái kết thúc": `INSERT INTO execution_steps (org_id, execution_id, task_id, step_seq, kind, status, response) VALUES (` + q(orgA) + `,` + q(a.exec) + `,` + q(a.task) + `,7,'llm_call','succeeded','{"text":"giả"}')`,
			"execution quay lại queued":      `UPDATE agent_executions SET status = 'queued' WHERE id = ` + q(a.exec),
			"task quay lại queued":           `UPDATE agent_tasks SET status = 'queued' WHERE id = ` + q(a.task),
			"giảm iterations":                `UPDATE agent_tasks SET iterations = iterations + 5 WHERE id = ` + q(a.task) + `; UPDATE agent_tasks SET iterations = 0 WHERE id = ` + q(a.task),
			"giảm chi phí cộng dồn":          `UPDATE agent_executions SET estimated_cost = 2 WHERE id = ` + q(a.exec) + `; UPDATE agent_executions SET estimated_cost = 0 WHERE id = ` + q(a.exec),
			"sửa step đã kết thúc":           `UPDATE execution_steps SET status = 'succeeded', response = '{}' WHERE task_id = ` + q(a.task) + ` AND step_seq = 1; UPDATE execution_steps SET status = 'failed' WHERE task_id = ` + q(a.task) + ` AND step_seq = 1`,
			"đổi model của step đã kết thúc": `UPDATE execution_steps SET status = 'succeeded', response = '{}' WHERE task_id = ` + q(a.task) + ` AND step_seq = 1; UPDATE execution_steps SET model_id = gen_random_uuid() WHERE task_id = ` + q(a.task) + ` AND step_seq = 1`,
			"sửa execution đã kết thúc":      `UPDATE agent_executions SET status = 'completed' WHERE id = ` + q(a.exec) + `; UPDATE agent_executions SET status = 'failed', output = '{}' WHERE id = ` + q(a.exec),
		} {
			if _, err := tx.Exec(ctx, "SAVEPOINT s"); err != nil {
				return err
			}
			var err error
			for _, part := range strings.Split(stmt, "; ") {
				if _, err = tx.Exec(ctx, part); err != nil {
					break
				}
			}
			var pgErr *pgconn.PgError
			if err == nil {
				t.Errorf("%s: database chấp nhận", name)
			} else if !errors.As(err, &pgErr) || (pgErr.Code != "23001" && pgErr.Code != "23505") {
				t.Errorf("%s: lỗi không phải từ trigger chặn: %v", name, err)
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

	// Cấu hình task đã chụp không đổi được với MỌI role, kể cả role có quyền UPDATE cả bảng.
	for name, stmt := range map[string]string{
		"chuỗi model":   `UPDATE agent_tasks SET model_chain = ARRAY[gen_random_uuid()] WHERE id = $1`,
		"system prompt": `UPDATE agent_tasks SET system_prompt = 'khác' WHERE id = $1`,
		"goal":          `UPDATE agent_tasks SET goal = 'khác' WHERE id = $1`,
		"budget_limit":  `UPDATE agent_tasks SET budget_limit = 999 WHERE id = $1`,
		// Cấu trúc của kế hoạch (00021): task nào chạy và thấy kết quả của task nào.
		"plan_key":   `UPDATE agent_tasks SET plan_key = 'khac' WHERE id = $1`,
		"depends_on": `UPDATE agent_tasks SET depends_on = ARRAY[gen_random_uuid()] WHERE id = $1`,
		"on_failure": `UPDATE agent_tasks SET on_failure = 'continue' WHERE id = $1`,
		"depth":      `UPDATE agent_tasks SET depth = depth + 1 WHERE id = $1`,
		// Tóm tắt kết quả chỉ ghi được đúng lúc task kết thúc.
		"output_summary khi task chưa kết thúc": `UPDATE agent_tasks SET output_summary = 'chèn vào context của task khác' WHERE id = $1`,
	} {
		var pgErr *pgconn.PgError
		if _, err := db.Admin.Exec(ctx, stmt, a.task); err == nil || !errors.As(err, &pgErr) || pgErr.Code != "23001" {
			t.Errorf("superuser đổi %s của task: err = %v, muốn bị trigger từ chối", name, err)
		}
	}
	for name, stmt := range map[string]string{
		"plan": `UPDATE agent_executions SET plan = '{"tasks":[]}' WHERE id = $1`,
		"mode": `UPDATE agent_executions SET mode = 'planned' WHERE id = $1`,
	} {
		var pgErr *pgconn.PgError
		if _, err := db.Admin.Exec(ctx, stmt, a.exec); err == nil || !errors.As(err, &pgErr) || (pgErr.Code != "23001" && pgErr.Code != "23514") {
			t.Errorf("superuser đổi %s của execution: err = %v, muốn bị từ chối", name, err)
		}
	}
	// Ràng buộc của 00021: kế hoạch đi cùng chế độ; depends_on không trỏ ra ngoài execution;
	// tóm tắt có trần.
	for name, stmt := range map[string]string{
		"execution planned không có plan": `INSERT INTO agent_executions (org_id, project_id, initiator_type, mode, input, deadline_at)
			SELECT org_id, project_id, 'user', 'planned', '{}', now() + interval '1 hour' FROM agent_executions WHERE id = $1`,
		"execution single_agent có plan": `INSERT INTO agent_executions (org_id, project_id, initiator_type, mode, input, deadline_at, plan)
			SELECT org_id, project_id, 'user', 'single_agent', '{}', now() + interval '1 hour', '{}' FROM agent_executions WHERE id = $1`,
		"depends_on trỏ ra ngoài execution": `INSERT INTO agent_tasks (org_id, execution_id, agent_id, agent_version_id, goal, depends_on)
			SELECT org_id, execution_id, agent_id, agent_version_id, 'g', ARRAY[gen_random_uuid()] FROM agent_tasks WHERE execution_id = $1 LIMIT 1`,
	} {
		if _, err := db.Admin.Exec(ctx, stmt, a.exec); err == nil {
			t.Errorf("%s: chèn được, muốn bị database từ chối", name)
		}
	}
}
