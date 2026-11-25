// Command integration-prometheus: managed MCP server chỉ đọc cho Prometheus.
//
// Chỉ nhận request mang ngữ cảnh do MCP Gateway ký (kiểm bằng khoá công khai HQ_SERVICE_VERIFY_KEY); không
// nhận token của người dùng. Địa chỉ Prometheus (HQ_PROMETHEUS_URL) và bearer token
// (HQ_PROMETHEUS_BEARER_TOKEN_FILE, tuỳ chọn) do người vận hành managed server đặt:
// đây không phải URL do tenant nhập, nên không đi qua SSRF guard, và không tool nào
// đổi được nó.
package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"

	"github.com/google/uuid"

	"github.com/hqgroups/mcp/internal/integration/prometheus"
	"github.com/hqgroups/mcp/internal/integration/serve"
	"github.com/hqgroups/mcp/internal/platform/process"
)

func main() {
	os.Exit(process.Run(process.Options{
		Name:        "integration-prometheus",
		DefaultAddr: ":8091",
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
		return "", errors.New("HQ_PROMETHEUS_BEARER_TOKEN_FILE: không đọc được file")
	}
	defer func() { _ = f.Close() }()
	const maxToken = 16 << 10
	b, err := io.ReadAll(io.LimitReader(f, maxToken+1))
	if err != nil || len(b) > maxToken || len(b) == 0 {
		return "", errors.New("HQ_PROMETHEUS_BEARER_TOKEN_FILE: file rỗng, quá lớn hoặc không đọc được")
	}
	return string(b), nil
}

func routes(_ context.Context, mux *http.ServeMux, _ process.Deps) error {
	apiURL := os.Getenv("HQ_PROMETHEUS_URL")
	if apiURL == "" {
		return errors.New("HQ_PROMETHEUS_URL is required (địa chỉ Prometheus do người vận hành cấu hình)")
	}
	token, err := readToken(os.Getenv("HQ_PROMETHEUS_BEARER_TOKEN_FILE"))
	if err != nil {
		return err
	}
	client, err := prometheus.NewClient(apiURL, prometheus.Options{BearerToken: token})
	if err != nil {
		return err
	}
	verifier, err := serve.VerifierFromEnv(os.Getenv)
	if err != nil {
		return err
	}
	// Prometheus này phục vụ đúng một organization.
	orgID, err := uuid.Parse(os.Getenv("HQ_PROMETHEUS_ORG_ID"))
	if err != nil || orgID == uuid.Nil {
		return errors.New("HQ_PROMETHEUS_ORG_ID is required (id của organization sở hữu Prometheus này)")
	}
	mux.Handle("/mcp", prometheus.Handler(client, verifier, orgID))
	return nil
}
