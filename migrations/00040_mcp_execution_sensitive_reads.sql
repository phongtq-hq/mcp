-- +goose Up
-- Kiểm soát luồng dữ liệu (security §5.4, T6): execution đã đọc một tool có
-- data_sensitivity = 'sensitive' mà gọi một tool egress (tool khai egress, hoặc mọi tool
-- của MCP server ngoài) thì lời gọi đó cần người duyệt. Gateway ghi một dòng TRƯỚC khi gọi
-- upstream của tool sensitive (không ghi được thì không gọi), và đọc nó khi quyết định
-- cho tool egress. Theo execution chứ không theo task: dữ liệu đi giữa các task của cùng
-- execution (output_summary của kế hoạch, kết quả của task con khi delegation).
SET LOCAL lock_timeout = '5s';

CREATE TABLE mcp_execution_sensitive_reads (
  org_id       UUID NOT NULL REFERENCES organizations(id),
  execution_id UUID NOT NULL,
  -- Tool sensitive đầu tiên mà execution đã đọc (để audit và người duyệt biết vì sao).
  first_tool_id UUID NOT NULL,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (org_id, execution_id),
  FOREIGN KEY (org_id, execution_id) REFERENCES agent_executions (org_id, id) ON DELETE CASCADE
);
ALTER TABLE mcp_execution_sensitive_reads ENABLE ROW LEVEL SECURITY;
ALTER TABLE mcp_execution_sensitive_reads FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON mcp_execution_sensitive_reads
  USING      (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid)
  WITH CHECK (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid);
-- Chỉ thêm: một execution đã đọc dữ liệu nhạy cảm thì không "quên" được điều đó.
GRANT SELECT, INSERT ON mcp_execution_sensitive_reads TO app_gateway;
GRANT SELECT ON mcp_execution_sensitive_reads TO app_admin;

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TABLE mcp_execution_sensitive_reads;
