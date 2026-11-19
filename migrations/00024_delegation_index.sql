-- +goose NO TRANSACTION
-- +goose Up
-- Mỗi bước của task cha giao việc nhiều nhất một lần: khoá idempotency của delegation.
-- CONCURRENTLY để không chặn ghi; vì vậy migration này chạy ngoài transaction. DROP trước
-- khi tạo: một lần chạy hỏng giữa chừng để lại index INVALID, và IF NOT EXISTS sẽ bỏ qua
-- nó mà vẫn ghi migration là đã áp. VALIDATE chỉ giữ SHARE UPDATE EXCLUSIVE.
DROP INDEX CONCURRENTLY IF EXISTS agent_tasks_delegated_uq;
CREATE UNIQUE INDEX CONCURRENTLY agent_tasks_delegated_uq
  ON agent_tasks (org_id, parent_task_id, delegated_step_seq) WHERE parent_task_id IS NOT NULL;
ALTER TABLE agent_tasks VALIDATE CONSTRAINT agent_tasks_delegated_check;

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS agent_tasks_delegated_uq;
