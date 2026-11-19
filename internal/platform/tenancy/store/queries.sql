-- name: GetOrganization :one
SELECT * FROM organizations WHERE id = @id;

-- name: UpdateOrganization :one
UPDATE organizations SET
  name       = COALESCE(sqlc.narg('name'), name),
  settings   = COALESCE(sqlc.narg('settings'), settings),
  updated_at = clock_timestamp()
WHERE id = @id
  AND (sqlc.narg('if_updated_at')::timestamptz IS NULL OR updated_at = sqlc.narg('if_updated_at'))
RETURNING *;

-- name: CreateProject :one
INSERT INTO projects (id, org_id, slug, name, environment, settings)
VALUES (@id, @org_id, @slug, @name, @environment, @settings)
RETURNING *;

-- name: GetProject :one
SELECT * FROM projects WHERE org_id = @org_id AND id = @id;

-- name: UpdateProject :one
UPDATE projects SET
  name       = COALESCE(sqlc.narg('name'), name),
  settings   = COALESCE(sqlc.narg('settings'), settings),
  updated_at = clock_timestamp()
WHERE org_id = @org_id AND id = @id AND archived_at IS NULL
  AND (sqlc.narg('if_updated_at')::timestamptz IS NULL OR updated_at = sqlc.narg('if_updated_at'))
RETURNING *;

-- name: ArchiveProject :execrows
UPDATE projects SET archived_at = clock_timestamp(), updated_at = clock_timestamp()
WHERE org_id = @org_id AND id = @id AND archived_at IS NULL;

-- name: ListProjectsByCreatedDesc :many
SELECT * FROM projects
WHERE org_id = @org_id AND archived_at IS NULL
  AND (sqlc.narg('environment')::text IS NULL OR environment = sqlc.narg('environment'))
  AND (sqlc.narg('only_ids')::uuid[] IS NULL OR id = ANY(sqlc.narg('only_ids')::uuid[]))
  AND (sqlc.narg('after_created')::timestamptz IS NULL
       OR (created_at, id) < (sqlc.narg('after_created'), sqlc.narg('after_id')::uuid))
ORDER BY created_at DESC, id DESC LIMIT @row_limit;

-- name: ListProjectsByCreatedAsc :many
SELECT * FROM projects
WHERE org_id = @org_id AND archived_at IS NULL
  AND (sqlc.narg('environment')::text IS NULL OR environment = sqlc.narg('environment'))
  AND (sqlc.narg('only_ids')::uuid[] IS NULL OR id = ANY(sqlc.narg('only_ids')::uuid[]))
  AND (sqlc.narg('after_created')::timestamptz IS NULL
       OR (created_at, id) > (sqlc.narg('after_created'), sqlc.narg('after_id')::uuid))
ORDER BY created_at ASC, id ASC LIMIT @row_limit;

-- name: ListProjectsByNameAsc :many
SELECT * FROM projects
WHERE org_id = @org_id AND archived_at IS NULL
  AND (sqlc.narg('environment')::text IS NULL OR environment = sqlc.narg('environment'))
  AND (sqlc.narg('only_ids')::uuid[] IS NULL OR id = ANY(sqlc.narg('only_ids')::uuid[]))
  AND (sqlc.narg('after_name')::text IS NULL
       OR (name, id) > (sqlc.narg('after_name'), sqlc.narg('after_id')::uuid))
ORDER BY name ASC, id ASC LIMIT @row_limit;

-- name: ListProjectsByNameDesc :many
SELECT * FROM projects
WHERE org_id = @org_id AND archived_at IS NULL
  AND (sqlc.narg('environment')::text IS NULL OR environment = sqlc.narg('environment'))
  AND (sqlc.narg('only_ids')::uuid[] IS NULL OR id = ANY(sqlc.narg('only_ids')::uuid[]))
  AND (sqlc.narg('after_name')::text IS NULL
       OR (name, id) < (sqlc.narg('after_name'), sqlc.narg('after_id')::uuid))
ORDER BY name DESC, id DESC LIMIT @row_limit;
