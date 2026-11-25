package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Cấu hình thiếu hoặc không an toàn làm process không khởi động được, và thông báo lỗi
// không chép token.
func TestConfigure(t *testing.T) {
	const token = "github_pat_11CONFIGTEST0123456789"
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	good := map[string]string{
		"HQ_ENV":                 "production",
		"HQ_GITHUB_REPOSITORIES": "acme/api, acme/web",
		"HQ_GITHUB_TOKEN_FILE":   write("token", token+"\n"),
		"HQ_GITHUB_ORG_ID":       uuid.NewString(),
		"HQ_SERVICE_VERIFY_KEY":  strings.Repeat("ab", 32),
	}
	env := func(overrides map[string]string) func(string) string {
		return func(key string) string {
			if v, ok := overrides[key]; ok {
				return v
			}
			return good[key]
		}
	}
	cfg, err := configure(env(nil))
	if err != nil {
		t.Fatalf("cấu hình hợp lệ: %v", err)
	}
	if got := cfg.client.Repositories(); len(got) != 2 || cfg.orgID == uuid.Nil || cfg.verifier == nil {
		t.Errorf("cấu hình đọc được: %v", got)
	}
	// http được nhận khi HQ_ENV=local (GitHub giả lập trên máy phát triển).
	if _, err := configure(env(map[string]string{"HQ_ENV": "local", "HQ_GITHUB_API_URL": "http://localhost:9999"})); err != nil {
		t.Errorf("http ở local: %v", err)
	}

	for name, overrides := range map[string]map[string]string{
		"thiếu allowlist":          {"HQ_GITHUB_REPOSITORIES": " , "},
		"allowlist wildcard":       {"HQ_GITHUB_REPOSITORIES": "acme/*"},
		"thiếu file token":         {"HQ_GITHUB_TOKEN_FILE": ""},
		"file token không tồn tại": {"HQ_GITHUB_TOKEN_FILE": filepath.Join(dir, "missing")},
		"file token rỗng":          {"HQ_GITHUB_TOKEN_FILE": write("empty", "")},
		"file token toàn trắng":    {"HQ_GITHUB_TOKEN_FILE": write("blank", " \n\t\r\n")},
		"token có xuống dòng giữa": {"HQ_GITHUB_TOKEN_FILE": write("multi", token+"\nsecond-line")},
		"http ngoài local":         {"HQ_GITHUB_API_URL": "http://github.internal/api/v3"},
		"http ở staging":           {"HQ_ENV": "staging", "HQ_GITHUB_API_URL": "http://github.internal/api/v3"},
		"URL có dấu hỏi cuối":      {"HQ_GITHUB_API_URL": "https://api.github.com?"},
		"thiếu org id":             {"HQ_GITHUB_ORG_ID": ""},
		"org id rỗng (nil uuid)":   {"HQ_GITHUB_ORG_ID": uuid.Nil.String()},
		"org id không phải uuid":   {"HQ_GITHUB_ORG_ID": "acme"},
		"thiếu khoá kiểm":          {"HQ_SERVICE_VERIFY_KEY": ""},
		"được cấp khoá ký":         {"HQ_SERVICE_SIGNING_KEY": strings.Repeat("ab", 32)},
		"khoá kiểm quá ngắn":       {"HQ_SERVICE_VERIFY_KEY": "abcd"},
	} {
		_, err := configure(env(overrides))
		if err == nil {
			t.Errorf("%s: configure phải từ chối", name)
		} else if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "CONFIGTEST") {
			t.Errorf("%s: lỗi cấu hình lộ token: %v", name, err)
		}
	}
}
