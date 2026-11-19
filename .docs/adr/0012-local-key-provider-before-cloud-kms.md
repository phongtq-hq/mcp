# ADR-0012: Bọc khoá bằng provider cục bộ trước, Cloud KMS sau

Trạng thái: Chấp thuận · 2026-11-18

## Bối cảnh

[ADR-0008](0008-secrets-envelope-encryption.md) quyết định envelope encryption: mỗi secret có DEK riêng, DEK được bọc bởi KEK theo org nằm trong KMS của nhà cung cấp cloud. Lựa chọn KMS cụ thể còn mở.

Yêu cầu khi chốt: **bắt đầu bằng phương án không tốn phí**. Theo hiểu biết lúc viết, KMS của các cloud lớn tính phí theo từng khoá mỗi tháng và theo số thao tác; mô hình một KEK cho mỗi org làm chi phí tăng theo số org. Bảng giá hiện hành chưa được kiểm tra lại và cần xác minh trước khi chọn nhà cung cấp.

## Quyết định

1. Việc bọc và mở DEK đi qua interface `KeyWrapper` (`Wrap`, `Unwrap` theo `org_id`), tách khỏi `SecretStore`. Định dạng ciphertext lưu kèm định danh provider và phiên bản khoá, để nhiều provider cùng tồn tại trong lúc chuyển đổi.
2. Phase 1 hiện thực provider **`local`**: một khoá gốc 256 bit nạp từ file do hạ tầng cấp (không nằm trong repo, không nằm trong database). Từ khoá gốc dẫn xuất (HKDF) một cặp khoá X25519; DEK của mỗi secret được bọc theo kiểu sealed-box tới khoá công khai đó, và khoá bọc được dẫn xuất (HKDF) kèm `org_id`, nên mỗi org có khoá bọc riêng (vai trò của KEK theo org). Bản mã gắn với đúng org, secret và version qua dữ liệu xác thực kèm theo. Không có chi phí.
3. Provider `local` **chỉ được dùng cho phát triển và staging, với credential không phải production**. Process từ chối khởi động với provider `local` khi `HQ_ENV=production`.
4. Phân quyền theo process của ADR-0008 vẫn giữ: chỉ `gateway` được cấp khoá gốc để mở DEK. `api` cần mã hoá mà không được giải mã, nên mã hoá bằng khoá công khai (bọc DEK bất đối xứng); `worker` không có gì.
5. Chọn và hiện thực provider Cloud KMS là điều kiện bắt buộc **trước khi lưu bất kỳ credential production hoặc `financial` nào**.

## Phương án đã cân nhắc

- **Cloud KMS ngay từ Phase 1.** Đúng với ADR-0008 nhất, nhưng có chi phí theo khoá và buộc chọn nhà cung cấp cloud trước khi biết môi trường triển khai.
- **Vault / OpenBao Transit tự vận hành.** Miễn phí bản quyền, nhưng là hệ thống có trạng thái cần HA, unseal và người vận hành có kinh nghiệm; ADR-0008 đã hoãn vì lý do đó.
- **Một KEK dùng chung cho mọi org trong Cloud KMS.** Rẻ, nhưng mất crypto-shredding theo org.

## Hệ quả

- Phase 1 chạy được đầu-cuối trên staging mà không phát sinh chi phí KMS.
- Provider `local` yếu hơn KMS thật: không có kiểm soát truy cập và audit độc lập trên khoá gốc; lộ file khoá gốc là lộ mọi secret. Đây chính là phương án "khoá trong biến môi trường" mà ADR-0008 đã loại cho production, nên giới hạn ở quyết định 3 là bắt buộc chứ không phải khuyến nghị.
- Crypto-shredding theo org không thực hiện được với provider `local` (KEK dẫn xuất được lại từ khoá gốc).
- Chuyển sang Cloud KMS cần bọc lại mọi DEK; định dạng ciphertext ở quyết định 1 cho phép làm dần.

## Điều kiện xem xét lại

Trước khi kết nối hệ thống production đầu tiên; hoặc khi môi trường triển khai được chốt (khi đó chọn KMS của chính cloud ấy).
