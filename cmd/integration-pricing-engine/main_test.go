package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testOrg    = "11111111-1111-4111-8111-111111111111"
	testAPIKey = "sk_live_startup_secret_value"
)

func writeFile(t *testing.T, content string) string {
	t.Helper()
	return writeMode(t, content, 0o600)
}

// writeMode ghi file rồi chmod (umask của máy chạy test không làm đổi quyền mong muốn).
func writeMode(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "api-key")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

// Cấu hình thiếu hoặc không an toàn làm process từ chối khởi động, và thông báo lỗi
// không chứa khoá.
func TestLoad(t *testing.T) {
	keyFile := writeFile(t, testAPIKey+"\n")
	base := map[string]string{
		"HQ_ENV": "local", "HQ_PRICING_ENGINE_URL": "http://localhost:3001", "HQ_PRICING_ENGINE_ORG_ID": testOrg,
		"HQ_PRICING_ENGINE_API_KEY_FILE": keyFile, "HQ_SERVICE_VERIFY_KEY": strings.Repeat("ab", 32),
	}
	with := func(over map[string]string) func(string) string {
		env := map[string]string{}
		for k, v := range base {
			env[k] = v
		}
		for k, v := range over {
			env[k] = v
		}
		return func(name string) string { return env[name] }
	}

	prod := map[string]string{"HQ_ENV": "production", "HQ_PRICING_ENGINE_URL": "https://pe.example"}
	and := func(a map[string]string, k, v string) map[string]string {
		out := map[string]string{k: v}
		for key, val := range a {
			out[key] = val
		}
		return out
	}
	for name, over := range map[string]map[string]string{
		"local: http tới localhost":       nil,
		"local: http tới 127.0.0.1":       {"HQ_PRICING_ENGINE_URL": "http://127.0.0.1:3001"},
		"local: http tới [::1]":           {"HQ_PRICING_ENGINE_URL": "http://[::1]:3001"},
		"local: https tới host bất kỳ":    {"HQ_PRICING_ENGINE_URL": "https://pe.internal"},
		"local: file khoá 0644 được nhận": {"HQ_PRICING_ENGINE_API_KEY_FILE": writeMode(t, testAPIKey+"\n", 0o644)},
		"https ở production, khoá 0600":   prod,
		"https ở production, khoá 0400":   and(prod, "HQ_PRICING_ENGINE_API_KEY_FILE", writeMode(t, testAPIKey+"\n", 0o400)),
		"https kèm path prefix ở staging": {"HQ_ENV": "staging", "HQ_PRICING_ENGINE_URL": "https://pe.example/pe/"},
		"local: khoá từ biến môi trường":  {"HQ_PRICING_ENGINE_API_KEY_FILE": "", "HQ_PRICING_ENGINE_API_KEY": testAPIKey},
	} {
		if _, err := load(with(over)); err != nil {
			t.Errorf("%s: phải khởi động được: %v", name, err)
		}
	}

	for name, over := range map[string]map[string]string{
		"thiếu HQ_ENV":                        {"HQ_ENV": ""},
		"HQ_ENV lạ":                           {"HQ_ENV": "dev"},
		"thiếu URL":                           {"HQ_PRICING_ENGINE_URL": ""},
		"http ở staging":                      {"HQ_ENV": "staging"},
		"http ở production":                   {"HQ_ENV": "production"},
		"local: http tới host không loopback": {"HQ_PRICING_ENGINE_URL": "http://pe.internal:3001"},
		"local: http tới IP riêng":            {"HQ_PRICING_ENGINE_URL": "http://192.168.1.10:3001"},
		"local: http tới tên giả loopback":    {"HQ_PRICING_ENGINE_URL": "http://localhost.evil.example:3001"},
		"production: khoá cho nhóm đọc":       and(prod, "HQ_PRICING_ENGINE_API_KEY_FILE", writeMode(t, testAPIKey+"\n", 0o640)),
		"production: khoá cho mọi người đọc":  and(prod, "HQ_PRICING_ENGINE_API_KEY_FILE", writeMode(t, testAPIKey+"\n", 0o604)),
		"staging: khoá 0644": {"HQ_ENV": "staging", "HQ_PRICING_ENGINE_URL": "https://pe.example",
			"HQ_PRICING_ENGINE_API_KEY_FILE": writeMode(t, testAPIKey+"\n", 0o644)},
		"file khoá là thư mục":                 {"HQ_PRICING_ENGINE_API_KEY_FILE": t.TempDir()},
		"URL mang credential":                  {"HQ_PRICING_ENGINE_URL": "http://user:" + testAPIKey + "@localhost:3001"},
		"thiếu file khoá":                      {"HQ_PRICING_ENGINE_API_KEY_FILE": ""},
		"file khoá không có":                   {"HQ_PRICING_ENGINE_API_KEY_FILE": filepath.Join(t.TempDir(), "missing")},
		"file khoá rỗng":                       {"HQ_PRICING_ENGINE_API_KEY_FILE": writeFile(t, " \n")},
		"file khoá hai dòng":                   {"HQ_PRICING_ENGINE_API_KEY_FILE": writeFile(t, testAPIKey+"\n"+testAPIKey+"\n")},
		"file khoá quá lớn":                    {"HQ_PRICING_ENGINE_API_KEY_FILE": writeFile(t, strings.Repeat(testAPIKey, 400))},
		"production: khoá từ biến môi trường":  and(prod, "HQ_PRICING_ENGINE_API_KEY", testAPIKey),
		"staging: khoá từ biến môi trường":     {"HQ_ENV": "staging", "HQ_PRICING_ENGINE_URL": "https://pe.example", "HQ_PRICING_ENGINE_API_KEY_FILE": "", "HQ_PRICING_ENGINE_API_KEY": testAPIKey},
		"local: đặt cả biến lẫn file":          {"HQ_PRICING_ENGINE_API_KEY": testAPIKey},
		"local: khoá trong biến có xuống dòng": {"HQ_PRICING_ENGINE_API_KEY_FILE": "", "HQ_PRICING_ENGINE_API_KEY": testAPIKey + "\n" + testAPIKey},
		"thiếu org":                            {"HQ_PRICING_ENGINE_ORG_ID": ""},
		"org rỗng (nil UUID)":                  {"HQ_PRICING_ENGINE_ORG_ID": "00000000-0000-0000-0000-000000000000"},
		"thiếu khoá kiểm":                      {"HQ_SERVICE_VERIFY_KEY": ""},
		"được cấp khoá ký":                     {"HQ_SERVICE_SIGNING_KEY": strings.Repeat("ab", 32)},
		"khoá kiểm quá ngắn":                   {"HQ_SERVICE_VERIFY_KEY": "abcd"},
	} {
		_, err := load(with(over))
		if err == nil {
			t.Errorf("%s: phải từ chối khởi động", name)
			continue
		}
		if strings.Contains(err.Error(), testAPIKey) {
			t.Errorf("%s: lỗi khởi động lộ khoá: %v", name, err)
		}
	}
}
