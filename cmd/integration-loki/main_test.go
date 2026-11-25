package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testOrg   = "11111111-1111-4111-8111-111111111111"
	testToken = "loki-startup-secret"
)

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Cấu hình thiếu hoặc không an toàn làm process từ chối khởi động, và thông báo lỗi
// không chứa token, khoá hay tenant.
func TestLoad(t *testing.T) {
	key := strings.Repeat("ab", 32)
	base := map[string]string{
		"HQ_ENV": "local", "HQ_LOKI_URL": "http://loki:3100", "HQ_LOKI_ORG_ID": testOrg, "HQ_SERVICE_VERIFY_KEY": key,
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

	ok := map[string]struct {
		env     map[string]string
		tenants int
	}{
		"tối thiểu ở local":       {nil, 0},
		"một tenant":              {map[string]string{"HQ_LOKI_TENANT_ID": "team-a"}, 1},
		"nhiều tenant khi bật cờ": {map[string]string{"HQ_LOKI_TENANT_ID": "team-a|team-b", "HQ_LOKI_ALLOW_MULTI_TENANT": "1"}, 2},
		"https kèm token ở production": {map[string]string{"HQ_ENV": "production", "HQ_LOKI_URL": "https://loki.example",
			"HQ_LOKI_BEARER_TOKEN_FILE": writeFile(t, testToken+"\n")}, 0},
	}
	for name, tc := range ok {
		s, err := load(with(tc.env))
		if err != nil {
			t.Errorf("%s: phải khởi động được: %v", name, err)
			continue
		}
		if s.client.Tenants() != tc.tenants {
			t.Errorf("%s: số tenant %d, muốn %d", name, s.client.Tenants(), tc.tenants)
		}
	}

	bad := map[string]map[string]string{
		"HQ_ENV không đặt":              {"HQ_ENV": ""},
		"HQ_ENV lạ":                     {"HQ_ENV": "dev"},
		"thiếu URL":                     {"HQ_LOKI_URL": ""},
		"http ở staging":                {"HQ_ENV": "staging"},
		"http ở production":             {"HQ_ENV": "production"},
		"token qua http":                {"HQ_LOKI_BEARER_TOKEN_FILE": writeFile(t, testToken)},
		"file token không tồn tại":      {"HQ_LOKI_URL": "https://loki.example", "HQ_LOKI_BEARER_TOKEN_FILE": filepath.Join(t.TempDir(), "missing")},
		"file token rỗng":               {"HQ_LOKI_URL": "https://loki.example", "HQ_LOKI_BEARER_TOKEN_FILE": writeFile(t, "")},
		"file token toàn khoảng trắng":  {"HQ_LOKI_URL": "https://loki.example", "HQ_LOKI_BEARER_TOKEN_FILE": writeFile(t, " \n\t\r\n")},
		"token nhiều dòng":              {"HQ_LOKI_URL": "https://loki.example", "HQ_LOKI_BEARER_TOKEN_FILE": writeFile(t, testToken+"\nX-Evil: 1")},
		"token có ký tự điều khiển":     {"HQ_LOKI_URL": "https://loki.example", "HQ_LOKI_BEARER_TOKEN_FILE": writeFile(t, testToken+"\x00tail")},
		"file token quá lớn":            {"HQ_LOKI_URL": "https://loki.example", "HQ_LOKI_BEARER_TOKEN_FILE": writeFile(t, strings.Repeat("a", 17<<10))},
		"nhiều tenant khi không bật cờ": {"HQ_LOKI_TENANT_ID": "team-a|team-b"},
		"cờ nhiều tenant giá trị lạ":    {"HQ_LOKI_TENANT_ID": "team-a|team-b", "HQ_LOKI_ALLOW_MULTI_TENANT": "true"},
		"tenant có khoảng trắng":        {"HQ_LOKI_TENANT_ID": "team a"},
		"thiếu khoá kiểm":               {"HQ_SERVICE_VERIFY_KEY": ""},
		"được cấp khoá ký":              {"HQ_SERVICE_SIGNING_KEY": strings.Repeat("ab", 32)},
		"khoá kiểm ngắn":                {"HQ_SERVICE_VERIFY_KEY": "abcd"},
		"thiếu org":                     {"HQ_LOKI_ORG_ID": ""},
		"org là nil":                    {"HQ_LOKI_ORG_ID": "00000000-0000-0000-0000-000000000000"},
	}
	for name, env := range bad {
		_, err := load(with(env))
		if err == nil {
			t.Errorf("%s: phải từ chối khởi động", name)
			continue
		}
		for _, secret := range []string{testToken, key, "team-a", "team-b"} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("%s: lỗi khởi động lộ %q: %v", name, secret, err)
			}
		}
	}
}
