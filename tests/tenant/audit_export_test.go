package tenant_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hqgroups/mcp/internal/adminops"
	"github.com/hqgroups/mcp/internal/shared/ids"
	"github.com/hqgroups/mcp/internal/testutil/pgtest"
)

// TestAuditVerifyAndExport: kiểm chuỗi hash của audit và file xuất (roadmap Phase 6).
// Chuỗi nguyên vẹn được xác nhận; một bản ghi bị sửa hay bị xoá giữa chuỗi bị phát hiện,
// trong database lẫn trong file đã xuất; file chỉ chứa audit của đúng org được xuất.
func TestAuditVerifyAndExport(t *testing.T) {
	db := pgtest.Start(t)
	ctx := context.Background()
	orgA, orgB := ids.New(), ids.New()
	if _, err := db.Admin.Exec(ctx, `INSERT INTO organizations (id, slug, name) VALUES ($1,'audit-a','A'), ($2,'audit-b','B')`, orgA, orgB); err != nil {
		t.Fatal(err)
	}
	record := func(org any, action string) {
		t.Helper()
		tx, err := db.Admin.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `SELECT set_config('app.org_id', $1, true)`, org); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `SELECT audit_append($1, NULL, 'system', NULL, NULL, $2, 'thing', NULL, 'success', NULL, '{"k":"v"}', NULL, NULL)`, org, action); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for _, a := range []string{"one", "two", "three", "four"} {
		record(orgA.String(), "a."+a)
	}
	record(orgB.String(), "b.only")

	conn, err := db.Admin.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	chains, err := adminops.VerifyAudit(ctx, conn.Conn(), "", false)
	if err != nil || len(chains) != 2 || chains[0].Records != 4 || chains[1].Records != 1 || chains[0].Anchored {
		t.Fatalf("chuỗi nguyên vẹn: %+v, %v", chains, err)
	}
	// Tài khoản không phải superuser, chỉ là thành viên của app_admin (cấu hình thật của
	// hqadmin): vẫn thấy và kiểm đủ mọi org. Một lần kiểm không kiểm gì không được báo sạch.
	adminPool := db.PoolAs(t, "app_admin", 1)
	member, err := adminPool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	chains, err = adminops.VerifyAudit(ctx, member.Conn(), "", false)
	member.Release()
	if err != nil || len(chains) != 2 || chains[0].Records != 4 {
		t.Fatalf("kiểm bằng thành viên của app_admin: %+v, %v", chains, err)
	}

	time.Sleep(50 * time.Millisecond)
	from, to := time.Now().Add(-time.Hour), time.Now()
	var file bytes.Buffer
	// Chỉ xuất khoảng đã đóng: khoảng còn nhận bản ghi mới cho ra file thiếu mà trông đủ.
	if _, err := adminops.ExportAudit(ctx, conn.Conn(), "audit-a", from, time.Now().Add(time.Hour), &file); err == nil || file.Len() != 0 {
		t.Errorf("xuất tới một thời điểm ở tương lai: err = %v, %d byte", err, file.Len())
	}
	m, err := adminops.ExportAudit(ctx, conn.Conn(), "audit-a", from, to, &file)
	if err != nil || m.Records != 4 || m.OrgID != orgA {
		t.Fatalf("xuất: %+v, %v", m, err)
	}
	if strings.Contains(file.String(), "b.only") || strings.Contains(file.String(), orgB.String()) {
		t.Error("file xuất của org A chứa audit của org B")
	}
	got, err := adminops.VerifyAuditFile(bytes.NewReader(file.Bytes()))
	if err != nil || got.Records != 4 || got.LastHash != m.LastHash {
		t.Fatalf("kiểm file: %+v, %v", got, err)
	}
	// File bị sửa nội dung, bị bỏ một dòng, hoặc bị cắt mất manifest đều bị phát hiện.
	lines := strings.Split(strings.TrimSpace(file.String()), "\n")
	var occurred string
	if err := db.Admin.QueryRow(ctx, `SELECT to_char(occurred_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"') FROM audit_logs WHERE org_id = $1 AND action = 'a.two'`, orgA).Scan(&occurred); err != nil {
		t.Fatal(err)
	}
	year := occurred[:4]
	for name, bad := range map[string]string{
		"sửa action": strings.Replace(file.String(), `"action":"a.two"`, `"action":"a.TWO"`, 1),
		// Trường hiển thị bị đổi mà hash và canonical giữ nguyên: mọi trường đều được đối chiếu.
		"sửa resource_type": strings.Replace(file.String(), `"resource_type":"thing"`, `"resource_type":"other"`, 1),
		"sửa details":       strings.Replace(file.String(), `"details":{"k":"v"}`, `"details":{"k":"w"}`, 1),
		"thêm actor_id":     strings.Replace(file.String(), `"actor_type":"system",`, `"actor_type":"system","actor_id":"`+orgB.String()+`",`, 1),
		"sửa thời điểm":     strings.Replace(file.String(), `"occurred_at":"`+year, `"occurred_at":"1999`, 1),
		"khoá trùng":        strings.Replace(file.String(), `"action":"a.two"`, `"action":"EVIL","action":"a.two"`, 1),
		"trường lạ":         strings.Replace(file.String(), `{"record":{`, `{"record":{"note":"x",`, 1),
		"manifest sai org":  strings.Replace(file.String(), `"org_id":"`+orgA.String()+`"`, `"org_id":"`+orgB.String()+`"`, 1),
		"bỏ một dòng":       strings.Join(append(append([]string{}, lines[:1]...), lines[2:]...), "\n") + "\n",
		"mất manifest":      strings.Join(lines[:len(lines)-1], "\n") + "\n",
		"thêm sau cuối":     file.String() + lines[0] + "\n",
	} {
		if _, err := adminops.VerifyAuditFile(strings.NewReader(bad)); !errors.Is(err, adminops.ErrAuditChainBroken) {
			t.Errorf("%s: err = %v, muốn chuỗi đứt", name, err)
		}
	}

	// Hai lần xuất liên tiếp nối nhau; một file không nối được với file trước bị phát hiện.
	record(orgA.String(), "a.five")
	time.Sleep(50 * time.Millisecond)
	var next bytes.Buffer
	if _, err := adminops.ExportAudit(ctx, conn.Conn(), "audit-a", to, time.Now(), &next); err != nil {
		t.Fatal(err)
	}
	if m, err := adminops.VerifyAuditFiles(bytes.NewReader(file.Bytes()), bytes.NewReader(next.Bytes())); err != nil || m.Records != 1 {
		t.Errorf("hai file liên tiếp: %+v, %v", m, err)
	}
	if _, err := adminops.VerifyAuditFiles(bytes.NewReader(next.Bytes()), bytes.NewReader(file.Bytes())); !errors.Is(err, adminops.ErrAuditChainBroken) {
		t.Errorf("hai file sai thứ tự: err = %v", err)
	}

	// Phần đầu của chuỗi bị xoá theo retention: chuỗi còn lại vẫn kiểm được, từ mỏ neo.
	// (audit_logs chỉ thêm với mọi role ứng dụng; test xoá bằng superuser và tắt trigger.)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := db.Admin.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	exec(`SET session_replication_role = replica`)
	defer exec(`SET session_replication_role = DEFAULT`)
	if _, err := conn.Exec(ctx, `SET session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `DELETE FROM audit_logs WHERE org_id = $1 AND action = 'a.one'`, orgA); err != nil {
		t.Fatal(err)
	}
	// Mất phần đầu là chuỗi đứt, trừ khi người vận hành nói rõ đó là do retention.
	if _, err := adminops.VerifyAudit(ctx, conn.Conn(), "audit-a", false); !errors.Is(err, adminops.ErrAuditChainBroken) {
		t.Errorf("chuỗi mất phần đầu: err = %v, muốn chuỗi đứt", err)
	}
	chains, err = adminops.VerifyAudit(ctx, conn.Conn(), "audit-a", true)
	if err != nil || len(chains) != 1 || chains[0].Records != 4 || !chains[0].Anchored {
		t.Fatalf("chuỗi sau retention: %+v, %v", chains, err)
	}
	// Một bản ghi giữa chuỗi bị xoá, hoặc bị sửa: chuỗi đứt.
	if _, err := conn.Exec(ctx, `UPDATE audit_logs SET action = 'a.forged' WHERE org_id = $1 AND action = 'a.three'`, orgA); err != nil {
		t.Fatal(err)
	}
	if _, err := adminops.VerifyAudit(ctx, conn.Conn(), "audit-a", true); !errors.Is(err, adminops.ErrAuditChainBroken) {
		t.Errorf("bản ghi bị sửa: err = %v, muốn chuỗi đứt", err)
	}
	var out bytes.Buffer
	if _, err := adminops.ExportAudit(ctx, conn.Conn(), "audit-a", from, to, &out); !errors.Is(err, adminops.ErrAuditChainBroken) {
		t.Errorf("xuất chuỗi đã đứt: err = %v", err)
	}
	// Org B không bị ảnh hưởng.
	if chains, err := adminops.VerifyAudit(ctx, conn.Conn(), "audit-b", false); err != nil || chains[0].Records != 1 {
		t.Errorf("org B: %+v, %v", chains, err)
	}
	_, _ = conn.Exec(ctx, `SET session_replication_role = DEFAULT`)
}
