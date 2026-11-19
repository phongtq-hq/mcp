# ADR-0006: Tách lifecycle của Agent khỏi execution; version bất biến

Trạng thái: Chấp thuận · 2026-11-17

## Bối cảnh

Đề bài mô tả một lifecycle cho Agent: `CREATED → CONFIGURED → ACTIVE → EXECUTING → COMPLETED / FAILED / PAUSED`. Chuỗi này trộn trạng thái của **định nghĩa** (đã cấu hình chưa, có được dùng không) với trạng thái của **một lần chạy** (đang chạy, xong, lỗi). Một Agent phục vụ nhiều execution đồng thời, nên không thể ở một trạng thái `EXECUTING` hay `FAILED` duy nhất.

## Quyết định

1. **Agent** có lifecycle `DRAFT → ACTIVE ⇄ DISABLED → ARCHIVED`.
2. **Execution** và **Task** có lifecycle `QUEUED → RUNNING ⇄ WAITING_APPROVAL / PAUSED → COMPLETED | FAILED | CANCELLED | BUDGET_EXCEEDED | TIMED_OUT`.
3. Cấu hình Agent nằm trong **Agent Version bất biến**. Sửa cấu hình là tạo version mới; publish và rollback là đổi con trỏ `active_version`.
4. Mỗi task ghi lại `agent_version_id` nó chạy. Execution đang chạy không bị ảnh hưởng khi version mới được publish.

Ánh xạ từ đề bài: `CREATED`, `CONFIGURED` → `DRAFT`; `ACTIVE` → `ACTIVE`; `EXECUTING`, `COMPLETED`, `FAILED`, `PAUSED` → trạng thái của execution.

## Phương án đã cân nhắc

- **Giữ một lifecycle như đề bài.** Buộc phải chọn giữa "một Agent chỉ chạy một việc một lúc" (giới hạn không cần thiết) và trạng thái vô nghĩa.
- **Cấu hình sửa tại chỗ, không version.** Không tái hiện được hành vi của execution cũ, không rollback được, audit thiếu căn cứ.

## Hệ quả

- Trả lời được chính xác "execution này đã chạy với prompt và quyền nào".
- Prompt versioning có sẵn mà không cần hệ thống riêng.
- Số dòng `agent_versions` tăng theo số lần sửa; chấp nhận được.
- "Agent đang bận không" trở thành câu truy vấn trên execution, không phải một cột trạng thái.

## Điều kiện xem xét lại

Không dự kiến. Nếu cần chia sẻ prompt giữa nhiều Agent, thêm thư viện prompt có version riêng, không thay đổi quyết định này.
