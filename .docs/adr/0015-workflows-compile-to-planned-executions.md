# ADR-0015: Workflow do người dùng định nghĩa được biên dịch thành execution `planned`; định dạng định nghĩa

Trạng thái: Đề xuất · 2026-11-18

## Bối cảnh

Roadmap Phase 3: "Workflow do người dùng định nghĩa (DAG khai báo: bước agent, tool, approval, wait), được một workflow Temporal tổng quát diễn giải", kèm lịch chạy, retry, timeout, compensation và bản chiếu `workflow_executions`. Định dạng định nghĩa là một quyết định còn mở (README của thư mục này).

Sau Phase 2, platform đã có một bộ chạy DAG: execution `mode: planned` ([agent-runtime §5.8](../architecture/agent-runtime.md)). Nó kiểm kế hoạch bằng code, chụp Agent Version / chuỗi model / budget lúc tạo, chạy song song có trần, xử lý thất bại từng phần, lan truyền huỷ, tạm dừng, approval cho tool ghi, và có lịch sử mẫu cho test replay. Việc tạo task luôn nằm ở process `api`; process chạy vòng lặp không tạo được task.

Câu hỏi: workflow của người dùng chạy bằng gì, và ai tạo các task của một lần chạy khi lần chạy đó do **lịch** kích hoạt (không có request HTTP nào tới `api`).

## Quyết định

1. **Định nghĩa workflow là dữ liệu JSON** lưu trong `workflows.definition`: danh sách bước, mỗi bước có `id`, `type` (`agent` | `wait` | `approval`; `tool` thêm sau, xem mục 5), `depends_on`, và trường theo loại (`agent_id` + `goal`, `seconds`, `on_failure`, `budget`). Không có biểu thức, biến hay template: một bước chỉ nhận `goal` tĩnh của nó và tóm tắt kết quả của các bước nó phụ thuộc. Định nghĩa được kiểm bằng code khi ghi và kiểm lại khi chạy.
2. **Một lần chạy workflow là một execution `planned`.** Process `api` biên dịch bản chụp của định nghĩa thành một kế hoạch và tạo execution qua đúng đường của `mode: planned`. Không có bộ diễn giải thứ hai: workflow Temporal "tổng quát" là `PlannedExecution` đã có, mở rộng thêm loại nút không phải Agent (`wait`). `workflow_executions` là bản chiếu trỏ tới execution đó.
3. **Mỗi workflow có một người chạy (`run_as`), và lần chạy theo lịch mang quyền của người đó.** Người chạy là người chịu trách nhiệm về thứ workflow chạy: người kích hoạt nó, hoặc người gần nhất đã sửa `definition` / `schedule` trong lúc nó `active`. Mọi thao tác như vậy ghi người gọi làm người chạy, đòi người gọi là user và tự có `workflow:run` + `execution:create`; API key không là người chạy được. Lý do: nếu sửa definition giữ nguyên người chạy, một người chỉ có `workflow:write` chạy được nội dung của mình dưới quyền người khác, và (khi có `approval:decide`) tự duyệt được hành động do chính mình viết, vì luật "người duyệt ≠ người yêu cầu" so với người chạy. Quyền của người chạy được kiểm lại ở mỗi lần lịch kích hoạt (user, membership, organization, `workflow:run`, `execution:create`) và được Gateway kiểm lại ở mỗi lời gọi như mọi execution.
4. **Lịch chạy dùng Temporal Schedules. Việc tạo lần chạy khi lịch kích hoạt là một activity chạy trong process `api`**, trên task queue riêng (`api-internal`) chỉ `api` poll. Workflow được lịch khởi chạy chỉ gọi activity đó với id của workflow; `api` đọc lại mọi thứ từ database và đi qua đúng các kiểm tra của một lần chạy thủ công. Khoá của một lần kích hoạt là workflow ID mà Temporal Schedules sinh ra (`workflow-run/<org>/<workflow>-<thời điểm>`), đọc từ thông tin của activity chứ không từ tham số; nó khử trùng lần chạy (`workflow_executions.trigger_key`), và lần thử lại làm tiếp từ đúng lần chạy đã ghi.
5. Bước `approval` (cổng chờ người) và bước `tool` (gọi một tool không qua LLM) được thêm bằng cách mở rộng loại nút của kế hoạch, không phải bằng một đường chạy khác. Bước `approval` đã có: approval của nó được tạo và tiêu thụ, và trạng thái của nút được ghi, bởi hàm trong database từ cấu hình đã chụp của nút (cùng mẫu với ADR-0014): process chạy kế hoạch không được là nơi quyết định một cổng đã mở. Database cũng từ chối bước đứng sau một cổng chưa mở, và Gateway kiểm điều đó ở mỗi lời gọi. Bước `tool` cần Gateway nhận một lời gọi không gắn với Agent Version, nên được làm sau và có phần riêng trong tài liệu khi làm.

## Phương án đã cân nhắc

- **Bộ diễn giải Temporal riêng cho workflow.** Tách bạch về khái niệm, nhưng phải viết lại (và giữ đồng bộ) kiểm kế hoạch, budget, huỷ, tạm dừng, thất bại từng phần, và một bộ lịch sử mẫu thứ hai.
- **Worker tự tạo lần chạy từ một ảnh chụp đã biên dịch, qua hàm database** (mẫu của delegation, ADR-0014). Không đổi vai trò của `api`, nhưng ảnh chụp cũ dần (Agent Version, model), và thêm một hàm `SECURITY DEFINER` nhiều luật nữa: toàn bộ việc tạo execution và task.
- **Process lịch riêng (`cmd/scheduler`).** Đã loại ở overview §8: Temporal Schedules làm việc này.
- **Template / biểu thức trong định nghĩa** (truyền output của bước này làm input của bước khác). Linh hoạt, nhưng đưa output của model vào arguments của tool mà không qua bước nào kiểm: đúng mẫu tấn công T1. Để sau, và chỉ với kiểu dữ liệu có schema.

## Hệ quả

- Mọi thứ của `planned` áp cho workflow mà không viết lại: trần 16 bước, chuỗi phụ thuộc dài tối đa 8, mỗi bước Agent bắt buộc có budget, approval cho tool ghi, huỷ, tạm dừng, luồng sự kiện.
- **`api` trở thành một Temporal worker** cho đúng một loại activity. Bảng "ba process" đổi: `api` nhận việc từ Temporal trên một task queue riêng. `api` không tin tham số của activity hơn một request HTTP: nó chỉ nhận id, đọc lại workflow trong tenant, và chỉ chạy workflow đang `active` có lịch. Ai ghi được vào namespace Temporal (kể cả `worker` bị chiếm quyền, vì chưa có phân quyền namespace) thì kích hoạt được lần chạy của workflow đang có lịch, dưới danh nghĩa người chạy của nó. Để điều đó không rộng hơn điều lịch vốn làm, `api` tự giữ các trần trong database thay vì tin nơi kích hoạt: khoá phải có dạng của lịch đúng workflow đó; hai lần chạy theo lịch của một workflow cách nhau ít nhất 4 phút (lịch hợp lệ không dày hơn 5 phút); lần kích hoạt bị bỏ khi lần chạy theo lịch trước chưa kết thúc. Tham chiếu trỏ tới workflow không tồn tại trong org được nêu không ghi gì vào audit của org đó. Phần còn lại (kích hoạt sớm một workflow đang có lịch, trong các trần trên) là rủi ro được chấp nhận cho tới khi có phân quyền namespace.
- `internal/workflow/definitions/schedule` là package duy nhất của context Workflow import Temporal SDK (rủi ro R4): danh sách cho phép của `depguard` và `tests/arch` thêm đúng package này. `cmd/worker` và `cmd/gateway` không link `internal/workflow/definitions` (kiểm ở `tests/arch`), nên hàng đợi `api-internal` chỉ `api` poll được.
- Workflow thừa hưởng cổng của `planned`: project chưa bật `settings.orchestration.planned_enabled` thì mọi lần chạy bị từ chối (`planned_not_enabled`).
- Lần kích hoạt bị từ chối vì lý do thử lại không đổi được (người chạy mất quyền, Agent bị tắt, `planned` bị tắt, budget đã chạm) được ghi audit `workflow.schedule_skip` và bỏ qua; ba lần liên tiếp thì workflow tự về `disabled` và lịch bị xoá (`workflow.auto_disable`). Lỗi tạm thời (database, Temporal, org đang chạm trần đồng thời) được thử lại tối đa 5 lần rồi bỏ lần kích hoạt đó; lịch lỡ quá một phút không chạy bù.
- Liên kết lần chạy ↔ execution có hai đầu: `agent_executions.workflow_execution_id` (nguồn sự thật: khoá ngoại ghép, unique, bất biến) và `workflow_executions.execution_id` (bản ghi lại, gắn ở transaction thứ hai). Process dừng giữa hai transaction để lại lần chạy chưa gắn; nó được làm tiếp bởi lần thử lại của lịch, hoặc được sửa khi có người đọc sau 2 phút.
- Trần: 200 workflow chưa lưu trữ mỗi project, 50 workflow đang có lịch mỗi organization. Hàng đợi `api-internal` dùng chung mọi tenant (4 activity đồng thời mỗi process `api`); chưa có trần công bằng theo org.
- Thứ tự deploy: mọi `worker` phải chạy code biết nút `wait` trước khi `api` tạo kế hoạch có nút đó.
- Đồng bộ lịch sang Temporal chạy sau khi database commit; lỗi được báo cho người gọi để gửi lại. Chưa có job đối soát: lịch thừa tự được dọn ở lần kích hoạt kế tiếp, lịch thiếu hoặc mang cron cũ thì không.
- Trần của `planned` cũng là trần của workflow: workflow lớn hơn 16 bước phải chia nhỏ.
- Không có compensation ở phiên bản này: bước Agent không có "hành động ngược" khai báo được. `cancel` huỷ execution; trạng thái `compensating` / `compensated` trong thiết kế chưa dùng.
- Bản chiếu `workflow_executions` lấy trạng thái từ execution của nó khi được đọc (đồng bộ lười), không qua một job nền.

## Điều kiện xem xét lại

- Cần workflow dài hơn trần của một kế hoạch, hoặc cần vòng lặp / nhánh điều kiện: khi đó mới cần một bộ diễn giải riêng.
- Số lần chạy theo lịch đủ lớn để activity ở `api` thành điểm nghẽn.
- Cần truyền dữ liệu có cấu trúc giữa các bước.
