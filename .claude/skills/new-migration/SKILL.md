---
name: new-migration
description: Viết một migration PostgreSQL (goose, SQL thuần) cho MCP Platform đúng quy ước — org_id + RLS, khoá ngoại ghép, an toàn khoá, expand/contract — rồi cập nhật truy vấn sqlc và schema.md. Dùng khi thêm bảng, thêm cột, thêm index, đổi ràng buộc, hoặc bất kỳ thay đổi schema nào.
argument-hint: <mô tả thay đổi schema>
---

# Viết migration

Thay đổi: $ARGUMENTS

## Trước khi viết

1. Đọc mục liên quan trong `.docs/database/schema.md` (§1 quy ước, DDL của bảng chạm tới ở §4, §5 RLS, §8 migration). DDL ở đó là thiết kế, chưa phải migration: dùng làm đích, nhưng kiểm tra lại từng ràng buộc.
2. Xem `migrations/` để lấy quy ước đặt tên và số thứ tự đang dùng, và đọc migration gần nhất để bám phong cách. Nếu thư mục chưa tồn tại, đây là migration đầu tiên: hỏi người dùng về quy ước đặt tên nếu chưa rõ, và kiểm tra `goose` đã được cài và cấu hình chưa.
3. Xác định bảng thuộc bounded context nào (overview §2) và có thuộc tenant không.

## Viết

Tạo **migration mới**; không sửa migration đã tồn tại. Có cả `-- +goose Up` và `-- +goose Down`.

**Bảng mới thuộc tenant** phải có đủ:

```sql
CREATE TABLE things (
  id         UUID PRIMARY KEY DEFAULT uuidv7(),
  org_id     UUID NOT NULL REFERENCES organizations(id),
  project_id UUID NOT NULL,
  status     TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','archived')),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (org_id, id),                                          -- nếu là đích của khoá ngoại ghép
  FOREIGN KEY (org_id, project_id) REFERENCES projects (org_id, id)
);
CREATE INDEX things_org_created_idx ON things (org_id, created_at DESC);

ALTER TABLE things ENABLE ROW LEVEL SECURITY;
ALTER TABLE things FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON things
  USING      (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid)
  WITH CHECK (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE ON things TO app_api;              -- tối thiểu, theo process thật sự cần
```

`uuidv7()` có sẵn từ PostgreSQL 18; kiểm tra phiên bản dự án đang dùng, nếu cũ hơn thì sinh ID ở ứng dụng.

**Thay đổi trên bảng đã có dữ liệu:**

- Đặt `SET lock_timeout` ở đầu.
- Index: `CREATE INDEX CONCURRENTLY`, trong migration riêng đánh dấu `-- +goose NO TRANSACTION`.
- Khoá ngoại và `CHECK`: thêm `NOT VALID`, rồi `VALIDATE CONSTRAINT` ở bước sau.
- Cột mới: nullable, hoặc default hằng không gây rewrite. `NOT NULL` thêm sau khi đã backfill.
- Bỏ hoặc đổi tên cột, đổi kiểu: tách thành expand → migrate → contract qua ít nhất hai lần deploy. Migration này chỉ làm bước expand; ghi rõ các bước sau còn nợ.

**Bảng bất biến** (`usage_records`, `audit_logs`): không cấp `UPDATE` / `DELETE` cho role ứng dụng.

## Sau khi viết

1. Chạy migration lên và xuống trên Postgres thật (testcontainers hoặc DB cục bộ) nếu môi trường có sẵn; lệnh `goose` cần xác nhận của người dùng. Không có môi trường thì nói rõ là chưa chạy.
2. Thêm hoặc sửa truy vấn `sqlc` rồi sinh lại code (`sqlc generate`); không sửa tay file sinh ra.
3. Bảng tenant mới: thêm vào bộ test cross-tenant.
4. Cập nhật `.docs/database/schema.md` cho khớp (DDL ở §4, sơ đồ §3 nếu thêm quan hệ, bảng retention §7 nếu là dữ liệu thực thi).
5. Gọi agent `db-reviewer` để review migration trước khi báo xong.

Báo lại: file đã tạo, đã chạy thử hay chưa và kết quả, các bước contract còn nợ nếu có.
