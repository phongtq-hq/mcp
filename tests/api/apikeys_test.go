package api_test

import (
	"context"
	"crypto/sha256"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/hqgroups/mcp/internal/adminops"
	"github.com/hqgroups/mcp/internal/ai/provider"
	"github.com/hqgroups/mcp/internal/shared/signedctx"
)

func TestPlatformAPIKeys(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	adminA, adminB, devA := e.tok("admin-a"), e.tok("admin-b"), e.tok("dev-a")
	projA := e.createProject(adminA, "keys-p1", "P1")["id"].(string)
	projA2 := e.createProject(adminA, "keys-p2", "P2")["id"].(string)
	projB := e.createProject(adminB, "keys-b", "B")["id"].(string)
	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := e.db.Admin.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	create := func(tok string, body map[string]any) resp { return e.do("POST", "/v1/api-keys", tok, body, nil) }
	mustCreate := func(body map[string]any) (id, key string) {
		t.Helper()
		r := create(adminA, body)
		if r.status != 201 {
			t.Fatalf("create key: %d %s", r.status, r.body)
		}
		j := r.json(t)
		return j["id"].(string), j["key"].(string)
	}

	r := create(adminA, map[string]any{"name": "ci", "role": "developer"})
	want(t, r, 201, "")
	created := r.json(t)
	keyID, key := created["id"].(string), created["key"].(string)

	t.Run("the key is shown once and only its hash is stored", func(t *testing.T) {
		if !strings.HasPrefix(key, "hqk_") || len(key) < 50 || created["role"] != "developer" || created["project_id"] != nil ||
			!strings.HasPrefix(key, created["prefix"].(string)) || r.header.Get("Cache-Control") != "no-store" {
			t.Errorf("key mới: %v", created)
		}
		list := e.do("GET", "/v1/api-keys", adminA, nil, nil)
		want(t, list, 200, "")
		if strings.Contains(string(list.body), key) || strings.Contains(string(list.body), `"key"`) || len(list.json(t)["data"].([]any)) != 1 {
			t.Errorf("danh sách: %s", list.body)
		}
		secret := key[strings.LastIndex(key, "_")+1:]
		for table, sql := range map[string]string{
			"api_keys":   `SELECT count(*) FROM api_keys WHERE to_jsonb(api_keys)::text LIKE '%' || $1 || '%'`,
			"audit_logs": `SELECT count(*) FROM audit_logs WHERE details::text LIKE '%' || $1 || '%'`,
		} {
			if n := count(sql, secret); n != 0 {
				t.Errorf("bí mật của khoá nằm trong %s", table)
			}
		}
		sum := sha256.Sum256([]byte(key))
		if n := count(`SELECT count(*) FROM api_keys WHERE id = $1 AND key_hash = $2`, keyID, sum[:]); n != 1 {
			t.Error("key_hash không phải SHA-256 của khoá")
		}
		if n := count(`SELECT count(*) FROM audit_logs WHERE org_id = $1 AND action = 'api_key.create' AND resource_id = $2`, e.orgA, keyID); n != 1 {
			t.Errorf("audit api_key.create: %d", n)
		}
	})

	t.Run("the key authenticates as its role, inside its tenant", func(t *testing.T) {
		want(t, e.do("GET", "/v1/projects/"+projA, key, nil, nil), 200, "")
		want(t, e.do("GET", "/v1/organization", key, nil, nil), 200, "")
		want(t, e.do("GET", "/v1/projects/"+projB, key, nil, nil), 404, "not_found")
		// developer không có quyền quản lý khoá hay sửa organization.
		want(t, e.do("GET", "/v1/api-keys", key, nil, nil), 403, "permission_denied")
		want(t, create(key, map[string]any{"name": "child", "role": "viewer"}), 403, "permission_denied")
		want(t, e.do("PATCH", "/v1/organization", key, map[string]any{"name": "x"}, nil), 403, "permission_denied")
		if n := count(`SELECT count(*) FROM api_keys WHERE id = $1 AND last_used_at IS NOT NULL`, keyID); n != 1 {
			t.Error("last_used_at không được ghi")
		}
	})

	t.Run("bad, tampered and foreign credentials are rejected alike", func(t *testing.T) {
		tampered := key[:len(key)-3] + "AAA"
		otherPrefix := "hqk_" + strings.Repeat("a", 12) + key[16:]
		for name, tok := range map[string]string{
			"tampered secret": tampered, "unknown prefix": otherPrefix, "prefix only": key[:16], "bare prefix": "hqk_",
			"too long": key + strings.Repeat("x", 200), "oidc-looking": "hqk_eyJhbGciOiJSUzI1NiJ9.e30.x",
		} {
			want(t, e.do("GET", "/v1/projects/"+projA, tok, nil, nil), 401, "unauthenticated")
			_ = name
		}
	})

	t.Run("a key never carries more than its creator has", func(t *testing.T) {
		// Developer không có api_key:create.
		want(t, create(devA, map[string]any{"name": "x", "role": "viewer"}), 403, "permission_denied")
		// Role chứa quyền quản lý khoá hoặc credential không gán được cho khoá.
		want(t, create(adminA, map[string]any{"name": "x", "role": "org_admin"}), 422, "validation_failed")
		for _, body := range []map[string]any{
			{"name": "x", "role": "nope"}, {"name": "", "role": "viewer"}, {"name": "x", "role": "viewer", "expires_in_days": 0},
			{"name": "x", "role": "viewer", "expires_in_days": 1000}, {"name": "x", "role": "viewer", "project_id": projB},
			{"name": "x", "role": "viewer", "project_id": uuid.NewString()},
		} {
			want(t, create(adminA, body), 422, "validation_failed")
		}
		// Endpoint trả secret không phát lại theo Idempotency-Key: hai lần gọi là hai khoá.
		h := map[string]string{"Idempotency-Key": uuid.NewString()}
		a := e.do("POST", "/v1/api-keys", adminA, map[string]any{"name": "twice", "role": "viewer"}, h).json(t)
		b := e.do("POST", "/v1/api-keys", adminA, map[string]any{"name": "twice", "role": "viewer"}, h).json(t)
		if a["key"] == b["key"] || a["id"] == b["id"] {
			t.Error("khoá bị phát lại theo Idempotency-Key")
		}
	})

	t.Run("a project-scoped key sees only its project", func(t *testing.T) {
		_, scoped := mustCreate(map[string]any{"name": "p1 only", "role": "viewer", "project_id": projA})
		want(t, e.do("GET", "/v1/projects/"+projA, scoped, nil, nil), 200, "")
		want(t, e.do("GET", "/v1/projects/"+projA2, scoped, nil, nil), 404, "not_found")
		list := e.do("GET", "/v1/projects", scoped, nil, nil)
		want(t, list, 200, "")
		if data := list.json(t)["data"].([]any); len(data) != 1 || data[0].(map[string]any)["id"] != projA {
			t.Errorf("khoá phạm vi project thấy: %s", list.body)
		}
		want(t, e.do("POST", "/v1/projects", scoped, map[string]any{"slug": "nope", "name": "n", "environment": "non_prod"}, nil), 403, "permission_denied")
	})

	t.Run("tenant isolation of key management", func(t *testing.T) {
		if data := e.do("GET", "/v1/api-keys", adminB, nil, nil).json(t)["data"].([]any); len(data) != 0 {
			t.Errorf("org B thấy khoá của org A: %v", data)
		}
		want(t, e.do("DELETE", "/v1/api-keys/"+keyID, adminB, nil, nil), 404, "not_found")
		want(t, e.do("DELETE", "/v1/api-keys/"+keyID, devA, nil, nil), 404, "not_found")
		want(t, e.do("DELETE", "/v1/api-keys/"+uuid.NewString(), adminA, nil, nil), 404, "not_found")
	})

	t.Run("a key works on the gateway and can start an execution", func(t *testing.T) {
		signer, _ := signedctx.NewSigner(signingKey)
		conn, err := e.db.Admin.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		up := newUpstream(t, "docker", readTool("list_containers", "Liệt kê container."))
		if _, err := adminops.RegisterManagedMCPServer(ctx, conn.Conn(), signer, adminops.MCPServer{OrgSlug: "org-a", Slug: "docker"}, "Docker", up.srv.URL); err != nil {
			t.Fatal(err)
		}
		// Client ngoài dùng khoá để gọi Gateway.
		req, _ := http.NewRequest("POST", e.gw.URL+"/mcp/v1/projects/"+projA, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Authorization", "Bearer "+key)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != 200 {
			t.Errorf("gateway với API key: %d", res.StatusCode)
		}

		post := func(path string, body any) map[string]any {
			t.Helper()
			r := e.do("POST", path, adminA, body, nil)
			if r.status != 201 {
				t.Fatalf("POST %s: %d %s", path, r.status, r.body)
			}
			return r.json(t)
		}
		serverID := e.do("GET", "/v1/mcp-servers", adminA, nil, nil).json(t)["data"].([]any)[0].(map[string]any)["id"].(string)
		provID := post("/v1/model-providers", map[string]any{"slug": "local", "adapter": "claude-cli"})["id"].(string)
		modelID := post("/v1/models", map[string]any{"provider_id": provID, "provider_model": "sonnet", "display_name": "S", "tier": "standard",
			"capabilities": map[string]any{"tool_calling": true, "context_window": 200000}})["id"].(string)
		post("/v1/models/"+modelID+"/pricing", map[string]any{"currency": "USD", "input_per_mtok": "1", "output_per_mtok": "1",
			"effective_from": time.Now().UTC().Format(time.RFC3339Nano)})
		agentID := post("/v1/projects/"+projA+"/agents", map[string]any{"slug": "ops", "name": "ops", "system_prompt": "x",
			"config": map[string]any{"model": map[string]any{"policy": "fixed", "fixed_model_id": modelID},
				"limits": map[string]any{"max_iterations": 3, "max_tool_calls": 3, "timeout_seconds": 30},
				"tools":  []any{map[string]any{"server_id": serverID, "allow": []string{"*"}}}}})["id"].(string)
		if r := e.do("PUT", "/v1/agents/"+agentID+"/active-version", adminA, map[string]any{"version": 1}, nil); r.status != 200 {
			t.Fatalf("publish: %d %s", r.status, r.body)
		}
		e.model.script(func(_ context.Context, n int, _ provider.Request) (provider.Response, error) {
			if n == 0 {
				return toolUse("docker.list_containers"), nil
			}
			return answer("xong"), nil
		})
		start := e.do("POST", "/v1/projects/"+projA+"/executions", key, map[string]any{"mode": "single_agent", "agent_id": agentID,
			"input": map[string]any{"message": "kiểm tra"}}, map[string]string{"Idempotency-Key": uuid.NewString()})
		want(t, start, 202, "")
		x := start.json(t)
		id := x["id"].(string)
		if x["initiator"].(map[string]any)["type"] != "api_key" || x["initiator"].(map[string]any)["id"] != keyID {
			t.Errorf("initiator: %v", x["initiator"])
		}
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if x = e.do("GET", "/v1/executions/"+id, key, nil, nil).json(t); x["status"] != "queued" && x["status"] != "running" {
				break
			}
		}
		up.mu.Lock()
		calls := len(up.calls)
		up.mu.Unlock()
		if x["status"] != "completed" || calls != 1 {
			t.Errorf("execution do khoá khởi tạo: %v, %d tool call tới upstream", x["status"], calls)
		}
		if n := count(`SELECT count(*) FROM audit_logs WHERE org_id = $1 AND action IN ('execution.create','mcp_tool.call')
			AND actor_type = 'api_key' AND actor_id = $2 AND (resource_id = $3 OR details->>'execution_id' = $3::text)`, e.orgA, keyID, id); n != 2 {
			t.Errorf("audit mang actor api_key: %d dòng, muốn 2", n)
		}
	})

	t.Run("revoked, expired and suspended keys stop working at once", func(t *testing.T) {
		want(t, e.do("DELETE", "/v1/api-keys/"+keyID, adminA, nil, nil), 204, "")
		want(t, e.do("DELETE", "/v1/api-keys/"+keyID, adminA, nil, nil), 204, "") // lặp lại là no-op
		want(t, e.do("GET", "/v1/projects/"+projA, key, nil, nil), 401, "unauthenticated")
		if n := count(`SELECT count(*) FROM audit_logs WHERE org_id = $1 AND action = 'api_key.revoke' AND resource_id = $2`, e.orgA, keyID); n != 1 {
			t.Errorf("audit api_key.revoke: %d", n)
		}
		g := e.do("GET", "/v1/api-keys", adminA, nil, nil)
		if !strings.Contains(string(g.body), `"revoked_at":"`) {
			t.Error("danh sách không cho thấy khoá đã thu hồi")
		}

		// Khoá hết hạn: chèn thẳng một khoá có hạn trong quá khứ.
		expired := "hqk_" + strings.Repeat("b", 12) + "_" + strings.Repeat("Z", 43)
		sum := sha256.Sum256([]byte(expired))
		var creator uuid.UUID
		_ = e.db.Admin.QueryRow(ctx, `SELECT id FROM users WHERE idp_subject = 'admin-a'`).Scan(&creator)
		if _, err := e.db.Admin.Exec(ctx, `INSERT INTO api_keys (org_id, name, key_prefix, key_hash, role_name, created_by, expires_at)
			VALUES ($1, 'old', $2, $3, 'viewer', $4, now() - interval '1 second')`, e.orgA, strings.Repeat("b", 12), sum[:], creator); err != nil {
			t.Fatal(err)
		}
		want(t, e.do("GET", "/v1/projects/"+projA, expired, nil, nil), 401, "unauthenticated")

		// Khoá không sống lâu hơn người tạo: người tạo bị vô hiệu hoá thì khoá ngừng hoạt động.
		_, orphan := mustCreate(map[string]any{"name": "orphan", "role": "viewer"})
		want(t, e.do("GET", "/v1/projects/"+projA, orphan, nil, nil), 200, "")
		if _, err := e.db.Admin.Exec(ctx, `UPDATE users SET status = 'disabled' WHERE idp_subject = 'admin-a'`); err != nil {
			t.Fatal(err)
		}
		want(t, e.do("GET", "/v1/projects/"+projA, orphan, nil, nil), 401, "unauthenticated")
		_, _ = e.db.Admin.Exec(ctx, `UPDATE users SET status = 'active' WHERE idp_subject = 'admin-a'`)
		want(t, e.do("GET", "/v1/projects/"+projA, orphan, nil, nil), 200, "")

		// project_id bịa ra không tạo thêm dòng audit từ chối cho mỗi giá trị.
		denies := count(`SELECT count(*) FROM audit_logs WHERE org_id = $1 AND action = 'authz.deny'`, e.orgA)
		for i := 0; i < 5; i++ {
			want(t, create(devA, map[string]any{"name": "x", "role": "viewer", "project_id": uuid.NewString()}), 422, "validation_failed")
		}
		if n := count(`SELECT count(*) FROM audit_logs WHERE org_id = $1 AND action = 'authz.deny'`, e.orgA); n != denies {
			t.Errorf("project_id bịa ra tạo thêm %d dòng audit từ chối", n-denies)
		}

		_, live := mustCreate(map[string]any{"name": "live", "role": "viewer"})
		want(t, e.do("GET", "/v1/projects/"+projA, live, nil, nil), 200, "")
		if _, err := e.db.Admin.Exec(ctx, `UPDATE organizations SET status = 'suspended' WHERE id = $1`, e.orgA); err != nil {
			t.Fatal(err)
		}
		want(t, e.do("GET", "/v1/projects/"+projA, live, nil, nil), 401, "unauthenticated")
		_, _ = e.db.Admin.Exec(ctx, `UPDATE organizations SET status = 'active' WHERE id = $1`, e.orgA)
	})
}
