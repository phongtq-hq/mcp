package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hqgroups/mcp/internal/adminops"
	"github.com/hqgroups/mcp/internal/gatewayserver"
	"github.com/hqgroups/mcp/internal/mcp/registry"
	"github.com/hqgroups/mcp/internal/platform/identity"
	"github.com/hqgroups/mcp/internal/shared/signedctx"
)

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if b.token != "" {
		r.Header.Set("Authorization", "Bearer "+b.token)
	}
	return http.DefaultTransport.RoundTrip(r)
}

func TestMCPGateway(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	signer, _ := signedctx.NewSigner(signingKey)
	verifier, err := identity.NewOIDCVerifier(ctx, e.is.srv.URL, audience, "org_id")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	gw := httptest.NewUnstartedServer(mux)
	gw.Start()
	t.Cleanup(gw.Close)
	if err := gatewayserver.Register(mux, e.db.PoolAs(t, "app_gateway", 4), verifier, signer,
		gatewayserver.Config{PublicURL: gw.URL, Issuer: e.is.srv.URL}); err != nil {
		t.Fatal(err)
	}

	conn, err := e.db.Admin.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	up := newUpstream(t, "docker",
		readTool("list_containers", "Liệt kê container."),
		readTool("get_container_logs", "Đọc log."),
		writeTool("restart_container", "Khởi động lại container."))
	srvA := adminops.MCPServer{OrgSlug: "org-a", Slug: "docker"}
	if _, err := adminops.RegisterManagedMCPServer(ctx, conn.Conn(), signer, srvA, "Docker", up.srv.URL); err != nil {
		t.Fatal(err)
	}

	adminA, adminB := e.tok("admin-a"), e.tok("admin-b")
	projA := e.createProject(adminA, "gw-a", "GW A")["id"].(string)
	projB := e.createProject(adminB, "gw-b", "GW B")["id"].(string)

	connect := func(t *testing.T, tok, project string) (*mcp.ClientSession, error) {
		c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
		s, err := c.Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint: gw.URL + "/mcp/v1/projects/" + project, HTTPClient: &http.Client{Transport: bearer{tok}},
			DisableStandaloneSSE: true, MaxRetries: -1,
		}, nil)
		if err == nil {
			t.Cleanup(func() { _ = s.Close() })
		}
		return s, err
	}
	status := func(tok, project string) int {
		req, _ := http.NewRequest("POST", gw.URL+"/mcp/v1/projects/"+project,
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		res, err := (&http.Client{Transport: bearer{tok}}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		if res.StatusCode == 401 && !strings.Contains(res.Header.Get("WWW-Authenticate"), "resource_metadata") {
			t.Error("401 thiếu WWW-Authenticate trỏ tới metadata")
		}
		return res.StatusCode
	}
	text := func(r *mcp.CallToolResult) string {
		if len(r.Content) != 1 {
			return ""
		}
		tc, _ := r.Content[0].(*mcp.TextContent)
		return tc.Text
	}
	audits := func(rule string) int {
		var n int
		if err := e.db.Admin.QueryRow(ctx,
			`SELECT count(*) FROM audit_logs WHERE org_id = $1 AND action = 'mcp_tool.call' AND details->>'rule' = $2`,
			e.orgA, rule).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	upstreamCalls := func() int {
		up.mu.Lock()
		defer up.mu.Unlock()
		return len(up.calls)
	}

	sess, err := connect(t, adminA, projA)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	t.Run("lists only enabled tools, namespaced by server", func(t *testing.T) {
		res, err := sess.ListTools(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, tool := range res.Tools {
			names = append(names, tool.Name)
		}
		sort.Strings(names)
		if got := strings.Join(names, ","); got != "docker.get_container_logs,docker.list_containers" {
			t.Errorf("tools = %s", got)
		}
	})

	t.Run("call goes upstream with signed context and is audited", func(t *testing.T) {
		res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "docker.list_containers", Arguments: map[string]any{"name": "web"}})
		if err != nil || res.IsError || text(res) != "result of list_containers" {
			t.Fatalf("call: %+v %v", res, err)
		}
		if upstreamCalls() != 1 || audits("risk_read") != 1 {
			t.Errorf("upstream calls = %d, audit = %d", upstreamCalls(), audits("risk_read"))
		}
		var details string
		_ = e.db.Admin.QueryRow(ctx, `SELECT details::text FROM audit_logs WHERE org_id = $1 AND action = 'mcp_tool.call'`, e.orgA).Scan(&details)
		if strings.Contains(details, "web") || !strings.Contains(details, "arguments_sha256") {
			t.Errorf("audit phải chứa hash, không chứa arguments: %s", details)
		}
	})

	t.Run("arguments are validated against the pinned schema", func(t *testing.T) {
		before := upstreamCalls()
		for _, args := range []map[string]any{{"name": 5}, {"extra": "x"}} {
			res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "docker.list_containers", Arguments: args})
			if err != nil || !res.IsError {
				t.Errorf("args %v: muốn lỗi tool, có %+v %v", args, res, err)
			}
		}
		if upstreamCalls() != before {
			t.Error("arguments sai vẫn tới upstream")
		}
		if audits("invalid_arguments") != 2 {
			t.Errorf("audit invalid_arguments = %d", audits("invalid_arguments"))
		}
	})

	t.Run("calls to unavailable tools are audited", func(t *testing.T) {
		before := audits("tool_not_available")
		_, _ = sess.CallTool(ctx, &mcp.CallToolParams{Name: "docker.restart_container", Arguments: map[string]any{}})
		if got := audits("tool_not_available"); got != before+1 {
			t.Errorf("audit tool_not_available: %d → %d", before, got)
		}
	})

	t.Run("disabled and unknown tools do not exist", func(t *testing.T) {
		for _, name := range []string{"docker.restart_container", "list_containers", "docker.nope"} {
			res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: map[string]any{}})
			if err == nil && !res.IsError {
				t.Errorf("%s: gọi được tool không tồn tại", name)
			}
		}
	})

	t.Run("non-read tool is denied to external clients even when enabled", func(t *testing.T) {
		on := true
		risk := "write"
		if err := adminops.ClassifyMCPTool(ctx, conn.Conn(), srvA, "restart_container",
			registry.Classification{Enabled: &on, RiskLevel: &risk}); err != nil {
			t.Fatal(err)
		}
		before := upstreamCalls()
		res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "docker.restart_container", Arguments: map[string]any{"name": "web"}})
		if err != nil || !res.IsError {
			t.Fatalf("muốn từ chối: %+v %v", res, err)
		}
		// Client MCP bên ngoài không có execution để gắn approval hay chống chạy trùng:
		// tool không chỉ đọc bị từ chối ở đường này (quy tắc đổi tên từ Phase 2).
		if upstreamCalls() != before || audits("risk_write_requires_execution") != 1 {
			t.Errorf("tool ghi: upstream %d→%d, audit %d", before, upstreamCalls(), audits("risk_write_requires_execution"))
		}
	})

	t.Run("results are redacted and truncated", func(t *testing.T) {
		up.mu.Lock()
		up.reply = "DATABASE_URL=postgres://app:hunter2secret@db:5432/x\nkey sk-ant-abcdefghijklmnopqrstuvwxyz012345\n" + strings.Repeat("x", 2<<20)
		up.mu.Unlock()
		defer func() { up.mu.Lock(); up.reply = ""; up.mu.Unlock() }()
		res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "docker.get_container_logs", Arguments: map[string]any{}})
		if err != nil || res.IsError {
			t.Fatalf("call: %+v %v", res, err)
		}
		out := text(res)
		if strings.Contains(out, "hunter2secret") || strings.Contains(out, "sk-ant-abc") {
			t.Error("secret lọt qua bộ lọc")
		}
		if !strings.Contains(out, "bị cắt") || len(out) > 1<<20+200 {
			t.Errorf("kết quả không bị cắt: %d byte", len(out))
		}
	})

	t.Run("access control", func(t *testing.T) {
		for name, c := range map[string]struct {
			tok, project string
			want         int
		}{
			"unauthenticated":              {"", projA, 401},
			"other tenant's project":       {adminB, projA, 404},
			"no role":                      {e.tok("norole-a"), projA, 404},
			"viewer cannot call tools":     {e.tok("viewer-a"), projA, 403},
			"unknown user not provisioned": {e.tok("never-logged-in"), projA, 403},
			"bad project id":               {adminA, "nope", 404},
		} {
			if got := status(c.tok, c.project); got != c.want {
				t.Errorf("%s: status %d, muốn %d", name, got, c.want)
			}
		}
		var n int
		_ = e.db.Admin.QueryRow(ctx, `SELECT count(*) FROM users WHERE idp_subject = 'never-logged-in'`).Scan(&n)
		if n != 0 {
			t.Error("gateway đã tạo user")
		}
		// Org B không thấy tool của org A.
		sb, err := connect(t, adminB, projB)
		if err != nil {
			t.Fatal(err)
		}
		if res, err := sb.ListTools(ctx, nil); err != nil || len(res.Tools) != 0 {
			t.Errorf("org B thấy tool của org A: %+v %v", res, err)
		}
	})

	t.Run("archived project", func(t *testing.T) {
		want(t, e.do("DELETE", "/v1/projects/"+projA, adminA, nil, nil), 204, "")
		if got := status(adminA, projA); got != 409 {
			t.Errorf("project đã lưu trữ: status %d", got)
		}
	})
}
