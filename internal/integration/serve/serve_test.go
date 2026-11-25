package serve_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hqgroups/mcp/internal/integration/serve"
	"github.com/hqgroups/mcp/internal/shared/signedctx"
)

const slug = "serve-test"

var (
	testKey = []byte(strings.Repeat("k", 32))
	testOrg = uuid.New()
)

type in struct{}

type out struct {
	OK bool `json:"ok"`
}

// fixture là một server có hai tool; guarded đếm số lần handler của nó thật sự chạy.
type fixture struct {
	guarded, open atomic.Int64
	signer        *signedctx.Signer
	url           string
}

func newFixture(t *testing.T, opts ...serve.Option) *fixture {
	t.Helper()
	f := &fixture{}
	f.signer, _ = signedctx.NewSigner(testKey)
	newServer := func() *mcp.Server {
		s := mcp.NewServer(&mcp.Implementation{Name: "serve-test", Version: "0"}, nil)
		mcp.AddTool(s, &mcp.Tool{Name: "get_guarded"}, func(context.Context, *mcp.CallToolRequest, in) (*mcp.CallToolResult, out, error) {
			f.guarded.Add(1)
			return nil, out{OK: true}, nil
		})
		mcp.AddTool(s, &mcp.Tool{Name: "get_open"}, func(context.Context, *mcp.CallToolRequest, in) (*mcp.CallToolResult, out, error) {
			f.open.Add(1)
			return nil, out{OK: true}, nil
		})
		return s
	}
	hs := httptest.NewServer(serve.Handler(slug, f.signer, testOrg, newServer, opts...))
	t.Cleanup(hs.Close)
	f.url = hs.URL
	return f
}

func (f *fixture) sign(c signedctx.Context) string {
	c.Audience = slug
	return f.signer.Sign(c, time.Now())
}

func call(org uuid.UUID, execution *uuid.UUID) signedctx.Context {
	return signedctx.Context{Kind: signedctx.KindCall, OrgID: org, ProjectID: uuid.New(), Initiator: "user:" + uuid.NewString(), ExecutionID: execution}
}

type headerTransport struct{ value func() string }

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set(signedctx.Header, h.value())
	return http.DefaultTransport.RoundTrip(r)
}

func (f *fixture) session(t *testing.T, c signedctx.Context) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	sess, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: f.url, DisableStandaloneSSE: true, MaxRetries: -1,
		HTTPClient: &http.Client{Transport: headerTransport{func() string { return f.sign(c) }}},
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

// refused: lời gọi bị từ chối (lỗi giao thức hoặc kết quả isError).
func refused(sess *mcp.ClientSession, tool string) (bool, string) {
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: map[string]any{}})
	if err != nil {
		return true, err.Error()
	}
	text := ""
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text += tc.Text
		}
	}
	return res.IsError, text
}

func (f *fixture) post(header, body string) (int, string) {
	req, _ := http.NewRequest(http.MethodPost, f.url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if header != "" {
		req.Header.Set(signedctx.Header, header)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(b)
}

// Không có tuỳ chọn: hành vi cũ. Tool call của org sở hữu chạy được dù có execution hay
// không; discovery chỉ liệt kê; org khác và chữ ký sai bị từ chối.
func TestHandlerWithoutOptions(t *testing.T) {
	f := newFixture(t)
	exec := uuid.New()
	for name, c := range map[string]signedctx.Context{"trực tiếp": call(testOrg, nil), "trong execution": call(testOrg, &exec)} {
		sess := f.session(t, c)
		for _, tool := range []string{"get_guarded", "get_open"} {
			if bad, text := refused(sess, tool); bad {
				t.Errorf("%s: %s bị từ chối: %s", name, tool, text)
			}
		}
	}
	if f.guarded.Load() != 2 || f.open.Load() != 2 {
		t.Errorf("handler chạy %d / %d lần, muốn 2 / 2", f.guarded.Load(), f.open.Load())
	}

	disc := f.session(t, signedctx.Context{Kind: signedctx.KindDiscovery})
	if tools, err := disc.ListTools(context.Background(), nil); err != nil || len(tools.Tools) != 2 {
		t.Errorf("discovery phải liệt kê 2 tool: %v", err)
	}
	if bad, _ := refused(disc, "get_open"); !bad {
		t.Error("ngữ cảnh discovery gọi được tool")
	}
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_open","arguments":{}}}`
	if status, _ := f.post(f.sign(call(uuid.New(), &exec)), body); status != http.StatusForbidden {
		t.Errorf("org khác: status %d, muốn 403", status)
	}
	if status, _ := f.post("", body); status != http.StatusUnauthorized {
		t.Errorf("thiếu ngữ cảnh: status %d, muốn 401", status)
	}
	if f.open.Load() != 2 {
		t.Errorf("request bị từ chối vẫn chạy handler (%d lần)", f.open.Load())
	}
}

// ExecutionOnly: tool đã khai chỉ chạy khi ngữ cảnh đã ký mang ExecutionID; tool khác không đổi.
func TestExecutionOnly(t *testing.T) {
	f := newFixture(t, serve.ExecutionOnly("get_guarded"))
	exec := uuid.New()

	direct := f.session(t, call(testOrg, nil))
	bad, text := refused(direct, "get_guarded")
	if !bad || !strings.Contains(text, serve.ErrExecutionOnly) {
		t.Errorf("gọi trực tiếp tool chỉ-execution phải bị từ chối rõ ràng: %v %q", bad, text)
	}
	if bad, text := refused(direct, "get_open"); bad {
		t.Errorf("tool không khai phải gọi trực tiếp được: %s", text)
	}
	// tools/list không đổi: Registry và client vẫn thấy đủ tool.
	if tools, err := direct.ListTools(context.Background(), nil); err != nil || len(tools.Tools) != 2 {
		t.Errorf("tools/list trực tiếp phải liệt kê 2 tool: %v", err)
	}

	// ExecutionID rỗng (nil UUID) không phải một execution.
	zero := uuid.Nil
	if bad, _ := refused(f.session(t, call(testOrg, &zero)), "get_guarded"); !bad {
		t.Error("ExecutionID rỗng được coi là execution")
	}

	// Body mơ hồ (khoá trùng, khoá viết hoa): tên mà middleware xét là tên SDK thực thi, nên
	// dù SDK chọn cách hiểu nào handler của tool chỉ-execution cũng không chạy.
	for _, params := range []string{
		`{"name":"get_open","Name":"get_guarded","arguments":{}}`,
		`{"Name":"get_guarded","arguments":{}}`,
		`{"name":"get_open","name":"get_guarded","arguments":{}}`,
		`{"name":"get_guarded","name":"get_open","arguments":{}}`,
	} {
		f.post(f.sign(call(testOrg, nil)), `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":`+params+`}`)
		f.post(f.sign(call(testOrg, nil)), `{"jsonrpc":"2.0","id":1,"Method":"tools/call","method":"tools/list","params":`+params+`}`)
	}
	if n := f.guarded.Load(); n != 0 {
		t.Fatalf("handler của tool chỉ-execution đã chạy %d lần ngoài execution", n)
	}

	inExec := f.session(t, call(testOrg, &exec))
	for _, tool := range []string{"get_guarded", "get_open"} {
		if bad, text := refused(inExec, tool); bad {
			t.Errorf("trong execution: %s bị từ chối: %s", tool, text)
		}
	}
	if n := f.guarded.Load(); n != 1 {
		t.Errorf("handler của tool chỉ-execution chạy %d lần, muốn 1", n)
	}

	// Luật cũ vẫn giữ: discovery chỉ liệt kê; org khác bị từ chối kể cả khi có execution.
	if bad, _ := refused(f.session(t, signedctx.Context{Kind: signedctx.KindDiscovery}), "get_open"); !bad {
		t.Error("ngữ cảnh discovery gọi được tool")
	}
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_guarded","arguments":{}}}`
	if status, _ := f.post(f.sign(call(uuid.New(), &exec)), body); status != http.StatusForbidden {
		t.Errorf("org khác: status %d, muốn 403", status)
	}
	if n := f.guarded.Load(); n != 1 {
		t.Errorf("org khác chạy được tool chỉ-execution (%d)", n)
	}
}
