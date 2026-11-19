-- +goose Up
-- Quét các dòng đã có cho ràng buộc thêm ở 00015. VALIDATE chỉ cần khoá
-- SHARE UPDATE EXCLUSIVE, nên không chặn đọc / ghi task trong lúc quét.
SET LOCAL lock_timeout = '5s';
ALTER TABLE agent_tasks VALIDATE CONSTRAINT agent_tasks_model_chain_check;

-- +goose Down
SELECT 1;
