-- name: UpsertWebhook :one
INSERT INTO notification_webhooks (org_id, url, secret_id, enabled, created_by)
VALUES (@org_id, @url, @secret_id, true, @created_by)
ON CONFLICT (org_id) DO UPDATE
SET url = EXCLUDED.url, secret_id = EXCLUDED.secret_id, enabled = true, consecutive_failures = 0,
    disabled_reason = NULL, created_by = EXCLUDED.created_by, updated_at = now()
RETURNING org_id, url, secret_id, enabled, consecutive_failures, disabled_reason, created_by, created_at, updated_at;

-- name: GetWebhook :one
SELECT org_id, url, secret_id, enabled, consecutive_failures, disabled_reason, created_by, created_at, updated_at
FROM notification_webhooks WHERE org_id = @org_id;

-- name: DeleteWebhook :execrows
DELETE FROM notification_webhooks WHERE org_id = @org_id;

-- name: ListDeliveries :many
SELECT id, event_type, approval_id, status, attempts, next_attempt_at, last_http_status, last_error_code, created_at, finished_at
FROM notification_deliveries
WHERE org_id = @org_id
  AND (sqlc.narg(cursor_created_at)::timestamptz IS NULL OR (created_at, id) < (sqlc.narg(cursor_created_at)::timestamptz, sqlc.narg(cursor_id)::uuid))
ORDER BY created_at DESC, id DESC
LIMIT @row_limit;

-- name: ClaimDeliveries :many
SELECT id, org_id, approval_id, attempts FROM notification_claim(@batch::int);

-- name: GetDeliveryApproval :one
-- Dữ liệu của sự kiện, đọc lúc gửi. Chỉ định danh và phân loại: không tên tool, không
-- arguments, không thông điệp của cổng.
SELECT a.id, a.project_id, a.execution_id, a.subject_type, a.risk_level, a.required_approvals, a.status,
       a.expires_at, a.created_at, (a.expires_at <= now())::boolean AS expired
FROM approvals a WHERE a.org_id = @org_id AND a.id = @id;

-- name: FinishDelivery :execrows
UPDATE notification_deliveries
SET status = @status, finished_at = now(), last_http_status = sqlc.narg(http_status), last_error_code = sqlc.narg(error_code)
WHERE org_id = @org_id AND id = @id AND status = 'pending';

-- name: NoteDeliveryAttempt :execrows
UPDATE notification_deliveries
SET last_http_status = sqlc.narg(http_status), last_error_code = sqlc.narg(error_code)
WHERE org_id = @org_id AND id = @id AND status = 'pending';

-- name: ResetWebhookFailures :exec
UPDATE notification_webhooks SET consecutive_failures = 0, updated_at = now()
WHERE org_id = @org_id AND consecutive_failures <> 0;

-- name: BumpWebhookFailures :one
UPDATE notification_webhooks
SET consecutive_failures = consecutive_failures + 1,
    enabled = enabled AND consecutive_failures + 1 < @threshold::int,
    disabled_reason = CASE WHEN enabled AND consecutive_failures + 1 >= @threshold::int THEN 'delivery_failures' ELSE disabled_reason END,
    updated_at = now()
WHERE org_id = @org_id
RETURNING enabled, consecutive_failures;

-- name: DropPendingDeliveries :execrows
-- Webhook bị tự tắt: các thông báo còn chờ không còn nơi nhận.
UPDATE notification_deliveries SET status = 'dropped', finished_at = now(), last_error_code = 'not_configured'
WHERE org_id = @org_id AND status = 'pending';

-- name: LockWebhook :exec
-- Khoá dòng webhook TRƯỚC dòng thông báo: mọi transaction đóng một lần gửi lấy khoá theo
-- cùng thứ tự (webhook → thông báo), nên hai lần đóng đồng thời của cùng org không deadlock.
SELECT 1 FROM notification_webhooks WHERE org_id = @org_id FOR UPDATE;
