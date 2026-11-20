package process

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"
)

// HealthcheckArg là đối số duy nhất biến một binary của platform thành lệnh tự kiểm:
// `api healthcheck` hỏi /readyz của chính process đang chạy trong cùng container rồi
// thoát 0 (sẵn sàng) hoặc 1. Image chạy thật là distroless, không có shell hay curl, nên
// `healthcheck` của Docker / Compose không có công cụ nào khác để gọi.
const HealthcheckArg = "healthcheck"

const healthcheckTimeout = 3 * time.Second

// isHealthcheck báo binary được gọi ở chế độ tự kiểm.
func isHealthcheck(args []string) bool {
	return len(args) == 2 && args[1] == HealthcheckArg
}

// runHealthcheck hỏi /readyz ở địa chỉ process lắng nghe (HQ_HTTP_ADDR hoặc mặc định của
// binary) qua loopback. Trả về mã thoát. Không đọc cấu hình nào khác và không in thân
// phản hồi: lệnh này chạy vài giây một lần trong suốt đời container.
func runHealthcheck(getenv func(string) string, defaultAddr string) int {
	addr := getenv("HQ_HTTP_ADDR")
	if addr == "" {
		addr = defaultAddr
	}
	ctx, cancel := context.WithTimeout(context.Background(), healthcheckTimeout)
	defer cancel()
	if err := probeReady(ctx, addr); err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	return 0
}

// probeReady gọi GET /readyz ở cổng của listenAddr trên loopback; chỉ 200 là sẵn sàng.
func probeReady(ctx context.Context, listenAddr string) error {
	target, err := loopbackTarget(listenAddr)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+target+"/readyz", nil)
	if err != nil {
		return err
	}
	// Client riêng, không theo redirect: đích luôn là chính process này trên loopback.
	hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("không gọi được /readyz: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("/readyz trả %d", resp.StatusCode)
	}
	return nil
}

// loopbackTarget đổi địa chỉ lắng nghe (":8080", "0.0.0.0:8080", "[::]:8080") thành địa
// chỉ gọi được từ trong container. Địa chỉ lắng nghe cụ thể (không phải "mọi giao diện")
// được giữ nguyên, vì khi đó process không nghe trên loopback.
func loopbackTarget(listenAddr string) (string, error) {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil || port == "" {
		return "", fmt.Errorf("địa chỉ lắng nghe không hợp lệ: %q", listenAddr)
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port), nil
}
