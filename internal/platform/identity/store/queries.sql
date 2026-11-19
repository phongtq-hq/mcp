-- name: GetUserByIdentity :one
SELECT id, status FROM users WHERE idp_issuer = @idp_issuer AND idp_subject = @idp_subject;

-- name: InsertUser :exec
INSERT INTO users (id, idp_issuer, idp_subject, email, display_name)
VALUES (@id, @idp_issuer, @idp_subject, @email, @display_name)
ON CONFLICT (idp_issuer, idp_subject) DO NOTHING;

-- name: ListActiveOrgIDsForUser :many
-- Chạy với app.user_id đã đặt (policy own_memberships); trả về org ở mọi tenant của user.
SELECT org_id FROM memberships WHERE user_id = @user_id AND status = 'active' ORDER BY org_id LIMIT 100;

-- name: GetOrganizationStatus :one
-- Chạy trong transaction đã đặt tenant, ngay sau khi chọn org cho request.
SELECT status FROM organizations WHERE id = @id;

-- name: GetUserStatus :one
SELECT status FROM users WHERE id = @id;
