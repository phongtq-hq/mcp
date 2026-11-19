---
paths:
  - "internal/mcp/**"
  - "cmd/gateway/**"
---

# MCP Gateway và Registry

Nguồn: `.docs/architecture/mcp-platform.md` · ADR-0004.

## Giao thức

- SDK: `github.com/modelcontextprotocol/go-sdk`. Mục tiêu: MCP `2026-07-28`; tương thích ngược phía upstream với `2025-11-25`, `2025-06-18`, `2025-03-26`.
- Các chi tiết giao thức ghi trong thiết kế (stateless, header `Mcp-Method` / `Mcp-Name`, `ttlMs` / `cacheScope`, Client ID Metadata Documents, `subscriptions/listen`) **phải đối chiếu với đặc tả chính thức và mã nguồn SDK trước khi hiện thực**. Đặc tả thay đổi nhanh; không code theo trí nhớ.
- Không thiết kế gì dựa trên session MCP. Trạng thái nhiều lượt (chờ approval) thuộc execution của platform.
- Mọi phụ thuộc vào giao thức nằm trong `internal/mcp`.

## Pipeline `tools/call`: đủ 8 bước, đúng thứ tự

1. AuthN → 2. Tenant + rate limit → 3. Resolve tool → 4. Validate input → 5. Policy + approval → 6. Credential → 7. Upstream call → 8. Lọc kết quả + audit + usage.

- Không có đường tắt bỏ bước, kể cả cho Agent nội bộ hay `direct_tool`.
- Bước 3: tool ngoài tập được phép của principal thì **không tồn tại** với họ (không lộ qua lỗi khác biệt).
- Bước 4: validate theo JSON Schema **đã ghim**, không theo schema upstream trả về lúc gọi.
- Bước 5: Gateway luôn kiểm tra lại, không tin Runtime đã kiểm tra.
- Bước 6: lỗi credential trả 5xx, không lộ chi tiết.
- Bước 7: timeout, circuit breaker, propagate trace context; URL ngoài qua SSRF guard mỗi lần gọi.
- Bước 8: cắt theo `max_result_bytes`, che secret, ghi audit (hash arguments, quyết định policy, approval liên quan, kết quả, thời lượng).

## Credential

- **Không chuyển tiếp token của client xuống upstream.** Gateway dùng credential riêng cho từng upstream, theo org hoặc user.
- Kiểm tra `aud` của token đúng là Gateway.
- Execution token: JWT ngắn hạn, claim `org`, `project`, `execution_id`, `task_id`, `agent_version_id`, `initiator`; hết hạn theo deadline của task.

## Gateway

- Stateless, scale ngang, không sticky. Không chứa logic nghiệp vụ.
- `tools/list` cho Agent nội bộ trả đúng tập tool của Agent Version ∩ quyền người khởi tạo.
- Nằm trên đường nóng: đo độ trễ thêm từ Phase 1.

## Registry

- Tool mới phát hiện mặc định `risk_level = destructive`, `side_effect = non_idempotent`, `enabled = false` cho tới khi admin phân loại. Annotation của server chỉ là gợi ý.
- Discovery tính hash trên `name + description + inputSchema`. Thay đổi → `mcp_server_version` mới ở `pending_review`; tool bị đổi **tạm khoá** tới khi duyệt.
- `mcp_server_versions` là ảnh chụp bất biến.
- Health là dữ liệu vận hành (Redis, phản chiếu về Postgres); không dùng làm điều kiện phân quyền.
- STDIO bridge: chỉ platform admin, image đã duyệt, một process cho mỗi cặp (server, org). Phase 4, không làm sớm.
