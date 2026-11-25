// Package serve là phần dùng chung của mọi managed MCP server (integration): HTTP handler
// chỉ nhận request mang ngữ cảnh do Gateway ký, và phân biệt ngữ cảnh discovery với ngữ
// cảnh gọi tool.
//
// Vì sao là một package chứ không chép giữa các integration: đây là ranh giới bảo mật của
// server, và bản chép đầu tiên của nó có một lỗ hổng (xem Handler) nằm ở cả hai integration.
package serve

import (
	"context"
	"errors"
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
	executionOnly map[string]bool
}

// Option là tuỳ chọn của Handler.
type Option func(*config)

// ExecutionOnly khai những tool chỉ chạy trong một execution: tool call mang ngữ cảnh
// KindCall KHÔNG có ExecutionID (client bên ngoài gọi thẳng qua Gateway bằng token OIDC hay
// API key) nhận một kết quả lỗi và handler của tool không chạy. Dùng cho tool trả dữ liệu
// nhạy cảm, nơi mọi lần đọc phải nằm trong một execution: có Agent Version liệt kê tool đó,
// có budget, có execution_steps.
//
// Đây là mức sàn ở phía server, không thay cho policy của Gateway: ExecutionID là thứ
// Gateway ký sau khi đã kiểm task (GetActiveTask), server chỉ kiểm chữ ký. Giống luật
// discovery, nó được thực thi bằng middleware trong MCP SDK, trên method và tên tool mà
// chính SDK đã đọc ra, không bằng cách tự đọc body. tools/list vẫn liệt kê các tool này.
func ExecutionOnly(tools ...string) Option {
	return func(c *config) {
		if c.executionOnly == nil {
			c.executionOnly = map[string]bool{}
		}
		for _, t := range tools {
			c.executionOnly[t] = true
		}
	}
}

// executionOnly từ chối tools/call tới các tool đã khai. Request không đọc được tên tool
// cũng bị từ chối (fail-closed).
func executionOnly(tools map[string]bool) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "tools/call" {
				params, ok := req.GetParams().(*mcp.CallToolParamsRaw)
				if !ok || tools[params.Name] {
					return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: ErrExecutionOnly}}}, nil
				}
			}
			return next(ctx, method, req)
		}
	}
}

// Handler trả về HTTP handler của một managed MCP server: MCP Streamable HTTP không trạng
// thái, chỉ nhận request mang ngữ cảnh do Gateway ký cho đúng server này (slug là aud).
// Server không nhận và không kiểm token của người dùng; quyết định quyền thuộc về Gateway.
//
// Server thuộc về đúng một organization (orgID): tool call thay mặt org khác bị từ chối
// kể cả khi chữ ký hợp lệ; orgID rỗng thì mọi tool call bị từ chối. Nhờ vậy đăng ký nhầm
// server ở phạm vi platform không mở dữ liệu của hệ thống đích cho mọi tenant.
//
// Ngữ cảnh discovery (không gắn tenant) chỉ đọc được danh sách tool. Luật đó được thực thi
// bằng một mcp.Server riêng có middleware từ chối mọi method khác, KHÔNG bằng cách tự đọc
// body để đoán method: hai trình đọc JSON hiểu cùng một body theo hai cách (khoá "Method"
// viết hoa, khoá trùng) từng cho phép ngữ cảnh discovery gọi tool. newServer được gọi hai
// lần (ba lần khi có ExecutionOnly), mỗi lần phải trả về một server mới với cùng tập tool.
//
// Tuỳ chọn ExecutionOnly thêm một lớp nữa cho những tool được nêu tên; xem ở đó.
func Handler(slug string, signer *signedctx.Signer, orgID uuid.UUID, newServer func() *mcp.Server, opts ...Option) http.Handler {
	var cfg config
	for _, opt := range opts {
		opt(&cfg)
	}
	calls := newServer()
	discovery := newServer()
	discovery.AddReceivingMiddleware(discoveryOnly)
	// direct phục vụ tool call KHÔNG thuộc execution nào. Không có tuỳ chọn ExecutionOnly thì
	// nó chính là calls: hành vi của các integration không dùng tuỳ chọn không đổi.
	direct := calls
	if len(cfg.executionOnly) > 0 {
		direct = newServer()
		direct.AddReceivingMiddleware(executionOnly(cfg.executionOnly))
	}
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
		sc, err := signer.Verify(r.Header.Get(signedctx.Header), slug, time.Now())
		if err != nil {
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
		mcpHandler.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), kindKey{}, route)))
	})
}
