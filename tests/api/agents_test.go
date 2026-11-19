package api_test

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func agentBody(slug string) map[string]any {
	return map[string]any{
		"slug": slug, "name": "Agent " + slug, "system_prompt": "Bạn là DevOps Agent.",
		"config": validConfig(),
	}
}

func validConfig() map[string]any {
	return map[string]any{
		"model":  map[string]any{"policy": "tiered", "default_tier": "standard"},
		"limits": map[string]any{"max_iterations": 15, "max_tool_calls": 40, "timeout_seconds": 900},
		"tools":  []any{},
	}
}

func TestAgentsAPI(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	adminA, adminB, viewerA, devA := e.tok("admin-a"), e.tok("admin-b"), e.tok("viewer-a"), e.tok("dev-a")
	projA := e.createProject(adminA, "agents-a", "Agents A")["id"].(string)
	projA2 := e.createProject(adminA, "agents-a2", "Agents A2")["id"].(string)
	projB := e.createProject(adminB, "agents-b", "Agents B")["id"].(string)
	agentsPath := func(p string) string { return "/v1/projects/" + p + "/agents" }

	create := func(tok, proj, slug string) map[string]any {
		t.Helper()
		r := e.do("POST", agentsPath(proj), tok, agentBody(slug), nil)
		if r.status != 201 {
			t.Fatalf("create agent %s: %d %s", slug, r.status, r.body)
		}
		return r.json(t)
	}

	t.Run("create and read", func(t *testing.T) {
		r := e.do("POST", agentsPath(projA), adminA, agentBody("devops"), nil)
		want(t, r, 201, "")
		a := r.json(t)
		id := a["id"].(string)
		if a["status"] != "draft" || a["latest_version"] != float64(1) || a["active_version"] != nil ||
			a["project_id"] != projA || r.header.Get("ETag") == "" || r.header.Get("Location") != "/v1/agents/"+id {
			t.Errorf("agent mới: %s", r.body)
		}
		want(t, e.do("GET", "/v1/agents/"+id, adminA, nil, nil), 200, "")
		v := e.do("GET", "/v1/agents/"+id+"/versions/1", adminA, nil, nil)
		want(t, v, 200, "")
		vj := v.json(t)
		if vj["system_prompt"] != "Bạn là DevOps Agent." || vj["created_by"] == nil ||
			vj["config"].(map[string]any)["limits"].(map[string]any)["max_iterations"] != float64(15) {
			t.Errorf("version 1: %s", v.body)
		}
		want(t, e.do("POST", agentsPath(projA), adminA, agentBody("devops"), nil), 409, "slug_taken")
		// Slug chỉ duy nhất trong một project.
		want(t, e.do("POST", agentsPath(projA2), adminA, agentBody("devops"), nil), 201, "")
		want(t, e.do("GET", "/v1/agents/"+id+"/versions/2", adminA, nil, nil), 404, "not_found")
		want(t, e.do("GET", "/v1/agents/"+id+"/versions/abc", adminA, nil, nil), 404, "not_found")
		want(t, e.do("GET", "/v1/agents/"+uuid.NewString(), adminA, nil, nil), 404, "not_found")
		want(t, e.do("POST", agentsPath(uuid.NewString()), adminA, agentBody("x-1"), nil), 404, "not_found")
	})

	t.Run("version validation is fail-closed", func(t *testing.T) {
		mut := func(f func(c map[string]any)) map[string]any {
			b := agentBody("bad-" + uuid.NewString()[:8])
			f(b["config"].(map[string]any))
			return b
		}
		cases := map[string]map[string]any{
			"max_iterations 0":     mut(func(c map[string]any) { c["limits"].(map[string]any)["max_iterations"] = 0 }),
			"vượt trần iterations": mut(func(c map[string]any) { c["limits"].(map[string]any)["max_iterations"] = 100000 }),
			"vượt trần tool calls": mut(func(c map[string]any) { c["limits"].(map[string]any)["max_tool_calls"] = 100000 }),
			"timeout âm":           mut(func(c map[string]any) { c["limits"].(map[string]any)["timeout_seconds"] = -1 }),
			"policy lạ":            mut(func(c map[string]any) { c["model"] = map[string]any{"policy": "smart"} }),
			"fixed thiếu model":    mut(func(c map[string]any) { c["model"] = map[string]any{"policy": "fixed"} }),
			"fixed với model không tồn tại": mut(func(c map[string]any) {
				c["model"] = map[string]any{"policy": "fixed", "fixed_model_id": uuid.NewString()}
			}),
			"tool khi chưa có registry": mut(func(c map[string]any) {
				c["tools"] = []any{map[string]any{"server_id": uuid.NewString(), "allow": []string{"*"}}}
			}),
			"delegation": mut(func(c map[string]any) {
				c["delegation"] = map[string]any{"can_delegate_to": []string{uuid.NewString()}, "max_depth": 2}
			}),
			"isolated runtime": mut(func(c map[string]any) { c["runtime"] = map[string]any{"isolation": "isolated"} }),
			"budget âm": mut(func(c map[string]any) {
				c["limits"].(map[string]any)["budget"] = map[string]any{"amount": "-999999999999999999999", "currency": "USD"}
			}),
			"budget không phải số": mut(func(c map[string]any) {
				c["limits"].(map[string]any)["budget"] = map[string]any{"amount": "1e9", "currency": "USD"}
			}),
			"budget bằng 0": mut(func(c map[string]any) {
				c["limits"].(map[string]any)["budget"] = map[string]any{"amount": "0.00", "currency": "USD"}
			}),
			"budget vượt trần": mut(func(c map[string]any) {
				c["limits"].(map[string]any)["budget"] = map[string]any{"amount": "1000.01", "currency": "USD"}
			}),
			"budget tiền tệ lạ": mut(func(c map[string]any) {
				c["limits"].(map[string]any)["budget"] = map[string]any{"amount": "2.00", "currency": "not a currency"}
			}),
			"session_history lạ":          mut(func(c map[string]any) { c["memory"] = map[string]any{"session_history": "whatever"} }),
			"session_history chưa hỗ trợ": mut(func(c map[string]any) { c["memory"] = map[string]any{"session_history": "full"} }),
			"max_depth âm":                mut(func(c map[string]any) { c["delegation"] = map[string]any{"max_depth": -5} }),
			"max_output_tokens quá lớn": mut(func(c map[string]any) {
				c["model"] = map[string]any{"policy": "tiered", "max_output_tokens": 2000000000}
			}),
			"default_tier lạ": mut(func(c map[string]any) { c["model"] = map[string]any{"policy": "tiered", "default_tier": "ultra"} }),
			"NUL trong role":  mut(func(c map[string]any) { c["role"] = "a\x00b" }),
		}
		for name, body := range cases {
			r := e.do("POST", agentsPath(projA), adminA, body, nil)
			if r.status != 422 || r.code(t) != "validation_failed" {
				t.Errorf("%s: %d %s", name, r.status, r.body)
			}
		}
		// Tên chứa ký tự định dạng vô hình (bidi override) bị từ chối.
		bidi := agentBody("bidi-name")
		bidi["name"] = "safe\u202Etxt.exe"
		want(t, e.do("POST", agentsPath(projA), adminA, bidi, nil), 422, "validation_failed")
		// Budget hợp lệ được nhận.
		withBudget := agentBody("with-budget")
		withBudget["config"].(map[string]any)["limits"].(map[string]any)["budget"] = map[string]any{"amount": "2.50", "currency": "USD"}
		want(t, e.do("POST", agentsPath(projA), adminA, withBudget, nil), 201, "")
		noPrompt := agentBody("no-prompt")
		noPrompt["system_prompt"] = "   "
		want(t, e.do("POST", agentsPath(projA), adminA, noPrompt, nil), 422, "validation_failed")
		badSlug := agentBody("Bad Slug")
		want(t, e.do("POST", agentsPath(projA), adminA, badSlug, nil), 422, "validation_failed")
		// Không agent nào được tạo dở dang.
		var n int
		_ = e.db.Admin.QueryRow(ctx, `SELECT count(*) FROM agents WHERE slug LIKE 'bad-%' OR slug IN ('no-prompt','bidi-name')`).Scan(&n)
		if n != 0 {
			t.Errorf("%d agent được tạo từ request không hợp lệ", n)
		}
	})

	t.Run("lifecycle, versions and publish", func(t *testing.T) {
		a := create(adminA, projA, "lifecycle")
		id := a["id"].(string)
		p := "/v1/agents/" + id

		// Draft không bật được khi chưa publish version nào.
		want(t, e.do("PATCH", p, adminA, map[string]any{"status": "active"}, nil), 409, "no_active_version")
		// PATCH không lưu trữ được agent: việc đó chỉ qua DELETE.
		want(t, e.do("PATCH", p, adminA, map[string]any{"status": "archived"}, nil), 422, "validation_failed")

		v2 := map[string]any{"system_prompt": "Prompt v2", "config": validConfig(), "change_note": "siết giới hạn"}
		r := e.do("POST", p+"/versions", adminA, v2, nil)
		want(t, r, 201, "")
		if r.json(t)["version"] != float64(2) || r.header.Get("Location") != p+"/versions/2" {
			t.Errorf("version 2: %s", r.body)
		}

		// Publish version 2: agent rời draft.
		r = e.do("PUT", p+"/active-version", adminA, map[string]any{"version": 2}, nil)
		want(t, r, 200, "")
		if g := r.json(t); g["status"] != "active" || g["active_version"] != float64(2) || g["latest_version"] != float64(2) {
			t.Errorf("sau publish: %s", r.body)
		}
		// Rollback về version 1.
		r = e.do("PUT", p+"/active-version", adminA, map[string]any{"version": 1}, nil)
		if g := r.json(t); r.status != 200 || g["active_version"] != float64(1) || g["status"] != "active" {
			t.Errorf("sau rollback: %d %s", r.status, r.body)
		}
		want(t, e.do("PUT", p+"/active-version", adminA, map[string]any{"version": 99}, nil), 404, "not_found")
		want(t, e.do("PUT", p+"/active-version", adminA, map[string]any{"version": 0}, nil), 422, "validation_failed")

		// Tắt / bật, có kiểm ETag.
		etag := r.header.Get("ETag")
		r = e.do("PATCH", p, adminA, map[string]any{"status": "disabled", "name": "Renamed"}, map[string]string{"If-Match": etag})
		if g := r.json(t); r.status != 200 || g["status"] != "disabled" || g["name"] != "Renamed" || g["active_version"] != float64(1) {
			t.Errorf("disable: %d %s", r.status, r.body)
		}
		want(t, e.do("PATCH", p, adminA, map[string]any{"status": "active"}, map[string]string{"If-Match": etag}), 412, "precondition_failed")
		want(t, e.do("PATCH", p, adminA, map[string]any{"status": "active"}, nil), 200, "")

		// Tạo và publish trong một bước.
		v3 := map[string]any{"system_prompt": "Prompt v3", "config": validConfig(), "activate": true}
		want(t, e.do("POST", p+"/versions", adminA, v3, nil), 201, "")
		if g := e.do("GET", p, adminA, nil, nil).json(t); g["active_version"] != float64(3) {
			t.Errorf("activate=true không publish: %v", g)
		}

		// Gửi lại đúng nội dung của version mới nhất không sinh version mới.
		dup := e.do("POST", p+"/versions", adminA, map[string]any{"system_prompt": "Prompt v3", "config": validConfig()}, nil)
		if dup.status != 201 || dup.json(t)["version"] != float64(3) {
			t.Errorf("nội dung trùng phải trả version 3: %d %s", dup.status, dup.body)
		}

		// Danh sách version: mới nhất trước, phân trang.
		r = e.do("GET", p+"/versions?limit=2", adminA, nil, nil)
		page := r.json(t)
		data := page["data"].([]any)
		if len(data) != 2 || data[0].(map[string]any)["version"] != float64(3) || page["next_cursor"] != "2" {
			t.Errorf("trang 1: %s", r.body)
		}
		r = e.do("GET", p+"/versions?limit=2&cursor=2", adminA, nil, nil)
		page = r.json(t)
		if d := page["data"].([]any); len(d) != 1 || d[0].(map[string]any)["version"] != float64(1) || page["next_cursor"] != nil {
			t.Errorf("trang 2: %s", r.body)
		}

		// Version bất biến ở mức database, kể cả với superuser.
		for _, stmt := range []string{
			`UPDATE agent_versions SET system_prompt = 'pwned' WHERE agent_id = $1`,
			`DELETE FROM agent_versions WHERE agent_id = $1`,
		} {
			if _, err := e.db.Admin.Exec(ctx, stmt, id); err == nil || !strings.Contains(err.Error(), "immutable") {
				t.Errorf("%q: muốn lỗi immutable, nhận %v", stmt, err)
			}
		}
		// Database từ chối active_version trỏ sang version của agent khác.
		other := create(adminA, projA, "lifecycle-other")["id"].(string)
		if _, err := e.db.Admin.Exec(ctx, `UPDATE agents SET active_version_id =
			(SELECT id FROM agent_versions WHERE agent_id = $1 AND version = 1) WHERE id = $2`, id, other); err == nil {
			t.Error("active_version trỏ được sang version của agent khác")
		}

		// Lưu trữ là trạng thái cuối.
		want(t, e.do("DELETE", p, adminA, nil, nil), 204, "")
		want(t, e.do("DELETE", p, adminA, nil, nil), 204, "")
		want(t, e.do("PATCH", p, adminA, map[string]any{"name": "x"}, nil), 409, "agent_archived")
		want(t, e.do("POST", p+"/versions", adminA, v2, nil), 409, "agent_archived")
		want(t, e.do("PUT", p+"/active-version", adminA, map[string]any{"version": 1}, nil), 409, "agent_archived")
		if g := e.do("GET", p, adminA, nil, nil).json(t); g["status"] != "archived" {
			t.Errorf("sau archive: %v", g)
		}

		// Mỗi bước để lại audit.
		rows, _ := e.db.Admin.Query(ctx, `SELECT action, count(*) FROM audit_logs WHERE resource_id = $1 GROUP BY 1`, id)
		got := map[string]int{}
		for rows.Next() {
			var k string
			var n int
			_ = rows.Scan(&k, &n)
			got[k] = n
		}
		rows.Close()
		wantCounts := map[string]int{"agent.create": 1, "agent.version.create": 2, "agent.publish": 3, "agent.update": 2, "agent.archive": 1}
		for k, n := range wantCounts {
			if got[k] != n {
				t.Errorf("audit %s = %d, muốn %d (có: %v)", k, got[k], n, got)
			}
		}
	})

	t.Run("concurrent version creation is serialized", func(t *testing.T) {
		id := create(adminA, projA, "concurrent")["id"].(string)
		const n = 12
		res := make(chan float64, n)
		for i := 0; i < n; i++ {
			go func(i int) {
				r := e.do("POST", "/v1/agents/"+id+"/versions", adminA,
					map[string]any{"system_prompt": fmt.Sprintf("p%d", i), "config": validConfig()}, nil)
				if r.status != 201 {
					res <- -1
					return
				}
				res <- r.json(t)["version"].(float64)
			}(i)
		}
		var got []int
		for i := 0; i < n; i++ {
			got = append(got, int(<-res))
		}
		sort.Ints(got)
		for i, v := range got {
			if v != i+2 {
				t.Fatalf("số version không liên tục, không trùng: %v", got)
			}
		}
	})

	t.Run("list filter search and sort", func(t *testing.T) {
		proj := e.createProject(adminA, "agents-list", "List")["id"].(string)
		for _, s := range []string{"charlie", "alpha", "bravo"} {
			create(adminA, proj, s)
		}
		arch := create(adminA, proj, "zulu-archived")["id"].(string)
		want(t, e.do("DELETE", "/v1/agents/"+arch, adminA, nil, nil), 204, "")

		slugs := func(query string) []string {
			var out []string
			cursor := ""
			for i := 0; i < 10; i++ {
				r := e.do("GET", agentsPath(proj)+"?"+query+"&cursor="+cursor, adminA, nil, nil)
				if r.status != 200 {
					t.Fatalf("list %s: %d %s", query, r.status, r.body)
				}
				m := r.json(t)
				for _, d := range m["data"].([]any) {
					out = append(out, d.(map[string]any)["slug"].(string))
				}
				next, ok := m["next_cursor"].(string)
				if !ok {
					return out
				}
				cursor = next
			}
			t.Fatal("phân trang không kết thúc")
			return nil
		}
		eq := func(got []string, want ...string) {
			t.Helper()
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("got %v, muốn %v", got, want)
			}
		}
		eq(slugs("limit=2"), "alpha", "bravo", "charlie") // mặc định theo name, không gồm archived
		eq(slugs("limit=2&sort=-name"), "charlie", "bravo", "alpha")
		eq(slugs("limit=2&sort=created_at"), "charlie", "alpha", "bravo")
		eq(slugs("limit=2&sort=-created_at"), "bravo", "alpha", "charlie")
		eq(slugs("status=archived"), "zulu-archived")
		eq(slugs("status=draft&q=RAV"), "bravo")
		eq(slugs("q=%25")) // ký tự đại diện của người dùng không có tác dụng
		eq(slugs("q=a_p")) // tương tự với _
		want(t, e.do("GET", agentsPath(proj)+"?status=bogus", adminA, nil, nil), 400, "invalid_parameter")
		want(t, e.do("GET", agentsPath(proj)+"?sort=slug", adminA, nil, nil), 400, "invalid_parameter")
		want(t, e.do("GET", agentsPath(proj)+"?limit=500", adminA, nil, nil), 400, "invalid_parameter")
	})

	t.Run("authorization", func(t *testing.T) {
		id := create(adminA, projA, "authz-agent")["id"].(string)
		p := "/v1/agents/" + id
		v := map[string]any{"system_prompt": "p", "config": validConfig()}

		// viewer: chỉ đọc.
		want(t, e.do("GET", p, viewerA, nil, nil), 200, "")
		want(t, e.do("GET", agentsPath(projA), viewerA, nil, nil), 200, "")
		want(t, e.do("GET", p+"/versions", viewerA, nil, nil), 200, "")
		want(t, e.do("POST", agentsPath(projA), viewerA, agentBody("nope"), nil), 403, "permission_denied")
		want(t, e.do("PATCH", p, viewerA, map[string]any{"name": "n"}, nil), 403, "permission_denied")
		want(t, e.do("POST", p+"/versions", viewerA, v, nil), 403, "permission_denied")
		want(t, e.do("PUT", p+"/active-version", viewerA, map[string]any{"version": 1}, nil), 403, "permission_denied")
		want(t, e.do("DELETE", p, viewerA, nil, nil), 403, "permission_denied")

		// developer: viết được version, nhưng publish là quyền riêng.
		want(t, e.do("POST", p+"/versions", devA, v, nil), 201, "")
		want(t, e.do("PUT", p+"/active-version", devA, map[string]any{"version": 2}, nil), 403, "permission_denied")
		withActivate := map[string]any{"system_prompt": "p", "config": validConfig(), "activate": true}
		want(t, e.do("POST", p+"/versions", devA, withActivate, nil), 403, "permission_denied")
		want(t, e.do("DELETE", p, devA, nil, nil), 403, "permission_denied")
		// Sửa tên thì được, nhưng bật agent là quyết định publish.
		want(t, e.do("PATCH", p, devA, map[string]any{"name": "Renamed by dev"}, nil), 200, "")
		want(t, e.do("PATCH", p, devA, map[string]any{"status": "active"}, nil), 403, "permission_denied")
		if g := e.do("GET", p, adminA, nil, nil).json(t); g["status"] != "draft" || g["latest_version"] != float64(2) {
			t.Errorf("developer đã publish hoặc tạo thừa version: %v", g)
		}
		// Không có role: agent không tồn tại.
		want(t, e.do("GET", p, e.tok("norole-a"), nil, nil), 404, "not_found")
		want(t, e.do("GET", agentsPath(projA), e.tok("norole-a"), nil, nil), 404, "not_found")
	})

	t.Run("cross tenant", func(t *testing.T) {
		id := create(adminA, projA, "secret-agent")["id"].(string)
		p := "/v1/agents/" + id
		v := map[string]any{"system_prompt": "pwned", "config": validConfig()}
		for name, r := range map[string]resp{
			"get":                            e.do("GET", p, adminB, nil, nil),
			"patch":                          e.do("PATCH", p, adminB, map[string]any{"name": "pwned"}, nil),
			"delete":                         e.do("DELETE", p, adminB, nil, nil),
			"list versions":                  e.do("GET", p+"/versions", adminB, nil, nil),
			"get version":                    e.do("GET", p+"/versions/1", adminB, nil, nil),
			"create version":                 e.do("POST", p+"/versions", adminB, v, nil),
			"set active":                     e.do("PUT", p+"/active-version", adminB, map[string]any{"version": 1}, nil),
			"list in project":                e.do("GET", agentsPath(projA), adminB, nil, nil),
			"create in project of other org": e.do("POST", agentsPath(projA), adminB, agentBody("intruder"), nil),
		} {
			if r.status != 404 || r.code(t) != "not_found" {
				t.Errorf("%s: %d %s", name, r.status, r.body)
			}
		}
		// Org B thấy danh sách rỗng trong project của mình, và org A nguyên vẹn.
		r := e.do("GET", agentsPath(projB), adminB, nil, nil)
		if r.status != 200 || strings.Contains(string(r.body), "secret-agent") {
			t.Errorf("danh sách của org B: %d %s", r.status, r.body)
		}
		if g := e.do("GET", p, adminA, nil, nil).json(t); g["name"] != "Agent secret-agent" || g["status"] != "draft" || g["latest_version"] != float64(1) {
			t.Errorf("agent của org A bị thay đổi: %v", g)
		}
	})

	t.Run("idempotent create", func(t *testing.T) {
		key := map[string]string{"Idempotency-Key": "agent-" + uuid.NewString()}
		first := e.do("POST", agentsPath(projA), adminA, agentBody("idem-agent"), key)
		again := e.do("POST", agentsPath(projA), adminA, agentBody("idem-agent"), key)
		if first.status != 201 || again.status != 201 || string(first.body) != string(again.body) {
			t.Errorf("idempotent create: %d %d", first.status, again.status)
		}
	})

	t.Run("archived project freezes its agents", func(t *testing.T) {
		proj := e.createProject(adminA, "agents-archived", "Archived")["id"].(string)
		id := create(adminA, proj, "frozen")["id"].(string)
		p := "/v1/agents/" + id
		want(t, e.do("DELETE", "/v1/projects/"+proj, adminA, nil, nil), 204, "")
		want(t, e.do("POST", agentsPath(proj), adminA, agentBody("late"), nil), 409, "project_archived")
		v := map[string]any{"system_prompt": "late", "config": validConfig(), "activate": true}
		want(t, e.do("POST", p+"/versions", adminA, v, nil), 409, "project_archived")
		want(t, e.do("PUT", p+"/active-version", adminA, map[string]any{"version": 1}, nil), 409, "project_archived")
		want(t, e.do("PATCH", p, adminA, map[string]any{"name": "x"}, nil), 409, "project_archived")
		if g := e.do("GET", p, adminA, nil, nil).json(t); g["status"] != "draft" || g["latest_version"] != float64(1) {
			t.Errorf("agent trong project đã lưu trữ bị thay đổi: %v", g)
		}
		// Vẫn lưu trữ được agent để dọn dẹp.
		want(t, e.do("DELETE", p, adminA, nil, nil), 204, "")
	})

	// Chạy sau cùng: gỡ quyền của dev-a.
	t.Run("idempotent replay re-checks permission", func(t *testing.T) {
		id := create(adminA, projA, "replay-authz")["id"].(string)
		key := map[string]string{"Idempotency-Key": "rp-" + uuid.NewString()}
		v := map[string]any{"system_prompt": "bí mật của version", "config": validConfig()}
		want(t, e.do("POST", "/v1/agents/"+id+"/versions", devA, v, key), 201, "")
		again := e.do("POST", "/v1/agents/"+id+"/versions", devA, v, key)
		if again.status != 201 || again.header.Get("Idempotent-Replayed") != "true" {
			t.Fatalf("phát lại khi còn quyền: %d %v", again.status, again.header)
		}
		if _, err := e.db.Admin.Exec(ctx, `DELETE FROM role_bindings WHERE principal_id = (SELECT id FROM users WHERE idp_subject = 'dev-a')`); err != nil {
			t.Fatal(err)
		}
		r := e.do("POST", "/v1/agents/"+id+"/versions", devA, v, key)
		if r.status != 404 || strings.Contains(string(r.body), "bí mật") {
			t.Errorf("sau khi mất quyền: %d %s", r.status, r.body)
		}
	})
}
