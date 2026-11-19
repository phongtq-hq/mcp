# ADR-0001: Modular monolith — một repo, ba process

Trạng thái: Chấp thuận · 2026-11-17

## Bối cảnh

Nền tảng có khoảng mười bounded context. Đội phát triển ở giai đoạn đầu nhỏ. Yêu cầu nêu rõ: modular monolith trước, có khả năng tách microservices, không over-engineering.

## Quyết định

Một repository Go, một module, build ra ba binary: `api`, `gateway`, `worker`. Mỗi bounded context là một package dưới `internal/` với các luật:

1. Context khác chỉ dùng được interface công khai của nó, không dùng package lưu trữ.
2. Interface được khai báo ở phía bên dùng.
3. Không join SQL xuyên context, trừ truy vấn báo cáo của Cost và Audit.
4. Luật 1 và 3 được kiểm bằng linter trong CI.

## Phương án đã cân nhắc

- **Một binary duy nhất.** Đơn giản nhất, nhưng Gateway (nhận traffic không tin cậy, giữ quyền giải mã secret) chung process với API quản trị làm rộng bề mặt tấn công, và không scale riêng được.
- **Microservices từ đầu.** Chi phí vận hành và giao tiếp mạng cho một đội nhỏ, khi ranh giới context chưa được kiểm chứng bằng thực tế.

## Hệ quả

- Triển khai và debug đơn giản; transaction trong một database.
- Ba process có quyền IAM và role DB khác nhau ngay từ đầu.
- Rủi ro: ranh giới bị xói mòn theo thời gian nếu linter bị nới.

## Điều kiện xem xét lại

Tách một context thành service riêng khi có ít nhất một trong: hồ sơ tải khác hẳn và đã đo được; đội riêng sở hữu và cần chu kỳ release độc lập; yêu cầu cách ly bảo mật mà process riêng không đáp ứng.
