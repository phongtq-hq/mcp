package api_test

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestUsageReports: tổng hợp usage theo chiều và chuỗi thời gian (cost-management §3).
// Số liệu là tổng của usage ledger trong tenant: org khác không góp vào, lượt chưa có giá
// được đếm riêng, tiền là chuỗi thập phân.
func TestUsageReports(t *testing.T) {
	e := setup(t, false)
	ctx := context.Background()
	adminA, adminB, viewerA, devA := e.tok("admin-a"), e.tok("admin-b"), e.tok("viewer-a"), e.tok("dev-a")
	p1 := e.createProject(adminA, "usage-one", "Usage One")["id"].(string)
	p2 := e.createProject(adminA, "usage-two", "Usage Two")["id"].(string)
	pb := e.createProject(adminB, "usage-b", "Usage B")["id"].(string)
	agentX, agentY, model := uuid.NewString(), uuid.NewString(), uuid.NewString()
	exec1, exec2, exec3 := uuid.NewString(), uuid.NewString(), uuid.NewString()
	at := time.Now().Add(-5 * time.Minute).UTC()
	seed := func(org uuid.UUID, project, agent, exec string, in, out int, cost any) {
		t.Helper()
		if _, err := e.db.Admin.Exec(ctx, `INSERT INTO usage_records (org_id, project_id, agent_id, execution_id, model_id, kind, purpose, input_tokens, output_tokens, estimated_cost, occurred_at)
			VALUES ($1,$2,$3,$4,$5,'llm','agent_turn',$6,$7,$8,$9)`, org, project, agent, exec, model, in, out, cost, at); err != nil {
			t.Fatal(err)
		}
	}
	seed(e.orgA, p1, agentX, exec1, 1000, 100, "0.50000000")
	seed(e.orgA, p1, agentX, exec1, 2000, 200, "0.25000000")
	seed(e.orgA, p1, agentY, exec2, 4000, 400, "1.00000000")
	seed(e.orgA, p2, agentY, exec3, 8000, 800, nil) // chưa có giá
	seed(e.orgB, pb, agentX, uuid.NewString(), 9, 9, "9.00000000")

	rng := "from=" + url.QueryEscape(time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)) + "&to=" + url.QueryEscape(time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	amount := func(v any) string { return v.(map[string]any)["amount"].(string) }

	t.Run("summary: totals and groups by dimension, within the tenant", func(t *testing.T) {
		r := e.do("GET", "/v1/usage/summary?"+rng, adminA, nil, nil)
		want(t, r, 200, "")
		tot := r.json(t)["totals"].(map[string]any)
		if tot["calls"] != float64(4) || tot["executions"] != float64(3) || tot["input_tokens"] != float64(15000) || tot["unpriced_calls"] != float64(1) ||
			amount(tot["estimated_cost"]) != "1.75000000" || len(r.json(t)["groups"].([]any)) != 0 || r.json(t)["cost_basis"] != "estimated" {
			t.Errorf("tổng: %s", r.body)
		}
		// Theo agent: xếp theo chi phí giảm dần.
		groups := e.do("GET", "/v1/usage/summary?"+rng+"&group_by=agent", adminA, nil, nil).json(t)["groups"].([]any)
		if len(groups) != 2 {
			t.Fatalf("nhóm theo agent: %v", groups)
		}
		first, second := groups[0].(map[string]any), groups[1].(map[string]any)
		if first["key"].(map[string]any)["agent"] != agentY || amount(first["estimated_cost"]) != "1.00000000" || first["unpriced_calls"] != float64(1) ||
			second["key"].(map[string]any)["agent"] != agentX || amount(second["estimated_cost"]) != "0.75000000" ||
			amount(second["avg_cost_per_execution"]) != "0.75000000" || second["executions"] != float64(1) {
			t.Errorf("nhóm theo agent: %v", groups)
		}
		// Hai chiều, và bộ lọc.
		two := e.do("GET", "/v1/usage/summary?"+rng+"&group_by=project,agent", adminA, nil, nil).json(t)["groups"].([]any)
		if len(two) != 3 {
			t.Errorf("nhóm theo project, agent: %d nhóm", len(two))
		}
		only := e.do("GET", "/v1/usage/summary?"+rng+"&project_id="+p1+"&group_by=purpose", adminA, nil, nil).json(t)
		if amount(only["totals"].(map[string]any)["estimated_cost"]) != "1.75000000" || only["totals"].(map[string]any)["calls"] != float64(3) ||
			only["groups"].([]any)[0].(map[string]any)["key"].(map[string]any)["purpose"] != "agent_turn" {
			t.Errorf("lọc theo project: %v", only)
		}
		// Org B chỉ thấy usage của mình; project của org A không lọc được từ org B.
		b := e.do("GET", "/v1/usage/summary?"+rng+"&group_by=project", adminB, nil, nil).json(t)
		if amount(b["totals"].(map[string]any)["estimated_cost"]) != "9.00000000" || len(b["groups"].([]any)) != 1 {
			t.Errorf("org B: %v", b)
		}
		// Project của org khác, hay project không tồn tại, là 404, và không sinh dòng audit từ
		// chối nào (project_id bịa ra không đi vào quyết định phân quyền).
		before := 0
		if err := e.db.Admin.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action = 'authz.deny'`).Scan(&before); err != nil {
			t.Fatal(err)
		}
		want(t, e.do("GET", "/v1/usage/summary?"+rng+"&project_id="+p1, adminB, nil, nil), 404, "not_found")
		want(t, e.do("GET", "/v1/usage/timeseries?"+rng+"&project_id="+p1, adminB, nil, nil), 404, "not_found")
		want(t, e.do("GET", "/v1/usage/summary?"+rng+"&project_id="+uuid.NewString(), viewerA, nil, nil), 404, "not_found")
		after := 0
		if err := e.db.Admin.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action = 'authz.deny'`).Scan(&after); err != nil {
			t.Fatal(err)
		}
		if after != before {
			t.Errorf("project_id không tồn tại sinh %d dòng audit từ chối", after-before)
		}
		// Bộ lọc của org khác cho kết quả rỗng, như một định danh không tồn tại.
		if x := e.do("GET", "/v1/usage/summary?"+rng+"&agent_id="+agentX, adminB, nil, nil).json(t)["totals"].(map[string]any); x["calls"] != float64(1) {
			t.Errorf("org B lọc theo agent: %v", x) // agentX cũng là agent của dòng usage org B trong test này
		}
		want(t, e.do("GET", "/v1/usage/summary?"+rng, viewerA, nil, nil), 403, "permission_denied")
		want(t, e.do("GET", "/v1/usage/summary?"+rng, devA, nil, nil), 200, "")
		for _, bad := range []string{"", rng + "&group_by=secret", rng + "&group_by=agent,agent", rng + "&group_by=agent,model,user", rng + "&agent_id=x",
			"from=2020-01-01T00:00:00Z&to=2026-01-01T00:00:00Z"} {
			want(t, e.do("GET", "/v1/usage/summary?"+bad, adminA, nil, nil), 400, "invalid_parameter")
		}
	})

	t.Run("timeseries: one series per key, forecast by linear run rate", func(t *testing.T) {
		r := e.do("GET", "/v1/usage/timeseries?"+rng, adminA, nil, nil)
		want(t, r, 200, "")
		series := r.json(t)["series"].([]any)
		if len(series) != 1 || r.json(t)["interval"] != "day" || r.json(t)["forecast"] != nil {
			t.Fatalf("chuỗi: %s", r.body)
		}
		var in float64
		for _, raw := range series[0].(map[string]any)["points"].([]any) {
			in += raw.(map[string]any)["input_tokens"].(float64)
		}
		if in != 15000 || series[0].(map[string]any)["key"] != nil {
			t.Errorf("chuỗi tổng: %v", series)
		}
		byAgent := e.do("GET", "/v1/usage/timeseries?"+rng+"&group_by=agent&interval=month", adminA, nil, nil).json(t)["series"].([]any)
		if len(byAgent) != 2 {
			t.Errorf("chuỗi theo agent: %v", byAgent)
		}
		fc := e.do("GET", "/v1/usage/timeseries?"+rng+"&include_forecast=true", adminA, nil, nil)
		want(t, fc, 200, "")
		// Ngày đầu tháng chưa có ngày trọn nào: không có căn cứ, forecast là null. Các ngày
		// khác: mọi ngày trọn đều không có usage, nên dự báo đúng bằng chi phí từ đầu tháng.
		if f, ok := fc.json(t)["forecast"].(map[string]any); time.Now().UTC().Day() > 1 {
			if !ok || f["method"] != "linear_run_rate" || amount(f["projected_cost"]) != "1.75000000" || amount(f["low"]) != "1.75000000" || amount(f["high"]) != "1.75000000" {
				t.Errorf("dự báo: %s", fc.body)
			}
		} else if ok {
			t.Errorf("ngày đầu tháng vẫn có dự báo: %v", f)
		}
		if b := e.do("GET", "/v1/usage/timeseries?"+rng, adminB, nil, nil).json(t)["series"].([]any); len(b) != 1 ||
			amount(b[0].(map[string]any)["points"].([]any)[0].(map[string]any)["estimated_cost"]) != "9.00000000" {
			t.Errorf("org B: %v", b)
		}
		want(t, e.do("GET", "/v1/usage/timeseries?"+rng, viewerA, nil, nil), 403, "permission_denied")
		for _, bad := range []string{rng + "&interval=hour", rng + "&group_by=user", rng + "&include_forecast=yes",
			"from=2020-01-01T00:00:00Z&to=2026-01-01T00:00:00Z"} {
			want(t, e.do("GET", "/v1/usage/timeseries?"+bad, adminA, nil, nil), 400, "invalid_parameter")
		}
	})
}
