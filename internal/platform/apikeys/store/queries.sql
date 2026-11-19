-- name: InsertKey :one
INSERT INTO api_keys (id, org_id, project_id, name, key_prefix, key_hash, role_name, created_by, expires_at)
VALUES (@id, @org_id, sqlc.narg('project_id'), @name, @key_prefix, @key_hash, @role_name, @created_by, @expires_at)
RETURNING id, org_id, project_id, name, key_prefix, role_name, created_by, expires_at, revoked_at, last_used_at, created_at;

-- name: BindRole :exec
SELECT api_key_bind_role(@key_id, @created_by);

-- name: BuiltinRolePermissions :one
SELECT permissions FROM roles WHERE org_id IS NULL AND name = @name;

-- name: GetKey :one
SELECT id, org_id, project_id, name, key_prefix, role_name, created_by, expires_at, revoked_at, last_used_at, created_at
FROM api_keys WHERE org_id = @org_id AND id = @id;

-- name: ListKeys :many
SELECT id, org_id, project_id, name, key_prefix, role_name, created_by, expires_at, revoked_at, last_used_at, created_at
FROM api_keys
WHERE org_id = @org_id
  AND (sqlc.narg('after_created_at')::timestamptz IS NULL
       OR (created_at, id) < (sqlc.narg('after_created_at')::timestamptz, sqlc.narg('after_id')::uuid))
ORDER BY created_at DESC, id DESC LIMIT @row_limit;

-- name: RevokeKey :execrows
UPDATE api_keys SET revoked_at = now() WHERE org_id = @org_id AND id = @id AND revoked_at IS NULL;

-- name: LookupByPrefix :one
-- Chạy trong transaction chỉ đặt app.api_key_prefix (policy key_lookup).
SELECT id, org_id, project_id, key_hash, created_by, expires_at, revoked_at, last_used_at FROM api_keys WHERE key_prefix = @key_prefix;

-- name: TouchKey :exec
UPDATE api_keys SET last_used_at = now()
WHERE org_id = @org_id AND id = @id AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute');

-- name: KeyUsable :one
-- Cho Gateway: khoá khởi tạo một execution còn dùng được hay không.
SELECT (revoked_at IS NULL AND expires_at > now())::bool AS usable, created_by FROM api_keys WHERE org_id = @org_id AND id = @id;
