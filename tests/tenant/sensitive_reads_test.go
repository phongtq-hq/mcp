package tenant_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	pgxv5 "github.com/jackc/pgx/v5"

	"github.com/hqgroups/mcp/internal/platform/tenancy"
	"github.com/hqgroups/mcp/internal/shared/ids"
	"github.com/hqgroups/mcp/internal/testutil/pgtest"
)

// mcp_execution_sensitive_reads (migration 00040): Gateway ghi khi một execution đọc tool
// sensitive. Cách ly theo org, chỉ thêm, không gắn được vào execution của org khác, và
// chỉ app_gateway ghi được.
func TestSensitiveReadsIsolation(t *testing.T) {
	db := pgtest.Start(t)
	ctx := context.Background()
	orgA, orgB := ids.New(), ids.New()
	execs := map[uuid.UUID]uuid.UUID{}
	seed := func(sql string, args ...any) {
		t.Helper()
		if _, err := db.Admin.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	seed(`INSERT INTO organizations (id, slug, name) VALUES ($1,'a','A'), ($2,'b','B')`, orgA, orgB)
	user := ids.New()
	seed(`INSERT INTO users (id, idp_issuer, idp_subject, email) VALUES ($1,'iss','u','u@example.test')`, user)
	for _, org := range []uuid.UUID{orgA, orgB} {
		project, exec := ids.New(), ids.New()
		execs[org] = exec
		seed(`INSERT INTO projects (id, org_id, slug, name) VALUES ($1,$2,'p','P')`, project, org)
		seed(`INSERT INTO agent_executions (id, org_id, project_id, initiator_type, initiator_id, mode, status, input, deadline_at)
		      VALUES ($1,$2,$3,'user',$4,'single_agent','running','{}', now() + interval '1 hour')`, exec, org, project, user)
	}
	tool := ids.New()
	gw := db.PoolAs(t, "app_gateway", 1)
	inOrg := func(org uuid.UUID) context.Context { return tenancy.WithOrg(ctx, org) }
	exec := func(role string, org uuid.UUID, stmt string, args ...any) error {
		return tenancy.InTx(inOrg(org), db.PoolAs(t, role, 1), func(tx pgxv5.Tx) error {
			_, err := tx.Exec(ctx, stmt, args...)
			return err
		})
	}

	// Gateway ghi cho execution của chính org; ghi lại là no-op.
	for range 2 {
		if err := tenancy.InTx(inOrg(orgA), gw, func(tx pgxv5.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO mcp_execution_sensitive_reads (org_id, execution_id, first_tool_id) VALUES ($1,$2,$3)
				ON CONFLICT (org_id, execution_id) DO NOTHING`, orgA, execs[orgA], tool)
			return err
		}); err != nil {
			t.Fatalf("gateway ghi: %v", err)
		}
	}
	// Org khác không thấy dòng đó.
	var n int
	if err := tenancy.InTx(inOrg(orgB), gw, func(tx pgxv5.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM mcp_execution_sensitive_reads`).Scan(&n)
	}); err != nil || n != 0 {
		t.Errorf("org B thấy %d dòng (%v)", n, err)
	}
	// Ghi thay org khác, hoặc gắn vào execution của org khác: bị từ chối.
	if err := exec("app_gateway", orgA, `INSERT INTO mcp_execution_sensitive_reads (org_id, execution_id, first_tool_id) VALUES ($1,$2,$3)`,
		orgB, execs[orgB], tool); err == nil {
		t.Error("ghi được dòng của org khác")
	}
	if err := exec("app_gateway", orgA, `INSERT INTO mcp_execution_sensitive_reads (org_id, execution_id, first_tool_id) VALUES ($1,$2,$3)`,
		orgA, execs[orgB], tool); err == nil {
		t.Error("gắn được vào execution của org khác")
	}
	// Chỉ thêm, và chỉ gateway ghi.
	for role, stmts := range map[string][]string{
		"app_gateway": {`UPDATE mcp_execution_sensitive_reads SET first_tool_id = first_tool_id`, `DELETE FROM mcp_execution_sensitive_reads`},
		"app_api":     {`SELECT 1 FROM mcp_execution_sensitive_reads`, `DELETE FROM mcp_execution_sensitive_reads`},
		"app_worker":  {`SELECT 1 FROM mcp_execution_sensitive_reads`, `DELETE FROM mcp_execution_sensitive_reads`},
	} {
		for _, stmt := range stmts {
			err := exec(role, orgA, stmt)
			var pgErr *pgconn.PgError
			if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "42501" {
				t.Errorf("%s: %q → %v, muốn bị từ chối vì thiếu quyền", role, stmt, err)
			}
		}
	}
}

// mcp_server_projects (migration 00041): cách ly theo org; không gắn được server của org này
// với project của org khác, kể cả bằng tài khoản quản trị; chỉ app_admin ghi được.
func TestMCPServerProjectsIsolation(t *testing.T) {
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
	projA, projB, server := ids.New(), ids.New(), ids.New()
	seed(`INSERT INTO projects (id, org_id, slug, name) VALUES ($1,$2,'p','P'), ($3,$4,'p','P')`, projA, orgA, projB, orgB)
	seed(`INSERT INTO mcp_servers (id, org_id, slug, name, kind, transport, endpoint, auth_method) VALUES ($1,$2,'srv-a','S','managed','streamable_http','http://x/mcp','service_token')`, server, orgA)
	seed(`INSERT INTO mcp_server_projects (org_id, server_id, project_id) VALUES ($1,$2,$3)`, orgA, server, projA)

	for name, args := range map[string][]any{
		"project của org khác":         {orgA, server, projB},
		"server của org khác (dưới B)": {orgB, server, projB},
	} {
		if _, err := db.Admin.Exec(ctx, `INSERT INTO mcp_server_projects (org_id, server_id, project_id) VALUES ($1,$2,$3)`, args...); err == nil {
			t.Errorf("%s: gắn được", name)
		}
	}
	if _, err := db.Admin.Exec(ctx, `UPDATE mcp_servers SET project_scoped = true WHERE org_id IS NULL`); err != nil {
		t.Fatal(err) // không có server của platform: 0 dòng, không lỗi
	}
	platform := ids.New()
	seed(`INSERT INTO mcp_servers (id, slug, name, kind, transport, endpoint, auth_method) VALUES ($1,'plat','P','managed','streamable_http','http://y/mcp','service_token')`, platform)
	if _, err := db.Admin.Exec(ctx, `UPDATE mcp_servers SET project_scoped = true WHERE id = $1`, platform); err == nil {
		t.Error("server của platform giới hạn theo project được")
	}

	count := func(role string, org uuid.UUID) (int, error) {
		var n int
		err := tenancy.InTx(tenancy.WithOrg(ctx, org), db.PoolAs(t, role, 1), func(tx pgxv5.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM mcp_server_projects`).Scan(&n)
		})
		return n, err
	}
	for _, role := range []string{"app_api", "app_gateway"} {
		if n, err := count(role, orgA); err != nil || n != 1 {
			t.Errorf("%s đọc org A: %d %v", role, n, err)
		}
		if n, err := count(role, orgB); err != nil || n != 0 {
			t.Errorf("%s thấy dòng của org khác: %d %v", role, n, err)
		}
		err := tenancy.InTx(tenancy.WithOrg(ctx, orgA), db.PoolAs(t, role, 1), func(tx pgxv5.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM mcp_server_projects`)
			return err
		})
		var pgErr *pgconn.PgError
		if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Errorf("%s xoá được binding: %v", role, err)
		}
	}
}
