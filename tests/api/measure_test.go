package api_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hqgroups/mcp/internal/adminops"
	"github.com/hqgroups/mcp/internal/ai/provider"
	"github.com/hqgroups/mcp/internal/shared/signedctx"
)

func percentiles(d []time.Duration) (p50, p95, p99 time.Duration) {
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	at := func(q float64) time.Duration { return d[int(float64(len(d)-1)*q)] }
	return at(0.50), at(0.95), at(0.99)
}

// TestMeasure đo các số mà roadmap Phase 1 yêu cầu. Không chạy trong bộ test thường:
//
//	HQ_MEASURE=1 go test ./tests/api -run TestMeasure -v     (hoặc `make measure`)
//
// Mọi thành phần chạy trong một process trên cùng máy với Postgres trong container,
// nên đây là chi phí xử lý của Gateway, không gồm độ trễ mạng giữa các máy.
func TestMeasure(t *testing.T) {
	if os.Getenv("HQ_MEASURE") != "1" {
		t.Skip("đặt HQ_MEASURE=1 để chạy")
	}
	e := setup(t)
	ctx := context.Background()
	adminA := e.tok("admin-a")
	signer := testSigner
	conn, err := e.db.Admin.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	up := newUpstream(t, "docker", readTool("list_containers", "Liệt kê container."))
	if _, err := adminops.RegisterManagedMCPServer(ctx, conn.Conn(), signer, adminops.MCPServer{OrgSlug: "org-a", Slug: "docker"}, "Docker", up.srv.URL); err != nil {
		t.Fatal(err)
	}
	projA := e.createProject(adminA, "measure", "M")["id"].(string)
	// Gỡ giới hạn tần suất của phép đo: nhiều principal để không chạm 120 request/phút.
	const n = 300

	// Gọi thẳng managed server, theo đúng cách Gateway gọi: mỗi lần một phiên MCP mới.
	direct := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		start := time.Now()
		hc := &http.Client{Transport: &signedctx.Transport{Signer: signer, Context: signedctx.Context{Kind: signedctx.KindCall,
			Audience: "docker", OrgID: e.orgA, ProjectID: uuid.MustParse(projA), Initiator: "user:" + uuid.NewString()}}}
		c := mcp.NewClient(&mcp.Implementation{Name: "measure", Version: "1"}, nil)
		s, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: up.srv.URL, HTTPClient: hc, DisableStandaloneSSE: true, MaxRetries: -1}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "list_containers", Arguments: map[string]any{}}); err != nil {
			t.Fatal(err)
		}
		_ = s.Close()
		direct = append(direct, time.Since(start))
	}

	// Qua Gateway: xác thực, phân quyền, resolve, validate, policy, ký ngữ cảnh, upstream, che secret, audit.
	via := make([]time.Duration, 0, n)
	users := []string{"multi-a", "dev-a", "multi"} // admin-a để dành cho execution: mỗi principal 120 request/phút
	sessions := make([]*mcp.ClientSession, len(users))
	for i, u := range users {
		c := mcp.NewClient(&mcp.Implementation{Name: "measure", Version: "1"}, nil)
		tok := e.is.token(t, u, map[string]any{"org_id": e.orgA.String()})
		s, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: e.gw.URL + "/mcp/v1/projects/" + projA,
			HTTPClient: &http.Client{Transport: bearer{tok}}, DisableStandaloneSSE: true, MaxRetries: -1}, nil)
		if err != nil {
			t.Fatalf("connect %s: %v", u, err)
		}
		defer func() { _ = s.Close() }()
		sessions[i] = s
	}
	for i := 0; i < n; i++ {
		start := time.Now()
		res, err := sessions[i%len(sessions)].CallTool(ctx, &mcp.CallToolParams{Name: "docker.list_containers", Arguments: map[string]any{}})
		if err != nil || res.IsError {
			t.Fatalf("call %d: %+v %v", i, res, err)
		}
		via = append(via, time.Since(start))
	}
	d50, d95, d99 := percentiles(direct)
	g50, g95, g99 := percentiles(via)

	// Execution: mỗi execution hai lượt model và một tool call.
	post := func(path string, b any) map[string]any {
		r := e.do("POST", path, adminA, b, nil)
		if r.status != 201 {
			t.Fatalf("POST %s: %d %s", path, r.status, r.body)
		}
		return r.json(t)
	}
	serverID := e.do("GET", "/v1/mcp-servers", adminA, nil, nil).json(t)["data"].([]any)[0].(map[string]any)["id"].(string)
	provID := post("/v1/model-providers", map[string]any{"slug": "local", "adapter": "claude-cli"})["id"].(string)
	modelID := post("/v1/models", map[string]any{"provider_id": provID, "provider_model": "sonnet", "display_name": "S", "tier": "standard",
		"capabilities": map[string]any{"tool_calling": true, "context_window": 200000}})["id"].(string)
	post("/v1/models/"+modelID+"/pricing", map[string]any{"currency": "USD", "input_per_mtok": "3", "output_per_mtok": "15",
		"effective_from": time.Now().UTC().Format(time.RFC3339Nano)})
	agentID := post("/v1/projects/"+projA+"/agents", map[string]any{"slug": "measure", "name": "m", "system_prompt": "Bạn là DevOps Agent chỉ đọc.",
		"config": map[string]any{"model": map[string]any{"policy": "fixed", "fixed_model_id": modelID},
			"limits": map[string]any{"max_iterations": 4, "max_tool_calls": 4, "timeout_seconds": 60},
			"tools":  []any{map[string]any{"server_id": serverID, "allow": []string{"*"}}}}})["id"].(string)
	if r := e.do("PUT", "/v1/agents/"+agentID+"/active-version", adminA, map[string]any{"version": 1}, nil); r.status != 200 {
		t.Fatalf("publish: %d", r.status)
	}
	const runs = 12
	var total time.Duration
	for i := 0; i < runs; i++ {
		e.model.script(func(_ context.Context, turn int, _ provider.Request) (provider.Response, error) {
			if turn == 0 {
				return toolUse("docker.list_containers"), nil
			}
			return answer("Có hai container đang chạy; không có container nào ở trạng thái lỗi."), nil
		})
		start := time.Now()
		r := e.do("POST", "/v1/projects/"+projA+"/executions", adminA, map[string]any{"mode": "single_agent", "agent_id": agentID,
			"input": map[string]any{"message": "Container nào đang lỗi?"}}, map[string]string{"Idempotency-Key": uuid.NewString()})
		if r.status != 202 {
			t.Fatalf("start: %d %s", r.status, r.body)
		}
		id := r.json(t)["id"].(string)
		for {
			if s := e.do("GET", "/v1/executions/"+id, adminA, nil, nil).json(t)["status"]; s != "queued" && s != "running" {
				if s != "completed" {
					t.Fatalf("execution %s: %v", id, s)
				}
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		total += time.Since(start)
	}
	var steps, stepBytes, contentBytes, usageRows, usageBytes, auditRows, auditBytes int
	q := func(dst *int, sql string) {
		if err := e.db.Admin.QueryRow(ctx, sql, e.orgA).Scan(dst); err != nil {
			t.Fatal(err)
		}
	}
	q(&steps, `SELECT count(*) FROM execution_steps WHERE org_id = $1`)
	q(&stepBytes, `SELECT COALESCE(sum(pg_column_size(s.*)), 0)::int FROM execution_steps s WHERE org_id = $1`)
	q(&contentBytes, `SELECT COALESCE(sum(COALESCE(pg_column_size(request), 0) + COALESCE(pg_column_size(response), 0)), 0)::int FROM execution_steps WHERE org_id = $1`)
	q(&usageRows, `SELECT count(*) FROM usage_records WHERE org_id = $1`)
	q(&usageBytes, `SELECT COALESCE(sum(pg_column_size(u.*)), 0)::int FROM usage_records u WHERE org_id = $1`)
	q(&auditRows, `SELECT count(*) FROM audit_logs WHERE org_id = $1 AND (action LIKE 'execution.%' OR details ? 'execution_id')`)
	q(&auditBytes, `SELECT COALESCE(sum(pg_column_size(a.*)), 0)::int FROM audit_logs a WHERE org_id = $1 AND (action LIKE 'execution.%' OR details ? 'execution_id')`)

	fmt.Printf("\n=== Số đo Phase 1 (một process, Postgres trong container, %d lần gọi, %d execution) ===\n", n, runs)
	fmt.Printf("tool call thẳng tới managed server : p50 %v  p95 %v  p99 %v\n", d50.Round(10*time.Microsecond), d95.Round(10*time.Microsecond), d99.Round(10*time.Microsecond))
	fmt.Printf("tool call qua Gateway              : p50 %v  p95 %v  p99 %v\n", g50.Round(10*time.Microsecond), g95.Round(10*time.Microsecond), g99.Round(10*time.Microsecond))
	fmt.Printf("độ trễ thêm của Gateway            : p50 %v  p95 %v  p99 %v\n", (g50 - d50).Round(10*time.Microsecond), (g95 - d95).Round(10*time.Microsecond), (g99 - d99).Round(10*time.Microsecond))
	fmt.Printf("execution (2 lượt model giả, 1 tool): trung bình %v từ lúc gửi tới lúc xong\n", (total / runs).Round(100*time.Microsecond))
	fmt.Printf("step                               : %.1f step/execution, %d byte/step (trong đó nội dung request+response %d byte)\n",
		float64(steps)/runs, stepBytes/max(steps, 1), contentBytes/max(steps, 1))
	fmt.Printf("dữ liệu ghi mỗi execution          : step %d byte, usage %d dòng / %d byte, audit %d dòng / %d byte\n",
		stepBytes/runs, usageRows/runs, usageBytes/runs, auditRows/runs, auditBytes/runs)
}
