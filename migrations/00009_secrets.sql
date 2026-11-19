-- +goose Up
SET LOCAL lock_timeout = '5s';

-- Secret (schema §4.8, ADR-0008, ADR-0012). Database chỉ giữ bản mã: plaintext không
-- có cột nào. Mỗi lần đổi giá trị là một dòng mới với version tăng dần; dòng cũ
-- không sửa được.
CREATE TABLE secrets (
  id            UUID PRIMARY KEY DEFAULT uuidv7(),
  org_id        UUID NOT NULL REFERENCES organizations(id),
  name          TEXT NOT NULL CHECK (name ~ '^[a-z0-9][a-z0-9_./-]{0,199}$'),
  kind          TEXT NOT NULL CHECK (kind IN ('api_key','oauth_client','ssh_key','kubeconfig','oauth_token')),
  owner_user_id UUID REFERENCES users(id),
  version       INT  NOT NULL DEFAULT 1 CHECK (version >= 1),
  ciphertext    BYTEA NOT NULL,          -- nonce || AES-256-GCM(DEK, plaintext)
  wrapped_dek   BYTEA NOT NULL,          -- DEK được bọc bởi lớp khoá ngoài database
  kek_ref       TEXT  NOT NULL,          -- provider và khoá đã bọc DEK
  rotation_interval INTERVAL,
  rotated_at    TIMESTAMPTZ,
  expires_at    TIMESTAMPTZ,
  created_by    UUID REFERENCES users(id),
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE NULLS NOT DISTINCT (org_id, name, owner_user_id, version),
  UNIQUE (org_id, id)
);

ALTER TABLE secrets ENABLE ROW LEVEL SECURITY;
ALTER TABLE secrets FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON secrets
  USING      (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid)
  WITH CHECK (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid);

-- +goose StatementBegin
CREATE FUNCTION secrets_guard() RETURNS trigger
LANGUAGE plpgsql AS $fn$
BEGIN
  RAISE EXCEPTION 'secrets: bản mã là bất biến (% rejected); đổi giá trị bằng version mới', TG_OP USING ERRCODE = 'restrict_violation';
END
$fn$;
-- +goose StatementEnd
CREATE TRIGGER secrets_immutable BEFORE UPDATE ON secrets
  FOR EACH ROW EXECUTE FUNCTION secrets_guard();

-- Phân quyền theo cột: api ghi được secret và đọc metadata, nhưng không đọc được bản
-- mã; chỉ gateway đọc được bản mã (và vẫn cần khoá gốc để giải mã).
GRANT INSERT ON secrets TO app_api;
GRANT SELECT (id, org_id, name, kind, owner_user_id, version, kek_ref, rotation_interval, rotated_at, expires_at, created_by, created_at, updated_at) ON secrets TO app_api;
GRANT SELECT ON secrets TO app_gateway;

-- Khoá của model provider là một secret của chính org đó.
ALTER TABLE model_providers ADD CONSTRAINT model_providers_secret_fk
  FOREIGN KEY (org_id, secret_id) REFERENCES secrets (org_id, id) NOT VALID;
ALTER TABLE model_providers VALIDATE CONSTRAINT model_providers_secret_fk;
-- Khoá ngoại ghép không kiểm gì khi org_id NULL: provider của platform không mang
-- secret (khoá của platform nằm ở cấu hình của gateway).
ALTER TABLE model_providers ADD CONSTRAINT model_providers_secret_scope
  CHECK (secret_id IS NULL OR org_id IS NOT NULL) NOT VALID;
ALTER TABLE model_providers VALIDATE CONSTRAINT model_providers_secret_scope;
GRANT UPDATE (secret_id) ON model_providers TO app_api;
-- Gateway gọi model thay runtime (ADR-0004, ADR-0008): cần biết model thuộc provider
-- nào và provider dùng secret nào.
GRANT SELECT (id, org_id, scope_org_id, provider_id, provider_model, enabled, deprecated_at) ON models TO app_gateway;
-- Gateway chặn lượt gọi model vượt max_iterations của Agent Version.
GRANT SELECT (iterations) ON agent_tasks TO app_gateway;
GRANT SELECT (id, org_id, scope_org_id, adapter, secret_id, enabled, archived_at) ON model_providers TO app_gateway;

ALTER TABLE roles NO FORCE ROW LEVEL SECURITY;
-- +goose StatementBegin
DO $$
DECLARE n int;
BEGIN
  -- Đặt hoặc xoay khoá của provider là quyền riêng, tách khỏi việc sửa cấu hình provider.
  UPDATE roles SET permissions = permissions || ARRAY['model_provider:set_credential'], updated_at = now()
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
-- Gỡ tham chiếu tới secret trước khi bỏ bảng, để Up chạy lại được.
ALTER TABLE model_providers NO FORCE ROW LEVEL SECURITY;
UPDATE model_providers SET secret_id = NULL WHERE secret_id IS NOT NULL;
ALTER TABLE model_providers FORCE ROW LEVEL SECURITY;
ALTER TABLE model_providers DROP CONSTRAINT model_providers_secret_scope;
ALTER TABLE roles NO FORCE ROW LEVEL SECURITY;
UPDATE roles SET permissions = array_remove(permissions, 'model_provider:set_credential') WHERE org_id IS NULL;
ALTER TABLE roles FORCE ROW LEVEL SECURITY;
REVOKE SELECT (id, org_id, scope_org_id, adapter, secret_id, enabled, archived_at) ON model_providers FROM app_gateway;
REVOKE SELECT (iterations) ON agent_tasks FROM app_gateway;
REVOKE SELECT (id, org_id, scope_org_id, provider_id, provider_model, enabled, deprecated_at) ON models FROM app_gateway;
REVOKE UPDATE (secret_id) ON model_providers FROM app_api;
ALTER TABLE model_providers DROP CONSTRAINT model_providers_secret_fk;
DROP TABLE secrets;
DROP FUNCTION secrets_guard();
