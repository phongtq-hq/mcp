---
name: add-endpoint
description: Thêm hoặc sửa một endpoint REST của Management API theo quy trình spec-first — sửa openapi.yaml, sinh kiểu, viết handler có authz, audit, idempotency, ETag, lỗi RFC 9457, và test cross-tenant bắt buộc. Dùng khi hiện thực một endpoint đã có trong openapi.yaml hoặc khi thêm endpoint mới.
argument-hint: <method và đường dẫn, hoặc operationId>
---

# Thêm endpoint Management API

Endpoint: $ARGUMENTS

Đây là API quản trị trên process `api`. Endpoint giao thức MCP thuộc `gateway` và không đi theo quy trình này.

## 1. Spec trước

Mở `.docs/api/openapi.yaml`. Đọc `info.description` (quy ước chung) và một operation tương tự đã có.

- Endpoint đã có trong spec: dùng đúng những gì spec nói. Thấy spec sai hoặc thiếu thì sửa spec trước, nói rõ đã sửa gì.
- Endpoint mới: thêm vào spec theo khuôn hiện có.
  - `operationId` camelCase, `tags`, response `default` trỏ `#/components/responses/Problem`.
  - Tài nguyên thuộc project: tạo và liệt kê dưới `/projects/{projectId}/...`, truy cập theo id ở đường dẫn gốc. Không có `org_id` trong đường dẫn hay body.
  - Danh sách: `Limit`, `Cursor`, `sort` với enum trường cho phép; trả `next_cursor`.
  - `POST` tạo hoặc kích hoạt: tham số `Idempotency-Key`.
  - `PATCH` / `DELETE`: `If-Match`; tài nguyên trả `ETag`.
  - Dùng lại schema trong `components`; không khai báo trùng.
- Thay đổi phá tương thích trên `/v1` không được phép: thêm trường thì được, đổi nghĩa hoặc bỏ trường thì không.

## 2. Sinh kiểu và validate

Sinh lại kiểu request / response và validate ở biên từ spec bằng generator dự án đang dùng (xem Makefile; nếu chưa chọn generator, hỏi người dùng thay vì tự chọn). Không viết tay kiểu trùng với spec.

## 3. Handler

Theo thứ tự:

1. Tenant và principal lấy từ context (middleware đã xác lập). Không đọc `org_id` từ input.
2. Validate input theo schema; giới hạn kích thước body.
3. Kiểm tra quyền bằng `authz.Decide` với permission dạng `resource:action`. Permission mới: thêm vào danh mục hằng số trong code và vào role dựng sẵn phù hợp.
4. Idempotency: tra `idempotency_keys`; cùng key cùng body trả phản hồi cũ, cùng key khác body trả 422.
5. Gọi service của bounded context sở hữu tài nguyên, qua interface khai báo ở bên dùng. Handler không chứa logic nghiệp vụ và không chạm `store` của context khác.
6. Mọi truy vấn chạy trong transaction đã `SET LOCAL app.org_id`.
7. Thay đổi cấu hình, quyền, budget, secret: ghi `audit_logs` trong cùng transaction. Cần phát sự kiện: ghi `outbox` trong cùng transaction.
8. Lỗi trả `application/problem+json` với `code` ổn định. Tài nguyên không tồn tại **hoặc thuộc tenant khác**: 404. Xung đột trạng thái: 409. ETag lệch: 412. Vượt budget: 402.
9. Phản hồi không chứa plaintext secret.

Tạo execution trả 202 + `execution_id`.

## 4. Test

- **Cross-tenant (bắt buộc):** hai org; org B đọc, sửa, xoá tài nguyên của org A nhận 404; danh sách của B không chứa dòng của A.
- Quyền: principal thiếu permission nhận 403.
- Idempotency, ETag, phân trang (trang cuối `next_cursor: null`), validate (400 / 422).
- Chạy trên Postgres thật bằng role ứng dụng.

## 5. Hoàn tất

Chạy build, lint, test; báo kết quả thật. Với endpoint chạm quyền, secret, approval hoặc execution, gọi agent `security-reviewer`.
