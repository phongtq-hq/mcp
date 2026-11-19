-- +goose Up
-- Phase 3: thông báo cho người duyệt (security §4.5, §4.7; roadmap Phase 3 "thông báo
-- (webhook)").
--
-- notification_webhooks: mỗi organization tối đa một webhook nhận sự kiện của platform.
-- Khoá ký HMAC là một secret (envelope encryption): api chỉ mã hoá được, gateway là nơi
-- duy nhất mở và gửi.
--
-- notification_deliveries: outbox. Dòng được chèn bởi trigger của `approvals`, trong
-- CÙNG transaction tạo approval, bất kể approval do Gateway, worker hay hàm database
-- tạo: không có đường tạo approval nào quên báo. Dòng chỉ mang định danh; nội dung của
-- sự kiện được dựng lúc gửi từ dữ liệu hiện tại.
SET LOCAL lock_timeout = '5s';

CREATE TABLE notification_webhooks (
  org_id               UUID PRIMARY KEY REFERENCES organizations(id),
  url                  TEXT NOT NULL CHECK (length(url) BETWEEN 9 AND 2048),
  secret_id            UUID NOT NULL,
  enabled              BOOLEAN NOT NULL DEFAULT true,
  -- Số lần gửi hỏng hẳn liên tiếp; chạm ngưỡng thì gateway tự tắt webhook.
  consecutive_failures INT NOT NULL DEFAULT 0 CHECK (consecutive_failures >= 0),
  disabled_reason      TEXT CHECK (disabled_reason IN ('delivery_failures')),
  created_by           UUID REFERENCES users(id),
  created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
  FOREIGN KEY (org_id, secret_id) REFERENCES secrets (org_id, id)
);
ALTER TABLE notification_webhooks ENABLE ROW LEVEL SECURITY;
ALTER TABLE notification_webhooks FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON notification_webhooks
  USING      (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid)
  WITH CHECK (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid);

CREATE TABLE notification_deliveries (
  id              UUID PRIMARY KEY DEFAULT uuidv7(),
  org_id          UUID NOT NULL REFERENCES organizations(id),
  event_type      TEXT NOT NULL CHECK (event_type IN ('approval.requested')),
  approval_id     UUID NOT NULL,
  status          TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','delivered','failed','dropped')),
  attempts        INT  NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 6),
  next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  -- Kết quả lần thử gần nhất: mã HTTP (nếu có phản hồi) và một mã lỗi cố định của
  -- platform. Không lưu thân phản hồi hay thông báo lỗi của bên nhận.
  last_http_status INT CHECK (last_http_status BETWEEN 100 AND 599),
  last_error_code  TEXT CHECK (last_error_code IN ('blocked','timeout','connect','http_status','secret_unavailable','not_configured','approval_closed','attempts_exhausted')),
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  finished_at     TIMESTAMPTZ,
  UNIQUE (org_id, id),
  -- Mỗi approval một thông báo cho mỗi loại sự kiện.
  UNIQUE (org_id, approval_id, event_type),
  FOREIGN KEY (org_id, approval_id) REFERENCES approvals (org_id, id) ON DELETE CASCADE,
  CHECK ((status = 'pending') = (finished_at IS NULL))
);
-- Hàng chờ theo org: phục vụ trần của trigger, và hàm notification_claim (nó duyệt từng
-- org có dòng tới hạn rồi lấy vài dòng cũ nhất của org đó).
CREATE INDEX notification_deliveries_pending_idx ON notification_deliveries (org_id, next_attempt_at, id) WHERE status = 'pending';
CREATE INDEX notification_deliveries_list_idx ON notification_deliveries (org_id, created_at DESC, id DESC);

ALTER TABLE notification_deliveries ENABLE ROW LEVEL SECURITY;
ALTER TABLE notification_deliveries FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON notification_deliveries
  USING      (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid)
  WITH CHECK (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid);

-- Vòng đời của một lần gửi, với mọi role: pending → delivered | failed | dropped, không
-- quay lại; định danh không đổi; số lần thử chỉ tăng.
-- +goose StatementBegin
CREATE FUNCTION notification_deliveries_guard() RETURNS trigger
LANGUAGE plpgsql AS $fn$
BEGIN
  IF NEW.id <> OLD.id OR NEW.org_id <> OLD.org_id OR NEW.event_type <> OLD.event_type
     OR NEW.approval_id <> OLD.approval_id OR NEW.created_at <> OLD.created_at THEN
    RAISE EXCEPTION 'notification_deliveries: định danh của % không đổi được', OLD.id USING ERRCODE = 'restrict_violation';
  END IF;
  IF OLD.status <> 'pending' THEN
    RAISE EXCEPTION 'notification_deliveries: % đã kết thúc (%)', OLD.id, OLD.status USING ERRCODE = 'restrict_violation';
  END IF;
  IF NEW.attempts < OLD.attempts THEN
    RAISE EXCEPTION 'notification_deliveries: số lần thử của % không giảm được', OLD.id USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END
$fn$;
-- +goose StatementEnd
CREATE TRIGGER notification_deliveries_lifecycle BEFORE UPDATE ON notification_deliveries
  FOR EACH ROW EXECUTE FUNCTION notification_deliveries_guard();

-- Dòng mới luôn bắt đầu ở pending, chưa thử lần nào.
-- +goose StatementBegin
CREATE FUNCTION notification_deliveries_insert_guard() RETURNS trigger
LANGUAGE plpgsql AS $fn$
BEGIN
  IF NEW.status <> 'pending' OR NEW.attempts <> 0 OR NEW.finished_at IS NOT NULL THEN
    RAISE EXCEPTION 'notification_deliveries: dòng mới phải ở pending' USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END
$fn$;
-- +goose StatementEnd
CREATE TRIGGER notification_deliveries_insert BEFORE INSERT ON notification_deliveries
  FOR EACH ROW EXECUTE FUNCTION notification_deliveries_insert_guard();

-- Xếp một thông báo khi approval được tạo. SECURITY DEFINER: role tạo approval
-- (app_gateway, hoặc owner của plan_step_request_approval) không có INSERT trên outbox.
-- Trần 500 thông báo đang chờ mỗi org: outbox không lớn vô hạn khi bên nhận chết; quá
-- trần thì approval vẫn được tạo, chỉ không có thông báo.
-- +goose StatementBegin
CREATE FUNCTION approvals_enqueue_notification() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $fn$
BEGIN
  IF NEW.status = 'pending'
     AND EXISTS (SELECT 1 FROM notification_webhooks w WHERE w.org_id = NEW.org_id AND w.enabled)
     AND (SELECT count(*) FROM notification_deliveries d WHERE d.org_id = NEW.org_id AND d.status = 'pending') < 500 THEN
    INSERT INTO notification_deliveries (org_id, event_type, approval_id)
    VALUES (NEW.org_id, 'approval.requested', NEW.id)
    ON CONFLICT DO NOTHING;
  END IF;
  RETURN NULL;
END
$fn$;
-- +goose StatementEnd
CREATE TRIGGER approvals_notify AFTER INSERT ON approvals
  FOR EACH ROW EXECUTE FUNCTION approvals_enqueue_notification();

-- Nhận một lô thông báo tới hạn, xuyên org. Đây là đường đọc xuyên tenant duy nhất của
-- outbox; chỉ gateway gọi được. Mỗi dòng được "thuê" bằng cách đẩy next_attempt_at ra
-- sau (30 giây × 2^số lần đã thử, tối đa 15 phút), nên process chết giữa chừng thì
-- dòng tự quay lại; dòng đã thử đủ 6 lần được đóng `failed` ngay tại đây.
--
-- Công bằng giữa các org: ứng viên được chọn THEO TỪNG ORG trước (tối đa 4 dòng cũ nhất
-- của mỗi org có dòng tới hạn), rồi mới cắt theo p_limit. Một org có hàng trăm dòng chờ
-- chỉ chiếm 4 chỗ của lô; phần còn lại là của các org khác.
-- +goose StatementBegin
CREATE FUNCTION notification_claim(p_limit int) RETURNS SETOF notification_deliveries
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $fn$
BEGIN
  IF p_limit IS NULL OR p_limit < 1 OR p_limit > 64 THEN
    RAISE EXCEPTION 'notification_claim: p_limit phải trong 1..64' USING ERRCODE = 'invalid_parameter_value';
  END IF;
  -- SKIP LOCKED ở cả hai câu lệnh: hai replica không chờ nhau và không deadlock.
  UPDATE notification_deliveries d
  SET status = 'failed', finished_at = now(), last_error_code = 'attempts_exhausted'
  WHERE d.id IN (SELECT x.id FROM notification_deliveries x
                 WHERE x.status = 'pending' AND x.attempts >= 6 AND x.next_attempt_at <= now()
                 ORDER BY x.id LIMIT 256 FOR UPDATE SKIP LOCKED);

  RETURN QUERY
  WITH orgs AS (
    SELECT DISTINCT x.org_id FROM notification_deliveries x
    WHERE x.status = 'pending' AND x.next_attempt_at <= now()
  ), cand AS (
    SELECT c.id FROM orgs o CROSS JOIN LATERAL (
      SELECT x.id FROM notification_deliveries x
      WHERE x.org_id = o.org_id AND x.status = 'pending' AND x.next_attempt_at <= now()
      ORDER BY x.next_attempt_at, x.id LIMIT 4) c
  ), picked AS (
    SELECT x.id FROM notification_deliveries x
    WHERE x.id IN (SELECT cand.id FROM cand) AND x.status = 'pending' AND x.next_attempt_at <= now()
    ORDER BY x.next_attempt_at, x.id
    LIMIT p_limit
    FOR UPDATE SKIP LOCKED
  )
  UPDATE notification_deliveries d
  SET attempts = d.attempts + 1,
      next_attempt_at = now() + least(interval '30 seconds' * power(2, d.attempts), interval '15 minutes')
  FROM picked WHERE d.id = picked.id
  RETURNING d.*;
END
$fn$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION notification_claim(int) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION notification_claim(int) TO app_gateway;
REVOKE ALL ON FUNCTION approvals_enqueue_notification() FROM PUBLIC;

-- api: cấu hình webhook, đọc lịch sử gửi. gateway: đọc cấu hình, ghi kết quả gửi và tự
-- tắt webhook hỏng. worker: không có quyền gì.
GRANT SELECT, INSERT, DELETE ON notification_webhooks TO app_api;
GRANT UPDATE (url, secret_id, enabled, consecutive_failures, disabled_reason, created_by, updated_at) ON notification_webhooks TO app_api;
GRANT SELECT ON notification_webhooks TO app_gateway;
GRANT UPDATE (enabled, consecutive_failures, disabled_reason, updated_at) ON notification_webhooks TO app_gateway;
GRANT SELECT ON notification_deliveries TO app_api;
GRANT SELECT ON notification_deliveries TO app_gateway;
GRANT UPDATE (status, last_http_status, last_error_code, finished_at) ON notification_deliveries TO app_gateway;
GRANT SELECT, INSERT, UPDATE, DELETE ON notification_webhooks, notification_deliveries TO app_admin;

-- Khoá ký webhook là một loại secret mới. DROP / ADD CONSTRAINT lấy ACCESS EXCLUSIVE
-- trên secrets tới hết transaction, nên đặt gần cuối để giữ ngắn nhất; bảng secrets nhỏ
-- nên việc kiểm lại các dòng cũ là không đáng kể.
ALTER TABLE secrets DROP CONSTRAINT secrets_kind_check;
ALTER TABLE secrets ADD CONSTRAINT secrets_kind_check
  CHECK (kind IN ('api_key','oauth_client','ssh_key','kubeconfig','oauth_token','webhook_signing_key'));

-- notification_claim chạy không có tenant trên bảng FORCE RLS: nó chỉ thấy dòng khi
-- owner của hàm (tài khoản chạy migration) bỏ qua được RLS, như 00001 yêu cầu. Kiểm ở
-- đây để một tài khoản sai không tạo ra một outbox im lặng không bao giờ được gửi.
-- +goose StatementBegin
DO $$
BEGIN
  IF NOT (SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user) THEN
    RAISE EXCEPTION 'migration 00035: tài khoản % phải là superuser hoặc có BYPASSRLS (notification_claim đọc xuyên tenant)', current_user;
  END IF;
END
$$;
-- +goose StatementEnd

-- Quyền mới: chỉ org_admin. Webhook nhận định danh của mọi approval trong org.
ALTER TABLE roles NO FORCE ROW LEVEL SECURITY;
-- +goose StatementBegin
DO $$
DECLARE
  n int;
BEGIN
  UPDATE roles SET permissions = permissions || ARRAY['notification_webhook:read','notification_webhook:write'], updated_at = now()
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
UPDATE roles SET permissions = array_remove(array_remove(permissions, 'notification_webhook:read'), 'notification_webhook:write'), updated_at = now()
WHERE org_id IS NULL;
ALTER TABLE roles FORCE ROW LEVEL SECURITY;
DROP TRIGGER approvals_notify ON approvals;
DROP FUNCTION notification_claim(int);
DROP FUNCTION approvals_enqueue_notification();
DROP TABLE notification_deliveries;
DROP FUNCTION notification_deliveries_guard();
DROP FUNCTION notification_deliveries_insert_guard();
DROP TABLE notification_webhooks;
-- Secret loại webhook_signing_key không còn nơi dùng; xoá để CHECK cũ áp lại được.
DELETE FROM secrets WHERE kind = 'webhook_signing_key';
ALTER TABLE secrets DROP CONSTRAINT secrets_kind_check;
ALTER TABLE secrets ADD CONSTRAINT secrets_kind_check
  CHECK (kind IN ('api_key','oauth_client','ssh_key','kubeconfig','oauth_token'));
