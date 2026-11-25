package api_test

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hqgroups/mcp/internal/adminops"
)

// Chính sách project của MCP server (migration 00041): server giới hạn theo project chỉ
// dùng được ở project đã gắn, cả qua Gateway (client bên ngoài, tools/list và tools/call)
// lẫn khi tạo Agent Version. Server không giới hạn giữ hành vi cũ.
func TestMCPServerProjectScope(t *testing.T) {
	if testing.Short() {
		t.Skip("cần Docker")
	}
	e := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	conn, err := e.db.Admin.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	srv := adminops.MCPServer{OrgSlug: "org-a", Slug: "scoped"}
	up := newUpstream(t, "scoped", readTool("list_things", "Liệt kê."))
	if _, err := adminops.RegisterManagedMCPServer(ctx, conn.Conn(), testSigner, srv, "Scoped", up.srv.URL); err != nil {
		t.Fatal(err)
	}
	adminA, adminB := e.tok("admin-a"), e.tok("admin-b")
	ops := e.createProject(adminA, "ops-s", "Ops")["id"].(string)
	sandbox := e.createProject(adminA, "sandbox-s", "Sandbox")["id"].(string)
	e.createProject(adminB, "other-s", "Other")

	listed := func(project string) []string {
		t.Helper()
		s, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint: e.gw.URL + "/mcp/v1/projects/" + project, HTTPClient: &http.Client{Transport: bearer{adminA}},
			DisableStandaloneSSE: true, MaxRetries: -1,
		}, nil)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		defer func() { _ = s.Close() }()
		res, err := s.ListTools(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, tool := range res.Tools {
			names = append(names, tool.Name)
		}
		sort.Strings(names)
		if c, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "scoped.list_things", Arguments: map[string]any{}}); err == nil && !c.IsError {
			names = append(names, "called")
		}
		return names
	}
	if got := strings.Join(listed(sandbox), ","); got != "scoped.list_things,called" {
		t.Fatalf("server chưa giới hạn phải dùng được ở mọi project: %s", got)
	}

	scoped := true
	now, bound, err := adminops.ScopeMCPServer(ctx, conn.Conn(), srv, &scoped, []string{"ops-s"}, nil)
	if err != nil || !now || strings.Join(bound, ",") != "ops-s" {
		t.Fatalf("scope: %v %v %v", now, bound, err)
	}
	calls := len(up.calls)
	if got := strings.Join(listed(sandbox), ","); got != "" {
		t.Errorf("project chưa gắn vẫn thấy hoặc gọi được tool: %s", got)
	}
	if len(up.calls) != calls {
		t.Errorf("project chưa gắn vẫn tạo request tới server")
	}
	if got := strings.Join(listed(ops), ","); got != "scoped.list_things,called" {
		t.Errorf("project đã gắn: %s", got)
	}

	// Agent Version tham chiếu server chỉ tạo được ở project đã gắn.
	serverID := ""
	for _, s := range e.do("GET", "/v1/mcp-servers?limit=200", adminA, nil, nil).json(t)["data"].([]any) {
		if m := s.(map[string]any); m["slug"] == "scoped" {
			serverID = m["id"].(string)
		}
	}
	agent := func(project, slug string) resp {
		return e.do("POST", "/v1/projects/"+project+"/agents", adminA, map[string]any{"slug": slug, "name": slug, "system_prompt": "x",
			"config": map[string]any{"model": map[string]any{"policy": "tiered"}, "limits": map[string]any{"max_iterations": 3, "max_tool_calls": 3, "timeout_seconds": 30},
				"tools": []any{map[string]any{"server_id": serverID, "allow": []string{"*"}}}}}, nil)
	}
	if r := agent(sandbox, "in-sandbox"); r.status != 422 || !strings.Contains(string(r.body), "server không tồn tại") {
		t.Errorf("Agent ở project chưa gắn: %d %s", r.status, r.body)
	}
	if r := agent(ops, "in-ops"); strings.Contains(string(r.body), "server không tồn tại") {
		t.Errorf("Agent ở project đã gắn bị từ chối vì server: %d %s", r.status, r.body)
	}

	// Không gắn được project của org khác, không giới hạn được server của platform.
	if _, _, err := adminops.ScopeMCPServer(ctx, conn.Conn(), srv, nil, []string{"other-s"}, nil); err == nil {
		t.Error("gắn được project của org khác")
	}
	if _, _, err := adminops.ScopeMCPServer(ctx, conn.Conn(), adminops.MCPServer{Slug: "scoped"}, &scoped, nil, nil); err == nil {
		t.Error("giới hạn được server không thuộc org")
	}
	// Bỏ giới hạn: trở lại hành vi cũ, và mọi thay đổi có audit.
	off := false
	if now, _, err := adminops.ScopeMCPServer(ctx, conn.Conn(), srv, &off, nil, []string{"ops-s"}); err != nil || now {
		t.Fatalf("bỏ giới hạn: %v %v", now, err)
	}
	if got := strings.Join(listed(sandbox), ","); got != "scoped.list_things,called" {
		t.Errorf("sau khi bỏ giới hạn: %s", got)
	}
	var n int
	if err := e.db.Admin.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE org_id = $1 AND action = 'mcp_server.project_scope'`, e.orgA).Scan(&n); err != nil || n != 2 {
		t.Errorf("audit mcp_server.project_scope = %d (%v), muốn 2", n, err)
	}
}
