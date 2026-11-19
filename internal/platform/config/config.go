// Package config nạp cấu hình process từ biến môi trường.
package config

import (
	"fmt"
	"time"
)

// Các môi trường hợp lệ.
const (
	EnvLocal      = "local"
	EnvStaging    = "staging"
	EnvProduction = "production"
)

// Config là cấu hình chung của một process.
// Cấu hình OpenTelemetry dùng các biến chuẩn OTEL_* do SDK tự đọc.
type Config struct {
	Env             string        // HQ_ENV: local | staging | production (mặc định local)
	HTTPAddr        string        // HQ_HTTP_ADDR
	DatabaseURL     string        // HQ_DATABASE_URL (bắt buộc)
	ShutdownTimeout time.Duration // HQ_SHUTDOWN_TIMEOUT
}

// Load đọc cấu hình qua getenv (thường là os.Getenv).
func Load(getenv func(string) string, defaultAddr string) (Config, error) {
	return load(getenv, defaultAddr, true)
}

// LoadWithoutDatabase giống Load nhưng không yêu cầu HQ_DATABASE_URL, cho process
// không dùng database (managed MCP server).
func LoadWithoutDatabase(getenv func(string) string, defaultAddr string) (Config, error) {
	return load(getenv, defaultAddr, false)
}

func load(getenv func(string) string, defaultAddr string, needDB bool) (Config, error) {
	c := Config{
		Env:             orDefault(getenv("HQ_ENV"), "local"),
		HTTPAddr:        orDefault(getenv("HQ_HTTP_ADDR"), defaultAddr),
		DatabaseURL:     getenv("HQ_DATABASE_URL"),
		ShutdownTimeout: 20 * time.Second,
	}
	// Giá trị lạ (gõ sai, dính khoảng trắng, "prod", "live") bị từ chối thay vì bị
	// coi như một môi trường nào đó: nhiều hành vi an toàn phụ thuộc vào HQ_ENV.
	switch c.Env {
	case EnvLocal, EnvStaging, EnvProduction:
	default:
		return Config{}, fmt.Errorf("config: HQ_ENV phải là %s, %s hoặc %s (nhận %q)", EnvLocal, EnvStaging, EnvProduction, c.Env)
	}
	if needDB && c.DatabaseURL == "" {
		return Config{}, fmt.Errorf("config: HQ_DATABASE_URL is required")
	}
	if v := getenv("HQ_SHUTDOWN_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return Config{}, fmt.Errorf("config: HQ_SHUTDOWN_TIMEOUT must be a positive duration")
		}
		c.ShutdownTimeout = d
	}
	return c, nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
