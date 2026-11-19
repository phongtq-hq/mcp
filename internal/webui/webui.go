// Package webui phục vụ giao diện web của platform ở /ui/: một ứng dụng React (mã nguồn ở
// web/, ADR-0016) được build thành file tĩnh vào thư mục dist và nhúng vào binary `api`.
//
// Giao diện không có quyền gì của riêng nó. Trình duyệt đăng nhập với IdP bằng
// Authorization Code + PKCE (public client), rồi gọi Management API (/v1) bằng access
// token của người dùng: mọi kiểm tra quyền, tenant và audit vẫn nằm ở API. Package này
// chỉ trả file tĩnh và một file cấu hình công khai (địa chỉ IdP, client id).
//
// dist/ không nằm trong mã nguồn (chỉ có .gitkeep): `make ui-build` tạo nó. Binary build
// khi chưa có bản build vẫn chạy; /ui/ khi đó trả 503 kèm chỉ dẫn.
package webui

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
)

// all: để .gitkeep (file giữ chỗ) cũng được nhúng; thư mục nhúng không bao giờ rỗng.
//
//go:embed all:dist
var dist embed.FS

// Config là cấu hình công khai mà trình duyệt cần để đăng nhập. Không có secret nào ở
// đây: client của giao diện là public client, bảo vệ bằng PKCE và danh sách redirect URI
// đăng ký ở IdP.
type Config struct {
	// Issuer là issuer OIDC mà TRÌNH DUYỆT tới được (cùng giá trị với `iss` trong token).
	Issuer string
	// ClientID là public client của giao diện ở IdP.
	ClientID string
}

// Handler trả về handler cho tiền tố /ui/.
func Handler(cfg Config) http.Handler {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err) // thư mục nhúng lúc biên dịch; không thể thiếu lúc chạy
	}
	return newHandler(cfg, sub)
}

const notBuilt = "Giao diện chưa được build. Chạy `make ui-build` rồi build lại binary api.\n" +
	"The web console has not been built. Run `make ui-build`, then rebuild the api binary.\n"

// newHandler phục vụ files (gốc của bản build: index.html, assets/…) ở /ui/.
func newHandler(cfg Config, files fs.FS) http.Handler {
	public, _ := json.Marshal(map[string]string{
		"issuer": strings.TrimRight(cfg.Issuer, "/"), "client_id": cfg.ClientID, "api_base": "/v1"})
	csp := contentSecurityPolicy(cfg.Issuer)
	_, statErr := fs.Stat(files, "index.html")
	built := statErr == nil
	server := http.StripPrefix("/ui/", http.FileServerFS(files))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		// Mặc định: luôn hỏi lại, không giữ bản cũ (index.html đổi theo bản phát hành của api).
		h.Set("Cache-Control", "no-cache")
		if r.URL.Path == "/ui/config.json" {
			h.Set("Content-Type", "application/json; charset=utf-8")
			_, _ = w.Write(public)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/ui/")
		// Không phục vụ file ẩn (.gitkeep) và không liệt kê thư mục.
		if hidden(name) || (name != "" && strings.HasSuffix(name, "/")) {
			http.NotFound(w, r)
			return
		}
		if !built {
			h.Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(notBuilt))
			return
		}
		// File trong assets/ mang hash nội dung trong tên: nội dung của một tên không bao
		// giờ đổi, trình duyệt giữ được lâu. (Phản hồi lỗi của file server tự bỏ header này.)
		if strings.HasPrefix(name, "assets/") {
			h.Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		server.ServeHTTP(w, r)
	})
}

// hidden: đường dẫn có thành phần bắt đầu bằng dấu chấm (file ẩn, "..").
func hidden(name string) bool {
	for _, part := range strings.Split(name, "/") {
		if strings.HasPrefix(part, ".") {
			return true
		}
	}
	return false
}

// contentSecurityPolicy: chỉ chạy script và style của chính trang (không inline, không
// CDN); chỉ gọi mạng tới chính origin này (API) và tới IdP (đổi code lấy token).
func contentSecurityPolicy(issuer string) string {
	connect := "'self'"
	if u, err := url.Parse(issuer); err == nil && u.Scheme != "" && u.Host != "" {
		connect += " " + u.Scheme + "://" + u.Host
	}
	return "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; " +
		"connect-src " + connect + "; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"
}
