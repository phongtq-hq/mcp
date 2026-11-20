package api_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/hqgroups/mcp/internal/shared/ids"
)

func TestModelCatalogAPI(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	adminA, adminB, viewerA := e.tok("admin-a"), e.tok("admin-b"), e.tok("viewer-a")

	post := func(tok, path string, body any) map[string]any {
		t.Helper()
		r := e.do("POST", path, tok, body, nil)
		if r.status != 201 {
			t.Fatalf("POST %s: %d %s", path, r.status, r.body)
		}
		return r.json(t)
	}
	provA := post(adminA, "/v1/model-providers", map[string]any{"slug": "claude-local", "adapter": "claude-cli"})
	provID := provA["id"].(string)
	modelBody := func(pm, tier string) map[string]any {
		return map[string]any{"provider_id": provID, "provider_model": pm, "display_name": "Claude " + pm, "tier": tier,
			"capabilities": map[string]any{"tool_calling": true, "context_window": 1000000}}
	}
	sonnet := post(adminA, "/v1/models", modelBody("sonnet", "standard"))["id"].(string)
	haiku := post(adminA, "/v1/models", modelBody("haiku", "economy"))["id"].(string)

	t.Run("providers", func(t *testing.T) {
		if provA["scope"] != "organization" || provA["has_credential"] != false || provA["enabled"] != true || provA["adapter"] != "claude-cli" {
			t.Errorf("provider mới: %v", provA)
		}
		want(t, e.do("POST", "/v1/model-providers", adminA, map[string]any{"slug": "claude-local", "adapter": "claude-cli"}, nil), 409, "slug_taken")
		// Chỉ nhận adapter mà process này thực sự có.
		want(t, e.do("POST", "/v1/model-providers", adminA, map[string]any{"slug": "p-openai", "adapter": "openai"}, nil), 422, "validation_failed")
		want(t, e.do("POST", "/v1/model-providers", adminA, map[string]any{"slug": "Bad Slug", "adapter": "claude-cli"}, nil), 422, "validation_failed")
		// API key và base_url bị từ chối cho tới khi có Secrets và SSRF guard; khoá không bị lưu hay vọng lại.
		r := e.do("POST", "/v1/model-providers", adminA, map[string]any{"slug": "p-key", "adapter": "claude-cli", "api_key": "sk-ant-SECRETVALUE"}, nil)
		want(t, r, 422, "validation_failed")
		if strings.Contains(string(r.body), "SECRETVALUE") {
			t.Error("phản hồi vọng lại api_key")
		}
		want(t, e.do("POST", "/v1/model-providers", adminA, map[string]any{"slug": "p-url", "adapter": "claude-cli", "base_url": "http://169.254.169.254/"}, nil), 422, "validation_failed")
		want(t, e.do("PATCH", "/v1/model-providers/"+provID, adminA, map[string]any{"api_key": "sk-ant-SECRETVALUE"}, nil), 422, "validation_failed")
		var leaked int
		_ = e.db.Admin.QueryRow(ctx, `SELECT (SELECT count(*) FROM model_providers WHERE slug IN ('p-key','p-url','p-openai'))
			+ (SELECT count(*) FROM audit_logs WHERE details::text LIKE '%SECRETVALUE%')
			+ (SELECT count(*) FROM idempotency_keys WHERE convert_from(response_body, 'UTF8') LIKE '%SECRETVALUE%')`).Scan(&leaked)
		if leaked != 0 {
			t.Errorf("request bị từ chối để lại %d dấu vết", leaked)
		}

		g := e.do("GET", "/v1/model-providers/"+provID, adminA, nil, nil)
		want(t, g, 200, "")
		etag := g.header.Get("ETag")
		r = e.do("PATCH", "/v1/model-providers/"+provID, adminA, map[string]any{"settings": map[string]any{"note": "local"}}, map[string]string{"If-Match": etag})
		want(t, r, 200, "")
		want(t, e.do("PATCH", "/v1/model-providers/"+provID, adminA, map[string]any{"enabled": true}, map[string]string{"If-Match": etag}), 412, "precondition_failed")

		// Provider còn model thì không xoá được; provider rỗng thì xoá được.
		want(t, e.do("DELETE", "/v1/model-providers/"+provID, adminA, nil, nil), 409, "provider_in_use")
		empty := post(adminA, "/v1/model-providers", map[string]any{"slug": "empty", "adapter": "claude-cli"})["id"].(string)
		want(t, e.do("DELETE", "/v1/model-providers/"+empty, adminA, nil, nil), 204, "")
		want(t, e.do("GET", "/v1/model-providers/"+empty, adminA, nil, nil), 404, "not_found")
		want(t, e.do("DELETE", "/v1/model-providers/"+empty, adminA, nil, nil), 404, "not_found")
		// Xoá mềm: dòng còn đó để audit tra được.
		var archived bool
		_ = e.db.Admin.QueryRow(ctx, `SELECT archived_at IS NOT NULL AND NOT enabled FROM model_providers WHERE id = $1`, empty).Scan(&archived)
		if !archived {
			t.Error("provider phải được lưu trữ (archived_at) chứ không bị xoá cứng")
		}
		// settings được lưu plaintext và trả lại: không nhận thứ trông như bí mật.
		for _, s := range []map[string]any{
			{"api_key": "sk-ant-SECRETVALUE"}, {"nested": map[string]any{"Auth_Token": "x"}}, {"list": []any{map[string]any{"password": "x"}}},
		} {
			want(t, e.do("PATCH", "/v1/model-providers/"+provID, adminA, map[string]any{"settings": s}, nil), 422, "validation_failed")
		}
		if r := e.do("GET", "/v1/model-providers", adminA, nil, nil); r.status != 200 || len(r.json(t)["data"].([]any)) != 1 {
			t.Errorf("list providers: %d %s", r.status, r.body)
		}
	})

	t.Run("models", func(t *testing.T) {
		want(t, e.do("POST", "/v1/models", adminA, modelBody("sonnet", "standard"), nil), 409, "model_exists")
		bad := e.do("POST", "/v1/models", adminA, modelBody("opus", "ultra"), nil)
		want(t, bad, 422, "validation_failed")
		// Lỗi validate chỉ rõ trường sai (`errors[]` của problem+json).
		if errs, _ := bad.json(t)["errors"].([]any); len(errs) != 1 || errs[0].(map[string]any)["field"] != "tier" {
			t.Errorf("errors[] của tier sai: %s", bad.body)
		}
		want(t, e.do("POST", "/v1/models", adminA, modelBody("has space", "premium"), nil), 422, "validation_failed")
		missing := modelBody("opus", "premium")
		missing["provider_id"] = uuid.NewString()
		want(t, e.do("POST", "/v1/models", adminA, missing, nil), 422, "validation_failed")

		g := e.do("GET", "/v1/models/"+sonnet, adminA, nil, nil)
		want(t, g, 200, "")
		m := g.json(t)
		if m["tier"] != "standard" || m["has_current_pricing"] != false || m["provider_model"] != "sonnet" ||
			m["capabilities"].(map[string]any)["tool_calling"] != true {
			t.Errorf("model: %s", g.body)
		}
		count := func(q string) int {
			r := e.do("GET", "/v1/models?"+q, adminA, nil, nil)
			if r.status != 200 {
				t.Fatalf("list %s: %d %s", q, r.status, r.body)
			}
			return len(r.json(t)["data"].([]any))
		}
		if count("") != 2 || count("tier=economy") != 1 || count("tier=premium") != 0 || count("provider_id="+provID) != 2 ||
			count("provider_id="+uuid.NewString()) != 0 {
			t.Error("lọc model sai")
		}
		want(t, e.do("PATCH", "/v1/models/"+haiku, adminA, map[string]any{"enabled": false, "display_name": "Haiku (tắt)"}, nil), 200, "")
		if count("enabled=true") != 1 || count("enabled=false") != 1 {
			t.Error("lọc enabled sai")
		}

		// If-Match: ETag của bản đã đọc; lệch thì 412 và không sửa gì. Không gửi thì sửa vô điều kiện.
		etag := g.header.Get("ETag")
		if etag == "" {
			t.Fatal("GET /v1/models/{id} phải trả ETag")
		}
		u := e.do("PATCH", "/v1/models/"+sonnet, adminA, map[string]any{"display_name": "Sonnet A"}, map[string]string{"If-Match": etag})
		want(t, u, 200, "")
		if next := u.header.Get("ETag"); next == "" || next == etag {
			t.Errorf("PATCH phải trả ETag mới: %q → %q", etag, next)
		}
		want(t, e.do("PATCH", "/v1/models/"+sonnet, adminA, map[string]any{"display_name": "Sonnet B"}, map[string]string{"If-Match": etag}), 412, "precondition_failed")
		want(t, e.do("PATCH", "/v1/models/"+sonnet, adminA, map[string]any{"display_name": "Sonnet B"}, map[string]string{"If-Match": "không phải etag"}), 412, "precondition_failed")
		if name := e.do("GET", "/v1/models/"+sonnet, adminA, nil, nil).json(t)["display_name"]; name != "Sonnet A" {
			t.Errorf("PATCH bị 412 vẫn sửa tên: %v", name)
		}
		want(t, e.do("PATCH", "/v1/models/"+sonnet, adminA, map[string]any{"display_name": "sonnet"}, map[string]string{"If-Match": u.header.Get("ETag")}), 200, "")
		want(t, e.do("GET", "/v1/models?tier=bogus", adminA, nil, nil), 400, "invalid_parameter")
		want(t, e.do("GET", "/v1/models?enabled=maybe", adminA, nil, nil), 400, "invalid_parameter")
		// Phân trang.
		r := e.do("GET", "/v1/models?limit=1", adminA, nil, nil)
		next, _ := r.json(t)["next_cursor"].(string)
		r2 := e.do("GET", "/v1/models?limit=1&cursor="+next, adminA, nil, nil)
		if next == "" || len(r2.json(t)["data"].([]any)) != 1 || r2.json(t)["next_cursor"] != nil ||
			r.json(t)["data"].([]any)[0].(map[string]any)["id"] == r2.json(t)["data"].([]any)[0].(map[string]any)["id"] {
			t.Errorf("phân trang model: %s | %s", r.body, r2.body)
		}
	})

	t.Run("pricing", func(t *testing.T) {
		path := "/v1/models/" + sonnet + "/pricing"
		ts := func(d time.Duration) string { return time.Now().Add(d).UTC().Format(time.RFC3339Nano) }
		price := func(in, out, from string) map[string]any {
			return map[string]any{"currency": "USD", "input_per_mtok": in, "output_per_mtok": out, "effective_from": from,
				"cached_input_per_mtok": "0.10", "source": "bảng giá công khai 2026-11"}
		}
		// Giá không viết lại được quá khứ, và không đặt lịch quá xa.
		want(t, e.do("POST", path, adminA, price("0", "0", "2000-01-01T00:00:00Z"), nil), 422, "validation_failed")
		want(t, e.do("POST", path, adminA, price("0", "0", ts(-time.Hour)), nil), 422, "validation_failed")
		want(t, e.do("POST", path, adminA, price("1", "1", ts(400*24*time.Hour)), nil), 422, "validation_failed")
		want(t, e.do("POST", path, adminA, price("1", "1", "2200-01-01T00:00:00Z"), nil), 422, "validation_failed")

		r := e.do("POST", path, adminA, price("2.00", "10.00", ts(0)), nil)
		want(t, r, 201, "")
		p1 := r.json(t)
		if p1["version"] != float64(1) || p1["scope"] != "organization" || p1["input_per_mtok"] != "2.00000000" ||
			p1["cached_input_per_mtok"] != "0.10000000" || p1["effective_to"] != nil || p1["reasoning_per_mtok"] != nil {
			t.Errorf("giá v1: %s", r.body)
		}
		// Thời điểm bắt đầu do database quyết định: không sớm hơn lúc ghi.
		from1, err := time.Parse(time.RFC3339Nano, p1["effective_from"].(string))
		if err != nil || from1.Before(time.Now().Add(-time.Minute)) {
			t.Errorf("effective_from v1 = %v (%v)", p1["effective_from"], err)
		}
		time.Sleep(50 * time.Millisecond)
		if g := e.do("GET", "/v1/models/"+sonnet, adminA, nil, nil).json(t); g["has_current_pricing"] != true {
			t.Error("has_current_pricing phải là true sau khi có giá")
		}

		// Giá mới (đặt lịch 1 giờ nữa) đóng giá cũ đúng tại effective_from của nó.
		later := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
		r = e.do("POST", path, adminA, price("1.50", "8.00", later.Format(time.RFC3339)), nil)
		want(t, r, 201, "")
		if r.json(t)["version"] != float64(2) {
			t.Errorf("giá v2: %s", r.body)
		}
		l := e.do("GET", path, adminA, nil, nil)
		data := l.json(t)["data"].([]any)
		if len(data) != 2 || data[0].(map[string]any)["version"] != float64(2) ||
			data[1].(map[string]any)["effective_to"] != later.Format(time.RFC3339) {
			t.Errorf("lịch sử giá: %s", l.body)
		}
		// Tra giá theo thời điểm phát sinh.
		at := func(when time.Time) []any {
			return e.do("GET", path+"?effective_at="+when.UTC().Format(time.RFC3339), adminA, nil, nil).json(t)["data"].([]any)
		}
		if d := at(time.Now().Add(30 * time.Minute)); len(d) != 1 || d[0].(map[string]any)["version"] != float64(1) {
			t.Errorf("giá sau 30 phút: %v", d)
		}
		if d := at(later); len(d) != 1 || d[0].(map[string]any)["version"] != float64(2) {
			t.Errorf("giá tại đúng thời điểm chuyển: %v", d)
		}
		if d := at(time.Now().Add(-24 * time.Hour)); len(d) != 0 {
			t.Errorf("trước khi có giá: %v", d)
		}

		// Giá sau phải bắt đầu sau giá đang mở: không chen vào trước một giá đã đặt lịch.
		want(t, e.do("POST", path, adminA, price("9.00", "9.00", later.Format(time.RFC3339)), nil), 409, "pricing_conflict")
		want(t, e.do("POST", path, adminA, price("9.00", "9.00", ts(time.Minute)), nil), 409, "pricing_conflict")
		for name, body := range map[string]map[string]any{
			"ký hiệu khoa học":       price("1e3", "1", ts(2*time.Hour)),
			"số âm":                  price("-1", "1", ts(2*time.Hour)),
			"NaN":                    price("NaN", "1", ts(2*time.Hour)),
			"quá 8 chữ số thập phân": price("0.123456789", "1", ts(2*time.Hour)),
			"float trong JSON":       {"currency": "USD", "input_per_mtok": 1.5, "output_per_mtok": "1", "effective_from": ts(2 * time.Hour)},
			"currency thường":        {"currency": "usd", "input_per_mtok": "1", "output_per_mtok": "1", "effective_from": ts(2 * time.Hour)},
			"thiếu output":           {"currency": "USD", "input_per_mtok": "1", "effective_from": ts(2 * time.Hour)},
			"source có NUL":          {"currency": "USD", "input_per_mtok": "1", "output_per_mtok": "1", "effective_from": ts(2 * time.Hour), "source": "a\x00b"},
		} {
			if r := e.do("POST", path, adminA, body, nil); r.status != 422 && r.status != 400 {
				t.Errorf("%s: %d %s", name, r.status, r.body)
			}
		}
		if n := len(e.do("GET", path, adminA, nil, nil).json(t)["data"].([]any)); n != 2 {
			t.Errorf("request sai để lại giá: còn %d dòng", n)
		}
		// Không gửi effective_from: giá áp dụng ngay theo giờ máy chủ (model khác, để không
		// đụng các dòng giá của sonnet được kiểm bên dưới).
		before := time.Now().Add(-time.Second)
		now := e.do("POST", "/v1/models/"+haiku+"/pricing", adminA, map[string]any{"currency": "USD", "input_per_mtok": "1", "output_per_mtok": "2"}, nil)
		want(t, now, 201, "")
		if at, err := time.Parse(time.RFC3339Nano, now.json(t)["effective_from"].(string)); err != nil || at.Before(before) || at.After(time.Now().Add(time.Second)) {
			t.Errorf("effective_from mặc định = %v (%v), muốn giờ máy chủ lúc tạo", now.json(t)["effective_from"], err)
		}
		if priced := e.do("GET", "/v1/models/"+haiku, adminA, nil, nil).json(t)["has_current_pricing"]; priced != true {
			t.Errorf("giá áp dụng ngay phải có hiệu lực ngay: has_current_pricing = %v", priced)
		}

		// Audit ghi đủ để tự tra: dòng mới, dòng bị đóng, các đơn giá.
		var full int
		_ = e.db.Admin.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action = 'model_pricing.create' AND resource_id = $1
			AND details ? 'pricing_id' AND details ? 'closed_pricing_id' AND details ->> 'cached_input_per_mtok' = '0.10000000'`, sonnet).Scan(&full)
		if full != 2 {
			t.Errorf("audit giá đầy đủ: %d bản, muốn 2", full)
		}

		// Bất biến ở mức database, kể cả với owner: chỉ được đóng dòng đang mở, và chỉ
		// đóng từ hiện tại trở đi (không rút ngắn khoảng thời gian đã trôi qua).
		for _, stmt := range []string{
			`UPDATE model_pricing SET input_per_mtok = 0 WHERE model_id = $1`,
			`UPDATE model_pricing SET effective_to = NULL WHERE model_id = $1 AND effective_to IS NOT NULL`,
			`UPDATE model_pricing SET effective_to = effective_to + interval '1 day' WHERE model_id = $1 AND effective_to IS NOT NULL`,
			`UPDATE model_pricing SET effective_to = now() - interval '1 second' WHERE model_id = $1 AND effective_to IS NULL`,
			`DELETE FROM model_pricing WHERE model_id = $1`,
		} {
			if _, err := e.db.Admin.Exec(ctx, stmt, sonnet); err == nil || !strings.Contains(err.Error(), "immutable") {
				t.Errorf("%q: muốn lỗi immutable, nhận %v", stmt, err)
			}
		}
		if _, err := e.db.Admin.Exec(ctx, `TRUNCATE model_pricing`); err == nil {
			t.Error("TRUNCATE model_pricing được phép")
		}
		want(t, e.do("GET", "/v1/models/"+uuid.NewString()+"/pricing", adminA, nil, nil), 404, "not_found")
	})

	t.Run("platform-managed catalog is read-only for tenants", func(t *testing.T) {
		pp, pm := ids.New(), ids.New()
		for _, q := range []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO model_providers (id, org_id, slug, adapter) VALUES ($1, NULL, 'platform-claude', 'claude-cli')`, []any{pp}},
			{`INSERT INTO models (id, org_id, provider_id, provider_model, display_name, tier) VALUES ($1, NULL, $2, 'opus', 'Opus (platform)', 'premium')`, []any{pm, pp}},
			{`INSERT INTO model_pricing (model_id, org_id, version, input_per_mtok, output_per_mtok, effective_from) VALUES ($1, NULL, 1, 4, 20, '2026-01-01')`, []any{pm}},
		} {
			if _, err := e.db.Admin.Exec(ctx, q.sql, q.args...); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
		for _, tok := range []string{adminA, adminB} {
			g := e.do("GET", "/v1/models/"+pm.String(), tok, nil, nil)
			if g.status != 200 || g.json(t)["has_current_pricing"] != true {
				t.Errorf("tenant phải thấy model của platform có giá niêm yết: %d %s", g.status, g.body)
			}
			if p := e.do("GET", "/v1/model-providers/"+pp.String(), tok, nil, nil); p.status != 200 || p.json(t)["scope"] != "platform" {
				t.Errorf("provider của platform: %d %s", p.status, p.body)
			}
		}
		want(t, e.do("PATCH", "/v1/models/"+pm.String(), adminA, map[string]any{"enabled": false}, nil), 403, "platform_managed")
		want(t, e.do("PATCH", "/v1/model-providers/"+pp.String(), adminA, map[string]any{"enabled": false}, nil), 403, "platform_managed")
		want(t, e.do("DELETE", "/v1/model-providers/"+pp.String(), adminA, nil, nil), 403, "platform_managed")
		under := map[string]any{"provider_id": pp.String(), "provider_model": "sneaky", "display_name": "x"}
		want(t, e.do("POST", "/v1/models", adminA, under, nil), 403, "platform_managed")

		// Tenant không tự đặt được giá cho model của platform: nếu được, đặt giá 0 là
		// vô hiệu hoá mọi budget theo tiền. Giá của model platform do platform quản lý.
		path := "/v1/models/" + pm.String() + "/pricing"
		zero := map[string]any{"currency": "USD", "input_per_mtok": "0", "output_per_mtok": "0",
			"effective_from": time.Now().UTC().Format(time.RFC3339Nano)}
		want(t, e.do("POST", path, adminA, zero, nil), 403, "platform_managed")
		for _, tok := range []string{adminA, adminB} {
			d := e.do("GET", path, tok, nil, nil).json(t)["data"].([]any)
			if len(d) != 1 || d[0].(map[string]any)["scope"] != "list" || d[0].(map[string]any)["input_per_mtok"] != "4.00000000" {
				t.Errorf("giá của model platform phải chỉ có giá niêm yết: %v", d)
			}
		}
		// Cấu hình vận hành của provider platform không lộ cho tenant.
		if _, err := e.db.Admin.Exec(ctx, `UPDATE model_providers SET settings = '{"internal_endpoint":"http://10.0.0.9"}', base_url = 'http://10.0.0.9' WHERE id = $1`, pp); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{"/v1/model-providers/" + pp.String(), "/v1/model-providers?limit=200"} {
			if r := e.do("GET", p, adminA, nil, nil); strings.Contains(string(r.body), "10.0.0.9") {
				t.Errorf("%s lộ cấu hình của provider platform: %s", p, r.body)
			}
		}
	})

	t.Run("agent version can pin a usable model", func(t *testing.T) {
		proj := e.createProject(adminA, "pin-model", "Pin")["id"].(string)
		body := func(slug, model string) map[string]any {
			b := agentBody(slug)
			b["config"].(map[string]any)["model"] = map[string]any{"policy": "fixed", "fixed_model_id": model}
			return b
		}
		path := "/v1/projects/" + proj + "/agents"
		want(t, e.do("POST", path, adminA, body("pinned", sonnet), nil), 201, "")
		// haiku đã bị tắt ở test trên; model của org khác và model không tồn tại đều bị từ chối như nhau.
		want(t, e.do("POST", path, adminA, body("pinned-disabled", haiku), nil), 422, "validation_failed")
		want(t, e.do("POST", path, adminA, body("pinned-missing", uuid.NewString()), nil), 422, "validation_failed")
		provB := post(adminB, "/v1/model-providers", map[string]any{"slug": "b-claude", "adapter": "claude-cli"})["id"].(string)
		modelB := post(adminB, "/v1/models", map[string]any{"provider_id": provB, "provider_model": "sonnet", "display_name": "B Sonnet"})["id"].(string)
		// Model bị tắt sau khi version được tạo: publish version đó bị từ chối.
		target := post(adminA, "/v1/models", modelBody("pin-target", "standard"))["id"].(string)
		agentID := post(adminA, path, body("pinned-later-disabled", target))["id"].(string)
		want(t, e.do("PATCH", "/v1/models/"+target, adminA, map[string]any{"enabled": false}, nil), 200, "")
		want(t, e.do("PUT", "/v1/agents/"+agentID+"/active-version", adminA, map[string]any{"version": 1}, nil), 422, "validation_failed")
		want(t, e.do("PATCH", "/v1/models/"+target, adminA, map[string]any{"enabled": true}, nil), 200, "")
		want(t, e.do("PUT", "/v1/agents/"+agentID+"/active-version", adminA, map[string]any{"version": 1}, nil), 200, "")
		r := e.do("POST", path, adminA, body("pinned-foreign", modelB), nil)
		miss := e.do("POST", path, adminA, body("pinned-missing2", uuid.NewString()), nil)
		if r.status != 422 || string(r.body) != string(miss.body) {
			t.Errorf("model của org khác phải bị từ chối giống hệt model không tồn tại: %d %s", r.status, r.body)
		}
	})

	t.Run("authorization", func(t *testing.T) {
		for _, p := range []string{"/v1/model-providers", "/v1/model-providers/" + provID, "/v1/models", "/v1/models/" + sonnet, "/v1/models/" + sonnet + "/pricing"} {
			want(t, e.do("GET", p, viewerA, nil, nil), 200, "")
		}
		want(t, e.do("POST", "/v1/model-providers", viewerA, map[string]any{"slug": "v", "adapter": "claude-cli"}, nil), 403, "permission_denied")
		want(t, e.do("POST", "/v1/models", viewerA, modelBody("v", "economy"), nil), 403, "permission_denied")
		want(t, e.do("PATCH", "/v1/models/"+sonnet, viewerA, map[string]any{"enabled": false}, nil), 403, "permission_denied")
		want(t, e.do("DELETE", "/v1/model-providers/"+provID, viewerA, nil, nil), 403, "permission_denied")
		want(t, e.do("POST", "/v1/models/"+sonnet+"/pricing", viewerA,
			map[string]any{"currency": "USD", "input_per_mtok": "0", "output_per_mtok": "0", "effective_from": time.Now().Add(3 * time.Hour).UTC().Format(time.RFC3339)}, nil), 403, "permission_denied")
		noRole := e.tok("norole-a")
		want(t, e.do("GET", "/v1/models", noRole, nil, nil), 403, "permission_denied")
		want(t, e.do("GET", "/v1/models/"+sonnet, noRole, nil, nil), 404, "not_found")
		want(t, e.do("GET", "/v1/model-providers/"+provID, noRole, nil, nil), 404, "not_found")
		want(t, e.do("GET", "/v1/models/"+sonnet+"/pricing", noRole, nil, nil), 404, "not_found")
	})

	t.Run("cross tenant", func(t *testing.T) {
		newPrice := map[string]any{"currency": "USD", "input_per_mtok": "0", "output_per_mtok": "0", "effective_from": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}
		for name, r := range map[string]resp{
			"get provider":    e.do("GET", "/v1/model-providers/"+provID, adminB, nil, nil),
			"patch provider":  e.do("PATCH", "/v1/model-providers/"+provID, adminB, map[string]any{"enabled": false}, nil),
			"delete provider": e.do("DELETE", "/v1/model-providers/"+provID, adminB, nil, nil),
			"get model":       e.do("GET", "/v1/models/"+sonnet, adminB, nil, nil),
			"patch model":     e.do("PATCH", "/v1/models/"+sonnet, adminB, map[string]any{"enabled": false}, nil),
			"list pricing":    e.do("GET", "/v1/models/"+sonnet+"/pricing", adminB, nil, nil),
			"create pricing":  e.do("POST", "/v1/models/"+sonnet+"/pricing", adminB, newPrice, nil),
		} {
			if r.status != 404 || r.code(t) != "not_found" {
				t.Errorf("%s: %d %s", name, r.status, r.body)
			}
		}
		// Thêm model dưới provider của org khác: provider "không tồn tại".
		want(t, e.do("POST", "/v1/models", adminB, modelBody("intruder", "economy"), nil), 422, "validation_failed")
		for _, p := range []string{"/v1/models?limit=200", "/v1/model-providers?limit=200"} {
			if r := e.do("GET", p, adminB, nil, nil); strings.Contains(string(r.body), provID) || strings.Contains(string(r.body), sonnet) {
				t.Errorf("%s của org B lộ dữ liệu org A", p)
			}
		}
		if g := e.do("GET", "/v1/models/"+sonnet, adminA, nil, nil).json(t); g["enabled"] != true {
			t.Errorf("model của org A bị thay đổi: %v", g)
		}
	})
}
