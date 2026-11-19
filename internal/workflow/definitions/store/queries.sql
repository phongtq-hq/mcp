-- name: InsertWorkflow :one
INSERT INTO workflows (id, org_id, project_id, slug, name, description, definition, created_by, definition_by_type, definition_by)
VALUES (@id, @org_id, @project_id, @slug, @name, sqlc.narg('description'), @definition, sqlc.narg('created_by'), @definition_by_type, @definition_by)
RETURNING *;

-- name: GetWorkflow :one
SELECT * FROM workflows WHERE org_id = @org_id AND id = @id;

-- name: LockWorkflow :one
SELECT * FROM workflows WHERE org_id = @org_id AND id = @id FOR UPDATE;

-- name: ListWorkflows :many
SELECT * FROM workflows
WHERE org_id = @org_id AND project_id = @project_id
  AND ((sqlc.narg('status')::text IS NULL AND status <> 'archived') OR status = sqlc.narg('status'))
  AND (sqlc.narg('after_created_at')::timestamptz IS NULL
       OR (created_at, id) < (sqlc.narg('after_created_at')::timestamptz, sqlc.narg('after_id')::uuid))
ORDER BY created_at DESC, id DESC LIMIT @row_limit;

-- name: UpdateWorkflow :one
UPDATE workflows SET name = @name, description = sqlc.narg('description'), definition = @definition, version = @version,
  definition_by_type = sqlc.narg('definition_by_type'), definition_by = sqlc.narg('definition_by'),
  schedule = sqlc.narg('schedule'), status = @status, run_as = sqlc.narg('run_as'), schedule_failures = 0, updated_at = now()
WHERE org_id = @org_id AND id = @id
RETURNING *;

-- name: InsertWorkflowExecution :one
-- Lần kích hoạt theo lịch đã có lần chạy (cùng trigger_key) thì không chèn gì: không có dòng trả về.
INSERT INTO workflow_executions (id, org_id, project_id, workflow_id, workflow_version, definition_snapshot, definition_by_type, definition_by,
  trigger_type, triggered_by, input, trigger_key)
VALUES (@id, @org_id, @project_id, @workflow_id, @workflow_version, @definition_snapshot, sqlc.narg('definition_by_type'), sqlc.narg('definition_by'),
        @trigger_type, sqlc.narg('triggered_by'), sqlc.narg('input'), sqlc.narg('trigger_key'))
ON CONFLICT (org_id, workflow_id, trigger_key) DO NOTHING
RETURNING *;

-- name: GetWorkflowExecutionByTrigger :one
SELECT * FROM workflow_executions WHERE org_id = @org_id AND workflow_id = @workflow_id AND trigger_key = @trigger_key;

-- name: GetWorkflowExecution :one
SELECT * FROM workflow_executions WHERE org_id = @org_id AND id = @id;

-- name: ListWorkflowExecutions :many
-- Danh sách không kéo bản chụp định nghĩa (tới 256 KiB mỗi dòng).
SELECT id, project_id, workflow_id, workflow_version, trigger_type, status, input, output, error, execution_id,
       started_at, finished_at, created_at, updated_at
FROM workflow_executions
WHERE org_id = @org_id AND workflow_id = @workflow_id
  AND (sqlc.narg('status')::text IS NULL OR status = sqlc.narg('status'))
  AND (sqlc.narg('after_created_at')::timestamptz IS NULL
       OR (created_at, id) < (sqlc.narg('after_created_at')::timestamptz, sqlc.narg('after_id')::uuid))
ORDER BY created_at DESC, id DESC LIMIT @row_limit;

-- name: LinkWorkflowExecution :execrows
-- Lần chạy đã có execution của nó.
UPDATE workflow_executions SET execution_id = @execution_id, updated_at = now()
WHERE org_id = @org_id AND id = @id AND execution_id IS NULL
  AND status NOT IN ('completed','failed','cancelled','compensated','budget_exceeded','timed_out');

-- name: SyncWorkflowExecution :execrows
-- Bản chiếu lấy trạng thái từ execution của nó. Chỉ lần chạy chưa kết thúc mới đổi.
UPDATE workflow_executions SET status = @status, output = sqlc.narg('output'), error = sqlc.narg('error'),
  started_at = sqlc.narg('started_at'), finished_at = sqlc.narg('finished_at'), updated_at = now()
WHERE org_id = @org_id AND id = @id
  AND status NOT IN ('completed','failed','cancelled','compensated','budget_exceeded','timed_out');

-- name: LockWorkflowLimits :exec
-- Tuần tự hoá việc đếm workflow của một org (trần số workflow và số lịch).
SELECT pg_advisory_xact_lock(hashtextextended('workflow_limits:' || CAST(@org_id AS uuid)::text, 0));

-- name: CountProjectWorkflows :one
SELECT count(*) FROM workflows WHERE org_id = @org_id AND project_id = @project_id AND status <> 'archived';

-- name: CountScheduledWorkflows :one
-- Số workflow đang có lịch chạy của org, không kể workflow đang xét.
SELECT count(*) FROM workflows
WHERE org_id = @org_id AND status = 'active' AND schedule IS NOT NULL AND id <> @except_id;

-- name: LatestScheduledRun :one
-- Lần chạy theo lịch gần nhất của một workflow.
-- recent: nó được tạo trong vòng gap_seconds, theo đồng hồ của database.
SELECT id, status, execution_id, trigger_key,
       (created_at > now() - make_interval(secs => @gap_seconds::float8))::boolean AS recent
FROM workflow_executions
WHERE org_id = @org_id AND workflow_id = @workflow_id AND trigger_type = 'schedule'
ORDER BY created_at DESC, id DESC LIMIT 1;

-- name: BumpScheduleFailures :one
-- Một lần lịch kích hoạt bị từ chối. Không đổi updated_at: ETag của workflow là của người sửa.
UPDATE workflows SET schedule_failures = schedule_failures + 1
WHERE org_id = @org_id AND id = @id AND status = 'active'
RETURNING schedule_failures;

-- name: ResetScheduleFailures :exec
UPDATE workflows SET schedule_failures = 0
WHERE org_id = @org_id AND id = @id AND schedule_failures <> 0 AND status <> 'archived';

-- name: DisableWorkflow :one
-- Platform tắt một workflow có lịch bị từ chối liên tiếp.
UPDATE workflows SET status = 'disabled', updated_at = now()
WHERE org_id = @org_id AND id = @id AND status = 'active'
RETURNING *;
