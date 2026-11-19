package config_test

import (
	"testing"

	"github.com/hqgroups/mcp/internal/platform/config"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestLoad(t *testing.T) {
	base := map[string]string{"HQ_DATABASE_URL": "postgres://x"}
	c, err := config.Load(env(base), ":8080")
	if err != nil || c.Env != config.EnvLocal || c.HTTPAddr != ":8080" {
		t.Fatalf("mặc định: %+v %v", c, err)
	}
	for _, v := range []string{"local", "staging", "production"} {
		if _, err := config.Load(env(map[string]string{"HQ_DATABASE_URL": "x", "HQ_ENV": v}), ":1"); err != nil {
			t.Errorf("HQ_ENV=%q: %v", v, err)
		}
	}
	// Giá trị lạ không được âm thầm coi là một môi trường nào đó.
	for _, v := range []string{"prod", "PRODUCTION", "production ", " production", "production\n", "live", "prd", "dev", "Local"} {
		if _, err := config.Load(env(map[string]string{"HQ_DATABASE_URL": "x", "HQ_ENV": v}), ":1"); err == nil {
			t.Errorf("HQ_ENV=%q phải bị từ chối", v)
		}
	}
	if _, err := config.Load(env(map[string]string{}), ":1"); err == nil {
		t.Error("thiếu HQ_DATABASE_URL phải lỗi")
	}
	for _, v := range []string{"abc", "-5s", "0"} {
		if _, err := config.Load(env(map[string]string{"HQ_DATABASE_URL": "x", "HQ_SHUTDOWN_TIMEOUT": v}), ":1"); err == nil {
			t.Errorf("HQ_SHUTDOWN_TIMEOUT=%q phải lỗi", v)
		}
	}
}
