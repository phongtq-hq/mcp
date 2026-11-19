-- +goose Up
-- Siết lại sau khi review lát Budget (00017).
SET LOCAL lock_timeout = '5s';

-- Worker không thực thi budget theo phạm vi: việc đó nằm ở Gateway (nơi gọi model có
-- khoá) và ở process api khi vòng lặp chạy trong process. RLS không phải ranh giới với
-- một worker bị chiếm quyền (nó tự đặt app.org_id), nên nó không được đọc cấu hình
-- budget hay phân bố chi tiêu của các org. Hai cột estimated_cost, currency cấp ở 00013
-- (RETURNING của lệnh ghi usage) được giữ.
REVOKE SELECT ON budgets FROM app_worker;
REVOKE SELECT (org_id, project_id, user_id, agent_id, occurred_at) ON usage_records FROM app_worker;

-- budget_limit là căn cứ Gateway chặn chi tiêu của một task: nó được chụp lúc tạo và
-- không đổi được sau đó, với mọi role (tới giờ chỉ được giữ bằng danh sách cột GRANT).
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION agent_tasks_snapshot_guard() RETURNS trigger
LANGUAGE plpgsql AS $fn$
BEGIN
  IF NEW.model_chain IS DISTINCT FROM OLD.model_chain OR NEW.system_prompt IS DISTINCT FROM OLD.system_prompt
     OR NEW.goal IS DISTINCT FROM OLD.goal OR NEW.agent_version_id IS DISTINCT FROM OLD.agent_version_id
     OR NEW.execution_id IS DISTINCT FROM OLD.execution_id OR NEW.budget_limit IS DISTINCT FROM OLD.budget_limit THEN
    RAISE EXCEPTION 'agent_tasks: cấu hình đã chụp của task % không đổi được', OLD.id USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END
$fn$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION agent_executions_snapshot_guard() RETURNS trigger
LANGUAGE plpgsql AS $fn$
BEGIN
  IF NEW.budget_limit IS DISTINCT FROM OLD.budget_limit OR NEW.input IS DISTINCT FROM OLD.input
     OR NEW.project_id IS DISTINCT FROM OLD.project_id OR NEW.initiator_type IS DISTINCT FROM OLD.initiator_type
     OR NEW.initiator_id IS DISTINCT FROM OLD.initiator_id OR NEW.deadline_at IS DISTINCT FROM OLD.deadline_at THEN
    RAISE EXCEPTION 'agent_executions: cấu hình đã chụp của execution % không đổi được', OLD.id USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END
$fn$;
-- +goose StatementEnd
CREATE TRIGGER agent_executions_snapshot BEFORE UPDATE ON agent_executions
  FOR EACH ROW EXECUTE FUNCTION agent_executions_snapshot_guard();

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TRIGGER agent_executions_snapshot ON agent_executions;
DROP FUNCTION agent_executions_snapshot_guard();
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION agent_tasks_snapshot_guard() RETURNS trigger
LANGUAGE plpgsql AS $fn$
BEGIN
  IF NEW.model_chain IS DISTINCT FROM OLD.model_chain OR NEW.system_prompt IS DISTINCT FROM OLD.system_prompt
     OR NEW.goal IS DISTINCT FROM OLD.goal OR NEW.agent_version_id IS DISTINCT FROM OLD.agent_version_id
     OR NEW.execution_id IS DISTINCT FROM OLD.execution_id THEN
    RAISE EXCEPTION 'agent_tasks: cấu hình đã chụp của task % không đổi được', OLD.id USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END
$fn$;
-- +goose StatementEnd
GRANT SELECT (org_id, project_id, user_id, agent_id, occurred_at) ON usage_records TO app_worker;
GRANT SELECT ON budgets TO app_worker;
