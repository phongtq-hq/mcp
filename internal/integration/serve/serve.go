// Package serve là phần dùng chung của mọi managed MCP server (integration): HTTP handler
// chỉ nhận request mang ngữ cảnh do Gateway ký, và phân biệt ngữ cảnh discovery với ngữ
// cảnh gọi tool.
//
// Vì sao là một package chứ không chép giữa các integration: đây là ranh giới bảo mật của
// server, và bản chép đầu tiên của nó có một lỗ hổng (xem Handler) nằm ở cả hai integration.
package serve

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hqgroups/mcp/internal/shared/signedctx"
)

// MaxRequestBody là trần kích thước body của một request MCP tới integration.
const MaxRequestBody = 1 << 20

// discoveryMethods là những method mà ngữ cảnh discovery (không gắn tenant) được gọi.
var discoveryMethods = map[string]bool{"initialize": true, "notifications/initialized": true, "tools/list": true, "ping": true}

// errDiscoveryOnly: ngữ cảnh discovery chỉ liệt kê được tool.
var errDiscoveryOnly = errors.New("ngữ cảnh discovery chỉ liệt kê được tool")

// discoveryOnly từ chối mọi method ngoài discoveryMethods. Nó chạy BÊN TRONG MCP SDK, trên
// method mà chính SDK đã đọc ra và sẽ thực thi.
func discoveryOnly(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if !discoveryMethods[method] {
			return nil, errDiscoveryOnly
		}
		return next(ctx, method, req)
	}
}

type kindKey struct{}

// Tuyến của một request đã kiểm chữ ký: tool call trong một execution, tool call trực tiếp
// (client bên ngoài gọi qua Gateway, không thuộc execution nào); còn lại là discovery.
const (
	routeExecution = "execution"
	routeDirect    = "direct"
)

// ErrExecutionOnly là thông điệp của kết quả lỗi khi một tool chỉ chạy trong execution bị
// gọi trực tiếp.
const ErrExecutionOnly = "tool này chỉ chạy trong một execution của Agent; không gọi trực tiếp được qua Gateway"

type config struct {
	direct map[string]bool
}

// Option là tuỳ chọn của Handler.
type Option func(*config)

// Direct khai những tool được gọi TRỰC TIẾP: tool call mang ngữ cảnh KindCall không có
// ExecutionID (client bên ngoài gọi thẳng qua Gateway bằng token OIDC hay API key).
//
// Mặc định mọi tool chỉ chạy trong một execution của Agent (ADR-0018): có Agent Version
// liệt kê tool đó, có budget, có execution_steps. Chỉ khai Direct cho tool trả siêu dữ
// liệu ít nhạy cảm (danh sách, trạng thái, số đo); tool trả log, văn bản tự do do bên
// ngoài viết hay dữ liệu kinh doanh thì không. Mặc định an toàn để integration mới quên
// khai thì đóng, không mở.
//
// Đây là mức sàn ở phía server, không thay cho policy của Gateway: ExecutionID là thứ
// Gateway ký sau khi đã kiểm task (GetActiveTask), server chỉ kiểm chữ ký. Luật được thực
// thi bằng middleware trong MCP SDK, trên method và tên tool mà chính SDK đã đọc ra,
// không bằng cách tự đọc body. tools/list vẫn liệt kê mọi tool.
func Direct(tools ...string) Option {
	return func(c *config) {
		if c.direct == nil {
			c.direct = map[string]bool{}
		}
		for _, t := range tools {
			c.direct[t] = true
		}
	}
}

// directOnly từ chối tools/call tới tool không nằm trong danh sách Direct. Request không
// đọc được tên tool cũng bị từ chối (fail-closed).
func directOnly(allowed map[string]bool) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "tools/call" {
				params, ok := req.GetParams().(*mcp.CallToolParamsRaw)
				if !ok || !allowed[params.Name] {
					return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: ErrExecutionOnly}}}, nil
				}
			}
			return next(ctx, method, req)
		}
	}
}

// Handler trả về HTTP handler của một managed MCP server: MCP Streamable HTTP không trạng
// thái, chỉ nhận request mang ngữ cảnh do Gateway ký cho đúng server này (slug là aud),
// đúng body này, và chưa dùng lần nào. Server không nhận và không kiểm token của người
// dùng; quyết định quyền thuộc về Gateway. Server chỉ giữ khoá CÔNG KHAI (verifier): nó
// không ký được ngữ cảnh, nên bị chiếm quyền cũng không gọi được integration khác.
//
// Server thuộc về đúng một organization (orgID): tool call thay mặt org khác bị từ chối
// kể cả khi chữ ký hợp lệ; orgID rỗng thì mọi tool call bị từ chối. Nhờ vậy đăng ký nhầm
// server ở phạm vi platform không mở dữ liệu của hệ thống đích cho mọi tenant.
//
// Ngữ cảnh discovery (không gắn tenant) chỉ đọc được danh sách tool. Luật đó được thực thi
// bằng một mcp.Server riêng có middleware từ chối mọi method khác, KHÔNG bằng cách tự đọc
// body để đoán method: hai trình đọc JSON hiểu cùng một body theo hai cách (khoá "Method"
// viết hoa, khoá trùng) từng cho phép ngữ cảnh discovery gọi tool. newServer được gọi ba
// lần, mỗi lần phải trả về một server mới với cùng tập tool. Body chỉ được đọc để so hash
// với ngữ cảnh, không để phân quyền.
//
// Tool call không thuộc execution chỉ chạy được tool khai bằng Direct; xem ở đó.
func Handler(slug string, verifier *signedctx.Verifier, orgID uuid.UUID, newServer func() *mcp.Server, opts ...Option) http.Handler {
	var cfg config
	for _, opt := range opts {
		opt(&cfg)
	}
	calls := newServer()
	discovery := newServer()
	discovery.AddReceivingMiddleware(discoveryOnly)
	direct := newServer()
	direct.AddReceivingMiddleware(directOnly(cfg.direct))
	replay := signedctx.NewReplayGuard(0)
	mcpHandler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		switch route, _ := r.Context().Value(kindKey{}).(string); route {
		case routeExecution:
			return calls
		case routeDirect:
			return direct
		}
		return discovery // mọi trường hợp khác: server chỉ liệt kê được tool
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, MaxRequestBodyBytes: MaxRequestBody})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxRequestBody))
		if err != nil {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		now := time.Now()
		sc, err := verifier.Verify(r.Header.Get(signedctx.Header), slug, body, now)
		if err != nil || !replay.Accept(sc, now) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case sc.Kind == signedctx.KindCall:
			if orgID == uuid.Nil || sc.OrgID != orgID {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		case r.Method != http.MethodPost:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Tuyến được chọn từ ngữ cảnh ĐÃ KIỂM CHỮ KÝ, không từ body: execution chỉ có khi
		// Gateway ký ExecutionID (tool call của Agent Runtime qua execution token).
		route := ""
		if sc.Kind == signedctx.KindCall {
			route = routeDirect
			if sc.ExecutionID != nil && *sc.ExecutionID != uuid.Nil {
				route = routeExecution
			}
		}
		r = r.WithContext(context.WithValue(r.Context(), kindKey{}, route))
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		mcpHandler.ServeHTTP(w, r)
	})
}

// VerifierFromEnv đọc khoá công khai kiểm ngữ cảnh của Gateway từ HQ_SERVICE_VERIFY_KEY
// (hex, `hqadmin service-key public`). Managed server KHÔNG được giữ khoá ký: nếu
// HQ_SERVICE_SIGNING_KEY có mặt trong môi trường của nó, process từ chối khởi động, để
// một cấu hình sai (chép nguyên file env của gateway) không biến integration thành bên ký
// được ngữ cảnh cho mọi integration khác (ADR-0018). Thông báo lỗi không chứa giá trị khoá.
func VerifierFromEnv(getenv func(string) string) (*signedctx.Verifier, error) {
	if getenv("HQ_SERVICE_SIGNING_KEY") != "" {
		return nil, errors.New("HQ_SERVICE_SIGNING_KEY không được đặt cho managed server: nó chỉ cần khoá công khai HQ_SERVICE_VERIFY_KEY")
	}
	pub, err := signedctx.ParseVerifyKey(getenv("HQ_SERVICE_VERIFY_KEY"))
	if err != nil {
		return nil, errors.New("HQ_SERVICE_VERIFY_KEY: " + err.Error())
	}
	return signedctx.NewVerifier(pub)
}
