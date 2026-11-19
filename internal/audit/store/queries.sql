-- name: AppendEvent :one
-- Đường ghi duy nhất: hàm trong database tự lấy thời điểm, tính hash và tuần tự hoá.
SELECT audit_append(@org_id::uuid, sqlc.narg('project_id')::uuid, @actor_type::text, sqlc.narg('actor_id')::uuid,
  sqlc.narg('on_behalf_of')::uuid, @action::text, @resource_type::text, sqlc.narg('resource_id')::uuid,
  @outcome::text, sqlc.narg('execution_id')::uuid, @details::jsonb, sqlc.narg('ip')::text::inet,
  sqlc.narg('trace_id')::text)::uuid AS id;

-- name: Verify :one
SELECT audit_verify(@org_id::uuid)::bigint AS records;

-- name: ListAuditLogs :many
-- Mới nhất trước; phân trang keyset theo (occurred_at, id). Khoảng thời gian luôn có
-- cận, để chỉ chạm các phân vùng tháng cần thiết.
SELECT id, project_id, actor_type, actor_id, on_behalf_of, action, resource_type, resource_id, outcome,
       execution_id, details, COALESCE(host(ip), '')::text AS ip, trace_id, occurred_at
FROM audit_logs
WHERE org_id = @org_id AND occurred_at >= @from_time AND occurred_at < @to_time
  AND (sqlc.narg('actor_id')::uuid IS NULL OR actor_id = sqlc.narg('actor_id'))
  AND (sqlc.narg('actor_type')::text IS NULL OR actor_type = sqlc.narg('actor_type'))
  AND (sqlc.narg('action_prefix')::text IS NULL OR action LIKE sqlc.narg('action_prefix') || '%')
  AND (sqlc.narg('resource_type')::text IS NULL OR resource_type = sqlc.narg('resource_type'))
  AND (sqlc.narg('resource_id')::uuid IS NULL OR resource_id = sqlc.narg('resource_id'))
  AND (sqlc.narg('execution_id')::uuid IS NULL OR execution_id = sqlc.narg('execution_id')
       OR details->>'execution_id' = sqlc.narg('execution_id')::uuid::text
       OR (resource_type = 'execution' AND resource_id = sqlc.narg('execution_id')))
  AND (sqlc.narg('outcome')::text IS NULL OR outcome = sqlc.narg('outcome'))
  AND (sqlc.narg('project_id')::uuid IS NULL OR project_id = sqlc.narg('project_id'))
  AND (sqlc.narg('after_time')::timestamptz IS NULL
       OR (occurred_at, id) < (sqlc.narg('after_time')::timestamptz, sqlc.narg('after_id')::uuid))
ORDER BY occurred_at DESC, id DESC LIMIT @row_limit;
