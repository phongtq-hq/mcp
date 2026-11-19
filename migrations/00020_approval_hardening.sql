-- +goose Up
-- Siết lại sau khi review lát Approval (00019).
SET LOCAL lock_timeout = '5s';

-- Người yêu cầu là con người đứng sau execution: với execution do API key khởi tạo, đó
-- là người tạo key. Người này không được tự duyệt (security §4.4).
ALTER TABLE approvals ADD COLUMN requested_by_owner UUID;

-- Tham chiếu chéo tenant bị database từ chối: approval cha cùng org, task cùng org.
-- Approval và claim sống đúng bằng execution / task của chúng (xoá theo retention của
-- dữ liệu thực thi; dấu vết duyệt nằm trong audit_logs, giữ lâu hơn).
ALTER TABLE approvals DROP CONSTRAINT approvals_parent_approval_id_fkey;
ALTER TABLE approvals ADD CONSTRAINT approvals_parent_fkey
  FOREIGN KEY (org_id, parent_approval_id) REFERENCES approvals (org_id, id);
ALTER TABLE approvals ADD CONSTRAINT approvals_task_fkey
  FOREIGN KEY (org_id, task_id) REFERENCES agent_tasks (org_id, id) ON DELETE CASCADE;
ALTER TABLE mcp_tool_claims ADD CONSTRAINT mcp_tool_claims_task_fkey
  FOREIGN KEY (org_id, task_id) REFERENCES agent_tasks (org_id, id) ON DELETE CASCADE;

-- Index này chưa có truy vấn nào dùng (danh sách sắp theo created_at).
DROP INDEX approvals_pending_idx;

-- Gateway chỉ chèn được các cột mô tả hành động; status, decisions, resolved_at nhận
-- giá trị mặc định (pending, rỗng, NULL).
REVOKE INSERT ON approvals FROM app_gateway;
GRANT INSERT (id, org_id, project_id, execution_id, task_id, step_seq, subject_type, tool_id, tool_name, args_hash, payload,
              risk_level, requested_by_type, requested_by, requested_by_owner, expires_at) ON approvals TO app_gateway;
-- Worker chỉ cần biết approval của task nó đang chờ còn pending hay không.
REVOKE SELECT (execution_id, step_seq) ON approvals FROM app_worker;

-- Vòng đời của approval, với mọi role:
--   * dòng mới luôn pending, chưa có quyết định;
--   * pending → approved cần đúng một quyết định approve mới, của một user không phải
--     người yêu cầu (hay người đứng sau họ), và đủ số quyết định yêu cầu;
--   * pending → rejected cần đúng một quyết định reject mới;
--   * quyết định chỉ được NỐI THÊM vào cuối, khi còn pending;
--   * resolved_at chỉ được đặt cùng lúc với một lần chuyển trạng thái.
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
CREATE TRIGGER approvals_insert BEFORE INSERT ON approvals
  FOR EACH ROW EXECUTE FUNCTION approvals_guard();

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TRIGGER approvals_insert ON approvals;
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION approvals_guard() RETURNS trigger
LANGUAGE plpgsql AS $fn$
BEGIN
  IF (to_jsonb(NEW) - ARRAY['status','decisions','resolved_at']) <> (to_jsonb(OLD) - ARRAY['status','decisions','resolved_at']) THEN
    RAISE EXCEPTION 'approvals: nội dung của approval % không đổi được', OLD.id USING ERRCODE = 'restrict_violation';
  END IF;
  IF NEW.status <> OLD.status AND NOT (
       (OLD.status = 'pending'  AND NEW.status IN ('approved','rejected','expired','cancelled'))
    OR (OLD.status = 'approved' AND NEW.status IN ('consumed','expired','cancelled'))) THEN
    RAISE EXCEPTION 'approvals: % không chuyển được từ % sang %', OLD.id, OLD.status, NEW.status USING ERRCODE = 'restrict_violation';
  END IF;
  IF NEW.decisions <> OLD.decisions AND (OLD.status <> 'pending'
       OR jsonb_array_length(NEW.decisions) <> jsonb_array_length(OLD.decisions) + 1
       OR NOT (NEW.decisions @> OLD.decisions)) THEN
    RAISE EXCEPTION 'approvals: quyết định của % chỉ được thêm khi còn pending', OLD.id USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END
$fn$;
-- +goose StatementEnd
GRANT SELECT (execution_id, step_seq) ON approvals TO app_worker;
REVOKE INSERT (id, org_id, project_id, execution_id, task_id, step_seq, subject_type, tool_id, tool_name, args_hash, payload,
               risk_level, requested_by_type, requested_by, requested_by_owner, expires_at) ON approvals FROM app_gateway;
GRANT INSERT ON approvals TO app_gateway;
CREATE INDEX approvals_pending_idx ON approvals (org_id, project_id, expires_at) WHERE status = 'pending';
ALTER TABLE mcp_tool_claims DROP CONSTRAINT mcp_tool_claims_task_fkey;
ALTER TABLE approvals DROP CONSTRAINT approvals_task_fkey;
ALTER TABLE approvals DROP CONSTRAINT approvals_parent_fkey;
ALTER TABLE approvals ADD CONSTRAINT approvals_parent_approval_id_fkey FOREIGN KEY (parent_approval_id) REFERENCES approvals(id);
ALTER TABLE approvals DROP COLUMN requested_by_owner;
