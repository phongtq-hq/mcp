-- +goose Up
-- Phase 3: thử lại theo bước của workflow (agent-runtime §5.12).
--
-- Một lần thử lại là một task MỚI: khoá idempotency của tool (task:step_seq) và chỗ giữ ở
-- Gateway (mcp_tool_claims) không khử trùng được giữa hai lần thử. Vì vậy một bước chỉ
-- được thử lại khi lần thử trước CHẮC CHẮN chưa gây tác động: không tool không-chỉ-đọc
-- nào được Gateway giữ chỗ cho nó (kể cả tool có kết quả không xác định), không step
-- `unknown`, và nó chưa giao việc cho task con (task con có thể đã ghi).
--
-- Điểm quyết định nằm ở database, không ở nơi chạy kế hoạch: chỗ giữ do Gateway ghi và
-- worker không đọc hay sửa được bảng đó. Hàm chỉ trả lời có / không cho một task của
-- tenant hiện tại.
SET LOCAL lock_timeout = '5s';

-- +goose StatementBegin
CREATE FUNCTION agent_task_retry_safe(p_task uuid) RETURNS boolean
LANGUAGE sql STABLE SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $fn$
  SELECT COALESCE((
    SELECT NOT EXISTS (SELECT 1 FROM mcp_tool_claims c WHERE c.org_id = t.org_id AND c.task_id = t.id)
       AND NOT EXISTS (SELECT 1 FROM agent_tasks ch WHERE ch.org_id = t.org_id AND ch.parent_task_id = t.id)
       AND NOT EXISTS (SELECT 1 FROM execution_steps s
                       WHERE s.org_id = t.org_id AND s.task_id = t.id AND (s.status = 'unknown' OR s.kind = 'delegation'))
    FROM agent_tasks t
    WHERE t.id = p_task AND t.org_id = NULLIF(current_setting('app.org_id', true), '')::uuid
  ), false)
$fn$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION agent_task_retry_safe(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION agent_task_retry_safe(uuid) TO app_api, app_worker;

-- +goose Down
DROP FUNCTION agent_task_retry_safe(uuid);
