-- +goose Up
SET LOCAL lock_timeout = '5s';

-- Thực thi (schema §4.4): execution → task → step, và usage ledger (§4.8).
-- Phase 1: vòng lặp chạy trong process api, nên chỉ app_api được ghi. Chưa có
-- agent_sessions và workflow: cột session_id / workflow_execution_id sẽ được thêm
-- cùng với các bảng đó.

CREATE TABLE agent_executions (
  id                  UUID PRIMARY KEY DEFAULT uuidv7(),
  org_id              UUID NOT NULL REFERENCES organizations(id),
  project_id          UUID NOT NULL,
  initiator_type      TEXT NOT NULL CHECK (initiator_type IN ('user','api_key','schedule','workflow')),
  initiator_id        UUID,
  mode                TEXT NOT NULL CHECK (mode IN ('direct_tool','single_agent','planned')),
  status              TEXT NOT NULL DEFAULT 'queued' CHECK (status IN
    ('queued','running','waiting_approval','paused','completed','failed','cancelled','budget_exceeded','timed_out')),
  input               JSONB NOT NULL CHECK (jsonb_typeof(input) = 'object'),
  output              JSONB,
  error               JSONB,
  budget_limit        NUMERIC(18,8) CHECK (budget_limit >= 0 AND budget_limit <> 'NaN'),
  estimated_cost      NUMERIC(18,8) NOT NULL DEFAULT 0 CHECK (estimated_cost >= 0 AND estimated_cost <> 'NaN'),
  currency            CHAR(3) NOT NULL DEFAULT 'USD' CHECK (currency ~ '^[A-Z]{3}$'),
  trace_id            TEXT,
  -- Yêu cầu huỷ: vòng lặp đọc cột này trước mỗi bước. Trạng thái chỉ chuyển sang
  -- cancelled khi vòng lặp thực sự dừng.
  cancel_requested_at TIMESTAMPTZ,
  -- Hạn của execution. Phase 1 không bền vững qua restart: execution còn "running"
  -- quá hạn này là execution mồ côi và được đóng lại khi có người đọc nó.
  deadline_at         TIMESTAMPTZ NOT NULL,
  started_at          TIMESTAMPTZ,
  finished_at         TIMESTAMPTZ,
  created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
  FOREIGN KEY (org_id, project_id) REFERENCES projects (org_id, id),
  UNIQUE (org_id, id)
);
CREATE INDEX agent_executions_list_idx   ON agent_executions (org_id, project_id, created_at DESC, id DESC);

CREATE TABLE agent_tasks (
  id               UUID PRIMARY KEY DEFAULT uuidv7(),
  org_id           UUID NOT NULL REFERENCES organizations(id),
  execution_id     UUID NOT NULL,
  parent_task_id   UUID,
  agent_id         UUID NOT NULL,
  agent_version_id UUID NOT NULL,
  plan_key         TEXT,
  depends_on       UUID[] NOT NULL DEFAULT '{}',
  depth            INT  NOT NULL DEFAULT 0 CHECK (depth >= 0),
  goal             TEXT NOT NULL,
  status           TEXT NOT NULL DEFAULT 'queued' CHECK (status IN
    ('queued','running','waiting_approval','paused','completed','failed','cancelled','budget_exceeded','timed_out','skipped')),
  output_summary   TEXT,
  output           JSONB,
  error            JSONB,
  budget_limit     NUMERIC(18,8) CHECK (budget_limit >= 0 AND budget_limit <> 'NaN'),
  estimated_cost   NUMERIC(18,8) NOT NULL DEFAULT 0 CHECK (estimated_cost >= 0 AND estimated_cost <> 'NaN'),
  iterations       INT NOT NULL DEFAULT 0 CHECK (iterations >= 0),
  tool_calls       INT NOT NULL DEFAULT 0 CHECK (tool_calls >= 0),
  started_at       TIMESTAMPTZ,
  finished_at      TIMESTAMPTZ,
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  FOREIGN KEY (org_id, execution_id) REFERENCES agent_executions (org_id, id) ON DELETE CASCADE,
  FOREIGN KEY (org_id, agent_id) REFERENCES agents (org_id, id),
  FOREIGN KEY (agent_id, agent_version_id) REFERENCES agent_versions (agent_id, id),
  UNIQUE (org_id, id),
  FOREIGN KEY (org_id, parent_task_id) REFERENCES agent_tasks (org_id, id)
);
CREATE INDEX agent_tasks_execution_idx ON agent_tasks (org_id, execution_id, created_at, id);

-- Nhật ký step. Phân vùng theo tháng nên không có khoá ngoại; tính toàn vẹn do ứng
-- dụng bảo đảm. Ràng buộc duy nhất phải chứa khoá phân vùng, nên idempotency theo
-- (task_id, step_seq) được bảo đảm bằng khoá dòng task + kiểm tra tồn tại.
CREATE TABLE execution_steps (
  id            UUID NOT NULL DEFAULT uuidv7(),
  org_id        UUID NOT NULL,
  execution_id  UUID NOT NULL,
  task_id       UUID NOT NULL,
  step_seq      INT  NOT NULL CHECK (step_seq >= 1),
  kind          TEXT NOT NULL CHECK (kind IN ('llm_call','tool_call','approval_wait','delegation','summarization')),
  status        TEXT NOT NULL CHECK (status IN ('started','succeeded','failed','denied','unknown')),
  model_id      UUID,
  routing       JSONB,
  tool_id       UUID,
  tool_name     TEXT,
  args_hash     BYTEA,
  request       JSONB,
  response      JSONB,
  error         JSONB,
  approval_id   UUID,
  duration_ms   INT,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at);
CREATE INDEX execution_steps_task_idx ON execution_steps (org_id, task_id, step_seq);

-- Usage ledger: chỉ thêm. Mỗi lời gọi LLM một dòng, ghi cùng transaction với step.
CREATE TABLE usage_records (
  id                  UUID NOT NULL DEFAULT uuidv7(),
  org_id              UUID NOT NULL,
  project_id          UUID NOT NULL,
  user_id             UUID,
  agent_id            UUID,
  agent_version_id    UUID,
  execution_id        UUID,
  task_id             UUID,
  step_seq            INT,
  workflow_id         UUID,
  kind                TEXT NOT NULL CHECK (kind IN ('llm','tool','adjustment')),
  purpose             TEXT,
  provider_id         UUID,
  model_id            UUID,
  tool_id             UUID,
  input_tokens        BIGINT NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
  output_tokens       BIGINT NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
  cached_input_tokens BIGINT NOT NULL DEFAULT 0 CHECK (cached_input_tokens >= 0),
  cache_write_tokens  BIGINT NOT NULL DEFAULT 0 CHECK (cache_write_tokens >= 0),
  reasoning_tokens    BIGINT NOT NULL DEFAULT 0 CHECK (reasoning_tokens >= 0),
  units               NUMERIC(18,4),
  pricing_id          UUID,
  estimated_cost      NUMERIC(18,8) CHECK (estimated_cost <> 'NaN'),   -- NULL nếu chưa có giá
  currency            CHAR(3) NOT NULL DEFAULT 'USD' CHECK (currency ~ '^[A-Z]{3}$'),
  occurred_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (id, occurred_at)
) PARTITION BY RANGE (occurred_at);
CREATE INDEX usage_records_org_time_idx  ON usage_records (org_id, occurred_at);
CREATE INDEX usage_records_execution_idx ON usage_records (org_id, execution_id);

-- Tạo phân vùng tháng cho execution_steps và usage_records. Hai bảng này KHÔNG có
-- phân vùng mặc định: thiếu phân vùng thì ghi lỗi rõ ràng (execution thất bại) thay
-- vì dồn dữ liệu vào một nơi không xoá theo retention được.
--
-- Bảng được tạo rời rồi ATTACH (như audit_logs ở 00003): ATTACH chỉ cần
-- SHARE UPDATE EXCLUSIVE trên bảng cha, còn CREATE TABLE ... PARTITION OF cần
-- ACCESS EXCLUSIVE và sẽ chặn mọi lần ghi step / usage trong lúc chờ khoá.
-- +goose StatementBegin
CREATE FUNCTION execution_create_partitions(month_start date) RETURNS void
LANGUAGE plpgsql
SET search_path = pg_catalog, public
SET timezone = 'UTC'
SET lock_timeout = '5s'
AS $fn$
DECLARE
  t    text;
  col  text;
  part text;
  lo   timestamptz := month_start::timestamptz;
  hi   timestamptz := (month_start + interval '1 month')::timestamptz;
BEGIN
  IF month_start <> date_trunc('month', month_start)::date THEN
    RAISE EXCEPTION 'month_start phải là ngày đầu tháng: %', month_start;
  END IF;
  FOREACH t IN ARRAY ARRAY['execution_steps','usage_records'] LOOP
    part := t || '_' || to_char(month_start, 'YYYY_MM');
    col  := CASE t WHEN 'execution_steps' THEN 'created_at' ELSE 'occurred_at' END;
    CONTINUE WHEN EXISTS (SELECT FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
                          WHERE i.inhparent = ('public.' || t)::regclass AND c.relname = part);
    EXECUTE format('CREATE TABLE public.%I (LIKE public.%I INCLUDING DEFAULTS INCLUDING CONSTRAINTS INCLUDING INDEXES)', part, t);
    -- CHECK khớp biên cho phép ATTACH bỏ qua việc quét bảng.
    EXECUTE format('ALTER TABLE public.%I ADD CONSTRAINT partition_bound CHECK (%I >= %L AND %I < %L)', part, col, lo, col, hi);
    EXECUTE format('ALTER TABLE public.%I ENABLE ROW LEVEL SECURITY', part);
    EXECUTE format('ALTER TABLE public.%I FORCE ROW LEVEL SECURITY', part);
    EXECUTE format($p$CREATE POLICY tenant_isolation ON public.%I
      USING      (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid)
      WITH CHECK (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid)$p$, part);
    EXECUTE format('ALTER TABLE public.%I ATTACH PARTITION public.%I FOR VALUES FROM (%L) TO (%L)', t, part, lo, hi);
    EXECUTE format('ALTER TABLE public.%I DROP CONSTRAINT partition_bound', part);
  END LOOP;
END
$fn$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION execution_create_partitions(date) FROM PUBLIC;

-- Tạo sẵn 24 tháng. Job định kỳ (còn nợ, cùng với audit_logs) gọi hàm này cho các
-- tháng sau.
-- +goose StatementBegin
DO $$
DECLARE
  m date := date_trunc('month', now() AT TIME ZONE 'UTC')::date;
BEGIN
  FOR i IN 0..23 LOOP
    PERFORM execution_create_partitions((m + (i || ' months')::interval)::date);
  END LOOP;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
DO $$
DECLARE t text;
BEGIN
  FOREACH t IN ARRAY ARRAY['agent_executions','agent_tasks','execution_steps','usage_records'] LOOP
    EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
    EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
    EXECUTE format($p$CREATE POLICY tenant_isolation ON %I
      USING      (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid)
      WITH CHECK (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid)$p$, t);
  END LOOP;
END
$$;
-- +goose StatementEnd

-- Trạng thái kết thúc là cuối cùng: execution / task đã xong không chạy lại, không
-- đổi kết quả.
-- +goose StatementBegin
CREATE FUNCTION execution_terminal_guard() RETURNS trigger
LANGUAGE plpgsql AS $fn$
BEGIN
  IF OLD.status IN ('completed','failed','cancelled','budget_exceeded','timed_out','skipped') THEN
    RAISE EXCEPTION '%: % đã ở trạng thái kết thúc (%)', TG_TABLE_NAME, OLD.id, OLD.status USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END
$fn$;
-- +goose StatementEnd
CREATE TRIGGER agent_executions_terminal BEFORE UPDATE ON agent_executions
  FOR EACH ROW EXECUTE FUNCTION execution_terminal_guard();
CREATE TRIGGER agent_tasks_terminal BEFORE UPDATE ON agent_tasks
  FOR EACH ROW EXECUTE FUNCTION execution_terminal_guard();

-- Step đã kết thúc và usage đã ghi là bất biến. Step chỉ được hoàn tất một lần
-- (started → kết quả); nội dung yêu cầu không đổi sau khi ghi.
-- +goose StatementBegin
CREATE FUNCTION execution_steps_guard() RETURNS trigger
LANGUAGE plpgsql AS $fn$
BEGIN
  IF TG_OP = 'UPDATE' AND OLD.status = 'started' AND NEW.status <> 'started'
     AND (to_jsonb(NEW) - ARRAY['status','response','error','duration_ms','model_id','routing'])
       = (to_jsonb(OLD) - ARRAY['status','response','error','duration_ms','model_id','routing']) THEN
    RETURN NEW;
  END IF;
  RAISE EXCEPTION 'execution_steps: % rejected', TG_OP USING ERRCODE = 'restrict_violation';
END
$fn$;
-- +goose StatementEnd
CREATE TRIGGER execution_steps_finish_once BEFORE UPDATE ON execution_steps
  FOR EACH ROW EXECUTE FUNCTION execution_steps_guard();

-- +goose StatementBegin
CREATE FUNCTION usage_records_guard() RETURNS trigger
LANGUAGE plpgsql AS $fn$
BEGIN
  RAISE EXCEPTION 'usage_records is append-only (% rejected)', TG_OP USING ERRCODE = 'restrict_violation';
END
$fn$;
-- +goose StatementEnd
CREATE TRIGGER usage_records_no_update BEFORE UPDATE OR DELETE ON usage_records
  FOR EACH ROW EXECUTE FUNCTION usage_records_guard();
-- Xoá theo retention là DROP / DETACH phân vùng, không qua trigger dòng.

GRANT SELECT, INSERT ON agent_executions, agent_tasks, execution_steps, usage_records TO app_api;
GRANT UPDATE (status, output, error, estimated_cost, cancel_requested_at, started_at, finished_at, updated_at) ON agent_executions TO app_api;
GRANT UPDATE (status, output, output_summary, error, estimated_cost, iterations, tool_calls, started_at, finished_at, updated_at) ON agent_tasks TO app_api;
GRANT UPDATE (status, response, error, duration_ms, model_id, routing) ON execution_steps TO app_api;
GRANT SELECT ON agent_executions, agent_tasks, execution_steps, usage_records TO app_admin;
-- Gateway kiểm lại ở mỗi tool call của Agent nội bộ: task còn chạy, chưa bị huỷ, và
-- tool nằm trong cấu hình của Agent Version.
-- Chỉ những cột gateway cần: nó không đọc được prompt, input hay output.
GRANT SELECT (id, org_id, project_id, initiator_type, initiator_id, status, cancel_requested_at, deadline_at) ON agent_executions TO app_gateway;
GRANT SELECT (id, org_id, execution_id, agent_id, agent_version_id, status) ON agent_tasks TO app_gateway;
GRANT SELECT (id, org_id, agent_id, config) ON agent_versions TO app_gateway;
GRANT SELECT (id, org_id, status) ON agents TO app_gateway;

ALTER TABLE roles NO FORCE ROW LEVEL SECURITY;
-- +goose StatementBegin
DO $$
DECLARE
  n int;
  total int := 0;
  grants jsonb := '{
    "org_admin":     ["execution:read","execution:create","execution:cancel","execution:read_content"],
    "project_admin": ["execution:read","execution:create","execution:cancel","execution:read_content"],
    "operator":      ["execution:read","execution:create","execution:cancel","execution:read_content"],
    "developer":     ["execution:read","execution:create","execution:cancel","execution:read_content"],
    "approver":      ["execution:read"],
    "viewer":        ["execution:read"]
  }';
  r record;
BEGIN
  FOR r IN SELECT key, value FROM jsonb_each(grants) LOOP
    UPDATE roles SET permissions = permissions || ARRAY(SELECT jsonb_array_elements_text(r.value)), updated_at = now()
    WHERE org_id IS NULL AND name = r.key;
    GET DIAGNOSTICS n = ROW_COUNT;
    total := total + n;
  END LOOP;
  IF total <> 6 THEN
    RAISE EXCEPTION 'cập nhật role dựng sẵn tác động % dòng, muốn 6', total;
  END IF;
END
$$;
-- +goose StatementEnd
ALTER TABLE roles FORCE ROW LEVEL SECURITY;

-- +goose Down
ALTER TABLE roles NO FORCE ROW LEVEL SECURITY;
UPDATE roles SET permissions = ARRAY(SELECT p FROM unnest(permissions) p WHERE p !~ '^execution:')
WHERE org_id IS NULL;
ALTER TABLE roles FORCE ROW LEVEL SECURITY;
REVOKE SELECT (id, org_id, agent_id, config) ON agent_versions FROM app_gateway;
REVOKE SELECT (id, org_id, status) ON agents FROM app_gateway;
DROP TABLE usage_records;
DROP FUNCTION usage_records_guard();
DROP TABLE execution_steps;
DROP FUNCTION execution_steps_guard();
DROP FUNCTION execution_create_partitions(date);
DROP TABLE agent_tasks;
DROP TABLE agent_executions;
DROP FUNCTION execution_terminal_guard();
