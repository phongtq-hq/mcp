---
paths:
  - ".docs/**"
---

# Tài liệu thiết kế và ADR

- Viết bằng **tiếng Việt**, giữ thuật ngữ kỹ thuật và định danh bằng tiếng Anh. Giọng văn như tài liệu hiện có: trực tiếp, nêu lý do, nêu cả điều chưa biết.
- Mỗi tài liệu có dòng `Trạng thái:` ở đầu và liên kết tới ADR liên quan.
- Liên kết giữa tài liệu dùng đường dẫn tương đối. Khi đổi tên hoặc số mục, sửa các liên kết trỏ tới nó.
- Không ước lượng thời lượng, không cam kết SLA hay throughput khi chưa có số đo. Phase được định nghĩa bằng tiêu chí "Xong khi" kiểm chứng được.
- Sơ đồ dùng Mermaid.

## ADR

- Định dạng: Bối cảnh → Quyết định → Phương án đã cân nhắc → Hệ quả → Điều kiện xem xét lại.
- Trạng thái hợp lệ: `Đề xuất`, `Chấp thuận`, `Thay thế bởi ADR-xxxx`, `Loại bỏ`.
- ADR đã `Chấp thuận` **không sửa nội dung**; đổi quyết định bằng ADR mới và cập nhật trạng thái ADR cũ thành `Thay thế bởi`. ADR còn `Đề xuất` thì sửa được.
- Tên file `NNNN-kebab-case.md`, số tăng dần. Thêm dòng vào bảng trong `.docs/adr/README.md`.
- Chỉ người dùng mới chuyển một ADR sang `Chấp thuận`.

## Giữ đồng bộ

| Thay đổi | Cập nhật cùng lúc |
|---|---|
| Schema DB | `.docs/database/schema.md` |
| Endpoint, quy ước API | `.docs/api/openapi.yaml` |
| Bounded context, cấu trúc thư mục | `overview.md` §2, §8 |
| Phạm vi hoặc thứ tự phase | `roadmap.md` |
| Hạ tầng mới, đảo một quyết định | ADR mới |

Các quyết định còn mở (IdP, KMS, Temporal tự vận hành hay dịch vụ quản lý, khung eval, định dạng workflow) chưa có ADR: đừng viết code giả định một lựa chọn; giữ sau interface.
