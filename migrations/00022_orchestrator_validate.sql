-- +goose Up
-- Tách khỏi 00021: VALIDATE chỉ giữ SHARE UPDATE EXCLUSIVE, không chặn đọc / ghi task
-- trong lúc quét bảng.
SET LOCAL lock_timeout = '5s';
ALTER TABLE agent_tasks VALIDATE CONSTRAINT agent_tasks_on_failure_check;
ALTER TABLE agent_tasks VALIDATE CONSTRAINT agent_tasks_depends_on_check;
ALTER TABLE agent_tasks VALIDATE CONSTRAINT agent_tasks_output_summary_check;
ALTER TABLE agent_executions VALIDATE CONSTRAINT agent_executions_plan_check;

-- +goose Down
SELECT 1;
