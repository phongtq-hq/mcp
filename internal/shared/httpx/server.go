package httpx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	apperr "github.com/hqgroups/mcp/internal/shared/errors"
)

// MaxBodyBytes là trần mặc định cho thân request.
const MaxBodyBytes = 1 << 20

// Serve chạy HTTP server cho tới khi ctx bị huỷ, rồi ngừng nhận kết nối mới
// và chờ request đang xử lý tối đa shutdownTimeout.
func Serve(ctx context.Context, addr string, h http.Handler, shutdownTimeout time.Duration) error {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	return ServeListener(ctx, ln, h, shutdownTimeout)
}

// ServeListener giống Serve nhưng nhận listener có sẵn.
func ServeListener(ctx context.Context, ln net.Listener, h http.Handler, shutdownTimeout time.Duration) error {
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	slog.InfoContext(ctx, "http server listening", "addr", ln.Addr().String())

	select {
	case err := <-errc:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		_ = srv.Close()
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

// Recover biến panic trong handler thành lỗi 500 dạng problem+json.
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler { //nolint:errorlint // giá trị sentinel của net/http
					panic(v)
				}
				WriteError(w, r, apperr.Wrap(fmt.Errorf("panic: %v", v),
					apperr.KindInternal, "internal_error", ""))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// LimitBody giới hạn kích thước thân request.
func LimitBody(n int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, n)
			next.ServeHTTP(w, r)
		})
	}
}
