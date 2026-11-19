---
paths:
  - "internal/cost/**"
  - "internal/ai/**"
---

# Cost và AI Provider

Nguồn: `.docs/architecture/cost-management.md` · ADR-0007, ADR-0010.

## Usage ledger

- Mỗi lần gọi LLM và mỗi tool call có tính phí tạo đúng một `usage_record`, cùng activity với `execution_step`, idempotent theo `(task_id, step_seq)`.
- Token lấy **từ response của provider**. Tokenizer cục bộ chỉ để ước lượng trước khi gọi.
- Ghi đủ loại token: input, output, cached input, cache write, reasoning.
- Các chiều phân tích ghi sẵn trên bản ghi, không join lúc truy vấn.
- Bản ghi bất biến; sửa bằng bản ghi điều chỉnh.
- Lời gọi thất bại vẫn ghi nếu provider có tính phí (stream bị huỷ giữa chừng).
- Ghi `purpose`: `agent_step`, `planning`, `summarization`, `routing`, `aggregation`.

## Giá

- **Không có giá nào trong source code.** `model_pricing` là dữ liệu có phiên bản và khoảng hiệu lực; giá mới là dòng mới.
- Tra giá theo thời điểm phát sinh; `usage_record` tham chiếu `pricing_id` đã dùng.
- Model chưa có giá hiệu lực: mặc định **từ chối gọi**.
- Ước tính (`usage_records.estimated_cost`) và thực tế (`cost_records`) lưu riêng, không ghi đè, không trộn khi hiển thị.

## Budget (từ Phase 2)

- Reserve → Call → Settle. Reserve trừ nguyên tử mọi bộ đếm liên quan bằng một Lua script; thiếu ở bất kỳ cấp nào thì từ chối và hoàn lại.
- Một lời gọi phải thoả mọi budget: task → execution → agent → user → project → org.
- Reservation có TTL để tự hoàn khi worker chết.
- Postgres là nguồn sự thật; Redis là bộ đếm nhanh, dựng lại được từ ledger. Mất Redis: **fail-closed** cho org có `hard_action = block`.
- Vượt ở task / execution → `BUDGET_EXCEEDED` kèm kết quả dở dang. Vượt ở cấp cao hơn → HTTP 402 `budget_exceeded` cho execution mới.
- Hiện thực (`internal/cost/budget`, cost-management §4.4): đặt trước / quyết toán nằm ở nơi gọi model (Gateway; bước `CallModel` cho adapter trong process). Tiền tính bằng số nguyên đơn vị 10^-8, không float. Lượt bị chặn là `Turn.Blocked`, không phải một stop reason của provider. Budget execution và task hiện là một; worker không đọc bảng `budgets`.

## Provider và Router

- Provider sau interface `ai.Provider`. Phase 1 chỉ một adapter.
- Agent và platform tham chiếu **tier** (`economy`, `standard`, `premium`), không tên model. Ánh xạ tier → model là cấu hình theo org / project.
- Router là **hàm thuần**, luật tĩnh theo thứ tự: `policy: fixed` → ràng buộc cứng (khả năng, provider được phép, phân loại dữ liệu) → `purpose` → điều chỉnh theo budget và lần thử lại → sức khoẻ provider và fallback.
- **Không dùng LLM để phân loại độ phức tạp.**
- Ghi `model_requested`, `model_used`, `routing_reason` vào step.
- LLM 429 / 5xx / timeout: chuyển ngay sang model dự phòng trong chuỗi của task; cả chuỗi lỗi thì thử lại cả chuỗi với backoff + jitter, tối đa 3 vòng (trần 3 × độ dài chuỗi lời gọi mỗi lượt). Model không phục vụ được (bị tắt, mất khoá) bị bỏ qua. Lỗi nội dung và lỗi của chính platform: không chuyển model.
- Chuỗi model của task chốt lúc tạo (`agent_tasks.model_chain`); Gateway chỉ phục vụ model trong chuỗi. Ánh xạ tier → model nằm ở `settings.model_routing` của organization / project (`internal/ai/router`).
- Sắp xếp context để tận dụng prompt caching: phần ổn định ở đầu. Không cache phản hồi LLM theo ngữ nghĩa.

## Ranh giới

`cost` không gọi LLM, không biết Agent hoạt động thế nào. `ai/router` không biết gì về Agent.
