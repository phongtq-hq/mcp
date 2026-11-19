package webui

import (
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
)

// page là phần của một phản hồi mà test cần (thân đã đọc và đóng trong get).
type page struct {
	StatusCode int
	Header     http.Header
	Path       string // đường dẫn sau khi theo redirect
}

func get(t *testing.T, srv *httptest.Server, path string) (page, string) {
	t.Helper()
	res, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	return page{StatusCode: res.StatusCode, Header: res.Header, Path: res.Request.URL.Path}, string(b)
}

// builtFS có hình dạng của một bản build Vite: index.html ở gốc, mọi thứ khác trong assets/.
func builtFS() fstest.MapFS {
	return fstest.MapFS{
		".gitkeep":                {Data: []byte("")},
		"index.html":              {Data: []byte(`<!doctype html><script type="module" src="/ui/assets/index-abc.js"></script>`)},
		"assets/index-abc.js":     {Data: []byte("console.log(1)")},
		"assets/index-abc.css":    {Data: []byte("body{}")},
		"assets/.hidden-chunk.js": {Data: []byte("x")},
	}
}

func TestHandler(t *testing.T) {
	cfg := Config{Issuer: "https://idp.example.test/realms/hq/", ClientID: "hq-web"}
	srv := httptest.NewServer(newHandler(cfg, builtFS()))
	defer srv.Close()

	// Mọi file của giao diện có mặt và mang header bảo mật.
	for _, path := range []string{"/ui/", "/ui/assets/index-abc.js", "/ui/assets/index-abc.css"} {
		res, body := get(t, srv, path)
		if res.StatusCode != http.StatusOK || body == "" {
			t.Fatalf("%s: status %d, %d byte", path, res.StatusCode, len(body))
		}
		csp := res.Header.Get("Content-Security-Policy")
		for _, want := range []string{"default-src 'none'", "script-src 'self'", "style-src 'self'", "connect-src 'self' https://idp.example.test", "frame-ancestors 'none'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("%s: CSP thiếu %q: %s", path, want, csp)
			}
		}
		if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") || strings.Contains(csp, "*") {
			t.Errorf("%s: CSP bị nới: %s", path, csp)
		}
		if res.Header.Get("X-Content-Type-Options") != "nosniff" || res.Header.Get("X-Frame-Options") != "DENY" {
			t.Errorf("%s: thiếu header bảo mật: %v", path, res.Header)
		}
	}

	// Chỉ file mang hash trong tên (assets/) được giữ lâu; index.html luôn được hỏi lại.
	if res, _ := get(t, srv, "/ui/"); res.Header.Get("Cache-Control") != "no-cache" {
		t.Errorf("index.html: Cache-Control = %q", res.Header.Get("Cache-Control"))
	}
	if res, _ := get(t, srv, "/ui/assets/index-abc.js"); !strings.Contains(res.Header.Get("Cache-Control"), "immutable") {
		t.Errorf("asset: Cache-Control = %q", res.Header.Get("Cache-Control"))
	}
	if res, _ := get(t, srv, "/ui/assets/khong-co.js"); res.StatusCode != http.StatusNotFound || strings.Contains(res.Header.Get("Cache-Control"), "immutable") {
		t.Errorf("asset không tồn tại: %d, Cache-Control = %q", res.StatusCode, res.Header.Get("Cache-Control"))
	}

	// Cấu hình công khai: đúng ba trường, không có gì khác.
	res, body := get(t, srv, "/ui/config.json")
	var public map[string]string
	if err := json.Unmarshal([]byte(body), &public); err != nil || res.Header.Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("config.json: %v %s", err, body)
	}
	if len(public) != 3 || public["issuer"] != "https://idp.example.test/realms/hq" || public["client_id"] != "hq-web" || public["api_base"] != "/v1" {
		t.Fatalf("config.json = %v", public)
	}
	if res.Header.Get("Cache-Control") != "no-cache" {
		t.Errorf("config.json: Cache-Control = %q", res.Header.Get("Cache-Control"))
	}

	// Chỉ đọc.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/ui/", nil)
	post, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = post.Body.Close()
	if post.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /ui/: %d", post.StatusCode)
	}

	// Không lộ file ẩn, không liệt kê thư mục, không ra ngoài thư mục nhúng.
	for _, path := range []string{
		"/ui/.gitkeep", "/ui/assets/.hidden-chunk.js", "/ui/assets/",
		"/ui/../webui.go", "/ui/%2e%2e/webui.go", "/ui/webui.go", "/ui/dist/index.html",
	} {
		if res, _ := get(t, srv, path); res.StatusCode == http.StatusOK && !strings.HasSuffix(res.Path, "/ui/") {
			t.Errorf("%s trả 200", path)
		}
	}
}

// Binary build khi chưa có bản build của giao diện: /ui/ nói rõ điều đó thay vì trả trang
// trắng; config.json vẫn có (nó không phụ thuộc bản build).
func TestHandlerNotBuilt(t *testing.T) {
	srv := httptest.NewServer(newHandler(Config{Issuer: "https://idp.example.test/realms/hq", ClientID: "hq-web"},
		fstest.MapFS{".gitkeep": {Data: []byte("")}}))
	defer srv.Close()
	for _, path := range []string{"/ui/", "/ui/assets/index-abc.js"} {
		res, body := get(t, srv, path)
		if res.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "make ui-build") {
			t.Errorf("%s: %d %q", path, res.StatusCode, body)
		}
		if res.Header.Get("Content-Security-Policy") == "" {
			t.Errorf("%s: thiếu CSP", path)
		}
	}
	if res, _ := get(t, srv, "/ui/config.json"); res.StatusCode != http.StatusOK {
		t.Errorf("config.json: %d", res.StatusCode)
	}
	if res, _ := get(t, srv, "/ui/.gitkeep"); res.StatusCode != http.StatusNotFound {
		t.Errorf(".gitkeep: %d", res.StatusCode)
	}
}

var (
	scriptTag    = regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script>`)
	inlineHandle = regexp.MustCompile(`(?i)\son[a-z]+\s*=`)
)

// Bản build thật (nếu có): index.html không có script hay style nội tuyến. CSP sẽ chặn
// chúng và trang sẽ hỏng lặng lẽ; test này bắt một thay đổi cấu hình Vite làm điều đó.
func TestBuiltIndexHasNoInlineCode(t *testing.T) {
	raw, err := dist.ReadFile("dist/index.html")
	if err != nil {
		// CI đặt HQ_UI_REQUIRE_BUILD=1: ở đó thiếu bản build là lỗi, không phải lý do bỏ qua
		// (nếu không, đổi thứ tự bước trong CI sẽ lặng lẽ tắt test này).
		if os.Getenv("HQ_UI_REQUIRE_BUILD") == "1" {
			t.Fatal("HQ_UI_REQUIRE_BUILD=1 nhưng chưa có bản build của giao diện (make ui-build)")
		}
		t.Skip("chưa có bản build của giao diện (make ui-build)")
	}
	index := string(raw)

	// Handler thật trên file nhúng thật: trang và mọi file nó tham chiếu được phục vụ kèm CSP.
	srv := httptest.NewServer(Handler(Config{Issuer: "https://idp.example.test/realms/hq", ClientID: "hq-web"}))
	defer srv.Close()
	if res, body := get(t, srv, "/ui/"); res.StatusCode != http.StatusOK || body != index || res.Header.Get("Content-Security-Policy") == "" {
		t.Fatalf("Handler() /ui/: status %d, thân khớp index.html: %v", res.StatusCode, body == index)
	}
	for _, ref := range regexp.MustCompile(`(?:src|href)="(/ui/[^"]+)"`).FindAllStringSubmatch(index, -1) {
		if res, body := get(t, srv, ref[1]); res.StatusCode != http.StatusOK || body == "" || res.Header.Get("Content-Security-Policy") == "" {
			t.Errorf("Handler() %s: status %d, %d byte", ref[1], res.StatusCode, len(body))
		}
	}
	if strings.Contains(strings.ToLower(index), "<style") || strings.Contains(index, " style=") || inlineHandle.MatchString(index) {
		t.Errorf("index.html có style hoặc handler nội tuyến")
	}
	scripts := scriptTag.FindAllStringSubmatch(index, -1)
	if len(scripts) == 0 {
		t.Fatalf("index.html không nạp script nào")
	}
	for _, m := range scripts {
		if !strings.Contains(m[1], `src="/ui/assets/`) || strings.TrimSpace(m[2]) != "" {
			t.Errorf("script không phải file của chính origin: %s", m[0])
		}
	}
	for _, ref := range regexp.MustCompile(`(?:src|href)="([^"]+)"`).FindAllStringSubmatch(index, -1) {
		if !strings.HasPrefix(ref[1], "/ui/") && !strings.HasPrefix(ref[1], "data:image/svg+xml,") {
			t.Errorf("index.html tham chiếu ngoài origin: %s", ref[1])
		}
	}
}

// Mã nguồn giao diện không đưa HTML thô vào DOM và không dựng code từ chuỗi: dữ liệu từ
// API (kể cả nội dung do model hay hệ thống ngoài sinh ra) chỉ vào trang dạng văn bản.
// ESLint của web/ kiểm cùng điều này theo cú pháp; test này giữ bất biến đó trong
// `make test`, nơi không có Node. (Bản build không quét được: chính React có dùng các API này.)
func TestSourceHasNoRawHTMLSinks(t *testing.T) {
	root := filepath.Join("..", "..", "web", "src")
	banned := []*regexp.Regexp{
		regexp.MustCompile(`dangerouslySetInnerHTML`),
		// Không đòi dấu chấm phía trước: el['innerHTML'] cũng bị bắt.
		regexp.MustCompile(`innerHTML|outerHTML|insertAdjacentHTML`),
		regexp.MustCompile(`setHTMLUnsafe|parseFromString`),
		regexp.MustCompile(`document\.write`),
		regexp.MustCompile(`new Function\(`),
		regexp.MustCompile(`\beval\(`),
		regexp.MustCompile(`createContextualFragment`),
		regexp.MustCompile(`\bsrcdoc\b`),
	}
	files := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || (!strings.HasSuffix(path, ".ts") && !strings.HasSuffix(path, ".tsx")) || strings.HasSuffix(path, ".gen.ts") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files++
		for _, re := range banned {
			if loc := re.FindIndex(raw); loc != nil {
				t.Errorf("%s dùng %s", path, raw[loc[0]:loc[1]])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("đọc mã nguồn giao diện: %v", err)
	}
	if files < 20 {
		t.Fatalf("chỉ quét được %d file trong %s: bộ quét của test hỏng", files, root)
	}
}
