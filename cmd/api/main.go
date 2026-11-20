// Command api: REST Management API (control plane). Không có quyền giải mã secret.
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
	"github.com/hqgroups/mcp/internal/apiserver"
	"github.com/hqgroups/mcp/internal/platform/config"
	"github.com/hqgroups/mcp/internal/platform/identity"
	"github.com/hqgroups/mcp/internal/platform/process"
	"github.com/hqgroups/mcp/internal/platform/secrets"
	"github.com/hqgroups/mcp/internal/shared/safehttp"
	"github.com/hqgroups/mcp/internal/shared/signedctx"
	"github.com/hqgroups/mcp/internal/webui"
	"github.com/hqgroups/mcp/internal/workflow/definitions/schedule"
)

func main() {
	os.Exit(process.Run(process.Options{
		Name:        "api",
		DefaultAddr: ":8080",
		Routes:      routes,
	}))
}

func routes(ctx context.Context, mux *http.ServeMux, deps process.Deps) error {
	orgClaim := os.Getenv("HQ_OIDC_ORG_CLAIM")
	if orgClaim == "" {
		orgClaim = "org_id"
	}
	// Discovery chạy lúc khởi động: IdP không tới được thì process không lên,
	// thay vì lên và chấp nhận request không kiểm tra được.
	discovery, err := identity.DiscoveryOverride(os.Getenv)
	if err != nil {
		return err
	}
	verifier, err := identity.NewOIDCVerifierVia(ctx, os.Getenv("HQ_OIDC_ISSUER"), discovery, os.Getenv("HQ_OIDC_AUDIENCE"), orgClaim)
	if err != nil {
		return err
	}
	// Adapter claude-cli chỉ có trên máy lập trình viên: HQ_ENV phải đúng là local
	// VÀ phải bật tường minh (ADR-0013). Staging và production dùng adapter
	// anthropic, gọi qua Gateway.
	cfg := apiserver.Config{Providers: map[string]provider.Provider{}, GatewayURL: os.Getenv("HQ_GATEWAY_INTERNAL_URL")}
	if claudecli.Allowed(deps.Config.Env) && os.Getenv("HQ_ENABLE_CLAUDE_CLI") == "1" {
		p, err := claudecli.New(claudecli.Config{Env: deps.Config.Env})
		if err != nil {
			return err
		}
		cfg.Adapters = append(cfg.Adapters, claudecli.Name)
		cfg.Providers[claudecli.Name] = p
	}
	// Process này không giữ khoá LLM nào: adapter anthropic được gọi qua Gateway. Nó
	// chỉ giữ khoá CÔNG KHAI của Secrets, đủ để mã hoá api_key do org nhập mà không
	// đọc lại được.
	if pub := os.Getenv("HQ_SECRETS_PUBLIC_KEY"); pub != "" {
		sealer, err := secrets.NewLocalSealer(pub, deps.Config.Env)
		if err != nil {
			return fmt.Errorf("api: HQ_SECRETS_PUBLIC_KEY: %w", err)
		}
		cfg.SecretSealer = sealer
	}
	// Agent Runtime gọi tool qua Gateway bằng execution token. Thiếu URL hoặc khoá
	// thì API vẫn chạy, nhưng tạo execution trả 503.
	if v := os.Getenv("HQ_EXECUTION_TOKEN_KEY"); v != "" {
		if cfg.ExecutionTokenKey, err = signedctx.ParseKey(v); err != nil {
			return fmt.Errorf("api: HQ_EXECUTION_TOKEN_KEY: %w", err)
		}
	}
	// Có Temporal thì vòng lặp chạy ở process worker và bền vững qua restart. Không có
	// thì vòng lặp chạy ngay trong process này: chỉ chấp nhận trên máy lập trình viên.
	local := deps.Config.Env == config.EnvLocal
	if os.Getenv("HQ_TEMPORAL_ADDRESS") != "" {
		c, err := durable.DialFromEnv(ctx, os.Getenv, local)
		if err != nil {
			return err
		}
		// Lịch chạy của workflow (ADR-0015): Temporal Schedules, và worker của hàng đợi
		// api-internal chạy ngay trong process này để tạo lần chạy khi lịch kích hoạt.
		stopSchedules := func() {}
		cfg.WorkflowScheduler = schedule.Scheduler{Client: c}
		cfg.ScheduleHost = func(runs apiserver.ScheduledRuns) error {
			stop, err := schedule.Start(c, runs)
			if err == nil {
				stopSchedules = stop
			}
			return err
		}
		deps.OnShutdown(func(context.Context) {
			stopSchedules()
			c.Close()
		})
		cfg.Runner = &durable.Starter{Client: c}
	} else if !local {
		return errors.New("api: thiếu HQ_TEMPORAL_ADDRESS (bắt buộc ngoài local)")
	} else {
		cfg.InProcessRuntime = true
	}
	// Khoá ký service token gửi Gateway (uỷ thác discovery của MCP server ngoài). Khác
	// khoá execution token: worker giữ khoá kia. Thiếu thì discovery không dùng được.
	if v := os.Getenv("HQ_SERVICE_TOKEN_KEY"); v != "" {
		if cfg.ServiceTokenKey, err = signedctx.ParseKey(v); err != nil {
			return fmt.Errorf("api: HQ_SERVICE_TOKEN_KEY: %w", err)
		}
	}
	// Chính sách SSRF cho endpoint MCP do tenant đăng ký; phải khớp với của gateway.
	cfg.UpstreamPolicy = safehttp.Policy{AllowInsecureLocal: deps.Config.Env == config.EnvLocal && os.Getenv("HQ_SSRF_ALLOW_LOCAL") == "1"}
	h, wait, err := apiserver.New(ctx, deps.DB, verifier, cfg)
	if err != nil {
		return err
	}
	// Execution đang chạy được đóng (failed / interrupted) trước khi process thoát.
	deps.OnShutdown(wait)
	mux.Handle("/v1/", h)
	// Giao diện web: file tĩnh, không có quyền gì của riêng nó (mọi lời gọi của nó đi qua
	// /v1 bằng token của người dùng). Ở local nó bật sẵn (HQ_UI_DISABLED=1 để tắt); ở môi
	// trường khác phải bật tường minh bằng HQ_UI_ENABLED=1: đặt token của người dùng vào
	// trình duyệt trên origin của API là một quyết định, không phải mặc định.
	ui := os.Getenv("HQ_UI_ENABLED") == "1" || (deps.Config.Env == config.EnvLocal && os.Getenv("HQ_UI_DISABLED") != "1")
	if ui {
		client := os.Getenv("HQ_UI_OIDC_CLIENT_ID")
		if client == "" {
			client = "hq-web"
		}
		webui.Mount(mux, webui.Config{Issuer: os.Getenv("HQ_OIDC_ISSUER"), ClientID: client})
	}
	return nil
}
