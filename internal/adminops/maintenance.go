package adminops

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	pgxv5 "github.com/jackc/pgx/v5"
)

// MaintenanceResult là kết quả của một lần chạy bảo trì.
type MaintenanceResult struct {
	// Months là các tháng (ngày đầu tháng, UTC) đã được bảo đảm có phân vùng.
	Months []string
	// IdempotencyKeysDeleted là số idempotency key hết hạn đã xoá.
	IdempotencyKeysDeleted int64
	// AuditRowsInDefault là số dòng audit đang nằm ở phân vùng mặc định. Khác 0 nghĩa
	// là đã có lúc thiếu phân vùng tháng: lần chạy này đã chuyển được những dòng thuộc
	// các tháng nó tạo; phần còn lại cần người xem.
	AuditRowsInDefault int64
}

const (
	maxMaintenanceMonths = 36
	idempotencyBatch     = 5000
	idempotencyBatches   = 200 // trần mỗi lần chạy: một triệu dòng
)

// RunMaintenance làm các việc định kỳ xuyên tenant của database:
//
//   - bảo đảm có phân vùng tháng cho audit_logs, execution_steps và usage_records từ
//     tháng hiện tại tới monthsAhead tháng sau (hai bảng sau không có phân vùng mặc
//     định, thiếu phân vùng thì việc ghi lỗi);
//   - xoá idempotency key đã hết hạn, theo lô có trần.
//
// Hàm idempotent: chạy lại không tạo thêm gì. Tạo phân vùng chạy bằng tài khoản kết
// nối (chủ sở hữu bảng); xoá key chạy dưới app_admin. Lịch chạy do người vận hành đặt
// (cron) cho tới khi có Temporal Schedules.
func RunMaintenance(ctx context.Context, conn *pgxv5.Conn, monthsAhead int, now time.Time) (MaintenanceResult, error) {
	var res MaintenanceResult
	if monthsAhead < 1 || monthsAhead > maxMaintenanceMonths {
		return res, fmt.Errorf("months phải từ 1 đến %d", maxMaintenanceMonths)
	}
	first := time.Date(now.UTC().Year(), now.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i <= monthsAhead; i++ {
		month := first.AddDate(0, i, 0).Format("2006-01-02")
		// Mỗi tháng một transaction ngắn: khoá trên bảng cha được nhả giữa các tháng.
		// lock_timeout: ATTACH cần khoá trên phân vùng mặc định của audit_logs, và mọi
		// lần ghi audit xếp hàng sau lệnh đang chờ khoá đó. Không lấy được khoá trong 5
		// giây thì bỏ, lần chạy sau thử lại.
		if err := createPartitions(ctx, conn, month); err != nil {
			return res, fmt.Errorf("tạo phân vùng tháng %s: %w", month, err)
		}
		res.Months = append(res.Months, month)
	}

	for batch := 0; batch < idempotencyBatches; batch++ {
		var n int64
		err := adminTx(ctx, conn, "", func(tx pgxv5.Tx, _ *uuid.UUID) error {
			tag, err := tx.Exec(ctx, `DELETE FROM idempotency_keys WHERE ctid IN (
				SELECT ctid FROM idempotency_keys WHERE expires_at < now() LIMIT $1)`, idempotencyBatch)
			n = tag.RowsAffected()
			return err
		})
		if err != nil {
			return res, fmt.Errorf("dọn idempotency_keys: %w", err)
		}
		res.IdempotencyKeysDeleted += n
		if n < idempotencyBatch {
			break
		}
	}

	err := adminTx(ctx, conn, "", func(tx pgxv5.Tx, _ *uuid.UUID) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM public.audit_logs_default`).Scan(&res.AuditRowsInDefault)
	})
	if err != nil {
		return res, fmt.Errorf("đếm phân vùng mặc định của audit: %w", err)
	}
	return res, nil
}

func createPartitions(ctx context.Context, conn *pgxv5.Conn, month string) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = '5s'"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT audit_logs_create_partition($1::date), execution_create_partitions($1::date)`, month); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
