// Command gateway: MCP Gateway: điểm thực thi chính sách và proxy tới MCP server. Process duy nhất được giải mã secret.
package main

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/hqgroups/mcp/internal/cost/budget"
	"github.com/hqgroups/mcp/internal/gatewayserver"
	"github.com/hqgroups/mcp/internal/mcp/gateway"
	"github.com/hqgroups/mcp/internal/platform/config"
	"github.com/hqgroups/mcp/internal/platform/identity"
	"github.com/hqgroups/mcp/internal/platform/process"
	"github.com/hqgroups/mcp/internal/platform/secrets"
	"github.com/hqgroups/mcp/internal/shared/safehttp"
	"github.com/hqgroups/mcp/internal/shared/signedctx"
)

func main() {
	os.Exit(process.Run(process.Options{
		Name:        "gateway",
		DefaultAddr: ":8081",
		// Một lượt gọi model mang cả hội thoại của task.
		MaxBodyBytes: gateway.MaxModelRequestBytes,
		Routes:       routes,
	}))
}

func routes(ctx context.Context, mux *http.ServeMux, deps process.Deps) error {
	orgClaim := os.Getenv("HQ_OIDC_ORG_CLAIM")
	if orgClaim == "" {
		orgClaim = "org_id"
	}
	// Audience của gateway tách khỏi audience của Management API: token phát cho
	// API không dùng được ở đây và ngược lại.
	audience := os.Getenv("HQ_GATEWAY_OIDC_AUDIENCE")
	if audience == "" {
		return fmt.Errorf("gateway: HQ_GATEWAY_OIDC_AUDIENCE is required")
	}
	issuer := os.Getenv("HQ_OIDC_ISSUER")
	discovery, err := identity.DiscoveryOverride(os.Getenv)
	if err != nil {
		return err
	}
	verifier, err := identity.NewOIDCVerifierVia(ctx, issuer, discovery, audience, orgClaim)
	if err != nil {
		return err
	}
	priv, err := signedctx.ParseSigningKey(os.Getenv("HQ_SERVICE_SIGNING_KEY"))
	if err != nil {
		return fmt.Errorf("gateway: HQ_SERVICE_SIGNING_KEY: %w", err)
	}
	key := priv.Seed()
	signer, err := signedctx.NewSigner(priv)
	if err != nil {
		return err
	}
	cfg := gatewayserver.Config{
		PublicURL: strings.TrimRight(os.Getenv("HQ_GATEWAY_PUBLIC_URL"), "/"),
		Issuer:    issuer,
	}
	// Khoá kiểm execution token của Agent nội bộ. Không đặt thì endpoint nội bộ tắt.
	if v := os.Getenv("HQ_EXECUTION_TOKEN_KEY"); v != "" {
		if cfg.ExecutionTokenKey, err = signedctx.ParseKey(v); err != nil {
			return fmt.Errorf("gateway: HQ_EXECUTION_TOKEN_KEY: %w", err)
		}
		// Process api giữ khoá execution token; nó không được vì thế mà giữ luôn khoá
		// ký ngữ cảnh tới managed server.
		if subtle.ConstantTimeCompare(cfg.ExecutionTokenKey, key) == 1 {
			return fmt.Errorf("gateway: HQ_EXECUTION_TOKEN_KEY không được trùng HQ_SERVICE_SIGNING_KEY")
		}
	}
	// Khoá kiểm service token của process api (uỷ thác discovery). Khác khoá execution
	// token, vì worker giữ khoá kia.
	if v := os.Getenv("HQ_SERVICE_TOKEN_KEY"); v != "" {
		if cfg.ServiceTokenKey, err = signedctx.ParseKey(v); err != nil {
			return fmt.Errorf("gateway: HQ_SERVICE_TOKEN_KEY: %w", err)
		}
		if subtle.ConstantTimeCompare(cfg.ServiceTokenKey, key) == 1 {
			return fmt.Errorf("gateway: HQ_SERVICE_TOKEN_KEY không được trùng HQ_SERVICE_SIGNING_KEY")
		}
	}
	// Khoá gốc để giải mã secret của org (khoá LLM). Chỉ process này được cấp.
	if path := os.Getenv("HQ_SECRETS_ROOT_KEY_FILE"); path != "" {
		root, err := secrets.LoadLocalRootKey(path)
		if err != nil {
			return fmt.Errorf("gateway: HQ_SECRETS_ROOT_KEY_FILE: %w", err)
		}
		opener, err := secrets.NewLocalOpener(root, deps.Config.Env)
		if err != nil {
			return err
		}
		cfg.SecretOpener = opener
	}
	// Khoá LLM của platform, cho provider phạm vi platform.
	if key := os.Getenv("HQ_ANTHROPIC_API_KEY"); key != "" {
		cfg.PlatformLLMKeys = map[string]string{"anthropic": key}
	}
	cfg.UpstreamPolicy = upstreamPolicy(deps.Config.Env)
	// Rate limit dùng chung giữa các replica. Không có Redis thì mỗi process tự đếm
	// trong bộ nhớ, chỉ chấp nhận được khi chạy một replica (local).
	if raw := os.Getenv("HQ_REDIS_URL"); raw != "" {
		opt, err := redis.ParseURL(raw)
		if err != nil {
			return fmt.Errorf("gateway: HQ_REDIS_URL không hợp lệ") // không in lỗi gốc: URL có thể chứa mật khẩu
		}
		rdb := redis.NewClient(opt)
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = rdb.Ping(pctx).Err()
		cancel()
		if err != nil {
			return fmt.Errorf("gateway: không kết nối được Redis: %w", err)
		}
		deps.OnShutdown(func(context.Context) { _ = rdb.Close() })
		cfg.Limiter = gateway.NewRedisLimiter(rdb)
		// Bộ đếm budget cũng dùng chung giữa các replica; nó dựng lại được từ usage ledger.
		cfg.BudgetCounter = budget.NewRedisCounter(rdb)
	} else if deps.Config.Env != config.EnvLocal {
		return fmt.Errorf("gateway: HQ_REDIS_URL là bắt buộc ngoài môi trường local (rate limit phải dùng chung giữa các replica)")
	}
	if err := gatewayserver.Register(mux, deps.DB, verifier, signer, cfg); err != nil {
		return err
	}
	// Bộ gửi thông báo webhook: dừng theo context của process.
	if n := gatewayserver.NewNotifier(deps.DB, cfg); n != nil {
		go n.Run(ctx)
	}
	return nil
}

// upstreamPolicy là chính sách SSRF cho MCP server ngoài. Ngoại lệ cho http và địa chỉ
// cục bộ chỉ có trên máy lập trình viên và phải bật tường minh.
func upstreamPolicy(env string) safehttp.Policy {
	return safehttp.Policy{AllowInsecureLocal: env == config.EnvLocal && os.Getenv("HQ_SSRF_ALLOW_LOCAL") == "1"}
}
