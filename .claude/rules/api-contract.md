---
paths:
  - ".docs/api/**"
  - "cmd/api/**"
  - "internal/**/http*/**"
  - "internal/**/*handler*.go"
  - "internal/shared/httpx/**"
---

# REST Management API

Nguồn: `.docs/api/openapi.yaml` (phần `info.description` chứa các quy ước).

- **Spec trước.** Thay đổi hành vi API bắt đầu bằng sửa `openapi.yaml`, rồi `make generate`: kiểu request / response nằm ở `internal/api` (sinh bằng `oapi-codegen`, không sửa tay). Handler dùng kiểu đó ở biên và chuyển sang kiểu miền của context; validate ngữ nghĩa nằm ở service.
- API này tách khỏi endpoint MCP (`gateway`). Không đưa endpoint giao thức MCP vào đây.
- **Tenant:** Organization xác định từ token, không từ đường dẫn hay body. Tài nguyên thuộc project tạo và liệt kê dưới `/projects/{projectId}/...`, truy cập theo id ở đường dẫn gốc.
- **Phiên bản:** tiền tố `/v1`. Thêm trường / endpoint không tăng phiên bản; thay đổi phá tương thích tạo `/v2`.
- **Phân trang:** con trỏ, `limit` (mặc định 50, tối đa 200) + `cursor`, trả `next_cursor`. Không offset.
- **Sắp xếp:** `sort=<trường>` hoặc `sort=-<trường>`, chỉ trên trường được liệt kê.
- **Idempotency:** mọi `POST` tạo tài nguyên hoặc kích hoạt hành động nhận `Idempotency-Key`. Cùng key cùng body trả phản hồi ban đầu; cùng key khác body trả 422. Bắt buộc với tạo execution, tạo workflow execution, quyết định approval.
- **Đồng thời:** tài nguyên sửa được trả `ETag`; `PATCH` / `DELETE` nhận `If-Match`, lệch trả 412.
- **Lỗi:** `application/problem+json` (RFC 9457) với `code` ổn định. 400 sai cú pháp · 401 · 403 · 404 (kể cả tài nguyên tenant khác) · 409 xung đột trạng thái · 412 · 422 sai ngữ nghĩa · 402 vượt budget · 429 kèm `Retry-After`.
- Tạo execution trả **202** + `execution_id`, kể cả ở Phase 1 khi vòng lặp chạy trong process `api`.
- Phản hồi không bao giờ chứa plaintext secret; API key chỉ hiển thị một lần lúc tạo.
- Mỗi endpoint mới có: kiểm tra quyền qua `authz` (`Require`), `audit.Record` trong cùng transaction nếu thay đổi cấu hình, và **test cross-tenant** trong `tests/api`.
- Tài nguyên theo id: đọc trong tenant trước, rồi kiểm quyền đọc; không đọc được thì 404, đọc được mà thiếu quyền cho hành động thì 403.
- `operationId` dạng camelCase động từ + danh từ (`listProjects`, `createAgentVersion`); mỗi operation có `tags` và response `default` trỏ `Problem`.
