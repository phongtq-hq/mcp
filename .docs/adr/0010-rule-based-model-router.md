# ADR-0010: Model Router theo luật và tier

Trạng thái: Chấp thuận · 2026-11-17

## Bối cảnh

Yêu cầu: tự động chọn model phù hợp với độ phức tạp nhiệm vụ; không hardcode model; hỗ trợ nhiều provider và model nội bộ. Ở thời điểm bắt đầu chưa có dữ liệu nào về chất lượng từng model trên từng loại task của hệ thống này.

## Quyết định

1. Agent và platform tham chiếu **tier** (`economy`, `standard`, `premium`), không tham chiếu tên model. Ánh xạ tier → model là cấu hình theo org / project.
2. Router là hàm thuần, chọn theo luật tĩnh: ràng buộc khả năng và chính sách dữ liệu → `purpose` của lời gọi → điều chỉnh theo budget và lần thử lại → sức khoẻ provider và chuỗi fallback.
3. Agent Version có thể đặt `policy: fixed` để ghim một model.
4. Mọi quyết định định tuyến được ghi vào step.
5. Không dùng LLM để phân loại độ phức tạp ở giai đoạn này.

## Phương án đã cân nhắc

- **Phân loại độ phức tạp bằng một lời gọi LLM.** Thêm chi phí và độ trễ cho mọi yêu cầu, và không có dữ liệu để biết bộ phân loại có đúng không.
- **Mỗi Agent ghim một model.** Đơn giản, nhưng tóm tắt và nén context dùng cùng model đắt với suy luận chính.
- **Dùng gateway LLM bên thứ ba.** Thêm một bên nhìn thấy toàn bộ prompt và một điểm lỗi; chức năng cần thiết ở đây đủ nhỏ để tự làm.

## Hệ quả

- Tiết kiệm ngay ở phần dễ: tác vụ phụ trợ dùng tier rẻ.
- Đổi model không cần deploy, không cần sửa Agent.
- Router không "thông minh"; có thể chọn tier cao hơn cần thiết cho task đơn giản.

## Điều kiện xem xét lại

Khi dữ liệu step đủ để so sánh tỉ lệ thành công và chi phí theo tier trên từng loại task (dự kiến đánh giá ở Phase 5), và khi có bộ eval cho Agent để đo ảnh hưởng chất lượng của việc đổi tier.
