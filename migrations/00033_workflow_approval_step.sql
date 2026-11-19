-- +goose Up
-- Phase 3: bước `approval` của workflow (ADR-0015 mục 5) và approval nhiều người duyệt.
--
-- Một nút `approval` của kế hoạch là một cổng chờ người: kế hoạch dừng ở nút đó tới khi
-- đủ người duyệt, có người từ chối, hoặc hết hạn. Approval của nút gắn với
-- (execution, plan_key) thay vì (task, step_seq).
--
-- Process chạy kế hoạch (app_worker) không phải là nơi quyết định cổng có mở hay không.
-- Database giữ điều đó, theo mẫu của agent_task_delegate (ADR-0014):
--   * approval của nút được tạo bởi plan_step_request_approval, với nội dung (thông điệp,
--     số người duyệt, hạn, người yêu cầu) đọc từ cấu hình đã chụp của nút và từ execution;
--   * kết quả của nút được ghi bởi plan_step_close_approval, từ trạng thái của approval;
--   * trigger của execution_plan_steps từ chối "completed" cho nút approval chưa có
--     approval đã tiêu thụ, với mọi role (worker có UPDATE trên cột status);
--   * plan_node_gates_open / agent_task_gates_open cho BeginTask và Gateway biết mọi cổng
--     mà một bước phụ thuộc (kể cả gián tiếp) đã mở hay chưa: Gateway không phục vụ tool
--     call hay lượt model của task đứng sau một cổng chưa mở, dù worker nói gì.
SET LOCAL lock_timeout = '5s';
ALTER TABLE approvals ADD COLUMN plan_key TEXT;
-- Người không được duyệt ngoài người yêu cầu: người đã viết hành động được duyệt (với
-- cổng của workflow: người viết definition của lần chạy).
ALTER TABLE approvals ADD COLUMN excluded_approver UUID;
ALTER TABLE approvals ADD CONSTRAINT approvals_workflow_step_check
  CHECK (subject_type <> 'workflow_step' OR (execution_id IS NOT NULL AND plan_key IS NOT NULL AND task_id IS NULL)) NOT VALID; -- VALIDATE ở 00034
ALTER TABLE approvals ADD CONSTRAINT approvals_plan_key_check
  CHECK (plan_key IS NULL OR subject_type = 'workflow_step') NOT VALID; -- VALIDATE ở 00034
-- Approval của một nút sống đúng bằng nút đó.
ALTER TABLE approvals ADD CONSTRAINT approvals_plan_step_fkey
  FOREIGN KEY (org_id, execution_id, plan_key) REFERENCES execution_plan_steps (org_id, execution_id, plan_key) ON DELETE CASCADE NOT VALID; -- VALIDATE ở 00034

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION approvals_guard() RETURNS trigger
LANGUAGE plpgsql AS $fn$
DECLARE
  last jsonb;
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.status <> 'pending' OR NEW.decisions <> '[]'::jsonb OR NEW.resolved_at IS NOT NULL THEN
      RAISE EXCEPTION 'approvals: approval mới phải ở trạng thái pending, chưa có quyết định' USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
  END IF;
  IF (to_jsonb(NEW) - ARRAY['status','decisions','resolved_at']) <> (to_jsonb(OLD) - ARRAY['status','decisions','resolved_at']) THEN
    RAISE EXCEPTION 'approvals: nội dung của approval % không đổi được', OLD.id USING ERRCODE = 'restrict_violation';
  END IF;
  IF NEW.status <> OLD.status AND NOT (
       (OLD.status = 'pending'  AND NEW.status IN ('approved','rejected','expired','cancelled'))
    OR (OLD.status = 'approved' AND NEW.status IN ('consumed','expired','cancelled'))) THEN
    RAISE EXCEPTION 'approvals: % không chuyển được từ % sang %', OLD.id, OLD.status, NEW.status USING ERRCODE = 'restrict_violation';
  END IF;
  IF NEW.status = OLD.status AND NEW.resolved_at IS DISTINCT FROM OLD.resolved_at THEN
    RAISE EXCEPTION 'approvals: resolved_at của % chỉ đổi cùng một lần chuyển trạng thái', OLD.id USING ERRCODE = 'restrict_violation';
  END IF;
  IF NEW.decisions <> OLD.decisions THEN
    -- Nối thêm đúng một phần tử vào cuối: bỏ phần tử cuối của bản mới phải ra đúng bản cũ.
    IF OLD.status <> 'pending' OR jsonb_array_length(NEW.decisions) <> jsonb_array_length(OLD.decisions) + 1
       OR (NEW.decisions - (-1)) <> OLD.decisions THEN
      RAISE EXCEPTION 'approvals: quyết định của % chỉ được nối thêm khi còn pending', OLD.id USING ERRCODE = 'restrict_violation';
    END IF;
    last := NEW.decisions -> -1;
    IF last->>'user_id' IS NULL OR last->>'decision' NOT IN ('approve','reject')
       OR last->>'user_id' !~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' THEN
      RAISE EXCEPTION 'approvals: quyết định của % không hợp lệ', OLD.id USING ERRCODE = 'restrict_violation';
    END IF;
    -- So theo uuid, không theo chuỗi: cách viết khác của cùng một định danh không lọt.
    IF (last->>'user_id')::uuid IN (OLD.requested_by, OLD.requested_by_owner, OLD.excluded_approver) THEN
      RAISE EXCEPTION 'approvals: quyết định của % là của người yêu cầu hoặc người viết hành động', OLD.id USING ERRCODE = 'restrict_violation';
    END IF;
    -- Mỗi người duyệt quyết định một lần: approval cần hai người duyệt là hai người khác nhau.
    IF EXISTS (SELECT 1 FROM jsonb_array_elements(OLD.decisions) AS d(v) WHERE lower(d.v->>'user_id') = lower(last->>'user_id')) THEN
      RAISE EXCEPTION 'approvals: người duyệt đã quyết định approval % rồi', OLD.id USING ERRCODE = 'restrict_violation';
    END IF;
  END IF;
  IF OLD.status = 'pending' AND NEW.status IN ('approved','rejected') THEN
    last := NEW.decisions -> -1;
    IF NEW.decisions = OLD.decisions
       OR (NEW.status = 'approved' AND (jsonb_array_length(NEW.decisions) < NEW.required_approvals
           OR EXISTS (SELECT 1 FROM jsonb_array_elements(NEW.decisions) AS d(v) WHERE d.v->>'decision' <> 'approve')))
       OR (NEW.status = 'rejected' AND last->>'decision' <> 'reject') THEN
      RAISE EXCEPTION 'approvals: % không được % mà không có quyết định tương ứng', OLD.id, NEW.status USING ERRCODE = 'restrict_violation';
    END IF;
  END IF;
  RETURN NEW;
END
$fn$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION execution_plan_steps_guard() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $fn$
BEGIN
  IF NEW.kind IS DISTINCT FROM OLD.kind OR NEW.config IS DISTINCT FROM OLD.config
     OR NEW.plan_key IS DISTINCT FROM OLD.plan_key OR NEW.execution_id IS DISTINCT FROM OLD.execution_id
     OR NEW.org_id IS DISTINCT FROM OLD.org_id OR NEW.created_at IS DISTINCT FROM OLD.created_at
     OR (OLD.started_at IS NOT NULL AND NEW.started_at IS DISTINCT FROM OLD.started_at) THEN
    RAISE EXCEPTION 'execution_plan_steps: cấu hình của nút % không đổi được', OLD.plan_key USING ERRCODE = 'restrict_violation';
  END IF;
  IF OLD.status IN ('completed','failed','cancelled','timed_out','skipped') OR NEW.status = 'queued' THEN
    RAISE EXCEPTION 'execution_plan_steps: nút % đã kết thúc hoặc không quay lại queued', OLD.plan_key USING ERRCODE = 'restrict_violation';
  END IF;
  -- "completed" là điều mở các bước phụ thuộc, nên database tự giữ điều kiện của nó, với
  -- mọi role: nút chờ phải đã chạy và đã chờ đủ (trừ 2 giây cho độ lệch đồng hồ của nơi
  -- chạy kế hoạch). Nút approval chỉ hoàn thành được qua approval của nó (00033).
  IF NEW.status = 'completed' THEN
    IF OLD.kind = 'wait' AND (OLD.status <> 'running' OR OLD.started_at IS NULL
         OR OLD.started_at > now() - make_interval(secs => GREATEST((OLD.config->>'seconds')::float8 - 2, 0))) THEN
      RAISE EXCEPTION 'execution_plan_steps: nút chờ % chưa chờ đủ', OLD.plan_key USING ERRCODE = 'restrict_violation';
    END IF;
    -- Nút approval: phải có approval của đúng nút này đã được tiêu thụ (đủ người duyệt).
    IF OLD.kind = 'approval' AND NOT EXISTS (SELECT 1 FROM public.approvals a
         WHERE a.org_id = OLD.org_id AND a.execution_id = OLD.execution_id AND a.plan_key = OLD.plan_key AND a.status = 'consumed') THEN
      RAISE EXCEPTION 'execution_plan_steps: nút approval % chưa được duyệt', OLD.plan_key USING ERRCODE = 'restrict_violation';
    END IF;
  END IF;
  RETURN NEW;
END
$fn$;
-- +goose StatementEnd

-- plan_node_gates_open: mọi nút approval mà nút p_key phụ thuộc, trực tiếp hay gián tiếp,
-- đã hoàn thành. on_failure: continue của một bước ở giữa không mở được cổng đã bị từ chối.
-- +goose StatementBegin
CREATE FUNCTION plan_node_gates_open(p_execution uuid, p_key text) RETURNS boolean
LANGUAGE sql STABLE SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $fn$
  WITH RECURSIVE nodes AS (
    SELECT n.node->>'id' AS key, COALESCE(n.node->>'kind', '') AS kind,
           CASE WHEN jsonb_typeof(n.node->'depends_on') = 'array' THEN n.node->'depends_on' ELSE '[]'::jsonb END AS deps
    FROM public.agent_executions e, jsonb_array_elements(e.plan->'tasks') AS n(node)
    WHERE e.org_id = NULLIF(current_setting('app.org_id', true), '')::uuid AND e.id = p_execution
      AND jsonb_typeof(e.plan->'tasks') = 'array'
  ), anc(key) AS (
    SELECT d.value FROM nodes n, jsonb_array_elements_text(n.deps) AS d(value) WHERE n.key = p_key
    UNION
    SELECT d.value FROM anc a JOIN nodes n ON n.key = a.key, jsonb_array_elements_text(n.deps) AS d(value)
  )
  SELECT NOT EXISTS (
    SELECT 1 FROM anc a JOIN nodes n ON n.key = a.key
    WHERE n.kind = 'approval' AND NOT EXISTS (
      SELECT 1 FROM public.execution_plan_steps s
      WHERE s.org_id = NULLIF(current_setting('app.org_id', true), '')::uuid AND s.execution_id = p_execution
        AND s.plan_key = n.key AND s.status = 'completed'));
$fn$;
-- +goose StatementEnd

-- agent_task_gates_open: như trên, cho một task. Task con của một lần giao việc theo task
-- gốc của nó trong kế hoạch. Task không thuộc kế hoạch nào thì không đứng sau cổng nào.
-- +goose StatementBegin
CREATE FUNCTION agent_task_gates_open(p_task uuid) RETURNS boolean
LANGUAGE sql STABLE SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $fn$
  WITH RECURSIVE up AS (
    SELECT t.id, t.parent_task_id, t.execution_id, t.plan_key, 0 AS hops FROM public.agent_tasks t
    WHERE t.org_id = NULLIF(current_setting('app.org_id', true), '')::uuid AND t.id = p_task
    UNION ALL
    SELECT t.id, t.parent_task_id, t.execution_id, t.plan_key, up.hops + 1 FROM public.agent_tasks t JOIN up ON t.id = up.parent_task_id
    WHERE t.org_id = NULLIF(current_setting('app.org_id', true), '')::uuid AND up.hops < 8
  )
  SELECT COALESCE((SELECT public.plan_node_gates_open(up.execution_id, up.plan_key) FROM up
                   WHERE up.parent_task_id IS NULL AND up.plan_key IS NOT NULL LIMIT 1), true);
$fn$;
-- +goose StatementEnd

-- plan_step_request_approval tạo approval cho một nút `approval` đang chạy, hoặc trả về
-- approval đã có của nút đó (idempotent theo execution + plan_key). NULL khi nút không
-- tồn tại, không phải nút approval, không ở trạng thái running, còn đứng sau một cổng
-- chưa mở, hoặc không xác định được người yêu cầu (fail-closed). Người yêu cầu là người
-- khởi tạo của execution (với lần chạy theo lịch: người chạy của workflow), nên người đó
-- không tự duyệt được cổng của chính mình; người đã viết definition của lần chạy cũng không.
-- +goose StatementBegin
CREATE FUNCTION plan_step_request_approval(p_execution uuid, p_key text) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $fn$
DECLARE
  org    uuid := NULLIF(current_setting('app.org_id', true), '')::uuid;
  ex     record;
  st     record;
  wf     record;
  ap     uuid;
  owner  uuid;
  author uuid;
BEGIN
  IF org IS NULL THEN
    RAISE EXCEPTION 'plan_step_request_approval: transaction chưa đặt tenant' USING ERRCODE = 'insufficient_privilege';
  END IF;
  SELECT id, project_id, initiator_type, initiator_id, deadline_at, status, cancel_requested_at, workflow_execution_id INTO ex
  FROM public.agent_executions WHERE org_id = org AND id = p_execution FOR NO KEY UPDATE;
  IF NOT FOUND OR ex.status NOT IN ('running','waiting_approval','paused') OR ex.cancel_requested_at IS NOT NULL
     OR ex.deadline_at <= now() OR ex.initiator_type NOT IN ('user','api_key') OR ex.initiator_id IS NULL THEN
    RETURN NULL;
  END IF;
  SELECT kind, status, config INTO st FROM public.execution_plan_steps
  WHERE org_id = org AND execution_id = p_execution AND plan_key = p_key;
  IF NOT FOUND OR st.kind <> 'approval' OR st.status <> 'running' OR NOT public.plan_node_gates_open(p_execution, p_key) THEN
    RETURN NULL;
  END IF;
  SELECT id INTO ap FROM public.approvals WHERE org_id = org AND execution_id = p_execution AND plan_key = p_key;
  IF FOUND THEN
    RETURN ap;
  END IF;
  owner := ex.initiator_id;
  IF ex.initiator_type = 'api_key' THEN
    SELECT created_by INTO owner FROM public.api_keys WHERE org_id = org AND id = ex.initiator_id;
  END IF;
  IF owner IS NULL THEN
    RETURN NULL;
  END IF;
  -- Người viết definition của lần chạy (nếu execution là của một lần chạy workflow).
  IF ex.workflow_execution_id IS NOT NULL THEN
    SELECT definition_by_type, definition_by INTO wf FROM public.workflow_executions
    WHERE org_id = org AND id = ex.workflow_execution_id;
    IF FOUND AND wf.definition_by IS NOT NULL THEN
      author := wf.definition_by;
      IF wf.definition_by_type = 'api_key' THEN
        SELECT created_by INTO author FROM public.api_keys WHERE org_id = org AND id = wf.definition_by;
      END IF;
    END IF;
  END IF;
  INSERT INTO public.approvals (org_id, project_id, execution_id, plan_key, subject_type, payload, risk_level, required_approvals,
                                requested_by_type, requested_by, requested_by_owner, excluded_approver, expires_at)
  VALUES (org, ex.project_id, ex.id, p_key, 'workflow_step',
          jsonb_build_object('plan_key', p_key, 'message', st.config->>'message'), 'write', (st.config->>'required_approvals')::int,
          ex.initiator_type, ex.initiator_id, owner, author,
          LEAST(now() + make_interval(secs => (st.config->>'timeout_seconds')::float8), ex.deadline_at))
  RETURNING id INTO ap;
  PERFORM public.audit_append(org, ex.project_id, ex.initiator_type, ex.initiator_id, NULL, 'approval.request', 'approval', ap,
    'success', ex.id, jsonb_build_object('subject_type', 'workflow_step', 'plan_key', p_key,
                                         'required_approvals', st.config->>'required_approvals'), NULL, NULL);
  RETURN ap;
END
$fn$;
-- +goose StatementEnd

-- plan_step_approval_status: trạng thái approval của một nút, cho nơi đang chờ nó.
-- Approval còn pending mà đã quá hạn được báo là expired. Approval đã được duyệt (quyết
-- định luôn được ghi trong hạn) giữ nguyên là approved. NULL: chưa có.
-- +goose StatementBegin
CREATE FUNCTION plan_step_approval_status(p_execution uuid, p_key text) RETURNS text
LANGUAGE plpgsql STABLE SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $fn$
DECLARE
  org uuid := NULLIF(current_setting('app.org_id', true), '')::uuid;
  st  text;
BEGIN
  IF org IS NULL THEN
    RAISE EXCEPTION 'plan_step_approval_status: transaction chưa đặt tenant' USING ERRCODE = 'insufficient_privilege';
  END IF;
  SELECT CASE WHEN status = 'pending' AND expires_at <= now() THEN 'expired' ELSE status END INTO st
  FROM public.approvals WHERE org_id = org AND execution_id = p_execution AND plan_key = p_key;
  RETURN st;
END
$fn$;
-- +goose StatementEnd

-- plan_step_close_approval kết thúc một nút approval: nó đọc approval của nút, ghi trạng
-- thái của CẢ approval lẫn nút, và trả về trạng thái của nút:
--   completed — đủ người duyệt (quyết định được ghi trong hạn): approval được tiêu thụ
--               (consumed, dùng một lần), kèm audit;
--   failed    — có người từ chối, hoặc hết hạn mà chưa đủ người duyệt (hết hạn là từ chối);
--   cancelled / timed_out — kế hoạch dừng khi còn chờ (execution bị huỷ, quá hạn, đã
--               kết thúc, hoặc nơi chạy kế hoạch báo dừng qua p_stop): approval bị huỷ;
--   pending   — approval còn chờ và không có lý do dừng: KHÔNG đổi gì. Nơi gọi chờ tiếp.
-- NULL: không phải nút approval của execution này. Gọi lại trả về cùng kết quả.
-- p_stop là gợi ý của nơi chạy kế hoạch và chỉ có thể làm cổng ĐÓNG (huỷ), không mở.
-- +goose StatementBegin
CREATE FUNCTION plan_step_close_approval(p_execution uuid, p_key text, p_stop text) RETURNS text
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $fn$
DECLARE
  org      uuid := NULLIF(current_setting('app.org_id', true), '')::uuid;
  ex       record;
  st       record;
  ap       record;
  has_ap   boolean;
  live     boolean;
  stopping text;
  final    text;
BEGIN
  IF org IS NULL THEN
    RAISE EXCEPTION 'plan_step_close_approval: transaction chưa đặt tenant' USING ERRCODE = 'insufficient_privilege';
  END IF;
  -- Thứ tự khoá: execution → nút → approval, như mọi bước của kế hoạch.
  SELECT status, cancel_requested_at, deadline_at INTO ex
  FROM public.agent_executions WHERE org_id = org AND id = p_execution FOR NO KEY UPDATE;
  IF NOT FOUND THEN
    RETURN NULL;
  END IF;
  SELECT kind, status INTO st FROM public.execution_plan_steps
  WHERE org_id = org AND execution_id = p_execution AND plan_key = p_key FOR UPDATE;
  IF NOT FOUND OR st.kind <> 'approval' THEN
    RETURN NULL;
  END IF;
  IF st.status NOT IN ('queued','running') THEN
    RETURN st.status; -- đã đóng: cùng kết quả
  END IF;
  live := ex.status IN ('running','waiting_approval','paused') AND ex.cancel_requested_at IS NULL AND ex.deadline_at > now();
  stopping := CASE WHEN NOT live AND ex.deadline_at <= now() THEN 'timed_out'
                   WHEN NOT live THEN 'cancelled'
                   WHEN p_stop IN ('cancelled','timed_out') THEN p_stop END;
  SELECT id, project_id, status, (expires_at <= now()) AS expired, requested_by_type, requested_by INTO ap
  FROM public.approvals WHERE org_id = org AND execution_id = p_execution AND plan_key = p_key FOR UPDATE;
  has_ap := FOUND;
  IF has_ap AND ap.status = 'consumed' THEN
    final := 'completed';
  ELSIF has_ap AND ap.status = 'approved' AND live AND st.status = 'running' THEN
    UPDATE public.approvals SET status = 'consumed', resolved_at = now() WHERE org_id = org AND id = ap.id;
    PERFORM public.audit_append(org, ap.project_id, ap.requested_by_type, ap.requested_by, NULL, 'approval.consume', 'approval', ap.id,
      'success', p_execution, jsonb_build_object('subject_type', 'workflow_step', 'plan_key', p_key), NULL, NULL);
    final := 'completed';
  ELSIF has_ap AND ap.status = 'rejected' THEN
    final := 'failed';
  ELSIF has_ap AND ap.status = 'expired' THEN
    final := 'failed';
  ELSIF has_ap AND ap.status = 'pending' AND ap.expired THEN
    UPDATE public.approvals SET status = 'expired', resolved_at = now() WHERE org_id = org AND id = ap.id;
    final := 'failed';
  ELSIF stopping IS NULL THEN
    IF has_ap AND ap.status = 'cancelled' THEN
      final := 'cancelled'; -- approval đã bị huỷ ở nơi khác (execution bị huỷ)
    ELSE
      RETURN 'pending';
    END IF;
  ELSE
    IF has_ap AND ap.status IN ('pending','approved') THEN
      UPDATE public.approvals SET status = 'cancelled', resolved_at = now() WHERE org_id = org AND id = ap.id;
    END IF;
    final := stopping;
  END IF;
  UPDATE public.execution_plan_steps SET status = final, finished_at = now()
  WHERE org_id = org AND execution_id = p_execution AND plan_key = p_key;
  RETURN final;
END
$fn$;
-- +goose StatementEnd

-- plan_close_open_approvals huỷ approval còn mở của mọi nút approval của một execution
-- đang được đóng (kế hoạch kết thúc, execution mồ côi, execution không khởi chạy được):
-- không cổng nào của một execution đã đóng còn nhận quyết định. Trả về số approval đã huỷ.
-- +goose StatementBegin
CREATE FUNCTION plan_close_open_approvals(p_execution uuid) RETURNS int
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $fn$
DECLARE
  org uuid := NULLIF(current_setting('app.org_id', true), '')::uuid;
  n   int;
BEGIN
  IF org IS NULL THEN
    RAISE EXCEPTION 'plan_close_open_approvals: transaction chưa đặt tenant' USING ERRCODE = 'insufficient_privilege';
  END IF;
  UPDATE public.approvals SET status = 'cancelled', resolved_at = now()
  WHERE org_id = org AND execution_id = p_execution AND plan_key IS NOT NULL AND status IN ('pending','approved');
  GET DIAGNOSTICS n = ROW_COUNT;
  RETURN n;
END
$fn$;
-- +goose StatementEnd

REVOKE ALL ON FUNCTION plan_node_gates_open(uuid, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION agent_task_gates_open(uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION plan_step_request_approval(uuid, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION plan_step_approval_status(uuid, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION plan_step_close_approval(uuid, text, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION plan_close_open_approvals(uuid) FROM PUBLIC;
-- app_api cần các hàm này ở chế độ chạy kế hoạch trong process (local, test) và khi đóng
-- execution mồ côi. Gateway chỉ cần biết cổng của một task đã mở hay chưa.
GRANT EXECUTE ON FUNCTION plan_node_gates_open(uuid, text) TO app_worker, app_api;
GRANT EXECUTE ON FUNCTION agent_task_gates_open(uuid) TO app_worker, app_api, app_gateway;
GRANT EXECUTE ON FUNCTION plan_step_request_approval(uuid, text) TO app_worker, app_api;
GRANT EXECUTE ON FUNCTION plan_step_approval_status(uuid, text) TO app_worker, app_api;
GRANT EXECUTE ON FUNCTION plan_step_close_approval(uuid, text, text) TO app_worker, app_api;
GRANT EXECUTE ON FUNCTION plan_close_open_approvals(uuid) TO app_worker, app_api;

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP FUNCTION plan_close_open_approvals(uuid);
DROP FUNCTION plan_step_close_approval(uuid, text, text);
DROP FUNCTION plan_step_approval_status(uuid, text);
DROP FUNCTION plan_step_request_approval(uuid, text);
DROP FUNCTION agent_task_gates_open(uuid);
DROP FUNCTION plan_node_gates_open(uuid, text);
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION execution_plan_steps_guard() RETURNS trigger
LANGUAGE plpgsql AS $fn$
BEGIN
  IF NEW.kind IS DISTINCT FROM OLD.kind OR NEW.config IS DISTINCT FROM OLD.config
     OR NEW.plan_key IS DISTINCT FROM OLD.plan_key OR NEW.execution_id IS DISTINCT FROM OLD.execution_id
     OR NEW.org_id IS DISTINCT FROM OLD.org_id OR NEW.created_at IS DISTINCT FROM OLD.created_at
     OR (OLD.started_at IS NOT NULL AND NEW.started_at IS DISTINCT FROM OLD.started_at) THEN
    RAISE EXCEPTION 'execution_plan_steps: cấu hình của nút % không đổi được', OLD.plan_key USING ERRCODE = 'restrict_violation';
  END IF;
  IF OLD.status IN ('completed','failed','cancelled','timed_out','skipped') OR NEW.status = 'queued' THEN
    RAISE EXCEPTION 'execution_plan_steps: nút % đã kết thúc hoặc không quay lại queued', OLD.plan_key USING ERRCODE = 'restrict_violation';
  END IF;
  -- "completed" là điều mở các bước phụ thuộc, nên database tự giữ điều kiện của nó, với
  -- mọi role: nút chờ phải đã chạy và đã chờ đủ (trừ 2 giây cho độ lệch đồng hồ của nơi
  -- chạy kế hoạch). Nút approval chỉ hoàn thành được qua approval của nó (00033).
  IF NEW.status = 'completed' THEN
    IF OLD.kind = 'wait' AND (OLD.status <> 'running' OR OLD.started_at IS NULL
         OR OLD.started_at > now() - make_interval(secs => GREATEST((OLD.config->>'seconds')::float8 - 2, 0))) THEN
      RAISE EXCEPTION 'execution_plan_steps: nút chờ % chưa chờ đủ', OLD.plan_key USING ERRCODE = 'restrict_violation';
    END IF;
    IF OLD.kind = 'approval' THEN
      RAISE EXCEPTION 'execution_plan_steps: nút approval % chỉ hoàn thành qua approval của nó', OLD.plan_key USING ERRCODE = 'restrict_violation';
    END IF;
  END IF;
  RETURN NEW;
END
$fn$;
-- +goose StatementEnd
-- Hàm guard trở lại bản của 00029 (không SECURITY DEFINER).
ALTER FUNCTION execution_plan_steps_guard() SECURITY INVOKER;
ALTER FUNCTION execution_plan_steps_guard() RESET search_path;
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION approvals_guard() RETURNS trigger
LANGUAGE plpgsql AS $fn$
DECLARE
  last jsonb;
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.status <> 'pending' OR NEW.decisions <> '[]'::jsonb OR NEW.resolved_at IS NOT NULL THEN
      RAISE EXCEPTION 'approvals: approval mới phải ở trạng thái pending, chưa có quyết định' USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
  END IF;
  IF (to_jsonb(NEW) - ARRAY['status','decisions','resolved_at']) <> (to_jsonb(OLD) - ARRAY['status','decisions','resolved_at']) THEN
    RAISE EXCEPTION 'approvals: nội dung của approval % không đổi được', OLD.id USING ERRCODE = 'restrict_violation';
  END IF;
  IF NEW.status <> OLD.status AND NOT (
       (OLD.status = 'pending'  AND NEW.status IN ('approved','rejected','expired','cancelled'))
    OR (OLD.status = 'approved' AND NEW.status IN ('consumed','expired','cancelled'))) THEN
    RAISE EXCEPTION 'approvals: % không chuyển được từ % sang %', OLD.id, OLD.status, NEW.status USING ERRCODE = 'restrict_violation';
  END IF;
  IF NEW.status = OLD.status AND NEW.resolved_at IS DISTINCT FROM OLD.resolved_at THEN
    RAISE EXCEPTION 'approvals: resolved_at của % chỉ đổi cùng một lần chuyển trạng thái', OLD.id USING ERRCODE = 'restrict_violation';
  END IF;
  IF NEW.decisions <> OLD.decisions THEN
    -- Nối thêm đúng một phần tử vào cuối: bỏ phần tử cuối của bản mới phải ra đúng bản cũ.
    IF OLD.status <> 'pending' OR jsonb_array_length(NEW.decisions) <> jsonb_array_length(OLD.decisions) + 1
       OR (NEW.decisions - (-1)) <> OLD.decisions THEN
      RAISE EXCEPTION 'approvals: quyết định của % chỉ được nối thêm khi còn pending', OLD.id USING ERRCODE = 'restrict_violation';
    END IF;
    last := NEW.decisions -> -1;
    IF last->>'user_id' IS NULL OR last->>'decision' NOT IN ('approve','reject')
       OR last->>'user_id' = OLD.requested_by::text OR last->>'user_id' = OLD.requested_by_owner::text THEN
      RAISE EXCEPTION 'approvals: quyết định của % không hợp lệ, hoặc là của chính người yêu cầu', OLD.id USING ERRCODE = 'restrict_violation';
    END IF;
  END IF;
  IF OLD.status = 'pending' AND NEW.status IN ('approved','rejected') THEN
    last := NEW.decisions -> -1;
    IF NEW.decisions = OLD.decisions
       OR (NEW.status = 'approved' AND (last->>'decision' <> 'approve' OR jsonb_array_length(NEW.decisions) < NEW.required_approvals))
       OR (NEW.status = 'rejected' AND last->>'decision' <> 'reject') THEN
      RAISE EXCEPTION 'approvals: % không được % mà không có quyết định tương ứng', OLD.id, NEW.status USING ERRCODE = 'restrict_violation';
    END IF;
  END IF;
  RETURN NEW;
END
$fn$;
-- +goose StatementEnd
ALTER TABLE approvals DROP CONSTRAINT approvals_plan_step_fkey;
ALTER TABLE approvals DROP CONSTRAINT approvals_plan_key_check;
ALTER TABLE approvals DROP CONSTRAINT approvals_workflow_step_check;
ALTER TABLE approvals DROP COLUMN excluded_approver;
ALTER TABLE approvals DROP COLUMN plan_key;
