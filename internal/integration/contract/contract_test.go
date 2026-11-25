package contract_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hqgroups/mcp/internal/integration/contract"
	"github.com/hqgroups/mcp/internal/integration/serve"
	"github.com/hqgroups/mcp/internal/shared/signedctx"
)

// knobs bật từng lỗi của server mẫu. Giá trị không là server đạt chuẩn.
type knobs struct {
	noSignature      bool // không kiểm chữ ký
	discoveryCalls   bool // ngữ cảnh discovery gọi được tool
	sniffBody        bool // chặn discovery bằng cách tự đọc "method" trong body (lệch trình đọc JSON của SDK)
	anyOrg           bool // không kiểm org
	looseSchema      bool // input schema nhận trường lạ
	credentialField  bool // tool nhận tham số "api_key"
	unboundedString  bool // không giới hạn độ dài chuỗi
	bigResult        bool // kết quả vượt trần đã khai
	notReadOnly      bool // tool đọc thiếu readOnlyHint
	badName          bool // tên tool không đạt
	ignoreKey        bool // tool ghi không khử trùng theo idempotency_key
	dryRunWrites     bool // dry_run vẫn ghi
	noDryRunProperty bool // tool ghi thiếu dry_run
	noReplayGuard    bool // nhận lại cùng một header
	directAll        bool // mọi tool gọi trực tiếp được, kể cả tool không khai Direct
}

// target là hệ thống đích giả lập: đếm lần đọc và giữ các ghi chú đã tạo.
type target struct {
	mu       sync.Mutex
	requests int
	notes    int
	byKey    map[string]bool
}

func (g *target) read() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.requests++
}

func (g *target) write(key string, dedupe bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.requests++
	if dedupe && g.byKey[key] {
		return
	}
	g.byKey[key] = true
	g.notes++
}

func (g *target) count() (requests, notes int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.requests, g.notes
}

type getIn struct {
	Name string `json:"name" jsonschema:"tên ghi chú"`
}

type getInCredential struct {
	Name   string `json:"name" jsonschema:"tên ghi chú"`
	APIKey string `json:"api_key,omitempty" jsonschema:"khoá API"`
}

type getOut struct {
	Name string `json:"name"`
	Body string `json:"body"`
}

type addIn struct {
	Name           string `json:"name" jsonschema:"tên ghi chú"`
	IdempotencyKey string `json:"idempotency_key" jsonschema:"khoá khử trùng"`
	DryRun         bool   `json:"dry_run,omitempty" jsonschema:"true để chỉ mô tả tác động"`
}

type addInNoDryRun struct {
	Name           string `json:"name" jsonschema:"tên ghi chú"`
	IdempotencyKey string `json:"idempotency_key" jsonschema:"khoá khử trùng"`
}

type addOut struct {
	Created bool   `json:"created"`
	Plan    string `json:"plan,omitempty"`
}

const slug = "notes"

func newServer(k knobs, g *target) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "hq-notes", Version: "0"}, nil)
	get := func(name string) (getOut, error) {
		if !k.unboundedString && len(name) > 64 {
			return getOut{}, errors.New("name quá dài")
		}
		g.read()
		body := "nội dung"
		if k.bigResult {
			body = strings.Repeat("x", 4096)
		}
		return getOut{Name: "n", Body: body}, nil
	}
	tool := &mcp.Tool{Name: "get_note", Description: "Đọc một ghi chú. Chỉ đọc.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: !k.notReadOnly}}
	if k.badName {
		tool.Name = "getNote"
	}
	switch {
	case k.credentialField:
		mcp.AddTool(s, tool, func(_ context.Context, _ *mcp.CallToolRequest, in getInCredential) (*mcp.CallToolResult, getOut, error) {
			out, err := get(in.Name)
			return nil, out, err
		})
	case k.looseSchema:
		tool.InputSchema = json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`)
		mcp.AddTool(s, tool, func(_ context.Context, _ *mcp.CallToolRequest, in map[string]any) (*mcp.CallToolResult, getOut, error) {
			name, _ := in["name"].(string)
			out, err := get(name)
			return nil, out, err
		})
	default:
		mcp.AddTool(s, tool, func(_ context.Context, _ *mcp.CallToolRequest, in getIn) (*mcp.CallToolResult, getOut, error) {
			out, err := get(in.Name)
			return nil, out, err
		})
	}

	add := func(name, key string, dry bool) (addOut, error) {
		if len(name) > 64 || key == "" || len(key) > 64 {
			return addOut{}, errors.New("arguments không hợp lệ")
		}
		if dry && !k.dryRunWrites {
			return addOut{Plan: "sẽ tạo ghi chú " + name}, nil
		}
		g.write(key, !k.ignoreKey)
		return addOut{Created: true}, nil
	}
	addTool := &mcp.Tool{Name: "add_note", Description: "Tạo một ghi chú."}
	if k.noDryRunProperty {
		mcp.AddTool(s, addTool, func(_ context.Context, _ *mcp.CallToolRequest, in addInNoDryRun) (*mcp.CallToolResult, addOut, error) {
			out, err := add(in.Name, in.IdempotencyKey, false)
			return nil, out, err
		})
	} else {
		mcp.AddTool(s, addTool, func(_ context.Context, _ *mcp.CallToolRequest, in addIn) (*mcp.CallToolResult, addOut, error) {
			out, err := add(in.Name, in.IdempotencyKey, in.DryRun)
			return nil, out, err
		})
	}
	return s
}

var discoveryMethods = map[string]bool{"initialize": true, "notifications/initialized": true, "tools/list": true, "ping": true}

// handler của server mẫu. Không có knob nào thì nó là đúng handler dùng chung của mọi
// integration (serve.Handler). Các knob dựng lại những cách làm sai để chứng minh bộ test
// bắt được chúng; sniffBody là đúng đoạn code từng nằm trong hai integration.
func handler(k knobs, g *target, verifier *signedctx.Verifier, org uuid.UUID) http.Handler {
	if !k.noSignature && !k.discoveryCalls && !k.anyOrg && !k.sniffBody && !k.noReplayGuard {
		direct := []string{"get_note"}
		if k.directAll {
			direct = append(direct, "add_note")
		}
		return serve.Handler(slug, verifier, org, func() *mcp.Server { return newServer(k, g) }, serve.Direct(direct...))
	}
	server := newServer(k, g)
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if k.noSignature {
			h.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		sc, err := verifier.Verify(r.Header.Get(signedctx.Header), slug, body, time.Now())
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case sc.Kind == signedctx.KindCall:
			if !k.anyOrg && sc.OrgID != org {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		case !k.discoveryCalls:
			var msg struct {
				Method string `json:"method"`
			}
			if json.Unmarshal(body, &msg) != nil || !discoveryMethods[msg.Method] {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}

func server(k knobs) contract.Server {
	priv, _ := signedctx.ParseSigningKey(strings.Repeat("6b", 32))
	signer, _ := signedctx.NewSigner(priv)
	org := uuid.New()
	g := &target{byKey: map[string]bool{}}
	add := contract.Tool{Name: "add_note", Write: true, MaxResultBytes: 1024,
		Samples: []map[string]any{{"name": "a"}},
		Effects: func() int { _, n := g.count(); return n }}
	if k.noDryRunProperty {
		// Cố ý không khai NoDryRun: thiếu dry_run mà không nêu lý do là vi phạm.
		add.NoDryRun = ""
	}
	get := contract.Tool{Name: "get_note", Direct: true, MaxResultBytes: 1024, Samples: []map[string]any{{"name": "a"}}}
	if k.badName {
		get.Name = "getNote"
	}
	return contract.Server{
		Slug: slug, Handler: handler(k, g, signer.Verifier(), org), Signer: signer, OrgID: org,
		TargetRequests: func() int { n, _ := g.count(); return n },
		Tools:          []contract.Tool{get, add},
	}
}

// Server đạt chuẩn qua bộ test; mỗi lỗi cố ý bị bắt đúng tên.
func TestContractCatchesViolations(t *testing.T) {
	contract.Run(t, server(knobs{}))

	for name, tc := range map[string]struct {
		k    knobs
		want string
	}{
		"không kiểm chữ ký":         {knobs{noSignature: true}, "chữ ký:"},
		"discovery gọi được tool":   {knobs{discoveryCalls: true}, "discovery:"},
		"chặn discovery bằng body":  {knobs{sniffBody: true}, "discovery: tools/call thô được chấp nhận"},
		"không kiểm org":            {knobs{anyOrg: true}, "tenant:"},
		"schema nhận trường lạ":     {knobs{looseSchema: true}, "additionalProperties: false"},
		"tham số credential":        {knobs{credentialField: true}, "tên trường bị cấm (credential)"},
		"chuỗi không giới hạn":      {knobs{unboundedString: true}, "cần giới hạn độ dài"},
		"kết quả vượt trần":         {knobs{bigResult: true}, "vượt trần"},
		"tool đọc thiếu chú thích":  {knobs{notReadOnly: true}, "readOnlyHint"},
		"tên tool sai":              {knobs{badName: true}, "snake_case"},
		"bỏ qua idempotency_key":    {knobs{ignoreKey: true}, "cùng idempotency_key gây 2 tác động"},
		"dry_run vẫn ghi":           {knobs{dryRunWrites: true}, "dry_run gây 1 tác động"},
		"tool ghi thiếu dry_run":    {knobs{noDryRunProperty: true}, "phải có dry_run"},
		"nhận lại header":           {knobs{noReplayGuard: true}, "dùng lại header"},
		"mở gọi trực tiếp quá rộng": {knobs{directAll: true}, "không khai Direct nhưng gọi trực tiếp được"},
	} {
		got := contract.Check(context.Background(), server(tc.k))
		if !strings.Contains(strings.Join(got, "\n"), tc.want) {
			t.Errorf("%s: muốn vi phạm chứa %q, nhận %q", name, tc.want, got)
		}
	}
}

// Tool ghi khai lý do không có dry_run thì được miễn dry_run, không được miễn idempotency.
func TestNoDryRunNeedsReason(t *testing.T) {
	srv := server(knobs{noDryRunProperty: true})
	srv.Tools[1].NoDryRun = "hệ thống đích không có chế độ chạy thử"
	if got := contract.Check(context.Background(), srv); len(got) != 0 {
		t.Errorf("muốn không vi phạm, nhận %q", got)
	}
}
