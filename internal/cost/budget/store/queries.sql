-- name: InsertBudget :one
INSERT INTO budgets (id, org_id, scope_type, scope_id, period, limit_amount, currency, soft_threshold_pct, hard_action, created_by)
VALUES (@id, @org_id, @scope_type, @scope_id, @period, CAST(@limit_amount AS text)::numeric, @currency, sqlc.narg('soft_threshold_pct'), @hard_action, sqlc.narg('created_by'))
RETURNING id, org_id, scope_type, scope_id, period, limit_amount::text AS limit_amount, currency, soft_threshold_pct, hard_action, enabled, created_at, updated_at;

-- name: GetBudget :one
SELECT id, org_id, scope_type, scope_id, period, limit_amount::text AS limit_amount, currency, soft_threshold_pct, hard_action, enabled, created_at, updated_at
FROM budgets WHERE org_id = @org_id AND id = @id;

-- name: ListBudgets :many
SELECT id, org_id, scope_type, scope_id, period, limit_amount::text AS limit_amount, currency, soft_threshold_pct, hard_action, enabled, created_at, updated_at
FROM budgets
WHERE org_id = @org_id
  AND (sqlc.narg('scope_type')::text IS NULL OR scope_type = sqlc.narg('scope_type'))
  AND (sqlc.narg('scope_id')::uuid IS NULL OR scope_id = sqlc.narg('scope_id'))
  AND (sqlc.narg('after_created')::timestamptz IS NULL OR (created_at, id) < (sqlc.narg('after_created'), sqlc.narg('after_id')::uuid))
ORDER BY created_at DESC, id DESC LIMIT @row_limit;

-- name: UpdateBudget :one
UPDATE budgets SET
  limit_amount = COALESCE(CAST(sqlc.narg('limit_amount') AS text)::numeric, limit_amount),
  soft_threshold_pct = CASE WHEN @set_soft::bool THEN sqlc.narg('soft_threshold_pct') ELSE soft_threshold_pct END,
  hard_action = COALESCE(sqlc.narg('hard_action'), hard_action),
  enabled = COALESCE(sqlc.narg('enabled'), enabled),
  updated_at = now()
WHERE org_id = @org_id AND id = @id
  AND (sqlc.narg('if_updated_at')::timestamptz IS NULL OR updated_at = sqlc.narg('if_updated_at'))
RETURNING id, org_id, scope_type, scope_id, period, limit_amount::text AS limit_amount, currency, soft_threshold_pct, hard_action, enabled, created_at, updated_at;

-- name: DeleteBudget :one
DELETE FROM budgets WHERE org_id = @org_id AND id = @id
RETURNING scope_type, scope_id, period, limit_amount::text AS limit_amount;

-- name: ApplicableBudgets :many
-- Mọi budget đang bật áp cho một lời gọi: của organization, của project, của user và
-- của agent. Một lời gọi phải thoả tất cả.
SELECT id, scope_type, scope_id, period, limit_amount::text AS limit_amount, currency, soft_threshold_pct, hard_action
FROM budgets
WHERE org_id = @org_id AND enabled
  AND (scope_type = 'organization'
    OR (scope_type = 'project' AND scope_id = @project_id)
    OR (scope_type = 'agent' AND scope_id = @agent_id)
    OR (scope_type = 'user' AND scope_id = sqlc.narg('user_id')::uuid))
ORDER BY id LIMIT 64;

-- Mức đã tiêu của một phạm vi kể từ đầu kỳ, theo ledger (nguồn sự thật). Truy vấn báo
-- cáo của Cost trên bảng của chính nó. Mỗi phạm vi một truy vấn để planner luôn dùng
-- được index của phạm vi đó. Tổng âm (bản ghi điều chỉnh) được coi là 0.

-- name: SpentSinceOrg :one
SELECT GREATEST(COALESCE(sum(estimated_cost), 0), 0)::text AS spent FROM usage_records
WHERE org_id = @org_id AND occurred_at >= @since AND currency = 'USD';

-- name: SpentSinceProject :one
SELECT GREATEST(COALESCE(sum(estimated_cost), 0), 0)::text AS spent FROM usage_records
WHERE org_id = @org_id AND project_id = @scope_id AND occurred_at >= @since AND currency = 'USD';

-- name: SpentSinceAgent :one
SELECT GREATEST(COALESCE(sum(estimated_cost), 0), 0)::text AS spent FROM usage_records
WHERE org_id = @org_id AND agent_id = @scope_id AND occurred_at >= @since AND currency = 'USD';

-- name: SpentSinceUser :one
SELECT GREATEST(COALESCE(sum(estimated_cost), 0), 0)::text AS spent FROM usage_records
WHERE org_id = @org_id AND user_id = @scope_id AND occurred_at >= @since AND currency = 'USD';
