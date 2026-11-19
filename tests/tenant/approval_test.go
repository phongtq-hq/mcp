package tenant_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	pgxv5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/hqgroups/mcp/internal/platform/identity"
	"github.com/hqgroups/mcp/internal/platform/tenancy"
	"github.com/hqgroups/mcp/internal/shared/ids"
	"github.com/hqgroups/mcp/internal/testutil/pgtest"
	"github.com/hqgroups/mcp/internal/workflow/approval"
)

// TestApprovalLifecycle: một approval gắn với đúng một hành động, dùng một lần, có hạn;
// vòng đời của nó được giữ ở database với mọi role; và bảng cách ly theo tenant.
func TestApprovalLifecycle(t *testing.T) {
	db := pgtest.Start(t)
	ctx := context.Background()
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
	requester, approver := ids.New(), ids.New()
	seed(`INSERT INTO users (id, idp_issuer, idp_subject, email) VALUES ($1,'iss','req','r@example.test'), ($2,'iss','app','a@example.test')`, requester, approver)
	for _, org := range []uuid.UUID{orgA, orgB} {
		f := fixture{ids.New(), ids.New(), ids.New(), ids.New(), ids.New()}
		fx[org] = f
		seed(`INSERT INTO projects (id, org_id, slug, name) VALUES ($1,$2,'p','P')`, f.project, org)
		seed(`INSERT INTO agents (id, org_id, project_id, slug, name) VALUES ($1,$2,$3,'a','A')`, f.agent, org, f.project)
		seed(`INSERT INTO agent_versions (id, org_id, agent_id, version, system_prompt, config, config_hash) VALUES ($1,$2,$3,1,'p','{}','\x00')`, f.version, org, f.agent)
		seed(`INSERT INTO agent_executions (id, org_id, project_id, initiator_type, initiator_id, mode, status, input, deadline_at)
		      VALUES ($1,$2,$3,'user',$4,'single_agent','running','{}', now() + interval '1 hour')`, f.exec, org, f.project, requester)
		seed(`INSERT INTO agent_tasks (id, org_id, execution_id, agent_id, agent_version_id, goal, status) VALUES ($1,$2,$3,$4,$5,'g','running')`,
			f.task, org, f.exec, f.agent, f.version)
	}
	a, b := fx[orgA], fx[orgB]
	tool := ids.New()
	gw := approval.NewService(db.PoolAs(t, "app_gateway", 2))
	api := approval.NewService(db.PoolAs(t, "app_api", 2))
	worker := approval.NewService(db.PoolAs(t, "app_worker", 1))
	inOrg := func(org uuid.UUID) context.Context { return tenancy.WithOrg(ctx, org) }
	as := func(org, user uuid.UUID) context.Context {
		return identity.WithPrincipal(inOrg(org), identity.Principal{Type: identity.PrincipalUser, ID: user, OrgID: org})
	}
	req := func(f fixture, step int, hash string) approval.Request {
		return approval.Request{ProjectID: f.project, ExecutionID: f.exec, TaskID: f.task, StepSeq: step, ToolID: tool,
			ToolName: "ops.restart", ServerName: "ops", RiskLevel: "destructive", ArgsHash: []byte(hash),
			Arguments: []byte(`{"name":"api"}`), AgentID: f.agent, RequestedByType: "user", RequestedBy: requester,
			ExpiresAt: time.Now().Add(time.Hour)}
	}
	resolve := func(r approval.Request) approval.State {
		t.Helper()
		st, err := gw.Resolve(inOrg(orgA), r)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		return st
	}

	t.Run("bound to one action, single use", func(t *testing.T) {
		st := resolve(req(a, 1, "hash-1"))
		if !st.Created || st.Status != "pending" || st.Granted {
			t.Fatalf("lần đầu: %+v", st)
		}
		// Hỏi lại: vẫn là approval đó, vẫn chờ.
		if again := resolve(req(a, 1, "hash-1")); again.ID != st.ID || again.Created || again.Status != "pending" {
			t.Errorf("hỏi lại: %+v", again)
		}
		// Worker (đang chờ) chỉ đọc được trạng thái, và chỉ của task của nó.
		if s, err := worker.Status(inOrg(orgA), a.task, st.ID); err != nil || s != "pending" {
			t.Errorf("worker đọc trạng thái: %q %v", s, err)
		}
		if _, err := worker.Status(inOrg(orgA), b.task, st.ID); err == nil {
			t.Error("đọc được approval qua task khác")
		}
		if _, err := worker.Status(inOrg(orgB), a.task, st.ID); err == nil {
			t.Error("org B đọc được trạng thái approval của org A")
		}
		// Người yêu cầu không tự duyệt; API key / principal không phải user không duyệt được.
		if _, err := api.Decide(as(orgA, requester), st.ID, "approve", ""); !errors.Is(err, approval.ErrSelfApproval) {
			t.Errorf("tự duyệt: %v", err)
		}
		key := identity.WithPrincipal(inOrg(orgA), identity.Principal{Type: identity.PrincipalAPIKey, ID: ids.New(), OrgID: orgA})
		if _, err := api.Decide(key, st.ID, "approve", ""); err == nil {
			t.Error("API key duyệt được approval")
		}
		// Org B không thấy và không quyết định được.
		if _, err := api.Get(inOrg(orgB), st.ID); err == nil {
			t.Error("org B đọc được approval của org A")
		}
		if _, err := api.Decide(as(orgB, approver), st.ID, "approve", ""); err == nil {
			t.Error("org B quyết định được approval của org A")
		}
		got, err := api.Decide(as(orgA, approver), st.ID, "approve", "ổn")
		if err != nil || got.Status != "approved" || len(got.Decisions) != 1 || got.Decisions[0].UserID != approver {
			t.Fatalf("duyệt: %+v %v", got, err)
		}
		if _, err := api.Decide(as(orgA, approver), st.ID, "reject", ""); !errors.Is(err, approval.ErrNotPending) {
			t.Errorf("quyết định lần hai: %v", err)
		}
		// Arguments khác hoặc tool khác: approval không khớp, và KHÔNG bị tiêu thụ.
		if m := resolve(req(a, 1, "hash-OTHER")); m.Status != approval.StatusMismatch || m.Granted {
			t.Errorf("arguments khác: %+v", m)
		}
		other := req(a, 1, "hash-1")
		other.ToolID = ids.New()
		if m := resolve(other); m.Status != approval.StatusMismatch || m.Granted {
			t.Errorf("tool khác: %+v", m)
		}
		// Đúng hành động: được cấp, approval bị tiêu thụ đúng một lần.
		if g := resolve(req(a, 1, "hash-1")); !g.Granted || g.Status != "consumed" {
			t.Fatalf("hành động đã duyệt: %+v", g)
		}
		var consumed int
		if err := db.Admin.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action = 'approval.consume' AND resource_id = $1`, st.ID).Scan(&consumed); err != nil || consumed != 1 {
			t.Errorf("audit approval.consume: %d %v", consumed, err)
		}
		// Hỏi lại sau khi đã tiêu thụ: vẫn "được cấp cho chính hành động này" (bên gọi chống
		// chạy trùng bằng claim), nhưng không ghi thêm lần tiêu thụ nào.
		if g := resolve(req(a, 1, "hash-1")); !g.Granted {
			t.Errorf("hỏi lại sau khi tiêu thụ: %+v", g)
		}
		if err := db.Admin.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action = 'approval.consume' AND resource_id = $1`, st.ID).Scan(&consumed); err != nil || consumed != 1 {
			t.Errorf("tiêu thụ được ghi %d lần", consumed)
		}
	})

	t.Run("rejected, expired and cancelled approvals grant nothing", func(t *testing.T) {
		rej := resolve(req(a, 2, "h"))
		if _, err := api.Decide(as(orgA, approver), rej.ID, "reject", ""); err != nil {
			t.Fatal(err)
		}
		if st := resolve(req(a, 2, "h")); st.Granted || st.Status != "rejected" {
			t.Errorf("bị từ chối: %+v", st)
		}
		// Hết hạn: được duyệt rồi nhưng quá hạn trước khi dùng thì không còn giá trị.
		short := req(a, 3, "h")
		short.ExpiresAt = time.Now().Add(300 * time.Millisecond)
		exp := resolve(short)
		if _, err := api.Decide(as(orgA, approver), exp.ID, "approve", ""); err != nil {
			t.Fatal(err)
		}
		time.Sleep(400 * time.Millisecond)
		if st := resolve(req(a, 3, "h")); st.Granted || st.Status != "expired" {
			t.Errorf("đã duyệt nhưng quá hạn: %+v", st)
		}
		if got, _ := api.Get(inOrg(orgA), exp.ID); got.Status != "expired" {
			t.Errorf("approval quá hạn hiển thị: %s", got.Status)
		}
		// Yêu cầu với hạn đã qua không tạo approval.
		past := req(a, 4, "h")
		past.ExpiresAt = time.Now().Add(-time.Second)
		if st := resolve(past); st.Granted || st.Status != "expired" || st.ID != uuid.Nil {
			t.Errorf("hạn đã qua: %+v", st)
		}
		// Huỷ theo execution: cả approval đang chờ lẫn đã duyệt chưa dùng.
		pend, appr := resolve(req(a, 5, "h")), resolve(req(a, 6, "h"))
		if _, err := api.Decide(as(orgA, approver), appr.ID, "approve", ""); err != nil {
			t.Fatal(err)
		}
		if err := api.CancelForExecution(inOrg(orgB), a.exec); err != nil {
			t.Fatal(err)
		}
		if st := resolve(req(a, 5, "h")); st.Status != "pending" {
			t.Errorf("org B huỷ được approval của org A: %+v", st)
		}
		if err := api.CancelForExecution(inOrg(orgA), a.exec); err != nil {
			t.Fatal(err)
		}
		for _, st := range []approval.State{resolve(req(a, 5, "h")), resolve(req(a, 6, "h"))} {
			if st.Granted || st.Status != "cancelled" {
				t.Errorf("sau khi huỷ execution: %+v (pending %s, approved %s)", st, pend.ID, appr.ID)
			}
		}
	})

	t.Run("the lifecycle and the claims table are enforced by the database for every role", func(t *testing.T) {
		st := resolve(req(a, 7, "h"))
		q := func(id uuid.UUID) string { return "'" + id.String() + "'" }
		// Mọi câu dưới đây chạy bằng superuser (không bị GRANT hay RLS cản): chỉ trigger chặn.
		for name, stmt := range map[string]string{
			"đổi arguments đã duyệt":      `UPDATE approvals SET args_hash = '\x01' WHERE id = ` + q(st.ID),
			"đổi payload":                 `UPDATE approvals SET payload = '{}' WHERE id = ` + q(st.ID),
			"đổi tool":                    `UPDATE approvals SET tool_id = gen_random_uuid() WHERE id = ` + q(st.ID),
			"kéo dài hạn":                 `UPDATE approvals SET expires_at = now() + interval '1 year' WHERE id = ` + q(st.ID),
			"đổi người yêu cầu":           `UPDATE approvals SET requested_by = gen_random_uuid() WHERE id = ` + q(st.ID),
			"pending thẳng sang consumed": `UPDATE approvals SET status = 'consumed' WHERE id = ` + q(st.ID),
		} {
			_, err := db.Admin.Exec(ctx, stmt)
			var pgErr *pgconn.PgError
			if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "23001" {
				t.Errorf("%s: %v, muốn bị trigger từ chối", name, err)
			}
		}
		if _, err := api.Decide(as(orgA, approver), st.ID, "reject", ""); err != nil {
			t.Fatal(err)
		}
		for name, stmt := range map[string]string{
			"rejected quay lại approved": `UPDATE approvals SET status = 'approved' WHERE id = ` + q(st.ID),
			"rejected sang consumed":     `UPDATE approvals SET status = 'consumed' WHERE id = ` + q(st.ID),
			"xoá quyết định":             `UPDATE approvals SET decisions = '[]' WHERE id = ` + q(st.ID),
			"sửa quyết định":             `UPDATE approvals SET decisions = jsonb_set(decisions, '{0,decision}', '"approve"') WHERE id = ` + q(st.ID),
		} {
			_, err := db.Admin.Exec(ctx, stmt)
			var pgErr *pgconn.PgError
			if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "23001" {
				t.Errorf("%s: %v, muốn bị trigger từ chối", name, err)
			}
		}

		// Quyền theo role: gateway không quyết định được; api không tạo được; worker không ghi gì;
		// claim chỉ thêm.
		seed(`INSERT INTO mcp_tool_claims (org_id, task_id, step_seq, tool_id, args_hash) VALUES ($1,$2,1,$3,'\x00'), ($4,$5,1,$3,'\x00')`,
			orgA, a.task, tool, orgB, b.task)
		for role, stmts := range map[string][]string{
			"app_gateway": {
				`UPDATE approvals SET decisions = decisions || '[{"x":1}]'::jsonb WHERE id = ` + q(st.ID),
				`DELETE FROM approvals WHERE id = ` + q(st.ID),
				`UPDATE mcp_tool_claims SET args_hash = '\x01'`,
				`DELETE FROM mcp_tool_claims`,
			},
			"app_api": {
				`INSERT INTO approvals (org_id, project_id, subject_type, payload, risk_level, requested_by_type, expires_at) VALUES (` + q(orgA) + `,` + q(a.project) + `,'workflow_step','{}','read','user', now())`,
				`DELETE FROM approvals WHERE id = ` + q(st.ID),
				`SELECT task_id FROM mcp_tool_claims LIMIT 1`,
			},
			"app_worker": {
				`UPDATE approvals SET status = 'approved' WHERE id = ` + q(st.ID),
				`SELECT payload FROM approvals LIMIT 1`,
				`SELECT decisions FROM approvals LIMIT 1`,
				`SELECT task_id FROM mcp_tool_claims LIMIT 1`,
				`INSERT INTO mcp_tool_claims (org_id, task_id, step_seq, tool_id, args_hash) VALUES (` + q(orgA) + `,` + q(a.task) + `,9,` + q(tool) + `,'\x00')`,
			},
		} {
			err := tenancy.InTx(inOrg(orgA), db.PoolAs(t, role, 1), func(tx pgxv5.Tx) error {
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
		// Cách ly tenant của cả hai bảng dưới role của Gateway.
		err := tenancy.InTx(inOrg(orgA), db.PoolAs(t, "app_gateway", 1), func(tx pgxv5.Tx) error {
			for _, table := range []string{"approvals", "mcp_tool_claims"} {
				var n, foreign int
				if err := tx.QueryRow(ctx, "SELECT count(*), count(*) FILTER (WHERE org_id = $1) FROM "+table, orgB).Scan(&n, &foreign); err != nil {
					return err
				}
				if n == 0 || foreign != 0 {
					t.Errorf("org A thấy %d dòng trong %s (%d của org B)", n, table, foreign)
				}
			}
			for name, stmt := range map[string]string{
				"approval khai org B":          `INSERT INTO approvals (org_id, project_id, subject_type, payload, risk_level, requested_by_type, expires_at) VALUES (` + q(orgB) + `,` + q(b.project) + `,'workflow_step','{}','read','user', now()) RETURNING id`,
				"approval org A cho project B": `INSERT INTO approvals (org_id, project_id, subject_type, payload, risk_level, requested_by_type, expires_at) VALUES (` + q(orgA) + `,` + q(b.project) + `,'workflow_step','{}','read','user', now()) RETURNING id`,
				"approval org A cho execution B": `INSERT INTO approvals (org_id, project_id, execution_id, task_id, step_seq, subject_type, tool_id, tool_name, args_hash, payload, risk_level, requested_by_type, expires_at)
					VALUES (` + q(orgA) + `,` + q(a.project) + `,` + q(b.exec) + `,` + q(b.task) + `,1,'tool_call',` + q(tool) + `,'t','\x00','{}','write','user', now()) RETURNING id`,
				"claim khai org B":     `INSERT INTO mcp_tool_claims (org_id, task_id, step_seq, tool_id, args_hash) VALUES (` + q(orgB) + `,` + q(b.task) + `,2,` + q(tool) + `,'\x00') RETURNING task_id`,
				"tool_call thiếu bước": `INSERT INTO approvals (org_id, project_id, subject_type, payload, risk_level, requested_by_type, expires_at) VALUES (` + q(orgA) + `,` + q(a.project) + `,'tool_call','{}','write','user', now()) RETURNING id`,
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
	})
}
