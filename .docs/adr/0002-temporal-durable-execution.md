# ADR-0002: Temporal cho durable execution, đưa vào từ Phase 2

Trạng thái: Chấp thuận · 2026-11-17

## Bối cảnh

Một execution có thể kéo dài từ vài giây tới nhiều giờ (chờ approval), gồm nhiều lần gọi LLM và tool, có nhánh song song, cần huỷ lan truyền, và phải tiếp tục đúng chỗ sau khi process restart mà không chạy lại tool có side effect. Đề bài đặt Temporal ở Phase 3 và Orchestrator ở Phase 2.

## Quyết định

1. Dùng Temporal làm nền durable execution cho: vòng lặp Agent, Orchestrator, workflow người dùng định nghĩa, approval, lịch chạy.
2. **Đưa Temporal vào ở Phase 2**, cùng lúc với Orchestrator.
3. Phase 1 chạy vòng lặp đơn Agent trong process `api`, không bền vững qua restart, chỉ tool đọc, nhưng viết sau interface `Executor` với các bước rời có đầu vào / đầu ra tuần tự hoá được, để chuyển thành activity không cần viết lại.
4. Temporal history chỉ chứa tham chiếu; nội dung prompt và kết quả tool nằm ở Postgres.
5. Lịch chạy dùng Temporal Schedules; không có process `scheduler` riêng.

## Phương án đã cân nhắc

- **Giữ đúng thứ tự đề bài (Temporal ở Phase 3).** Orchestrator ở Phase 2 phải tự hiện thực fan-out, chờ, huỷ, phục hồi; rồi bị thay ở Phase 3. Làm hai lần phần khó nhất.
- **Tự viết trên Postgres (bảng job + state machine).** Khả thi cho vòng lặp đơn, nhưng child workflow, signal, timer bền vững, versioning code đang chạy là những bài toán Temporal đã giải. Tự viết là chi phí ẩn lớn.
- **Temporal từ Phase 1.** Thêm một hệ thống phải vận hành trước khi có thứ gì chạy được. Phase 1 chỉ có tool đọc nên mất execution khi restart là chấp nhận được.

## Hệ quả

- Thêm một thành phần hạ tầng phải vận hành (cluster Temporal + database của nó), hoặc chi phí dịch vụ quản lý.
- Code workflow phải deterministic; cần kỷ luật versioning và test replay trong CI.
- Đổi lại: phục hồi, retry, timeout, huỷ, chờ dài hạn, lịch chạy có sẵn và nhất quán.

## Điều kiện xem xét lại

Nếu sau Phase 2, chi phí vận hành Temporal vượt quá giá trị với quy mô thực tế (ví dụ chỉ có vài chục execution mỗi ngày), cân nhắc dịch vụ quản lý trước khi cân nhắc bỏ.
