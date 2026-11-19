-- +goose Up
-- Phase 2: Budget (cost-management §4). Một budget = phạm vi + chu kỳ + hạn mức + hành
-- vi khi vượt. Bảng này là cấu hình; mức đã tiêu lấy từ usage_records (nguồn sự thật),
-- bộ đếm nhanh nằm ở Redis và dựng lại được từ ledger.
--
-- Budget theo execution / task không nằm ở đây: nó là limits.budget của Agent Version,
-- được chụp vào agent_executions.budget_limit và agent_tasks.budget_limit lúc tạo.
SET LOCAL lock_timeout = '5s';

CREATE TABLE budgets (
  id                 UUID PRIMARY KEY DEFAULT uuidv7(),
  org_id             UUID NOT NULL REFERENCES organizations(id),
  scope_type         TEXT NOT NULL CHECK (scope_type IN ('organization','project','user','agent')),
  -- Tham chiếu đa hình (organization, project, user, agent): không có khoá ngoại; ứng
  -- dụng kiểm phạm vi tồn tại trong org khi tạo. scope organization: scope_id = org_id.
  scope_id           UUID NOT NULL,
  period             TEXT NOT NULL CHECK (period IN ('daily','monthly','total')),
  limit_amount       NUMERIC(18,8) NOT NULL CHECK (limit_amount >= 0 AND limit_amount <> 'NaN'),
  currency           CHAR(3) NOT NULL DEFAULT 'USD' CHECK (currency ~ '^[A-Z]{3}$'),
  soft_threshold_pct INT CHECK (soft_threshold_pct BETWEEN 1 AND 100),
  hard_action        TEXT NOT NULL DEFAULT 'block' CHECK (hard_action IN ('block','alert_only')),
  enabled            BOOLEAN NOT NULL DEFAULT true,
  created_by         UUID REFERENCES users(id),
  created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (org_id, scope_type, scope_id, period),
  CHECK (scope_type <> 'organization' OR scope_id = org_id)
);
ALTER TABLE budgets ENABLE ROW LEVEL SECURITY;
ALTER TABLE budgets FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON budgets
  USING      (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid)
  WITH CHECK (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid);

-- api quản lý budget; gateway (và worker, cho adapter chạy trong process ở local) chỉ
-- đọc để thực thi.
GRANT SELECT, INSERT, DELETE ON budgets TO app_api;
GRANT UPDATE (limit_amount, soft_threshold_pct, hard_action, enabled, updated_at) ON budgets TO app_api;
GRANT SELECT ON budgets TO app_gateway, app_worker, app_admin;

-- Thực thi budget cần: mức đã tiêu theo phạm vi (dựng lại bộ đếm từ ledger), giá của
-- model (ước lượng trần chi phí một lượt), và budget của task.
GRANT SELECT (org_id, project_id, user_id, agent_id, occurred_at, estimated_cost, currency) ON usage_records TO app_gateway, app_worker;
GRANT SELECT ON model_pricing TO app_gateway;
GRANT SELECT (budget_limit, estimated_cost) ON agent_tasks TO app_gateway;

-- Truy vấn "mức đã tiêu của project / user trong kỳ" (agent đã có index ở 00011).
-- Bảng còn nhỏ nên tạo thẳng trên bảng cha; phân vùng tạo sau tự có nhờ LIKE ...
-- INCLUDING INDEXES. Trên hệ đã có nhiều dữ liệu phải làm theo kiểu ON ONLY +
-- CONCURRENTLY từng phân vùng.
CREATE INDEX usage_records_project_idx ON usage_records (org_id, project_id, occurred_at);
CREATE INDEX usage_records_user_idx    ON usage_records (org_id, user_id, occurred_at) WHERE user_id IS NOT NULL;

-- Permission: đọc budget rộng (ai xem được chi phí thì xem được hạn mức); đặt budget
-- là việc của quản trị organization.
ALTER TABLE roles NO FORCE ROW LEVEL SECURITY;
-- +goose StatementBegin
DO $$
DECLARE
  n int;
  total int := 0;
  r record;
  grants jsonb := '{
    "org_admin":      ["budget:read","budget:write"],
    "project_admin":  ["budget:read"],
    "operator":       ["budget:read"],
    "developer":      ["budget:read"],
    "billing_viewer": ["budget:read"]
  }';
BEGIN
  FOR r IN SELECT key, value FROM jsonb_each(grants) LOOP
    UPDATE roles SET permissions = permissions || ARRAY(SELECT jsonb_array_elements_text(r.value)), updated_at = now()
    WHERE org_id IS NULL AND name = r.key;
    GET DIAGNOSTICS n = ROW_COUNT;
    total := total + n;
  END LOOP;
  IF total <> 5 THEN
    RAISE EXCEPTION 'cập nhật role dựng sẵn tác động % dòng, muốn 5', total;
  END IF;
END
$$;
-- +goose StatementEnd
ALTER TABLE roles FORCE ROW LEVEL SECURITY;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE roles NO FORCE ROW LEVEL SECURITY;
UPDATE roles SET permissions = array_remove(array_remove(permissions, 'budget:read'), 'budget:write'), updated_at = now()
WHERE org_id IS NULL;
ALTER TABLE roles FORCE ROW LEVEL SECURITY;
DROP INDEX usage_records_user_idx;
DROP INDEX usage_records_project_idx;
REVOKE SELECT (budget_limit, estimated_cost) ON agent_tasks FROM app_gateway;
REVOKE SELECT ON model_pricing FROM app_gateway;
REVOKE SELECT (org_id, project_id, user_id, agent_id, occurred_at, estimated_cost, currency) ON usage_records FROM app_gateway;
-- app_worker đã có SELECT (estimated_cost, currency) từ 00013 (RETURNING của lệnh ghi usage): không thu hồi hai cột đó.
REVOKE SELECT (org_id, project_id, user_id, agent_id, occurred_at) ON usage_records FROM app_worker;
DROP TABLE budgets;
