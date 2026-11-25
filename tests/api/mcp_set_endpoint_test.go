package api_test

import (
	"context"
	"strings"
	"testing"

	"github.com/hqgroups/mcp/internal/adminops"
)

// TestSetManagedEndpoint: đổi địa chỉ của một managed MCP server (ví dụ chuyển từ chạy
// trực tiếp sang chạy trong container) không phải đường tắt để thay tool: địa chỉ mới
// phải trả lời discovery, và định nghĩa khác đi thì vào hàng chờ duyệt như mọi lần discovery.
func TestSetManagedEndpoint(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	signer := testSigner
	conn, err := e.db.Admin.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	srv := adminops.MCPServer{OrgSlug: "org-a", Slug: "ops"}
	first := newUpstream(t, "ops", readTool("list_things", "Liệt kê."))
	same := newUpstream(t, "ops", readTool("list_things", "Liệt kê."))
	changed := newUpstream(t, "ops", readTool("list_things", "Liệt kê MỌI THỨ và làm thêm việc khác."))
	if _, err := adminops.RegisterManagedMCPServer(ctx, conn.Conn(), signer, srv, "Ops", first.srv.URL); err != nil {
		t.Fatal(err)
	}
	endpoint := func() string {
		var s string
		if err := e.db.Admin.QueryRow(ctx, `SELECT endpoint FROM mcp_servers WHERE slug = 'ops' AND org_id = $1`, e.orgA).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}

	// Địa chỉ không hợp lệ hoặc không trả lời discovery: không ghi gì.
	for _, bad := range []string{"", "ftp://x/mcp", "http://user:pw@127.0.0.1:1/mcp", same.srv.URL + "?k=v", "http://127.0.0.1:1/mcp"} {
		if _, err := adminops.SetManagedMCPServerEndpoint(ctx, conn.Conn(), signer, srv, bad, true); err == nil {
			t.Errorf("địa chỉ %q được chấp nhận", bad)
		}
	}
	if endpoint() != first.srv.URL {
		t.Fatalf("địa chỉ bị đổi bởi một lời gọi lỗi: %s", endpoint())
	}
	// Server không tồn tại, hoặc của org khác.
	if _, err := adminops.SetManagedMCPServerEndpoint(ctx, conn.Conn(), signer, adminops.MCPServer{OrgSlug: "org-b", Slug: "ops"}, same.srv.URL, true); err == nil {
		t.Error("đổi được địa chỉ của server không thuộc org-b")
	}

	// http chỉ được nhận khi người gọi cho phép (local); server phạm vi platform bị từ chối.
	if _, err := adminops.SetManagedMCPServerEndpoint(ctx, conn.Conn(), signer, srv, same.srv.URL, false); err == nil || !strings.Contains(err.Error(), "https") {
		t.Errorf("http được nhận khi không cho phép: %v", err)
	}
	if _, err := adminops.SetManagedMCPServerEndpoint(ctx, conn.Conn(), signer, adminops.MCPServer{Slug: "ops"}, same.srv.URL, true); err == nil {
		t.Error("đổi được địa chỉ ở phạm vi platform (không có audit)")
	}
	// Database chỉ cho đổi endpoint của managed server, với mọi role (migration 00038).
	var extID string
	if err := e.db.Admin.QueryRow(ctx, `INSERT INTO mcp_servers (org_id, slug, name, kind, transport, endpoint, auth_method, status)
		VALUES ($1, 'ext', 'Ext', 'external', 'streamable_http', 'https://mcp.example.test/mcp', 'none', 'registered') RETURNING id`, e.orgA).Scan(&extID); err != nil {
		t.Fatalf("seed external server: %v", err)
	}
	if _, err := e.db.Admin.Exec(ctx, `UPDATE mcp_servers SET endpoint = 'https://evil.example.test/mcp' WHERE id = $1`, extID); err == nil {
		t.Error("endpoint của server external đổi được")
	}

	// Cùng định nghĩa ở địa chỉ mới: chỉ địa chỉ đổi, có audit.
	res, err := adminops.SetManagedMCPServerEndpoint(ctx, conn.Conn(), signer, srv, same.srv.URL, true)
	if err != nil || res.Changed {
		t.Fatalf("đổi sang server cùng định nghĩa: %+v, %v", res, err)
	}
	if endpoint() != same.srv.URL {
		t.Fatalf("địa chỉ = %s", endpoint())
	}
	var details string
	if err := e.db.Admin.QueryRow(ctx, `SELECT details::text FROM audit_logs WHERE org_id = $1 AND action = 'mcp_server.set_endpoint'`, e.orgA).Scan(&details); err != nil ||
		!strings.Contains(details, first.srv.URL) || !strings.Contains(details, same.srv.URL) {
		t.Fatalf("audit: %s (%v)", details, err)
	}
	tools := e.do("GET", "/v1/mcp-tools", e.tok("admin-a"), nil, nil).json(t)["data"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["enabled"] != true {
		t.Fatalf("tool sau khi đổi địa chỉ: %v", tools)
	}

	// Định nghĩa khác ở địa chỉ mới: version chờ duyệt, tool bị khoá.
	res, err = adminops.SetManagedMCPServerEndpoint(ctx, conn.Conn(), signer, srv, changed.srv.URL, true)
	if err != nil || !res.Changed || len(res.LockedTools) != 1 {
		t.Fatalf("đổi sang server khác định nghĩa: %+v, %v", res, err)
	}
	tools = e.do("GET", "/v1/mcp-tools", e.tok("admin-a"), nil, nil).json(t)["data"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["enabled"] == true {
		t.Fatalf("tool đổi định nghĩa vẫn bật: %v", tools)
	}
}
