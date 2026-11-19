// Package pgx chứa phần truy cập PostgreSQL dùng chung: tạo pool và
// transaction có ngữ cảnh tenant cho Row-Level Security (ADR-0005).
package pgx

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	pgxv5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NewPool tạo connection pool và kiểm tra kết nối.
func NewPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		// Không bọc lỗi gốc: thông báo parse của pgx có thể chứa DSN kèm mật khẩu.
		return nil, fmt.Errorf("parse database url: invalid format")
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}

// RequireRLSEnforced trả lỗi nếu role hiện tại của kết nối là superuser hoặc có
// BYPASSRLS. Với role như vậy PostgreSQL bỏ qua mọi policy, kể cả FORCE, và lớp
// cách ly tenant ở database biến mất mà không có dấu hiệu nào (ADR-0005).
// Ba process ứng dụng gọi hàm này lúc khởi động và từ chối chạy nếu lỗi.
func RequireRLSEnforced(ctx context.Context, pool *pgxpool.Pool) error {
	var super, bypass bool
	var role string
	err := pool.QueryRow(ctx,
		`SELECT rolname, rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).
		Scan(&role, &super, &bypass)
	if err != nil {
		return fmt.Errorf("check database role: %w", err)
	}
	if super || bypass {
		return fmt.Errorf("database role %q bypasses row-level security (superuser=%v, bypassrls=%v): "+
			"process ứng dụng phải kết nối bằng tài khoản thuộc app_api / app_gateway / app_worker", role, super, bypass)
	}
	return nil
}

// Beginner là thứ mở được transaction (pool hoặc connection).
type Beginner interface {
	Begin(ctx context.Context) (pgxv5.Tx, error)
}

// WithTenantTx chạy fn trong một transaction đã đặt app.org_id.
//
// Giá trị được đặt với phạm vi transaction (tương đương SET LOCAL), nên an toàn
// với connection pool: connection trả về pool không còn mang tenant.
// Commit nếu fn trả nil; ngược lại rollback.
//
// Đây là hàm mức thấp. Code nghiệp vụ dùng tenancy.InTx, nơi org được lấy từ
// context thay vì do người gọi tự truyền.
func WithTenantTx(ctx context.Context, db Beginner, orgID uuid.UUID, fn func(tx pgxv5.Tx) error) error {
	return withLocalTx(ctx, db, "app.org_id", orgID, fn)
}

// WithUserTx chạy fn trong một transaction đã đặt app.user_id và KHÔNG đặt tenant.
// Chỉ dùng ở bước xác thực, khi chưa biết org: policy own_memberships cho phép
// user đọc membership của chính mình để chọn tenant. Mọi bảng tenant khác
// không trả về dòng nào trong transaction này.
func WithUserTx(ctx context.Context, db Beginner, userID uuid.UUID, fn func(tx pgxv5.Tx) error) error {
	return withLocalTx(ctx, db, "app.user_id", userID, fn)
}

func withLocalTx(ctx context.Context, db Beginner, setting string, id uuid.UUID, fn func(tx pgxv5.Tx) error) error {
	if id == uuid.Nil {
		return fmt.Errorf("%s tx: nil id", setting)
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	// Rollback sau Commit là no-op; bảo đảm rollback cả khi fn panic.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if _, err := tx.Exec(ctx, "SELECT set_config($1, $2, true)", setting, id.String()); err != nil {
		return fmt.Errorf("set %s: %w", setting, err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
