-- +goose Up
-- Tách khỏi 00030: VALIDATE chỉ giữ SHARE UPDATE EXCLUSIVE.
SET LOCAL lock_timeout = '5s';
ALTER TABLE agent_executions VALIDATE CONSTRAINT agent_executions_wfe_fk;

-- +goose Down
SELECT 1;
