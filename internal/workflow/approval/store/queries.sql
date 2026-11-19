-- name: GetApproval :one
SELECT id, org_id, project_id, execution_id, task_id, step_seq, subject_type, tool_id, tool_name, args_hash, payload, risk_level,
       required_approvals, decisions, status, requested_by_type, requested_by, requested_by_owner, expires_at, resolved_at, created_at,
       (expires_at <= now())::bool AS expired
FROM approvals WHERE org_id = @org_id AND id = @id;

-- name: LockApprovalForStep :one
-- Approval của một bước, khoá dòng: việc tạo, quyết định và tiêu thụ được tuần tự hoá.
-- Hết hạn được so bằng đồng hồ của database, chung cho mọi process.
SELECT id, tool_id, args_hash, risk_level, status, (expires_at <= now())::bool AS expired FROM approvals
WHERE org_id = @org_id AND task_id = @task_id AND step_seq = @step_seq FOR UPDATE;

-- name: LockApproval :one
SELECT id, org_id, project_id, execution_id, task_id, step_seq, subject_type, tool_id, tool_name, args_hash, payload, risk_level,
       required_approvals, decisions, status, requested_by_type, requested_by, requested_by_owner, excluded_approver, expires_at, resolved_at, created_at,
       (expires_at <= now())::bool AS expired
FROM approvals WHERE org_id = @org_id AND id = @id FOR UPDATE;

-- name: InsertApproval :one
INSERT INTO approvals (id, org_id, project_id, execution_id, task_id, step_seq, subject_type, tool_id, tool_name, args_hash, payload,
  risk_level, requested_by_type, requested_by, requested_by_owner, expires_at)
VALUES (@id, @org_id, @project_id, @execution_id, @task_id, @step_seq, 'tool_call', @tool_id, @tool_name, @args_hash, @payload,
  @risk_level, @requested_by_type, sqlc.narg('requested_by'), sqlc.narg('requested_by_owner'), @expires_at)
ON CONFLICT (org_id, task_id, step_seq) WHERE task_id IS NOT NULL DO NOTHING
RETURNING id;

-- name: SetApprovalStatus :execrows
UPDATE approvals SET status = @status, resolved_at = now()
WHERE org_id = @org_id AND id = @id AND status = @from_status;

-- name: DecideApproval :execrows
UPDATE approvals SET status = @status, decisions = decisions || @decision::jsonb, resolved_at = now()
WHERE org_id = @org_id AND id = @id AND status = 'pending' AND expires_at > now();

-- name: AppendApprovalDecision :execrows
-- Một quyết định approve khi chưa đủ số người duyệt: approval vẫn pending.
UPDATE approvals SET decisions = decisions || @decision::jsonb
WHERE org_id = @org_id AND id = @id AND status = 'pending' AND expires_at > now();

-- name: CountPendingApprovals :one
-- Số approval còn chờ của một execution (trần, để người duyệt không bị dội yêu cầu).
-- Chỉ approval của tool call: cổng approval của workflow không chiếm trần này.
SELECT count(*) FROM approvals WHERE org_id = @org_id AND execution_id = @execution_id AND task_id IS NOT NULL
  AND status = 'pending' AND expires_at > now();

-- name: CancelApprovalsOfExecution :execrows
UPDATE approvals SET status = 'cancelled', resolved_at = now()
WHERE org_id = @org_id AND execution_id = @execution_id AND status IN ('pending','approved');

-- name: ApprovalStatus :one
-- Cho runtime (role app_worker chỉ đọc được các cột này).
SELECT status, (expires_at <= now())::bool AS expired FROM approvals WHERE org_id = @org_id AND id = @id AND task_id = @task_id;

-- name: ListApprovals :many
SELECT id, org_id, project_id, execution_id, task_id, step_seq, subject_type, tool_id, tool_name, args_hash, payload, risk_level,
       required_approvals, decisions, status, requested_by_type, requested_by, requested_by_owner, expires_at, resolved_at, created_at,
       (expires_at <= now())::bool AS expired
FROM approvals
WHERE org_id = @org_id
  AND (sqlc.narg('project_id')::uuid IS NULL OR project_id = sqlc.narg('project_id'))
  AND (sqlc.narg('execution_id')::uuid IS NULL OR execution_id = sqlc.narg('execution_id'))
  -- Approval còn pending / approved nhưng đã quá hạn được coi là expired, như khi hiển thị.
  AND (sqlc.narg('status')::text IS NULL
    OR (sqlc.narg('status') = 'expired' AND (status = 'expired' OR (status IN ('pending','approved') AND expires_at <= now())))
    OR (sqlc.narg('status') IN ('pending','approved') AND status = sqlc.narg('status') AND expires_at > now())
    OR (sqlc.narg('status') NOT IN ('pending','approved','expired') AND status = sqlc.narg('status')))
  AND (sqlc.narg('not_requested_by')::uuid IS NULL OR requested_by IS DISTINCT FROM sqlc.narg('not_requested_by'))
  AND (sqlc.narg('after_created')::timestamptz IS NULL OR (created_at, id) < (sqlc.narg('after_created'), sqlc.narg('after_id')::uuid))
ORDER BY created_at DESC, id DESC LIMIT @row_limit;
