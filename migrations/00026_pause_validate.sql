-- +goose Up
-- Tách khỏi 00025: VALIDATE chỉ giữ SHARE UPDATE EXCLUSIVE.
SET LOCAL lock_timeout = '5s';
ALTER TABLE agent_executions VALIDATE CONSTRAINT agent_executions_pause_check;

-- +goose Down
SELECT 1;
