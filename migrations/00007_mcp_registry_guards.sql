-- +goose Up
SET LOCAL lock_timeout = '5s';

-- Siết MCP Registry ở mức database (sau review của 00006). Ba bảng mới và nhỏ, nên
-- các thay đổi dưới đây không cần chia bước.

-- 1. Version đang hoạt động phải là version đã duyệt. Khoá ngoại của 00006 chỉ bảo
-- đảm version thuộc đúng server. Version approved không đổi trạng thái được nữa
-- (trigger của 00006), nên kiểm lúc gán là đủ.
-- +goose StatementBegin
CREATE FUNCTION mcp_servers_active_version_guard() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, public   -- bảng tạm cùng tên không che được bảng thật
AS $fn$
BEGIN
  IF NEW.active_version_id IS NOT NULL AND NOT EXISTS (
       SELECT FROM public.mcp_server_versions WHERE id = NEW.active_version_id AND review_status = 'approved') THEN
    RAISE EXCEPTION 'mcp_servers: active_version_id phải là version đã approved' USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END
$fn$;
-- +goose StatementEnd
CREATE TRIGGER mcp_servers_active_version_approved BEFORE INSERT OR UPDATE OF active_version_id ON mcp_servers
  FOR EACH ROW EXECUTE FUNCTION mcp_servers_active_version_guard();

-- 2. Tool chỉ được thêm trong chính transaction tạo ra version của nó. Sau đó tập
-- tool của version là ảnh chụp đã đóng: không thể chèn tool vào version đã duyệt mà
-- không qua discovery và snapshot_hash.
--
-- Transaction tạo version được ghi vào một cột thật (xid8, không quay vòng), không
-- suy từ xmin: xmin đổi ở mỗi UPDATE, nên transaction DUYỆT version cũng sẽ khớp.
-- Trigger bất biến của 00006 so toàn bộ dòng, nên cột này không sửa được.
ALTER TABLE mcp_server_versions ADD COLUMN created_xact xid8 NOT NULL DEFAULT pg_current_xact_id();
-- Version đã có từ trước được đóng lại: không transaction nào về sau mang xid này.
-- +goose StatementBegin
CREATE FUNCTION mcp_tools_insert_guard() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, public
AS $fn$
BEGIN
  IF NOT EXISTS (
       SELECT FROM public.mcp_server_versions
       WHERE id = NEW.server_version_id AND created_xact = pg_current_xact_id()) THEN
    RAISE EXCEPTION 'mcp_tools: chỉ thêm được tool trong transaction tạo version' USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END
$fn$;
-- +goose StatementEnd
CREATE TRIGGER mcp_tools_sealed_version BEFORE INSERT ON mcp_tools
  FOR EACH ROW EXECUTE FUNCTION mcp_tools_insert_guard();

-- 3. Schema của tool phải là schema của một object: SDK MCP từ chối (panic) tool có
-- inputSchema không khai type object.
-- COALESCE: thiếu khoá type cho NULL, và CHECK coi NULL là đạt.
ALTER TABLE mcp_tools ADD CONSTRAINT mcp_tools_input_schema_object CHECK (COALESCE(input_schema->>'type' = 'object', false));

-- 4. Chỉ cấp thứ code đang dùng. Đổi endpoint chuyển mọi tool đã duyệt sang host khác
-- mà không tạo version mới; khi cần, nó phải đi qua một đường có audit.
REVOKE UPDATE (name, description, endpoint, health_status, health_checked_at) ON mcp_servers FROM app_admin;

-- +goose Down
GRANT UPDATE (name, description, endpoint, health_status, health_checked_at) ON mcp_servers TO app_admin;
ALTER TABLE mcp_tools DROP CONSTRAINT mcp_tools_input_schema_object;
DROP TRIGGER mcp_tools_sealed_version ON mcp_tools;
DROP FUNCTION mcp_tools_insert_guard();
ALTER TABLE mcp_server_versions DROP COLUMN created_xact;
DROP TRIGGER mcp_servers_active_version_approved ON mcp_servers;
DROP FUNCTION mcp_servers_active_version_guard();
