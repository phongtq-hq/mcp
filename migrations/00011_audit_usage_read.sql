-- +goose Up
SET LOCAL lock_timeout = '5s';

-- Quyền đọc audit trail và usage ledger qua API. Dữ liệu đã được ghi từ trước
-- (00003, 00008); migration này chỉ thêm permission cho role dựng sẵn.
-- Audit chứa hành vi của mọi thành viên nên chỉ org_admin đọc được; usage (token và
-- chi phí, không có nội dung) rộng hơn.

-- Index cho các bộ lọc của API tra cứu audit (00003 hẹn thêm cùng lúc với API) và
-- của usage. Bảng còn nhỏ nên tạo thẳng trên bảng cha; phân vùng tạo sau tự có nhờ
-- LIKE ... INCLUDING INDEXES. Trên hệ đã có nhiều dữ liệu phải làm theo kiểu
-- ON ONLY + CONCURRENTLY từng phân vùng.
CREATE INDEX audit_logs_resource_idx  ON audit_logs (org_id, resource_type, resource_id, occurred_at DESC);
CREATE INDEX audit_logs_actor_idx     ON audit_logs (org_id, actor_id, occurred_at DESC);
CREATE INDEX audit_logs_execution_idx ON audit_logs (org_id, execution_id, occurred_at DESC) WHERE execution_id IS NOT NULL;
CREATE INDEX usage_records_agent_idx  ON usage_records (org_id, agent_id, occurred_at);
CREATE INDEX usage_records_model_idx  ON usage_records (org_id, model_id, occurred_at);

-- Job bảo trì đếm dòng nằm ở phân vùng mặc định của audit (dấu hiệu từng thiếu phân vùng tháng).
GRANT SELECT ON audit_logs_default TO app_admin;

ALTER TABLE roles NO FORCE ROW LEVEL SECURITY;
-- +goose StatementBegin
DO $$
DECLARE
  n int;
  total int := 0;
  r record;
  grants jsonb := '{
    "org_admin":     ["audit_log:read","usage:read"],
    "project_admin": ["usage:read"],
    "operator":      ["usage:read"],
    "developer":     ["usage:read"]
  }';
BEGIN
  FOR r IN SELECT key, value FROM jsonb_each(grants) LOOP
    UPDATE roles SET permissions = permissions || ARRAY(SELECT jsonb_array_elements_text(r.value)), updated_at = now()
    WHERE org_id IS NULL AND name = r.key;
    GET DIAGNOSTICS n = ROW_COUNT;
    total := total + n;
  END LOOP;
  IF total <> 4 THEN
    RAISE EXCEPTION 'cập nhật role dựng sẵn tác động % dòng, muốn 4', total;
  END IF;
END
$$;
-- +goose StatementEnd
ALTER TABLE roles FORCE ROW LEVEL SECURITY;

-- +goose Down
SET LOCAL lock_timeout = '5s';
REVOKE SELECT ON audit_logs_default FROM app_admin;
DROP INDEX usage_records_model_idx;
DROP INDEX usage_records_agent_idx;
DROP INDEX audit_logs_execution_idx;
DROP INDEX audit_logs_actor_idx;
DROP INDEX audit_logs_resource_idx;
ALTER TABLE roles NO FORCE ROW LEVEL SECURITY;
UPDATE roles SET permissions = ARRAY(SELECT p FROM unnest(permissions) p WHERE p NOT IN ('audit_log:read','usage:read')) WHERE org_id IS NULL;
ALTER TABLE roles FORCE ROW LEVEL SECURITY;
