# Cost Management

Trạng thái: **Đề xuất** · Liên quan: [ADR-0007](../adr/0007-usage-ledger-from-phase-1.md), [ADR-0010](../adr/0010-rule-based-model-router.md)

## 1. Mục tiêu và phạm vi

Cost Management là một bounded context độc lập (`internal/cost`) với bốn trách nhiệm:

1. **Ghi** mọi đơn vị tiêu thụ có chi phí (usage ledger).
2. **Định giá** theo bảng giá có phiên bản.
3. **Kiểm soát** bằng budget ở nhiều cấp, trước và trong khi chạy.
4. **Báo cáo** và đối soát với hoá đơn thật của provider.

Context này không gọi LLM, không biết Agent hoạt động thế nào. Nó nhận sự kiện tiêu thụ và trả lời câu hỏi "còn được tiêu bao nhiêu".

**Thứ tự triển khai khác đề bài:** phần *ghi* phải có từ Phase 1, vì dữ liệu tiêu thụ không ghi lúc phát sinh thì không khôi phục được. Budget có từ Phase 2 (khi multi-agent làm chi phí khó đoán). Dashboard, forecast, đối soát ở Phase 5.

## 2. Usage ledger

Mỗi lần gọi LLM và mỗi lần gọi tool có tính phí tạo đúng một `usage_record`, ghi trong cùng activity với `execution_step`.

Các chiều phân tích được **ghi sẵn trên bản ghi** (không join lúc truy vấn): `org_id`, `project_id`, `user_id`, `agent_id`, `agent_version_id`, `execution_id`, `task_id`, `workflow_id`, `provider_id`, `model_id`, `tool_id`.

Đại lượng đo:

| Trường | Ý nghĩa |
|---|---|
| `input_tokens`, `output_tokens` | Theo số provider trả về trong response |
| `cached_input_tokens`, `cache_write_tokens` | Giá khác với input thường |
| `reasoning_tokens` | Với model có tính riêng |
| `units`, `unit_type` | Cho tool tính phí theo lượt gọi hoặc đơn vị khác |
| `pricing_id` | Phiên bản giá đã dùng để tính |
| `estimated_cost`, `currency` | Chi phí ước tính tại thời điểm ghi |
| `purpose` | `agent_step`, `planning`, `summarization`, `routing`, `aggregation` — để thấy chi phí "phụ trợ" |

Nguyên tắc:

- **Lấy token từ response của provider**, không tự đếm. Tự đếm (tokenizer cục bộ) chỉ dùng để ước lượng trước khi gọi.
- Bản ghi bất biến. Sai sót được sửa bằng bản ghi điều chỉnh, không `UPDATE`.
- Idempotent theo `(task_id, step_seq)`.
- Lời gọi thất bại vẫn ghi nếu provider có tính phí (ví dụ stream bị huỷ giữa chừng).
- Tiền lưu dạng `NUMERIC(18,8)`. Không dùng float.

## 3. Bảng giá

`model_pricing` là dữ liệu, không phải code:

- Mỗi dòng: `model_id`, đơn giá theo từng loại token (trên một triệu token), `currency`, `effective_from`, `effective_to`, `source`, `version`.
- Giá mới = dòng mới với `effective_from` mới; dòng cũ được đóng lại. Không sửa dòng đã có hiệu lực.
- Tra giá theo thời điểm phát sinh: dòng có `effective_from ≤ t < effective_to`. Ràng buộc exclusion trong Postgres bảo đảm không có hai dòng chồng thời gian cho cùng (model, phạm vi giá).
- Hỗ trợ giá ghi đè theo org (giá thoả thuận riêng, hoặc model nội bộ có giá nội bộ). Org tự đặt giá cho model của chính mình; giá ghi đè trên model của platform do platform admin đặt, không phải tenant tự phục vụ.
- Cập nhật qua API quản trị hoặc file cấu hình nạp vào DB qua job; mọi thay đổi ghi audit. **Không có giá nào nằm trong source code.**
- Model nội bộ: giá nội bộ do platform admin đặt (ví dụ quy từ chi phí GPU), để router và báo cáo so sánh được.

Model chưa có giá hiệu lực: mặc định **từ chối gọi** (cấu hình được thành "cho phép, ghi chi phí null và cảnh báo"). Chạy mà không biết giá thì budget mất ý nghĩa.

## 4. Budget

### 4.1 Mô hình

Một budget = phạm vi + chu kỳ + hạn mức + hành vi khi vượt.

| Thuộc tính | Giá trị |
|---|---|
| `scope_type` | `organization`, `project`, `user`, `agent` |
| `period` | `daily`, `monthly`, `total` |
| `limit_amount`, `currency` | |
| `soft_threshold_pct` | Ví dụ 80: phát cảnh báo |
| `hard_action` | `block` (từ chối lời gọi mới) hoặc `alert_only` |

Budget theo execution và task không nằm trong bảng này: chúng là giới hạn của Agent Version (`limits.budget_usd`) và của kế hoạch Orchestrator, có vòng đời theo execution.

Một lời gọi LLM phải thoả **mọi** budget áp dụng: task → execution → agent → user → project → organization.

### 4.2 Thực thi: đặt trước rồi quyết toán

Vấn đề: chi phí chỉ biết chính xác **sau** khi gọi, và nhiều execution chạy song song cùng tiêu một budget.

1. **Reserve.** Trước khi gọi model, ước lượng trần chi phí: `input_tokens ước lượng × giá input + max_output_tokens × giá output`. Trừ nguyên tử vào mọi bộ đếm liên quan trên Redis (một Lua script). Bất kỳ bộ đếm nào không đủ → từ chối, hoàn lại phần đã trừ.
2. **Call.**
3. **Settle.** Ghi `usage_record` với số thật, hoàn chênh lệch giữa phần đặt trước và phần thật.
4. **Hết hạn đặt trước.** Reservation có TTL; nếu worker chết giữa chừng, phần đặt trước tự hoàn.

Postgres là nguồn sự thật; Redis là bộ đếm nhanh. Một job định kỳ dựng lại bộ đếm Redis từ ledger để sửa lệch. Mất Redis: dựng lại từ Postgres; trong lúc đó, mặc định **fail-closed** cho org có `hard_action = block`.

### 4.3 Hành vi khi chạm ngưỡng

| Sự kiện | Hành vi |
|---|---|
| Vượt ngưỡng mềm | Sự kiện cảnh báo (webhook / email), không ảnh hưởng execution |
| Chạm ngưỡng cứng ở task / execution | Dừng execution với `BUDGET_EXCEEDED`, trả kết quả dở dang |
| Chạm ngưỡng cứng ở agent / user / project / org | Từ chối execution mới (HTTP 402 với mã lỗi `budget_exceeded`); execution đang chạy dừng ở lời gọi LLM kế tiếp |
| Sắp chạm ngưỡng (tuỳ chọn) | Router hạ xuống tier rẻ hơn thay vì dừng |

### 4.4 Hiện thực (Phase 2, `internal/cost/budget`)

- **Cấu hình:** `GET / POST /v1/budgets`, `GET / PATCH / DELETE /v1/budgets/{id}` (quyền `budget:read`, `budget:write`; `PATCH` nhận `If-Match`). Chỉ USD.
- **Nơi thực thi:** nơi gọi model. Lượt gọi tới model có khoá ở Gateway được đặt trước và quyết toán **ở Gateway** (`internal/mcp/gateway/llm.go`), không dựa vào Runtime. Lượt gọi tới adapter chạy trong process (`claude-cli`, chỉ local) được kiểm ở bước `CallModel`: đủ mọi budget khi vòng lặp chạy trong process `api`, chỉ budget của task khi chạy ở `worker` (worker không nối bộ đếm dùng chung và không đọc được bảng `budgets`).
- **Bộ đếm:** `Counter` trên Redis (hai Lua script: đặt trước nguyên tử ở mọi bucket, quyết toán), hoặc trong bộ nhớ ở local. Mỗi budget một bucket theo kỳ; budget của task cũng là một bucket trong cùng lần đặt trước, nên các lượt gọi đồng thời của một task không cùng lọt qua. Bucket chưa có (kỳ mới, mất Redis) được nạp từ ledger (bucket của task: từ mức đã tiêu của task) rồi thử lại. Phần đặt trước hết hạn sau 12 phút; lần gọi lại cùng task + model thay thế phần đặt trước của lần trước.
- **Ước lượng trần:** `ceil(byte của request / 2)` token × giá đầu vào cao nhất (input hoặc cache write) + `max_output_tokens` × giá đầu ra cao nhất (output hoặc reasoning). Đây là ước lượng thận trọng, không phải bảo đảm; phần vượt được cộng vào bộ đếm lúc quyết toán.
- **Quyết toán:** theo token provider báo. Luồng bị ngắt sau khi provider đã bắt đầu xử lý được quyết toán theo usage dở dang (adapter trả usage kèm lỗi); lượt chưa được xử lý thì hoàn toàn bộ.
- **Fail-closed:** không dùng được bộ đếm mà có budget `block` (hoặc budget của task) → lượt gọi bị từ chối với `budget_unavailable` (503, thử lại được). Runtime chờ và gọi lại (1, 2, 4, 8 giây mỗi vòng); hết lần thử thì task kết thúc `failed` với mã `budget_unavailable`. Chỉ có budget `alert_only` thì cho qua.
- **Khi chạm ngưỡng:** lượt gọi nhận `budget_exceeded` (402 ở Gateway) → step đóng với mã đó → task và execution kết thúc `budget_exceeded`, giữ kết quả dở dang (văn bản của lượt model thành công gần nhất). Execution mới bị từ chối 402 khi một budget `block` đã chạm ngưỡng **theo ledger** (không tính phần đang đặt trước). Agent Version có `limits.budget` nhỏ hơn trần phần trả lời của một lượt bị từ chối 422 lúc tạo execution.
- **Ngưỡng mềm:** audit `budget.soft_threshold` (actor `system`) một lần mỗi kỳ. Webhook / email chưa có.
- **Audit:** `budget.create / update / delete`; Gateway ghi `llm.call` với `error_code` cho cả lượt bị từ chối vì budget.

Lệch so với thiết kế và nợ còn lại:

- Chuỗi "task → execution → agent → user → project → org": budget execution và task là một với execution đơn Agent. Với `mode: planned` ([agent-runtime §5.8](agent-runtime.md)), mỗi task bắt buộc có budget riêng, cấp tĩnh lúc tạo, và tổng không vượt budget của execution (nếu có); việc chặn vẫn theo task, budget của execution không phải một bucket được trừ lúc chạy. Delegation ([agent-runtime §5.9](agent-runtime.md)): task con nhận phần nhỏ hơn giữa budget của Agent đích và phần còn lại của task cha (sau khi giữ lại một lượt cho task cha), quyết định nguyên tử dưới khoá execution; mức đã tiêu của một task gồm cả các task con của nó, tính từ cây task. Còn nợ: bộ đếm ở Gateway vẫn theo từng task, nên phần chặn theo cây dựa vào `estimated_cost` do worker ghi; Gateway cần đặt trước vào bucket của cả chuỗi tổ tiên để không phụ thuộc số đó.
- Chưa có job định kỳ dựng lại bộ đếm từ ledger. Bộ đếm lệch thấp khi quyết toán lỗi hai lần liên tiếp (Redis gián đoạn); lệch được sửa khi bucket hết hạn hoặc sang kỳ mới.
- Mức đã tiêu dùng để nạp lại bộ đếm lấy từ dữ liệu do runtime ghi (`usage_records`, `agent_tasks.estimated_cost`). Lượt gọi mà Gateway đã quyết toán nhưng runtime không ghi usage (worker chết, hoặc lượt lỗi có tính phí) nằm trong bộ đếm nhưng không nằm trong ledger, và mất khỏi budget khi bộ đếm được dựng lại. Audit `llm.call` của Gateway (có token, kể cả lượt lỗi) là nguồn đối soát; chưa có job dùng nó.
- Budget phạm vi `user` không áp cho execution khởi tạo bằng API key (principal là key, `usage_records.user_id` rỗng).
- Kỳ `total` cộng trên phần ledger còn được giữ (retention 24 tháng); chưa có mốc cộng dồn bền vững.
- `current_period.reserved` không được trả về (process `api` không nối bộ đếm).
- Router chưa hạ tier khi budget sắp cạn (§5.3 luật 4).

## 5. Model Router

### 5.1 Nguyên tắc

Router là một hàm thuần: `(yêu cầu, chính sách, trạng thái provider, budget còn lại) → model + chuỗi fallback`. Bắt đầu bằng **luật tĩnh**, không dùng LLM để phân loại ([ADR-0010](../adr/0010-rule-based-model-router.md)).

### 5.2 Tier

Agent và platform nói chuyện bằng **tier**, không bằng tên model:

| Tier | Dùng cho |
|---|---|
| `economy` | Tóm tắt, trích xuất, phân loại, nén context |
| `standard` | Vòng lặp Agent thông thường với tool |
| `premium` | Lập kế hoạch multi-agent, phân tích nguyên nhân, tổng hợp cuối |

Ánh xạ tier → model cụ thể là cấu hình theo org / project, đổi được không cần deploy. Đây là cách đáp ứng yêu cầu "không hardcode model".

### 5.3 Luật chọn

Theo thứ tự:

1. Agent Version đặt `policy: fixed` → dùng đúng model đó.
2. Ràng buộc cứng: model phải hỗ trợ khả năng cần thiết (tool calling, độ dài context, structured output), provider được org cho phép, phân loại dữ liệu cho phép gửi tới provider đó.
3. `purpose` của lời gọi → tier (bảng trên).
4. Điều chỉnh: budget sắp cạn → hạ tier nếu Agent cho phép; lần thử lại sau lỗi chất lượng (output sai schema nhiều lần) → nâng tier một bậc, tối đa một lần.
5. Sức khoẻ provider: bỏ qua model đang mở circuit breaker; chọn model kế tiếp trong chuỗi fallback của tier.

Mọi quyết định của router được ghi vào step (`model_requested`, `model_used`, `routing_reason`) để đánh giá sau.

**Hiện thực (Phase 2, `internal/ai/router`):**

- Cấu hình: khoá `model_routing` trong `settings` của organization và project, dạng `{"tiers": {"<tier>": {"models": ["<model id>", …]}}}`. Mỗi tier là một chuỗi có thứ tự: model chính rồi tối đa ba model dự phòng. Project ghi đè organization theo từng tier. Cấu hình được kiểm khi ghi qua API (định dạng, model tồn tại trong tầm nhìn của tenant); cấu hình hỏng lúc đọc làm việc định tuyến dừng với lỗi, không âm thầm rơi về cấu hình của organization. Cột `models.tier` chỉ là nhãn của model, router không dùng nó.
- Luật đã có: 1 (`policy: fixed`), 2 một phần (tool calling khi Agent có tool; cửa sổ ngữ cảnh tối thiểu = `max_output_tokens` + 4096 token; model / provider đang bật; và ràng buộc của môi trường chạy: adapter có mặt, provider của org có khoá của org, model có giá hiệu lực bằng USD), 3 (`default_tier` của Agent Version, mặc định theo `purpose`: vòng lặp Agent là `standard`).
- Chưa có: luật 4 (hạ tier theo budget, nâng tier sau lỗi chất lượng: chờ budget và structured output), circuit breaker của luật 5 (mỗi lượt vẫn thử model chính trước), ràng buộc theo phân loại dữ liệu và danh sách provider được phép, ước lượng token theo từng lượt cho cửa sổ ngữ cảnh. Vì vậy "trạng thái provider, budget còn lại" ở §5.1 chưa là đầu vào của router.
- Chuỗi model được chốt lúc tạo task (`agent_tasks.model_chain`): đổi cấu hình không ảnh hưởng task đang chạy, và Gateway chỉ phục vụ model trong chuỗi của task.
- Dự phòng trong một lượt: model kế tiếp được thử ngay khi model đứng trước lỗi tạm thời (429 / 5xx / timeout của provider) hoặc không phục vụ được (bị tắt, mất khoá, provider từ chối khoá). Yêu cầu bị provider từ chối và lỗi của chính platform (Gateway quá tải, hết hạn mức) không chuyển model. Mỗi model nhận một phần bằng nhau của thời gian còn lại của bước. Khi cả chuỗi lỗi mà có lỗi tạm thời, cả chuỗi được thử lại với backoff, tối đa 3 vòng: **trần là 3 × độ dài chuỗi = 12 lời gọi provider cho một lượt.** Thứ tự này khác câu "tối đa 3 lần, rồi fallback" ở [agent-runtime.md §9](agent-runtime.md): chuyển model ngay rẻ hơn khi model chính quá tải; bảng ở đó đã được sửa theo.
- Ghi nhận: `execution_steps.routing` = `{policy, tier, model_requested_id, model_requested, model_used_id, model_used, fallback_index, reason, attempts[]}` cho cả step thành công lẫn thất bại; `attempts` là các lần gọi không có kết quả của lần chạy bước gần nhất (`model_id`, mã lỗi cố định, `retryable`). `execution_steps.model_id` và `usage_records.model_id` là model đã thực sự trả lời. Audit `execution.create` ghi tier, chuỗi đã chọn và model bị loại; audit `organization.update` / `project.update` ghi chuỗi model của từng tier; Gateway ghi audit `llm.call` cho cả lượt lỗi (kèm mã lỗi).
- Còn nợ: Gateway chưa có trần số lần gọi theo task (lượt lỗi không tăng `iterations`), chỉ có rate limit theo org + principal.

### 5.4 Lộ trình sau này

Chỉ cân nhắc router dựa trên phân loại độ phức tạp khi đã có dữ liệu: tỉ lệ thành công theo tier theo loại task. Không có dữ liệu này thì router "thông minh" chỉ là phỏng đoán có tốn phí.

## 6. Tối ưu token

Xếp theo tỉ lệ lợi ích / công sức, làm từ trên xuống:

| # | Kỹ thuật | Ghi chú |
|---|---|---|
| 1 | Không gọi LLM khi không cần | `direct_tool`, định tuyến tĩnh, tổng hợp deterministic |
| 2 | Chỉ đưa tool được phép vào prompt | Mô tả tool chiếm phần lớn input cố định mỗi lượt |
| 3 | Lọc kết quả tool tại Gateway | `max_result_bytes`, chọn trường, phân trang, artifact cho dữ liệu lớn |
| 4 | Prompt caching của provider | Sắp xếp context: phần ổn định (system prompt, định nghĩa tool) ở đầu, phần thay đổi ở cuối |
| 5 | Kết quả task có `summary` riêng | Agent khác đọc tóm tắt, không đọc toàn bộ |
| 6 | Nén lịch sử | Tóm tắt lượt cũ bằng tier `economy`; có chi phí riêng, chỉ kích hoạt khi vượt ngưỡng |
| 7 | Giới hạn `max_output_tokens` theo `purpose` | |
| 8 | Cache kết quả tool đọc, deterministic | TTL ngắn, khoá theo org + tool + hash arguments; chỉ bật cho tool được đánh dấu `cacheable` |

Không cache phản hồi LLM theo ngữ nghĩa ở giai đoạn đầu: tỉ lệ trúng thấp với tác vụ có tool, và rủi ro trả kết quả sai ngữ cảnh.

## 7. Giới hạn thực thi

Chi phí được chặn bằng giới hạn cấu trúc trước khi phải dùng tới budget:

| Giới hạn | Cấp |
|---|---|
| `max_iterations`, `max_tool_calls`, `timeout` | Agent Version |
| `max_tasks`, `max_depth`, `max_parallel_tasks` | Execution (theo project) |
| Số lần retry LLM, retry tool | Platform, cố định |
| Số execution đồng thời | Org, project |

## 8. Ước tính và chi phí thực

Hai con số khác nhau, lưu ở hai nơi, không trộn:

| | Ước tính | Thực tế |
|---|---|---|
| Nguồn | `usage_records.estimated_cost` = token × giá hiệu lực | Dữ liệu billing / usage export của provider |
| Độ chi tiết | Từng lời gọi | Theo ngày × model (tuỳ provider) |
| Có khi nào | Ngay lập tức | Trễ vài giờ tới vài ngày |
| Dùng cho | Budget, dashboard thời gian thực, phân bổ theo Agent / task | Đối soát, kế toán |

`cost_records` lưu chi phí thực theo `(org, provider, model, ngày)`, nạp bởi job đối soát. Job so tổng ước tính với thực tế cùng khoá, ghi `variance`. Lệch vượt ngưỡng (đề xuất khởi điểm 2%, điều chỉnh sau khi có số liệu) → cảnh báo: thường do bảng giá cũ, loại token chưa được tính, hoặc lời gọi không qua platform.

Dashboard luôn ghi rõ số đang hiển thị là ước tính hay đã đối soát.

Với provider không có API billing: `cost_records` nhập thủ công từ hoá đơn theo tháng; độ chi tiết thô hơn nhưng nguyên tắc không đổi.

## 9. Dashboard và API

Chỉ số cung cấp (đều lọc được theo org, project, khoảng thời gian):

- Tổng chi phí; theo ngày / tháng.
- Chi phí theo Agent, model, provider, user, workflow, tool.
- Cost per task và per execution: trung bình, p50, p95.
- Token input / output / cached; tỉ lệ cache hit.
- Tỉ trọng chi phí theo `purpose` (bao nhiêu phần trăm là planning, tóm tắt).
- Mức sử dụng budget theo từng budget.
- Dự báo cuối kỳ.

Hiện thực:

- Truy vấn trực tiếp trên `usage_records` (phân vùng theo tháng) cho khoảng thời gian ngắn.
- Bảng tổng hợp theo ngày `usage_daily`, cập nhật tăng dần bởi job, cho khoảng thời gian dài. Chỉ thêm khi truy vấn trực tiếp chậm thật; không dựng trước.
- **Forecast:** bắt đầu bằng ngoại suy tuyến tính theo mức tiêu thụ trung bình N ngày gần nhất trong kỳ. Hiển thị kèm khoảng dao động. Không dùng mô hình phức tạp hơn khi chưa có vài tháng dữ liệu.

**Đã hiện thực (Phase 5, lát "API tổng hợp", `internal/cost/ledger/report.go`):**

- `GET /v1/usage/summary`: tổng và nhóm theo tối đa hai chiều (`project`, `user`, `agent`, `model`, `provider`, `workflow`, `tool`, `purpose`), lọc theo project / agent / model / user, khoảng tối đa 92 ngày, tối đa 500 nhóm xếp theo chi phí giảm dần (`truncated` khi bị cắt). Mỗi nhóm có số lượt gọi, số execution, token, chi phí, chi phí trung bình mỗi execution, và `unpriced_calls` (lượt chưa có giá, không góp vào chi phí).
- `GET /v1/usage/timeseries`: theo ngày (≤ 92 ngày) hoặc tháng (≤ 400 ngày), mốc UTC, tuỳ chọn tách theo `project` / `agent` / `model` / `provider`; tối đa 5000 điểm.
- **Forecast** (`include_forecast=true`): tới hết tháng hiện tại (UTC), theo các bộ lọc (không theo `group_by`): chi phí từ đầu tháng + trung bình mỗi ngày của các ngày đã trọn × số ngày còn lại; `low` / `high` dùng ngày thấp nhất / cao nhất. `null` khi tháng chưa có ngày trọn nào.
- Quyền `usage:read` cấp organization; khi lọc theo `project_id` thì quyền trên project đó là đủ, trừ báo cáo tách hoặc lọc theo người dùng (`group_by=user`, `user_id`): chi tiêu của từng người cần quyền cấp organization. Project được đọc trong tenant trước khi kiểm quyền (không có thì 404), nên một `project_id` bịa ra không sinh dòng audit từ chối.
- Trần tải: mỗi process `api` chạy tối đa 4 báo cáo cùng lúc (quá thì 429), mỗi câu lệnh có `statement_timeout` 15 giây. Chưa có rate limit theo người gọi.
- Truy vấn trực tiếp trên `usage_records` (index `(org_id, occurred_at)`), không có bảng tổng hợp theo ngày: chưa có số đo cho thấy cần. Tên chiều chọn cột bằng `CASE` trên một danh sách cố định trong câu SQL; không có SQL nào được ghép từ input. Tiền được cộng bằng `NUMERIC` trong database (kể cả phép ngoại suy) và đi qua code ở dạng chuỗi thập phân. Chỉ cộng tiền USD: lượt có giá bằng tiền tệ khác vẫn được đếm lượt gọi và token, và nằm trong `unpriced_calls` cùng lượt chưa có giá. `avg_cost_per_execution` tính trên các execution có chi phí. Tổng và các nhóm (cũng như chuỗi và dự báo) là hai câu lệnh, có thể lệch nhau chút ít khi usage đang được ghi. Điểm của chuỗi thời gian chỉ có ở kỳ có usage, và kỳ đầu / cuối không trọn nếu `from` / `to` không nằm trên biên ngày / tháng.

Còn nợ của Phase 5: chiều `workflow` và `tool` luôn null (lượt model chưa ghi `workflow_id`; chưa có usage của tool); p50 / p95 chi phí mỗi execution; tỉ lệ cache hit; đối soát với billing của provider (`/costs/reconciliation`, cần dữ liệu hoá đơn thật); dashboard (UI); đa provider thật; đánh giá router; cache kết quả tool đọc. Tiêu chí "Xong khi" (lệch ước tính so với hoá đơn một tháng) cần dữ liệu billing thật, chưa kiểm được.

Metric vận hành (Prometheus): chi phí ước tính theo phút, số lần từ chối do budget, độ lệch bộ đếm Redis so với ledger.

## 10. Chi phí vận hành của chính platform

Ngoài chi phí LLM, cần theo dõi chi phí hạ tầng của platform. Các yếu tố chính và cách giữ thấp:

| Yếu tố | Ghi chú |
|---|---|
| Temporal | Tự vận hành cần Postgres riêng và vài process; hoặc dùng dịch vụ quản lý, tính theo action. Giữ history nhỏ (không chứa payload) làm giảm cả hai |
| Lưu trữ step | Nội dung prompt và kết quả tool là phần lớn nhất. Thời hạn giữ ngắn + nén + chuyển object storage khi cần |
| Log / trace | Sampling cho trace; không ghi nội dung prompt vào log |
| Worker | Phần lớn thời gian chờ I/O; một worker phục vụ được nhiều execution. Con số cụ thể xác định bằng load test |

Chưa đưa ra con số chi phí tuyệt đối vì phụ thuộc lưu lượng chưa biết. Roadmap Phase 1 có hạng mục đo chi phí hạ tầng trên mỗi execution để có căn cứ.
