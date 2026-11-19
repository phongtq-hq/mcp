// Command integration-github: managed MCP server chỉ đọc cho GitHub.
//
// Chỉ nhận request mang ngữ cảnh do MCP Gateway ký (HQ_SERVICE_SIGNING_KEY); không
// nhận token của người dùng. Địa chỉ API (HQ_GITHUB_API_URL), token
// (HQ_GITHUB_TOKEN_FILE) và allowlist repository (HQ_GITHUB_REPOSITORIES) do người
// vận hành managed server đặt: đây không phải giá trị do tenant nhập, nên địa chỉ
// không đi qua SSRF guard, và không tool nào đổi được chúng.
package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/google/uuid"

	"github.com/hqgroups/mcp/internal/integration/github"
	"github.com/hqgroups/mcp/internal/platform/process"
	"github.com/hqgroups/mcp/internal/shared/signedctx"
)

const defaultAPIURL = "https://api.github.com"

func main() {
	os.Exit(process.Run(process.Options{
		Name:        "integration-github",
		DefaultAddr: ":8094",
		NoDatabase:  true,
		Routes:      routes,
	}))
}

// Token nằm trong file chứ không trong biến môi trường: môi trường của process dễ
// lọt vào log khởi động, dump và /proc hơn một file có quyền đọc hẹp.
func readToken(path string) (string, error) {
	if path == "" {
		return "", errors.New("HQ_GITHUB_TOKEN_FILE is required (file chứa token GitHub chỉ đọc do người vận hành cấp)")
	}
	f, err := os.Open(path) //nolint:gosec // đường dẫn do người vận hành cấu hình
	if err != nil {
		return "", errors.New("HQ_GITHUB_TOKEN_FILE: không đọc được file")
	}
	defer func() { _ = f.Close() }()
	const maxToken = 16 << 10
	b, err := io.ReadAll(io.LimitReader(f, maxToken+1))
	if err != nil || len(b) > maxToken {
		return "", errors.New("HQ_GITHUB_TOKEN_FILE: file quá lớn hoặc không đọc được")
	}
	// File chỉ có khoảng trắng / xuống dòng là file rỗng: không chạy ẩn danh.
	token := strings.TrimSpace(string(b))
	if token == "" {
		return "", errors.New("HQ_GITHUB_TOKEN_FILE: file không chứa token")
	}
	return token, nil
}

// settings là cấu hình đã kiểm của process.
type settings struct {
	client *github.Client
	signer *signedctx.Signer
	orgID  uuid.UUID
}

// configure đọc và kiểm toàn bộ cấu hình từ getenv (và file token). Tách khỏi routes để
// test được mà không cần chạy process.
func configure(getenv func(string) string) (settings, error) {
	apiURL := getenv("HQ_GITHUB_API_URL")
	if apiURL == "" {
		apiURL = defaultAPIURL
	}
	repos := github.SplitRepositories(getenv("HQ_GITHUB_REPOSITORIES"))
	if len(repos) == 0 {
		return settings{}, errors.New("HQ_GITHUB_REPOSITORIES is required (allowlist owner/repo, phân tách dấu phẩy)")
	}
	token, err := readToken(getenv("HQ_GITHUB_TOKEN_FILE"))
	if err != nil {
		return settings{}, err
	}
	client, err := github.NewClient(apiURL, github.Options{
		Token:        token,
		Repositories: repos,
		// http chỉ dùng được trên máy phát triển (GitHub giả lập); ngoài local luôn https.
		AllowInsecureHTTP: getenv("HQ_ENV") == "local",
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
	// Token và allowlist này phục vụ đúng một organization của platform.
	orgID, err := uuid.Parse(getenv("HQ_GITHUB_ORG_ID"))
	if err != nil || orgID == uuid.Nil {
		return settings{}, errors.New("HQ_GITHUB_ORG_ID is required (id của organization sở hữu tài khoản GitHub này)")
	}
	return settings{client: client, signer: signer, orgID: orgID}, nil
}

func routes(_ context.Context, mux *http.ServeMux, _ process.Deps) error {
	cfg, err := configure(os.Getenv)
	if err != nil {
		return err
	}
	mux.Handle("/mcp", github.Handler(cfg.client, cfg.signer, cfg.orgID))
	return nil
}
