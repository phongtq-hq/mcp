# ADR-0008: Envelope encryption với KMS; hoãn Vault

Trạng thái: Chấp thuận · 2026-11-17

## Bối cảnh

Platform giữ credential của hạ tầng và hệ thống tài chính. Yêu cầu: Secret Vault, xoay vòng credential, không lưu plaintext. Đồng thời: chi phí vận hành tối ưu, không over-engineering.

## Quyết định

1. Secret lưu trong Postgres dưới dạng ciphertext (AES-256-GCM) với DEK riêng cho mỗi secret; DEK được bọc bởi KEK theo org nằm trong KMS của nhà cung cấp cloud.
2. Truy cập qua interface `SecretStore` (`Put`, `Get`, `Rotate`, `Delete`).
3. Chỉ process `gateway` (và managed server cần thiết) có quyền IAM giải mã. `api` chỉ mã hoá; `worker` không có quyền nào.
4. Secret có phiên bản, `rotation_interval`, `expires_at`.
5. Ưu tiên credential ngắn hạn do hệ thống đích cấp khi có thể.

## Phương án đã cân nhắc

- **HashiCorp Vault / OpenBao.** Mạnh nhất (dynamic secret, lease, audit riêng), nhưng là một hệ thống có trạng thái, cần HA, unseal, sao lưu, và người vận hành có kinh nghiệm.
- **Secret manager của cloud cho từng secret.** Đơn giản, nhưng chi phí và giới hạn tăng theo số secret của mọi tenant, và khoá chặt vào một cloud.
- **Mã hoá bằng khoá trong biến môi trường.** Không có xoay vòng khoá gốc, không có kiểm soát truy cập khoá, không có audit.

## Hệ quả

- Không thêm hệ thống có trạng thái nào; sao lưu secret đi cùng sao lưu Postgres (ciphertext vô dụng nếu thiếu KMS).
- Xoá KEK của một org vô hiệu hoá toàn bộ secret của org đó (crypto-shredding).
- Không có dynamic secret; xoay vòng do platform điều phối.
- Phụ thuộc tính sẵn sàng của KMS; cache DEK đã mở trong bộ nhớ với thời hạn ngắn để giảm phụ thuộc và chi phí.

## Điều kiện xem xét lại

Chuyển sang Vault (sau cùng interface) khi cần dynamic secret cho database hoặc SSH certificate ngắn hạn, hoặc khi yêu cầu tuân thủ đòi hỏi.
