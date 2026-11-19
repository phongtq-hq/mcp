// Package adminops chứa thao tác quản trị xuyên tenant, chạy dưới role
// app_admin (BYPASSRLS). Đây là đường code riêng mà ADR-0005 yêu cầu: không
// process nào (api, gateway, worker) import package này; chỉ công cụ dòng lệnh
// do người vận hành chạy.
package adminops

import (
	"context"
	"fmt"
	"regexp"

	"github.com/google/uuid"
	pgxv5 "github.com/jackc/pgx/v5"

	"github.com/hqgroups/mcp/internal/audit"
	"github.com/hqgroups/mcp/internal/shared/ids"
)

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)

// BootstrapOrg mô tả một organization mới và quản trị viên đầu tiên của nó.
type BootstrapOrg struct {
	Slug, Name   string
	AdminIssuer  string // `iss` của IdP
	AdminSubject string // `sub` của quản trị viên trong IdP
	AdminEmail   string
}

// CreateOrganization tạo organization, user quản trị (nếu chưa có), membership
// và binding role org_admin, trong một transaction. Trả về id của organization.
func CreateOrganization(ctx context.Context, conn *pgxv5.Conn, in BootstrapOrg) (uuid.UUID, error) {
	if !slugRe.MatchString(in.Slug) || in.Name == "" || in.AdminIssuer == "" || in.AdminSubject == "" {
		return uuid.Nil, fmt.Errorf("bootstrap: slug, name, admin issuer và admin subject là bắt buộc; slug phải khớp %s", slugRe)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	// BYPASSRLS không kế thừa qua membership: phải chuyển role tường minh.
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_admin"); err != nil {
		return uuid.Nil, fmt.Errorf("bootstrap: tài khoản kết nối phải là thành viên của app_admin: %w", err)
	}

	orgID := ids.New()
	if _, err := tx.Exec(ctx, `INSERT INTO organizations (id, slug, name) VALUES ($1, $2, $3)`,
		orgID, in.Slug, in.Name); err != nil {
		return uuid.Nil, fmt.Errorf("bootstrap: tạo organization: %w", err)
	}
	var userID uuid.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO users (id, idp_issuer, idp_subject, email) VALUES ($1, $2, $3, $4)
		ON CONFLICT (idp_issuer, idp_subject) DO UPDATE SET updated_at = now()
		RETURNING id`, ids.New(), in.AdminIssuer, in.AdminSubject, in.AdminEmail).Scan(&userID); err != nil {
		return uuid.Nil, fmt.Errorf("bootstrap: tạo user: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO memberships (org_id, user_id) VALUES ($1, $2)`, orgID, userID); err != nil {
		return uuid.Nil, fmt.Errorf("bootstrap: tạo membership: %w", err)
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO role_bindings (org_id, role_id, principal_type, principal_id)
		SELECT $1, id, 'user', $2 FROM roles WHERE org_id IS NULL AND name = 'org_admin'`, orgID, userID)
	if err != nil || tag.RowsAffected() != 1 {
		return uuid.Nil, fmt.Errorf("bootstrap: gán role org_admin (rows=%d): %w", tag.RowsAffected(), err)
	}
	// Đường quản trị cũng để lại audit trong chuỗi của org mới (actor: system).
	// audit_append yêu cầu transaction mang đúng tenant của bản ghi.
	if _, err := tx.Exec(ctx, "SELECT set_config('app.org_id', $1, true)", orgID.String()); err != nil {
		return uuid.Nil, fmt.Errorf("bootstrap: %w", err)
	}
	if err := audit.Record(ctx, tx, audit.Event{
		OrgID: orgID, Action: "organization.create", ResourceType: "organization", ResourceID: &orgID,
		Outcome: audit.OutcomeSuccess, Details: map[string]string{"slug": in.Slug, "admin_user_id": userID.String()},
	}); err != nil {
		return uuid.Nil, fmt.Errorf("bootstrap: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, err
	}
	return orgID, nil
}
