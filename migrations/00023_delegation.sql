-- +goose Up
-- Delegation (agent-runtime §5.4, ADR-0014): một Agent giao việc cho Agent khác bằng tool
-- nội bộ delegate_task. Lời gọi do model sinh ra là đề xuất; quyết định nằm ở hàm
-- agent_task_delegate dưới đây, chạy nguyên tử dưới khoá của execution.
SET LOCAL lock_timeout = '5s';

-- Ảnh chụp lúc tạo execution (do process api ghi, cùng transaction với execution):
-- Agent Version nào được giao việc cho Agent nào, và cấu hình đã kiểm của từng Agent
-- đích. Quyền của một execution cố định từ lúc bắt đầu. Hai bảng chỉ thêm.
CREATE TABLE execution_delegation_sources (
  org_id           UUID NOT NULL REFERENCES organizations(id),
  execution_id     UUID NOT NULL,
  agent_version_id UUID NOT NULL,
  can_delegate_to  UUID[] NOT NULL CHECK (cardinality(can_delegate_to) BETWEEN 1 AND 8
                                          AND array_position(can_delegate_to, NULL) IS NULL),
  -- Trần 5 là mức sàn của platform (orchestrator.MaxDelegationDepth).
  max_depth        INT NOT NULL CHECK (max_depth BETWEEN 1 AND 5),
  -- Trần chi phí một lượt model của Agent nguồn: phần nó giữ lại cho chính nó khi giao việc.
  budget_floor     NUMERIC(18,8) NOT NULL CHECK (budget_floor >= 0 AND budget_floor <> 'NaN'),
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (org_id, execution_id, agent_version_id),
  FOREIGN KEY (org_id, execution_id) REFERENCES agent_executions (org_id, id) ON DELETE CASCADE
);
CREATE TABLE execution_delegation_targets (
  org_id           UUID NOT NULL REFERENCES organizations(id),
  execution_id     UUID NOT NULL,
  agent_id         UUID NOT NULL,
  -- Version đang hoạt động của Agent đích lúc execution được tạo. System prompt không
  -- chụp ở đây: Agent Version bất biến, hàm quyết định đọc nó lúc tạo task con.
  agent_version_id UUID NOT NULL,
  slug             TEXT NOT NULL,
  model_chain      UUID[] NOT NULL CHECK (array_ndims(model_chain) = 1 AND cardinality(model_chain) BETWEEN 1 AND 4
                                          AND array_position(model_chain, NULL) IS NULL),
  -- limits.budget của Agent đích (NULL = không có); budget_floor là trần chi phí một
  -- lượt model của nó: budget của task con không được nhỏ hơn.
  budget           NUMERIC(18,8) CHECK (budget > 0 AND budget <> 'NaN'),
  budget_floor     NUMERIC(18,8) NOT NULL CHECK (budget_floor >= 0 AND budget_floor <> 'NaN'),
  -- Cấu hình chạy của task con (tham chiếu model, giới hạn); không có nội dung.
  spec             JSONB NOT NULL CHECK (jsonb_typeof(spec) = 'object'),
  timeout_seconds  INT NOT NULL CHECK (timeout_seconds > 0),
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (org_id, execution_id, agent_id),
  FOREIGN KEY (org_id, execution_id) REFERENCES agent_executions (org_id, id) ON DELETE CASCADE,
  FOREIGN KEY (org_id, agent_id) REFERENCES agents (org_id, id),
  FOREIGN KEY (agent_id, agent_version_id) REFERENCES agent_versions (agent_id, id)
);
ALTER TABLE execution_delegation_sources ENABLE ROW LEVEL SECURITY;
ALTER TABLE execution_delegation_sources FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON execution_delegation_sources
  USING      (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid)
  WITH CHECK (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid);
ALTER TABLE execution_delegation_targets ENABLE ROW LEVEL SECURITY;
ALTER TABLE execution_delegation_targets FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON execution_delegation_targets
  USING      (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid)
  WITH CHECK (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid);
-- +goose StatementBegin
CREATE FUNCTION execution_delegation_immutable() RETURNS trigger
LANGUAGE plpgsql AS $fn$
BEGIN
  RAISE EXCEPTION '%: ảnh chụp delegation của execution không đổi được', TG_TABLE_NAME USING ERRCODE = 'restrict_violation';
END
$fn$;
-- +goose StatementEnd
CREATE TRIGGER execution_delegation_sources_immutable BEFORE UPDATE ON execution_delegation_sources
  FOR EACH ROW EXECUTE FUNCTION execution_delegation_immutable();
CREATE TRIGGER execution_delegation_targets_immutable BEFORE UPDATE ON execution_delegation_targets
  FOR EACH ROW EXECUTE FUNCTION execution_delegation_immutable();
GRANT SELECT, INSERT ON execution_delegation_sources, execution_delegation_targets TO app_api;
GRANT SELECT ON execution_delegation_sources, execution_delegation_targets TO app_worker, app_admin;

-- Task con của một lần giao việc: bước (step_seq) của task cha đã yêu cầu nó. Cặp
-- (task cha, bước) là khoá idempotency: bước chạy lại không tạo task con thứ hai
-- (index duy nhất tạo ở 00024).
ALTER TABLE agent_tasks ADD COLUMN delegated_step_seq INT;
ALTER TABLE agent_tasks ADD CONSTRAINT agent_tasks_delegated_check
  CHECK ((parent_task_id IS NULL) = (delegated_step_seq IS NULL)) NOT VALID; -- VALIDATE ở 00024

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION agent_tasks_snapshot_guard() RETURNS trigger
LANGUAGE plpgsql AS $fn$
BEGIN
  IF NEW.model_chain IS DISTINCT FROM OLD.model_chain OR NEW.system_prompt IS DISTINCT FROM OLD.system_prompt
     OR NEW.goal IS DISTINCT FROM OLD.goal OR NEW.agent_version_id IS DISTINCT FROM OLD.agent_version_id
     OR NEW.execution_id IS DISTINCT FROM OLD.execution_id OR NEW.budget_limit IS DISTINCT FROM OLD.budget_limit
     OR NEW.plan_key IS DISTINCT FROM OLD.plan_key OR NEW.depends_on IS DISTINCT FROM OLD.depends_on
     OR NEW.on_failure IS DISTINCT FROM OLD.on_failure OR NEW.parent_task_id IS DISTINCT FROM OLD.parent_task_id
     OR NEW.depth IS DISTINCT FROM OLD.depth OR NEW.agent_id IS DISTINCT FROM OLD.agent_id
     OR NEW.delegated_step_seq IS DISTINCT FROM OLD.delegated_step_seq THEN
    RAISE EXCEPTION 'agent_tasks: cấu hình đã chụp của task % không đổi được', OLD.id USING ERRCODE = 'restrict_violation';
  END IF;
  IF NEW.output_summary IS DISTINCT FROM OLD.output_summary
     AND NEW.status NOT IN ('completed','failed','cancelled','budget_exceeded','timed_out') THEN
    RAISE EXCEPTION 'agent_tasks: output_summary của % chỉ được ghi khi task kết thúc', OLD.id USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END
$fn$;
-- +goose StatementEnd

-- agent_task_spent: mức một task đã tiêu, GỒM cả các task con của nó (và con của chúng).
-- Budget của task cha bị chặn theo số này, nên việc giao việc không mở thêm tiền: nó
-- được tính từ cây task, không phải từ một bộ cộng dồn mà process chạy vòng lặp tự ghi.
-- Chạy với quyền của bên gọi (RLS áp như mọi truy vấn khác).
-- +goose StatementBegin
CREATE FUNCTION agent_task_spent(p_org uuid, p_task uuid) RETURNS numeric
LANGUAGE sql STABLE
SET search_path = pg_catalog, public, pg_temp
AS $fn$
  WITH RECURSIVE sub AS (
    SELECT t.id, t.estimated_cost, 0 AS hop FROM public.agent_tasks t WHERE t.org_id = p_org AND t.id = p_task
    UNION ALL
    SELECT c.id, c.estimated_cost, s.hop + 1 FROM public.agent_tasks c JOIN sub s ON c.parent_task_id = s.id
     WHERE c.org_id = p_org AND s.hop < 8
  )
  SELECT COALESCE(sum(estimated_cost), 0) FROM sub
$fn$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION agent_task_spent(uuid, uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION agent_task_spent(uuid, uuid) TO app_api, app_worker, app_gateway;
-- Gateway chặn budget của task theo agent_task_spent, và áp mức sàn cho task được giao việc.
GRANT SELECT (parent_task_id, depth) ON agent_tasks TO app_gateway;

-- agent_task_delegate là ĐIỂM QUYẾT ĐỊNH của delegation (ADR-0014). Nó tạo task con của
-- một task đang chạy, hoặc trả về luật đã từ chối, và tự ghi audit cho cả hai (actor là
-- người khởi tạo của execution, đọc từ database). app_worker không tạo được task con bằng
-- đường nào khác (nó không có INSERT trên agent_tasks). Luật, theo thứ tự:
--   - bước đã giao việc rồi thì trả lại đúng task con đó (idempotent theo task cha + bước);
--   - task cha đang chạy, execution còn chạy, chưa bị huỷ, còn hạn;
--   - bước là một step delegation đang mở của task cha (mỗi task con gắn với một bước đã ghi);
--   - task cha không còn task con nào đang mở: giao việc là TUẦN TỰ, nên phần budget còn
--     lại của task cha không bị cấp hai lần;
--   - Agent đích nằm trong can_delegate_to của Agent Version nguồn và có trong ảnh chụp;
--   - Agent đích vẫn đang hoạt động với đúng version đã chụp (tắt hay rollback là giảm
--     quyền: có hiệu lực với mọi task con chưa tạo);
--   - Agent đích không phải tổ tiên của task con (không chu trình A → B → A);
--   - độ sâu không vượt max_depth nhỏ nhất của các Agent Version trong chuỗi tổ tiên
--     (đã bị chặn ở 5 bởi CHECK của bảng);
--   - tổng số task của execution chưa chạm trần 16 (orchestrator.MaxTasks);
--   - task con có budget: phần nhỏ hơn giữa budget của Agent đích và phần còn lại của
--     task cha sau khi giữ lại một lượt model cho chính nó; đủ cho ít nhất một lượt của đích.
-- Quyền của task con là của Agent đích giao với quyền của người khởi tạo: nó chạy dưới
-- Agent Version đích với cùng người khởi tạo, và Gateway kiểm cả hai ở mỗi lời gọi.
-- Kết quả: {"task_id": <uuid|null>, "rule": <luật đã khớp>}.
-- +goose StatementBegin
CREATE FUNCTION agent_task_delegate(p_parent uuid, p_step_seq int, p_target uuid, p_goal text) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $fn$
DECLARE
  org         uuid := NULLIF(current_setting('app.org_id', true), '')::uuid;
  par         record;
  ex          record;
  src         public.execution_delegation_sources;
  tgt         public.execution_delegation_targets;
  child       uuid;
  cur         uuid;
  cur_agent   uuid;
  cur_version uuid;
  cur_depth   int;
  max_d       int := 5;
  hops        int := 0;
  n           int;
  lim         numeric;
  prompt      text;
  rule        text;
BEGIN
  IF org IS NULL OR p_parent IS NULL OR p_target IS NULL OR p_step_seq IS NULL OR p_step_seq < 1 THEN
    RAISE EXCEPTION 'agent_task_delegate: thiếu tenant hoặc tham số' USING ERRCODE = 'invalid_parameter_value';
  END IF;
  SELECT execution_id INTO cur FROM public.agent_tasks WHERE org_id = org AND id = p_parent;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'agent_task_delegate: task không tồn tại trong tenant' USING ERRCODE = 'no_data_found';
  END IF;
  -- Thứ tự khoá như mọi đường ghi khác: execution rồi tới task.
  SELECT id, project_id, status, cancel_requested_at, deadline_at, initiator_type, initiator_id INTO ex
    FROM public.agent_executions WHERE org_id = org AND id = cur FOR NO KEY UPDATE;
  SELECT id, status, depth, agent_id, agent_version_id, budget_limit, created_at INTO par
    FROM public.agent_tasks WHERE org_id = org AND id = p_parent FOR NO KEY UPDATE;

  SELECT id INTO child FROM public.agent_tasks
   WHERE org_id = org AND parent_task_id = p_parent AND delegated_step_seq = p_step_seq;
  IF FOUND THEN
    RETURN jsonb_build_object('task_id', child, 'rule', 'replayed');
  END IF;

  -- Mọi nhánh từ chối đặt rule rồi nhảy xuống phần ghi audit ở cuối.
  <<decide>>
  BEGIN
    IF par.status <> 'running' OR ex.status NOT IN ('running','waiting_approval','paused')
       OR ex.cancel_requested_at IS NOT NULL OR ex.deadline_at <= now() THEN
      rule := 'task_not_active'; EXIT decide;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM public.execution_steps s
                    WHERE s.org_id = org AND s.task_id = par.id AND s.step_seq = p_step_seq
                      AND s.kind = 'delegation' AND s.status = 'started' AND s.created_at >= par.created_at) THEN
      rule := 'task_not_active'; EXIT decide;
    END IF;
    IF EXISTS (SELECT 1 FROM public.agent_tasks c
                WHERE c.org_id = org AND c.parent_task_id = par.id AND c.status IN ('queued','running','waiting_approval','paused')) THEN
      rule := 'child_running'; EXIT decide;
    END IF;
    SELECT * INTO src FROM public.execution_delegation_sources
     WHERE org_id = org AND execution_id = ex.id AND agent_version_id = par.agent_version_id;
    IF NOT FOUND THEN
      rule := 'delegation_not_configured'; EXIT decide;
    END IF;
    IF NOT (p_target = ANY (src.can_delegate_to)) THEN
      rule := 'target_not_allowed'; EXIT decide;
    END IF;
    SELECT * INTO tgt FROM public.execution_delegation_targets
     WHERE org_id = org AND execution_id = ex.id AND agent_id = p_target;
    IF NOT FOUND THEN
      rule := 'target_unavailable'; EXIT decide;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM public.agents a
                    WHERE a.org_id = org AND a.id = p_target AND a.status = 'active'
                      AND a.active_version_id = tgt.agent_version_id) THEN
      rule := 'target_unavailable'; EXIT decide;
    END IF;
    -- Đi ngược chuỗi tổ tiên (có trần): Agent đích không được là một tổ tiên, và độ sâu
    -- bị chặn bởi max_depth nhỏ nhất của mọi Agent Version trong chuỗi, nên giới hạn của
    -- Agent ở trên áp cho cả phần bên dưới.
    cur := par.id;
    WHILE cur IS NOT NULL AND hops < 8 LOOP
      SELECT agent_id, agent_version_id, parent_task_id INTO cur_agent, cur_version, cur
        FROM public.agent_tasks WHERE org_id = org AND id = cur;
      IF cur_agent = p_target THEN
        rule := 'cycle'; EXIT decide;
      END IF;
      SELECT s.max_depth INTO cur_depth FROM public.execution_delegation_sources s
       WHERE s.org_id = org AND s.execution_id = ex.id AND s.agent_version_id = cur_version;
      max_d := LEAST(max_d, COALESCE(cur_depth, 0));
      hops := hops + 1;
    END LOOP;
    IF cur IS NOT NULL OR par.depth + 1 > max_d THEN
      rule := 'max_depth'; EXIT decide;
    END IF;
    SELECT count(*) INTO n FROM public.agent_tasks WHERE org_id = org AND execution_id = ex.id;
    IF n >= 16 THEN
      rule := 'task_limit'; EXIT decide;
    END IF;
    IF p_goal IS NULL OR btrim(p_goal) = '' OR char_length(p_goal) > 20000 THEN
      rule := 'invalid_goal'; EXIT decide;
    END IF;
    lim := tgt.budget;
    IF par.budget_limit IS NOT NULL THEN
      -- LEAST bỏ qua NULL. Phần còn lại tính từ cây task, trừ đi một lượt của task cha.
      lim := LEAST(lim, par.budget_limit - public.agent_task_spent(org, par.id) - src.budget_floor);
    END IF;
    IF lim IS NULL THEN
      rule := 'budget_required'; EXIT decide;
    END IF;
    lim := trunc(lim, 8);
    IF lim <= 0 OR lim < tgt.budget_floor THEN
      rule := 'budget_insufficient'; EXIT decide;
    END IF;
    SELECT system_prompt INTO prompt FROM public.agent_versions
     WHERE org_id = org AND agent_id = p_target AND id = tgt.agent_version_id;
    IF NOT FOUND THEN
      rule := 'target_unavailable'; EXIT decide;
    END IF;

    INSERT INTO public.agent_tasks (org_id, execution_id, parent_task_id, delegated_step_seq, depth, agent_id, agent_version_id,
                                    goal, system_prompt, model_chain, budget_limit)
    VALUES (org, ex.id, par.id, p_step_seq, par.depth + 1, p_target, tgt.agent_version_id,
            p_goal, prompt, tgt.model_chain, lim)
    RETURNING id INTO child;
    rule := 'allowed';
  END;

  -- Mọi quyết định, cho phép lẫn từ chối, ghi audit kèm luật đã khớp, ngay tại điểm
  -- quyết định: không phụ thuộc process đã gọi.
  PERFORM public.audit_append(org, ex.project_id, ex.initiator_type, ex.initiator_id, NULL,
    CASE WHEN child IS NULL THEN 'delegation.deny' ELSE 'delegation.create' END, 'task', COALESCE(child, par.id),
    CASE WHEN child IS NULL THEN 'denied' ELSE 'success' END, ex.id,
    jsonb_build_object('rule', rule, 'parent_task_id', par.id, 'target_agent_id', p_target,
                       'step_seq', p_step_seq, 'depth', par.depth + 1), NULL, NULL);
  RETURN jsonb_build_object('task_id', child, 'rule', rule);
END
$fn$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION agent_task_delegate(uuid, int, uuid, text) FROM PUBLIC;
-- app_api chỉ cần hàm này ở chế độ chạy vòng lặp trong process (local, test).
GRANT EXECUTE ON FUNCTION agent_task_delegate(uuid, int, uuid, text) TO app_worker, app_api;

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP FUNCTION agent_task_delegate(uuid, int, uuid, text);
REVOKE SELECT (parent_task_id, depth) ON agent_tasks FROM app_gateway;
DROP FUNCTION agent_task_spent(uuid, uuid);
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION agent_tasks_snapshot_guard() RETURNS trigger
LANGUAGE plpgsql AS $fn$
BEGIN
  IF NEW.model_chain IS DISTINCT FROM OLD.model_chain OR NEW.system_prompt IS DISTINCT FROM OLD.system_prompt
     OR NEW.goal IS DISTINCT FROM OLD.goal OR NEW.agent_version_id IS DISTINCT FROM OLD.agent_version_id
     OR NEW.execution_id IS DISTINCT FROM OLD.execution_id OR NEW.budget_limit IS DISTINCT FROM OLD.budget_limit
     OR NEW.plan_key IS DISTINCT FROM OLD.plan_key OR NEW.depends_on IS DISTINCT FROM OLD.depends_on
     OR NEW.on_failure IS DISTINCT FROM OLD.on_failure OR NEW.parent_task_id IS DISTINCT FROM OLD.parent_task_id
     OR NEW.depth IS DISTINCT FROM OLD.depth OR NEW.agent_id IS DISTINCT FROM OLD.agent_id THEN
    RAISE EXCEPTION 'agent_tasks: cấu hình đã chụp của task % không đổi được', OLD.id USING ERRCODE = 'restrict_violation';
  END IF;
  IF NEW.output_summary IS DISTINCT FROM OLD.output_summary
     AND NEW.status NOT IN ('completed','failed','cancelled','budget_exceeded','timed_out') THEN
    RAISE EXCEPTION 'agent_tasks: output_summary của % chỉ được ghi khi task kết thúc', OLD.id USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END
$fn$;
-- +goose StatementEnd
ALTER TABLE agent_tasks DROP CONSTRAINT agent_tasks_delegated_check;
ALTER TABLE agent_tasks DROP COLUMN delegated_step_seq;
DROP TABLE execution_delegation_targets;
DROP TABLE execution_delegation_sources;
DROP FUNCTION execution_delegation_immutable();
