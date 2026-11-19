-- +goose Up
-- Tách khỏi 00027: VALIDATE chỉ giữ SHARE UPDATE EXCLUSIVE.
SET LOCAL lock_timeout = '5s';
ALTER TABLE agent_executions VALIDATE CONSTRAINT agent_executions_plan_source_check;

-- +goose Down
SELECT 1;
