---
paths:
  - "**/*.go"
---

# Quy ước Go

- **Context:** `context.Context` là tham số đầu tiên của mọi hàm có I/O. Tenant, principal, trace đi qua context, xác lập đúng một lần ở middleware. Không lưu context trong struct.
- **Tenant:** repository lấy `org_id` từ context. Hàm nhận `orgID` làm tham số do người gọi tự do truyền là dấu hiệu sai, trừ code đường riêng của job xuyên tenant.
- **Lỗi:** bọc bằng `%w` kèm ngữ cảnh; phân loại retryable / non-retryable bằng kiểu lỗi trong `shared/errors`, không so chuỗi. Lỗi trả ra API theo RFC 9457 với `code` ổn định.
- **ID:** UUIDv7 qua `shared/ids`. Không dùng số tự tăng lộ ra ngoài.
- **Tiền:** kiểu decimal (ánh xạ `NUMERIC(18,8)`), không `float64`. Luôn đi kèm `currency`.
- **Thời gian:** UTC. Nhận đồng hồ qua interface ở nơi cần test hoặc cần deterministic.
- **Interface:** khai báo ở bên dùng, nhỏ. Trả về struct cụ thể, nhận interface.
- **Giới hạn:** mọi vòng lặp, retry, đọc body, đọc kết quả tool đều có trần tường minh. Không có `for {}` chờ vô hạn; không `io.ReadAll` trên dữ liệu ngoài mà thiếu `LimitReader`.
- **Transaction:** trong hàm chạy bên trong `tenancy.InTx` chỉ dùng `tx` được trao. Không gọi service hay hàm nào tự mở transaction / xin kết nối khác từ pool: khi pool cạn, các request đang giữ transaction (và khoá dòng) chờ nhau mãi. Kiểm tra cần kết nối riêng thì làm trước khi mở transaction. Ghi audit bằng `audit.Record(ctx, tx, …)` ở cuối transaction.
- **Goroutine:** có chủ sở hữu, có đường dừng theo context, không rò rỉ khi shutdown.
- **Log / trace:** log có cấu trúc. `trace_id`, `execution_id`, `org_id`, `agent_id` là attribute chuẩn. Không ghi secret, không ghi nội dung prompt hay kết quả tool đầy đủ.
- **Cấu hình:** không hardcode tên model, giá, endpoint, tên Agent. Runtime không rẽ nhánh theo tên Agent.
- **SQL:** viết trong file `.sql` cho `sqlc`; không nối chuỗi SQL bằng input.
- **Code sinh:** không sửa tay file do `sqlc` hay generator OpenAPI sinh ra; sửa nguồn rồi sinh lại.
