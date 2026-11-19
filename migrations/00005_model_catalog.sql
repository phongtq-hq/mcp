-- +goose Up
SET LOCAL lock_timeout = '5s';

-- Cần cho ràng buộc EXCLUDE trên model_pricing (so sánh uuid bằng "=" trong GiST).
-- btree_gist là extension "trusted": tài khoản migrate cần quyền CREATE trên database.
-- Down không gỡ extension (database khác đối tượng có thể đang dùng).
CREATE EXTENSION IF NOT EXISTS btree_gist;

-- Provider LLM. org_id NULL = cấu hình của platform (mọi tenant đọc được, không
-- tenant nào ghi được); có giá trị = provider riêng của org.
CREATE TABLE model_providers (
  id         UUID PRIMARY KEY DEFAULT uuidv7(),
  org_id     UUID REFERENCES organizations(id),
  slug       TEXT NOT NULL,
  adapter    TEXT NOT NULL,                       -- tên adapter trong code: 'claude-cli', 'anthropic'
  base_url   TEXT,
  secret_id  UUID,                                -- API key, khi Secrets được hiện thực
  enabled    BOOLEAN NOT NULL DEFAULT true,
  settings   JSONB NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(settings) = 'object'),
  archived_at TIMESTAMPTZ,                        -- xoá mềm: audit và usage vẫn tham chiếu được
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  -- Phạm vi dưới dạng không NULL (UUID không cho platform), đích của khoá ngoại ghép.
  scope_org_id UUID GENERATED ALWAYS AS
    (COALESCE(org_id, '00000000-0000-0000-0000-000000000000'::uuid)) STORED,
  UNIQUE NULLS NOT DISTINCT (org_id, slug),
  UNIQUE (scope_org_id, id)
);
CREATE INDEX model_providers_org_created_idx ON model_providers (org_id, created_at DESC, id DESC);

ALTER TABLE model_providers ENABLE ROW LEVEL SECURITY;
ALTER TABLE model_providers FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON model_providers
  USING      (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid)
  WITH CHECK (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid);
CREATE POLICY platform_read ON model_providers FOR SELECT USING (org_id IS NULL);

-- Model thuộc đúng một provider cùng phạm vi: model của org chỉ nằm dưới provider
-- của chính org đó, model của platform chỉ nằm dưới provider của platform. Khoá
-- ngoại ghép bảo đảm điều này ở mức database (kiểm tra khoá ngoại không chịu RLS).
CREATE TABLE models (
  id             UUID PRIMARY KEY DEFAULT uuidv7(),
  org_id         UUID REFERENCES organizations(id),
  provider_id    UUID NOT NULL,
  provider_model TEXT NOT NULL,       -- định danh phía provider
  display_name   TEXT NOT NULL,
  tier           TEXT CHECK (tier IN ('economy','standard','premium')),
  capabilities   JSONB NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(capabilities) = 'object'),
  enabled        BOOLEAN NOT NULL DEFAULT true,
  deprecated_at  TIMESTAMPTZ,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  scope_org_id UUID GENERATED ALWAYS AS
    (COALESCE(org_id, '00000000-0000-0000-0000-000000000000'::uuid)) STORED,
  FOREIGN KEY (scope_org_id, provider_id) REFERENCES model_providers (scope_org_id, id),
  UNIQUE (provider_id, provider_model),
  UNIQUE (scope_org_id, id)
);
CREATE INDEX models_org_created_idx ON models (org_id, created_at DESC, id DESC);

ALTER TABLE models ENABLE ROW LEVEL SECURITY;
ALTER TABLE models FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON models
  USING      (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid)
  WITH CHECK (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid);
CREATE POLICY platform_read ON models FOR SELECT USING (org_id IS NULL);

-- Bảng giá có phiên bản và khoảng hiệu lực; không có giá nào trong source code.
-- org_id NULL = giá niêm yết (platform); có giá trị = giá riêng của org.
-- Giá mới là dòng mới; dòng đang hiệu lực chỉ được ĐÓNG (đặt effective_to), không sửa giá.
CREATE TABLE model_pricing (
  id                    UUID PRIMARY KEY DEFAULT uuidv7(),
  model_id              UUID NOT NULL,
  org_id                UUID REFERENCES organizations(id),
  -- Phạm vi của model được định giá: chính org này, hoặc UUID không cho model của
  -- platform. Cùng khoá ngoại ghép, database từ chối giá trỏ tới model của org khác.
  model_scope_org_id    UUID NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000'
    CHECK (model_scope_org_id = '00000000-0000-0000-0000-000000000000'::uuid
           OR COALESCE(model_scope_org_id = org_id, false)),
  version               INT  NOT NULL CHECK (version >= 1),
  currency              CHAR(3) NOT NULL DEFAULT 'USD' CHECK (currency ~ '^[A-Z]{3}$'),
  -- NaN >= 0 là đúng trong PostgreSQL, nên phải loại NaN tường minh.
  input_per_mtok        NUMERIC(18,8) NOT NULL CHECK (input_per_mtok >= 0 AND input_per_mtok <> 'NaN'),
  output_per_mtok       NUMERIC(18,8) NOT NULL CHECK (output_per_mtok >= 0 AND output_per_mtok <> 'NaN'),
  cached_input_per_mtok NUMERIC(18,8) CHECK (cached_input_per_mtok >= 0 AND cached_input_per_mtok <> 'NaN'),
  cache_write_per_mtok  NUMERIC(18,8) CHECK (cache_write_per_mtok >= 0 AND cache_write_per_mtok <> 'NaN'),
  reasoning_per_mtok    NUMERIC(18,8) CHECK (reasoning_per_mtok >= 0 AND reasoning_per_mtok <> 'NaN'),
  effective_from        TIMESTAMPTZ NOT NULL,
  effective_to          TIMESTAMPTZ,
  source                TEXT,          -- nơi lấy giá, để kiểm tra lại
  created_by            UUID REFERENCES users(id),
  created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
  FOREIGN KEY (model_scope_org_id, model_id) REFERENCES models (scope_org_id, id),
  CHECK (effective_to IS NULL OR effective_to > effective_from),
  -- Audit và usage tham chiếu "version N": duy nhất kể cả với đường ghi không qua service.
  UNIQUE NULLS NOT DISTINCT (org_id, model_id, version),
  -- Tại một thời điểm chỉ có một giá hiệu lực cho mỗi cặp (model, phạm vi giá).
  EXCLUDE USING gist (
    model_id WITH =,
    (COALESCE(org_id, '00000000-0000-0000-0000-000000000000'::uuid)) WITH =,
    tstzrange(effective_from, effective_to) WITH &&
  )
);
CREATE INDEX model_pricing_org_model_idx ON model_pricing (org_id, model_id, effective_from DESC);

ALTER TABLE model_pricing ENABLE ROW LEVEL SECURITY;
ALTER TABLE model_pricing FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON model_pricing
  USING      (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid)
  WITH CHECK (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid);
CREATE POLICY platform_read ON model_pricing FOR SELECT USING (org_id IS NULL);

-- Giá đã ghi là bất biến, trừ việc đóng khoảng hiệu lực đúng một lần, và chỉ đóng
-- từ hiện tại trở đi: không rút ngắn được khoảng thời gian đã trôi qua, nên giá
-- hiệu lực tại một thời điểm trong quá khứ không bao giờ đổi.
-- Trigger áp dụng cho mọi role, kể cả owner: xoá cứng model có giá hoặc offboarding
-- một org phải DISABLE TRIGGER trong một migration có chủ đích.
-- +goose StatementBegin
CREATE FUNCTION model_pricing_guard() RETURNS trigger
LANGUAGE plpgsql AS $fn$
BEGIN
  IF TG_OP = 'UPDATE' THEN
    IF OLD.effective_to IS NULL AND NEW.effective_to IS NOT NULL
       AND NEW.effective_to >= now()
       AND (to_jsonb(NEW) - 'effective_to') = (to_jsonb(OLD) - 'effective_to') THEN
      RETURN NEW;
    END IF;
  END IF;
  RAISE EXCEPTION 'model_pricing is immutable (% rejected): chỉ được đóng effective_to của dòng đang mở', TG_OP
    USING ERRCODE = 'restrict_violation';
END
$fn$;
-- +goose StatementEnd
CREATE TRIGGER model_pricing_no_update BEFORE UPDATE OR DELETE ON model_pricing
  FOR EACH ROW EXECUTE FUNCTION model_pricing_guard();
CREATE TRIGGER model_pricing_no_truncate BEFORE TRUNCATE ON model_pricing
  FOR EACH STATEMENT EXECUTE FUNCTION model_pricing_guard();

-- Chỉ cấp cho process đang có code dùng; worker được cấp SELECT khi runtime cần.
GRANT SELECT, INSERT ON model_providers TO app_api;
GRANT UPDATE (enabled, settings, archived_at, updated_at) ON model_providers TO app_api;
GRANT SELECT, INSERT ON models TO app_api;
GRANT UPDATE (display_name, tier, capabilities, enabled, deprecated_at, updated_at) ON models TO app_api;
GRANT SELECT, INSERT ON model_pricing TO app_api;
GRANT UPDATE (effective_to) ON model_pricing TO app_api;
GRANT SELECT, INSERT, UPDATE, DELETE ON model_providers, models TO app_admin;
GRANT SELECT, INSERT ON model_pricing TO app_admin;
GRANT UPDATE (effective_to) ON model_pricing TO app_admin;

-- Permission của Model Catalog cho role dựng sẵn (mẫu NO FORCE + kiểm số dòng).
ALTER TABLE roles NO FORCE ROW LEVEL SECURITY;
-- +goose StatementBegin
DO $$
DECLARE
  n int;
  total int := 0;
  read_all jsonb := '["model_provider:read","model:read","model_pricing:read"]';
  grants jsonb := jsonb_build_object(
    'org_admin',      read_all || '["model_provider:create","model_provider:update","model_provider:delete","model:create","model:update","model_pricing:create"]'::jsonb,
    'project_admin',  read_all,
    'developer',      read_all,
    'operator',       read_all,
    'approver',       read_all,
    'viewer',         read_all,
    'billing_viewer', '["model:read","model_pricing:read"]'::jsonb);
  r record;
BEGIN
  FOR r IN SELECT key, value FROM jsonb_each(grants) LOOP
    UPDATE roles SET permissions = permissions || ARRAY(SELECT jsonb_array_elements_text(r.value)),
                     updated_at = now()
    WHERE org_id IS NULL AND name = r.key;
    GET DIAGNOSTICS n = ROW_COUNT;
    total := total + n;
  END LOOP;
  IF total <> 7 THEN
    RAISE EXCEPTION 'cập nhật role dựng sẵn tác động % dòng, muốn 7', total;
  END IF;
END
$$;
-- +goose StatementEnd
ALTER TABLE roles FORCE ROW LEVEL SECURITY;

-- +goose Down
ALTER TABLE roles NO FORCE ROW LEVEL SECURITY;
UPDATE roles SET permissions = ARRAY(SELECT p FROM unnest(permissions) p WHERE p !~ '^(model_provider|model|model_pricing):')
WHERE org_id IS NULL;
ALTER TABLE roles FORCE ROW LEVEL SECURITY;
DROP TABLE model_pricing;
DROP FUNCTION model_pricing_guard();
DROP TABLE models;
DROP TABLE model_providers;
