package docker_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hqgroups/mcp/internal/integration/contract"
	"github.com/hqgroups/mcp/internal/integration/docker"
	"github.com/hqgroups/mcp/internal/shared/signedctx"
)

// frame tạo một khung của luồng log ghép của Docker.
func frame(stream byte, s string) []byte {
	h := make([]byte, 8)
	h[0] = stream
	binary.BigEndian.PutUint32(h[4:], uint32(len(s))) //nolint:gosec // dữ liệu test nhỏ
	return append(h, s...)
}

// fakeDocker là một Docker Engine API tối thiểu, ghi lại mọi request nhận được.
func fakeDocker(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var seen []string
	mux := http.NewServeMux()
	mux.HandleFunc("/containers/json", func(w http.ResponseWriter, r *http.Request) {
		all := `[{"Id":"aaaaaaaaaaaaaaaaaaaaaaaa","Names":["/api"],"Image":"api:1","State":"running","Status":"Up 2 hours","Created":1700000000},
		         {"Id":"bbbbbbbbbbbbbbbbbbbbbbbb","Names":["/worker"],"Image":"worker:1","State":"restarting","Status":"Restarting (1) 4 seconds ago","Created":1700000000}]`
		_, _ = w.Write([]byte(all))
	})
	mux.HandleFunc("/containers/worker/json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"Id":"bbbbbbbbbbbbbbbbbbbbbbbb","Name":"/worker","Created":"2026-11-01T00:00:00Z","RestartCount":47,
			"Config":{"Image":"worker:1","Env":["DATABASE_URL=postgres://u:hunter2@db/prod","API_TOKEN=sk-secret"],"Labels":{"secret":"label-secret"},"Cmd":["run","--password=cmd-secret"]},
			"State":{"Status":"restarting","Running":true,"Restarting":true,"ExitCode":1,"Error":"","Health":{"Status":"unhealthy"}},
			"HostConfig":{"RestartPolicy":{"Name":"always"},"Binds":["/host/secret-path:/data"]},
			"Mounts":[{"Source":"/host/secret-path"}],
			"NetworkSettings":{"Ports":{"8080/tcp":[{"HostIp":"0.0.0.0","HostPort":"18080"}]},"Networks":{"backend":{}}}}`))
	})
	mux.HandleFunc("/containers/worker/logs", func(w http.ResponseWriter, _ *http.Request) {
		var b bytes.Buffer
		b.Write(frame(1, "2026-11-17T08:00:00Z starting worker\n"))
		b.Write(frame(2, "2026-11-17T08:00:01Z panic: dial tcp 10.0.0.5:5432: connection refused\n"))
		_, _ = w.Write(b.Bytes())
	})
	mux.HandleFunc("/containers/noisy/logs", func(w http.ResponseWriter, _ *http.Request) {
		for i := 0; i < 20000; i++ {
			_, _ = w.Write(frame(1, "line "+strings.Repeat("x", 40)+"\n"))
		}
	})
	mux.HandleFunc("/containers/tty/logs", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("raw tty output\n"))
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

// headerTransport đặt header ngữ cảnh do value tính trên đúng body của request.
type headerTransport struct {
	value func(body []byte) string
	base  http.RoundTripper
}

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
		_ = r.Body.Close()
	}
	r = r.Clone(r.Context())
	r.Body = io.NopCloser(bytes.NewReader(body))
	if v := h.value(body); v != "" {
		r.Header.Set(signedctx.Header, v)
	}
	return h.base.RoundTrip(r)
}

var testSigner = contract.TestSigner()

var testOrg = uuid.New()

func signed(aud string) func([]byte) string { return signedFor(aud, signedctx.KindCall, testOrg) }

// signedFor ký như Gateway: tool call nằm trong một execution (tuyến của Agent Runtime).
func signedFor(aud, kind string, org uuid.UUID) func([]byte) string {
	return func(body []byte) string {
		c := signedctx.Context{Kind: kind, Audience: aud}
		if kind == signedctx.KindCall {
			exec := uuid.New()
			c.OrgID, c.ProjectID, c.Initiator, c.ExecutionID = org, uuid.New(), "user:"+uuid.NewString(), &exec
		}
		return testSigner.SignBody(c, body, time.Now())
	}
}

// connect dựng server MCP Docker trên một Docker API giả và nối một MCP client thật vào.
func connect(t *testing.T, header func([]byte) string) (*mcp.ClientSession, *[]string, string) {
	t.Helper()
	api, seen := fakeDocker(t)
	client, err := docker.NewClient(api.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(docker.Handler(client, testSigner.Verifier(), testOrg))
	t.Cleanup(srv.Close)

	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	sess, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: srv.URL, DisableStandaloneSSE: true, MaxRetries: -1,
		HTTPClient: &http.Client{Transport: headerTransport{value: header, base: http.DefaultTransport}},
	}, nil)
	if err != nil {
		return nil, seen, srv.URL
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess, seen, srv.URL
}

func call(t *testing.T, sess *mcp.ClientSession, name string, args any) (*mcp.CallToolResult, string) {
	t.Helper()
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	b, _ := json.Marshal(res.StructuredContent)
	text := ""
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text += tc.Text
		}
	}
	return res, string(b) + text
}

func TestToolsOverMCP(t *testing.T) {
	sess, seen, _ := connect(t, signed(docker.Slug))
	if sess == nil {
		t.Fatal("không kết nối được với ngữ cảnh hợp lệ")
	}
	ctx := context.Background()

	list, err := sess.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]*mcp.Tool{}
	for _, tool := range list.Tools {
		names[tool.Name] = tool
	}
	if len(names) != 3 {
		t.Fatalf("muốn đúng 3 tool, có %d", len(names))
	}
	for _, n := range []string{"list_containers", "inspect_container", "get_container_logs"} {
		tool := names[n]
		if tool == nil || tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("tool %s thiếu hoặc không được đánh dấu chỉ đọc", n)
			continue
		}
		// Schema chặt: không nhận trường lạ.
		schema, _ := json.Marshal(tool.InputSchema)
		if !strings.Contains(string(schema), `"additionalProperties":false`) {
			t.Errorf("schema của %s cho phép trường lạ: %s", n, schema)
		}
	}
	for n := range names {
		for _, bad := range []string{"exec", "run", "remove", "stop", "kill", "restart", "start", "create", "delete"} {
			if strings.Contains(n, bad) {
				t.Errorf("server chỉ đọc có tool %q", n)
			}
		}
	}

	res, out := call(t, sess, "list_containers", map[string]any{"all": true})
	if res.IsError || !strings.Contains(out, `"worker"`) || !strings.Contains(out, "Restarting (1) 4 seconds ago") || !strings.Contains(out, `"id":"bbbbbbbbbbbb"`) {
		t.Errorf("list_containers: %s", out)
	}

	res, out = call(t, sess, "inspect_container", map[string]any{"name": "worker"})
	if res.IsError || !strings.Contains(out, `"restart_count":47`) || !strings.Contains(out, `"health":"unhealthy"`) || !strings.Contains(out, "18080") {
		t.Errorf("inspect_container: %s", out)
	}
	// Biến môi trường, label, lệnh và đường dẫn mount không bao giờ rời server.
	for _, secret := range []string{"hunter2", "sk-secret", "label-secret", "cmd-secret", "secret-path", "DATABASE_URL"} {
		if strings.Contains(out, secret) {
			t.Errorf("inspect_container lộ %q: %s", secret, out)
		}
	}

	res, out = call(t, sess, "get_container_logs", map[string]any{"name": "worker", "tail": 50})
	if res.IsError || !strings.Contains(out, "starting worker") || !strings.Contains(out, "connection refused") || strings.Contains(out, "\\u0001") {
		t.Errorf("get_container_logs (tách khung stdout/stderr): %s", out)
	}
	_, out = call(t, sess, "get_container_logs", map[string]any{"name": "tty"})
	if !strings.Contains(out, "raw tty output") {
		t.Errorf("log của container TTY: %s", out)
	}
	res, out = call(t, sess, "get_container_logs", map[string]any{"name": "noisy", "tail": 1000})
	// SDK trả kết quả ở cả dạng có cấu trúc lẫn dạng văn bản, nên tổng gấp đôi; mỗi
	// bản vẫn bị giới hạn ở 256 KiB thay vì gần 1 MB log gốc.
	if res.IsError || !strings.Contains(out, `"truncated":true`) || len(out) > 3*(256<<10) {
		t.Errorf("log lớn phải bị cắt: %d byte, truncated=%v", len(out), strings.Contains(out, `"truncated":true`))
	}

	// Đầu vào xấu là lỗi của tool, không tới được Docker API.
	before := len(*seen)
	for name, args := range map[string]any{
		"path traversal":     map[string]any{"name": "../../version"},
		"tên có dấu gạch":    map[string]any{"name": "a/b"},
		"tên bắt đầu bằng -": map[string]any{"name": "-rf"},
		"tên rỗng":           map[string]any{"name": ""},
		"trường lạ":          map[string]any{"name": "worker", "exec": "rm -rf /"},
		"thiếu name":         map[string]any{},
	} {
		res, out := call(t, sess, "inspect_container", args)
		if !res.IsError {
			t.Errorf("%s: phải là lỗi tool, nhận %s", name, out)
		}
	}
	res, _ = call(t, sess, "get_container_logs", map[string]any{"name": "worker", "tail": 100000})
	if !res.IsError {
		t.Error("tail vượt trần phải bị từ chối")
	}
	if len(*seen) != before {
		t.Errorf("đầu vào xấu đã tới Docker API: %v", (*seen)[before:])
	}
	res, _ = call(t, sess, "inspect_container", map[string]any{"name": "missing"})
	if !res.IsError {
		t.Error("container không tồn tại phải là lỗi tool")
	}

	// Server chỉ gọi GET tới các đường dẫn đọc container.
	for _, r := range *seen {
		if !strings.HasPrefix(r, "GET /containers/") {
			t.Errorf("server gọi Docker API ngoài phạm vi đọc container: %s", r)
		}
	}
}

func TestOnlyGatewaySignedRequestsAreAccepted(t *testing.T) {
	otherPriv, _ := signedctx.ParseSigningKey(strings.Repeat("7a", 32))
	other, _ := signedctx.NewSigner(otherPriv)
	call := signedctx.Context{Kind: signedctx.KindCall, Audience: docker.Slug, OrgID: testOrg, ProjectID: uuid.New(), Initiator: "u"}
	for name, header := range map[string]func([]byte) string{
		"không có ngữ cảnh":   func([]byte) string { return "" },
		"đích là server khác": signed("kubernetes"),
		"khoá khác":           func(b []byte) string { return other.SignBody(call, b, time.Now()) },
		"đã hết hạn":          func(b []byte) string { return testSigner.SignBody(call, b, time.Now().Add(-time.Hour)) },
		"body khác":           func([]byte) string { return testSigner.SignBody(call, []byte("{}"), time.Now()) },
		"rác":                 func([]byte) string { return "abc.def" },
	} {
		sess, seen, url := connect(t, header)
		if sess != nil {
			t.Errorf("%s: kết nối được", name)
		}
		resp, err := http.Post(url, "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)) //nolint:noctx,gosec // test
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized || len(*seen) != 0 {
			t.Errorf("%s: status %d, request tới Docker: %v", name, resp.StatusCode, *seen)
		}
	}
}

// Chạy với Docker thật trên máy (chỉ đọc), nếu có socket.
func TestAgainstRealDocker(t *testing.T) {
	const sock = "/var/run/docker.sock"
	if _, err := os.Stat(sock); err != nil || testing.Short() {
		t.Skip("không có Docker socket hoặc đang chạy -short")
	}
	hc := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}
	c, err := docker.NewClient("http://docker", hc)
	if err != nil {
		t.Fatal(err)
	}
	cs, _, err := c.ListContainers(context.Background(), true, 5)
	if err != nil {
		t.Fatalf("list trên Docker thật: %v", err)
	}
	for _, ct := range cs {
		d, err := c.Inspect(context.Background(), ct.ID)
		if err != nil || d.ID != ct.ID {
			t.Errorf("inspect %s: %+v %v", ct.ID, d, err)
		}
		if _, _, err := c.Logs(context.Background(), ct.ID, 5, 0); err != nil {
			t.Errorf("logs %s: %v", ct.ID, err)
		}
	}
	t.Logf("đọc được %d container trên Docker thật", len(cs))
}

// Docker host thuộc một org: chữ ký hợp lệ của org khác không đọc được gì, và ngữ
// cảnh discovery chỉ liệt kê được tool.
func TestTenantBinding(t *testing.T) {
	if sess, _, _ := connect(t, signedFor(docker.Slug, signedctx.KindCall, uuid.New())); sess != nil {
		t.Error("org khác kết nối được vào Docker host của org này")
	}
	sess, seen, _ := connect(t, signedFor(docker.Slug, signedctx.KindDiscovery, uuid.Nil))
	if sess == nil {
		t.Fatal("discovery không kết nối được")
	}
	if res, err := sess.ListTools(context.Background(), nil); err != nil || len(res.Tools) != 3 {
		t.Errorf("discovery tools/list: %v %v", res, err)
	}
	if res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_containers", Arguments: map[string]any{}}); err == nil {
		t.Errorf("ngữ cảnh discovery gọi được tool: %+v", res)
	}
	if len(*seen) != 0 {
		t.Errorf("Docker API bị gọi: %v", *seen)
	}
}

// countingTransport đếm request mà server gửi tới Docker API.
type countingTransport struct{ n *atomic.Int64 }

func (c countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.n.Add(1)
	return http.DefaultTransport.RoundTrip(r)
}

// Bộ test hợp đồng dùng chung của managed MCP server (mcp-platform §7).
// MaxResultBytes là trần đề xuất cho Registry. get_container_logs cắt log theo kích thước
// ĐÃ TUẦN TỰ HOÁ (60 KiB, giữ phần mới nhất), nên kết quả nằm dưới trần mặc định 64 KiB
// của Registry và không bị Gateway cắt giữa chừng JSON.
func TestContract(t *testing.T) {
	api, _ := fakeDocker(t)
	var requests atomic.Int64
	client, err := docker.NewClient(api.URL, &http.Client{Timeout: 20 * time.Second, Transport: countingTransport{&requests}})
	if err != nil {
		t.Fatal(err)
	}
	contract.Run(t, contract.Server{
		Slug:           docker.Slug,
		Handler:        docker.Handler(client, testSigner.Verifier(), testOrg),
		Signer:         testSigner,
		OrgID:          testOrg,
		TargetRequests: func() int { return int(requests.Load()) },
		Tools: []contract.Tool{
			{Name: "list_containers", Direct: true, MaxResultBytes: 256 << 10, Samples: []map[string]any{{}, {"all": true, "limit": 200}}},
			{Name: "inspect_container", Direct: true, MaxResultBytes: 16 << 10, Samples: []map[string]any{{"name": "worker"}}},
			{Name: "get_container_logs", MaxResultBytes: 64 << 10, Samples: []map[string]any{
				{"name": "worker", "tail": 50}, {"name": "tty"}, {"name": "noisy", "tail": 1000, "since_seconds": 3600}}},
		},
	})
}
