// Command worker: Execution Worker. Chạy vòng lặp Agent như Temporal workflow và
// activity (ADR-0002). Không có quyền nào với secret: lượt model có khoá và mọi tool
// call đi qua MCP Gateway bằng execution token.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/hqgroups/mcp/internal/agent/runtime/durable"
	"github.com/hqgroups/mcp/internal/ai/provider"
	"github.com/hqgroups/mcp/internal/ai/provider/claudecli"
	"github.com/hqgroups/mcp/internal/platform/config"
	"github.com/hqgroups/mcp/internal/platform/process"
	"github.com/hqgroups/mcp/internal/shared/signedctx"
	"github.com/hqgroups/mcp/internal/workerserver"
)

func main() {
	os.Exit(process.Run(process.Options{
		Name:        "worker",
		DefaultAddr: ":8082",
		Routes:      routes,
	}))
}

// forbidden là các biến mà process này không được nhận: khoá của Secrets, của LLM
// provider, khoá ký ngữ cảnh tới managed server và khoá service token. Worker xử lý
// output của model, nên nó không được giữ thứ gì ngoài khoá execution token.
var forbidden = []string{"HQ_SECRETS_ROOT_KEY_FILE", "HQ_SERVICE_SIGNING_KEY", "HQ_SERVICE_TOKEN_KEY", "HQ_ANTHROPIC_API_KEY"}

func routes(ctx context.Context, _ *http.ServeMux, deps process.Deps) error {
	for _, name := range forbidden {
		if os.Getenv(name) != "" {
			return fmt.Errorf("worker: %s không được cấp cho process worker", name)
		}
	}
	if os.Getenv("HQ_TEMPORAL_ADDRESS") == "" {
		return errors.New("worker: thiếu HQ_TEMPORAL_ADDRESS")
	}
	key, err := signedctx.ParseKey(os.Getenv("HQ_EXECUTION_TOKEN_KEY"))
	if err != nil {
		return fmt.Errorf("worker: HQ_EXECUTION_TOKEN_KEY: %w", err)
	}
	cfg := workerserver.Config{GatewayURL: os.Getenv("HQ_GATEWAY_INTERNAL_URL"), ExecutionTokenKey: key, LocalProviders: map[string]provider.Provider{}}
	// Adapter claude-cli chỉ có trên máy lập trình viên (ADR-0013). Phải bật giống
	// process api: api chỉ nhận execution cho adapter mà nó được báo là worker có.
	if claudecli.Allowed(deps.Config.Env) && os.Getenv("HQ_ENABLE_CLAUDE_CLI") == "1" {
		p, err := claudecli.New(claudecli.Config{Env: deps.Config.Env})
		if err != nil {
			return err
		}
		cfg.LocalProviders[claudecli.Name] = p
	}
	c, err := durable.DialFromEnv(ctx, os.Getenv, deps.Config.Env == config.EnvLocal)
	if err != nil {
		return err
	}
	stop, err := workerserver.Start(c, deps.DB, cfg)
	if err != nil {
		c.Close()
		return fmt.Errorf("worker: %w", err)
	}
	deps.OnShutdown(func(context.Context) {
		stop()
		c.Close()
	})
	return nil
}
