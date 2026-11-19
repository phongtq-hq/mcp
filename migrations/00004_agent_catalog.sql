-- +goose Up
SET LOCAL lock_timeout = '5s';

-- Agent là định danh ổn định; cấu hình nằm trong agent_versions bất biến (ADR-0006).
CREATE TABLE agents (
  id                UUID PRIMARY KEY DEFAULT uuidv7(),
  org_id            UUID NOT NULL REFERENCES organizations(id),
  project_id        UUID NOT NULL,
  slug              TEXT NOT NULL,
  name              TEXT NOT NULL,
  description       TEXT,
  status            TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft','active','disabled','archived')),
  active_version_id UUID,
  created_by        UUID REFERENCES users(id),
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  FOREIGN KEY (org_id, project_id) REFERENCES projects (org_id, id),
  UNIQUE (project_id, slug),
  UNIQUE (org_id, id),
  -- Agent đang hoạt động hoặc bị tắt luôn có một version được chọn.
  CHECK (status IN ('draft','archived') OR active_version_id IS NOT NULL)
);
CREATE INDEX agents_org_project_name_idx    ON agents (org_id, project_id, name, id);
CREATE INDEX agents_org_project_created_idx ON agents (org_id, project_id, created_at DESC, id DESC);

ALTER TABLE agents ENABLE ROW LEVEL SECURITY;
ALTER TABLE agents FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON agents
  USING      (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid)
  WITH CHECK (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid);

CREATE TABLE agent_versions (
  id            UUID PRIMARY KEY DEFAULT uuidv7(),
  org_id        UUID NOT NULL REFERENCES organizations(id),
  agent_id      UUID NOT NULL,
  version       INT  NOT NULL CHECK (version >= 1),
  system_prompt TEXT NOT NULL,
  config        JSONB NOT NULL,     -- model, limits, tools, delegation, memory, runtime
  config_hash   BYTEA NOT NULL,     -- SHA-256 của system_prompt + config chuẩn hoá
  change_note   TEXT,
  created_by    UUID REFERENCES users(id),
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  FOREIGN KEY (org_id, agent_id) REFERENCES agents (org_id, id),
  UNIQUE (agent_id, version),
  UNIQUE (agent_id, id)             -- đích cho khoá ngoại active_version của agents
);

ALTER TABLE agent_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE agent_versions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON agent_versions
  USING      (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid)
  WITH CHECK (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid);

-- Version đang hoạt động phải là version của chính agent đó.
ALTER TABLE agents ADD CONSTRAINT agents_active_version_fk
  FOREIGN KEY (id, active_version_id) REFERENCES agent_versions (agent_id, id);

-- agent_versions bất biến ở mức database, độc lập với GRANT: execution cũ phải
-- luôn tái hiện được đúng prompt và quyền nó đã chạy.
-- +goose StatementBegin
CREATE FUNCTION agent_versions_immutable() RETURNS trigger
LANGUAGE plpgsql AS $fn$
BEGIN
  RAISE EXCEPTION 'agent_versions is immutable (% rejected)', TG_OP
    USING ERRCODE = 'restrict_violation';
END
$fn$;
-- +goose StatementEnd
CREATE TRIGGER agent_versions_no_update BEFORE UPDATE OR DELETE ON agent_versions
  FOR EACH ROW EXECUTE FUNCTION agent_versions_immutable();
-- TRUNCATE không kích hoạt trigger mức dòng, và CASCADE qua khoá ngoại vòng sẽ xoá cả agents.
CREATE TRIGGER agent_versions_no_truncate BEFORE TRUNCATE ON agent_versions
  FOR EACH STATEMENT EXECUTE FUNCTION agent_versions_immutable();
-- Retention hoặc xoá cứng agent về sau phải DISABLE TRIGGER trong một migration có chủ đích.

-- Chỉ cấp cho process đang có code dùng. worker / gateway được cấp SELECT bằng
-- migration mới khi runtime cần đọc version (system_prompt nằm ở đây).
GRANT SELECT, INSERT ON agents, agent_versions TO app_api;
GRANT UPDATE (name, description, status, active_version_id, updated_at) ON agents TO app_api;
GRANT SELECT, INSERT, UPDATE ON agents TO app_admin;
GRANT SELECT, INSERT ON agent_versions TO app_admin;

-- Thêm permission của Agent cho role dựng sẵn. roles đã FORCE RLS và dòng dựng sẵn
-- có org_id NULL, nên tạm bỏ FORCE trong transaction này và kiểm số dòng bị tác động:
-- UPDATE bị RLS che trả về 0 dòng mà không báo lỗi.
ALTER TABLE roles NO FORCE ROW LEVEL SECURITY;
-- +goose StatementBegin
DO $$
DECLARE
  n int;
  total int := 0;
  grants jsonb := '{
    "org_admin":     ["agent:read","agent:create","agent:update","agent:publish","agent:archive"],
    "project_admin": ["agent:read","agent:create","agent:update","agent:publish","agent:archive"],
    "developer":     ["agent:read","agent:create","agent:update"],
    "operator":      ["agent:read"],
    "approver":      ["agent:read"],
    "viewer":        ["agent:read"]
  }';
  r record;
BEGIN
  FOR r IN SELECT key, value FROM jsonb_each(grants) LOOP
    UPDATE roles SET permissions = permissions || ARRAY(SELECT jsonb_array_elements_text(r.value)),
                     updated_at = now()
    WHERE org_id IS NULL AND name = r.key;
    GET DIAGNOSTICS n = ROW_COUNT;
    total := total + n;
  END LOOP;
  IF total <> 6 THEN
    RAISE EXCEPTION 'cập nhật role dựng sẵn tác động % dòng, muốn 6', total;
  END IF;
END
$$;
-- +goose StatementEnd
ALTER TABLE roles FORCE ROW LEVEL SECURITY;

-- +goose Down
ALTER TABLE roles NO FORCE ROW LEVEL SECURITY;
UPDATE roles SET permissions = ARRAY(SELECT p FROM unnest(permissions) p WHERE p NOT LIKE 'agent:%')
WHERE org_id IS NULL;
ALTER TABLE roles FORCE ROW LEVEL SECURITY;
ALTER TABLE agents DROP CONSTRAINT agents_active_version_fk;
DROP TABLE agent_versions;
DROP FUNCTION agent_versions_immutable();
DROP TABLE agents;
