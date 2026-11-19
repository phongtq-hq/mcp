-- +goose Up
-- Workflow do người dùng định nghĩa (ADR-0015): định nghĩa có phiên bản, và bản chiếu
-- của từng lần chạy. Một lần chạy là một execution `planned`; workflow_executions trỏ tới nó.
SET LOCAL lock_timeout = '5s';
CREATE TABLE workflows (
  id          UUID PRIMARY KEY DEFAULT uuidv7(),
  org_id      UUID NOT NULL REFERENCES organizations(id),
  project_id  UUID NOT NULL,
  slug        TEXT NOT NULL CHECK (slug ~ '^[a-z0-9][a-z0-9-]{1,62}$'),
  name        TEXT NOT NULL,
  description TEXT,
  version     INT  NOT NULL DEFAULT 1 CHECK (version >= 1),
  definition  JSONB NOT NULL CHECK (jsonb_typeof(definition) = 'object'),
  -- Biểu thức cron (UTC); NULL = không chạy theo lịch. Đồng bộ sang Temporal Schedule.
  schedule    TEXT,
  status      TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft','active','disabled','archived')),
  -- Người đã kích hoạt workflow: lần chạy theo lịch mang quyền của người này.
  run_as      UUID REFERENCES users(id),
  -- Số lần lịch kích hoạt liên tiếp bị từ chối; chạm ngưỡng thì workflow tự về disabled.
  schedule_failures INT NOT NULL DEFAULT 0 CHECK (schedule_failures >= 0),
  created_by  UUID REFERENCES users(id),
  -- Người đã viết definition hiện tại (user hoặc API key): người này, hay người tạo key,
  -- không duyệt được cổng approval của các lần chạy definition đó.
  definition_by_type TEXT CHECK (definition_by_type IN ('user','api_key')),
  definition_by      UUID,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  FOREIGN KEY (org_id, project_id) REFERENCES projects (org_id, id),
  UNIQUE (project_id, slug),
  UNIQUE (org_id, project_id, id),
  CHECK ((definition_by IS NULL) = (definition_by_type IS NULL)),
  -- Workflow đang hoạt động luôn có người chạy.
  CHECK (status <> 'active' OR run_as IS NOT NULL)
);
CREATE INDEX workflows_list_idx ON workflows (org_id, project_id, created_at DESC, id DESC);

CREATE TABLE workflow_executions (
  id                  UUID PRIMARY KEY DEFAULT uuidv7(),
  org_id              UUID NOT NULL REFERENCES organizations(id),
  project_id          UUID NOT NULL,
  workflow_id         UUID NOT NULL,
  workflow_version    INT  NOT NULL,
  -- Định nghĩa đúng như lúc chạy: sửa workflow sau đó không đổi lần chạy này.
  definition_snapshot JSONB NOT NULL,
  -- Người đã viết bản definition được chụp (chép từ workflows lúc tạo lần chạy).
  definition_by_type  TEXT CHECK (definition_by_type IN ('user','api_key')),
  definition_by       UUID,
  trigger_type        TEXT NOT NULL CHECK (trigger_type IN ('manual','schedule','api')),
  -- User (manual; schedule = run_as của workflow) hoặc id của API key (api): không có khoá ngoại.
  triggered_by        UUID,
  -- Khoá của lần kích hoạt theo lịch (một lần lịch nổ = một khoá): bước tạo lần chạy
  -- được thử lại không tạo lần chạy thứ hai. NULL với lần chạy thủ công.
  trigger_key         TEXT CHECK (trigger_key IS NULL OR length(trigger_key) BETWEEN 1 AND 255),
  status              TEXT NOT NULL DEFAULT 'queued' CHECK (status IN
    ('queued','running','waiting_approval','paused','completed','failed','cancelled','compensating','compensated',
     'budget_exceeded','timed_out')),
  input               JSONB,
  output              JSONB,
  error               JSONB,
  -- Execution `planned` chạy lần này (context Execution). NULL khi chưa tạo được. Tham
  -- chiếu mềm, không khoá ngoại: nguồn sự thật của liên kết là
  -- agent_executions.workflow_execution_id (có khoá ngoại ghép và unique); cột này là bản
  -- ghi lại để đọc nhanh, và lần chạy sống lâu hơn execution của nó (retention).
  execution_id        UUID,
  started_at          TIMESTAMPTZ,
  finished_at         TIMESTAMPTZ,
  created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (org_id, id),
  FOREIGN KEY (org_id, project_id) REFERENCES projects (org_id, id),
  -- Lần chạy thuộc đúng project của workflow.
  FOREIGN KEY (org_id, project_id, workflow_id) REFERENCES workflows (org_id, project_id, id),
  UNIQUE (org_id, workflow_id, trigger_key),
  -- Lần chạy theo lịch luôn có khoá (khử trùng); lần chạy khác thì không.
  CHECK ((trigger_type = 'schedule') = (trigger_key IS NOT NULL))
);
CREATE INDEX workflow_executions_list_idx ON workflow_executions (org_id, workflow_id, created_at DESC, id DESC);

ALTER TABLE workflows ENABLE ROW LEVEL SECURITY;
ALTER TABLE workflows FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON workflows
  USING      (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid)
  WITH CHECK (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid);
ALTER TABLE workflow_executions ENABLE ROW LEVEL SECURITY;
ALTER TABLE workflow_executions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON workflow_executions
  USING      (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid)
  WITH CHECK (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid);

-- Định danh của workflow bất biến; archived là trạng thái cuối; version chỉ tăng, và đổi
-- definition phải tăng version.
-- +goose StatementBegin
CREATE FUNCTION workflows_guard() RETURNS trigger
LANGUAGE plpgsql AS $fn$
BEGIN
  IF OLD.status = 'archived' THEN
    RAISE EXCEPTION 'workflows: % đã archived', OLD.id USING ERRCODE = 'restrict_violation';
  END IF;
  IF NEW.org_id IS DISTINCT FROM OLD.org_id OR NEW.project_id IS DISTINCT FROM OLD.project_id
     OR NEW.slug IS DISTINCT FROM OLD.slug OR NEW.created_by IS DISTINCT FROM OLD.created_by
     OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
    RAISE EXCEPTION 'workflows: định danh của % không đổi được', OLD.id USING ERRCODE = 'restrict_violation';
  END IF;
  -- Đổi definition phải ghi người viết nó (không để trống, không giữ người cũ một cách vô ý:
  -- ứng dụng luôn ghi principal của request).
  IF NEW.definition IS DISTINCT FROM OLD.definition AND NEW.definition_by IS NULL THEN
    RAISE EXCEPTION 'workflows: đổi definition của % phải ghi người viết', OLD.id USING ERRCODE = 'restrict_violation';
  END IF;
  IF NEW.version < OLD.version OR (NEW.definition IS DISTINCT FROM OLD.definition AND NEW.version <= OLD.version) THEN
    RAISE EXCEPTION 'workflows: đổi definition của % phải tăng version', OLD.id USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END
$fn$;
-- +goose StatementEnd
CREATE TRIGGER workflows_guard BEFORE UPDATE ON workflows
  FOR EACH ROW EXECUTE FUNCTION workflows_guard();

-- Bản chụp của một lần chạy bất biến; trạng thái kết thúc không đổi nữa.
-- +goose StatementBegin
CREATE FUNCTION workflow_executions_guard() RETURNS trigger
LANGUAGE plpgsql AS $fn$
BEGIN
  IF NEW.definition_snapshot IS DISTINCT FROM OLD.definition_snapshot OR NEW.workflow_id IS DISTINCT FROM OLD.workflow_id
     OR NEW.workflow_version IS DISTINCT FROM OLD.workflow_version OR NEW.trigger_type IS DISTINCT FROM OLD.trigger_type
     OR NEW.triggered_by IS DISTINCT FROM OLD.triggered_by OR NEW.input IS DISTINCT FROM OLD.input
     OR NEW.trigger_key IS DISTINCT FROM OLD.trigger_key
     OR NEW.definition_by IS DISTINCT FROM OLD.definition_by OR NEW.definition_by_type IS DISTINCT FROM OLD.definition_by_type
     OR NEW.org_id IS DISTINCT FROM OLD.org_id OR NEW.project_id IS DISTINCT FROM OLD.project_id
     OR NEW.created_at IS DISTINCT FROM OLD.created_at
     OR (OLD.execution_id IS NOT NULL AND NEW.execution_id IS DISTINCT FROM OLD.execution_id) THEN
    RAISE EXCEPTION 'workflow_executions: bản chụp của lần chạy % không đổi được', OLD.id USING ERRCODE = 'restrict_violation';
  END IF;
  IF OLD.status IN ('completed','failed','cancelled','compensated','budget_exceeded','timed_out') THEN
    RAISE EXCEPTION 'workflow_executions: lần chạy % đã kết thúc', OLD.id USING ERRCODE = 'restrict_violation';
  END IF;
  IF NEW.status = 'queued' AND OLD.status <> 'queued' THEN
    RAISE EXCEPTION 'workflow_executions: lần chạy % không quay lại queued', OLD.id USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END
$fn$;
-- +goose StatementEnd
CREATE TRIGGER workflow_executions_guard BEFORE UPDATE ON workflow_executions
  FOR EACH ROW EXECUTE FUNCTION workflow_executions_guard();
-- Lần chạy được tạo ở trạng thái queued, chưa có execution hay kết quả.
-- +goose StatementBegin
CREATE FUNCTION workflow_executions_insert_guard() RETURNS trigger
LANGUAGE plpgsql AS $fn$
BEGIN
  IF NEW.status <> 'queued' OR NEW.execution_id IS NOT NULL OR NEW.output IS NOT NULL OR NEW.error IS NOT NULL
     OR NEW.started_at IS NOT NULL OR NEW.finished_at IS NOT NULL THEN
    RAISE EXCEPTION 'workflow_executions: lần chạy mới phải ở trạng thái queued, chưa có kết quả' USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END
$fn$;
-- +goose StatementEnd
CREATE TRIGGER workflow_executions_insert_guard BEFORE INSERT ON workflow_executions
  FOR EACH ROW EXECUTE FUNCTION workflow_executions_insert_guard();

-- Chỉ process api quản lý workflow. Process chạy vòng lặp và Gateway không đọc các bảng này.
GRANT SELECT, INSERT ON workflows, workflow_executions TO app_api;
GRANT UPDATE (name, description, version, definition, definition_by_type, definition_by, schedule, status, run_as, schedule_failures, updated_at) ON workflows TO app_api;
GRANT UPDATE (status, output, error, execution_id, started_at, finished_at, updated_at) ON workflow_executions TO app_api;
GRANT SELECT ON workflows, workflow_executions TO app_admin;

-- Quyền: xem, quản lý và chạy workflow.
ALTER TABLE roles NO FORCE ROW LEVEL SECURITY;
-- +goose StatementBegin
DO $$
DECLARE
  n int;
  total int := 0;
  r record;
  grants jsonb := '{
    "org_admin":     ["workflow:read","workflow:write","workflow:run"],
    "project_admin": ["workflow:read","workflow:write","workflow:run"],
    "developer":     ["workflow:read","workflow:write","workflow:run"],
    "operator":      ["workflow:read","workflow:run"],
    "viewer":        ["workflow:read"]
  }';
BEGIN
  FOR r IN SELECT key, value FROM jsonb_each(grants) LOOP
    UPDATE roles SET permissions = permissions || ARRAY(SELECT jsonb_array_elements_text(r.value)), updated_at = now()
    WHERE org_id IS NULL AND name = r.key;
    GET DIAGNOSTICS n = ROW_COUNT;
    total := total + n;
  END LOOP;
  IF total <> 5 THEN
    RAISE EXCEPTION 'cập nhật role dựng sẵn tác động % dòng, muốn 5', total;
  END IF;
END
$$;
-- +goose StatementEnd
ALTER TABLE roles FORCE ROW LEVEL SECURITY;

-- Execution do một lần chạy workflow tạo ra trỏ về lần chạy đó. Đặt cuối migration: khoá
-- trên agent_executions được giữ ngắn nhất có thể.
ALTER TABLE agent_executions ADD CONSTRAINT agent_executions_wfe_fk
  FOREIGN KEY (org_id, workflow_execution_id) REFERENCES workflow_executions (org_id, id) NOT VALID; -- VALIDATE ở 00031
-- Liên kết tới lần chạy workflow là một phần của cấu hình đã chụp.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION agent_executions_snapshot_guard() RETURNS trigger
LANGUAGE plpgsql AS $fn$
BEGIN
  IF NEW.budget_limit IS DISTINCT FROM OLD.budget_limit OR NEW.input IS DISTINCT FROM OLD.input
     OR NEW.project_id IS DISTINCT FROM OLD.project_id OR NEW.initiator_type IS DISTINCT FROM OLD.initiator_type
     OR NEW.initiator_id IS DISTINCT FROM OLD.initiator_id OR NEW.deadline_at IS DISTINCT FROM OLD.deadline_at
     OR NEW.plan IS DISTINCT FROM OLD.plan OR NEW.mode IS DISTINCT FROM OLD.mode
     OR NEW.plan_source IS DISTINCT FROM OLD.plan_source
     OR NEW.workflow_execution_id IS DISTINCT FROM OLD.workflow_execution_id THEN
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
     OR NEW.plan IS DISTINCT FROM OLD.plan OR NEW.mode IS DISTINCT FROM OLD.mode
     OR NEW.plan_source IS DISTINCT FROM OLD.plan_source THEN
    RAISE EXCEPTION 'agent_executions: cấu hình đã chụp của execution % không đổi được', OLD.id USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END
$fn$;
-- +goose StatementEnd
ALTER TABLE roles NO FORCE ROW LEVEL SECURITY;
UPDATE roles SET permissions = array_remove(array_remove(array_remove(permissions, 'workflow:read'), 'workflow:write'), 'workflow:run'), updated_at = now()
WHERE org_id IS NULL;
ALTER TABLE roles FORCE ROW LEVEL SECURITY;
ALTER TABLE agent_executions DROP CONSTRAINT agent_executions_wfe_fk;
DROP TABLE workflow_executions;
DROP FUNCTION workflow_executions_guard();
DROP FUNCTION workflow_executions_insert_guard();
DROP TABLE workflows;
DROP FUNCTION workflows_guard();
