package tenant_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	pgxv5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/hqgroups/mcp/internal/agent/orchestrator"
	"github.com/hqgroups/mcp/internal/agent/runtime"
	"github.com/hqgroups/mcp/internal/platform/tenancy"
	"github.com/hqgroups/mcp/internal/shared/ids"
	"github.com/hqgroups/mcp/internal/testutil/pgtest"
)

// TestDelegationDecision kiểm điểm quyết định của delegation (hàm agent_task_delegate,
// migration 00023) ngay trên database, dưới role của process chạy vòng lặp: từng luật,
// tính idempotent, cách ly tenant của ảnh chụp, và quyền của từng role.
func TestDelegationDecision(t *testing.T) {
	db := pgtest.Start(t)
	ctx := context.Background()
	seed := func(sql string, args ...any) {
		t.Helper()
		if _, err := db.Admin.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, sql)
		}
	}
	type fixture struct{ org, project, lead, leadV, target, targetV, other, otherV, exec, task uuid.UUID }
	newFixture := func(slug string) fixture {
		f := fixture{ids.New(), ids.New(), ids.New(), ids.New(), ids.New(), ids.New(), ids.New(), ids.New(), ids.New(), ids.New()}
		seed(`INSERT INTO organizations (id, slug, name) VALUES ($1,$2,$2)`, f.org, slug)
		seed(`INSERT INTO projects (id, org_id, slug, name) VALUES ($1,$2,'p','P')`, f.project, f.org)
		for _, a := range [][3]any{{f.lead, f.leadV, "lead"}, {f.target, f.targetV, "target"}, {f.other, f.otherV, "other"}} {
			seed(`INSERT INTO agents (id, org_id, project_id, slug, name) VALUES ($1,$2,$3,$4,$4)`, a[0], f.org, f.project, a[2])
			seed(`INSERT INTO agent_versions (id, org_id, agent_id, version, system_prompt, config, config_hash) VALUES ($1,$2,$3,1,$4,'{}','\x00')`,
				a[1], f.org, a[0], "PROMPT-"+a[2].(string))
			seed(`UPDATE agents SET status = 'active', active_version_id = $2 WHERE id = $1`, a[0], a[1])
		}
		seed(`INSERT INTO agent_executions (id, org_id, project_id, initiator_type, mode, status, input, deadline_at)
		      VALUES ($1,$2,$3,'user','single_agent','running','{}', now() + interval '1 hour')`, f.exec, f.org, f.project)
		seed(`INSERT INTO agent_tasks (id, org_id, execution_id, agent_id, agent_version_id, goal, status, budget_limit, estimated_cost)
		      VALUES ($1,$2,$3,$4,$5,'g','running',1,0.1)`, f.task, f.org, f.exec, f.lead, f.leadV)
		seed(`INSERT INTO execution_delegation_sources (org_id, execution_id, agent_version_id, can_delegate_to, max_depth, budget_floor)
		      VALUES ($1,$2,$3,$4,2,0.05)`, f.org, f.exec, f.leadV, []uuid.UUID{f.target, f.other})
		seed(`INSERT INTO execution_delegation_targets (org_id, execution_id, agent_id, agent_version_id, slug, model_chain, budget, budget_floor, spec, timeout_seconds)
		      VALUES ($1,$2,$3,$4,'target',$5,0.5,0.02,'{}',60)`, f.org, f.exec, f.target, f.targetV, []uuid.UUID{ids.New()})
		return f
	}
	a, b := newFixture("a"), newFixture("b")
	// openStep mở step delegation của một task ở một bước: hàm chỉ giao việc cho bước đang mở.
	openStep := func(f fixture, task uuid.UUID, seq int) {
		seed(`INSERT INTO execution_steps (org_id, execution_id, task_id, step_seq, kind, status) VALUES ($1,$2,$3,$4,'delegation','started')`, f.org, f.exec, task, seq)
	}
	type result struct {
		TaskID *uuid.UUID `json:"task_id"`
		Rule   string     `json:"rule"`
	}
	worker := db.PoolAs(t, "app_worker", 2)
	delegate := func(f fixture, task uuid.UUID, seq int, target uuid.UUID) (result, error) {
		var res result
		err := tenancy.InTx(tenancy.WithOrg(ctx, f.org), worker, func(tx pgxv5.Tx) error {
			var raw []byte
			if err := tx.QueryRow(ctx, `SELECT agent_task_delegate($1, $2, $3, 'việc được giao')`, task, seq, target).Scan(&raw); err != nil {
				return err
			}
			return json.Unmarshal(raw, &res)
		})
		return res, err
	}
	mustRule := func(name string, f fixture, task uuid.UUID, seq int, target uuid.UUID, want string) result {
		t.Helper()
		res, err := delegate(f, task, seq, target)
		if err != nil || res.Rule != want || (res.TaskID != nil) != (want == "allowed" || want == "replayed") {
			t.Fatalf("%s: %+v, err = %v; muốn luật %q", name, res, err, want)
		}
		return res
	}
	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := db.Admin.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Bước chưa mở step delegation: từ chối.
	mustRule("bước chưa mở", a, a.task, 1, a.target, "task_not_active")
	openStep(a, a.task, 1)
	openStep(a, a.task, 2)
	openStep(a, a.task, 3)
	mustRule("agent ngoài can_delegate_to", a, a.task, 1, a.lead, "target_not_allowed")
	mustRule("agent không có trong ảnh chụp", a, a.task, 1, a.other, "target_unavailable")

	// Được phép: task con ở độ sâu 1, system prompt lấy từ Agent Version đích, budget là
	// phần nhỏ hơn giữa budget của đích (0.5) và phần còn lại của cha sau khi giữ lại một
	// lượt cho chính nó (1 − 0.1 − 0.05 = 0.85).
	first := mustRule("giao việc", a, a.task, 1, a.target, "allowed")
	var prompt, limit string
	var depth, step int
	if err := db.Admin.QueryRow(ctx, `SELECT system_prompt, budget_limit::text, depth, delegated_step_seq FROM agent_tasks WHERE id = $1 AND parent_task_id = $2 AND agent_version_id = $3 AND status = 'queued'`,
		first.TaskID, a.task, a.targetV).Scan(&prompt, &limit, &depth, &step); err != nil || prompt != "PROMPT-target" || limit != "0.50000000" || depth != 1 || step != 1 {
		t.Fatalf("task con: prompt %q, budget %s, depth %d, step %d (%v)", prompt, limit, depth, step, err)
	}
	// Gọi lại cùng bước: đúng task con đó, không tạo dòng thứ hai, không ghi audit thêm.
	if again := mustRule("gọi lại cùng bước", a, a.task, 1, a.target, "replayed"); *again.TaskID != *first.TaskID {
		t.Errorf("gọi lại tạo task con khác: %v vs %v", again.TaskID, first.TaskID)
	}
	// Tuần tự: task cha còn task con đang mở thì không giao thêm (phần budget còn lại không
	// bị cấp hai lần), kể cả ở một bước khác.
	mustRule("giao thêm khi con còn chạy", a, a.task, 2, a.target, "child_running")
	// Task con tiêu 0.4 rồi kết thúc: phần còn lại của cha tính từ cây task.
	seed(`UPDATE agent_tasks SET estimated_cost = 0.4 WHERE id = $1`, first.TaskID)
	seed(`UPDATE agent_tasks SET status = 'completed', finished_at = now() WHERE id = $1`, first.TaskID)
	second := mustRule("giao việc lần hai", a, a.task, 2, a.target, "allowed")
	if err := db.Admin.QueryRow(ctx, `SELECT budget_limit::text FROM agent_tasks WHERE id = $1`, second.TaskID).Scan(&limit); err != nil || limit != "0.45000000" {
		t.Errorf("budget của task con thứ hai = %s, muốn 0.45 (1 − 0.1 − 0.4 − 0.05)", limit)
	}
	var spent string
	if err := db.Admin.QueryRow(ctx, `SELECT agent_task_spent($1, $2)::text`, a.org, a.task).Scan(&spent); err != nil || spent != "0.50000000" {
		t.Errorf("mức đã tiêu của task cha gồm task con = %s, muốn 0.5", spent)
	}
	// Task con không giao việc được tiếp: Agent Version của nó không có cấu hình delegation.
	seed(`UPDATE agent_tasks SET status = 'running' WHERE id = $1`, second.TaskID)
	openStep(a, *second.TaskID, 1)
	mustRule("task con giao việc tiếp", a, *second.TaskID, 1, a.other, "delegation_not_configured")
	seed(`UPDATE agent_tasks SET status = 'completed', finished_at = now() WHERE id = $1`, second.TaskID)

	// Không đủ budget: cha chỉ còn 1 − 0.1 − 0.4 − (chi phí con thứ hai 0) − 0.05; tiêu thêm cho tới khi không đủ sàn của đích.
	seed(`UPDATE agent_tasks SET estimated_cost = 0.54 WHERE id = $1`, a.task)
	mustRule("không đủ budget", a, a.task, 3, a.target, "budget_insufficient")
	// Agent đích bị tắt, hoặc version đang hoạt động của nó không còn là version đã chụp
	// (rollback): giảm quyền có hiệu lực với task con chưa tạo.
	seed(`UPDATE agents SET status = 'disabled' WHERE id = $1`, a.target)
	mustRule("agent đích đã bị tắt", a, a.task, 3, a.target, "target_unavailable")
	seed(`UPDATE agents SET status = 'active' WHERE id = $1`, a.target)

	// Trần số task của execution: mức sàn của platform, bằng orchestrator.MaxTasks.
	fresh := newFixture("c")
	openStep(fresh, fresh.task, 1)
	for i := count(`SELECT count(*) FROM agent_tasks WHERE execution_id = $1`, fresh.exec); i < orchestrator.MaxTasks-1; i++ {
		seed(`INSERT INTO agent_tasks (org_id, execution_id, agent_id, agent_version_id, goal, status) VALUES ($1,$2,$3,$4,'g','completed')`,
			fresh.org, fresh.exec, fresh.lead, fresh.leadV)
	}
	mustRule("task thứ MaxTasks", fresh, fresh.task, 1, fresh.target, "allowed")
	seed(`UPDATE agent_tasks SET status = 'completed', finished_at = now() WHERE parent_task_id = $1`, fresh.task)
	openStep(fresh, fresh.task, 2)
	mustRule("vượt MaxTasks", fresh, fresh.task, 2, fresh.target, "task_limit")
	if n := count(`SELECT count(*) FROM agent_tasks WHERE execution_id = $1`, fresh.exec); n != orchestrator.MaxTasks {
		t.Errorf("execution có %d task, trần là %d", n, orchestrator.MaxTasks)
	}

	// Mọi quyết định do hàm đưa ra đều có audit, do chính hàm ghi, kèm luật đã khớp.
	if n := count(`SELECT count(*) FROM audit_logs WHERE org_id = $1 AND action = 'delegation.create' AND outcome = 'success' AND execution_id = $2`, a.org, a.exec); n != 2 {
		t.Errorf("audit delegation.create = %d, muốn 2 (lần gọi lại không ghi thêm)", n)
	}
	for _, rule := range []string{"task_not_active", "target_not_allowed", "target_unavailable", "child_running", "budget_insufficient", "delegation_not_configured"} {
		if n := count(`SELECT count(*) FROM audit_logs WHERE org_id = $1 AND action = 'delegation.deny' AND outcome = 'denied' AND details->>'rule' = $2`, a.org, rule); n == 0 {
			t.Errorf("thiếu audit delegation.deny cho luật %s", rule)
		}
	}

	// Cách ly tenant: task của org khác không tồn tại với hàm; ảnh chụp của org khác không đọc được.
	var pgErr *pgconn.PgError
	if _, err := delegate(a, b.task, 1, b.target); err == nil || !errors.As(err, &pgErr) || pgErr.Code != "P0002" {
		t.Errorf("giao việc từ task của org B dưới tenant A: err = %v, muốn no_data_found", err)
	}
	q := func(id uuid.UUID) string { return "'" + id.String() + "'" }
	deniedOrEmpty := func(pool string, org uuid.UUID, stmts map[string]string, wantGrantError map[string]bool) {
		t.Helper()
		p := db.PoolAs(t, pool, 1)
		err := tenancy.InTx(tenancy.WithOrg(ctx, org), p, func(tx pgxv5.Tx) error {
			for name, stmt := range stmts {
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
					t.Errorf("%s (%s): tác động / thấy %d dòng, muốn bị từ chối", name, pool, affected)
				case err == nil && wantGrantError[name]:
					t.Errorf("%s (%s): chạy được (0 dòng), muốn thiếu quyền", name, pool)
				case err != nil && wantGrantError[name] && (!errors.As(err, &pgErr) || pgErr.Code != "42501"):
					t.Errorf("%s (%s): err = %v, muốn 42501", name, pool, err)
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
	deniedOrEmpty("app_api", a.org, map[string]string{
		"đọc nguồn của org B":       `SELECT agent_version_id FROM execution_delegation_sources WHERE execution_id = ` + q(b.exec),
		"đọc đích của org B":        `SELECT agent_id FROM execution_delegation_targets WHERE execution_id = ` + q(b.exec),
		"nguồn khai org B":          `INSERT INTO execution_delegation_sources (org_id, execution_id, agent_version_id, can_delegate_to, max_depth, budget_floor) VALUES (` + q(b.org) + `,` + q(b.exec) + `,` + q(b.otherV) + `,ARRAY[` + q(b.target) + `]::uuid[],1,0) RETURNING execution_id`,
		"nguồn cho execution của B": `INSERT INTO execution_delegation_sources (org_id, execution_id, agent_version_id, can_delegate_to, max_depth, budget_floor) VALUES (` + q(a.org) + `,` + q(b.exec) + `,` + q(a.otherV) + `,ARRAY[` + q(a.target) + `]::uuid[],1,0) RETURNING execution_id`,
		"đích là agent của org B":   `INSERT INTO execution_delegation_targets (org_id, execution_id, agent_id, agent_version_id, slug, model_chain, budget_floor, spec, timeout_seconds) VALUES (` + q(a.org) + `,` + q(a.exec) + `,` + q(b.other) + `,` + q(b.otherV) + `,'x',ARRAY[gen_random_uuid()],0,'{}',1) RETURNING execution_id`,
		"version không thuộc agent": `INSERT INTO execution_delegation_targets (org_id, execution_id, agent_id, agent_version_id, slug, model_chain, budget_floor, spec, timeout_seconds) VALUES (` + q(a.org) + `,` + q(a.exec) + `,` + q(a.other) + `,` + q(a.leadV) + `,'x',ARRAY[gen_random_uuid()],0,'{}',1) RETURNING execution_id`,
		"max_depth vượt trần":       `INSERT INTO execution_delegation_sources (org_id, execution_id, agent_version_id, can_delegate_to, max_depth, budget_floor) VALUES (` + q(a.org) + `,` + q(a.exec) + `,` + q(a.otherV) + `,ARRAY[` + q(a.target) + `]::uuid[],6,0) RETURNING execution_id`,
		"can_delegate_to rỗng":      `INSERT INTO execution_delegation_sources (org_id, execution_id, agent_version_id, can_delegate_to, max_depth, budget_floor) VALUES (` + q(a.org) + `,` + q(a.exec) + `,` + q(a.otherV) + `,'{}',1,0) RETURNING execution_id`,
		"sửa ảnh chụp":              `UPDATE execution_delegation_sources SET max_depth = 5 WHERE execution_id = ` + q(a.exec) + ` RETURNING execution_id`,
		"xoá ảnh chụp":              `DELETE FROM execution_delegation_targets WHERE execution_id = ` + q(a.exec) + ` RETURNING execution_id`,
	}, map[string]bool{"sửa ảnh chụp": true, "xoá ảnh chụp": true})
	deniedOrEmpty("app_worker", a.org, map[string]string{
		"đọc đích của org B":    `SELECT agent_id FROM execution_delegation_targets WHERE execution_id = ` + q(b.exec),
		"ghi ảnh chụp":          `INSERT INTO execution_delegation_sources (org_id, execution_id, agent_version_id, can_delegate_to, max_depth, budget_floor) VALUES (` + q(a.org) + `,` + q(a.exec) + `,` + q(a.otherV) + `,ARRAY[` + q(a.target) + `]::uuid[],1,0) RETURNING execution_id`,
		"sửa ảnh chụp":          `UPDATE execution_delegation_targets SET budget = 999 WHERE execution_id = ` + q(a.exec) + ` RETURNING execution_id`,
		"tự tạo task con":       `INSERT INTO agent_tasks (org_id, execution_id, parent_task_id, delegated_step_seq, agent_id, agent_version_id, goal) VALUES (` + q(a.org) + `,` + q(a.exec) + `,` + q(a.task) + `,9,` + q(a.target) + `,` + q(a.targetV) + `,'g') RETURNING id`,
		"đổi bước của task con": `UPDATE agent_tasks SET delegated_step_seq = 7 WHERE id = ` + q(*first.TaskID) + ` RETURNING id`,
	}, map[string]bool{"ghi ảnh chụp": true, "sửa ảnh chụp": true, "tự tạo task con": true, "đổi bước của task con": true})
	// Gateway không gọi được điểm quyết định và không đọc được ảnh chụp; nó đọc được mức đã tiêu.
	deniedOrEmpty("app_gateway", a.org, map[string]string{
		"gọi hàm quyết định": `SELECT agent_task_delegate(` + q(a.task) + `, 3, ` + q(a.target) + `, 'g')`,
		"đọc ảnh chụp":       `SELECT agent_id FROM execution_delegation_targets`,
	}, map[string]bool{"gọi hàm quyết định": true, "đọc ảnh chụp": true})
	gateway := db.PoolAs(t, "app_gateway", 1)
	if err := tenancy.InTx(tenancy.WithOrg(ctx, a.org), gateway, func(tx pgxv5.Tx) error {
		return tx.QueryRow(ctx, `SELECT agent_task_spent($1, $2)::text`, a.org, a.task).Scan(&spent)
	}); err != nil {
		t.Errorf("gateway không đọc được mức đã tiêu của task: %v", err)
	}

	// Với MỌI role: ảnh chụp bất biến; bước của task con bất biến; mỗi bước một task con;
	// task con luôn có bước, task gốc thì không.
	for name, stmt := range map[string]string{
		"sửa nguồn":                  `UPDATE execution_delegation_sources SET max_depth = 5 WHERE execution_id = $1`,
		"sửa đích":                   `UPDATE execution_delegation_targets SET budget = 999 WHERE execution_id = $1`,
		"đổi bước của task con":      `UPDATE agent_tasks SET delegated_step_seq = 9 WHERE execution_id = $1 AND parent_task_id IS NOT NULL`,
		"task con trùng bước":        `INSERT INTO agent_tasks (org_id, execution_id, parent_task_id, delegated_step_seq, depth, agent_id, agent_version_id, goal) SELECT org_id, execution_id, parent_task_id, delegated_step_seq, depth, agent_id, agent_version_id, 'g' FROM agent_tasks WHERE execution_id = $1 AND parent_task_id IS NOT NULL LIMIT 1`,
		"task con không có bước":     `INSERT INTO agent_tasks (org_id, execution_id, parent_task_id, depth, agent_id, agent_version_id, goal) SELECT org_id, execution_id, parent_task_id, depth, agent_id, agent_version_id, 'g' FROM agent_tasks WHERE execution_id = $1 AND parent_task_id IS NOT NULL LIMIT 1`,
		"task gốc có bước giao việc": `INSERT INTO agent_tasks (org_id, execution_id, delegated_step_seq, agent_id, agent_version_id, goal) SELECT org_id, execution_id, 5, agent_id, agent_version_id, 'g' FROM agent_tasks WHERE execution_id = $1 AND parent_task_id IS NULL LIMIT 1`,
	} {
		if _, err := db.Admin.Exec(ctx, stmt, a.exec); err == nil {
			t.Errorf("%s: chạy được với superuser, muốn bị database từ chối", name)
		}
	}
}

// TestActiveTaskDuringPause: execution sang paused (hoặc waiting_approval) ngay khi MỘT
// task của nó dừng; task khác đang giữa bước vẫn phải gọi được Gateway để chạy xong bước
// đó. Task đang tạm dừng thì không.
func TestActiveTaskDuringPause(t *testing.T) {
	db := pgtest.Start(t)
	ctx := context.Background()
	seed := func(sql string, args ...any) {
		t.Helper()
		if _, err := db.Admin.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, sql)
		}
	}
	org, project, agent, version, exec, running, held := ids.New(), ids.New(), ids.New(), ids.New(), ids.New(), ids.New(), ids.New()
	seed(`INSERT INTO organizations (id, slug, name) VALUES ($1,'a','A')`, org)
	seed(`INSERT INTO projects (id, org_id, slug, name) VALUES ($1,$2,'p','P')`, project, org)
	seed(`INSERT INTO agents (id, org_id, project_id, slug, name) VALUES ($1,$2,$3,'a','A')`, agent, org, project)
	seed(`INSERT INTO agent_versions (id, org_id, agent_id, version, system_prompt, config, config_hash) VALUES ($1,$2,$3,1,'p','{}','\x00')`, version, org, agent)
	seed(`INSERT INTO agent_executions (id, org_id, project_id, initiator_type, mode, status, input, deadline_at, plan, plan_source)
	      VALUES ($1,$2,$3,'user','planned','running','{}', now() + interval '1 hour', '{"tasks":[]}', 'caller')`, exec, org, project)
	for _, id := range []uuid.UUID{running, held} {
		seed(`INSERT INTO agent_tasks (id, org_id, execution_id, agent_id, agent_version_id, goal, status) VALUES ($1,$2,$3,$4,$5,'g','running')`, id, org, exec, agent, version)
	}
	reader := runtime.NewTaskReader(db.PoolAs(t, "app_gateway", 1))
	active := func(task uuid.UUID) bool {
		_, err := reader.ActiveTask(tenancy.WithOrg(ctx, org), task)
		return err == nil
	}
	for _, status := range []string{"paused", "waiting_approval"} {
		seed(`UPDATE agent_tasks SET status = $2 WHERE id = $1`, held, status)
		seed(`UPDATE agent_executions SET status = $2 WHERE id = $1`, exec, status)
		if !active(running) {
			t.Errorf("execution %s: task đang chạy bị Gateway từ chối giữa bước", status)
		}
		if active(held) {
			t.Errorf("execution %s: task đang %s vẫn gọi được Gateway", status, status)
		}
		seed(`UPDATE agent_tasks SET status = 'running' WHERE id = $1`, held)
	}
	// Nguồn gốc của kế hoạch (người gọi hay model) quyết định mức sàn approval: nó bất biến
	// với mọi role, và đi cùng kế hoạch.
	var pgErr *pgconn.PgError
	if _, err := db.Admin.Exec(ctx, `UPDATE agent_executions SET plan_source = 'model' WHERE id = $1`, exec); err == nil || !errors.As(err, &pgErr) || pgErr.Code != "23001" {
		t.Errorf("đổi plan_source: err = %v, muốn bị trigger từ chối", err)
	}
	if _, err := db.Admin.Exec(ctx, `INSERT INTO agent_executions (org_id, project_id, initiator_type, mode, input, deadline_at, plan)
		VALUES ($1,$2,'user','planned','{}', now() + interval '1 hour', '{"tasks":[]}')`, org, project); err == nil {
		t.Error("execution planned không có plan_source được chèn")
	}
	// Dấu của lần lập kế hoạch: process chạy vòng lặp và Gateway không đọc được.
	for _, role := range []string{"app_worker", "app_gateway"} {
		err := tenancy.InTx(tenancy.WithOrg(ctx, org), db.PoolAs(t, role, 1), func(tx pgxv5.Tx) error {
			_, err := tx.Exec(ctx, `SELECT execution_id FROM plan_proposals`)
			return err
		})
		if err == nil || !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Errorf("%s đọc plan_proposals: err = %v, muốn thiếu quyền", role, err)
		}
	}
	// Yêu cầu huỷ thì Gateway tự thực thi, với mọi task.
	seed(`UPDATE agent_executions SET cancel_requested_at = now() WHERE id = $1`, exec)
	if active(running) {
		t.Error("task của execution đã có yêu cầu huỷ vẫn gọi được Gateway")
	}
}
