-- +goose Up
-- Đề xuất kế hoạch bằng LLM (agent-runtime §5.11). Một execution lập kế hoạch được ĐÁNH
-- DẤU lúc tạo: chỉ execution có dấu mới cho ra "đề xuất", và danh sách Agent mà kế hoạch
-- được dùng được giữ ở đây chứ không suy lại lúc đọc.
SET LOCAL lock_timeout = '5s';
CREATE TABLE plan_proposals (
  org_id            UUID NOT NULL REFERENCES organizations(id),
  execution_id      UUID NOT NULL,
  project_id        UUID NOT NULL,
  planner_agent_id  UUID NOT NULL,
  -- Các Agent mà kế hoạch được phép dùng (đang hoạt động lúc đề xuất, đã thu hẹp theo yêu cầu).
  agent_ids         UUID[] NOT NULL CHECK (cardinality(agent_ids) BETWEEN 1 AND 32 AND array_position(agent_ids, NULL) IS NULL),
  requested_by_type TEXT NOT NULL CHECK (requested_by_type IN ('user','api_key')),
  requested_by      UUID NOT NULL,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (org_id, execution_id),
  FOREIGN KEY (org_id, execution_id) REFERENCES agent_executions (org_id, id) ON DELETE CASCADE,
  FOREIGN KEY (org_id, project_id) REFERENCES projects (org_id, id),
  FOREIGN KEY (org_id, planner_agent_id) REFERENCES agents (org_id, id)
);
ALTER TABLE plan_proposals ENABLE ROW LEVEL SECURITY;
ALTER TABLE plan_proposals FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON plan_proposals
  USING      (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid)
  WITH CHECK (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid);
-- Chỉ thêm: không role ứng dụng nào sửa hay xoá được dấu. Process chạy vòng lặp và
-- Gateway không đọc bảng này.
GRANT SELECT, INSERT ON plan_proposals TO app_api;
GRANT SELECT ON plan_proposals TO app_admin;

-- Nguồn gốc của kế hoạch một execution planned: 'caller' (người gọi viết) hoặc 'model'
-- (do Agent lập kế hoạch đề xuất, api chạy thẳng từ đề xuất). Với 'model', goal của các
-- task là output của model: chúng được đánh dấu như vậy, và tool ghi của chúng luôn cần
-- người duyệt (mức sàn ở Gateway). NULL với execution không planned.
ALTER TABLE agent_executions ADD COLUMN plan_source TEXT;
ALTER TABLE agent_executions ADD CONSTRAINT agent_executions_plan_source_check
  CHECK ((plan IS NULL) = (plan_source IS NULL) AND (plan_source IS NULL OR plan_source IN ('caller','model'))) NOT VALID; -- VALIDATE ở 00028
-- Execution planned tạo trước migration này: kế hoạch do người gọi viết.
UPDATE agent_executions SET plan_source = 'caller' WHERE plan IS NOT NULL;
GRANT SELECT (plan_source) ON agent_executions TO app_gateway;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION agent_executions_snapshot_guard() RETURNS trigger
LANGUAGE plpgsql AS $fn$
BEGIN
  IF NEW.budget_limit IS DISTINCT FROM OLD.budget_limit OR NEW.input IS DISTINCT FROM OLD.input
     OR NEW.project_id IS DISTINCT FROM OLD.project_id OR NEW.initiator_type IS DISTINCT FROM OLD.initiator_type
     OR NEW.initiator_id IS DISTINCT FROM OLD.initiator_id OR NEW.deadline_at IS DISTINCT FROM OLD.deadline_at
     OR NEW.plan IS DISTINCT FROM OLD.plan OR NEW.mode IS DISTINCT FROM OLD.mode
     OR NEW.plan_source IS DISTINCT FROM OLD.plan_source THEN
    RAISE EXCEPTION 'agent_executions: cấu hình đã chụp của execution % không đổi được', OLD.id USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END
$fn$;
-- +goose StatementEnd

-- +goose Down
SET LOCAL lock_timeout = '5s';
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
REVOKE SELECT (plan_source) ON agent_executions FROM app_gateway;
ALTER TABLE agent_executions DROP CONSTRAINT agent_executions_plan_source_check;
ALTER TABLE agent_executions DROP COLUMN plan_source;
DROP TABLE plan_proposals;
