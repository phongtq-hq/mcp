package loki

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// Client không dùng proxy từ biến môi trường: truy vấn (và token) chỉ đi thẳng tới địa
// chỉ người vận hành đã cấu hình. net/http bỏ qua proxy cho loopback, nên kiểm trực
// tiếp trên transport thay vì bằng một proxy giả.
func TestProxyFromEnvironmentIsIgnored(t *testing.T) {
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy"} {
		t.Setenv(name, "http://proxy.invalid:3128")
	}
	t.Setenv("NO_PROXY", "")
	c, err := NewClient("https://loki.example:3100", Options{})
	if err != nil {
		t.Fatal(err)
	}
	tr, ok := c.hc.Transport.(*http.Transport)
	if !ok || tr.Proxy != nil {
		t.Fatalf("transport phải là *http.Transport với Proxy == nil, có %T", c.hc.Transport)
	}
	if c.hc.Timeout != defaultAPITimeout || c.hc.CheckRedirect == nil {
		t.Errorf("client phải có timeout %s và không theo redirect", defaultAPITimeout)
	}
}

func TestScrubError(t *testing.T) {
	for name, tc := range map[string]struct {
		in     string
		absent []string
		keep   []string
	}{
		"userinfo có @ trong mật khẩu": {`Get "https://user:p@ss@w0rd@minio.internal:9000/bucket": EOF`,
			[]string{"p@ss", "w0rd", "user:"}, []string{"minio.internal:9000/bucket", "EOF"}},
		"query string":   {`GET https://s3.internal/chunk?X-Amz-Signature=abc123&x=1 failed`, []string{"abc123", "X-Amz"}, []string{"s3.internal/chunk", "failed"}},
		"bearer":         {`upstream said: Authorization: Bearer eyJhbGciOi.payload.sig rejected`, []string{"eyJhbGciOi", "payload.sig"}, []string{"rejected"}},
		"basic":          {`header "basic dXNlcjpwYXNz" invalid`, []string{"dXNlcjpwYXNz"}, []string{"invalid"}},
		"password=":      {`dial failed password=hunter2 host=db`, []string{"hunter2"}, []string{"host=db"}},
		"secret có nháy": {`config: client_secret: "s3 cr3t", region: eu`, []string{"s3 cr3t", "cr3t"}, []string{"region: eu"}},
		"access_key":     {`s3 error access_key_id=AKIAEXAMPLE secret_access_key=wJalrEXAMPLE`, []string{"AKIAEXAMPLE", "wJalrEXAMPLE"}, []string{"s3 error"}},
		"token json":     {`{"api_token":"tok-123","status":"bad"}`, []string{"tok-123"}, []string{"status"}},
		"lỗi LogQL nguyên": {`parse error at line 1, col 5: syntax error: unexpected $end, expecting = or != or =~ or !~`, nil,
			[]string{"parse error at line 1, col 5: syntax error: unexpected $end, expecting = or != or =~ or !~"}},
		"lỗi giới hạn nguyên": {`the query time range exceeds the limit (query length: 720h0m0s, limit: 24h0m0s)`, nil,
			[]string{"the query time range exceeds the limit (query length: 720h0m0s, limit: 24h0m0s)"}},
	} {
		got := scrubError(tc.in)
		for _, s := range tc.absent {
			if strings.Contains(got, s) {
				t.Errorf("%s: còn %q trong %q", name, s, got)
			}
		}
		for _, s := range tc.keep {
			if !strings.Contains(got, s) {
				t.Errorf("%s: mất %q trong %q", name, s, got)
			}
		}
	}
	if got := scrubError(strings.Repeat("é", 4000)); len(got) > maxErrorBytes+len("…") {
		t.Errorf("thông báo dài phải bị cắt: %d byte", len(got))
	}
}

func TestParseWindow(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"5m": 5 * time.Minute, "1h30m": 90 * time.Minute, "500ms": 500 * time.Millisecond, "90s": 90 * time.Second,
		"1d": 24 * time.Hour, "1m1ms": time.Minute + time.Millisecond,
	} {
		if got, ok := parseWindow(in); !ok || got != want {
			t.Errorf("parseWindow(%q) = %s, %v; muốn %s", in, got, ok, want)
		}
	}
	// Giá trị khổng lồ không tràn thành số nhỏ hay âm.
	for _, in := range []string{"1w", "2d", "999999999w", "999999999w999999999w999999999w", "999999999h999999999d"} {
		if got, ok := parseWindow(in); !ok || got <= maxRangeWindow {
			t.Errorf("parseWindow(%q) = %s, %v; phải lớn hơn trần", in, got, ok)
		}
	}
	for _, in := range []string{"", "5", "m", "5 m", "1.5h", "1y", "5ns", "5us", "5µs", "-5m", "+5m", "5M", "0s", "0m0s", "5m:1m", "5m,1h",
		"1234567890s", "$__interval", strings.Repeat("1s", 40)} {
		if got, ok := parseWindow(in); ok {
			t.Errorf("parseWindow(%q) phải bị từ chối, nhận %s", in, got)
		}
	}
}

// labelSet: quá nhiều label và tên trùng nhau sau khi cắt được báo ở cờ, label sau không
// ghi đè label trước; __tenant_id__ bị bỏ lặng lẽ.
func TestLabelSet(t *testing.T) {
	if out, truncated := labelSet(map[string]string{"app": "api", tenantLabel: "team-a"}); truncated || len(out) != 1 || out["app"] != "api" {
		t.Errorf("label thường: %v truncated=%v", out, truncated)
	}
	many := map[string]string{}
	for i := range maxLabels + 5 {
		many[strings.Repeat("l", 3)+string(rune('a'+i/26))+string(rune('a'+i%26))] = "v"
	}
	if out, truncated := labelSet(many); !truncated || len(out) != maxLabels {
		t.Errorf("quá nhiều label: %d truncated=%v", len(out), truncated)
	}
	prefix := strings.Repeat("n", maxLabelNameBytes)
	out, truncated := labelSet(map[string]string{prefix + "_a": "first", prefix + "_b": "second", "z": "kept"})
	if !truncated || len(out) != 2 || out["z"] != "kept" || out[prefix+"…"] != "first" {
		t.Errorf("tên trùng sau khi cắt: %d label, truncated=%v", len(out), truncated)
	}
}
