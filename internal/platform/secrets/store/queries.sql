-- name: NextVersion :one
SELECT (COALESCE(max(version), 0) + 1)::int AS next FROM secrets
WHERE org_id = @org_id AND name = @name AND owner_user_id IS NULL;

-- name: InsertSecret :exec
INSERT INTO secrets (id, org_id, name, kind, version, ciphertext, wrapped_dek, kek_ref, created_by)
VALUES (@id, @org_id, @name, @kind, @version, @ciphertext, @wrapped_dek, @kek_ref, sqlc.narg('created_by'));

-- name: GetSealed :one
-- Chỉ role của gateway đọc được hai cột bản mã.
SELECT id, org_id, kind, version, ciphertext, wrapped_dek, kek_ref FROM secrets
WHERE org_id = @org_id AND id = @id;
