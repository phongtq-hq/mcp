-- name: ListGrantsForPrincipal :many
-- Binding có điều kiện ABAC (Phase 3) chưa được đánh giá nên bị loại: an toàn theo hướng từ chối.
SELECT r.name AS role_name, r.permissions, b.project_id
FROM role_bindings b
-- Điều kiện org trên roles là lớp thứ hai bên cạnh RLS và khoá ngoại ghép.
JOIN roles r ON r.id = b.role_id AND (r.org_id = b.org_id OR r.org_id IS NULL)
WHERE b.org_id = @org_id
  AND b.principal_type = @principal_type
  AND b.principal_id = @principal_id
  AND b.condition IS NULL
ORDER BY b.id
LIMIT 500;
