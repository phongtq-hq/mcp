-- +goose Up
SET LOCAL lock_timeout = '5s';

-- Tenant tự đăng ký MCP server NGOÀI qua Management API (mcp-platform §5). Managed
-- server (code của platform, được tin annotation chỉ đọc, nhận ngữ cảnh ký) vẫn chỉ
-- đăng ký được qua đường quản trị.

-- Credential của server ngoài là một secret của chính org đó: cột thật có khoá ngoại
-- ghép, không phải một id nằm trong JSONB. Database từ chối server trỏ tới secret của
-- org khác; server của platform không mang secret.
ALTER TABLE mcp_servers ADD COLUMN secret_id UUID;
ALTER TABLE mcp_servers ADD CONSTRAINT mcp_servers_secret_fk
  FOREIGN KEY (org_id, secret_id) REFERENCES secrets (org_id, id) NOT VALID;
ALTER TABLE mcp_servers VALIDATE CONSTRAINT mcp_servers_secret_fk;
ALTER TABLE mcp_servers ADD CONSTRAINT mcp_servers_secret_scope
  CHECK (secret_id IS NULL OR org_id IS NOT NULL) NOT VALID;
ALTER TABLE mcp_servers VALIDATE CONSTRAINT mcp_servers_secret_scope;

-- Slug của server đã lưu trữ dùng lại được (đăng ký lại là cách duy nhất để đổi
-- endpoint): tính duy nhất chỉ áp cho server chưa lưu trữ.
ALTER TABLE mcp_servers DROP CONSTRAINT mcp_servers_org_id_slug_key;
CREATE UNIQUE INDEX mcp_servers_org_slug_live_key ON mcp_servers (org_id, slug) NULLS NOT DISTINCT WHERE status <> 'archived';

GRANT INSERT ON mcp_servers, mcp_server_versions, mcp_tools TO app_api;
GRANT UPDATE (name, description, status, active_version_id, secret_id, updated_at) ON mcp_servers TO app_api;
GRANT UPDATE (review_status, reviewed_by, reviewed_at) ON mcp_server_versions TO app_api;
-- Gồm cả các cột mà việc kế thừa phân loại khi duyệt version mới ghi.
GRANT UPDATE (enabled, risk_level, side_effect, data_sensitivity, egress, cacheable, timeout_ms, max_result_bytes, updated_at) ON mcp_tools TO app_api;

-- Lớp chặn ở database, không chỉ ở code: role của api chỉ tạo và sửa được server
-- NGOÀI qua Streamable HTTP. Một lỗi ở tầng ứng dụng không biến được server của
-- tenant thành managed server.
CREATE POLICY api_external_only_insert ON mcp_servers AS RESTRICTIVE FOR INSERT TO app_api
  WITH CHECK (kind = 'external' AND transport = 'streamable_http' AND auth_method IN ('none','org_secret') AND stdio_spec IS NULL);
CREATE POLICY api_external_only_update ON mcp_servers AS RESTRICTIVE FOR UPDATE TO app_api
  USING (kind = 'external') WITH CHECK (kind = 'external');
-- Tool và version của managed server cũng không sửa được bởi api.
CREATE POLICY api_external_only_update ON mcp_tools AS RESTRICTIVE FOR UPDATE TO app_api
  USING (EXISTS (SELECT 1 FROM mcp_servers s WHERE s.id = mcp_tools.server_id AND s.kind = 'external'));
CREATE POLICY api_external_only_insert ON mcp_tools AS RESTRICTIVE FOR INSERT TO app_api
  WITH CHECK (EXISTS (SELECT 1 FROM mcp_servers s WHERE s.id = mcp_tools.server_id AND s.kind = 'external'));
CREATE POLICY api_external_only_update ON mcp_server_versions AS RESTRICTIVE FOR UPDATE TO app_api
  USING (EXISTS (SELECT 1 FROM mcp_servers s WHERE s.id = mcp_server_versions.server_id AND s.kind = 'external'));
-- Version của server ngoài luôn bắt đầu ở trạng thái chờ duyệt: api không chèn thẳng
-- được một version đã duyệt.
CREATE POLICY api_external_only_insert ON mcp_server_versions AS RESTRICTIVE FOR INSERT TO app_api
  WITH CHECK (review_status = 'pending_review' AND reviewed_by IS NULL AND reviewed_at IS NULL
              AND EXISTS (SELECT 1 FROM mcp_servers s WHERE s.id = mcp_server_versions.server_id AND s.kind = 'external'));

-- Slug của platform và của org dùng chung một không gian tên (tên tool phía client và
-- đích của ngữ cảnh ký đều là slug). Trước đây chỉ kiểm ở code quản trị; nay tenant tự
-- đăng ký nên ràng buộc nằm ở database. Dòng của platform luôn nhìn thấy được
-- (platform_read), nên trigger chạy bằng quyền người gọi vẫn thấy xung đột với platform;
-- phía quản trị (BYPASSRLS) thấy mọi org.
-- +goose StatementBegin
CREATE FUNCTION mcp_servers_slug_guard() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, public
AS $fn$
BEGIN
  PERFORM pg_advisory_xact_lock(hashtextextended('mcp_slug:' || NEW.slug, 0));
  IF EXISTS (SELECT 1 FROM public.mcp_servers s
             WHERE s.slug = NEW.slug AND s.status <> 'archived' AND (s.org_id IS NULL) <> (NEW.org_id IS NULL)) THEN
    RAISE EXCEPTION 'mcp_servers: slug % đã được dùng ở phạm vi khác', NEW.slug USING ERRCODE = 'unique_violation';
  END IF;
  RETURN NEW;
END
$fn$;
-- +goose StatementEnd
CREATE TRIGGER mcp_servers_slug_namespace BEFORE INSERT ON mcp_servers
  FOR EACH ROW EXECUTE FUNCTION mcp_servers_slug_guard();

ALTER TABLE roles NO FORCE ROW LEVEL SECURITY;
-- +goose StatementBegin
DO $$
DECLARE n int;
BEGIN
  UPDATE roles SET permissions = permissions || ARRAY['mcp_server:create','mcp_server:update','mcp_server:delete',
    'mcp_server:discover','mcp_server:review','mcp_tool:update'], updated_at = now()
  WHERE org_id IS NULL AND name = 'org_admin';
  GET DIAGNOSTICS n = ROW_COUNT;
  IF n <> 1 THEN
    RAISE EXCEPTION 'cập nhật role dựng sẵn tác động % dòng, muốn 1', n;
  END IF;
END
$$;
-- +goose StatementEnd
ALTER TABLE roles FORCE ROW LEVEL SECURITY;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE roles NO FORCE ROW LEVEL SECURITY;
UPDATE roles SET permissions = ARRAY(SELECT p FROM unnest(permissions) p WHERE p NOT IN
  ('mcp_server:create','mcp_server:update','mcp_server:delete','mcp_server:discover','mcp_server:review','mcp_tool:update'))
WHERE org_id IS NULL;
ALTER TABLE roles FORCE ROW LEVEL SECURITY;
DROP TRIGGER mcp_servers_slug_namespace ON mcp_servers;
DROP FUNCTION mcp_servers_slug_guard();
DROP POLICY api_external_only_insert ON mcp_server_versions;
DROP POLICY api_external_only_update ON mcp_server_versions;
DROP POLICY api_external_only_insert ON mcp_tools;
DROP POLICY api_external_only_update ON mcp_tools;
DROP POLICY api_external_only_update ON mcp_servers;
DROP POLICY api_external_only_insert ON mcp_servers;
REVOKE UPDATE (enabled, risk_level, side_effect, data_sensitivity, egress, cacheable, timeout_ms, max_result_bytes, updated_at) ON mcp_tools FROM app_api;
REVOKE UPDATE (review_status, reviewed_by, reviewed_at) ON mcp_server_versions FROM app_api;
REVOKE UPDATE (name, description, status, active_version_id, secret_id, updated_at) ON mcp_servers FROM app_api;
REVOKE INSERT ON mcp_servers, mcp_server_versions, mcp_tools FROM app_api;
-- Khôi phục ràng buộc duy nhất ban đầu: lỗi nếu đã có slug dùng lại sau khi lưu trữ.
DROP INDEX mcp_servers_org_slug_live_key;
ALTER TABLE mcp_servers ADD CONSTRAINT mcp_servers_org_id_slug_key UNIQUE NULLS NOT DISTINCT (org_id, slug);
ALTER TABLE mcp_servers DROP CONSTRAINT mcp_servers_secret_scope;
ALTER TABLE mcp_servers DROP CONSTRAINT mcp_servers_secret_fk;
ALTER TABLE mcp_servers DROP COLUMN secret_id;
