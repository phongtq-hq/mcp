---
paths:
  - "migrations/**"
  - "**/*.sql"
  - "**/sqlc.yaml"
  - "internal/**/store/**"
  - "internal/shared/pgx/**"
  - ".docs/database/**"
---

# PostgreSQL: schema, tenant, migration

Nguồn: `.docs/database/schema.md` · ADR-0005, ADR-0007.

## Quy ước bảng

- Khoá chính `UUID` v7. Thời gian `TIMESTAMPTZ` UTC; `created_at` mọi bảng, `updated_at` cho bảng sửa được.
- Trạng thái là `TEXT` + `CHECK`, không dùng enum.
- Tiền `NUMERIC(18,8)` + `currency CHAR(3)`.
- JSONB chỉ cho dữ liệu có cấu trúc thay đổi theo loại. Trường dùng để lọc hoặc join là cột thật.
- Cấu hình xoá mềm bằng `archived_at`; dữ liệu thực thi xoá theo retention bằng `DROP PARTITION`, không `DELETE` hàng loạt.
- Không tạo bảng `permissions`, `mcp_resources`, `mcp_prompts`: đã quyết định không tách (schema §2).

## Cách ly tenant (bắt buộc cho mọi bảng thuộc tenant)

```sql
ALTER TABLE t ENABLE ROW LEVEL SECURITY;
ALTER TABLE t FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON t
  USING      (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid)
  WITH CHECK (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid);
```

- Cột `org_id` có mặt kể cả khi suy ra được qua khoá ngoại.
- Khoá ngoại ghép `(org_id, …)` ở quan hệ cha–con chính, để DB từ chối tham chiếu chéo tenant.
- Index trên bảng tenant **bắt đầu bằng `org_id`**.
- Bảng có dòng dùng chung của platform (`org_id IS NULL`) cần policy riêng cho đọc và ghi; đừng dùng lại policy mặc định.
- Mỗi transaction chạy `SET LOCAL app.org_id` ngay sau `BEGIN`. Truy vấn ngoài transaction đã đặt tenant là lỗi.
- Role `app_api`, `app_gateway`, `app_worker` không có `BYPASSRLS`. `app_admin` chỉ dùng cho job xuyên tenant, trong đường code riêng.
- Bảng tenant mới phải được thêm vào bộ test cross-tenant.

## Bảng chỉ thêm

`usage_records`, `audit_logs`, `agent_versions` đã publish, `model_pricing` đã hiệu lực: không `UPDATE`, không `DELETE`. Sửa sai bằng bản ghi điều chỉnh hoặc dòng mới. `audit_logs`: role ứng dụng chỉ có `INSERT` và `SELECT`, mỗi dòng chứa hash dòng trước trong cùng org.

## Index và truy vấn

- Chỉ tạo index cho truy vấn đã biết. Phân trang keyset trên `(created_at, id)`, không `OFFSET`.
- Không join xuyên bounded context (trừ báo cáo Cost, Audit).

## Migration (`goose`, SQL thuần)

- Chạy như bước riêng trước deploy, không tự chạy khi process khởi động. Production chỉ tiến; vẫn viết `down` cho phát triển.
- **Không sửa migration đã merge.** Viết migration mới.
- Thay đổi phá tương thích theo expand → migrate → contract qua ít nhất hai lần deploy.
- An toàn khoá: đặt `lock_timeout`; `CREATE INDEX CONCURRENTLY` (tách khỏi transaction); khoá ngoại `NOT VALID` rồi `VALIDATE`; cột mới nullable hoặc có default không gây rewrite.
- Sau khi đổi schema: sinh lại `sqlc`, cập nhật `.docs/database/schema.md` trong cùng thay đổi.
