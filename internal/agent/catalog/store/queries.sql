-- name: InsertAgent :exec
INSERT INTO agents (id, org_id, project_id, slug, name, description, created_by)
VALUES (@id, @org_id, @project_id, @slug, @name, @description, @created_by);

-- name: GetAgent :one
SELECT sqlc.embed(a), av.version AS active_version,
       COALESCE((SELECT max(v.version) FROM agent_versions v WHERE v.org_id = a.org_id AND v.agent_id = a.id), 0)::int AS latest_version
FROM agents a
LEFT JOIN agent_versions av ON av.org_id = a.org_id AND av.id = a.active_version_id
WHERE a.org_id = @org_id AND a.id = @id;

-- name: LockAgent :one
-- Khoá dòng agent để tuần tự hoá việc tạo version và đổi trạng thái.
SELECT id, project_id, status, active_version_id, updated_at FROM agents
WHERE org_id = @org_id AND id = @id FOR UPDATE;

-- name: UpdateAgent :exec
UPDATE agents SET
  name        = COALESCE(sqlc.narg('name'), name),
  description = COALESCE(sqlc.narg('description'), description),
  status      = COALESCE(sqlc.narg('status'), status),
  updated_at  = clock_timestamp()
WHERE org_id = @org_id AND id = @id;

-- name: SetActiveVersion :exec
UPDATE agents SET active_version_id = @active_version_id, status = @status, updated_at = clock_timestamp()
WHERE org_id = @org_id AND id = @id;

-- name: GetLatestAgentVersion :one
SELECT * FROM agent_versions WHERE org_id = @org_id AND agent_id = @agent_id ORDER BY version DESC LIMIT 1;

-- name: InsertAgentVersion :one
INSERT INTO agent_versions (id, org_id, agent_id, version, system_prompt, config, config_hash, change_note, created_by)
VALUES (@id, @org_id, @agent_id, @version, @system_prompt, @config, @config_hash, @change_note, @created_by)
RETURNING *;

-- name: GetAgentVersion :one
SELECT * FROM agent_versions WHERE org_id = @org_id AND agent_id = @agent_id AND version = @version;

-- name: ListAgentVersions :many
SELECT * FROM agent_versions
WHERE org_id = @org_id AND agent_id = @agent_id
  AND (sqlc.narg('before_version')::int IS NULL OR version < sqlc.narg('before_version'))
ORDER BY version DESC LIMIT @row_limit;

-- name: ListAgentsByName :many
-- Một truy vấn cho cả hai chiều: @descending đảo điều kiện keyset và thứ tự.
SELECT sqlc.embed(a), av.version AS active_version,
       COALESCE((SELECT max(v.version) FROM agent_versions v WHERE v.org_id = a.org_id AND v.agent_id = a.id), 0)::int AS latest_version
FROM agents a
LEFT JOIN agent_versions av ON av.org_id = a.org_id AND av.id = a.active_version_id
WHERE a.org_id = @org_id AND a.project_id = @project_id
  AND (sqlc.narg('status')::text IS NULL OR a.status = sqlc.narg('status'))
  AND (sqlc.narg('status')::text IS NOT NULL OR a.status <> 'archived')
  AND (sqlc.narg('search')::text IS NULL OR a.name ILIKE sqlc.narg('search') OR a.slug ILIKE sqlc.narg('search'))
  AND (sqlc.narg('after_name')::text IS NULL OR
       CASE WHEN @descending::bool THEN (a.name, a.id) < (sqlc.narg('after_name'), sqlc.narg('after_id')::uuid)
            ELSE (a.name, a.id) > (sqlc.narg('after_name'), sqlc.narg('after_id')::uuid) END)
ORDER BY CASE WHEN @descending::bool THEN a.name END DESC, CASE WHEN @descending::bool THEN a.id END DESC,
         CASE WHEN NOT @descending::bool THEN a.name END ASC, CASE WHEN NOT @descending::bool THEN a.id END ASC
LIMIT @row_limit;

-- name: ListAgentsByCreated :many
SELECT sqlc.embed(a), av.version AS active_version,
       COALESCE((SELECT max(v.version) FROM agent_versions v WHERE v.org_id = a.org_id AND v.agent_id = a.id), 0)::int AS latest_version
FROM agents a
LEFT JOIN agent_versions av ON av.org_id = a.org_id AND av.id = a.active_version_id
WHERE a.org_id = @org_id AND a.project_id = @project_id
  AND (sqlc.narg('status')::text IS NULL OR a.status = sqlc.narg('status'))
  AND (sqlc.narg('status')::text IS NOT NULL OR a.status <> 'archived')
  AND (sqlc.narg('search')::text IS NULL OR a.name ILIKE sqlc.narg('search') OR a.slug ILIKE sqlc.narg('search'))
  AND (sqlc.narg('after_created')::timestamptz IS NULL OR
       CASE WHEN @descending::bool THEN (a.created_at, a.id) < (sqlc.narg('after_created'), sqlc.narg('after_id')::uuid)
            ELSE (a.created_at, a.id) > (sqlc.narg('after_created'), sqlc.narg('after_id')::uuid) END)
ORDER BY CASE WHEN @descending::bool THEN a.created_at END DESC, CASE WHEN @descending::bool THEN a.id END DESC,
         CASE WHEN NOT @descending::bool THEN a.created_at END ASC, CASE WHEN NOT @descending::bool THEN a.id END ASC
LIMIT @row_limit;

-- name: GetAgentVersionByID :one
SELECT v.config, a.status AS agent_status FROM agent_versions v
JOIN agents a ON a.id = v.agent_id AND a.org_id = v.org_id
WHERE v.org_id = @org_id AND v.id = @id;

-- name: ActiveAgentsUsingServer :one
-- Số Agent đang hoạt động mà version đang hoạt động tham chiếu một MCP server.
SELECT count(*)::int AS n FROM agents a
JOIN agent_versions v ON v.id = a.active_version_id AND v.org_id = a.org_id
WHERE a.org_id = @org_id AND a.status = 'active'
  AND EXISTS (SELECT 1 FROM jsonb_array_elements(CASE WHEN jsonb_typeof(v.config->'tools') = 'array' THEN v.config->'tools' ELSE '[]'::jsonb END) t
              WHERE t->>'server_id' = CAST(@server_id AS uuid)::text);
