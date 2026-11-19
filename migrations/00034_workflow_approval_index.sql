-- +goose NO TRANSACTION
-- +goose Up
-- Mỗi nút approval của một execution có nhiều nhất một approval (đích của việc tra theo
-- execution + plan_key). CONCURRENTLY để không chặn ghi vào approvals; DROP trước vì một
-- lần chạy hỏng giữa chừng để lại index INVALID.
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS approvals_plan_step_key;
CREATE UNIQUE INDEX CONCURRENTLY approvals_plan_step_key
  ON approvals (org_id, execution_id, plan_key) WHERE plan_key IS NOT NULL;
ALTER TABLE approvals VALIDATE CONSTRAINT approvals_workflow_step_check;
ALTER TABLE approvals VALIDATE CONSTRAINT approvals_plan_key_check;
ALTER TABLE approvals VALIDATE CONSTRAINT approvals_plan_step_fkey;
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
DROP INDEX CONCURRENTLY IF EXISTS approvals_plan_step_key;
RESET lock_timeout;
