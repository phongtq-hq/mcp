-- +goose Up
-- Nút không phải Agent của một kế hoạch (ADR-0015): nút `wait` và nút `approval` (cổng chờ người duyệt). Mỗi nút như vậy
-- có một dòng trạng thái ở đây, như task của Agent có dòng trong agent_tasks. Process
-- api chèn chúng cùng transaction tạo execution; process chạy kế hoạch chỉ đổi trạng thái.
SET LOCAL lock_timeout = '5s';
CREATE TABLE execution_plan_steps (
  org_id       UUID NOT NULL REFERENCES organizations(id),
  execution_id UUID NOT NULL,
  plan_key     TEXT NOT NULL CHECK (plan_key ~ '^[a-z0-9][a-z0-9_-]{0,31}$'),
  kind         TEXT NOT NULL CHECK (kind IN ('wait','approval')),
  status       TEXT NOT NULL DEFAULT 'queued' CHECK (status IN
    ('queued','running','completed','failed','cancelled','timed_out','skipped')),
  -- Cấu hình của nút, chụp lúc tạo. wait: {"seconds": n}. approval: {"message": text,
  -- "required_approvals": 1|2, "timeout_seconds": n}; approval của nút được tạo từ đây
  -- bởi hàm plan_step_request_approval (00033), không từ tham số của nơi chạy kế hoạch. Nguồn sự thật của thời gian
  -- chờ là agent_executions.plan (nơi chạy kế hoạch đọc từ đó); đây là bản để đối chiếu.
  config       JSONB NOT NULL CHECK (jsonb_typeof(config) = 'object'),
  started_at   TIMESTAMPTZ,
  finished_at  TIMESTAMPTZ,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  -- Khoá tự nhiên có chủ ý: một nút được định danh bằng khoá của nó trong kế hoạch, và
  -- không bảng nào tham chiếu tới nó. Không có updated_at: started_at / finished_at là
  -- hai lần đổi duy nhất của một dòng.
  PRIMARY KEY (org_id, execution_id, plan_key),
  FOREIGN KEY (org_id, execution_id) REFERENCES agent_executions (org_id, id) ON DELETE CASCADE,
  CHECK (kind <> 'wait' OR COALESCE(jsonb_typeof(config->'seconds') = 'number'
         AND (config->>'seconds')::numeric BETWEEN 1 AND 3600, false)),
  CHECK (kind <> 'approval' OR COALESCE(jsonb_typeof(config->'message') = 'string' AND length(config->>'message') BETWEEN 1 AND 8000
         AND jsonb_typeof(config->'required_approvals') = 'number' AND (config->>'required_approvals') IN ('1','2')
         AND jsonb_typeof(config->'timeout_seconds') = 'number' AND (config->>'timeout_seconds')::numeric BETWEEN 60 AND 86400, false))
);
ALTER TABLE execution_plan_steps ENABLE ROW LEVEL SECURITY;
ALTER TABLE execution_plan_steps FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON execution_plan_steps
  USING      (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid)
  WITH CHECK (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid);
-- Trạng thái chỉ đi tới; nút đã kết thúc không đổi nữa; loại và cấu hình bất biến.
-- +goose StatementBegin
CREATE FUNCTION execution_plan_steps_guard() RETURNS trigger
LANGUAGE plpgsql AS $fn$
BEGIN
  IF NEW.kind IS DISTINCT FROM OLD.kind OR NEW.config IS DISTINCT FROM OLD.config
     OR NEW.plan_key IS DISTINCT FROM OLD.plan_key OR NEW.execution_id IS DISTINCT FROM OLD.execution_id
     OR NEW.org_id IS DISTINCT FROM OLD.org_id OR NEW.created_at IS DISTINCT FROM OLD.created_at
     OR (OLD.started_at IS NOT NULL AND NEW.started_at IS DISTINCT FROM OLD.started_at) THEN
    RAISE EXCEPTION 'execution_plan_steps: cấu hình của nút % không đổi được', OLD.plan_key USING ERRCODE = 'restrict_violation';
  END IF;
  IF OLD.status IN ('completed','failed','cancelled','timed_out','skipped') OR NEW.status = 'queued' THEN
    RAISE EXCEPTION 'execution_plan_steps: nút % đã kết thúc hoặc không quay lại queued', OLD.plan_key USING ERRCODE = 'restrict_violation';
  END IF;
  -- "completed" là điều mở các bước phụ thuộc, nên database tự giữ điều kiện của nó, với
  -- mọi role: nút chờ phải đã chạy và đã chờ đủ (trừ 2 giây cho độ lệch đồng hồ của nơi
  -- chạy kế hoạch). Nút approval chỉ hoàn thành được qua approval của nó (00033).
  IF NEW.status = 'completed' THEN
    IF OLD.kind = 'wait' AND (OLD.status <> 'running' OR OLD.started_at IS NULL
         OR OLD.started_at > now() - make_interval(secs => GREATEST((OLD.config->>'seconds')::float8 - 2, 0))) THEN
      RAISE EXCEPTION 'execution_plan_steps: nút chờ % chưa chờ đủ', OLD.plan_key USING ERRCODE = 'restrict_violation';
    END IF;
    IF OLD.kind = 'approval' THEN
      RAISE EXCEPTION 'execution_plan_steps: nút approval % chỉ hoàn thành qua approval của nó', OLD.plan_key USING ERRCODE = 'restrict_violation';
    END IF;
  END IF;
  RETURN NEW;
END
$fn$;
-- +goose StatementEnd
CREATE TRIGGER execution_plan_steps_guard BEFORE UPDATE ON execution_plan_steps
  FOR EACH ROW EXECUTE FUNCTION execution_plan_steps_guard();
-- Nút được tạo ở trạng thái queued: không chèn được một nút đã chạy xong.
-- +goose StatementBegin
CREATE FUNCTION execution_plan_steps_insert_guard() RETURNS trigger
LANGUAGE plpgsql AS $fn$
BEGIN
  IF NEW.status <> 'queued' OR NEW.started_at IS NOT NULL OR NEW.finished_at IS NOT NULL THEN
    RAISE EXCEPTION 'execution_plan_steps: nút mới phải ở trạng thái queued' USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END
$fn$;
-- +goose StatementEnd
CREATE TRIGGER execution_plan_steps_insert_guard BEFORE INSERT ON execution_plan_steps
  FOR EACH ROW EXECUTE FUNCTION execution_plan_steps_insert_guard();
GRANT SELECT, INSERT ON execution_plan_steps TO app_api;
GRANT UPDATE (status, started_at, finished_at) ON execution_plan_steps TO app_api, app_worker;
GRANT SELECT ON execution_plan_steps TO app_worker, app_admin;

-- Lần chạy workflow đã tạo ra execution này (ADR-0015); NULL với execution khác. Khoá
-- ngoại tới workflow_executions được thêm cùng bảng đó (00030).
ALTER TABLE agent_executions ADD COLUMN workflow_execution_id UUID;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE agent_executions DROP COLUMN workflow_execution_id;
DROP TABLE execution_plan_steps;
DROP FUNCTION execution_plan_steps_guard();
DROP FUNCTION execution_plan_steps_insert_guard();
