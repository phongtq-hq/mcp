-- name: ClaimToolCall :execrows
-- Giữ quyền thực thi một tool call không chỉ đọc cho đúng một bước của một task. 0 dòng
-- nghĩa là bước này đã được giữ từ trước (lần gọi trước có thể đã thực thi).
INSERT INTO mcp_tool_claims (org_id, task_id, step_seq, tool_id, args_hash)
VALUES (@org_id, @task_id, @step_seq, @tool_id, @args_hash)
ON CONFLICT (org_id, task_id, step_seq) DO NOTHING;

-- name: GetToolClaim :one
SELECT tool_id, args_hash FROM mcp_tool_claims WHERE org_id = @org_id AND task_id = @task_id AND step_seq = @step_seq;

-- name: CountToolClaims :one
-- Số bước của task mà Gateway đã cho chạy một tool có giữ chỗ.
SELECT count(*) FROM mcp_tool_claims WHERE org_id = @org_id AND task_id = @task_id;

-- name: MarkSensitiveRead :exec
-- Execution sắp đọc một tool sensitive (migration 00040). Ghi trước khi gọi upstream.
INSERT INTO mcp_execution_sensitive_reads (org_id, execution_id, first_tool_id)
VALUES (@org_id, @execution_id, @tool_id)
ON CONFLICT (org_id, execution_id) DO NOTHING;

-- name: HasSensitiveRead :one
SELECT EXISTS (SELECT 1 FROM mcp_execution_sensitive_reads WHERE org_id = @org_id AND execution_id = @execution_id);
