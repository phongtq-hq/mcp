# ADR-0011: Keycloak tự vận hành làm Identity Provider

Trạng thái: Đề xuất · 2026-11-17

## Bối cảnh

Platform không tự lưu mật khẩu; người dùng đăng nhập bằng OIDC (Authorization Code + PKCE) qua một Identity Provider ([security §4.1](../architecture/security.md)). MCP Gateway là resource server OAuth 2.1 và cũng cần một authorization server phát hành token cho MCP client bên ngoài. Lựa chọn IdP là một trong các quyết định còn mở của Phase 0.

Một user có thể thuộc nhiều organization, trong khi API quy định organization được xác định từ token chứ không từ đường dẫn.

## Quyết định

1. Dùng **Keycloak tự vận hành** làm IdP cho người dùng và làm authorization server cho MCP client bên ngoài.
2. Code của platform **chỉ phụ thuộc OIDC chuẩn**: discovery, JWKS, kiểm tra `iss`, `aud`, `exp`, chữ ký. Không gọi Admin API của Keycloak, không dùng thư viện riêng của Keycloak.
3. Danh tính của user trong platform là cặp `(iss, sub)`. Bản ghi `users` được tạo ở lần đăng nhập đầu tiên.
4. **Membership và quyền nằm trong database của platform**, không nằm trong Keycloak. Keycloak chỉ trả lời "đây là ai".
5. Organization của một request: nếu token có claim `org_id` thì dùng nó, với điều kiện user có membership đang hoạt động ở org đó; nếu không có claim và user thuộc đúng một org thì dùng org đó; còn lại từ chối với mã `organization_required`. Tên claim cấu hình được.
6. Phát triển cục bộ chạy Keycloak trong docker-compose. Test tự động dùng một issuer OIDC giả lập trong process, không cần Keycloak.

## Phương án đã cân nhắc

- **Zitadel tự vận hành.** Viết bằng Go, nhẹ hơn, có sẵn khái niệm organization. Cộng đồng và tài liệu nhỏ hơn; đội chưa có kinh nghiệm vận hành.
- **Dịch vụ quản lý (Auth0, Entra ID...).** Không phải vận hành, nhưng có chi phí theo user và đưa một phụ thuộc bên ngoài vào đường đăng nhập của công cụ vận hành hạ tầng.
- **Lưu membership và role trong Keycloak (group, realm role).** Bớt một bảng, nhưng quyền mức tool và điều kiện ABAC không diễn đạt được ở đó, và platform sẽ phụ thuộc mô hình dữ liệu của một IdP cụ thể.

## Hệ quả

- Thêm một thành phần có trạng thái phải vận hành, sao lưu và nâng cấp (Keycloak + database của nó).
- Đổi IdP sau này chỉ là đổi cấu hình issuer, vì quyết định 2 và 4.
- User tồn tại ở hai nơi (Keycloak và bảng `users`); vô hiệu hoá user phải làm ở cả hai, hoặc dựa vào thời hạn ngắn của token.
- Chuyển org đối với user thuộc nhiều org cần Keycloak phát token có claim `org_id`; cách cấu hình cụ thể cần làm khi dựng realm.

## Điều kiện xem xét lại

Chi phí vận hành Keycloak vượt giá trị với quy mô thực tế; hoặc cần tính năng mà Keycloak không có và một IdP khác có.
