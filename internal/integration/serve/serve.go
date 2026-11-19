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
// lần, mỗi lần phải trả về một server mới với cùng tập tool.
func Handler(slug string, signer *signedctx.Signer, orgID uuid.UUID, newServer func() *mcp.Server) http.Handler {
	calls := newServer()
	discovery := newServer()
	discovery.AddReceivingMiddleware(discoveryOnly)
	mcpHandler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		if kind, _ := r.Context().Value(kindKey{}).(string); kind == signedctx.KindCall {
			return calls
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
		mcpHandler.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), kindKey{}, sc.Kind)))
	})
}
