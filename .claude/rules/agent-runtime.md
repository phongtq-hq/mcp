---
paths:
  - "internal/agent/**"
  - "internal/workflow/**"
  - "cmd/worker/**"
---

# Agent Runtime, Orchestrator, Temporal

Nguồn: `.docs/architecture/agent-runtime.md` · ADR-0002, ADR-0006.

## Mô hình

Agent (định danh) → Agent Version (bất biến) → Execution → Task → Step. Hai lifecycle tách biệt:

- Agent: `DRAFT → ACTIVE ⇄ DISABLED → ARCHIVED`. Không có trạng thái `EXECUTING` trên Agent.
- Execution / Task: `QUEUED → RUNNING ⇄ WAITING_APPROVAL / PAUSED → COMPLETED | FAILED | CANCELLED | BUDGET_EXCEEDED | TIMED_OUT`. Giữ các trạng thái kết thúc tách biệt; đừng gộp vào `FAILED`.

Sửa cấu hình = version mới. Task ghi `agent_version_id` nó chạy. Runtime không có nhánh code theo tên Agent.

## Vòng lặp

Kiểm tra giới hạn → `CallModel` → nếu có tool call: `AuthorizeToolCall` → (approval) → `CallTool` qua Gateway → chuẩn hoá kết quả → cập nhật context.

- Từ Phase 2: `runtime.Run` là thân của workflow Temporal (`internal/agent/runtime/durable`), chạy ở process `worker`; mọi thay đổi thứ tự / số lần gọi `Executor` là thay đổi workflow. Chỉ khi `HQ_ENV=local` và không có `HQ_TEMPORAL_ADDRESS`, vòng lặp chạy trong process `api` (`LocalRunner`, không bền vững).
- Vòng lặp nằm sau interface `Executor`; các bước nhận và trả dữ liệu **tuần tự hoá được**, không giữ con trỏ, connection hay closure qua ranh giới bước. Đây là điều kiện để Phase 2 chuyển thành activity mà không viết lại.
- Chạy song song **chỉ** tool `side_effect = none`. Tool ghi chạy tuần tự theo thứ tự model đưa ra.
- Tool bị deny hoặc approval bị từ chối: trả về model như kết quả tool; không tự thử lại cùng hành động. Deny lặp N lần → `FAILED`.
- Vượt `max_iterations` / `max_tool_calls` → `FAILED` (`limit_exceeded`) kèm kết quả dở dang.

## Idempotency

- Step ghi theo khoá `(task_id, step_seq)`; retry không tạo bản ghi trùng.
- Tham chiếu đi qua Temporal (`TaskRef`, `CallRef`, `Outcome`) không được tin: bước đối chiếu với database, đọc tên tool và arguments từ step, sinh thông báo lỗi từ mã lỗi.
- Usage được ghi cho mọi lượt provider đã trả lời, kể cả khi step đã bị đóng (huỷ, hết hạn). `usage_record` ghi trong cùng activity, cùng khoá.
- Tool có side effect nhận `idempotency_key = task_id:step_seq`.
- Hiện thực (security §4.5): Gateway giữ chỗ `(task, step_seq)` trước khi gọi tool không chỉ đọc; bước chạy lại nhận `outcome_unknown` thay vì chạy lần hai. Bước `CallTool` không được ghi nhận một tool có thể đã chạy là "thất bại" / "chưa thực thi"; không đóng được step thì task dừng. Chờ approval là `Executor.AwaitApproval`, rồi gọi lại đúng bước đó.
- Tool `non_idempotent`: retry tối đa 1 lần. Kết quả không xác định (timeout sau khi đã gửi) → step `UNKNOWN`, execution dừng chờ người. **Không đoán, không tự retry.**

## Temporal (từ Phase 2)

- Code workflow **deterministic**: không `time.Now()`, không random, không I/O, không goroutine / channel / select gốc của Go, không duyệt map theo thứ tự ngẫu nhiên. Dùng API của workflow SDK. Mọi I/O là activity.
- History chỉ chứa **tham chiếu** tới `execution_steps`; không chứa prompt, kết quả tool hay secret.
- Thay đổi logic workflow đang có instance chạy cần versioning và test replay trong CI. Mốc đã có: `control` (`workflow.GetVersion`, version 1 = đọc huỷ + tạm dừng bằng local activity `Control`). Thêm lệnh mới cho workflow đang chạy thì thêm mốc / version mới, không sửa nghĩa của version đã phát hành. Lịch sử mẫu `*_v0.json` không bao giờ ghi lại; test ghi ra `*_v1.json`.
- Task dài dùng continue-as-new sau mỗi N step.
- `org_id` trong workflow ID và search attribute. Isolated runtime = task queue riêng, cùng code.
- Retry policy khai báo theo loại activity; lỗi phân loại retryable / non-retryable.
- Huỷ lan từ workflow cha xuống child workflow, activity (qua context) và approval đang chờ.

## Orchestrator

- Hiện thực (agent-runtime §5.8): `mode: planned` nhận kế hoạch trong request; `internal/agent/orchestrator` là phần **thuần** (kiểm kế hoạch, `orchestrator.Run` sau interface `Engine`, không import package nào của dự án, không đồng hồ); việc tạo execution và các bước có I/O nằm trong `internal/agent/runtime` (`plan.go`). `orchestrator.Run` là thân của workflow cha `PlannedExecution`: đổi thứ tự / số lần gọi `Engine` là đổi workflow (versioning + lịch sử mẫu mới). Mỗi task là child workflow `AgentTask` chạy `runtime.Run`. Activity của kế hoạch không tin thứ tự do workflow đưa: `BeginTask` kiểm phụ thuộc trong database, `FinishPlan` suy trạng thái từ database. Mỗi task của `planned` bắt buộc có budget.
- Nút `wait` và `approval` (agent-runtime §5.12, ADR-0015): nút của kế hoạch không phải task (`orchestrator.IsStep`), trạng thái ở `execution_plan_steps`; chỉ lần chạy của workflow người dùng tạo được chúng. Approval của nút `approval` chỉ được tạo / đóng qua hàm database (`plan_step_request_approval`, `plan_step_close_approval`): nội dung và kết quả không đến từ nơi chạy kế hoạch. Các bước `BeginPlanStep` / `FinishPlanStep` / `SkipPlanStep` theo cùng luật "không tin thứ tự do workflow đưa" (kiểm phụ thuộc, hạn và "đã chờ đủ" trong database). Thêm loại nút mới: thêm vào `CHECK` của bảng, `planDepsDone`, cả hai engine (`localEngine`, `planEngine`) và ghi lịch sử mẫu mới.
- Thử lại theo bước của workflow (`max_attempts`, agent-runtime §5.12): mỗi lần thử là một task được chụp sẵn trong kế hoạch (`<id>-try2`…), nối bằng `retry_of`; luật ở `orchestrator.Run` / `Effective` và kiểm lại ở `planDepsDone`. Mọi nơi đọc "phụ thuộc đã xong chưa" hay "kết quả của bước" phải đi qua `Effective` / `storedPlan.effective`, không so thẳng trạng thái của nút cuối.
- Chọn chế độ rẻ nhất trước: `direct_tool` → `single_agent` → `planned`. `planned` phải bật tường minh theo project.
- Kế hoạch là dữ liệu, validate bằng code (Agent tồn tại và được phép, không chu trình, số task và độ sâu trong giới hạn, tổng budget không vượt). Kế hoạch không hợp lệ bị **từ chối, không "sửa giúp"**.
- Hiện thực delegation (agent-runtime §5.9, ADR-0014): `delegate_task` là tool nội bộ của runtime (không qua Gateway); điểm quyết định là hàm database `agent_task_delegate` (tự ghi audit); Agent đích chụp lúc tạo execution (`execution_delegation_*`); chạy tuần tự qua `Executor.AwaitTask` (child workflow `AgentTask`); mức đã tiêu của task gồm cả task con (`agent_task_spent`). Thêm luật cho delegation là sửa hàm bằng migration mới và thêm ca vào `tests/tenant/delegation_test.go`; danh sách tool đưa cho model (`delegationView.allowed`) phải áp cùng luật.
- Agent không gọi Agent. Delegation là tool `delegate_task` đi tới Orchestrator, qua policy: `can_delegate_to`, `max_depth`, không chu trình tổ tiên, budget lấy từ phần còn lại của cha.
- Kết quả task có `summary` (vào context Agent khác) và `details` (truy xuất khi cần).
- Xung đột nhận định: ghi nhận cả hai kèm bằng chứng. Xung đột hành động: khoá theo `resource_key`, thực thi tuần tự.

## Context

Thứ tự khi vượt ngân sách token: lọc kết quả tool tại nguồn → thay kết quả tool cũ bằng tóm tắt → tóm tắt lượt cũ bằng tier `economy`. Memory là dữ liệu, chèn trong khối đánh dấu rõ; không thay đổi được quyền. Chưa dùng pgvector.
