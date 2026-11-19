# ADR-0007: Usage ledger từ Phase 1; tách ước tính và chi phí thực

Trạng thái: Chấp thuận · 2026-11-17

## Bối cảnh

Đề bài đặt Cost Management ở Phase 5 và đồng thời gọi nó là một trong những thành phần quan trọng nhất. Dữ liệu tiêu thụ token chỉ có tại thời điểm gọi model; không ghi lúc đó thì không khôi phục được. Multi-agent (Phase 2) làm chi phí một yêu cầu khó đoán trước.

## Quyết định

1. Từ Phase 1, mỗi lần gọi LLM ghi một `usage_record` bất biến, kèm mọi chiều phân tích và chi phí ước tính theo bảng giá hiệu lực.
2. Bảng giá (`model_pricing`) là dữ liệu có phiên bản và khoảng hiệu lực; không có giá trong code. Mỗi `usage_record` tham chiếu phiên bản giá đã dùng.
3. Budget cứng (reserve / settle) có từ Phase 2.
4. **Chi phí ước tính** (`usage_records`) và **chi phí thực** từ provider (`cost_records`) lưu riêng, không ghi đè lên nhau; job đối soát ghi độ lệch.
5. Dashboard, forecast, đối soát ở Phase 5.

## Phương án đã cân nhắc

- **Làm toàn bộ ở Phase 5.** Mất dữ liệu của Phase 1–4; chạy multi-agent không có trần chi phí.
- **Chỉ dựa vào billing của provider.** Không phân bổ được theo Agent, task, user; trễ nhiều giờ nên không dùng để chặn được.
- **Gộp ước tính và thực tế vào một bảng.** Hai nguồn có độ chi tiết và thời điểm khác nhau; gộp lại làm mờ con số nào là sự thật.

## Hệ quả

- Phase 1 và 2 nặng hơn một chút.
- Có dữ liệu thật để thiết kế router và dashboard thay vì phỏng đoán.
- Ước tính có thể lệch thực tế; độ lệch được đo và cảnh báo chứ không bị che.

## Điều kiện xem xét lại

Nếu một provider cung cấp chi phí chính xác theo từng lời gọi ngay trong response, dùng số đó thay cho ước tính với provider ấy, vẫn giữ đối soát.
