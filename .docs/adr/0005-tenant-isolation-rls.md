# ADR-0005: Shared schema + `org_id` + Row-Level Security

Trạng thái: Chấp thuận · 2026-11-17

## Bối cảnh

Nền tảng hướng tới multi-tenant. Lỗi cách ly tenant là loại lỗi nghiêm trọng nhất và dễ phát sinh nhất (quên một điều kiện `WHERE`).

## Quyết định

1. Một database, một schema, cột `org_id` trên mọi bảng thuộc tenant.
2. Row-Level Security bật và `FORCE` trên các bảng đó, điều kiện theo `current_setting('app.org_id')` đặt bằng `SET LOCAL` trong mỗi transaction.
3. Role ứng dụng không có `BYPASSRLS`. Job xuyên tenant dùng role riêng, trong đường code riêng.
4. Khoá ngoại ghép `(org_id, …)` ở các quan hệ cha–con chính, để database từ chối tham chiếu chéo tenant.
5. Bộ test cross-tenant tự động cho mọi endpoint là điều kiện bắt buộc trong CI.

## Phương án đã cân nhắc

- **Chỉ lọc ở tầng ứng dụng.** Một lớp phòng thủ duy nhất; một lỗi code là lộ dữ liệu.
- **Schema riêng cho mỗi tenant.** Cách ly tốt hơn, nhưng migration nhân theo số tenant và connection pool phức tạp.
- **Database riêng cho mỗi tenant.** Cách ly mạnh nhất; chi phí và vận hành không tương xứng ở giai đoạn này.

## Hệ quả

- Hai lớp độc lập: quên đặt tenant thì không thấy dòng nào thay vì thấy tất cả.
- RLS có chi phí hiệu năng nhỏ; mọi index trên bảng tenant phải bắt đầu bằng `org_id`.
- Cần kỷ luật: mọi truy vấn chạy trong transaction đã đặt tenant.
- Bảng có dòng dùng chung của platform (`org_id IS NULL`: role dựng sẵn, MCP server và provider của platform) cần policy riêng cho đọc và ghi.

## Điều kiện xem xét lại

Tenant có yêu cầu tuân thủ đòi cách ly vật lý, hoặc một tenant đủ lớn để ảnh hưởng hiệu năng tenant khác: chuyển tenant đó sang database riêng. Thiết kế `org_id` nhất quán giúp việc di chuyển này khả thi.
