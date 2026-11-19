package httpx

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// Check là một kiểm tra sẵn sàng; trả lỗi nếu phụ thuộc chưa dùng được.
type Check func(ctx context.Context) error

const readyTimeout = 2 * time.Second

// RegisterHealth gắn /healthz (process còn sống) và /readyz (phụ thuộc sẵn sàng).
func RegisterHealth(mux *http.ServeMux, checks map[string]Check) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
		defer cancel()

		status := http.StatusOK
		result := make(map[string]string, len(checks))
		for name, check := range checks {
			if err := check(ctx); err != nil {
				// Chi tiết lỗi chỉ vào log: endpoint này không cần xác thực.
				slog.WarnContext(ctx, "readiness check failed", "check", name, "error", err)
				result[name] = "unavailable"
				status = http.StatusServiceUnavailable
				continue
			}
			result[name] = "ok"
		}
		writeJSON(w, status, result)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
