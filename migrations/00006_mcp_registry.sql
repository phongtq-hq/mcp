-- +goose Up
SET LOCAL lock_timeout = '5s';

-- MCP Registry. org_id NULL = server của platform (mọi tenant đọc được, không tenant
-- nào ghi được); có giá trị = server của một org. Version và tool luôn cùng phạm vi
-- với server của chúng (cột sinh scope_org_id + khoá ngoại ghép, như Model Catalog).
CREATE TABLE mcp_servers (
  id                UUID PRIMARY KEY DEFAULT uuidv7(),
  org_id            UUID REFERENCES organizations(id),
  slug              TEXT NOT NULL CHECK (slug ~ '^[a-z0-9][a-z0-9-]{1,62}$'),
  name              TEXT NOT NULL,
  description       TEXT,
  kind              TEXT NOT NULL CHECK (kind IN ('managed','external')),
  transport         TEXT NOT NULL CHECK (transport IN ('streamable_http','stdio')),
  endpoint          TEXT,               -- URL; NULL với stdio
  stdio_spec        JSONB,              -- image, command, args; chỉ platform admin
  auth_method       TEXT NOT NULL CHECK (auth_method IN ('none','service_token','org_secret','user_oauth')),
  auth_config       JSONB NOT NULL DEFAULT '{}',   -- tham chiếu secret_id, scope; không chứa secret
  status            TEXT NOT NULL DEFAULT 'registered' CHECK (status IN ('registered','active','disabled','archived')),
  health_status     TEXT NOT NULL DEFAULT 'unknown' CHECK (health_status IN ('unknown','healthy','degraded','unreachable')),
  health_checked_at TIMESTAMPTZ,
  active_version_id UUID,
  created_by        UUID REFERENCES users(id),
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  scope_org_id UUID GENERATED ALWAYS AS
    (COALESCE(org_id, '00000000-0000-0000-0000-000000000000'::uuid)) STORED,
  UNIQUE NULLS NOT DISTINCT (org_id, slug),
  UNIQUE (scope_org_id, id),
  CHECK (status <> 'active' OR active_version_id IS NOT NULL)
);
CREATE INDEX mcp_servers_org_created_idx ON mcp_servers (org_id, created_at DESC, id DESC);

ALTER TABLE mcp_servers ENABLE ROW LEVEL SECURITY;
ALTER TABLE mcp_servers FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON mcp_servers
  USING      (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid)
  WITH CHECK (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid);
CREATE POLICY platform_read ON mcp_servers FOR SELECT USING (org_id IS NULL);

-- Ảnh chụp bất biến của một lần discovery. Định nghĩa tool (tên, mô tả, schema) đi
-- vào prompt của model, nên mọi thay đổi tạo version mới chờ duyệt (chống đổi mô tả
-- tool sau khi đã được tin).
CREATE TABLE mcp_server_versions (
  id               UUID PRIMARY KEY DEFAULT uuidv7(),
  org_id           UUID REFERENCES organizations(id),
  server_id        UUID NOT NULL,
  version          INT  NOT NULL CHECK (version >= 1),
  reported_version TEXT,
  protocol_version TEXT NOT NULL,
  snapshot         JSONB NOT NULL,
  snapshot_hash    BYTEA NOT NULL,
  review_status    TEXT NOT NULL DEFAULT 'pending_review' CHECK (review_status IN ('pending_review','approved','rejected')),
  reviewed_by      UUID REFERENCES users(id),
  reviewed_at      TIMESTAMPTZ,
  discovered_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  scope_org_id UUID GENERATED ALWAYS AS
    (COALESCE(org_id, '00000000-0000-0000-0000-000000000000'::uuid)) STORED,
  FOREIGN KEY (scope_org_id, server_id) REFERENCES mcp_servers (scope_org_id, id),
  UNIQUE (server_id, version),
  UNIQUE (server_id, id),
  UNIQUE (scope_org_id, id)
);

ALTER TABLE mcp_server_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE mcp_server_versions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON mcp_server_versions
  USING      (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid)
  WITH CHECK (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid);
CREATE POLICY platform_read ON mcp_server_versions FOR SELECT USING (org_id IS NULL);

-- Version đang hoạt động phải là version của chính server đó.
ALTER TABLE mcp_servers ADD CONSTRAINT mcp_servers_active_version_fk
  FOREIGN KEY (id, active_version_id) REFERENCES mcp_server_versions (server_id, id);

CREATE TABLE mcp_tools (
  id                UUID PRIMARY KEY DEFAULT uuidv7(),
  org_id            UUID REFERENCES organizations(id),
  server_id         UUID NOT NULL,
  server_version_id UUID NOT NULL,
  name              TEXT NOT NULL CHECK (name ~ '^[A-Za-z0-9_-]{1,64}$'),
  description       TEXT,
  input_schema      JSONB NOT NULL CHECK (jsonb_typeof(input_schema) = 'object'),
  definition_hash   BYTEA NOT NULL,   -- SHA-256 của name + description + inputSchema chuẩn hoá
  -- Thuộc tính chính sách do platform quản lý. Mặc định là mức an toàn nhất: tool
  -- mới phát hiện không dùng được cho tới khi được phân loại.
  enabled           BOOLEAN NOT NULL DEFAULT false,
  risk_level        TEXT NOT NULL DEFAULT 'destructive' CHECK (risk_level IN ('read','write','destructive','financial')),
  side_effect       TEXT NOT NULL DEFAULT 'non_idempotent' CHECK (side_effect IN ('none','idempotent','non_idempotent')),
  data_sensitivity  TEXT NOT NULL DEFAULT 'internal' CHECK (data_sensitivity IN ('public','internal','sensitive')),
  egress            BOOLEAN NOT NULL DEFAULT false,
  cacheable         BOOLEAN NOT NULL DEFAULT false,
  timeout_ms        INT NOT NULL DEFAULT 30000 CHECK (timeout_ms BETWEEN 100 AND 600000),
  max_result_bytes  INT NOT NULL DEFAULT 65536 CHECK (max_result_bytes BETWEEN 1024 AND 4194304),
  unit_cost         NUMERIC(18,8) CHECK (unit_cost >= 0 AND unit_cost <> 'NaN'),
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  scope_org_id UUID GENERATED ALWAYS AS
    (COALESCE(org_id, '00000000-0000-0000-0000-000000000000'::uuid)) STORED,
  FOREIGN KEY (scope_org_id, server_id) REFERENCES mcp_servers (scope_org_id, id),
  FOREIGN KEY (server_id, server_version_id) REFERENCES mcp_server_versions (server_id, id),
  UNIQUE (server_version_id, name)
);
CREATE INDEX mcp_tools_server_idx ON mcp_tools (server_id, name);
CREATE INDEX mcp_tools_org_version_idx ON mcp_tools (org_id, server_version_id, name);

ALTER TABLE mcp_tools ENABLE ROW LEVEL SECURITY;
ALTER TABLE mcp_tools FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON mcp_tools
  USING      (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid)
  WITH CHECK (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid);
CREATE POLICY platform_read ON mcp_tools FOR SELECT USING (org_id IS NULL);

-- Version là ảnh chụp bất biến: chỉ được ghi kết quả duyệt, đúng một lần.
-- +goose StatementBegin
CREATE FUNCTION mcp_server_versions_guard() RETURNS trigger
LANGUAGE plpgsql AS $fn$
BEGIN
  IF TG_OP = 'UPDATE' THEN
    IF OLD.review_status = 'pending_review' AND NEW.review_status IN ('approved','rejected')
       -- scope_org_id là cột sinh: trong trigger BEFORE nó chưa được tính cho NEW.
       AND (to_jsonb(NEW) - ARRAY['review_status','reviewed_by','reviewed_at','scope_org_id'])
         = (to_jsonb(OLD) - ARRAY['review_status','reviewed_by','reviewed_at','scope_org_id']) THEN
      RETURN NEW;
    END IF;
  END IF;
  RAISE EXCEPTION 'mcp_server_versions is immutable (% rejected)', TG_OP USING ERRCODE = 'restrict_violation';
END
$fn$;
-- +goose StatementEnd
CREATE TRIGGER mcp_server_versions_no_update BEFORE UPDATE OR DELETE ON mcp_server_versions
  FOR EACH ROW EXECUTE FUNCTION mcp_server_versions_guard();
CREATE TRIGGER mcp_server_versions_no_truncate BEFORE TRUNCATE ON mcp_server_versions
  FOR EACH STATEMENT EXECUTE FUNCTION mcp_server_versions_guard();

-- Định nghĩa của tool (thứ đã được ghim hash và duyệt) là bất biến; chỉ thuộc tính
-- chính sách sửa được.
-- +goose StatementBegin
CREATE FUNCTION mcp_tools_guard() RETURNS trigger
LANGUAGE plpgsql AS $fn$
DECLARE
  -- Không gồm scope_org_id: cột sinh chưa được tính cho NEW trong trigger BEFORE, và
  -- nó chỉ phụ thuộc org_id (đã có trong danh sách).
  definition text[] := ARRAY['id','org_id','server_id','server_version_id','name','description','input_schema','definition_hash','created_at'];
  k text;
BEGIN
  IF TG_OP = 'UPDATE' THEN
    FOREACH k IN ARRAY definition LOOP
      IF (to_jsonb(NEW) -> k) IS DISTINCT FROM (to_jsonb(OLD) -> k) THEN
        RAISE EXCEPTION 'mcp_tools: định nghĩa tool là bất biến (cột %)', k USING ERRCODE = 'restrict_violation';
      END IF;
    END LOOP;
    RETURN NEW;
  END IF;
  RAISE EXCEPTION 'mcp_tools: % rejected', TG_OP USING ERRCODE = 'restrict_violation';
END
$fn$;
-- +goose StatementEnd
CREATE TRIGGER mcp_tools_no_redefine BEFORE UPDATE OR DELETE ON mcp_tools
  FOR EACH ROW EXECUTE FUNCTION mcp_tools_guard();
CREATE TRIGGER mcp_tools_no_truncate BEFORE TRUNCATE ON mcp_tools
  FOR EACH STATEMENT EXECUTE FUNCTION mcp_tools_guard();
-- Hai trigger trên áp dụng cho mọi role: xoá cứng một server phải DISABLE TRIGGER
-- trong một migration có chủ đích.

-- Đăng ký, discovery, duyệt và phân loại hiện chỉ đi qua đường quản trị (hqadmin,
-- role app_admin). Tenant chưa tự đăng ký server ngoài: việc đó cần SSRF guard và
-- Secrets. api đọc registry; gateway đọc registry và ghi audit cho mỗi tool call.
GRANT SELECT ON mcp_servers, mcp_server_versions, mcp_tools TO app_api, app_gateway;
GRANT SELECT, INSERT ON mcp_servers, mcp_server_versions, mcp_tools TO app_admin;
GRANT UPDATE (name, description, endpoint, status, health_status, health_checked_at, active_version_id, updated_at) ON mcp_servers TO app_admin;
GRANT UPDATE (review_status, reviewed_by, reviewed_at) ON mcp_server_versions TO app_admin;
GRANT UPDATE (enabled, risk_level, side_effect, data_sensitivity, egress, cacheable, timeout_ms, max_result_bytes, unit_cost, updated_at) ON mcp_tools TO app_admin;

-- Gateway xác thực người dùng đã có (không tạo user mới) và chọn tenant từ membership.
GRANT SELECT ON users, memberships TO app_gateway;
GRANT EXECUTE ON FUNCTION audit_append(uuid, uuid, text, uuid, uuid, text, text, uuid, text, uuid, jsonb, inet, text) TO app_gateway;

ALTER TABLE roles NO FORCE ROW LEVEL SECURITY;
-- +goose StatementBegin
DO $$
DECLARE
  n int;
  total int := 0;
  grants jsonb := '{
    "org_admin":     ["mcp_server:read","mcp_tool:call"],
    "project_admin": ["mcp_server:read","mcp_tool:call"],
    "operator":      ["mcp_server:read","mcp_tool:call"],
    "developer":     ["mcp_server:read","mcp_tool:call"],
    "approver":      ["mcp_server:read"],
    "viewer":        ["mcp_server:read"]
  }';
  r record;
BEGIN
  FOR r IN SELECT key, value FROM jsonb_each(grants) LOOP
    UPDATE roles SET permissions = permissions || ARRAY(SELECT jsonb_array_elements_text(r.value)), updated_at = now()
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
UPDATE roles SET permissions = ARRAY(SELECT p FROM unnest(permissions) p WHERE p !~ '^(mcp_server|mcp_tool):')
WHERE org_id IS NULL;
ALTER TABLE roles FORCE ROW LEVEL SECURITY;
REVOKE EXECUTE ON FUNCTION audit_append(uuid, uuid, text, uuid, uuid, text, text, uuid, text, uuid, jsonb, inet, text) FROM app_gateway;
REVOKE SELECT ON users, memberships FROM app_gateway;
ALTER TABLE mcp_servers DROP CONSTRAINT mcp_servers_active_version_fk;
DROP TABLE mcp_tools;
DROP FUNCTION mcp_tools_guard();
DROP TABLE mcp_server_versions;
DROP FUNCTION mcp_server_versions_guard();
DROP TABLE mcp_servers;
