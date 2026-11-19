---
name: new-adr
description: Tạo một Architecture Decision Record mới trong .docs/adr/ theo đúng định dạng của dự án và cập nhật bảng chỉ mục. Dùng khi cần ghi lại một quyết định kiến trúc, thêm thành phần hạ tầng, đảo hoặc thay thế một ADR cũ, hoặc chốt một trong các quyết định còn mở (IdP, KMS, Temporal tự vận hành hay quản lý, khung eval, định dạng workflow).
argument-hint: <tiêu đề quyết định>
---

# Tạo ADR

Quyết định cần ghi: $ARGUMENTS

## Các bước

1. **Đọc bối cảnh.** `.docs/adr/README.md` để lấy số kế tiếp và danh sách hiện có. Đọc các ADR liên quan và mục thiết kế tương ứng trong `.docs/architecture/`. Đọc một ADR hiện có (ví dụ `0003-defer-message-broker.md`) để bám giọng văn và độ dài.

2. **Kiểm tra có cần ADR không.** Cần khi: thêm hoặc bỏ thành phần hạ tầng; thay đổi ranh giới context hoặc quyền của process; đảo một quyết định đã ghi; chọn giữa các phương án có hệ quả lâu dài. Không cần cho chi tiết hiện thực đổi được dễ dàng. Nếu không cần, nói vậy và dừng.

3. **Làm rõ phần thiếu.** Nếu chưa rõ quyết định là gì hoặc chưa biết các phương án đã cân nhắc, hỏi người dùng. Không bịa ra bối cảnh, số liệu hay phương án. Không ghi con số (throughput, chi phí, thời lượng) chưa được đo.

4. **Viết file** `.docs/adr/NNNN-kebab-case.md`, tiếng Việt:

   ```markdown
   # ADR-NNNN: <Tiêu đề nêu thẳng quyết định>

   Trạng thái: Đề xuất · <YYYY-MM-DD>

   ## Bối cảnh
   <Vấn đề, ràng buộc, điều đề bài hoặc thực tế yêu cầu. Vì sao phải quyết định bây giờ.>

   ## Quyết định
   <Danh sách đánh số, mỗi mục là một cam kết cụ thể kiểm chứng được.>

   ## Phương án đã cân nhắc
   - **<Phương án>.** <Vì sao không chọn.>

   ## Hệ quả
   - <Cả lợi và giá phải trả. Rủi ro mới sinh ra.>

   ## Điều kiện xem xét lại
   <Tín hiệu đo được nào sẽ khiến quyết định này được mở lại.>
   ```

   Trạng thái luôn là `Đề xuất`. Chỉ người dùng chuyển sang `Chấp thuận`.

5. **Nếu thay thế ADR cũ:** ADR cũ đã `Chấp thuận` thì không sửa nội dung, chỉ đổi dòng trạng thái thành `Thay thế bởi ADR-NNNN`. ADR mới nêu rõ nó thay thế ADR nào và điều gì đã thay đổi so với lúc đó.

6. **Cập nhật chỉ mục.** Thêm dòng vào bảng trong `.docs/adr/README.md`. Nếu ADR chốt một mục trong danh sách "Quyết định còn mở" ở cuối file đó, xoá mục ấy khỏi danh sách.

7. **Cập nhật tài liệu bị ảnh hưởng.** Tìm các chỗ trong `.docs/architecture/`, `roadmap.md`, `schema.md` nói khác với quyết định mới và sửa, kèm liên kết tới ADR. Nếu ADR thay đổi một bất biến ghi trong `.claude/CLAUDE.md` hoặc `.claude/rules/`, sửa ở đó nữa.

8. **Báo lại** đường dẫn file, tóm tắt quyết định trong một câu, và các tài liệu đã sửa theo.
