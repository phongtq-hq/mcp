-- +goose Up
-- Phase 2: vòng lặp Agent chạy trong process worker (Temporal activity). Role app_worker
-- nhận đúng những quyền các bước của vòng lặp cần: đọc / ghi step, ghi usage, cập nhật
-- trạng thái execution và task, đọc bảng giá.
--
-- Cố ý KHÔNG cấp: tạo execution hay task (việc của api), ghi cancel_requested_at (chỉ
-- api nhận yêu cầu huỷ), bảng của Agent Catalog, Secrets, Registry hay API key. Worker
-- không giữ credential (ADR-0008); tool call và lượt model đi qua Gateway.
--
-- RLS không phải ranh giới với một worker bị chiếm quyền (nó tự đặt app.org_id), nên
-- migration này cũng thêm các chặn ở mức dòng không phụ thuộc role: step chỉ được mở
-- một lần cho mỗi (task, step_seq), trạng thái và số cộng dồn chỉ tiến.
SET LOCAL lock_timeout = '5s';

-- Task chụp system prompt của Agent Version lúc tạo: các bước đọc nó từ chính task,
-- không join sang bảng của Agent Catalog (ADR-0001), và worker không cần quyền nào
-- trên agent_versions. NULL với task tạo trước migration này (đều đã kết thúc hoặc
-- chạy trong process api cũ).
ALTER TABLE agent_tasks ADD COLUMN system_prompt TEXT;

GRANT SELECT ON agent_executions, agent_tasks, execution_steps TO app_worker;
GRANT UPDATE (status, output, error, estimated_cost, started_at, finished_at, updated_at) ON agent_executions TO app_worker;
GRANT UPDATE (status, output, error, estimated_cost, iterations, tool_calls, started_at, finished_at, updated_at) ON agent_tasks TO app_worker;
GRANT INSERT ON execution_steps TO app_worker;
GRANT UPDATE (status, response, error, duration_ms, routing) ON execution_steps TO app_worker;
-- usage_records chỉ thêm; SELECT theo cột chỉ đủ cho RETURNING của lệnh ghi.
GRANT INSERT ON usage_records TO app_worker;
GRANT SELECT (estimated_cost, currency) ON usage_records TO app_worker;

GRANT SELECT (id, org_id, provider_id) ON models TO app_worker;
GRANT SELECT ON model_pricing TO app_worker;

-- Kết thúc execution được ghi audit trong cùng transaction.
GRANT EXECUTE ON FUNCTION audit_append(uuid, uuid, text, uuid, uuid, text, text, uuid, text, uuid, jsonb, inet, text) TO app_worker;

-- Step mới chỉ được mở (started) hoặc ghi một quyết định từ chối (denied), và mỗi
-- (task, step_seq) chỉ có một dòng. Bảng phân vùng theo thời gian nên không có ràng
-- buộc duy nhất cho điều này; code ghi step dưới khoá dòng của task, trigger này là
-- lớp chặn ở database cho mọi role.
-- +goose StatementBegin
CREATE FUNCTION execution_steps_insert_guard() RETURNS trigger
LANGUAGE plpgsql AS $fn$
BEGIN
  IF NEW.status NOT IN ('started','denied') THEN
    RAISE EXCEPTION 'execution_steps: step mới phải ở trạng thái started hoặc denied' USING ERRCODE = 'restrict_violation';
  END IF;
  IF EXISTS (SELECT 1 FROM execution_steps s WHERE s.org_id = NEW.org_id AND s.task_id = NEW.task_id AND s.step_seq = NEW.step_seq) THEN
    RAISE EXCEPTION 'execution_steps: step % của task % đã tồn tại', NEW.step_seq, NEW.task_id USING ERRCODE = 'unique_violation';
  END IF;
  RETURN NEW;
END
$fn$;
-- +goose StatementEnd
CREATE TRIGGER execution_steps_insert_once BEFORE INSERT ON execution_steps
  FOR EACH ROW EXECUTE FUNCTION execution_steps_insert_guard();

-- Execution / task không quay lại queued, và số cộng dồn không giảm.
-- +goose StatementBegin
CREATE FUNCTION execution_forward_guard() RETURNS trigger
LANGUAGE plpgsql AS $fn$
BEGIN
  IF NEW.status = 'queued' AND OLD.status <> 'queued' THEN
    RAISE EXCEPTION '%: % không quay lại queued', TG_TABLE_NAME, OLD.id USING ERRCODE = 'restrict_violation';
  END IF;
  IF NEW.estimated_cost < OLD.estimated_cost THEN
    RAISE EXCEPTION '%: estimated_cost của % không được giảm', TG_TABLE_NAME, OLD.id USING ERRCODE = 'restrict_violation';
  END IF;
  IF TG_TABLE_NAME = 'agent_tasks' AND
     ((to_jsonb(NEW)->>'iterations')::int < (to_jsonb(OLD)->>'iterations')::int OR
      (to_jsonb(NEW)->>'tool_calls')::int < (to_jsonb(OLD)->>'tool_calls')::int) THEN
    RAISE EXCEPTION 'agent_tasks: iterations / tool_calls của % không được giảm', OLD.id USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END
$fn$;
-- +goose StatementEnd
CREATE TRIGGER agent_executions_forward BEFORE UPDATE ON agent_executions
  FOR EACH ROW EXECUTE FUNCTION execution_forward_guard();
CREATE TRIGGER agent_tasks_forward BEFORE UPDATE ON agent_tasks
  FOR EACH ROW EXECUTE FUNCTION execution_forward_guard();

-- +goose Down
DROP TRIGGER agent_tasks_forward ON agent_tasks;
DROP TRIGGER agent_executions_forward ON agent_executions;
DROP FUNCTION execution_forward_guard();
DROP TRIGGER execution_steps_insert_once ON execution_steps;
DROP FUNCTION execution_steps_insert_guard();
REVOKE EXECUTE ON FUNCTION audit_append(uuid, uuid, text, uuid, uuid, text, text, uuid, text, uuid, jsonb, inet, text) FROM app_worker;
REVOKE ALL ON model_pricing, models, usage_records, execution_steps, agent_tasks, agent_executions FROM app_worker;
ALTER TABLE agent_tasks DROP COLUMN system_prompt;
