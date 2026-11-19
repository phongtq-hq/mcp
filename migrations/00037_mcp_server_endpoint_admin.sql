-- +goose Up
-- Migration 00007 thu hồi quyền sửa `endpoint` của app_admin vì khi đó không có đường nào
-- đổi địa chỉ mà để lại dấu vết: "đổi endpoint chuyển mọi tool đã duyệt sang host khác mà
-- không tạo version mới; khi cần, nó phải đi qua một đường có audit".
--
-- Đường đó nay có: `hqadmin mcp-server set-endpoint` (adminops.SetManagedMCPServerEndpoint).
-- Nó chỉ nhận managed server, chạy discovery ở địa chỉ mới bằng ngữ cảnh ký của đúng server
-- đó trước khi ghi, áp luật của discovery cho định nghĩa tìm thấy (khác đi → version chờ
-- duyệt, tool bị khoá) và ghi audit `mcp_server.set_endpoint` kèm địa chỉ cũ và mới trong
-- cùng transaction. Chỉ cấp lại đúng cột này, cho đúng role của đường quản trị.
SET LOCAL lock_timeout = '5s';
GRANT UPDATE (endpoint) ON mcp_servers TO app_admin;

-- +goose Down
SET LOCAL lock_timeout = '5s';
REVOKE UPDATE (endpoint) ON mcp_servers FROM app_admin;
