-- +goose Up
-- Phase 2: Model Router (ADR-0010). Chuỗi model của một task (model chính và các model
-- dự phòng, theo thứ tự) được chọn lúc tạo task và ghi lại ở đây: cấu hình của một
-- execution cố định khi bắt đầu, và Gateway chỉ phục vụ đúng các model trong chuỗi.
SET LOCAL lock_timeout = '5s';

-- NULL với task tạo trước migration này (Gateway khi đó dùng model được ghim của
-- Agent Version như trước). Không có khoá ngoại tới models (mảng): model đã có giá thì
-- không xoá được, và Gateway resolve lại từng id dưới RLS ở mỗi lượt gọi.
ALTER TABLE agent_tasks ADD COLUMN model_chain UUID[];
-- NOT VALID: ràng buộc áp cho dòng mới ngay; việc quét dòng cũ (VALIDATE) nằm ở
-- migration 00016 để không chạy dưới khoá ACCESS EXCLUSIVE của ADD COLUMN.
ALTER TABLE agent_tasks ADD CONSTRAINT agent_tasks_model_chain_check
  CHECK (model_chain IS NULL OR (array_ndims(model_chain) = 1 AND cardinality(model_chain) BETWEEN 1 AND 4
         AND array_position(model_chain, NULL) IS NULL)) NOT VALID;

-- Những gì task chụp lại lúc tạo (chuỗi model, system prompt, yêu cầu, Agent Version)
-- không đổi được sau đó, với mọi role: model_chain là căn cứ Gateway cho phép gọi model.
-- +goose StatementBegin
CREATE FUNCTION agent_tasks_snapshot_guard() RETURNS trigger
LANGUAGE plpgsql AS $fn$
BEGIN
  IF NEW.model_chain IS DISTINCT FROM OLD.model_chain OR NEW.system_prompt IS DISTINCT FROM OLD.system_prompt
     OR NEW.goal IS DISTINCT FROM OLD.goal OR NEW.agent_version_id IS DISTINCT FROM OLD.agent_version_id
     OR NEW.execution_id IS DISTINCT FROM OLD.execution_id THEN
    RAISE EXCEPTION 'agent_tasks: cấu hình đã chụp của task % không đổi được', OLD.id USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END
$fn$;
-- +goose StatementEnd
CREATE TRIGGER agent_tasks_snapshot BEFORE UPDATE ON agent_tasks
  FOR EACH ROW EXECUTE FUNCTION agent_tasks_snapshot_guard();

-- Gateway đọc chuỗi để kiểm model của một lượt gọi. api ghi nó lúc tạo task (INSERT);
-- không role nào có UPDATE trên cột này, kể cả worker.
GRANT SELECT (model_chain) ON agent_tasks TO app_gateway;

-- Truy vấn kiểm model dùng được (dùng chung bởi api và gateway) nay đọc cả khả năng
-- của model, cho ràng buộc cứng của router.
GRANT SELECT (capabilities) ON models TO app_gateway;

-- Lệnh hoàn tất step nay luôn ghi model đã thực sự trả lời (model dự phòng nếu model
-- chính lỗi), nên worker cần quyền này cho MỌI lần hoàn tất step, không chỉ khi có dự
-- phòng. 00013 đã bỏ nó vì lúc đó chưa có truy vấn nào cần. Trigger execution_steps_guard
-- vẫn chỉ cho đổi model_id đúng lúc step chuyển từ started sang kết quả.
GRANT UPDATE (model_id) ON execution_steps TO app_worker;

-- +goose Down
REVOKE UPDATE (model_id) ON execution_steps FROM app_worker;
REVOKE SELECT (capabilities) ON models FROM app_gateway;
REVOKE SELECT (model_chain) ON agent_tasks FROM app_gateway;
DROP TRIGGER agent_tasks_snapshot ON agent_tasks;
DROP FUNCTION agent_tasks_snapshot_guard();
ALTER TABLE agent_tasks DROP COLUMN model_chain;
