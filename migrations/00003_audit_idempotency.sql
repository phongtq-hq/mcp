-- +goose Up
SET LOCAL lock_timeout = '5s';
SET LOCAL timezone = 'UTC';

-- Audit trail: chỉ thêm. Mỗi bản ghi chứa hash của bản ghi trước trong cùng org,
-- nên sửa hoặc xoá một dòng ở giữa làm đứt chuỗi và bị audit_verify phát hiện.
--
-- Role ứng dụng KHÔNG có quyền ghi trực tiếp: mọi bản ghi đi qua audit_append,
-- hàm tự lấy thời điểm của database, tự tính hash và tự tuần tự hoá theo org.
-- Vì vậy một process bị chiếm không đặt được thời điểm, hash hay id tuỳ ý.
-- Giới hạn còn lại: (1) process ghi audit vẫn có thể thêm một sự kiện sai sự thật
-- (nó là bên khai báo actor); (2) ai xoá được dòng thì cắt được đuôi chuỗi mà
-- không bị phát hiện. Cả hai chỉ được giải quyết bằng việc neo chuỗi ra kho ngoài
-- (Phase 6).
CREATE TABLE audit_logs (
  id            UUID NOT NULL,
  org_id        UUID NOT NULL,           -- sự kiện cấp platform chưa có nơi dùng; thêm khi cần, kèm policy riêng
  project_id    UUID,
  actor_type    TEXT NOT NULL CHECK (actor_type IN ('user','api_key','agent','system')),
  actor_id      UUID,
  on_behalf_of  UUID,                    -- người khởi tạo, khi actor là agent
  action        TEXT NOT NULL,           -- 'project.create', 'authz.deny'...
  resource_type TEXT NOT NULL,
  resource_id   UUID,
  outcome       TEXT NOT NULL CHECK (outcome IN ('success','denied','error')),
  execution_id  UUID,
  details       JSONB NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(details) = 'object'),
  ip            INET,
  trace_id      TEXT,
  prev_hash     BYTEA,
  hash          BYTEA NOT NULL,
  occurred_at   TIMESTAMPTZ NOT NULL,    -- tăng nghiêm ngặt trong một org
  PRIMARY KEY (id, occurred_at)
) PARTITION BY RANGE (occurred_at);
-- Index tra cứu theo tài nguyên và theo actor được thêm cùng API tra cứu audit.
CREATE INDEX audit_logs_org_time_idx ON audit_logs (org_id, occurred_at DESC);

ALTER TABLE audit_logs ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_logs FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON audit_logs
  USING      (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid)
  WITH CHECK (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid);

CREATE TABLE audit_logs_default PARTITION OF audit_logs DEFAULT;
ALTER TABLE audit_logs_default ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_logs_default FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON audit_logs_default
  USING      (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid)
  WITH CHECK (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid);

-- Tạo phân vùng cho một tháng (UTC). Chỉ tài khoản quản trị gọi (migration, job).
-- Bảng được tạo rời rồi ATTACH để không giữ AccessExclusiveLock trên bảng cha;
-- dòng của tháng đó đang nằm ở phân vùng mặc định (job chạy chậm) được chuyển sang
-- trước khi attach, vì nếu không ATTACH sẽ lỗi vĩnh viễn cho tháng ấy.
-- +goose StatementBegin
CREATE FUNCTION audit_logs_create_partition(month_start date) RETURNS void
LANGUAGE plpgsql
SET search_path = pg_catalog, public
SET timezone = 'UTC'
AS $fn$
DECLARE
  part    text := 'audit_logs_' || to_char(month_start, 'YYYY_MM');
  lo      timestamptz := month_start::timestamptz;
  hi      timestamptz := (month_start + interval '1 month')::timestamptz;
BEGIN
  IF month_start <> date_trunc('month', month_start)::date THEN
    RAISE EXCEPTION 'month_start phải là ngày đầu tháng: %', month_start;
  END IF;
  IF EXISTS (SELECT FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
             WHERE i.inhparent = 'public.audit_logs'::regclass AND c.relname = part) THEN
    RETURN;
  END IF;
  EXECUTE format('CREATE TABLE public.%I (LIKE public.audit_logs INCLUDING DEFAULTS INCLUDING CONSTRAINTS INCLUDING INDEXES)', part);
  -- CHECK khớp biên cho phép ATTACH bỏ qua việc quét bảng.
  EXECUTE format('ALTER TABLE public.%I ADD CONSTRAINT partition_bound CHECK (occurred_at >= %L AND occurred_at < %L)', part, lo, hi);
  EXECUTE format('ALTER TABLE public.%I ENABLE ROW LEVEL SECURITY', part);
  EXECUTE format('ALTER TABLE public.%I FORCE ROW LEVEL SECURITY', part);
  EXECUTE format($p$CREATE POLICY tenant_isolation ON public.%I
    USING      (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid)
    WITH CHECK (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid)$p$, part);
  EXECUTE format($m$WITH moved AS (
      DELETE FROM public.audit_logs_default WHERE occurred_at >= %L AND occurred_at < %L RETURNING *)
    INSERT INTO public.%I SELECT * FROM moved$m$, lo, hi, part);
  EXECUTE format('ALTER TABLE public.audit_logs ATTACH PARTITION public.%I FOR VALUES FROM (%L) TO (%L)', part, lo, hi);
  EXECUTE format('ALTER TABLE public.%I DROP CONSTRAINT partition_bound', part);
END
$fn$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION audit_logs_create_partition(date) FROM PUBLIC;

-- Tạo sẵn 24 tháng kể từ tháng hiện tại. Một job định kỳ (chưa có) phải gọi
-- audit_logs_create_partition cho các tháng sau đó và cảnh báo khi phân vùng
-- mặc định có dòng.
-- +goose StatementBegin
DO $$
DECLARE
  m date := date_trunc('month', now() AT TIME ZONE 'UTC')::date;
BEGIN
  FOR i IN 0..23 LOOP
    PERFORM audit_logs_create_partition((m + (i || ' months')::interval)::date);
  END LOOP;
END
$$;
-- +goose StatementEnd

-- Dạng chuẩn hoá của một bản ghi, đầu vào của hàm hash. Thứ tự phần tử là một phần
-- của định dạng (phiên bản 1): KHÔNG đổi mà không tăng số phiên bản, vì mọi chuỗi
-- đã ghi sẽ không còn kiểm tra được.
-- +goose StatementBegin
CREATE FUNCTION audit_canonical(r audit_logs) RETURNS bytea
LANGUAGE sql IMMUTABLE
SET search_path = pg_catalog, public
AS $fn$
  SELECT convert_to(jsonb_build_array(
    1, r.id, r.org_id, r.project_id, r.actor_type, r.actor_id, r.on_behalf_of, r.action,
    r.resource_type, r.resource_id, r.outcome, r.execution_id, r.details, host(r.ip), r.trace_id,
    to_char(r.occurred_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"')
  )::text, 'UTF8')
$fn$;
-- +goose StatementEnd

-- Đường ghi duy nhất vào audit_logs. SECURITY DEFINER: chạy bằng quyền của owner,
-- nên role ứng dụng không cần (và không có) INSERT trên bảng. Người gọi phải đang ở
-- trong transaction đã đặt đúng tenant.
-- +goose StatementBegin
CREATE FUNCTION audit_append(
  p_org_id uuid, p_project_id uuid, p_actor_type text, p_actor_id uuid, p_on_behalf_of uuid,
  p_action text, p_resource_type text, p_resource_id uuid, p_outcome text, p_execution_id uuid,
  p_details jsonb, p_ip inet, p_trace_id text
) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, public
AS $fn$
DECLARE
  r    public.audit_logs;
  prev record;
BEGIN
  IF p_org_id IS NULL OR p_org_id IS DISTINCT FROM NULLIF(current_setting('app.org_id', true), '')::uuid THEN
    RAISE EXCEPTION 'audit_append: org_id không khớp tenant của transaction'
      USING ERRCODE = 'insufficient_privilege';
  END IF;
  -- Tuần tự hoá việc ghi trong một org tới hết transaction.
  PERFORM pg_advisory_xact_lock(hashtextextended('audit:' || p_org_id::text, 0));

  SELECT hash, occurred_at INTO prev FROM public.audit_logs
  WHERE org_id = p_org_id ORDER BY occurred_at DESC LIMIT 1;

  r.id := uuidv7();
  r.org_id := p_org_id;           r.project_id := p_project_id;
  r.actor_type := p_actor_type;   r.actor_id := p_actor_id;       r.on_behalf_of := p_on_behalf_of;
  r.action := p_action;           r.resource_type := p_resource_type;  r.resource_id := p_resource_id;
  r.outcome := p_outcome;         r.execution_id := p_execution_id;
  r.details := COALESCE(p_details, '{}'::jsonb);
  r.ip := p_ip;                   r.trace_id := p_trace_id;
  r.prev_hash := prev.hash;
  -- Thời điểm là đồng hồ của database, tăng nghiêm ngặt trong org.
  r.occurred_at := GREATEST(clock_timestamp(), prev.occurred_at + interval '1 microsecond');
  r.hash := sha256(COALESCE(prev.hash, ''::bytea) || public.audit_canonical(r));

  INSERT INTO public.audit_logs VALUES (r.*);
  RETURN r.id;
END
$fn$;
-- +goose StatementEnd

-- Tính lại toàn bộ chuỗi của một org; trả về số bản ghi đã kiểm. Chạy bằng quyền
-- người gọi (chịu RLS). Khoá ghi của org được lấy trước khi đọc, nên không có báo
-- động giả do bản ghi đang được thêm; duyệt bằng con trỏ nên không nạp cả chuỗi
-- vào bộ nhớ. Chuỗi đứt báo lỗi SQLSTATE 'HQA01'.
-- +goose StatementBegin
CREATE FUNCTION audit_verify(p_org_id uuid) RETURNS bigint
LANGUAGE plpgsql
SET search_path = pg_catalog, public
AS $fn$
DECLARE
  r    public.audit_logs;
  prev bytea;
  n    bigint := 0;
BEGIN
  PERFORM pg_advisory_xact_lock(hashtextextended('audit:' || p_org_id::text, 0));
  FOR r IN SELECT * FROM public.audit_logs WHERE org_id = p_org_id ORDER BY occurred_at LOOP
    IF r.prev_hash IS DISTINCT FROM prev
       OR r.hash IS DISTINCT FROM sha256(COALESCE(prev, ''::bytea) || public.audit_canonical(r)) THEN
      RAISE EXCEPTION 'audit hash chain broken at record % (position %)', r.id, n
        USING ERRCODE = 'HQA01';
    END IF;
    prev := r.hash;
    n := n + 1;
  END LOOP;
  RETURN n;
END
$fn$;
-- +goose StatementEnd

REVOKE ALL ON FUNCTION audit_append(uuid, uuid, text, uuid, uuid, text, text, uuid, text, uuid, jsonb, inet, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION audit_verify(uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION audit_canonical(audit_logs) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION audit_append(uuid, uuid, text, uuid, uuid, text, text, uuid, text, uuid, jsonb, inet, text) TO app_api, app_admin;
GRANT EXECUTE ON FUNCTION audit_verify(uuid), audit_canonical(audit_logs) TO app_api, app_admin;
-- Chỉ SELECT, và chỉ trên bảng cha: phân vùng không truy cập trực tiếp được.
GRANT SELECT ON audit_logs TO app_api, app_admin;

-- Idempotency-Key của Management API. Key thuộc về một principal trong một org:
-- người khác dùng cùng chuỗi key là một request độc lập.
-- Lưu ý: endpoint trả secret một lần (tạo API key) KHÔNG được bọc bằng cơ chế này,
-- vì phản hồi được lưu ở đây.
CREATE TABLE idempotency_keys (
  org_id           UUID NOT NULL REFERENCES organizations(id),
  principal_id     UUID NOT NULL,
  key              TEXT NOT NULL,
  request_hash     BYTEA NOT NULL,          -- cùng key, khác request → 422
  status           TEXT NOT NULL CHECK (status IN ('in_progress','completed')),
  response_code    INT,
  response_headers JSONB,
  response_body    BYTEA,
  created_at       TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),  -- cũng là định danh của lần nhận key
  expires_at       TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (org_id, principal_id, key),
  CHECK (status = 'in_progress' OR response_code IS NOT NULL)
);
-- Cho việc dọn key hết hạn, kể cả job xuyên tenant: ngoại lệ có chủ đích với quy
-- tắc "index bắt đầu bằng org_id".
CREATE INDEX idempotency_keys_expires_idx ON idempotency_keys (expires_at);

ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON idempotency_keys
  USING      (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid)
  WITH CHECK (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON idempotency_keys TO app_api;
GRANT SELECT, DELETE ON idempotency_keys TO app_admin;

-- +goose Down
DROP TABLE idempotency_keys;
DROP FUNCTION audit_verify(uuid);
DROP FUNCTION audit_append(uuid, uuid, text, uuid, uuid, text, text, uuid, text, uuid, jsonb, inet, text);
DROP FUNCTION audit_canonical(audit_logs);
DROP FUNCTION audit_logs_create_partition(date);
DROP TABLE audit_logs;
