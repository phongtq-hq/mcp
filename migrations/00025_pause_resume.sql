-- +goose Up
-- Pause / resume của execution (agent-runtime §2). Yêu cầu tạm dừng nằm ở execution,
-- như yêu cầu huỷ: vòng lặp của từng task đọc nó ở ranh giới bước (trước mỗi lượt model
-- và mỗi tool call) rồi tự dừng. Trạng thái `paused` của execution và task đã có trong
-- CHECK từ 00008. NULL = không có yêu cầu tạm dừng.
SET LOCAL lock_timeout = '5s';
ALTER TABLE agent_executions ADD COLUMN pause_requested_at TIMESTAMPTZ;
-- Ai đã yêu cầu tạm dừng: người đó tự gỡ được; người khác cần quyền execution:resume.
ALTER TABLE agent_executions ADD COLUMN pause_requested_by_type TEXT;
ALTER TABLE agent_executions ADD COLUMN pause_requested_by UUID;
ALTER TABLE agent_executions ADD CONSTRAINT agent_executions_pause_check
  CHECK ((pause_requested_at IS NULL) = (pause_requested_by IS NULL)
     AND (pause_requested_at IS NULL) = (pause_requested_by_type IS NULL)
     AND (pause_requested_by_type IS NULL OR pause_requested_by_type IN ('user','api_key'))) NOT VALID; -- VALIDATE ở 00026
-- Chỉ process api ghi yêu cầu (đặt khi pause, xoá khi resume). Process chạy vòng lặp chỉ
-- đọc nó: nó không tự tạm dừng hay tự cho mình chạy tiếp được.
GRANT UPDATE (pause_requested_at, pause_requested_by_type, pause_requested_by) ON agent_executions TO app_api;

-- Cho một execution đang bị tạm dừng chạy tiếp là thao tác MỞ lại hoạt động, khác với
-- huỷ và tạm dừng (chỉ giảm): gỡ yêu cầu tạm dừng của người khác cần quyền riêng.
ALTER TABLE roles NO FORCE ROW LEVEL SECURITY;
-- +goose StatementBegin
DO $$
DECLARE
  n int;
BEGIN
  UPDATE roles SET permissions = permissions || ARRAY['execution:resume'], updated_at = now()
  WHERE org_id IS NULL AND name IN ('org_admin','project_admin','operator');
  GET DIAGNOSTICS n = ROW_COUNT;
  IF n <> 3 THEN
    RAISE EXCEPTION 'cập nhật role dựng sẵn tác động % dòng, muốn 3', n;
  END IF;
END
$$;
-- +goose StatementEnd
ALTER TABLE roles FORCE ROW LEVEL SECURITY;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE roles NO FORCE ROW LEVEL SECURITY;
UPDATE roles SET permissions = array_remove(permissions, 'execution:resume'), updated_at = now() WHERE org_id IS NULL;
ALTER TABLE roles FORCE ROW LEVEL SECURITY;
REVOKE UPDATE (pause_requested_at, pause_requested_by_type, pause_requested_by) ON agent_executions FROM app_api;
ALTER TABLE agent_executions DROP CONSTRAINT agent_executions_pause_check;
ALTER TABLE agent_executions DROP COLUMN pause_requested_by;
ALTER TABLE agent_executions DROP COLUMN pause_requested_by_type;
ALTER TABLE agent_executions DROP COLUMN pause_requested_at;
