package process

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIsHealthcheck(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"api"}, false},
		{[]string{"api", "healthcheck"}, true},
		{[]string{"api", "healthcheck", "x"}, false},
		{[]string{"api", "-healthcheck"}, false},
		{nil, false},
	} {
		if got := isHealthcheck(tc.args); got != tc.want {
			t.Errorf("isHealthcheck(%v) = %v, muốn %v", tc.args, got, tc.want)
		}
	}
}

func TestLoopbackTarget(t *testing.T) {
	for in, want := range map[string]string{
		":8080":          "127.0.0.1:8080",
		"0.0.0.0:8081":   "127.0.0.1:8081",
		"[::]:8082":      "127.0.0.1:8082",
		"127.0.0.1:9000": "127.0.0.1:9000",
		"10.1.2.3:9000":  "10.1.2.3:9000",
	} {
		got, err := loopbackTarget(in)
		if err != nil || got != want {
			t.Errorf("loopbackTarget(%q) = %q, %v; muốn %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "8080", "localhost", ":"} {
		if _, err := loopbackTarget(in); err == nil {
			t.Errorf("loopbackTarget(%q) phải lỗi", in)
		}
	}
}

func TestRunHealthcheck(t *testing.T) {
	status := http.StatusOK
	mux := http.NewServeMux()
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	env := func(k string) string {
		if k == "HQ_HTTP_ADDR" {
			return ":" + port
		}
		return ""
	}

	if code := runHealthcheck(env, ":1"); code != 0 {
		t.Errorf("sẵn sàng: mã thoát %d, muốn 0", code)
	}
	// Không có HQ_HTTP_ADDR thì dùng địa chỉ mặc định của binary.
	if code := runHealthcheck(func(string) string { return "" }, ":"+port); code != 0 {
		t.Errorf("địa chỉ mặc định: mã thoát %d, muốn 0", code)
	}
	// Phụ thuộc chưa sẵn sàng (503) là không khoẻ: Compose không được coi container là lên.
	status = http.StatusServiceUnavailable
	if code := runHealthcheck(env, ":1"); code != 1 {
		t.Errorf("503: mã thoát %d, muốn 1", code)
	}
	// Redirect không được theo: chỉ đúng 200 của /readyz mới là sẵn sàng.
	status = http.StatusFound
	if err := probeReady(context.Background(), ":"+port); err == nil {
		t.Error("302 phải bị coi là chưa sẵn sàng")
	}
	srv.Close()
	if code := runHealthcheck(env, ":1"); code != 1 {
		t.Errorf("không có ai nghe: mã thoát %d, muốn 1", code)
	}
}
