-- +goose Up
SET LOCAL lock_timeout = '5s';

-- API key của platform (schema §4.1, security §4.1): xác thực cho máy và CI. Khoá
-- chỉ hiển thị một lần lúc tạo; database chỉ giữ SHA-256 của nó.
CREATE TABLE api_keys (
  id           UUID PRIMARY KEY DEFAULT uuidv7(),
  org_id       UUID NOT NULL REFERENCES organizations(id),
  project_id   UUID,                         -- NULL = khoá phạm vi org
  name         TEXT NOT NULL,
  key_prefix   TEXT NOT NULL UNIQUE CHECK (key_prefix ~ '^[a-z0-9]{12}$'),  -- phần tra cứu, không bí mật
  key_hash     BYTEA NOT NULL CHECK (octet_length(key_hash) = 32),          -- SHA-256 của cả khoá
  role_name    TEXT NOT NULL,                -- role dựng sẵn được gắn cho khoá, để hiển thị
  created_by   UUID NOT NULL REFERENCES users(id),
  expires_at   TIMESTAMPTZ NOT NULL,         -- không có khoá vô thời hạn
  revoked_at   TIMESTAMPTZ,
  last_used_at TIMESTAMPTZ,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  -- Transaction tạo ra khoá: role chỉ gắn được trong chính transaction đó.
  created_xact xid8 NOT NULL DEFAULT pg_current_xact_id(),
  FOREIGN KEY (org_id, project_id) REFERENCES projects (org_id, id),
  UNIQUE (org_id, id)
);
CREATE INDEX api_keys_org_created_idx ON api_keys (org_id, created_at DESC, id DESC);

ALTER TABLE api_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE api_keys FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON api_keys
  USING      (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid)
  WITH CHECK (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid);
-- Bước xác thực chưa biết org: transaction chỉ đặt app.api_key_prefix đọc được đúng
-- dòng mang prefix đó (như own_memberships cho user). Prefix không phải bí mật, và
-- dòng chỉ chứa hash của khoá.
CREATE POLICY key_lookup ON api_keys FOR SELECT
  USING (key_prefix = NULLIF(current_setting('app.api_key_prefix', true), ''));

-- Chỉ thu hồi và ghi dấu lần dùng cuối; mọi thứ khác của khoá là bất biến.
-- +goose StatementBegin
CREATE FUNCTION api_keys_guard() RETURNS trigger
LANGUAGE plpgsql AS $fn$
BEGIN
  IF (to_jsonb(NEW) - ARRAY['revoked_at','last_used_at']) IS DISTINCT FROM (to_jsonb(OLD) - ARRAY['revoked_at','last_used_at'])
     OR (OLD.revoked_at IS NOT NULL AND NEW.revoked_at IS DISTINCT FROM OLD.revoked_at) THEN
    RAISE EXCEPTION 'api_keys: chỉ thu hồi được khoá, không sửa được' USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END
$fn$;
-- +goose StatementEnd
CREATE TRIGGER api_keys_immutable BEFORE UPDATE ON api_keys
  FOR EACH ROW EXECUTE FUNCTION api_keys_guard();

GRANT SELECT, INSERT ON api_keys TO app_api;
GRANT UPDATE (revoked_at, last_used_at) ON api_keys TO app_api;
GRANT SELECT ON api_keys TO app_gateway;
GRANT UPDATE (last_used_at) ON api_keys TO app_gateway;
GRANT SELECT, INSERT, UPDATE, DELETE ON api_keys TO app_admin;

-- Gắn role cho một API key. Role ứng dụng không có INSERT trên role_bindings (gắn
-- quyền cho user vẫn là đường quản trị); hàm này là cổng cấp quyền duy nhất ở mức
-- database cho API key, nên các luật nằm ở đây chứ không chỉ ở code:
--   - role lấy từ chính dòng khoá (role_name), người gọi không chọn được;
--   - chỉ trong transaction tạo ra khoá, và mỗi khoá đúng một binding;
--   - role dựng sẵn chứa quyền quản lý khoá hoặc credential không gắn được cho khoá;
--   - khoá phải thuộc org của transaction, đúng phạm vi project của nó.
-- +goose StatementBegin
CREATE FUNCTION api_key_bind_role(p_key_id uuid, p_created_by uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, public
AS $fn$
DECLARE
  k    public.api_keys;
  rid  uuid;
  org  uuid := NULLIF(current_setting('app.org_id', true), '')::uuid;
BEGIN
  SELECT * INTO k FROM public.api_keys
   WHERE id = p_key_id AND org_id = org AND created_xact = pg_current_xact_id();
  IF NOT FOUND THEN
    RAISE EXCEPTION 'api_key_bind_role: khoá không tồn tại trong tenant hoặc không được tạo trong transaction này' USING ERRCODE = 'no_data_found';
  END IF;
  SELECT id INTO rid FROM public.roles
   WHERE org_id IS NULL AND name = k.role_name
     AND NOT EXISTS (SELECT 1 FROM unnest(permissions) p
                     WHERE p LIKE 'api\_key:%' OR p = 'model_provider:set_credential');
  IF NOT FOUND THEN
    RAISE EXCEPTION 'api_key_bind_role: role % không gắn được cho API key', k.role_name USING ERRCODE = 'no_data_found';
  END IF;
  IF EXISTS (SELECT 1 FROM public.role_bindings
             WHERE org_id = k.org_id AND principal_type = 'api_key' AND principal_id = k.id) THEN
    RAISE EXCEPTION 'api_key_bind_role: khoá đã được gắn role' USING ERRCODE = 'unique_violation';
  END IF;
  INSERT INTO public.role_bindings (org_id, role_id, principal_type, principal_id, project_id, created_by)
  VALUES (k.org_id, rid, 'api_key', k.id, k.project_id, p_created_by);
END
$fn$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION api_key_bind_role(uuid, uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION api_key_bind_role(uuid, uuid) TO app_api;

ALTER TABLE roles NO FORCE ROW LEVEL SECURITY;
-- +goose StatementBegin
DO $$
DECLARE
  n int;
  total int := 0;
  r record;
  grants jsonb := '{
    "org_admin":     ["api_key:read","api_key:create","api_key:revoke"],
    "project_admin": ["api_key:read","api_key:create","api_key:revoke"]
  }';
BEGIN
  FOR r IN SELECT key, value FROM jsonb_each(grants) LOOP
    UPDATE roles SET permissions = permissions || ARRAY(SELECT jsonb_array_elements_text(r.value)), updated_at = now()
    WHERE org_id IS NULL AND name = r.key;
    GET DIAGNOSTICS n = ROW_COUNT;
    total := total + n;
  END LOOP;
  IF total <> 2 THEN
    RAISE EXCEPTION 'cập nhật role dựng sẵn tác động % dòng, muốn 2', total;
  END IF;
END
$$;
-- +goose StatementEnd
ALTER TABLE roles FORCE ROW LEVEL SECURITY;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE roles NO FORCE ROW LEVEL SECURITY;
UPDATE roles SET permissions = ARRAY(SELECT p FROM unnest(permissions) p WHERE p !~ '^api_key:') WHERE org_id IS NULL;
ALTER TABLE roles FORCE ROW LEVEL SECURITY;
ALTER TABLE role_bindings NO FORCE ROW LEVEL SECURITY;
DELETE FROM role_bindings WHERE principal_type = 'api_key';
ALTER TABLE role_bindings FORCE ROW LEVEL SECURITY;
DROP FUNCTION api_key_bind_role(uuid, uuid);
DROP TABLE api_keys;
DROP FUNCTION api_keys_guard();
