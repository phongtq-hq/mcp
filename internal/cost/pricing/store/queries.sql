-- name: LockPricingScope :exec
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg('lock_key')::text, 0));

-- name: EffectiveStart :one
-- Thời điểm bắt đầu thực tế của giá mới: không sớm hơn "bây giờ" của database.
SELECT GREATEST(sqlc.arg('requested')::timestamptz, now())::timestamptz AS effective_from;

-- name: GetOpenPricing :one
-- Dòng giá riêng của org đang mở (chưa có effective_to) cho model.
SELECT id, version, effective_from FROM model_pricing
WHERE org_id = @org_id AND model_id = @model_id AND effective_to IS NULL;

-- name: MaxPricingVersion :one
SELECT COALESCE(max(version), 0)::int FROM model_pricing WHERE org_id = @org_id AND model_id = @model_id;

-- name: ClosePricing :exec
UPDATE model_pricing SET effective_to = @effective_to WHERE org_id = @org_id AND id = @id AND effective_to IS NULL;

-- name: InsertPricing :one
INSERT INTO model_pricing (id, model_id, org_id, model_scope_org_id, version, currency,
  input_per_mtok, output_per_mtok, cached_input_per_mtok, cache_write_per_mtok, reasoning_per_mtok,
  effective_from, source, created_by)
VALUES (@id, @model_id, @org_id, @model_scope_org_id, @version, @currency,
  CAST(@input_per_mtok AS text)::numeric, CAST(@output_per_mtok AS text)::numeric,
  CAST(sqlc.narg('cached_input_per_mtok') AS text)::numeric, CAST(sqlc.narg('cache_write_per_mtok') AS text)::numeric,
  CAST(sqlc.narg('reasoning_per_mtok') AS text)::numeric, @effective_from, @source, @created_by)
RETURNING id, model_id, org_id, version, currency,
  input_per_mtok::text AS input_per_mtok, output_per_mtok::text AS output_per_mtok,
  COALESCE(cached_input_per_mtok::text, '')::text AS cached_input_per_mtok, COALESCE(cache_write_per_mtok::text, '')::text AS cache_write_per_mtok,
  COALESCE(reasoning_per_mtok::text, '')::text AS reasoning_per_mtok, effective_from, effective_to, source;

-- name: ListPricing :many
-- Lịch sử giá của một model mà org thấy được: giá riêng của org và giá niêm yết.
SELECT id, model_id, org_id, version, currency,
  input_per_mtok::text AS input_per_mtok, output_per_mtok::text AS output_per_mtok,
  COALESCE(cached_input_per_mtok::text, '')::text AS cached_input_per_mtok, COALESCE(cache_write_per_mtok::text, '')::text AS cache_write_per_mtok,
  COALESCE(reasoning_per_mtok::text, '')::text AS reasoning_per_mtok, effective_from, effective_to, source
FROM model_pricing
WHERE model_id = @model_id AND (org_id = @org_id OR org_id IS NULL)
  AND (sqlc.narg('effective_at')::timestamptz IS NULL
       OR (effective_from <= sqlc.narg('effective_at') AND (effective_to IS NULL OR effective_to > sqlc.narg('effective_at'))))
  AND (sqlc.narg('after_from')::timestamptz IS NULL
       OR (effective_from, id) < (sqlc.narg('after_from'), sqlc.narg('after_id')::uuid))
ORDER BY effective_from DESC, id DESC LIMIT @row_limit;

-- name: PriceAt :one
-- Giá hiệu lực tại một thời điểm: ưu tiên giá riêng của org, sau đó giá niêm yết.
SELECT id, model_id, org_id, version, currency,
  input_per_mtok::text AS input_per_mtok, output_per_mtok::text AS output_per_mtok,
  COALESCE(cached_input_per_mtok::text, '')::text AS cached_input_per_mtok, COALESCE(cache_write_per_mtok::text, '')::text AS cache_write_per_mtok,
  COALESCE(reasoning_per_mtok::text, '')::text AS reasoning_per_mtok, effective_from, effective_to, source
FROM model_pricing
WHERE model_id = @model_id AND (org_id = @org_id OR org_id IS NULL)
  AND effective_from <= @at AND (effective_to IS NULL OR effective_to > @at)
ORDER BY (org_id IS NULL) ASC LIMIT 1;

-- name: ModelsWithCurrentPricing :many
SELECT DISTINCT model_id FROM model_pricing
WHERE model_id = ANY(@model_ids::uuid[]) AND (org_id = @org_id OR org_id IS NULL)
  AND effective_from <= now() AND (effective_to IS NULL OR effective_to > now());
