// Package process chứa phần khởi động và dừng dùng chung của ba binary
// (api, gateway, worker): cấu hình, observability, database, health, graceful shutdown.
package process

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/hqgroups/mcp/internal/observability"
	"github.com/hqgroups/mcp/internal/platform/config"
	"github.com/hqgroups/mcp/internal/shared/httpx"
	sharedpgx "github.com/hqgroups/mcp/internal/shared/pgx"
)

// Deps là các phụ thuộc đã khởi tạo, trao cho hàm đăng ký route.
type Deps struct {
	Config config.Config
	DB     *pgxpool.Pool
	// OnShutdown đăng ký việc phải làm sau khi server HTTP đã dừng và trước khi
	// process thoát (ví dụ chờ công việc nền ghi xong). ctx hết hạn theo ShutdownTimeout.
	OnShutdown func(fn func(ctx context.Context))
}

// Options mô tả một process.
type Options struct {
	Name        string // api | gateway | worker | integration-<tên>
	DefaultAddr string
	// NoDatabase dành cho process không dùng database (managed MCP server):
	// không cần HQ_DATABASE_URL, Deps.DB là nil, không có kiểm tra readiness của Postgres.
	NoDatabase bool
	// MaxBodyBytes là trần thân request của process; 0 = mặc định (1 MiB).
	MaxBodyBytes int64
	// Routes đăng ký handler của process. Có thể nil. Lỗi làm process dừng khởi động.
	Routes func(ctx context.Context, mux *http.ServeMux, deps Deps) error
}

// Run chạy process tới khi nhận SIGINT / SIGTERM. Trả về mã thoát.
func Run(opts Options) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	load := config.Load
	if opts.NoDatabase {
		load = config.LoadWithoutDatabase
	}
	cfg, err := load(os.Getenv, opts.DefaultAddr)
	if err != nil {
		slog.Error("startup failed", "error", err)
		return 2
	}

	shutdownTracing, err := observability.Setup(ctx, opts.Name, cfg.Env)
	if err != nil {
		slog.Error("startup failed", "error", err)
		return 1
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if err := shutdownTracing(sctx); err != nil {
			slog.Warn("tracing shutdown", "error", err)
		}
	}()

	checks := map[string]httpx.Check{}
	var pool *pgxpool.Pool
	if !opts.NoDatabase {
		pool, err = sharedpgx.NewPool(ctx, cfg.DatabaseURL)
		if err != nil {
			slog.Error("startup failed", "error", err)
			return 1
		}
		defer pool.Close()
		if err := sharedpgx.RequireRLSEnforced(ctx, pool); err != nil {
			slog.Error("startup failed", "error", err)
			return 1
		}
		checks["postgres"] = pool.Ping
	}

	var hooks []func(context.Context)
	mux := http.NewServeMux()
	httpx.RegisterHealth(mux, checks)
	if opts.Routes != nil {
		if err := opts.Routes(ctx, mux, Deps{Config: cfg, DB: pool, OnShutdown: func(fn func(context.Context)) { hooks = append(hooks, fn) }}); err != nil {
			slog.Error("startup failed", "error", err)
			return 1
		}
	}

	var h http.Handler = mux
	limit := opts.MaxBodyBytes
	if limit <= 0 {
		limit = httpx.MaxBodyBytes
	}
	h = httpx.LimitBody(limit)(h)
	h = httpx.Recover(h)
	// Biên công khai: không nối tiếp trace do client gửi (traceparent), nếu không
	// client gán được trace id tuỳ ý cho hành động của mình trong log và audit.
	h = otelhttp.NewHandler(h, opts.Name,
		otelhttp.WithPublicEndpointFn(func(*http.Request) bool { return true }))

	err = httpx.Serve(ctx, cfg.HTTPAddr, h, cfg.ShutdownTimeout)
	// Công việc nền được dừng và chờ trước khi đóng database (defer ở trên).
	stop()
	hctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	for _, fn := range hooks {
		fn(hctx)
	}
	cancel()
	if err != nil {
		slog.Error("server stopped with error", "error", err)
		return 1
	}
	slog.Info("shutdown complete")
	return 0
}
