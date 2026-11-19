// Command migrate áp dụng migration đã nhúng. Đây là công cụ chạy như một bước
// riêng trước khi deploy, không phải một process của platform: ba process
// (api, gateway, worker) không bao giờ tự chạy migration khi khởi động.
//
// Dùng: migrate up | down | status
// Kết nối lấy từ HQ_MIGRATE_DATABASE_URL. Tài khoản phải là superuser, hoặc có
// CREATEROLE và BYPASSRLS: migration tạo role và ghi dòng dùng chung của platform
// vào bảng đã FORCE ROW LEVEL SECURITY.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/hqgroups/mcp/migrations"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: migrate up|down|status")
	}
	dsn := os.Getenv("HQ_MIGRATE_DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("HQ_MIGRATE_DATABASE_URL is required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		// Không in lỗi gốc: có thể chứa DSN kèm mật khẩu.
		return fmt.Errorf("invalid HQ_MIGRATE_DATABASE_URL")
	}
	defer func() { _ = db.Close() }()

	p, err := migrations.NewProvider(db)
	if err != nil {
		return err
	}
	switch args[0] {
	case "up":
		results, err := p.Up(ctx)
		for _, r := range results {
			fmt.Println(r)
		}
		if err != nil {
			return err
		}
		fmt.Printf("đã áp dụng %d migration\n", len(results))
	case "down": // lùi đúng một migration; chỉ dùng trong phát triển
		r, err := p.Down(ctx)
		if err != nil {
			return err
		}
		fmt.Println(r)
	case "status":
		st, err := p.Status(ctx)
		if err != nil {
			return err
		}
		for _, s := range st {
			fmt.Printf("%-8s %s\n", s.State, s.Source.Path)
		}
	default:
		return fmt.Errorf("unknown command %q (up|down|status)", args[0])
	}
	return nil
}
