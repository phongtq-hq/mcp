// Command integration-pricing-engine: managed MCP server chỉ đọc cho Pricing Engine (PE).
//
// Chỉ nhận request mang ngữ cảnh do MCP Gateway ký (HQ_SERVICE_SIGNING_KEY); không
// nhận token của người dùng. Địa chỉ PE (HQ_PRICING_ENGINE_URL) và API key của PE
// (HQ_PRICING_ENGINE_API_KEY_FILE) do người vận hành managed server đặt: đây không phải
// giá trị do tenant nhập, nên URL không đi qua SSRF guard, và không tool nào đổi được
// chúng.
package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/google/uuid"

	"github.com/hqgroups/mcp/internal/integration/pricingengine"
	"github.com/hqgroups/mcp/internal/platform/config"
	"github.com/hqgroups/mcp/internal/platform/process"
	"github.com/hqgroups/mcp/internal/shared/signedctx"
)

func main() {
	os.Exit(process.Run(process.Options{
		Name:        "integration-pricing-engine",
		DefaultAddr: ":8095",
		NoDatabase:  true,
		Routes:      routes,
	}))
}

// Khoá nằm trong file chứ không trong biến môi trường (trừ ngoại lệ ở local, xem apiKeyFrom):
// môi trường của process dễ lọt
// vào log khởi động, dump và /proc hơn một file có quyền đọc hẹp. Thông báo lỗi không
// chứa nội dung file.
func readAPIKey(path string, strictMode bool) (string, error) {
	if path == "" {
		return "", errors.New("HQ_PRICING_ENGINE_API_KEY_FILE is required (đường dẫn file chứa API key của PE)")
	}
	f, err := os.Open(path) //nolint:gosec // đường dẫn do người vận hành cấu hình
	if err != nil {
		return "", errors.New("HQ_PRICING_ENGINE_API_KEY_FILE: không đọc được file")
	}
	defer func() { _ = f.Close() }()
	// Kiểm trên chính file đã mở (không Stat lại theo đường dẫn): ngoài local, khoá mà nhóm
	// hoặc người khác trên máy đọc được là cấu hình sai, không phải cảnh báo.
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("HQ_PRICING_ENGINE_API_KEY_FILE: phải là một file thường")
	}
	if strictMode && info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("HQ_PRICING_ENGINE_API_KEY_FILE: file cho nhóm hoặc người khác đọc; đặt quyền 0600 (hoặc 0400) cho user của process")
	}
	const maxKey = 4 << 10
	b, err := io.ReadAll(io.LimitReader(f, maxKey+1))
	if err != nil || len(b) > maxKey {
		return "", errors.New("HQ_PRICING_ENGINE_API_KEY_FILE: file quá lớn hoặc không đọc được")
	}
	key := strings.TrimSpace(string(b))
	if key == "" {
		return "", errors.New("HQ_PRICING_ENGINE_API_KEY_FILE: file rỗng hoặc chỉ có khoảng trắng")
	}
	if strings.ContainsFunc(key, func(r rune) bool { return r <= 0x20 || r >= 0x7f }) {
		return "", errors.New("HQ_PRICING_ENGINE_API_KEY_FILE: khoá chứa khoảng trắng hoặc ký tự không hợp lệ (file phải chứa đúng một khoá trên một dòng)")
	}
	return key, nil
}

// apiKeyFrom lấy API key của PE. Đường chuẩn là file (HQ_PRICING_ENGINE_API_KEY_FILE). Riêng ở
// local, khoá được nhận thẳng từ biến HQ_PRICING_ENGINE_API_KEY cho tiện chạy thử trên máy
// phát triển; ngoài local biến đó làm process từ chối khởi động, để không ai mang cách cấu
// hình này lên staging / production. Đặt cả hai là cấu hình mơ hồ và bị từ chối.
func apiKeyFrom(getenv func(string) string, env string) (string, error) {
	inline := getenv("HQ_PRICING_ENGINE_API_KEY")
	path := getenv("HQ_PRICING_ENGINE_API_KEY_FILE")
	if inline == "" {
		return readAPIKey(path, env != config.EnvLocal)
	}
	if env != config.EnvLocal {
		return "", errors.New("HQ_PRICING_ENGINE_API_KEY chỉ được nhận khi HQ_ENV=local; ở môi trường khác dùng HQ_PRICING_ENGINE_API_KEY_FILE")
	}
	if path != "" {
		return "", errors.New("đặt HQ_PRICING_ENGINE_API_KEY hoặc HQ_PRICING_ENGINE_API_KEY_FILE, không đặt cả hai")
	}
	key := strings.TrimSpace(inline)
	if key == "" || strings.ContainsFunc(key, func(r rune) bool { return r <= 0x20 || r >= 0x7f }) {
		return "", errors.New("HQ_PRICING_ENGINE_API_KEY: khoá rỗng, hoặc chứa khoảng trắng hay ký tự không hợp lệ")
	}
	return key, nil
}

// settings là cấu hình của process sau khi đọc và kiểm.
type settings struct {
	client *pricingengine.Client
	signer *signedctx.Signer
	orgID  uuid.UUID
}

// load đọc cấu hình từ môi trường. Mọi thiếu sót là lỗi khởi động; thông báo lỗi không
// chứa giá trị của khoá.
func load(getenv func(string) string) (settings, error) {
	// HQ_ENV quyết định có nhận http tới loopback và có kiểm quyền file khoá hay không, nên
	// ở đây nó phải được đặt tường
	// minh: mặc định "local" của process sẽ lặng lẽ nới hai kiểm tra đó (http tới loopback,
	// quyền của file khoá).
	env := getenv("HQ_ENV")
	switch env {
	case config.EnvLocal, config.EnvStaging, config.EnvProduction:
	case "":
		return settings{}, errors.New("HQ_ENV is required (local, staging hoặc production): việc bắt buộc https tới Pricing Engine phụ thuộc vào nó")
	default:
		return settings{}, errors.New("HQ_ENV phải là local, staging hoặc production")
	}
	apiURL := getenv("HQ_PRICING_ENGINE_URL")
	if apiURL == "" {
		return settings{}, errors.New("HQ_PRICING_ENGINE_URL is required (địa chỉ Pricing Engine do người vận hành cấu hình)")
	}
	apiKey, err := apiKeyFrom(getenv, env)
	if err != nil {
		return settings{}, err
	}
	client, err := pricingengine.NewClient(apiURL, pricingengine.Options{
		APIKey: apiKey,
		// Khoá và giá mua vào đi trên đường nối này: luôn https, trừ http tới host loopback
		// ở local (PE chạy trên chính máy phát triển).
		AllowLoopbackHTTP: env == config.EnvLocal,
	})
	if err != nil {
		return settings{}, err
	}
	key, err := signedctx.ParseKey(getenv("HQ_SERVICE_SIGNING_KEY"))
	if err != nil {
		return settings{}, errors.New("HQ_SERVICE_SIGNING_KEY: " + err.Error())
	}
	signer, err := signedctx.NewSigner(key)
	if err != nil {
		return settings{}, err
	}
	// PE này phục vụ đúng một organization.
	orgID, err := uuid.Parse(getenv("HQ_PRICING_ENGINE_ORG_ID"))
	if err != nil || orgID == uuid.Nil {
		return settings{}, errors.New("HQ_PRICING_ENGINE_ORG_ID is required (id của organization sở hữu Pricing Engine này)")
	}
	return settings{client: client, signer: signer, orgID: orgID}, nil
}

func routes(_ context.Context, mux *http.ServeMux, _ process.Deps) error {
	s, err := load(os.Getenv)
	if err != nil {
		return err
	}
	mux.Handle("/mcp", pricingengine.Handler(s.client, s.signer, s.orgID))
	return nil
}
