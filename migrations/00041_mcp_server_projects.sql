-- +goose Up
-- Gắn MCP server với project (review bảo mật 2026-11-25, lớp "chính sách project" của
-- security §4). Trước đây mọi server đang hoạt động của org dùng được ở MỌI project: một
-- developer của project sandbox đọc được log hay bảng giá của mọi hệ thống nội bộ đã nối.
--
-- project_scoped = false (mặc định, giữ hành vi cũ): server dùng được ở mọi project của org.
-- project_scoped = true: chỉ ở các project có dòng trong mcp_server_projects. Server của
-- platform (org_id IS NULL) không giới hạn theo project được: nó không thuộc org nào.
-- Ghi chỉ qua hqadmin (app_admin), có audit; Gateway và api chỉ đọc.
SET LOCAL lock_timeout = '5s';

ALTER TABLE mcp_servers ADD COLUMN project_scoped BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE mcp_servers ADD CONSTRAINT mcp_servers_project_scoped_org
  CHECK (NOT project_scoped OR org_id IS NOT NULL);
GRANT UPDATE (project_scoped) ON mcp_servers TO app_admin;

CREATE TABLE mcp_server_projects (
  org_id     UUID NOT NULL REFERENCES organizations(id),
  server_id  UUID NOT NULL,
  project_id UUID NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (org_id, server_id, project_id),
  FOREIGN KEY (org_id, server_id) REFERENCES mcp_servers (scope_org_id, id),
  FOREIGN KEY (org_id, project_id) REFERENCES projects (org_id, id)
);
CREATE INDEX mcp_server_projects_project_idx ON mcp_server_projects (org_id, project_id);
ALTER TABLE mcp_server_projects ENABLE ROW LEVEL SECURITY;
ALTER TABLE mcp_server_projects FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON mcp_server_projects
  USING      (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid)
  WITH CHECK (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid);
GRANT SELECT ON mcp_server_projects TO app_api, app_gateway;
GRANT SELECT, INSERT, DELETE ON mcp_server_projects TO app_admin;

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TABLE mcp_server_projects;
REVOKE UPDATE (project_scoped) ON mcp_servers FROM app_admin;
ALTER TABLE mcp_servers DROP CONSTRAINT mcp_servers_project_scoped_org;
ALTER TABLE mcp_servers DROP COLUMN project_scoped;
