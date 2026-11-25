package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestLoadSettings(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	const secret = "sa-token-value-secret"
	token, blank, empty := write("token", secret+"\n"), write("blank", " \n\t\n"), write("empty", "")
	base := map[string]string{
		"HQ_KUBERNETES_URL":               "https://kube.example:6443",
		"HQ_KUBERNETES_ORG_ID":            uuid.NewString(),
		"HQ_KUBERNETES_NAMESPACES":        "prod,default",
		"HQ_KUBERNETES_BEARER_TOKEN_FILE": token,
		"HQ_SERVICE_VERIFY_KEY":           strings.Repeat("ab", 32),
	}
	load := func(env string, override map[string]string) (settings, error) {
		vars := map[string]string{}
		for k, v := range base {
			vars[k] = v
		}
		for k, v := range override {
			vars[k] = v
		}
		return loadSettings(func(k string) string { return vars[k] }, env)
	}

	cfg, err := load("production", nil)
	if err != nil || cfg.client == nil || cfg.verifier == nil || strings.Join(cfg.namespaces, ",") != "default,prod" || !strings.HasPrefix(cfg.scope, "2 namespace") {
		t.Fatalf("cấu hình hợp lệ: %+v %v", cfg.scope, err)
	}
	cfg, err = load("production", map[string]string{"HQ_KUBERNETES_NAMESPACES": "*"})
	if err != nil || cfg.namespaces != nil || !strings.Contains(cfg.scope, "mọi namespace") {
		t.Errorf("*: %q %v", cfg.scope, err)
	}
	// http chỉ khi local, và không kèm token.
	if _, err := load("local", map[string]string{"HQ_KUBERNETES_URL": "http://127.0.0.1:8001", "HQ_KUBERNETES_BEARER_TOKEN_FILE": ""}); err != nil {
		t.Errorf("http ở local: %v", err)
	}

	for name, tc := range map[string]struct {
		env      string
		override map[string]string
		want     string
	}{
		"thiếu URL":              {"production", map[string]string{"HQ_KUBERNETES_URL": ""}, "HQ_KUBERNETES_URL"},
		"thiếu org":              {"production", map[string]string{"HQ_KUBERNETES_ORG_ID": ""}, "HQ_KUBERNETES_ORG_ID"},
		"org không phải uuid":    {"production", map[string]string{"HQ_KUBERNETES_ORG_ID": "acme"}, "HQ_KUBERNETES_ORG_ID"},
		"org nil":                {"production", map[string]string{"HQ_KUBERNETES_ORG_ID": uuid.Nil.String()}, "HQ_KUBERNETES_ORG_ID"},
		"phạm vi không đặt":      {"production", map[string]string{"HQ_KUBERNETES_NAMESPACES": ""}, "HQ_KUBERNETES_NAMESPACES"},
		"phạm vi trắng":          {"production", map[string]string{"HQ_KUBERNETES_NAMESPACES": "  "}, "HQ_KUBERNETES_NAMESPACES"},
		"* trộn với tên":         {"production", map[string]string{"HQ_KUBERNETES_NAMESPACES": "*,prod"}, "HQ_KUBERNETES_NAMESPACES"},
		"phạm vi hỏng":           {"production", map[string]string{"HQ_KUBERNETES_NAMESPACES": "prod,,x"}, "HQ_KUBERNETES_NAMESPACES"},
		"file token rỗng":        {"production", map[string]string{"HQ_KUBERNETES_BEARER_TOKEN_FILE": empty}, "HQ_KUBERNETES_BEARER_TOKEN_FILE"},
		"file token toàn trắng":  {"production", map[string]string{"HQ_KUBERNETES_BEARER_TOKEN_FILE": blank}, "HQ_KUBERNETES_BEARER_TOKEN_FILE"},
		"file token không có":    {"production", map[string]string{"HQ_KUBERNETES_BEARER_TOKEN_FILE": filepath.Join(dir, "missing")}, "HQ_KUBERNETES_BEARER_TOKEN_FILE"},
		"https thiếu token":      {"production", map[string]string{"HQ_KUBERNETES_BEARER_TOKEN_FILE": ""}, "bearer token"},
		"http ngoài local":       {"staging", map[string]string{"HQ_KUBERNETES_URL": "http://kube:8080", "HQ_KUBERNETES_BEARER_TOKEN_FILE": ""}, "https"},
		"token qua http ở local": {"local", map[string]string{"HQ_KUBERNETES_URL": "http://kube:8080"}, "https"},
		"CA không phải PEM":      {"production", map[string]string{"HQ_KUBERNETES_CA_FILE": token}, "CA"},
		"thiếu khoá kiểm":        {"production", map[string]string{"HQ_SERVICE_VERIFY_KEY": ""}, "HQ_SERVICE_VERIFY_KEY"},
		"được cấp khoá ký":       {"production", map[string]string{"HQ_SERVICE_SIGNING_KEY": strings.Repeat("ab", 32)}, "HQ_SERVICE_SIGNING_KEY"},
	} {
		_, err := load(tc.env, tc.override)
		switch {
		case err == nil:
			t.Errorf("%s: phải từ chối khởi động", name)
		case !strings.Contains(err.Error(), tc.want):
			t.Errorf("%s: lỗi phải nêu %q: %v", name, tc.want, err)
		case strings.Contains(err.Error(), secret):
			t.Errorf("%s: lỗi lộ nội dung file token: %v", name, err)
		}
	}
}
