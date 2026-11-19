-- +goose NO TRANSACTION
-- +goose Up
-- Một lần chạy workflow có nhiều nhất một execution, và tìm execution theo lần chạy là
-- truy vấn có index (sửa lần chạy bị bỏ dở giữa hai transaction của việc khởi chạy).
-- CONCURRENTLY để không chặn ghi vào agent_executions; DROP trước vì một lần chạy hỏng
-- giữa chừng để lại index INVALID mà IF NOT EXISTS sẽ bỏ qua.
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS agent_executions_wfe_uq;
CREATE UNIQUE INDEX CONCURRENTLY agent_executions_wfe_uq
  ON agent_executions (org_id, workflow_execution_id) WHERE workflow_execution_id IS NOT NULL;
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS agent_executions_wfe_uq;
RESET lock_timeout;
