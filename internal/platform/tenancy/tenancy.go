// Package tenancy giữ ngữ cảnh tenant của một request.
//
// Tenant được xác lập đúng một lần ở middleware xác thực (từ token, không từ
// input của người dùng) và đi theo context.Context. Mọi truy vấn vào bảng thuộc
// tenant chạy qua InTx.
package tenancy

import (
	"context"
	"errors"

	"github.com/google/uuid"
	pgxv5 "github.com/jackc/pgx/v5"

	sharedpgx "github.com/hqgroups/mcp/internal/shared/pgx"
)

// ErrNoTenant báo context chưa có tenant. Đây là lỗi lập trình hoặc request
// chưa qua xác thực; không bao giờ được xử lý bằng cách chạy truy vấn không tenant.
var ErrNoTenant = errors.New("tenancy: no organization in context")

type ctxKey struct{}

// WithOrg gắn organization vào context. Chỉ middleware xác thực và code
// đường riêng của job xuyên tenant được gọi hàm này.
func WithOrg(ctx context.Context, orgID uuid.UUID) context.Context {
	return context.WithValue(ctx, ctxKey{}, orgID)
}

// OrgFrom trả về organization trong context.
func OrgFrom(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(ctxKey{}).(uuid.UUID)
	if !ok || id == uuid.Nil {
		return uuid.Nil, false
	}
	return id, true
}

// InTx chạy fn trong transaction đã đặt tenant lấy từ context.
func InTx(ctx context.Context, db sharedpgx.Beginner, fn func(tx pgxv5.Tx) error) error {
	orgID, ok := OrgFrom(ctx)
	if !ok {
		return ErrNoTenant
	}
	return sharedpgx.WithTenantTx(ctx, db, orgID, fn)
}
