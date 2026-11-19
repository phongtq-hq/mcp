// Package client là MCP client của Agent Runtime tới MCP Gateway. Runtime không
// bao giờ nối thẳng tới MCP server: mọi tool call đi qua Gateway (ADR-0004), xác
// thực bằng execution token của task.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	maxResponseBytes = 8 << 20
	maxToolPages     = 20
	maxTools         = 2000
)

// Tool là định nghĩa một tool mà Gateway cho phép task dùng.
type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
	// ReadOnly: Gateway khai tool này chỉ đọc (gọi lại không có tác dụng phụ).
	ReadOnly bool
}

// Result là kết quả của một tool call đã qua hậu xử lý của Gateway.
type Result struct {
	Text    string
	IsError bool
	// Code là mã quyết định của Gateway khi tool không được chạy (approval_required,
	// approval_denied, denied, outcome_unknown); rỗng với kết quả của chính tool.
	Code string
	// ApprovalID là approval liên quan, nếu có.
	ApprovalID *uuid.UUID
}

// ErrNotSent: lời gọi tool chưa được gửi tới Gateway (lỗi ở bước mở phiên). Bên gọi biết
// chắc tool chưa chạy.
var ErrNotSent = errors.New("mcp client: tool call chưa được gửi")

// Khoá _meta của đường nội bộ, khớp internal/mcp/gateway.
const (
	metaStepSeq    = "hq/step_seq"
	metaCode       = "hq/code"
	metaApprovalID = "hq/approval_id"
)

// Gateway gọi endpoint nội bộ của MCP Gateway.
type Gateway struct {
	base string
	rt   http.RoundTripper
}

// NewGateway nhận URL gốc của Gateway (cấu hình của người vận hành, không phải của tenant).
func NewGateway(baseURL string) (*Gateway, error) {
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return nil, errors.New("mcp client: URL của gateway phải là http(s)")
	}
	return &Gateway{base: strings.TrimRight(baseURL, "/"), rt: http.DefaultTransport}, nil
}

type bearer struct {
	token string
	base  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	resp, err := b.base.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	resp.Body = &limited{ReadCloser: resp.Body, left: maxResponseBytes}
	return resp, nil
}

type limited struct {
	io.ReadCloser
	left int64
}

func (l *limited) Read(p []byte) (int, error) {
	if l.left <= 0 {
		return 0, errors.New("mcp client: phản hồi của gateway vượt giới hạn kích thước")
	}
	if int64(len(p)) > l.left {
		p = p[:l.left]
	}
	n, err := l.ReadCloser.Read(p)
	l.left -= int64(n)
	return n, err
}

func (g *Gateway) connect(ctx context.Context, token string, taskID uuid.UUID) (*mcp.ClientSession, error) {
	hc := &http.Client{
		Transport:     bearer{token: token, base: g.rt},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	c := mcp.NewClient(&mcp.Implementation{Name: "hq-agent-runtime", Version: "1"}, nil)
	return c.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: g.base + "/mcp/internal/v1/tasks/" + taskID.String(), HTTPClient: hc,
		DisableStandaloneSSE: true, MaxRetries: -1,
	}, nil)
}

// ListTools trả về tập tool của task.
func (g *Gateway) ListTools(ctx context.Context, token string, taskID uuid.UUID) ([]Tool, error) {
	sess, err := g.connect(ctx, token, taskID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = sess.Close() }()
	var out []Tool
	cursor := ""
	for page := 0; page < maxToolPages; page++ {
		res, err := sess.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, err
		}
		for _, t := range res.Tools {
			schema, err := json.Marshal(t.InputSchema)
			if err != nil {
				return nil, err
			}
			out = append(out, Tool{Name: t.Name, Description: t.Description, InputSchema: schema,
				ReadOnly: t.Annotations != nil && t.Annotations.ReadOnlyHint})
		}
		if res.NextCursor == "" || len(out) >= maxTools {
			break
		}
		cursor = res.NextCursor
	}
	return out, nil
}

// CallTool gọi một tool. Lỗi trả về là lỗi vận chuyển; tool bị từ chối hoặc thất bại
// là một Result với IsError.
// stepSeq là step_seq của tool call trong task: Gateway gắn approval và việc chống chạy
// trùng của tool ghi với (task, step_seq).
func (g *Gateway) CallTool(ctx context.Context, token string, taskID uuid.UUID, name string, args json.RawMessage, stepSeq int) (Result, error) {
	sess, err := g.connect(ctx, token, taskID)
	if err != nil {
		// Chưa tới bước gọi tool: Gateway từ chối phiên (token, quyền, task không còn
		// chạy) hoặc không tới được. Tool chắc chắn chưa chạy.
		return Result{}, fmt.Errorf("%w: %w", ErrNotSent, err)
	}
	defer func() { _ = sess.Close() }()
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Meta: mcp.Meta{metaStepSeq: stepSeq}, Name: name, Arguments: args})
	if err != nil {
		// Lỗi JSON-RPC là câu trả lời của Gateway (tool không còn trong danh sách của task,
		// tham số sai): nó từ chối TRƯỚC khi chạy pipeline, nên tool chắc chắn chưa chạy.
		// Chỉ lỗi truyền tải mới là "không biết".
		var rpcErr *jsonrpc.Error
		if errors.As(err, &rpcErr) {
			return Result{}, fmt.Errorf("%w: %w", ErrNotSent, err)
		}
		return Result{}, err
	}
	out := Result{IsError: res.IsError}
	if code, ok := res.Meta[metaCode].(string); ok && len(code) <= 64 {
		out.Code = code
	}
	if id, ok := res.Meta[metaApprovalID].(string); ok {
		if parsed, err := uuid.Parse(id); err == nil {
			out.ApprovalID = &parsed
		}
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	out.Text = b.String()
	return out, nil
}

// Discover yêu cầu Gateway chạy discovery cho một MCP server ngoài của org và trả về
// ảnh chụp ở dạng truyền. token là service token dành riêng cho server đó.
func (g *Gateway) Discover(ctx context.Context, token string, serverID uuid.UUID) (json.RawMessage, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.base+"/registry/internal/v1/servers/"+serverID.String()+"/discover", nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	hc := &http.Client{Transport: g.rt, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := hc.Do(req)
	if err != nil {
		return nil, 0, errors.New("mcp client: không kết nối được gateway")
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes+1))
	if err != nil || len(body) > maxResponseBytes {
		return nil, 0, errors.New("mcp client: phản hồi của gateway không đọc được hoặc quá lớn")
	}
	return body, res.StatusCode, nil
}
