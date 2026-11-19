# ADR-0003: Hoãn NATS JetStream; dùng transactional outbox

Trạng thái: Chấp thuận · 2026-11-17

## Bối cảnh

Đề bài liệt kê "NATS JetStream hoặc giải pháp message queue phù hợp". Các nhu cầu nhắn tin bất đồng bộ đã biết:

| Nhu cầu | Giải pháp đã có |
|---|---|
| Hàng đợi công việc cho execution | Temporal task queue |
| Chờ và nhận quyết định approval | Temporal signal |
| Lịch chạy | Temporal Schedules |
| Sự kiện tới client (SSE) | Postgres `LISTEN/NOTIFY` hoặc Redis pub/sub, dữ liệu đọc lại từ Postgres |
| Webhook, thông báo, cảnh báo budget | Outbox trong Postgres + một activity phát |

## Quyết định

Không triển khai message broker ở Phase 1–5. Sự kiện miền được ghi vào bảng `outbox` trong cùng transaction với thay đổi dữ liệu và được phát bởi một tiến trình nền, bảo đảm at-least-once. Bên nhận phải idempotent.

Code phát sự kiện đi qua interface `EventPublisher` để thay phần hiện thực được.

## Phương án đã cân nhắc

- **NATS JetStream từ đầu.** Thêm một hệ thống có trạng thái phải vận hành, sao lưu, giám sát, trong khi chưa có nhu cầu nào ở trên cần đến nó.
- **Redis Streams.** Redis đã có, nhưng ADR này giữ Redis ở vai trò dữ liệu tái tạo được; dùng nó làm hàng đợi bền vững phá nguyên tắc đó.

## Hệ quả

- Ít hạ tầng hơn; tính nhất quán giữa dữ liệu và sự kiện do transaction bảo đảm.
- Thông lượng sự kiện bị giới hạn bởi Postgres; đủ cho các nhu cầu đã liệt kê.

## Điều kiện xem xét lại

Đưa broker vào khi xuất hiện ít nhất một trong: nhiều service độc lập cần tiêu thụ cùng luồng sự kiện; thông lượng outbox đo được trở thành nút thắt; cần fan-out sự kiện thời gian thực tới nhiều consumer ngoài; tách microservices theo ADR-0001.
