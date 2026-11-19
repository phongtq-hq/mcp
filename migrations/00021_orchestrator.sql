-- +goose Up
-- Orchestrator (agent-runtime §5): execution chế độ planned có nhiều task chạy theo
-- một kế hoạch dạng DAG. Các cột plan_key, depends_on, parent_task_id, depth,
-- output_summary và trạng thái skipped của agent_tasks đã có từ 00008.
SET LOCAL lock_timeout = '5s';

-- Kế hoạch đã được kiểm, chụp lúc tạo execution. NULL với execution không phải planned.
ALTER TABLE agent_executions ADD COLUMN plan JSONB;

-- Cách một task thất bại ảnh hưởng tới phần còn lại của kế hoạch. Thêm cột có DEFAULT
-- hằng số không viết lại bảng.
ALTER TABLE agent_tasks ADD COLUMN on_failure TEXT NOT NULL DEFAULT 'skip_dependents';
ALTER TABLE agent_tasks ADD CONSTRAINT agent_tasks_on_failure_check
  CHECK (on_failure IN ('fail_execution','continue','skip_dependents')) NOT VALID; -- VALIDATE ở 00022

-- Kế hoạch và chế độ đi cùng nhau: execution planned luôn có kế hoạch, execution khác thì không.
ALTER TABLE agent_executions ADD CONSTRAINT agent_executions_plan_check
  CHECK ((mode = 'planned') = (plan IS NOT NULL) AND (plan IS NULL OR jsonb_typeof(plan) = 'object')) NOT VALID; -- VALIDATE ở 00022

-- depends_on là mảng nên không có khoá ngoại: ràng buộc dưới đây giữ nó trong cùng
-- execution. Trigger là deferred vì các task của một kế hoạch được chèn trong một
-- transaction (task phụ thuộc có thể được chèn trước task nó phụ thuộc).
ALTER TABLE agent_tasks ADD CONSTRAINT agent_tasks_depends_on_check
  CHECK (array_position(depends_on, NULL) IS NULL AND cardinality(depends_on) <= 16 AND NOT (id = ANY (depends_on))) NOT VALID;
-- output_summary đi vào context của task khác: trần ở database, không chỉ ở code.
ALTER TABLE agent_tasks ADD CONSTRAINT agent_tasks_output_summary_check
  CHECK (output_summary IS NULL OR char_length(output_summary) <= 4000) NOT VALID;
-- +goose StatementBegin
CREATE FUNCTION agent_tasks_depends_on_guard() RETURNS trigger
LANGUAGE plpgsql AS $fn$
BEGIN
  IF (SELECT count(*) FROM agent_tasks d
      WHERE d.org_id = NEW.org_id AND d.execution_id = NEW.execution_id AND d.id = ANY (NEW.depends_on))
     <> cardinality(NEW.depends_on) THEN
    RAISE EXCEPTION 'agent_tasks: depends_on của % trỏ ra ngoài execution của nó', NEW.id USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NULL;
END
$fn$;
-- +goose StatementEnd
CREATE CONSTRAINT TRIGGER agent_tasks_depends_on AFTER INSERT ON agent_tasks
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (cardinality(NEW.depends_on) > 0)
  EXECUTE FUNCTION agent_tasks_depends_on_guard();

-- Cấu trúc của kế hoạch là thứ quyết định task nào được chạy và thấy kết quả của task
-- nào: nó được chụp lúc tạo và không đổi được sau đó, với mọi role.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION agent_tasks_snapshot_guard() RETURNS trigger
LANGUAGE plpgsql AS $fn$
BEGIN
  IF NEW.model_chain IS DISTINCT FROM OLD.model_chain OR NEW.system_prompt IS DISTINCT FROM OLD.system_prompt
     OR NEW.goal IS DISTINCT FROM OLD.goal OR NEW.agent_version_id IS DISTINCT FROM OLD.agent_version_id
     OR NEW.execution_id IS DISTINCT FROM OLD.execution_id OR NEW.budget_limit IS DISTINCT FROM OLD.budget_limit
     OR NEW.plan_key IS DISTINCT FROM OLD.plan_key OR NEW.depends_on IS DISTINCT FROM OLD.depends_on
     OR NEW.on_failure IS DISTINCT FROM OLD.on_failure OR NEW.parent_task_id IS DISTINCT FROM OLD.parent_task_id
     OR NEW.depth IS DISTINCT FROM OLD.depth OR NEW.agent_id IS DISTINCT FROM OLD.agent_id THEN
    RAISE EXCEPTION 'agent_tasks: cấu hình đã chụp của task % không đổi được', OLD.id USING ERRCODE = 'restrict_violation';
  END IF;
  -- Tóm tắt kết quả chỉ được ghi đúng lúc task kết thúc: không ai sửa được phần mà các
  -- task phụ thuộc sẽ thấy khi task còn chạy hoặc chưa chạy.
  IF NEW.output_summary IS DISTINCT FROM OLD.output_summary
     AND NEW.status NOT IN ('completed','failed','cancelled','budget_exceeded','timed_out') THEN
    RAISE EXCEPTION 'agent_tasks: output_summary của % chỉ được ghi khi task kết thúc', OLD.id USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END
$fn$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION agent_executions_snapshot_guard() RETURNS trigger
LANGUAGE plpgsql AS $fn$
BEGIN
  IF NEW.budget_limit IS DISTINCT FROM OLD.budget_limit OR NEW.input IS DISTINCT FROM OLD.input
     OR NEW.project_id IS DISTINCT FROM OLD.project_id OR NEW.initiator_type IS DISTINCT FROM OLD.initiator_type
     OR NEW.initiator_id IS DISTINCT FROM OLD.initiator_id OR NEW.deadline_at IS DISTINCT FROM OLD.deadline_at
     OR NEW.plan IS DISTINCT FROM OLD.plan OR NEW.mode IS DISTINCT FROM OLD.mode THEN
    RAISE EXCEPTION 'agent_executions: cấu hình đã chụp của execution % không đổi được', OLD.id USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END
$fn$;
-- +goose StatementEnd

-- (Task đã kết thúc không sửa được nữa: trigger agent_tasks_terminal có từ 00008, nên
-- task đã completed không bị ghi đè khi execution của nó dừng, và task skipped không chạy.)

-- Worker ghi tóm tắt kết quả của task khi đóng nó (đầu vào cho các task phụ thuộc).
GRANT UPDATE (output_summary) ON agent_tasks TO app_worker;

-- +goose Down
SET LOCAL lock_timeout = '5s';
REVOKE UPDATE (output_summary) ON agent_tasks FROM app_worker;
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION agent_executions_snapshot_guard() RETURNS trigger
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
DROP TRIGGER agent_tasks_depends_on ON agent_tasks;
DROP FUNCTION agent_tasks_depends_on_guard();
ALTER TABLE agent_tasks DROP CONSTRAINT agent_tasks_output_summary_check;
ALTER TABLE agent_tasks DROP CONSTRAINT agent_tasks_depends_on_check;
ALTER TABLE agent_executions DROP CONSTRAINT agent_executions_plan_check;
ALTER TABLE agent_tasks DROP COLUMN on_failure;
ALTER TABLE agent_executions DROP COLUMN plan;
