package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/hqgroups/mcp/internal/adminops"
	"github.com/hqgroups/mcp/internal/integration/contract"
	"github.com/hqgroups/mcp/internal/integration/loki"
)

// lokiImage: bản Loki dùng cho test đầu-cuối. Đổi bằng HQ_TEST_LOKI_IMAGE.
const lokiImage = "grafana/loki:3.0.0"

// TestLokiEndToEnd chạy integration Loki với một Loki THẬT (container), qua Gateway:
// MCP client → Gateway → managed server → HTTP API của Loki.
func TestLokiEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("cần Docker")
	}
	e := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	image := lokiImage
	if v := os.Getenv("HQ_TEST_LOKI_IMAGE"); v != "" {
		image = v
	}
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        image,
			ExposedPorts: []string{"3100/tcp"},
			WaitingFor:   wait.ForHTTP("/ready").WithPort("3100/tcp").WithStartupTimeout(3 * time.Minute),
		},
		Started: true,
	})
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("start loki: %v", err)
	}
	endpoint, err := ctr.PortEndpoint(ctx, "3100/tcp", "http")
	if err != nil {
		t.Fatal(err)
	}
	// Dữ liệu mẫu, đẩy thẳng vào Loki (việc của test, không phải của integration: nó chỉ đọc).
	now := time.Now()
	var values [][]string
	for i := range 30 {
		values = append(values, []string{strconv.FormatInt(now.Add(-time.Duration(30-i)*time.Second).UnixNano(), 10), "DONG-LOG-" + strconv.Itoa(i) + " level=error"})
	}
	push, _ := json.Marshal(map[string]any{"streams": []any{map[string]any{"stream": map[string]string{"app": "e2e", "env": "test"}, "values": values}}})
	pushed := false
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline) && !pushed; time.Sleep(2 * time.Second) {
		res, err := http.Post(endpoint+"/loki/api/v1/push", "application/json", bytes.NewReader(push))
		if err == nil {
			pushed = res.StatusCode/100 == 2
			_ = res.Body.Close()
		}
	}
	if !pushed {
		t.Fatal("không đẩy được log mẫu vào Loki")
	}

	signer := testSigner
	client, err := loki.NewClient(endpoint, loki.Options{})
	if err != nil {
		t.Fatal(err)
	}
	up := httptest.NewServer(loki.Handler(client, signer.Verifier(), e.orgA))
	t.Cleanup(up.Close)
	conn, err := e.db.Admin.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = adminops.RegisterManagedMCPServer(ctx, conn.Conn(), signer, adminops.MCPServer{OrgSlug: "org-a", Slug: loki.Slug}, "Loki", up.URL)
	conn.Release()
	if err != nil {
		t.Fatalf("đăng ký server: %v", err)
	}
	projA := e.createProject(e.tok("admin-a"), "loki-a", "Loki A")["id"].(string)
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	sess, err := c.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: e.gw.URL + "/mcp/v1/projects/" + projA, HTTPClient: &http.Client{Transport: bearer{e.tok("admin-a")}},
		DisableStandaloneSSE: true, MaxRetries: -1,
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	text := func(res *mcp.CallToolResult) string {
		var b strings.Builder
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				b.WriteString(tc.Text)
			}
		}
		return b.String()
	}
	// direct: client bên ngoài qua Gateway (/mcp/v1/projects/…), không thuộc execution nào.
	direct := func(name string, args map[string]any) (string, bool) {
		t.Helper()
		res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "loki." + name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return text(res), res.IsError
	}
	// query_logs / query_metric chỉ chạy trong execution (serve.Direct). Các phép kiểm LogQL
	// trên Loki THẬT dưới đây gọi server bằng đúng ngữ cảnh Gateway ký trên tuyến execution
	// (contract.ExecutionTransport); đường execution → Gateway → managed server có test riêng
	// ở TestPricingEngineEndToEnd.
	execSess, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: up.URL, HTTPClient: &http.Client{Transport: contract.ExecutionTransport(signer, loki.Slug, e.orgA, nil)},
		DisableStandaloneSSE: true, MaxRetries: -1,
	}, nil)
	if err != nil {
		t.Fatalf("connect (execution): %v", err)
	}
	t.Cleanup(func() { _ = execSess.Close() })
	call := func(name string, args map[string]any) (string, bool) {
		t.Helper()
		res, err := execSess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return text(res), res.IsError
	}
	start, end := now.Add(-10*time.Minute).UTC().Format(time.RFC3339), now.Add(time.Minute).UTC().Format(time.RFC3339)

	// Loki ghi nhận log sau một nhịp ngắn: thử lại có trần.
	var out string
	var isErr bool
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		out, isErr = call("query_logs", map[string]any{"query": `{app="e2e"} |= "level=error"`, "start": start, "end": end, "limit": 10})
		if !isErr && strings.Contains(out, "DONG-LOG-29") {
			break
		}
	}
	if isErr || !strings.Contains(out, "DONG-LOG-29") || strings.Contains(out, "DONG-LOG-5 ") {
		t.Fatalf("query_logs (10 dòng mới nhất): err=%v %s", isErr, out)
	}
	if out, isErr = call("query_logs", map[string]any{"query": `{app="e2e"}`, "start": start, "end": end, "limit": 5, "direction": "forward"}); isErr || !strings.Contains(out, "DONG-LOG-0 ") || strings.Contains(out, "DONG-LOG-29") {
		t.Errorf("query_logs forward: err=%v %s", isErr, out)
	}
	if out, isErr = call("query_metric", map[string]any{"query": `sum(count_over_time({app="e2e"}[1m]))`, "start": start, "end": end, "step_seconds": 60}); isErr || !strings.Contains(out, `"samples":[{`) {
		t.Errorf("query_metric: err=%v %s", isErr, out)
	}
	// Hai tool đọc log không gọi trực tiếp được qua Gateway.
	for _, name := range []string{"query_logs", "query_metric"} {
		if out, isErr = direct(name, map[string]any{"query": `{app="e2e"}`, "start": start, "end": end}); !isErr || strings.Contains(out, "DONG-LOG") {
			t.Errorf("%s gọi trực tiếp qua Gateway được chấp nhận: %s", name, out)
		}
	}
	// Tool siêu dữ liệu gọi trực tiếp được.
	if out, isErr = direct("list_labels", map[string]any{}); isErr || !strings.Contains(out, `"app"`) || !strings.Contains(out, `"env"`) {
		t.Errorf("list_labels: err=%v %s", isErr, out)
	}
	if out, isErr = direct("list_label_values", map[string]any{"label": "app"}); isErr || !strings.Contains(out, `"e2e"`) {
		t.Errorf("list_label_values: err=%v %s", isErr, out)
	}
	// LogQL sai là lỗi của tool, không lộ địa chỉ Loki; sai loại truy vấn bị từ chối.
	if out, isErr = call("query_logs", map[string]any{"query": `{app="e2e"`, "start": start, "end": end}); !isErr || strings.Contains(out, endpoint) {
		t.Errorf("LogQL sai: err=%v %s", isErr, out)
	}
	// Trần thời gian không vòng qua được bằng LogQL: cửa sổ lớn, offset và việc biến dòng log
	// thành label (review đã chứng minh đường này trên Loki thật trước khi có bộ kiểm).
	recent := now.Add(-time.Minute).UTC().Format(time.RFC3339)
	for _, q := range []string{
		`sum by (l) (count_over_time({app="e2e"} | regexp "(?P<l>.*)" [30d]))`,
		`sum(count_over_time({app="e2e"}[5m] offset 2h))`,
		`sum(count_over_time({app="e2e"}[23h59m]))`,
	} {
		if out, isErr = call("query_metric", map[string]any{"query": q, "start": recent, "end": end, "step_seconds": 60}); !isErr || strings.Contains(out, "DONG-LOG") {
			t.Errorf("biểu thức vượt trần thời gian được chấp nhận: %s → %s", q, out)
		}
	}
	if out, isErr = call("query_logs", map[string]any{"query": `count_over_time({app="e2e"}[1m])`, "start": start, "end": end}); !isErr {
		t.Errorf("metric query vào query_logs được chấp nhận: %s", out)
	}
}
