-- +goose Up
-- Phân biệt tool bị Registry TẠM KHOÁ (định nghĩa đổi ở phía server, chờ duyệt) với tool
-- người vận hành chủ động TẮT. Cả hai đều có enabled = false; trước đây lần discovery kế
-- tiếp không phân biệt được nên tool đã tắt của managed server được bật lại lặng lẽ khi
-- định nghĩa đổi (review bảo mật 2026-11-25). locked_at đặt khi khoá, xoá khi người vận
-- hành bật lại tool; version mới kế thừa "đang bật" khi enabled OR locked_at IS NOT NULL.
SET LOCAL lock_timeout = '5s';

ALTER TABLE mcp_tools ADD COLUMN locked_at TIMESTAMPTZ;

GRANT UPDATE (locked_at) ON mcp_tools TO app_admin, app_api;

-- +goose Down
SET LOCAL lock_timeout = '5s';
REVOKE UPDATE (locked_at) ON mcp_tools FROM app_admin, app_api;
ALTER TABLE mcp_tools DROP COLUMN locked_at;
