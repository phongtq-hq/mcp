---
paths:
  - "internal/platform/**"
  - "internal/mcp/**"
  - "internal/agent/**"
  - "internal/workflow/approval/**"
  - "internal/audit/**"
  - "cmd/**"
---

# Bảo mật

Nguồn: `.docs/architecture/security.md` · ADR-0004, 0005, 0008, 0009.

## Giả định

1. Model không phải ranh giới bảo mật. Tool call, kế hoạch, lời giải thích do model sinh ra đều là input không tin cậy.
2. Mọi dữ liệu vào context (kết quả tool, log, mô tả tool, memory) có thể chứa chỉ dẫn độc hại.
3. Quyền của một execution cố định khi bắt đầu và chỉ giảm đi.
4. Tenant khác là đối thủ.

## Phân quyền

- Một điểm quyết định: `authz.Decide(ctx, principal, action, resource, attributes) → Decision`, trả về kèm quy tắc đã khớp. Không viết kiểm tra quyền rải rác bằng `if role == ...`.
- Tool call cần cả bốn lớp cho phép: quyền người khởi tạo ∩ cấu hình Agent Version ∩ chính sách project ∩ trạng thái tool trong Registry.
- Mức sàn của platform (ví dụ `financial` trên production cần hai người duyệt) là **code**, không phải dữ liệu, org không ghi đè được. Cấu hình của org và Agent chỉ được chặt hơn.
- Delegation không bao giờ làm tăng quyền: quyền task con = giao của Agent đích và người khởi tạo.
- Mọi quyết định `deny` và `require_approval` ghi audit kèm quy tắc đã khớp.
- 404 cho tài nguyên của tenant khác, không 403.

## Approval

- Gắn với đúng một hành động: `execution_id`, `task_id`, `step_seq`, tên tool, hash arguments đã chuẩn hoá.
- Dùng một lần, có hạn; hết hạn là từ chối.
- Gateway tính lại hash lúc thực thi; lệch thì từ chối.
- Người duyệt ≠ người yêu cầu. Lý do do Agent đưa ra được đánh dấu là do model sinh.

## Prompt injection

Giả định injection thành công ở tầng model; giới hạn hậu quả:

- Tập tool, budget, yêu cầu approval nạp từ Agent Version lúc bắt đầu. Không có API nào để Agent tự sửa.
- Kết quả tool và memory vào context ở vai `tool` / khối dữ liệu. **Không bao giờ nối vào system prompt.**
- Execution đã đọc nguồn `data_sensitivity` nhạy cảm mà gọi tool `egress` → `require_approval`.
- Che mẫu secret trong kết quả tool trước khi vào context, trước khi trả người dùng, trước khi ghi log.

## Secret

- Chỉ qua interface `SecretStore`. Envelope encryption: DEK riêng mỗi secret, KEK theo org trong KMS.
- Secret không xuất hiện trong: prompt, tool arguments, Temporal history, log, trace attribute, phản hồi API (chỉ trả metadata), thông báo lỗi.
- API key lưu dạng hash, hiển thị một lần.

## SSRF (mọi URL do người dùng cung cấp: endpoint MCP, webhook)

- Chỉ `https`, cổng trong danh sách cho phép.
- Phân giải DNS rồi kiểm tra **mọi** địa chỉ: từ chối loopback, private, link-local (kể cả `169.254.169.254`), CGNAT, IPv6 ULA, IPv4 ánh xạ.
- Ghim IP đã kiểm tra ở tầng dialer (chống DNS rebinding).
- Không tự theo redirect; mỗi bước kiểm tra lại như URL mới.
- Kiểm tra lúc đăng ký **và mỗi lần gọi**. Giới hạn kích thước và thời gian phản hồi.
- Dùng một HTTP client dùng chung đã có guard; không tạo `http.Client` trần cho URL ngoài.

## Khác

- Redis: mọi khoá có tiền tố `org:{id}:`. Redis không chứa dữ liệu không tái tạo được.
- Fail-closed khi phụ thuộc bảo mật hoặc budget không sẵn sàng.
- Webhook gửi đi ký HMAC kèm timestamp; header ngữ cảnh gửi managed server được ký.
- Audit không chứa secret hay nội dung prompt đầy đủ, chỉ tham chiếu step.
