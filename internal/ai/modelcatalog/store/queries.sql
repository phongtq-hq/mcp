-- name: InsertProvider :one
INSERT INTO model_providers (id, org_id, slug, adapter, settings)
VALUES (@id, @org_id, @slug, @adapter, @settings)
RETURNING id, org_id, slug, adapter, base_url, secret_id, enabled, settings, created_at, updated_at;

-- name: GetProvider :one
-- Thấy provider của org và provider của platform (org_id NULL).
SELECT id, org_id, slug, adapter, base_url, secret_id, enabled, settings, created_at, updated_at
FROM model_providers
WHERE id = @id AND (org_id = @org_id OR org_id IS NULL) AND archived_at IS NULL;

-- name: ListProviders :many
SELECT id, org_id, slug, adapter, base_url, secret_id, enabled, settings, created_at, updated_at
FROM model_providers
WHERE (org_id = @org_id OR org_id IS NULL) AND archived_at IS NULL
  AND (sqlc.narg('after_created')::timestamptz IS NULL
       OR (created_at, id) < (sqlc.narg('after_created'), sqlc.narg('after_id')::uuid))
ORDER BY created_at DESC, id DESC LIMIT @row_limit;

-- name: UpdateProvider :one
-- Chỉ provider của chính org; provider của platform không sửa được qua API tenant.
UPDATE model_providers SET
  enabled    = COALESCE(sqlc.narg('enabled'), enabled),
  settings   = COALESCE(sqlc.narg('settings'), settings),
  updated_at = clock_timestamp()
WHERE org_id = @org_id AND id = @id AND archived_at IS NULL
  AND (sqlc.narg('if_updated_at')::timestamptz IS NULL OR updated_at = sqlc.narg('if_updated_at'))
RETURNING id, org_id, slug, adapter, base_url, secret_id, enabled, settings, created_at, updated_at;

-- name: ArchiveProvider :execrows
UPDATE model_providers SET archived_at = clock_timestamp(), enabled = false, updated_at = clock_timestamp()
WHERE org_id = @org_id AND id = @id AND archived_at IS NULL;

-- name: CountModelsOfProvider :one
SELECT count(*) FROM models WHERE provider_id = @provider_id AND (org_id = @org_id OR org_id IS NULL);

-- name: InsertModel :one
INSERT INTO models (id, org_id, provider_id, provider_model, display_name, tier, capabilities)
VALUES (@id, @org_id, @provider_id, @provider_model, @display_name, @tier, @capabilities)
RETURNING id, org_id, provider_id, provider_model, display_name, tier, capabilities, enabled, deprecated_at, created_at, updated_at;

-- name: GetModel :one
SELECT id, org_id, provider_id, provider_model, display_name, tier, capabilities, enabled, deprecated_at, created_at, updated_at
FROM models
WHERE id = @id AND (org_id = @org_id OR org_id IS NULL);

-- name: SetProviderSecret :one
UPDATE model_providers SET secret_id = @secret_id, updated_at = clock_timestamp()
WHERE org_id = @org_id AND id = @id AND archived_at IS NULL
RETURNING id, org_id, slug, adapter, base_url, secret_id, enabled, settings, created_at, updated_at;

-- name: GetModelForUse :one
-- Model kèm trạng thái của provider, để kiểm tra có dùng được hay không.
SELECT m.id, m.provider_model, m.capabilities, m.enabled, m.deprecated_at, p.enabled AS provider_enabled, p.adapter,
       p.org_id AS provider_org_id, p.secret_id AS provider_secret_id
FROM models m
JOIN model_providers p ON p.id = m.provider_id AND p.scope_org_id = m.scope_org_id AND p.archived_at IS NULL
WHERE m.id = @id AND (m.org_id = @org_id OR m.org_id IS NULL);

-- name: ListModels :many
SELECT id, org_id, provider_id, provider_model, display_name, tier, capabilities, enabled, deprecated_at, created_at, updated_at
FROM models
WHERE (org_id = @org_id OR org_id IS NULL)
  AND (sqlc.narg('provider_id')::uuid IS NULL OR provider_id = sqlc.narg('provider_id'))
  AND (sqlc.narg('tier')::text IS NULL OR tier = sqlc.narg('tier'))
  AND (sqlc.narg('enabled')::bool IS NULL OR enabled = sqlc.narg('enabled'))
  AND (sqlc.narg('after_created')::timestamptz IS NULL
       OR (created_at, id) < (sqlc.narg('after_created'), sqlc.narg('after_id')::uuid))
ORDER BY created_at DESC, id DESC LIMIT @row_limit;

-- name: UpdateModel :one
UPDATE models SET
  display_name  = COALESCE(sqlc.narg('display_name'), display_name),
  tier          = COALESCE(sqlc.narg('tier'), tier),
  capabilities  = COALESCE(sqlc.narg('capabilities'), capabilities),
  enabled       = COALESCE(sqlc.narg('enabled'), enabled),
  deprecated_at = COALESCE(sqlc.narg('deprecated_at'), deprecated_at),
  updated_at    = clock_timestamp()
WHERE org_id = @org_id AND id = @id
RETURNING id, org_id, provider_id, provider_model, display_name, tier, capabilities, enabled, deprecated_at, created_at, updated_at;
