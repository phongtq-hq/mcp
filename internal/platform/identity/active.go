package identity

import (
	"context"
	"errors"
	"slices"

	"github.com/google/uuid"
	pgxv5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hqgroups/mcp/internal/platform/identity/store"
	apperr "github.com/hqgroups/mcp/internal/shared/errors"
	sharedpgx "github.com/hqgroups/mcp/internal/shared/pgx"
)

// ActiveChecker kiểm một principal đã biết (không qua token OIDC) còn được phục vụ
// hay không. Dùng cho đường Agent nội bộ của Gateway: execution chạy thay mặt một
// người, và không được sống lâu hơn quyền của người đó.
type ActiveChecker struct{ db *pgxpool.Pool }

// NewActiveChecker tạo ActiveChecker.
func NewActiveChecker(db *pgxpool.Pool) *ActiveChecker { return &ActiveChecker{db: db} }

// RequireActive trả lỗi nếu user bị vô hiệu hoá, không còn là thành viên đang hoạt
// động của org, hoặc org bị đình chỉ / lưu trữ. Cùng các điều kiện mà middleware
// xác thực áp dụng cho mỗi request có token OIDC.
func (c *ActiveChecker) RequireActive(ctx context.Context, userID, orgID uuid.UUID) error {
	status, err := store.New(c.db).GetUserStatus(ctx, userID)
	if errors.Is(err, pgxv5.ErrNoRows) || (err == nil && status != "active") {
		return errUserDisabled
	}
	if err != nil {
		return apperr.Wrap(err, apperr.KindInternal, "internal_error", "")
	}
	var orgs []uuid.UUID
	if err := sharedpgx.WithUserTx(ctx, c.db, userID, func(tx pgxv5.Tx) (err error) {
		orgs, err = store.New(tx).ListActiveOrgIDsForUser(ctx, userID)
		return err
	}); err != nil {
		return apperr.Wrap(err, apperr.KindInternal, "internal_error", "")
	}
	if !slices.Contains(orgs, orgID) {
		return errOrgRequired
	}
	return c.RequireOrgActive(ctx, orgID)
}

// RequireOrgActive trả lỗi nếu organization bị đình chỉ hoặc lưu trữ.
func (c *ActiveChecker) RequireOrgActive(ctx context.Context, orgID uuid.UUID) error {
	var orgStatus string
	err := sharedpgx.WithTenantTx(ctx, c.db, orgID, func(tx pgxv5.Tx) (err error) {
		orgStatus, err = store.New(tx).GetOrganizationStatus(ctx, orgID)
		return err
	})
	if errors.Is(err, pgxv5.ErrNoRows) || (err == nil && orgStatus != "active") {
		return errOrgInactive
	}
	if err != nil {
		return apperr.Wrap(err, apperr.KindInternal, "internal_error", "")
	}
	return nil
}
