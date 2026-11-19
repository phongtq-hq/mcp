-- +goose Up
-- Migration 00037 cấp lại UPDATE (endpoint) trên mcp_servers cho app_admin, cho đường
-- `hqadmin mcp-server set-endpoint`. Quyền theo cột áp cho mọi dòng; luật "chỉ managed
-- server, endpoint của server ngoài do tenant đăng ký không sửa được" khi đó chỉ nằm ở
-- một mệnh đề WHERE trong code. Đưa nó vào database, với mọi role.
SET LOCAL lock_timeout = '5s';

-- +goose StatementBegin
CREATE FUNCTION mcp_servers_endpoint_guard() RETURNS trigger
LANGUAGE plpgsql AS $fn$
BEGIN
  IF NEW.endpoint IS DISTINCT FROM OLD.endpoint AND OLD.kind <> 'managed' THEN
    RAISE EXCEPTION 'mcp_servers: endpoint của server % (%) không đổi được; đăng ký server mới', OLD.slug, OLD.kind
      USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END
$fn$;
-- +goose StatementEnd
CREATE TRIGGER mcp_servers_endpoint_managed_only BEFORE UPDATE OF endpoint ON mcp_servers
  FOR EACH ROW EXECUTE FUNCTION mcp_servers_endpoint_guard();

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TRIGGER mcp_servers_endpoint_managed_only ON mcp_servers;
DROP FUNCTION mcp_servers_endpoint_guard();
