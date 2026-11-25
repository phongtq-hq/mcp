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
	testSeed = strings.Repeat("6b", 32)
	testOrg  = uuid.New()
)

func mustSigner(t *testing.T, hexSeed string) *signedctx.Signer {
	t.Helper()
	priv, err := signedctx.ParseSigningKey(hexSeed)
	if err != nil {
		t.Fatal(err)
	}
	s, err := signedctx.NewSigner(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

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
	f.signer = mustSigner(t, testSeed)
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
	hs := httptest.NewServer(serve.Handler(slug, f.signer.Verifier(), testOrg, newServer, opts...))
	t.Cleanup(hs.Close)
	f.url = hs.URL
	return f
}

// sign ký c cho đúng body.
func (f *fixture) sign(c signedctx.Context, body string) string {
	c.Audience = slug
	return f.signer.SignBody(c, []byte(body), time.Now())
}

func call(org uuid.UUID, execution *uuid.UUID) signedctx.Context {
	return signedctx.Context{Kind: signedctx.KindCall, OrgID: org, ProjectID: uuid.New(), Initiator: "user:" + uuid.NewString(), ExecutionID: execution}
}

func (f *fixture) session(t *testing.T, c signedctx.Context) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	sess, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: f.url, DisableStandaloneSSE: true, MaxRetries: -1,
		HTTPClient: &http.Client{Transport: &signedctx.Transport{Signer: f.signer, Context: withAud(c)}},
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

func withAud(c signedctx.Context) signedctx.Context {
	c.Audience = slug
	return c
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

// Không có tuỳ chọn: mọi tool chỉ chạy trong execution (mặc định an toàn); discovery chỉ
// liệt kê; org khác và chữ ký sai bị từ chối.
func TestHandlerDefaultsToExecutionOnly(t *testing.T) {
	f := newFixture(t)
	exec := uuid.New()
	direct := f.session(t, call(testOrg, nil))
	for _, tool := range []string{"get_guarded", "get_open"} {
		bad, text := refused(direct, tool)
		if !bad || !strings.Contains(text, serve.ErrExecutionOnly) {
			t.Errorf("trực tiếp: %s phải bị từ chối rõ ràng: %v %q", tool, bad, text)
		}
	}
	// tools/list không đổi: Registry và client vẫn thấy đủ tool.
	if tools, err := direct.ListTools(context.Background(), nil); err != nil || len(tools.Tools) != 2 {
		t.Errorf("tools/list trực tiếp phải liệt kê 2 tool: %v", err)
	}
	inExec := f.session(t, call(testOrg, &exec))
	for _, tool := range []string{"get_guarded", "get_open"} {
		if bad, text := refused(inExec, tool); bad {
			t.Errorf("trong execution: %s bị từ chối: %s", tool, text)
		}
	}
	if f.guarded.Load() != 1 || f.open.Load() != 1 {
		t.Errorf("handler chạy %d / %d lần, muốn 1 / 1", f.guarded.Load(), f.open.Load())
	}
	// ExecutionID rỗng (nil UUID) không phải một execution.
	zero := uuid.Nil
	if bad, _ := refused(f.session(t, call(testOrg, &zero)), "get_guarded"); !bad {
		t.Error("ExecutionID rỗng được coi là execution")
	}

	disc := f.session(t, signedctx.Context{Kind: signedctx.KindDiscovery})
	if tools, err := disc.ListTools(context.Background(), nil); err != nil || len(tools.Tools) != 2 {
		t.Errorf("discovery phải liệt kê 2 tool: %v", err)
	}
	if bad, _ := refused(disc, "get_open"); !bad {
		t.Error("ngữ cảnh discovery gọi được tool")
	}
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_open","arguments":{}}}`
	if status, _ := f.post(f.sign(call(uuid.New(), &exec), body), body); status != http.StatusForbidden {
		t.Errorf("org khác: status %d, muốn 403", status)
	}
	if status, _ := f.post("", body); status != http.StatusUnauthorized {
		t.Errorf("thiếu ngữ cảnh: status %d, muốn 401", status)
	}
	if f.open.Load() != 1 {
		t.Errorf("request bị từ chối vẫn chạy handler (%d lần)", f.open.Load())
	}
}

// Direct: tool đã khai gọi trực tiếp được; tool khác vẫn chỉ chạy trong execution.
func TestDirect(t *testing.T) {
	f := newFixture(t, serve.Direct("get_open"))
	exec := uuid.New()

	direct := f.session(t, call(testOrg, nil))
	bad, text := refused(direct, "get_guarded")
	if !bad || !strings.Contains(text, serve.ErrExecutionOnly) {
		t.Errorf("gọi trực tiếp tool không khai Direct phải bị từ chối rõ ràng: %v %q", bad, text)
	}
	if bad, text := refused(direct, "get_open"); bad {
		t.Errorf("tool khai Direct phải gọi trực tiếp được: %s", text)
	}

	// Body mơ hồ (khoá trùng, khoá viết hoa): tên mà middleware xét là tên SDK thực thi, nên
	// dù SDK chọn cách hiểu nào handler của tool chỉ-execution cũng không chạy.
	for _, params := range []string{
		`{"name":"get_open","Name":"get_guarded","arguments":{}}`,
		`{"Name":"get_guarded","arguments":{}}`,
		`{"name":"get_open","name":"get_guarded","arguments":{}}`,
		`{"name":"get_guarded","name":"get_open","arguments":{}}`,
	} {
		for _, body := range []string{
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":` + params + `}`,
			`{"jsonrpc":"2.0","id":1,"Method":"tools/call","method":"tools/list","params":` + params + `}`,
		} {
			f.post(f.sign(call(testOrg, nil), body), body)
		}
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
}

// Ngữ cảnh gắn với đúng một request: body khác, dùng lần hai, ký bằng khoá khác (kể cả
// một bên chỉ có khoá công khai không thể ký) đều bị từ chối trước khi tool chạy.
func TestRequestBinding(t *testing.T) {
	f := newFixture(t)
	exec := uuid.New()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_open","arguments":{}}}`
	other := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_guarded","arguments":{}}}`

	header := f.sign(call(testOrg, &exec), body)
	if status, resp := f.post(header, body); status != http.StatusOK {
		t.Fatalf("lần đầu: %d %s", status, resp)
	}
	if status, _ := f.post(header, body); status != http.StatusUnauthorized {
		t.Errorf("dùng lại header: status %d, muốn 401", status)
	}
	if status, _ := f.post(f.sign(call(testOrg, &exec), body), other); status != http.StatusUnauthorized {
		t.Errorf("header của body khác: status %d, muốn 401", status)
	}
	stranger := mustSigner(t, strings.Repeat("5a", 32))
	c := withAud(call(testOrg, &exec))
	if status, _ := f.post(stranger.SignBody(c, []byte(other), time.Now()), other); status != http.StatusUnauthorized {
		t.Errorf("ký bằng khoá khác: status %d, muốn 401", status)
	}
	if f.open.Load() != 1 || f.guarded.Load() != 0 {
		t.Errorf("handler chạy %d / %d lần, muốn 1 / 0", f.open.Load(), f.guarded.Load())
	}
}
