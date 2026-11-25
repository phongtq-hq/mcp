package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/hqgroups/mcp/internal/adminops"
	"github.com/hqgroups/mcp/internal/integration/prometheus"
)

// prometheusImage: bản Prometheus dùng cho test đầu-cuối. Đổi bằng HQ_TEST_PROMETHEUS_IMAGE.
const prometheusImage = "prom/prometheus:v3.0.0"

// TestPrometheusEndToEnd chạy integration Prometheus với một Prometheus THẬT (container),
// qua đúng đường của client bên ngoài: MCP client → Gateway (xác thực, schema đã ghim,
// policy, ngữ cảnh ký, audit) → managed server → HTTP API của Prometheus.
func TestPrometheusEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("cần Docker")
	}
	e := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	image := prometheusImage
	if v := os.Getenv("HQ_TEST_PROMETHEUS_IMAGE"); v != "" {
		image = v
	}
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        image,
			ExposedPorts: []string{"9090/tcp"},
			WaitingFor:   wait.ForHTTP("/-/ready").WithPort("9090/tcp").WithStartupTimeout(90 * time.Second),
		},
		Started: true,
	})
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("start prometheus: %v", err)
	}
	endpoint, err := ctr.PortEndpoint(ctx, "9090/tcp", "http")
	if err != nil {
		t.Fatal(err)
	}

	signer := testSigner
	client, err := prometheus.NewClient(endpoint, prometheus.Options{})
	if err != nil {
		t.Fatal(err)
	}
	up := httptest.NewServer(prometheus.Handler(client, signer.Verifier(), e.orgA))
	t.Cleanup(up.Close)
	conn, err := e.db.Admin.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Đăng ký như người vận hành: discovery thật qua MCP, ghim hash định nghĩa từng tool.
	if _, err := adminops.RegisterManagedMCPServer(ctx, conn.Conn(), signer, adminops.MCPServer{OrgSlug: "org-a", Slug: prometheus.Slug}, "Prometheus", up.URL); err != nil {
		conn.Release()
		t.Fatalf("đăng ký server: %v", err)
	}
	conn.Release()

	adminA, adminB := e.tok("admin-a"), e.tok("admin-b")
	projA := e.createProject(adminA, "prom-a", "Prom A")["id"].(string)
	projB := e.createProject(adminB, "prom-b", "Prom B")["id"].(string)
	connect := func(tok, project string) *mcp.ClientSession {
		t.Helper()
		c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
		s, err := c.Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint: e.gw.URL + "/mcp/v1/projects/" + project, HTTPClient: &http.Client{Transport: bearer{tok}},
			DisableStandaloneSSE: true, MaxRetries: -1,
		}, nil)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	}
	text := func(r *mcp.CallToolResult) string {
		var b strings.Builder
		for _, c := range r.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				b.WriteString(tc.Text)
			}
		}
		return b.String()
	}
	sess := connect(adminA, projA)

	t.Run("discovery through the gateway lists the five read tools", func(t *testing.T) {
		res, err := sess.ListTools(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, tool := range res.Tools {
			names = append(names, tool.Name)
		}
		sort.Strings(names)
		wantNames := "prometheus.list_alerts,prometheus.list_metric_names,prometheus.list_targets,prometheus.query_instant,prometheus.query_range"
		if got := strings.Join(names, ","); got != wantNames {
			t.Fatalf("tools = %s", got)
		}
		// Org khác không thấy server của org A.
		other, err := connect(adminB, projB).ListTools(ctx, nil)
		if err != nil || len(other.Tools) != 0 {
			t.Fatalf("org B thấy %d tool (%v)", len(other.Tools), err)
		}
	})

	t.Run("queries reach the real Prometheus and come back structured", func(t *testing.T) {
		// Prometheus tự scrape chính nó; chờ lần scrape đầu, có trần.
		var out string
		for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
			res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "prometheus.query_instant", Arguments: map[string]any{"query": "up"}})
			if err != nil {
				t.Fatalf("query_instant: %v", err)
			}
			if out = text(res); !res.IsError && strings.Contains(out, `"job":"prometheus"`) {
				break
			}
		}
		if !strings.Contains(out, `"job":"prometheus"`) {
			t.Fatalf("query_instant(up) không trả series của job prometheus: %s", out)
		}
		now := time.Now().UTC()
		for name, args := range map[string]map[string]any{
			"prometheus.query_range":       {"query": "up", "start": now.Add(-5 * time.Minute).Format(time.RFC3339), "end": now.Format(time.RFC3339), "step_seconds": 15},
			"prometheus.list_targets":      {"health": "up"},
			"prometheus.list_metric_names": {"prefix": "prometheus_build"},
			"prometheus.list_alerts":       {},
		} {
			res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
			if err != nil || res.IsError {
				t.Errorf("%s: %v %s", name, err, text(res))
				continue
			}
			body := text(res)
			switch name {
			case "prometheus.query_range":
				if !strings.Contains(body, `"job":"prometheus"`) {
					t.Errorf("query_range: %s", body)
				}
			case "prometheus.list_targets":
				if !strings.Contains(body, `"health":"up"`) || !strings.Contains(body, `"total":1`) {
					t.Errorf("list_targets: %s", body)
				}
			case "prometheus.list_metric_names":
				if !strings.Contains(body, "prometheus_build_info") {
					t.Errorf("list_metric_names: %s", body)
				}
			case "prometheus.list_alerts":
				if !strings.Contains(body, `"total":0`) {
					t.Errorf("list_alerts: %s", body)
				}
			}
			if len(body) > 64<<10 {
				t.Errorf("%s: kết quả %d byte, vượt trần 64 KiB", name, len(body))
			}
		}
	})

	t.Run("a bad expression is a tool error, a bad argument never leaves the gateway", func(t *testing.T) {
		res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "prometheus.query_instant", Arguments: map[string]any{"query": "up{"}})
		if err != nil || !res.IsError {
			t.Fatalf("PromQL sai phải là lỗi của tool: %v %+v", err, res)
		}
		if strings.Contains(text(res), endpoint) {
			t.Errorf("lỗi lộ địa chỉ Prometheus: %s", text(res))
		}
		res, err = sess.CallTool(ctx, &mcp.CallToolParams{Name: "prometheus.query_instant", Arguments: map[string]any{"query": "up", "url": "http://evil.example"}})
		if err == nil && !res.IsError {
			t.Fatal("tham số lạ được chấp nhận")
		}
		var calls int
		if err := e.db.Admin.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE org_id = $1 AND action = 'mcp_tool.call' AND outcome = 'success'`, e.orgA).Scan(&calls); err != nil || calls < 5 {
			t.Fatalf("audit mcp_tool.call thành công = %d (%v), muốn ≥ 5", calls, err)
		}
	})
}
