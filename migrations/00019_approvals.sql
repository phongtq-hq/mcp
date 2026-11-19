-- +goose Up
-- Phase 2: Approval cơ bản và tool ghi (security §4.3, §4.4).
--
-- approvals: một approval gắn với đúng một hành động — tool call ở (task, step_seq) với
-- đúng hash arguments. Gateway tạo approval khi policy yêu cầu duyệt và tiêu thụ nó
-- (consumed) khi thực thi; api nhận quyết định của người duyệt.
--
-- mcp_tool_claims: Gateway ghi một dòng trước khi gọi upstream cho mọi tool không chỉ
-- đọc. Bước bị ngắt rồi chạy lại gặp dòng đã có thì KHÔNG gọi lại tool: tool ghi không
-- bao giờ chạy hai lần cho cùng một step.
SET LOCAL lock_timeout = '5s';

CREATE TABLE approvals (
  id                 UUID PRIMARY KEY DEFAULT uuidv7(),
  org_id             UUID NOT NULL REFERENCES organizations(id),
  project_id         UUID NOT NULL,
  execution_id       UUID,
  task_id            UUID,
  step_seq           INT,
  parent_approval_id UUID REFERENCES approvals(id),
  subject_type       TEXT NOT NULL CHECK (subject_type IN ('tool_call','remediation_plan','workflow_step')),
  tool_id            UUID,
  tool_name          TEXT,
  args_hash          BYTEA,
  payload            JSONB NOT NULL,
  risk_level         TEXT NOT NULL CHECK (risk_level IN ('read','write','destructive','financial')),
  required_approvals INT  NOT NULL DEFAULT 1 CHECK (required_approvals BETWEEN 1 AND 2),
  decisions          JSONB NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(decisions) = 'array'),
  status             TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','approved','rejected','expired','cancelled','consumed')),
  requested_by_type  TEXT NOT NULL CHECK (requested_by_type IN ('user','api_key','agent','system')),
  requested_by       UUID,
  expires_at         TIMESTAMPTZ NOT NULL,
  resolved_at        TIMESTAMPTZ,
  created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (org_id, id),
  FOREIGN KEY (org_id, project_id) REFERENCES projects (org_id, id),
  FOREIGN KEY (org_id, execution_id) REFERENCES agent_executions (org_id, id) ON DELETE CASCADE,
  -- Approval của tool call phải trỏ tới đúng một bước của một task.
  CHECK (subject_type <> 'tool_call' OR (execution_id IS NOT NULL AND task_id IS NOT NULL AND step_seq IS NOT NULL
         AND tool_id IS NOT NULL AND tool_name IS NOT NULL AND args_hash IS NOT NULL))
);
-- Mỗi bước của một task chỉ có một approval.
CREATE UNIQUE INDEX approvals_step_key ON approvals (org_id, task_id, step_seq) WHERE task_id IS NOT NULL;
CREATE INDEX approvals_pending_idx ON approvals (org_id, project_id, expires_at) WHERE status = 'pending';
CREATE INDEX approvals_list_idx ON approvals (org_id, created_at DESC, id DESC);
CREATE INDEX approvals_execution_idx ON approvals (org_id, execution_id) WHERE execution_id IS NOT NULL;

ALTER TABLE approvals ENABLE ROW LEVEL SECURITY;
ALTER TABLE approvals FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON approvals
  USING      (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid)
  WITH CHECK (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid);

-- Vòng đời và bất biến của approval, với mọi role:
--   pending → approved | rejected | expired | cancelled;  approved → consumed | expired | cancelled.
-- Hành động được duyệt (tool, arguments, bước) không đổi được sau khi tạo; quyết định chỉ
-- được thêm, không sửa hay xoá.
-- +goose StatementBegin
CREATE FUNCTION approvals_guard() RETURNS trigger
LANGUAGE plpgsql AS $fn$
BEGIN
  IF (to_jsonb(NEW) - ARRAY['status','decisions','resolved_at']) <> (to_jsonb(OLD) - ARRAY['status','decisions','resolved_at']) THEN
    RAISE EXCEPTION 'approvals: nội dung của approval % không đổi được', OLD.id USING ERRCODE = 'restrict_violation';
  END IF;
  IF NEW.status <> OLD.status AND NOT (
       (OLD.status = 'pending'  AND NEW.status IN ('approved','rejected','expired','cancelled'))
    OR (OLD.status = 'approved' AND NEW.status IN ('consumed','expired','cancelled'))) THEN
    RAISE EXCEPTION 'approvals: % không chuyển được từ % sang %', OLD.id, OLD.status, NEW.status USING ERRCODE = 'restrict_violation';
  END IF;
  IF NEW.decisions <> OLD.decisions AND (OLD.status <> 'pending'
       OR jsonb_array_length(NEW.decisions) <> jsonb_array_length(OLD.decisions) + 1
       OR NOT (NEW.decisions @> OLD.decisions)) THEN
    RAISE EXCEPTION 'approvals: quyết định của % chỉ được thêm khi còn pending', OLD.id USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END
$fn$;
-- +goose StatementEnd
CREATE TRIGGER approvals_lifecycle BEFORE UPDATE ON approvals
  FOR EACH ROW EXECUTE FUNCTION approvals_guard();

-- gateway tạo và tiêu thụ approval; api nhận quyết định và huỷ approval của execution
-- bị huỷ; worker chỉ đọc trạng thái để biết khi nào thôi chờ.
GRANT SELECT, INSERT ON approvals TO app_gateway;
GRANT UPDATE (status, resolved_at) ON approvals TO app_gateway;
GRANT SELECT ON approvals TO app_api, app_admin;
GRANT UPDATE (status, decisions, resolved_at) ON approvals TO app_api;
GRANT SELECT (id, org_id, execution_id, task_id, step_seq, status, expires_at) ON approvals TO app_worker;

CREATE TABLE mcp_tool_claims (
  org_id     UUID NOT NULL REFERENCES organizations(id),
  task_id    UUID NOT NULL,
  step_seq   INT  NOT NULL,
  tool_id    UUID NOT NULL,
  args_hash  BYTEA NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (org_id, task_id, step_seq)
);
ALTER TABLE mcp_tool_claims ENABLE ROW LEVEL SECURITY;
ALTER TABLE mcp_tool_claims FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON mcp_tool_claims
  USING      (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid)
  WITH CHECK (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid);
-- Chỉ thêm: không role nào sửa hay xoá được một claim.
GRANT SELECT, INSERT ON mcp_tool_claims TO app_gateway;
GRANT SELECT ON mcp_tool_claims TO app_admin;

-- Step của tool call đang chờ duyệt ghi lại approval của nó (runtime ghi, khi Gateway
-- yêu cầu duyệt). Guard của step cho thêm đúng thay đổi đó: gán approval_id một lần
-- trong lúc step còn started; mọi thay đổi khác vẫn như cũ (chỉ hoàn tất một lần).
GRANT UPDATE (approval_id) ON execution_steps TO app_worker, app_api;
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION execution_steps_guard() RETURNS trigger
LANGUAGE plpgsql AS $fn$
BEGIN
  IF TG_OP = 'UPDATE' AND OLD.status = 'started' AND NEW.status <> 'started'
     AND (to_jsonb(NEW) - ARRAY['status','response','error','duration_ms','model_id','routing'])
       = (to_jsonb(OLD) - ARRAY['status','response','error','duration_ms','model_id','routing']) THEN
    RETURN NEW;
  END IF;
  IF TG_OP = 'UPDATE' AND OLD.status = 'started' AND NEW.status = 'started' AND OLD.kind = 'tool_call'
     AND OLD.approval_id IS NULL AND NEW.approval_id IS NOT NULL
     AND (to_jsonb(NEW) - 'approval_id') = (to_jsonb(OLD) - 'approval_id') THEN
    RETURN NEW;
  END IF;
  RAISE EXCEPTION 'execution_steps: % rejected', TG_OP USING ERRCODE = 'restrict_violation';
END
$fn$;
-- +goose StatementEnd

-- Permission: xem approval rộng; quyết định là việc của người duyệt và quản trị.
ALTER TABLE roles NO FORCE ROW LEVEL SECURITY;
-- +goose StatementBegin
DO $$
DECLARE
  n int;
  total int := 0;
  r record;
  grants jsonb := '{
    "org_admin":     ["approval:read","approval:decide"],
    "project_admin": ["approval:read","approval:decide"],
    "approver":      ["approval:read","approval:decide"],
    "operator":      ["approval:read"],
    "developer":     ["approval:read"]
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

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE roles NO FORCE ROW LEVEL SECURITY;
UPDATE roles SET permissions = array_remove(array_remove(permissions, 'approval:read'), 'approval:decide'), updated_at = now()
WHERE org_id IS NULL;
ALTER TABLE roles FORCE ROW LEVEL SECURITY;
REVOKE UPDATE (approval_id) ON execution_steps FROM app_worker, app_api;
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION execution_steps_guard() RETURNS trigger
LANGUAGE plpgsql AS $fn$
BEGIN
  IF TG_OP = 'UPDATE' AND OLD.status = 'started' AND NEW.status <> 'started'
     AND (to_jsonb(NEW) - ARRAY['status','response','error','duration_ms','model_id','routing'])
       = (to_jsonb(OLD) - ARRAY['status','response','error','duration_ms','model_id','routing']) THEN
    RETURN NEW;
  END IF;
  RAISE EXCEPTION 'execution_steps: % rejected', TG_OP USING ERRCODE = 'restrict_violation';
END
$fn$;
-- +goose StatementEnd
DROP TABLE mcp_tool_claims;
DROP TABLE approvals;
DROP FUNCTION approvals_guard();
