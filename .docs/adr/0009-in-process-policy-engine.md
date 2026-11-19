# ADR-0009: Policy engine in-process bằng Go; hoãn OPA / Cedar

Trạng thái: Chấp thuận · 2026-11-17

## Bối cảnh

Cần RBAC, điều kiện thuộc tính (ABAC), phân quyền mức tool và mức Agent, và quyết định ba trạng thái (`allow`, `deny`, `require_approval`). Quyết định này nằm trên đường nóng của mọi tool call.

## Quyết định

1. Policy engine là một package Go (`internal/platform/authz`), gọi in-process từ `api`, `gateway` và `worker`.
2. Mô hình: role là tập permission; role binding theo phạm vi org hoặc project; điều kiện tuỳ chọn là dữ liệu JSON với tập toán tử cố định trên một tập thuộc tính cố định (môi trường, mức rủi ro, khung giờ, giá trị tham số).
3. Một hàm duy nhất `Decide(ctx, principal, action, resource, attributes) → Decision` trả về kết quả kèm quy tắc đã khớp.
4. Mức sàn của platform (ví dụ `financial` trên production luôn cần hai người duyệt) là code, không phải dữ liệu, và org không ghi đè được.

## Phương án đã cân nhắc

- **OPA (Rego).** Biểu đạt mạnh, nhưng thêm một ngôn ngữ đội phải học và test, và chính sách do tenant viết bằng ngôn ngữ tổng quát khó kiểm soát.
- **Cedar.** Mô hình phù hợp và phân tích được, nhưng hệ sinh thái Go còn mỏng hơn và chưa có nhu cầu vượt quá mô hình ở trên.
- **Chỉ RBAC.** Không diễn đạt được "chỉ trên non-prod" hay "dưới hạn mức X".

## Hệ quả

- Không thêm phụ thuộc hay bước mạng; quyết định trong vài micro giây; test bằng bảng.
- Biểu đạt có giới hạn: điều kiện mới cần thay đổi code. Đây là chủ ý ở giai đoạn đầu.
- Interface `Decide` ổn định nên thay phần hiện thực được.

## Điều kiện xem xét lại

Khi tenant cần tự viết chính sách vượt ngoài tập điều kiện cố định, hoặc số loại điều kiện tăng tới mức engine tự viết trở thành một ngôn ngữ chính sách không chủ đích.
