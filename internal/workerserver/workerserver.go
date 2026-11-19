// Package workerserver là composition root của process worker: nối các bước của Agent
// Runtime với Temporal. Worker không nhận quyền nào với secret (ADR-0008): nó chỉ giữ
// khoá ký execution token, và mọi lời gọi ra ngoài đi qua MCP Gateway.
package workerserver

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/sdk/client"

	"github.com/hqgroups/mcp/internal/agent/runtime/durable"
	"github.com/hqgroups/mcp/internal/ai/provider"
	"github.com/hqgroups/mcp/internal/runtimewire"
)

// Config là cấu hình của worker.
type Config struct {
	// GatewayURL là URL nội bộ của MCP Gateway.
	GatewayURL string
	// ExecutionTokenKey ký execution token cho từng lời gọi tới Gateway.
	ExecutionTokenKey []byte
	// LocalProviders là adapter LLM chạy ngay trong process (claude-cli, chỉ local).
	LocalProviders map[string]provider.Provider
}

// Start khởi động worker của shared runtime trên kết nối Temporal đã có. Hàm stop dừng
// nhận việc mới và chờ activity đang chạy ghi xong; phần chưa xong được Temporal giao
// lại cho worker khác. Người gọi đóng client sau khi stop.
func Start(c client.Client, db *pgxpool.Pool, cfg Config) (stop func(), err error) {
	if db == nil {
		return nil, errors.New("workerserver: thiếu kết nối database")
	}
	steps, err := runtimewire.New(db, cfg.GatewayURL, cfg.ExecutionTokenKey, cfg.LocalProviders, nil)
	if err != nil {
		return nil, err
	}
	w := durable.NewWorker(c, steps)
	if err := w.Start(); err != nil {
		return nil, fmt.Errorf("workerserver: không khởi động được Temporal worker: %w", err)
	}
	return w.Stop, nil
}
