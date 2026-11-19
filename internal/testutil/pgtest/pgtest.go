// Package pgtest dựng một PostgreSQL thật (testcontainers) đã chạy toàn bộ
// migration, cho test tích hợp. Không mock database: RLS, khoá ngoại ghép và
// các ràng buộc chỉ kiểm được trên database thật.
package pgtest

import (
	"context"
	"database/sql"
	"testing"
	"time"

	pgxv5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/hqgroups/mcp/migrations"
)

// Image phải từ PostgreSQL 18 trở lên (cần uuidv7()).
const Image = "postgres:18-alpine"

// DB là một database test đã migrate.
type DB struct {
	// Admin kết nối bằng superuser: chỉ dùng để chuẩn bị dữ liệu và kiểm tra
	// kết quả vượt qua RLS. Code được test phải dùng PoolAs.
	Admin *pgxpool.Pool
	dsn   string
}

// Start khởi động container, chạy migration và dọn dẹp khi test kết thúc.
// Bị bỏ qua với `go test -short`.
func Start(t testing.TB) *DB {
	t.Helper()
	if testing.Short() {
		t.Skip("bỏ qua test cần PostgreSQL (-short)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	ctr, err := postgres.Run(ctx, Image,
		postgres.WithDatabase("hq_test"),
		postgres.WithUsername("postgres"),
		postgres.WithPassword("postgres"),
		postgres.BasicWaitStrategies(),
	)
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	migrate(ctx, t, dsn)

	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}
	t.Cleanup(admin.Close)
	return &DB{Admin: admin, dsn: dsn}
}

func migrate(ctx context.Context, t testing.TB, dsn string) {
	t.Helper()
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()
	if err := migrations.Up(ctx, sqlDB); err != nil {
		t.Fatal(err)
	}
}

// PoolAs trả về pool mà mọi connection chạy dưới role ứng dụng đã cho
// (app_api, app_gateway, app_worker), tức là chịu RLS như production.
func (d *DB) PoolAs(t testing.TB, role string, maxConns int32) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(d.dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.MaxConns = maxConns
	setRole := "SET ROLE " + pgxv5.Identifier{role}.Sanitize()
	cfg.AfterConnect = func(ctx context.Context, c *pgxv5.Conn) error {
		_, err := c.Exec(ctx, setRole)
		return err
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pool as %s: %v", role, err)
	}
	t.Cleanup(pool.Close)
	return pool
}
