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
