// Command integration-kubernetes: managed MCP server chỉ đọc cho một cluster Kubernetes.
//
// Chỉ nhận request mang ngữ cảnh do MCP Gateway ký (kiểm bằng khoá công khai HQ_SERVICE_VERIFY_KEY); không
// nhận token của người dùng. Địa chỉ apiserver (HQ_KUBERNETES_URL), CA
// (HQ_KUBERNETES_CA_FILE), bearer token của service account
// (HQ_KUBERNETES_BEARER_TOKEN_FILE) và phạm vi namespace (HQ_KUBERNETES_NAMESPACES, bắt buộc)
// do người vận hành managed server đặt: đây không phải giá trị do tenant nhập, nên
// không đi qua SSRF guard, và không tool nào đổi được chúng.
package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"

	"github.com/google/uuid"

	"github.com/hqgroups/mcp/internal/integration/kubernetes"
	"github.com/hqgroups/mcp/internal/integration/serve"
	"github.com/hqgroups/mcp/internal/platform/config"
	"github.com/hqgroups/mcp/internal/platform/process"
	"github.com/hqgroups/mcp/internal/shared/signedctx"
)

func main() {
	os.Exit(process.Run(process.Options{
		Name:        "integration-kubernetes",
		DefaultAddr: ":8093",
		NoDatabase:  true,
		Routes:      routes,
	}))
}

// readFile đọc một file cấu hình nhỏ (token, CA). Token nằm trong file chứ không
// trong biến môi trường: môi trường của process dễ lọt vào log khởi động, dump và
// /proc hơn một file có quyền đọc hẹp. Thông báo lỗi chỉ nêu tên biến, không nêu nội dung.
func readFile(getenv func(string) string, env string, limit int64) ([]byte, error) {
	path := getenv(env)
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path) //nolint:gosec // đường dẫn do người vận hành cấu hình
	if err != nil {
		return nil, errors.New(env + ": không đọc được file")
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(b)) > limit || len(bytes.TrimSpace(b)) == 0 {
		return nil, errors.New(env + ": file rỗng, quá lớn hoặc không đọc được")
	}
	return b, nil
}

// settings là cấu hình đã kiểm của process.
type settings struct {
	client   *kubernetes.Client
	verifier *signedctx.Verifier
	orgID    uuid.UUID
	// scope mô tả phạm vi namespace cho log khởi động; không chứa credential.
	scope      string
	namespaces []string
}

// loadSettings đọc và kiểm mọi biến cấu hình. Không có I/O nào ngoài đọc file token và
// CA; lỗi không bao giờ chứa nội dung file.
func loadSettings(getenv func(string) string, env string) (settings, error) {
	apiURL := getenv("HQ_KUBERNETES_URL")
	if apiURL == "" {
		return settings{}, errors.New("HQ_KUBERNETES_URL is required (địa chỉ kube-apiserver do người vận hành cấu hình)")
	}
	// Cluster này phục vụ đúng một organization.
	orgID, err := uuid.Parse(getenv("HQ_KUBERNETES_ORG_ID"))
	if err != nil || orgID == uuid.Nil {
		return settings{}, errors.New("HQ_KUBERNETES_ORG_ID is required (id của organization sở hữu cluster này)")
	}
	// Phạm vi namespace là bắt buộc và tường minh: thiếu biến không được thành "mọi namespace".
	namespaces, all, err := kubernetes.ParseNamespaces(getenv("HQ_KUBERNETES_NAMESPACES"))
	if err != nil {
		return settings{}, errors.New("HQ_KUBERNETES_NAMESPACES: " + err.Error())
	}
	scope := "mọi namespace mà service account đọc được (*)"
	if !all {
		scope = strconv.Itoa(len(namespaces)) + " namespace trong allowlist"
	}
	token, err := readFile(getenv, "HQ_KUBERNETES_BEARER_TOKEN_FILE", 16<<10)
	if err != nil {
		return settings{}, err
	}
	ca, err := readFile(getenv, "HQ_KUBERNETES_CA_FILE", 1<<20)
	if err != nil {
		return settings{}, err
	}
	client, err := kubernetes.NewClient(apiURL, kubernetes.Options{
		BearerToken: string(token), CAPEM: ca, Namespaces: namespaces, AllowHTTP: env == config.EnvLocal,
	})
	if err != nil {
		return settings{}, err
	}
	verifier, err := serve.VerifierFromEnv(getenv)
	if err != nil {
		return settings{}, err
	}
	return settings{client: client, verifier: verifier, orgID: orgID, scope: scope, namespaces: namespaces}, nil
}

func routes(ctx context.Context, mux *http.ServeMux, deps process.Deps) error {
	cfg, err := loadSettings(os.Getenv, deps.Config.Env)
	if err != nil {
		return err
	}
	slog.InfoContext(ctx, "integration-kubernetes: phạm vi namespace", "scope", cfg.scope, "namespaces", cfg.namespaces)
	mux.Handle("/mcp", kubernetes.Handler(cfg.client, cfg.verifier, cfg.orgID))
	return nil
}
