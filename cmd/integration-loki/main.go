// Command integration-loki: managed MCP server chỉ đọc cho Grafana Loki.
//
// Chỉ nhận request mang ngữ cảnh do MCP Gateway ký (kiểm bằng khoá công khai HQ_SERVICE_VERIFY_KEY); không
// nhận token của người dùng. Địa chỉ Loki (HQ_LOKI_URL), bearer token
// (HQ_LOKI_BEARER_TOKEN_FILE, tuỳ chọn) và tenant của Loki (HQ_LOKI_TENANT_ID, tuỳ
// chọn, gửi trong header X-Scope-OrgID) do người vận hành managed server đặt: đây
// không phải giá trị do tenant nhập, nên URL không đi qua SSRF guard, và không tool
// nào đổi được chúng.
package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/google/uuid"

	"github.com/hqgroups/mcp/internal/integration/loki"
	"github.com/hqgroups/mcp/internal/integration/serve"
	"github.com/hqgroups/mcp/internal/platform/config"
	"github.com/hqgroups/mcp/internal/platform/process"
	"github.com/hqgroups/mcp/internal/shared/signedctx"
)

func main() {
	os.Exit(process.Run(process.Options{
		Name:        "integration-loki",
		DefaultAddr: ":8092",
		NoDatabase:  true,
		Routes:      routes,
	}))
}

// Token nằm trong file chứ không trong biến môi trường: môi trường của process dễ
// lọt vào log khởi động, dump và /proc hơn một file có quyền đọc hẹp.
func readToken(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	f, err := os.Open(path) //nolint:gosec // đường dẫn do người vận hành cấu hình
	if err != nil {
		return "", errors.New("HQ_LOKI_BEARER_TOKEN_FILE: không đọc được file")
	}
	defer func() { _ = f.Close() }()
	const maxToken = 16 << 10
	b, err := io.ReadAll(io.LimitReader(f, maxToken+1))
	if err != nil || len(b) > maxToken {
		return "", errors.New("HQ_LOKI_BEARER_TOKEN_FILE: file quá lớn hoặc không đọc được")
	}
	// Một file được cấu hình mà không cho ra token dùng được là lỗi cấu hình, không phải
	// "không có token": nếu bỏ qua, server sẽ lặng lẽ gọi Loki không xác thực.
	token := strings.TrimSpace(string(b))
	if token == "" {
		return "", errors.New("HQ_LOKI_BEARER_TOKEN_FILE: file rỗng hoặc chỉ có khoảng trắng")
	}
	if strings.ContainsFunc(token, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "", errors.New("HQ_LOKI_BEARER_TOKEN_FILE: token chứa ký tự điều khiển (file phải chứa đúng một token trên một dòng)")
	}
	return token, nil
}

// settings là cấu hình của process sau khi đọc và kiểm.
type settings struct {
	client   *loki.Client
	verifier *signedctx.Verifier
	orgID    uuid.UUID
}

// load đọc cấu hình từ môi trường. Mọi thiếu sót là lỗi khởi động; thông báo lỗi không
// chứa giá trị của token, khoá hay tenant.
func load(getenv func(string) string) (settings, error) {
	// HQ_ENV quyết định có bắt buộc https hay không, nên ở đây nó phải được đặt tường
	// minh: mặc định "local" của process sẽ lặng lẽ cho phép http.
	env := getenv("HQ_ENV")
	switch env {
	case config.EnvLocal, config.EnvStaging, config.EnvProduction:
	case "":
		return settings{}, errors.New("HQ_ENV is required (local, staging hoặc production): việc bắt buộc https tới Loki phụ thuộc vào nó")
	default:
		return settings{}, errors.New("HQ_ENV phải là local, staging hoặc production")
	}
	apiURL := getenv("HQ_LOKI_URL")
	if apiURL == "" {
		return settings{}, errors.New("HQ_LOKI_URL is required (địa chỉ Loki do người vận hành cấu hình)")
	}
	token, err := readToken(getenv("HQ_LOKI_BEARER_TOKEN_FILE"))
	if err != nil {
		return settings{}, err
	}
	var multi bool
	switch getenv("HQ_LOKI_ALLOW_MULTI_TENANT") {
	case "", "0":
	case "1":
		multi = true
	default:
		return settings{}, errors.New("HQ_LOKI_ALLOW_MULTI_TENANT phải là 1 hoặc 0")
	}
	client, err := loki.NewClient(apiURL, loki.Options{
		BearerToken:      token,
		TenantID:         getenv("HQ_LOKI_TENANT_ID"),
		AllowMultiTenant: multi,
		// Dòng log đi trên đường nối này: ngoài local chỉ nhận https.
		RequireHTTPS: env != config.EnvLocal,
	})
	if err != nil {
		return settings{}, err
	}
	verifier, err := serve.VerifierFromEnv(getenv)
	if err != nil {
		return settings{}, err
	}
	// Loki này phục vụ đúng một organization.
	orgID, err := uuid.Parse(getenv("HQ_LOKI_ORG_ID"))
	if err != nil || orgID == uuid.Nil {
		return settings{}, errors.New("HQ_LOKI_ORG_ID is required (id của organization sở hữu Loki này)")
	}
	return settings{client: client, verifier: verifier, orgID: orgID}, nil
}

func routes(ctx context.Context, mux *http.ServeMux, _ process.Deps) error {
	s, err := load(os.Getenv)
	if err != nil {
		return err
	}
	// Số tenant của Loki mà process này đọc; không ghi giá trị.
	slog.InfoContext(ctx, "loki: cấu hình tenant", "loki_tenants", s.client.Tenants())
	mux.Handle("/mcp", loki.Handler(s.client, s.verifier, s.orgID))
	return nil
}
