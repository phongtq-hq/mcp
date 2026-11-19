-- +goose Up
SET LOCAL lock_timeout = '5s';

-- users là toàn cục (một người thuộc nhiều org): không có org_id, không RLS.
-- Danh tính là (idp_issuer, idp_subject); bản ghi được tạo ở lần đăng nhập đầu (ADR-0011).
CREATE TABLE users (
  id           UUID PRIMARY KEY DEFAULT uuidv7(),
  idp_issuer   TEXT NOT NULL,
  idp_subject  TEXT NOT NULL,
  email        TEXT NOT NULL,
  display_name TEXT,
  status       TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','disabled')),
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (idp_issuer, idp_subject)
);

CREATE TABLE memberships (
  org_id     UUID NOT NULL REFERENCES organizations(id),
  user_id    UUID NOT NULL REFERENCES users(id),
  status     TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('invited','active','removed')),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (org_id, user_id)
);
CREATE INDEX memberships_user_idx ON memberships (user_id);

ALTER TABLE memberships ENABLE ROW LEVEL SECURITY;
ALTER TABLE memberships FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON memberships
  USING      (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid)
  WITH CHECK (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid);
-- Lúc xác thực chưa biết org: một user được ĐỌC membership của chính mình ở mọi org
-- để platform chọn tenant cho request. Chỉ SELECT; ghi vẫn cần đúng tenant.
CREATE POLICY own_memberships ON memberships FOR SELECT
  USING (user_id = NULLIF(current_setting('app.user_id', true), '')::uuid);

CREATE TABLE roles (
  id          UUID PRIMARY KEY DEFAULT uuidv7(),
  org_id      UUID REFERENCES organizations(id),   -- NULL = role dựng sẵn của platform
  name        TEXT NOT NULL,
  description TEXT,
  permissions TEXT[] NOT NULL,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  -- Phạm vi của role dưới dạng không NULL (UUID không cho role của platform), để
  -- role_bindings tham chiếu được bằng khoá ngoại ghép: xem role_bindings.
  scope_org_id UUID GENERATED ALWAYS AS
    (COALESCE(org_id, '00000000-0000-0000-0000-000000000000'::uuid)) STORED,
  UNIQUE NULLS NOT DISTINCT (org_id, name),
  UNIQUE (scope_org_id, id)
);

-- Role dựng sẵn. Danh mục permission là hằng số trong code (internal/platform/authz).
-- Seed TRƯỚC khi bật RLS: sau FORCE, cả owner cũng không ghi được dòng org_id NULL.
-- Migration về sau cần sửa role dựng sẵn phải tạm NO FORCE ROW LEVEL SECURITY trong
-- cùng transaction và kiểm số dòng bị tác động (UPDATE bị RLS che trả 0 dòng, không lỗi).
INSERT INTO roles (org_id, name, description, permissions) VALUES
  (NULL, 'org_admin',      'Toàn quyền trong organization',
   ARRAY['organization:read','organization:update','project:read','project:create','project:update','project:archive']),
  (NULL, 'project_admin',  'Quản trị project',
   ARRAY['organization:read','project:read','project:update','project:archive']),
  (NULL, 'operator',       'Vận hành',        ARRAY['organization:read','project:read']),
  (NULL, 'developer',      'Phát triển',      ARRAY['organization:read','project:read']),
  (NULL, 'approver',       'Người duyệt',     ARRAY['organization:read','project:read']),
  (NULL, 'viewer',         'Chỉ đọc',         ARRAY['organization:read','project:read']),
  (NULL, 'billing_viewer', 'Xem chi phí',     ARRAY['organization:read']);

ALTER TABLE roles ENABLE ROW LEVEL SECURITY;
ALTER TABLE roles FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON roles
  USING      (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid)
  WITH CHECK (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid);
-- Role dựng sẵn của platform: mọi tenant đọc được, không tenant nào ghi được.
CREATE POLICY builtin_read ON roles FOR SELECT USING (org_id IS NULL);

CREATE TABLE role_bindings (
  id             UUID PRIMARY KEY DEFAULT uuidv7(),
  org_id         UUID NOT NULL REFERENCES organizations(id),
  role_id        UUID NOT NULL,
  -- Phạm vi của role được gắn: chính org này, hoặc UUID không cho role dựng sẵn.
  -- Cùng khoá ngoại ghép bên dưới, database từ chối binding trỏ tới role tuỳ biến
  -- của org khác (kiểm tra khoá ngoại không chịu RLS nên RLS không chặn được việc này).
  role_scope_org_id UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000'
    CHECK (role_scope_org_id IN (org_id, '00000000-0000-0000-0000-000000000000'::uuid)),
  principal_type TEXT NOT NULL CHECK (principal_type IN ('user','api_key')),
  principal_id   UUID NOT NULL,
  project_id     UUID,                              -- NULL = toàn org
  condition      JSONB,                             -- điều kiện ABAC, NULL = vô điều kiện
  created_by     UUID REFERENCES users(id),
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  FOREIGN KEY (org_id, project_id) REFERENCES projects (org_id, id),
  FOREIGN KEY (role_scope_org_id, role_id) REFERENCES roles (scope_org_id, id),
  UNIQUE NULLS NOT DISTINCT (org_id, role_id, principal_type, principal_id, project_id)
);
CREATE INDEX role_bindings_principal_idx ON role_bindings (org_id, principal_type, principal_id);

ALTER TABLE role_bindings ENABLE ROW LEVEL SECURITY;
ALTER TABLE role_bindings FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON role_bindings
  USING      (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid)
  WITH CHECK (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid);

-- Chỉ cấp quyền mà code hiện có dùng tới; quyền ghi membership và role binding
-- được cấp bằng migration mới khi có API quản lý chúng. users không có RLS nên
-- app_api không được UPDATE: sửa user là đường quản trị.
GRANT SELECT, INSERT ON users         TO app_api;
GRANT SELECT         ON memberships   TO app_api;
GRANT SELECT         ON roles         TO app_api, app_gateway, app_worker;
GRANT SELECT         ON role_bindings TO app_api, app_gateway, app_worker;
GRANT SELECT, INSERT, UPDATE, DELETE ON users, memberships, roles, role_bindings TO app_admin;

-- +goose Down
DROP TABLE role_bindings;
DROP TABLE roles;
DROP TABLE memberships;
DROP TABLE users;
