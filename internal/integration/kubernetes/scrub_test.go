package kubernetes

import (
	"net/http"
	"strings"
	"testing"
)

// scrubError che các dạng credential có hình thù rõ. Đây là lớp giảm thiểu: ca cuối
// ghi nhận rằng một secret trần trong câu KHÔNG bị che.
func TestScrubError(t *testing.T) {
	for name, tc := range map[string]struct {
		in     string
		gone   []string
		remain []string
	}{
		"userinfo đơn giản":         {`Get "https://puller:s3cr3t@registry.example/v2/": denied`, []string{"s3cr3t", "puller"}, []string{"registry.example", "denied"}},
		"mật khẩu chứa / và @":      {`dial postgres://app:p@ss/w0rd@db.internal:5432/app failed`, []string{"p@ss", "w0rd"}, []string{"db.internal", "failed"}},
		"query string":              {`GET https://api.example/v1?access_token=abc123&x=1 returned 500`, []string{"abc123"}, []string{"api.example", "returned 500"}},
		"bearer":                    {`header Authorization: Bearer abcdef0123456789 rejected`, []string{"abcdef0123456789"}, []string{"rejected"}},
		"basic":                     {`Authorization: Basic dXNlcjpodW50ZXIy was refused`, []string{"dXNlcjpodW50ZXIy"}, []string{"refused"}},
		"jwt trần":                  {`got eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJzeXN0ZW0ifQ.c2lnbmF0dXJl from upstream`, []string{"eyJhbGci", "c2lnbmF0dXJl"}, []string{"from upstream"}},
		"password=":                 {`connect failed password=hunter2 host=db`, []string{"hunter2"}, []string{"host=db"}},
		"password: có khoảng trắng": {`config error, password: hunter2, retrying`, []string{"hunter2"}, []string{"retrying"}},
		"--db-pass=":                {`exec: "/bin/web --db-pass=hunter2secret --port=80": not found`, []string{"hunter2secret"}, []string{"--port=80", "not found"}},
		"tiền tố và hoa thường":     {`env POSTGRES_PASSWORD=Sup3r and DB_PASSWD=x1y2 set`, []string{"Sup3r", "x1y2"}, []string{"set"}},
		"giá trị trong ngoặc kép":   {`secret="a b c d" then token: 'q r s' end`, []string{"a b c d", "q r s"}, []string{"end"}},
		"api-key, apikey":           {`api-key=AAA111 apikey=BBB222 api_key=CCC333 ok`, []string{"AAA111", "BBB222", "CCC333"}, []string{"ok"}},
		"access-key, private-key":   {`access-key=AAA111 private_key=BBB222 ok`, []string{"AAA111", "BBB222"}, []string{"ok"}},
		"client_secret, auth_token": {`client_secret=AAA111 auth_token=BBB222 ok`, []string{"AAA111", "BBB222"}, []string{"ok"}},
		"văn bản thường giữ nguyên": {`Back-off restarting failed container app in pod web-0`, nil, []string{"Back-off restarting failed container app in pod web-0"}},
	} {
		out := scrubError(tc.in, 4096)
		for _, s := range tc.gone {
			if strings.Contains(out, s) {
				t.Errorf("%s: còn %q trong %q", name, s, out)
			}
		}
		for _, s := range tc.remain {
			if !strings.Contains(out, s) {
				t.Errorf("%s: mất %q trong %q", name, s, out)
			}
		}
	}
	// Giới hạn đã biết: không có hình thù thì không che được.
	if out := scrubError(`secret "db-credentials" not found; tried hunter2 twice`, 4096); !strings.Contains(out, "hunter2") {
		t.Errorf("ca ghi nhận giới hạn đã đổi hành vi: %q", out)
	}
	if out := scrubError(strings.Repeat("x", 1000), 100); len(out) > 104 {
		t.Errorf("phải cắt về giới hạn: %d byte", len(out))
	}
}

// Client không dùng proxy từ biến môi trường (HTTPS_PROXY…): token chỉ đi thẳng tới địa
// chỉ đã cấu hình. Kiểm trên transport vì http.ProxyFromEnvironment bỏ qua proxy cho
// loopback và nhớ biến môi trường từ lần đọc đầu, nên một test hành vi với máy chủ
// httptest sẽ luôn qua dù Proxy có được đặt hay không.
func TestNoEnvironmentProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://proxy.invalid:3128")
	t.Setenv("HTTP_PROXY", "http://proxy.invalid:3128")
	c, err := NewClient("https://kube.example:6443", Options{BearerToken: "t"})
	if err != nil {
		t.Fatal(err)
	}
	tr, ok := c.hc.Transport.(*http.Transport)
	if !ok || tr.Proxy != nil {
		t.Fatalf("transport phải có Proxy = nil, nhận %T", c.hc.Transport)
	}
	if c.hc.CheckRedirect == nil || c.hc.Timeout <= 0 || tr.TLSClientConfig == nil || tr.TLSClientConfig.InsecureSkipVerify {
		t.Error("client phải không theo redirect, có timeout và xác minh TLS")
	}
}
