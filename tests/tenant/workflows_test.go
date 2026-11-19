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

// TestWorkflowTables kiểm các bảng của workflow do người dùng định nghĩa (migration
// 00029–00031) ngay trên database: cách ly tenant, quyền của từng role, và các bất biến
// do trigger giữ (bản chụp của lần chạy, trạng thái kết thúc, cấu hình của nút chờ).
func TestWorkflowTables(t *testing.T) {
	db := pgtest.Start(t)
	ctx := context.Background()
	seed := func(sql string, args ...any) {
		t.Helper()
		if _, err := db.Admin.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, sql)
		}
	}
	type fixture struct{ org, project, user, wf, run, exec uuid.UUID }
	newFixture := func(slug string) fixture {
		f := fixture{ids.New(), ids.New(), ids.New(), ids.New(), ids.New(), ids.New()}
		seed(`INSERT INTO organizations (id, slug, name) VALUES ($1,$2,$2)`, f.org, slug)
		seed(`INSERT INTO projects (id, org_id, slug, name) VALUES ($1,$2,'p','P')`, f.project, f.org)
		seed(`INSERT INTO users (id, idp_issuer, idp_subject, email) VALUES ($1,'iss',$2,$3)`, f.user, slug, slug+"@example.test")
		seed(`INSERT INTO workflows (id, org_id, project_id, slug, name, definition, status, run_as) VALUES ($1,$2,$3,'wf','WF','{"steps":[]}','active',$4)`,
			f.wf, f.org, f.project, f.user)
		seed(`INSERT INTO workflow_executions (id, org_id, project_id, workflow_id, workflow_version, definition_snapshot, trigger_type, triggered_by)
		      VALUES ($1,$2,$3,$4,1,'{"steps":[]}','manual',$5)`, f.run, f.org, f.project, f.wf, f.user)
		seed(`INSERT INTO agent_executions (id, org_id, project_id, initiator_type, initiator_id, mode, status, input, deadline_at, plan_source, workflow_execution_id, plan)
		      VALUES ($1,$2,$3,'user',$5,'planned','running','{}', now() + interval '1 hour','caller',$4,
		        '{"tasks":[{"id":"pause","kind":"wait"},{"id":"gate","kind":"approval","depends_on":["pause"]},{"id":"after","depends_on":["gate"]},{"id":"far","depends_on":["after"],"on_failure":"continue"}]}')`,
			f.exec, f.org, f.project, f.run, f.user)
		seed(`INSERT INTO execution_plan_steps (org_id, execution_id, plan_key, kind, config) VALUES ($1,$2,'pause','wait','{"seconds":5}')`, f.org, f.exec)
		return f
	}
	a, b := newFixture("wf-a"), newFixture("wf-b")
	q := func(id uuid.UUID) string { return "'" + id.String() + "'" }
	// check chạy từng câu dưới một role trong tenant của org: câu phải không thấy / không
	// tác động dòng nào; câu có tên trong grant phải bị từ chối vì thiếu quyền (42501).
	check := func(role string, org uuid.UUID, stmts map[string]string, grant map[string]bool) {
		t.Helper()
		p := db.PoolAs(t, role, 1)
		err := tenancy.InTx(tenancy.WithOrg(ctx, org), p, func(tx pgxv5.Tx) error {
			for name, stmt := range stmts {
				if _, err := tx.Exec(ctx, "SAVEPOINT s"); err != nil {
					return err
				}
				rows, err := tx.Query(ctx, stmt)
				n := 0
				if err == nil {
					for rows.Next() {
						n++
					}
					err = rows.Err()
					rows.Close()
				}
				var pgErr *pgconn.PgError
				switch {
				case err == nil && n > 0:
					t.Errorf("%s (%s): thấy / tác động %d dòng, muốn bị từ chối", name, role, n)
				case err == nil && grant[name]:
					t.Errorf("%s (%s): chạy được, muốn thiếu quyền", name, role)
				case err != nil && grant[name] && (!errors.As(err, &pgErr) || pgErr.Code != "42501"):
					t.Errorf("%s (%s): err = %v, muốn 42501", name, role, err)
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

	check("app_api", a.org, map[string]string{
		"đọc workflow của org B":           `SELECT id FROM workflows WHERE id = ` + q(b.wf),
		"liệt kê không lọc":                `SELECT id FROM workflows WHERE org_id <> ` + q(a.org),
		"đọc lần chạy của org B":           `SELECT id FROM workflow_executions WHERE id = ` + q(b.run),
		"đọc nút chờ của org B":            `SELECT plan_key FROM execution_plan_steps WHERE execution_id = ` + q(b.exec),
		"sửa workflow của org B":           `UPDATE workflows SET name = 'x' WHERE id = ` + q(b.wf) + ` RETURNING id`,
		"sửa lần chạy của org B":           `UPDATE workflow_executions SET status = 'failed' WHERE id = ` + q(b.run) + ` RETURNING id`,
		"workflow khai org B":              `INSERT INTO workflows (org_id, project_id, slug, name, definition) VALUES (` + q(b.org) + `,` + q(b.project) + `,'x1','x','{}') RETURNING id`,
		"workflow trong project của B":     `INSERT INTO workflows (org_id, project_id, slug, name, definition) VALUES (` + q(a.org) + `,` + q(b.project) + `,'x2','x','{}') RETURNING id`,
		"lần chạy của workflow org B":      `INSERT INTO workflow_executions (org_id, project_id, workflow_id, workflow_version, definition_snapshot, trigger_type) VALUES (` + q(a.org) + `,` + q(a.project) + `,` + q(b.wf) + `,1,'{}','manual') RETURNING id`,
		"execution trỏ lần chạy của B":     `UPDATE agent_executions SET workflow_execution_id = ` + q(b.run) + ` WHERE id = ` + q(a.exec) + ` RETURNING id`,
		"nút chờ cho execution của B":      `INSERT INTO execution_plan_steps (org_id, execution_id, plan_key, kind, config) VALUES (` + q(a.org) + `,` + q(b.exec) + `,'w2','wait','{"seconds":1}') RETURNING plan_key`,
		"workflow active không người chạy": `INSERT INTO workflows (org_id, project_id, slug, name, definition, status) VALUES (` + q(a.org) + `,` + q(a.project) + `,'x3','x','{}','active') RETURNING id`,
		"xoá workflow":                     `DELETE FROM workflows WHERE id = ` + q(a.wf) + ` RETURNING id`,
		"xoá lần chạy":                     `DELETE FROM workflow_executions WHERE id = ` + q(a.run) + ` RETURNING id`,
		"đổi project của workflow":         `UPDATE workflows SET project_id = ` + q(b.project) + ` WHERE id = ` + q(a.wf) + ` RETURNING id`,
		"đổi bản chụp của lần chạy":        `UPDATE workflow_executions SET definition_snapshot = '{}' WHERE id = ` + q(a.run) + ` RETURNING id`,
		"xoá nút chờ":                      `DELETE FROM execution_plan_steps WHERE execution_id = ` + q(a.exec) + ` RETURNING plan_key`,
		"đổi cấu hình nút chờ":             `UPDATE execution_plan_steps SET config = '{"seconds":1}' WHERE execution_id = ` + q(a.exec) + ` RETURNING plan_key`,
		// Trigger và ràng buộc: dòng mới bắt đầu ở queued; định danh và version của workflow được giữ.
		"lần chạy chèn sẵn kết quả":         `INSERT INTO workflow_executions (org_id, project_id, workflow_id, workflow_version, definition_snapshot, trigger_type, status) VALUES (` + q(a.org) + `,` + q(a.project) + `,` + q(a.wf) + `,1,'{}','manual','completed') RETURNING id`,
		"lần chạy thủ công có khoá lịch":    `INSERT INTO workflow_executions (org_id, project_id, workflow_id, workflow_version, definition_snapshot, trigger_type, trigger_key) VALUES (` + q(a.org) + `,` + q(a.project) + `,` + q(a.wf) + `,1,'{}','manual','k') RETURNING id`,
		"nút chờ chèn sẵn kết quả":          `INSERT INTO execution_plan_steps (org_id, execution_id, plan_key, kind, config, status) VALUES (` + q(a.org) + `,` + q(a.exec) + `,'w4','wait','{"seconds":1}','completed') RETURNING plan_key`,
		"nút chờ thiếu seconds":             `INSERT INTO execution_plan_steps (org_id, execution_id, plan_key, kind, config) VALUES (` + q(a.org) + `,` + q(a.exec) + `,'w5','wait','{}') RETURNING plan_key`,
		"đổi definition không tăng version": `UPDATE workflows SET definition = '{"x":1}' WHERE id = ` + q(a.wf) + ` RETURNING id`,
		"giảm version":                      `UPDATE workflows SET version = version - 1, definition = '{"x":2}' WHERE id = ` + q(a.wf) + ` RETURNING id`,
	}, map[string]bool{"xoá workflow": true, "xoá lần chạy": true, "đổi project của workflow": true, "đổi bản chụp của lần chạy": true,
		"xoá nút chờ": true, "đổi cấu hình nút chờ": true})

	// Process chạy vòng lặp không đọc định nghĩa workflow; với nút chờ nó chỉ đổi trạng thái.
	check("app_worker", a.org, map[string]string{
		"đọc workflow":             `SELECT id FROM workflows`,
		"đọc lần chạy":             `SELECT id FROM workflow_executions`,
		"sửa lần chạy":             `UPDATE workflow_executions SET status = 'completed' WHERE id = ` + q(a.run) + ` RETURNING id`,
		"tạo nút chờ":              `INSERT INTO execution_plan_steps (org_id, execution_id, plan_key, kind, config) VALUES (` + q(a.org) + `,` + q(a.exec) + `,'w3','wait','{"seconds":1}') RETURNING plan_key`,
		"đổi cấu hình nút chờ":     `UPDATE execution_plan_steps SET config = '{"seconds":1}' WHERE execution_id = ` + q(a.exec) + ` RETURNING plan_key`,
		"đổi trạng thái nút của B": `UPDATE execution_plan_steps SET status = 'running' WHERE execution_id = ` + q(b.exec) + ` RETURNING plan_key`,
		"đọc nút chờ của org B":    `SELECT plan_key FROM execution_plan_steps WHERE execution_id = ` + q(b.exec),
	}, map[string]bool{"đọc workflow": true, "đọc lần chạy": true, "sửa lần chạy": true, "tạo nút chờ": true, "đổi cấu hình nút chờ": true})
	check("app_gateway", a.org, map[string]string{
		"đọc workflow": `SELECT id FROM workflows`,
		"đọc lần chạy": `SELECT id FROM workflow_executions`,
		"đọc nút chờ":  `SELECT plan_key FROM execution_plan_steps`,
	}, map[string]bool{"đọc workflow": true, "đọc lần chạy": true, "đọc nút chờ": true})

	// Worker đổi được trạng thái nút chờ của org mình, theo một chiều.
	worker := db.PoolAs(t, "app_worker", 1)
	step := func(status string) error {
		return tenancy.InTx(tenancy.WithOrg(ctx, a.org), worker, func(tx pgxv5.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE execution_plan_steps SET status = $2 WHERE execution_id = $1 AND plan_key = 'pause'`, a.exec, status)
			return err
		})
	}
	if err := step("completed"); err == nil {
		t.Error("nút chờ chưa chạy được ghi là completed")
	}
	if err := step("running"); err != nil {
		t.Fatalf("worker không chuyển được nút chờ sang running: %v", err)
	}
	if err := step("queued"); err == nil {
		t.Error("nút chờ quay lại queued được")
	}
	// "completed" là điều mở các bước phụ thuộc: database tự giữ luật "đã chờ đủ" (nút này
	// chờ 5 giây và vừa bắt đầu), với mọi role.
	if err := step("completed"); err == nil {
		t.Error("nút chờ chưa chờ đủ được ghi là completed")
	}
	// Nút của org B đã bắt đầu từ 10 giây trước: đóng được.
	seed(`UPDATE execution_plan_steps SET status = 'running', started_at = now() - interval '10 seconds' WHERE execution_id = $1 AND plan_key = 'pause'`, b.exec)
	if err := tenancy.InTx(tenancy.WithOrg(ctx, b.org), worker, func(tx pgxv5.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE execution_plan_steps SET status = 'completed' WHERE execution_id = $1 AND plan_key = 'pause'`, b.exec)
		return err
	}); err != nil {
		t.Fatalf("worker không đóng được nút chờ đã chờ đủ: %v", err)
	}
	if err := step("cancelled"); err != nil {
		t.Fatalf("worker không huỷ được nút chờ: %v", err)
	}
	if err := step("failed"); err == nil {
		t.Error("nút chờ đã kết thúc vẫn đổi trạng thái được")
	}

	// Với MỌI role (kể cả superuser): bản chụp của lần chạy bất biến, execution đã gắn
	// không đổi được, lần chạy đã kết thúc không đổi nữa.
	seed(`UPDATE workflow_executions SET execution_id = $2, status = 'running' WHERE id = $1`, a.run, a.exec)
	for name, stmt := range map[string]string{
		"đổi bản chụp":        `UPDATE workflow_executions SET definition_snapshot = '{"x":1}' WHERE id = $1`,
		"đổi version":         `UPDATE workflow_executions SET workflow_version = 9 WHERE id = $1`,
		"đổi người kích hoạt": `UPDATE workflow_executions SET triggered_by = NULL WHERE id = $1`,
		"đổi input":           `UPDATE workflow_executions SET input = '{"message":"x"}' WHERE id = $1`,
		"đổi execution":       `UPDATE workflow_executions SET execution_id = gen_random_uuid() WHERE id = $1`,
		"quay lại queued":     `UPDATE workflow_executions SET status = 'queued' WHERE id = $1`,
		"đổi project":         `UPDATE workflow_executions SET project_id = gen_random_uuid() WHERE id = $1`,
	} {
		if _, err := db.Admin.Exec(ctx, stmt, a.run); err == nil {
			t.Errorf("%s: chạy được, muốn bị database từ chối", name)
		}
	}
	seed(`UPDATE workflow_executions SET status = 'completed', finished_at = now() WHERE id = $1`, a.run)
	if _, err := db.Admin.Exec(ctx, `UPDATE workflow_executions SET status = 'failed' WHERE id = $1`, a.run); err == nil {
		t.Error("lần chạy đã kết thúc vẫn đổi trạng thái được")
	}
	// Liên kết execution → lần chạy là một phần của cấu hình đã chụp, kể cả với superuser.
	if _, err := db.Admin.Exec(ctx, `UPDATE agent_executions SET workflow_execution_id = $2 WHERE id = $1`, a.exec, b.run); err == nil {
		t.Error("execution đổi được lần chạy workflow của nó")
	}
	// Một lần chạy có nhiều nhất một execution.
	if _, err := db.Admin.Exec(ctx, `INSERT INTO agent_executions (id, org_id, project_id, initiator_type, mode, status, input, deadline_at, plan_source, workflow_execution_id, plan)
		VALUES ($1,$2,$3,'user','planned','running','{}', now() + interval '1 hour','caller',$4,'{"tasks":[]}')`, ids.New(), a.org, a.project, a.run); err == nil {
		t.Error("một lần chạy có được hai execution")
	}
	// Nút approval: process chạy kế hoạch không ghi được vào approvals; approval của nút chỉ
	// đi qua các hàm trong database, với nội dung lấy từ cấu hình đã chụp của nút.
	c := newFixture("wf-c")
	seed(`INSERT INTO execution_plan_steps (org_id, execution_id, plan_key, kind, config)
	      VALUES ($1,$2,'gate','approval','{"message":"DUYET","required_approvals":2,"timeout_seconds":600}')`, c.org, c.exec)
	call := func(org uuid.UUID, sql string, args ...any) (out *string) {
		t.Helper()
		err := tenancy.InTx(tenancy.WithOrg(ctx, org), worker, func(tx pgxv5.Tx) error {
			return tx.QueryRow(ctx, sql, args...).Scan(&out)
		})
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return out
	}
	const request = `SELECT plan_step_request_approval($1, $2)::text`
	const gatesOpen = `SELECT plan_node_gates_open($1, $2)::text`
	if open := call(c.org, gatesOpen, c.exec, "far"); open == nil || *open != "false" {
		t.Errorf("bước đứng sau cổng chưa duyệt được coi là đã mở: %v", open)
	}
	if open := call(a.org, gatesOpen, c.exec, "gate"); open == nil || *open != "true" {
		t.Errorf("tenant khác: %v", open) // không thấy kế hoạch nào: không có cổng nào để chặn, và cũng không có gì để chạy
	}
	if id := call(c.org, request, c.exec, "gate"); id != nil {
		t.Errorf("nút chưa running vẫn tạo được approval %s", *id)
	}
	seed(`UPDATE execution_plan_steps SET status = 'running', started_at = now() WHERE execution_id = $1 AND plan_key = 'gate'`, c.exec)
	if id := call(a.org, request, c.exec, "gate"); id != nil {
		t.Errorf("tenant khác tạo được approval cho nút của org C")
	}
	if id := call(c.org, request, c.exec, "pause"); id != nil {
		t.Errorf("nút wait tạo được approval")
	}
	first := call(c.org, request, c.exec, "gate")
	if first == nil {
		t.Fatal("nút approval đang chạy không tạo được approval")
	}
	if again := call(c.org, request, c.exec, "gate"); again == nil || *again != *first {
		t.Errorf("gọi lại không trả về approval đã có")
	}
	var required int
	var message, subject string
	var requestedBy uuid.UUID
	if err := db.Admin.QueryRow(ctx, `SELECT required_approvals, payload->>'message', subject_type, requested_by FROM approvals WHERE id = $1`, *first).
		Scan(&required, &message, &subject, &requestedBy); err != nil {
		t.Fatal(err)
	}
	if required != 2 || message != "DUYET" || subject != "workflow_step" || requestedBy != c.user {
		t.Errorf("approval của nút: required=%d message=%q subject=%s requested_by=%s", required, message, subject, requestedBy)
	}
	const status = `SELECT plan_step_approval_status($1, $2)`
	if st := call(c.org, status, c.exec, "gate"); st == nil || *st != "pending" {
		t.Errorf("trạng thái approval: %v", st)
	}
	if st := call(a.org, status, c.exec, "gate"); st != nil {
		t.Errorf("tenant khác đọc được trạng thái approval của org C: %s", *st)
	}
	err := tenancy.InTx(tenancy.WithOrg(ctx, c.org), worker, func(tx pgxv5.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE approvals SET status = 'approved' WHERE id = $1`, *first)
		return err
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Errorf("worker tự duyệt approval: err = %v, muốn 42501", err)
	}
	// Hai người duyệt là hai người khác nhau, và không ai là người yêu cầu (trigger, mọi role).
	decide := func(user uuid.UUID, to string) error {
		_, err := db.Admin.Exec(ctx, `UPDATE approvals SET status = $3, decisions = decisions || jsonb_build_array(jsonb_build_object('user_id', $2::text, 'decision', 'approve')),
			resolved_at = CASE WHEN $3 = 'pending' THEN NULL ELSE now() END WHERE id = $1`, *first, user, to)
		return err
	}
	if err := decide(c.user, "pending"); err == nil {
		t.Error("người yêu cầu tự duyệt được cổng của mình")
	}
	if err := decide(a.user, "approved"); err == nil {
		t.Error("approval cần hai người duyệt được duyệt bằng một quyết định")
	}
	if err := decide(a.user, "pending"); err != nil {
		t.Fatalf("quyết định thứ nhất: %v", err)
	}
	if err := decide(a.user, "approved"); err == nil {
		t.Error("một người duyệt quyết định hai lần")
	}
	if err := decide(b.user, "approved"); err != nil {
		t.Fatalf("quyết định thứ hai: %v", err)
	}
	const closeGate = `SELECT plan_step_close_approval($1, $2, $3)`
	if res := call(a.org, closeGate, c.exec, "gate", ""); res != nil {
		t.Errorf("tenant khác đóng được approval của org C: %s", *res)
	}
	for i := range 2 { // gọi lại trả về cùng kết quả
		if res := call(c.org, closeGate, c.exec, "gate", ""); res == nil || *res != "completed" {
			t.Errorf("đóng cổng đã duyệt (lần %d): %v", i+1, res)
		}
	}
	var final, node string
	if err := db.Admin.QueryRow(ctx, `SELECT a.status, s.status FROM approvals a JOIN execution_plan_steps s ON s.execution_id = a.execution_id AND s.plan_key = a.plan_key
		WHERE a.id = $1`, *first).Scan(&final, &node); err != nil || final != "consumed" || node != "completed" {
		t.Errorf("sau khi dùng: approval %s, nút %s, %v", final, node, err)
	}
	if open := call(c.org, gatesOpen, c.exec, "far"); open == nil || *open != "true" {
		t.Errorf("cổng đã duyệt mà bước sau vẫn bị chặn: %v", open)
	}

	// Cổng của org B: worker không tự mở được nó, và cổng còn chờ không bị đóng khi không có lý do dừng.
	seed(`INSERT INTO execution_plan_steps (org_id, execution_id, plan_key, kind, config)
	      VALUES ($1,$2,'gate','approval','{"message":"DUYET","required_approvals":1,"timeout_seconds":600}')`, b.org, b.exec)
	force := func(status string) error {
		return tenancy.InTx(tenancy.WithOrg(ctx, b.org), worker, func(tx pgxv5.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE execution_plan_steps SET status = $2 WHERE execution_id = $1 AND plan_key = 'gate'`, b.exec, status)
			return err
		})
	}
	if err := force("completed"); err == nil {
		t.Error("worker ghi được completed cho cổng chưa bắt đầu")
	}
	if err := force("running"); err != nil {
		t.Fatal(err)
	}
	if id := call(b.org, request, b.exec, "gate"); id == nil {
		t.Fatal("cổng của org B không tạo được approval")
	}
	if err := force("completed"); err == nil {
		t.Error("worker ghi được completed cho cổng chưa được duyệt")
	}
	if res := call(b.org, closeGate, b.exec, "gate", ""); res == nil || *res != "pending" {
		t.Errorf("đóng cổng còn chờ, không có lý do dừng: %v, muốn pending (không đổi gì)", res)
	}
	if res := call(b.org, closeGate, b.exec, "gate", "completed"); res == nil || *res != "pending" {
		t.Errorf("gợi ý dừng không hợp lệ làm đổi cổng: %v", res)
	}
	if open := call(b.org, gatesOpen, b.exec, "far"); open == nil || *open != "false" {
		t.Errorf("bước đứng sau cổng chưa duyệt (qua một bước continue) được coi là đã mở: %v", open)
	}
	if res := call(b.org, closeGate, b.exec, "gate", "cancelled"); res == nil || *res != "cancelled" {
		t.Errorf("đóng cổng khi kế hoạch dừng: %v", res)
	}
	if err := db.Admin.QueryRow(ctx, `SELECT status FROM approvals WHERE execution_id = $1 AND plan_key = 'gate'`, b.exec).Scan(&final); err != nil || final != "cancelled" {
		t.Errorf("approval của cổng bị huỷ: %s %v", final, err)
	}

	// Workflow đã lưu trữ không đổi nữa.
	seed(`UPDATE workflows SET status = 'archived' WHERE id = $1`, b.wf)
	if _, err := db.Admin.Exec(ctx, `UPDATE workflows SET status = 'active' WHERE id = $1`, b.wf); err == nil {
		t.Error("workflow đã lưu trữ được kích hoạt lại")
	}
}
