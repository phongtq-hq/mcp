# ADR-0004: MCP Gateway là đường duy nhất tới tool; không chuyển tiếp token

Trạng thái: Chấp thuận · 2026-11-17

## Bối cảnh

Agent thực hiện hành động trên hệ thống thật qua tool. Nếu có nhiều đường tới tool (worker gọi thẳng MCP server, Agent giữ credential), mỗi đường phải tự hiện thực phân quyền, approval, audit, rate limit, và một đường thiếu sót là đủ để vượt kiểm soát.

## Quyết định

1. Mọi tool call, từ Agent nội bộ lẫn MCP client bên ngoài, đi qua MCP Gateway. Không có ngoại lệ, kể cả chế độ `direct_tool`.
2. Worker không có đường mạng và không có credential để gọi thẳng MCP server hay hệ thống đích.
3. Gateway không chuyển tiếp token của client xuống upstream. Nó dùng credential riêng cho từng upstream, lấy từ Secrets theo org hoặc user.
4. Agent nội bộ xác thực với Gateway bằng execution token ngắn hạn mang ngữ cảnh execution.
5. Gateway thực thi lại quyết định policy ở mỗi lời gọi, không tin vào việc Runtime đã kiểm tra.
6. Gateway không chứa logic nghiệp vụ; logic đó nằm ở từng MCP server.

## Phương án đã cân nhắc

- **Worker gọi thẳng managed server, Gateway chỉ cho client ngoài.** Bớt một bước mạng, nhưng tạo hai đường thực thi chính sách phải giữ đồng bộ.
- **Chuyển tiếp token người dùng xuống upstream.** Đơn giản hơn, nhưng upstream nhận token không phát hành cho nó (lỗi confused deputy), và đặc tả MCP cấm cách làm này.

## Hệ quả

- Một điểm duy nhất để audit, giới hạn, phân quyền và quan sát mọi tool call.
- Gateway nằm trên đường nóng: phải stateless, scale ngang, và được đo độ trễ từ Phase 1. Bản MCP `2026-07-28` bỏ session nên việc scale ngang không cần sticky routing.
- Gateway là mục tiêu giá trị cao: chỉ nó có quyền giải mã secret.

## Điều kiện xem xét lại

Nếu độ trễ thêm của Gateway đo được là đáng kể với một lớp tool cụ thể, tối ưu Gateway (cache quyết định policy, giữ kết nối) trước khi cân nhắc đường tắt.
