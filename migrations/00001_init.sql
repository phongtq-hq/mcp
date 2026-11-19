-- +goose Up
-- Tài khoản chạy migration phải là superuser hoặc có CREATEROLE + BYPASSRLS:
-- migration tạo role và ghi dòng dùng chung của platform vào bảng đã FORCE RLS.
SET LOCAL lock_timeout = '5s';

-- Role nhóm, không đăng nhập được. User đăng nhập của từng process được tạo lúc
-- triển khai và là thành viên của đúng một role dưới đây (ADR-0001, ADR-0005).
-- app_admin chỉ dành cho job nền xuyên tenant (đối soát, retention): thuộc tính
-- BYPASSRLS không kế thừa qua membership, nên job phải SET ROLE app_admin tường minh.
-- +goose StatementBegin
DO $$
BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_api')     THEN CREATE ROLE app_api     NOLOGIN; END IF;
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_gateway') THEN CREATE ROLE app_gateway NOLOGIN; END IF;
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_worker')  THEN CREATE ROLE app_worker  NOLOGIN; END IF;
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'app_admin')   THEN CREATE ROLE app_admin   NOLOGIN BYPASSRLS; END IF;
END
$$;
-- +goose StatementEnd

-- Role có thể đã tồn tại từ trước với thuộc tính khác. Không tự sửa (đổi thuộc tính
-- SUPERUSER cần superuser): từ chối chạy tiếp nếu một role ứng dụng bỏ qua được RLS.
-- +goose StatementBegin
DO $$
DECLARE
  bad text;
BEGIN
  SELECT string_agg(rolname, ', ') INTO bad FROM pg_roles
  WHERE (rolname IN ('app_api','app_gateway','app_worker') AND (rolsuper OR rolbypassrls OR rolcanlogin))
     OR (rolname = 'app_admin' AND (rolsuper OR NOT rolbypassrls OR rolcanlogin));
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION 'role có thuộc tính không đúng: %. app_api/app_gateway/app_worker phải NOSUPERUSER NOBYPASSRLS NOLOGIN; app_admin phải NOSUPERUSER BYPASSRLS NOLOGIN', bad;
  END IF;
END
$$;
-- +goose StatementEnd

CREATE TABLE organizations (
  id          UUID PRIMARY KEY DEFAULT uuidv7(),
  slug        TEXT NOT NULL UNIQUE,
  name        TEXT NOT NULL,
  status      TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','suspended','archived')),
  settings    JSONB NOT NULL DEFAULT '{}',
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- organizations không có cột org_id; cách ly bằng chính id của nó.
ALTER TABLE organizations ENABLE ROW LEVEL SECURITY;
ALTER TABLE organizations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON organizations
  USING      (id = NULLIF(current_setting('app.org_id', true), '')::uuid)
  WITH CHECK (id = NULLIF(current_setting('app.org_id', true), '')::uuid);

CREATE TABLE projects (
  id          UUID PRIMARY KEY DEFAULT uuidv7(),
  org_id      UUID NOT NULL REFERENCES organizations(id),
  slug        TEXT NOT NULL,
  name        TEXT NOT NULL,
  environment TEXT NOT NULL DEFAULT 'non_prod' CHECK (environment IN ('prod','non_prod')),
  settings    JSONB NOT NULL DEFAULT '{}',
  archived_at TIMESTAMPTZ,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (org_id, slug),
  UNIQUE (org_id, id)
);
-- Liệt kê theo tenant, phân trang keyset: mới nhất trước, và theo tên.
CREATE INDEX projects_org_created_idx ON projects (org_id, created_at DESC, id DESC);
CREATE INDEX projects_org_name_idx    ON projects (org_id, name, id);

ALTER TABLE projects ENABLE ROW LEVEL SECURITY;
ALTER TABLE projects FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON projects
  USING      (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid)
  WITH CHECK (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid);

-- Quyền tối thiểu theo process. Tạo organization là đường quản trị (app_admin).
-- Tài nguyên cấu hình xoá mềm bằng archived_at nên không cấp DELETE.
-- UPDATE cấp theo cột: tenant không tự đổi được status / slug của organization
-- hay environment / slug của project, kể cả khi một truy vấn sau này sơ suất.
GRANT SELECT ON organizations TO app_api, app_gateway, app_worker;
GRANT UPDATE (name, settings, updated_at) ON organizations TO app_api;
GRANT SELECT ON projects TO app_api, app_gateway, app_worker;
GRANT INSERT ON projects TO app_api;
GRANT UPDATE (name, settings, archived_at, updated_at) ON projects TO app_api;
GRANT SELECT, INSERT, UPDATE, DELETE ON organizations, projects TO app_admin;

-- +goose Down
DROP TABLE projects;
DROP TABLE organizations;
-- Role là đối tượng toàn cluster, có thể được database khác dùng: không drop ở đây.
