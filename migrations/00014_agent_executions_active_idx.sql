-- +goose NO TRANSACTION
-- +goose Up
-- Trần execution đồng thời theo org được đếm qua index này (schema.md đã mô tả nó;
-- tới Phase 2 mới có truy vấn dùng). CONCURRENTLY để không chặn ghi trên bảng đang có
-- dữ liệu; vì vậy migration này chạy ngoài transaction.
CREATE INDEX CONCURRENTLY IF NOT EXISTS agent_executions_active_idx ON agent_executions (org_id, status)
  WHERE status IN ('queued','running','waiting_approval','paused');

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS agent_executions_active_idx;
