# Agent Runtime và Orchestrator

Trạng thái: **Đề xuất** · Liên quan: [ADR-0002](../adr/0002-temporal-durable-execution.md), [ADR-0006](../adr/0006-agent-definition-vs-execution-lifecycle.md), [ADR-0010](../adr/0010-rule-based-model-router.md)

## 1. Mô hình khái niệm

| Khái niệm | Ý nghĩa | Vòng đời |
|---|---|---|
| **Agent** | Định danh ổn định của một vai trò (DevOps Agent...) | Dài hạn |
| **Agent Version** | Ảnh chụp bất biến của toàn bộ cấu hình: prompt, model, tool được phép, giới hạn | Bất biến sau khi publish |
| **Session** | Một cuộc hội thoại / ngữ cảnh làm việc với người dùng; tuỳ chọn | Phút → ngày |
| **Execution** | Một lần xử lý một yêu cầu, từ lúc nhận tới lúc có kết quả cuối. Là đơn vị tính budget, huỷ, audit | Giây → giờ |
| **Task** | Đơn vị công việc trong một execution, giao cho đúng một Agent Version. Tạo thành cây (delegation) và đồ thị phụ thuộc | Trong execution |
| **Step** | Một hành động nguyên tử trong task: một lần gọi LLM, một lần gọi tool, một lần chờ approval | Mili giây → phút |

Execution đơn Agent là trường hợp đặc biệt: một execution, một task.

## 2. Hai lifecycle tách biệt

Đề bài gộp hai thứ khác nhau vào một chuỗi trạng thái (`CREATED → CONFIGURED → ACTIVE → EXECUTING → COMPLETED/FAILED/PAUSED`). Một Agent phục vụ nhiều execution đồng thời nên "Agent đang EXECUTING" không có nghĩa xác định. Thiết kế tách làm hai:

**Agent (định nghĩa)**

```
DRAFT ──publish──▶ ACTIVE ──disable──▶ DISABLED ──archive──▶ ARCHIVED
                     ▲                     │
                     └──────enable─────────┘
```

- `DRAFT` tương ứng CREATED/CONFIGURED: chưa có version nào được publish.
- Sửa cấu hình = tạo version mới. Execution đang chạy giữ nguyên version nó bắt đầu.
- `DISABLED`: không nhận execution mới; execution đang chạy tiếp tục hoặc bị huỷ tuỳ tham số.

**Execution và Task**

```
QUEUED → RUNNING ⇄ WAITING_APPROVAL
            ⇅
          PAUSED
RUNNING → COMPLETED | FAILED | CANCELLED | BUDGET_EXCEEDED | TIMED_OUT
```

Trạng thái kết thúc được phân biệt rõ vì mỗi loại cần phản ứng vận hành khác nhau (ví dụ `BUDGET_EXCEEDED` không nên retry tự động).

## 3. Cấu hình Agent Version

```yaml
name: devops-agent
role: Vận hành hạ tầng production
system_prompt: |                           # nằm trong version, nên đổi prompt = version mới
  Bạn là DevOps Agent...
model:
  policy: tiered                           # fixed | tiered (xem Model Router)
  default_tier: standard
  fixed_model: null
  temperature: 0.2
  max_output_tokens: 4096
limits:
  max_iterations: 15
  max_tool_calls: 40
  timeout: 15m
  budget_usd: 2.00                         # trần cho một execution
tools:
  - server: kubernetes
    allow: [get_pods, get_logs, describe]  # read: tự do
  - server: kubernetes
    allow: [rollout_restart]
    require_approval: always               # ghi đè mức mặc định của tool nếu chặt hơn
  - server: docker
    allow: ["*"]
    deny: [system_prune]
delegation:
  can_delegate_to: [database-agent]
  max_depth: 2
memory:
  session_history: summarized              # none | full | summarized
  long_term: disabled
runtime:
  isolation: shared                        # shared | isolated
```

Thêm Agent mới chỉ là thêm cấu hình. Runtime không có nhánh code nào theo tên Agent.

Ràng buộc khi publish: mọi tool tham chiếu phải tồn tại trong Registry và nằm trong phạm vi quyền của project; `require_approval` của Agent chỉ được **chặt hơn** mức rủi ro của tool, không được nới lỏng.

## 4. Vòng lặp Agent

Mỗi task chạy như một Temporal workflow. Logic vòng lặp là code deterministic; mọi I/O là activity.

**Hiện thực (Phase 2, lát "Temporal + worker"):** `runtime.Run` là thân workflow `AgentExecution` (`internal/agent/runtime/durable`), task queue `agent-shared`, workflow ID `execution/<org>/<execution>`, search attribute `OrgId` / `ProjectId`. Activity: `Begin`, `ListTools`, `CallModel`, `AuthorizeToolCall` (chỉ khi từ chối), `CallTool`, `Finish`; `Cancelled` là local activity. Activity có lời gọi ra ngoài bị chặn bởi thời gian còn lại của task (`ScheduleToCloseTimeout`), heartbeat 15 giây, và workflow chờ activity dừng hẳn khi bị huỷ, nên yêu cầu huỷ tới một lời gọi đang chạy sau vài giây. Hội thoại không đi qua Temporal: mỗi lượt model dựng lại nó từ `execution_steps`. Chế độ không Temporal (`LocalRunner` trong process `api`) chỉ còn cho `HQ_ENV=local` và test.

Chờ approval: bước `CallTool` trả về "đang chờ approval X" khi Gateway yêu cầu duyệt; workflow chuyển task sang `waiting_approval`, ngủ trên signal `approval-resolved` (api gửi khi có quyết định) và một timer 2 phút làm lưới an toàn, rồi gọi lại **đúng bước đó**; Gateway là nơi xét quyết định. Chế độ trong process thì đọc trạng thái approval mỗi 500 ms.

Tool call và tác dụng phụ: bước `CallTool` không bao giờ ghi nhận một tool có thể đã chạy là "thất bại" hay "chưa thực thi". Lời gọi có thể đã được gửi mà không có câu trả lời (bước trước bị ngắt, mất phản hồi của Gateway) thì chỉ câu trả lời của Gateway cho chính bước đó mới đóng được step; không có thì step để mở cho lần thử sau, và ở lần thử cuối là `unknown` (trừ tool chỉ đọc). Vòng lặp dừng task khi một bước tool không đóng được. Lệch thiết kế, có chủ ý: activity `CallTool` có 3 lần thử cho mọi tool (thiết kế ghi "tối đa 1 lần" cho tool `non_idempotent`); an toàn dựa vào việc Gateway giữ chỗ từng bước chứ không dựa vào số lần thử. `UNKNOWN` hiện kết thúc task (`failed` / `tool_outcome_unknown`) thay vì "dừng chờ người".

Chưa làm, hoãn có chủ ý (luồng sự kiện SSE nay đã có, §4.1): continue-as-new (với trần hiện tại 100 lượt / 500 tool call, lịch sử tệ nhất khoảng vài nghìn event, dưới ngưỡng của Temporal. Đã xem lại khi thêm child workflow ở §5.8: lịch sử của workflow cha chỉ có tối đa 16 child và vài activity, lịch sử của mỗi child bằng một execution đơn Agent, nên vẫn chưa cần; xem lại khi nâng trần hoặc khi có delegation); quy trình versioning workflow (cần chốt trước lần deploy thứ hai có execution đang chạy; hiện chỉ có test replay trên lịch sử mẫu); task queue theo ưu tiên và isolated runtime; lan truyền trace qua Temporal; job quét execution mồ côi (hiện đóng khi có người đọc); luồng sự kiện SSE.

```
khởi tạo context (system prompt + task input + memory đã chọn)
lặp cho tới khi dừng:
  1. kiểm tra giới hạn: iterations, tool calls, deadline, budget còn lại
  2. activity CallModel        → ghi execution_step + usage_record
  3. nếu model trả lời cuối    → kết thúc COMPLETED
  4. với mỗi tool call model yêu cầu:
       a. activity AuthorizeToolCall → allow | deny | require_approval
       b. nếu require_approval → tạo approval, chờ signal (có hạn)
       c. activity CallTool (qua MCP Gateway) → ghi execution_step
       d. chuẩn hoá kết quả: cắt, lọc, tóm tắt nếu vượt ngưỡng
  5. cập nhật context, nén nếu vượt ngân sách token
```

Điều kiện dừng và trạng thái tương ứng:

| Điều kiện | Kết quả |
|---|---|
| Model trả lời cuối | `COMPLETED` |
| Vượt `max_iterations` / `max_tool_calls` | `FAILED` (lý do `limit_exceeded`), kèm kết quả dở dang |
| Vượt deadline | `TIMED_OUT` |
| Hết budget | `BUDGET_EXCEEDED` |
| Nhận cancel | `CANCELLED` |
| Tool bị deny | Trả lỗi về cho model như một kết quả tool; model có thể chọn hướng khác. Deny lặp lại N lần → `FAILED` |
| Approval bị từ chối hoặc hết hạn | Trả về model như kết quả tool "bị từ chối"; không tự thử lại cùng hành động |

### Vì sao tool call song song cần thận trọng

Model có thể yêu cầu nhiều tool call trong một lượt. Runtime chạy song song **chỉ** các tool có `side_effect = none`. Tool ghi được chạy tuần tự theo thứ tự model đưa ra, để kết quả audit có thứ tự xác định và để một lần deny chặn được các bước sau.

### Tính bền vững và idempotency

- Mỗi step có `step_seq` tăng dần trong task. Activity ghi step theo khoá `(task_id, step_seq)`, nên retry activity không tạo bản ghi trùng.
- Tool có side effect nhận `idempotency_key = task_id:step_seq`. MCP server managed phải tôn trọng khoá này. Với server ngoài không hỗ trợ, tool được đánh dấu `non_idempotent` và activity dùng retry policy "tối đa 1 lần"; khi kết quả không xác định (timeout sau khi đã gửi), step chuyển `UNKNOWN` và execution dừng chờ người xử lý thay vì đoán.
- Lịch sử Temporal không chứa prompt hay kết quả tool đầy đủ, chỉ chứa tham chiếu tới `execution_steps`. Việc này giữ history nhỏ và tránh đưa dữ liệu nhạy cảm vào Temporal.
- Task dài dùng continue-as-new sau mỗi N step.

### 4.1 Luồng sự kiện (Phase 2)

`GET /v1/executions/{id}/events` (Server-Sent Events, quyền `execution:read`). Handler ở process `api` đọc trạng thái của execution, các task và các step từ database theo nhịp khoảng một giây, và phát một sự kiện cho mỗi thay đổi (`task`, `step`, `execution`), rồi `end` khi execution kết thúc. Mỗi lần nối là một lần phát lại trạng thái hiện tại rồi tới thay đổi; không có `Last-Event-ID`. Sự kiện chỉ mang định danh và trạng thái (kể cả `approval_id` của step đang chờ duyệt), không có prompt, arguments hay kết quả tool. Trần: 64 luồng mỗi process, 16 mỗi organization, 4 mỗi người gọi, 5 phút mỗi luồng (sau đó `reconnect`). Quyền đọc được kiểm lại mỗi 30 giây trong lúc luồng mở.

Lựa chọn có chủ ý: nguồn là database, không thêm hạ tầng (broker, `LISTEN / NOTIFY`), theo nguyên tắc "thêm hạ tầng khi có bằng chứng". Cái giá: độ trễ tới một nhịp, và mỗi luồng tốn vài truy vấn theo index mỗi nhịp. Xem lại khi số luồng đồng thời cần vượt trần hiện tại. Còn nợ: luồng đang mở không tự đóng khi process shutdown (việc tắt process phải chờ hết thời gian chờ của server); API key bị thu hồi vẫn nghe được tới lần kiểm quyền kế tiếp hoặc hết thời gian sống của luồng.

## 5. Orchestrator và multi-agent

### 5.1 Khi nào dùng nhiều Agent

Orchestrator chọn chế độ theo thứ tự rẻ nhất trước:

1. `direct_tool`: request chỉ rõ tool. Không LLM.
2. `single_agent`: request chỉ rõ Agent, hoặc luật định tuyến tĩnh khớp (theo project, theo loại yêu cầu).
3. `planned`: cần phân rã. Một lần gọi LLM tạo kế hoạch dạng DAG các task, bị ràng buộc bởi danh sách Agent được phép.

Chế độ `planned` phải được bật tường minh theo project. Mặc định là 1 và 2.

### 5.2 Kế hoạch là dữ liệu, được kiểm tra trước khi chạy

Planner trả về cấu trúc:

```json
{
  "tasks": [
    {"id": "t1", "agent": "monitoring-agent", "goal": "...", "depends_on": []},
    {"id": "t2", "agent": "database-agent",   "goal": "...", "depends_on": []},
    {"id": "t3", "agent": "devops-agent",     "goal": "...", "depends_on": ["t1","t2"]}
  ]
}
```

Orchestrator validate bằng code: Agent tồn tại và người gọi có quyền dùng; đồ thị không có chu trình; số task và độ sâu trong giới hạn; tổng budget phân bổ không vượt budget execution. Kế hoạch không hợp lệ bị từ chối, không "sửa giúp".

### 5.3 Thực thi

- Execution = workflow cha. Mỗi task = child workflow. Task không phụ thuộc nhau chạy song song, giới hạn bởi `max_parallel_tasks`.
- **Budget phân cấp:** task con được cấp một phần budget của execution; không tiêu được quá phần đó; phần dư trả về cha.
- **Cancellation:** huỷ execution lan tới mọi child workflow, mọi activity đang chạy (qua context) và mọi approval đang chờ.
- **Thất bại từng phần:** mỗi task khai báo `on_failure: fail_execution | continue | skip_dependents`. Mặc định `skip_dependents`.

### 5.4 Delegation

Agent không gọi Agent. Agent được cấp một tool nội bộ `delegate_task(agent, goal, context_refs)`; lời gọi này tới Orchestrator, nơi policy engine kiểm tra:

- Agent đích nằm trong `can_delegate_to` của Agent nguồn.
- Độ sâu ≤ `max_depth`; tổng số task của execution ≤ trần.
- Không có chu trình (A → B → A) trong chuỗi tổ tiên.
- Quyền của task con = giao của quyền Agent đích và quyền của người khởi tạo execution. Delegation không bao giờ làm tăng quyền.
- Budget cấp cho con lấy từ phần còn lại của cha.

### 5.5 Context dùng chung và context cô lập

- **Isolated context (mặc định):** mỗi task có lịch sử hội thoại riêng. Task con chỉ nhận `goal` và các artifact được chỉ định.
- **Shared task context:** một "bảng tin" theo execution, lưu trong `agent_tasks.output` và các artifact có cấu trúc (finding, đề xuất, bằng chứng). Agent đọc bằng tool `read_task_results`, không được tiêm toàn bộ vào prompt.
- Kết quả task luôn có hai phần: `summary` (ngắn, đưa vào context của Agent khác) và `details` (đầy đủ, truy xuất khi cần). Đây là cơ chế chính để nhiều Agent không nhân chi phí token lên theo cấp số.

### 5.6 Tổng hợp và xử lý xung đột

- Tổng hợp là một bước tường minh ở cuối: deterministic nếu chỉ cần gộp danh sách; gọi LLM nếu cần suy luận nguyên nhân.
- Xung đột về **nhận định** (hai Agent kết luận khác nhau): bước tổng hợp ghi nhận cả hai kèm bằng chứng, không chọn im lặng.
- Xung đột về **hành động** (hai Agent cùng muốn tác động một tài nguyên): Agent ở giai đoạn phân tích chỉ tạo *đề xuất* remediation. Orchestrator gộp đề xuất thành một kế hoạch, khoá theo tài nguyên (`resource_key`, ví dụ `k8s:prod/deployment/api`), thực thi tuần tự trên mỗi tài nguyên.

### 5.7 Ví dụ "Kiểm tra production và xử lý lỗi"

| Giai đoạn | Thực hiện | Ghi chú |
|---|---|---|
| Chẩn đoán | Monitoring, DevOps, Database, Security Agent chạy song song, chỉ dùng tool read | Không cần approval |
| Tổng hợp | Gộp finding, xác định nguyên nhân | 1 lần gọi LLM |
| Lập kế hoạch remediation | Danh sách hành động, mỗi hành động có mức rủi ro và `resource_key` | Dữ liệu có cấu trúc |
| Approval | Một approval cho cả kế hoạch, hiển thị từng hành động; hành động rủi ro cao có thể cần người duyệt riêng | Execution ở `WAITING_APPROVAL` |
| Thực thi | Tuần tự theo tài nguyên; dừng ở lỗi đầu tiên | Tool call gắn với approval đã duyệt |
| Xác minh | Chạy lại các kiểm tra của giai đoạn chẩn đoán cho phần bị tác động | So sánh trước / sau |

### 5.8 Hiện thực (Phase 2: `mode: planned`)

**Đã có.** Execution `mode: planned` chạy một kế hoạch dạng DAG, mỗi task giao cho một Agent:

- **Kiểm kế hoạch** (`internal/agent/orchestrator`, logic thuần): tối đa 16 task, chuỗi phụ thuộc dài tối đa 8, id khớp `[a-z0-9][a-z0-9_-]{0,31}`, phụ thuộc tồn tại, không chu trình. Bên tạo execution kiểm thêm: mọi Agent thuộc project và đang `active`, version được phép chạy, model và giá của từng task, budget. Kế hoạch sai bị từ chối (422) trước khi ghi bất cứ thứ gì. Mọi task được ghi ngay ở `queued` cùng execution, với Agent Version, chuỗi model và budget chốt lúc đó; `agent_executions.plan` chụp kế hoạch đã kiểm.
- **Bật theo project:** `settings.orchestration = {planned_enabled, max_parallel_tasks}` (mặc định tắt; song song mặc định 4, trần 8). Kiểm khi ghi; cấu hình đã lưu bị hỏng thì dừng (`orchestration_config_invalid`). Khoá này không có tác dụng ở settings của organization.
- **Thực thi trên Temporal:** execution là workflow cha `PlannedExecution`, thân là `orchestrator.Run` (bộ lập lịch deterministic sau interface `Engine`); mỗi task là child workflow `AgentTask` có thân là đúng `runtime.Run` của execution đơn Agent. Activity mới: `BeginPlan`, `BeginTask`, `FinishTask`, `SkipTask`, `FinishPlan`. Workflow `AgentExecution` của `single_agent` không đổi. Lịch sử của cả hai workflow chỉ chứa tham chiếu (định danh, giới hạn, trạng thái, plan key); goal và kết quả nằm trong database. Đổi thứ tự gọi `Engine` trong `orchestrator.Run` là đổi workflow cha. Chế độ trong process (`LocalRunner`) chạy cùng bộ lập lịch bằng goroutine.
- **Activity không tin thứ tự do workflow đưa:** `BeginTask` chỉ cho một task bắt đầu khi execution là `planned`, chưa bị huỷ, còn hạn, và mọi task nó phụ thuộc đã kết thúc mà không chặn nó. Task chưa từng bắt đầu không đóng được thành `completed`. `FinishPlan` suy trạng thái của execution từ trạng thái các task trong database.
- **Thất bại từng phần:** `on_failure` của task là `skip_dependents` (mặc định: các task phụ thuộc thành `skipped`, phần còn lại chạy tiếp), `fail_execution` (dừng kế hoạch: task đang chạy bị huỷ, task chưa chạy thành `cancelled`) hoặc `continue` (task phụ thuộc vẫn chạy, thấy trạng thái thất bại và không có kết quả; execution không bị coi là thất bại vì task này).
- **Trạng thái của execution:** `completed` khi mọi task cần hoàn thành đã hoàn thành (kể cả khi hạn hay yêu cầu huỷ đến ngay sau đó); không thì `cancelled` / `timed_out` nếu execution bị dừng từ ngoài; không thì theo nguyên nhân gốc: mọi task thất bại đều vì budget → `budget_exceeded`, đều vì hết hạn → `timed_out`, còn lại `failed` / `task_failed`. Output luôn có trạng thái và tóm tắt của từng task (kết quả dở dang).
- **Context (§5.5):** task chỉ nhận `goal` của nó và `output_summary` của các task nó phụ thuộc, trong một khối `<task_results>` ở lượt user đầu tiên. Đó là output của model khác: dữ liệu không tin cậy, không bao giờ vào system prompt, không đổi được tập tool hay quyền của task; thẻ của khối trong nội dung bị vô hiệu hoá. `output_summary` hiện là trả lời cuối của task cắt còn 4000 ký tự (tóm tắt bằng model thuộc lát "quản lý context").
- **Tổng hợp (§5.6):** deterministic: `output.message` là kết quả của các task không còn task nào phụ thuộc vào (ghép kèm tiêu đề nếu nhiều hơn một), `output.tasks` là trạng thái và tóm tắt từng task. Bước tổng hợp cần suy luận là một task của kế hoạch (một Agent phụ thuộc vào các task chẩn đoán).
- **Budget (§5.3):** mỗi task **bắt buộc** có budget: `plan.tasks[].budget` (chỉ được chặt hơn `limits.budget` của Agent Version) hoặc `limits.budget`. Request có thể đặt `budget` cho execution; khi đó tổng budget của các task không được vượt nó. Việc chặn chi tiêu nằm ở từng task.
- **Thời hạn:** mỗi task có hạn riêng (timeout của Agent Version, tính từ lúc task bắt đầu) và không sống quá hạn của execution. Hạn của execution là thời gian kế hoạch cần khi mọi task dùng hết hạn của nó với đúng số task chạy song song, tối đa 2 giờ; `timeout_seconds` của request chỉ đặt được chặt hơn.
- **Huỷ:** huỷ execution huỷ workflow cha, lan tới mọi child workflow (`WaitForCancellation`, `PARENT_CLOSE_POLICY_REQUEST_CANCEL`), tới activity đang chạy và tới approval đang chờ. Có test với Temporal thật: bốn task đang gọi model dừng trong thời hạn xác định.
- **Approval:** một task chờ duyệt thì execution ở `waiting_approval`, các task khác vẫn chạy và vẫn gọi được Gateway. Quyết định của người duyệt được signal tới đúng child workflow của task.
- **Trần đồng thời theo org** đếm theo vòng lặp Agent: execution `planned` chiếm số task nó chạy song song (trần 8 mỗi org trên Temporal).

**Lệch thiết kế, có chủ ý:**

- §5.1–5.2 nói kế hoạch do một lần gọi LLM (planner) sinh ra. Hiện kế hoạch do **người gọi** đưa trong request (`plan`); planner chưa có. Phần "kế hoạch là dữ liệu, kiểm bằng code, không sửa giúp" đã đúng thiết kế; planner sau này chỉ là một nguồn khác sinh cùng cấu trúc.
- `internal/agent/orchestrator` chỉ chứa phần thuần (kiểu, kiểm, lập lịch, cấu hình). Việc tạo execution, các bước có I/O và handler của `planned` nằm trong `internal/agent/runtime` (cùng bounded context Execution), nên `runtime` import `orchestrator`, không ngược lại.
- Budget cấp cho task là tĩnh lúc tạo; không có "phần dư trả về cha" (§5.3). `agent_executions.budget_limit` của execution `planned` không được trừ lúc chạy: nó được giữ bằng việc tổng phần cấp không vượt nó.

**Chưa làm (các phần còn lại của Orchestrator):** tool `read_task_results` và artifact có cấu trúc; khoá theo `resource_key` (hai task song song vẫn có thể cùng gọi tool `write` non-prod lên một tài nguyên); `direct_tool`.

**Nợ của lát này:** rate limit của Gateway tính theo người khởi tạo nên các task song song dùng chung hạn mức; vượt thì task thất bại (`model_error`). Workflow cha không tự đọc yêu cầu huỷ từ database (nếu lệnh huỷ workflow không tới được Temporal, từng task tự dừng ở bước kế tiếp của nó). Chế độ trong process không áp trần theo task (chỉ dành cho local / test). `cmd/gateway` link `internal/agent/runtime` (để dùng `TaskReader`), nên mọi thứ thêm vào package đó cũng vào process `gateway`: cần tách phần đọc ra package nhỏ. Tổ chức không có cách cấm project bật `planned`.

### 5.9 Hiện thực (Phase 2: delegation)

Quyết định kiến trúc: [ADR-0014](../adr/0014-internal-runtime-tools-and-delegation-decision-in-database.md) (Đề xuất).

- **Cấu hình:** Agent Version khai `delegation.can_delegate_to` (tối đa 8 Agent cùng project) và `max_depth` (1–5, bắt buộc khi có `can_delegate_to`). Kiểm khi tạo version: Agent đích tồn tại trong project (Agent của project / tenant khác cho cùng câu trả lời với Agent không tồn tại).
- **Ảnh chụp lúc tạo execution** (`single_agent` lẫn `planned`): process `api` đi theo `can_delegate_to` từ các task gốc, kiểm từng Agent đích như một task (thuộc project, đang `active`, model và giá dùng được) và ghi vào `execution_delegation_sources` / `execution_delegation_targets`. Tối đa 16 Agent đích mỗi execution. Agent đích không dùng được lúc đó bị bỏ khỏi ảnh chụp và được ghi vào audit `execution.create` (`delegation_unavailable`).
- **Tool nội bộ `delegate_task {agent, goal}`**: runtime thêm vào danh sách tool của model khi task giao việc được cho ít nhất một Agent, với `agent` là enum các slug. Danh sách đó áp đúng các luật không phụ thuộc thời điểm của điểm quyết định (trong `can_delegate_to`, không phải tổ tiên, không vượt độ sâu), nên model không bị chào một việc chắc chắn bị từ chối. Mô tả tool chỉ chứa slug (tên hiển thị của Agent sửa được mà không qua publish). Lời gọi không đi qua Gateway.
- **Điểm quyết định** là hàm `agent_task_delegate` trong database, dưới khoá của execution. Luật theo thứ tự: idempotent theo (task cha, bước); task cha và execution còn chạy; bước là một step `delegation` đang mở; task cha không còn task con nào đang mở; Agent đích trong `can_delegate_to` và trong ảnh chụp; Agent đích còn `active` với đúng version đã chụp; không chu trình tổ tiên; độ sâu không vượt `max_depth` **nhỏ nhất** trong chuỗi tổ tiên; execution chưa đủ 16 task; có budget và đủ budget. Hàm tự ghi audit `delegation.create` / `delegation.deny` kèm luật.
- **Budget (§5.3, §5.4):** task con nhận phần nhỏ hơn giữa `limits.budget` của Agent đích và phần còn lại của task cha sau khi giữ lại một lượt model cho chính task cha. Cả hai đều không có budget thì từ chối. Mức đã tiêu của một task gồm cả các task con của nó, tính từ cây task (`agent_task_spent`), nên giao việc không mở thêm tiền.
- **Chạy:** tuần tự. Bước `CallTool` tạo task con và trả về id của nó; vòng lặp gọi `Executor.AwaitTask` (Temporal: activity `DelegateSpec` đọc cấu hình task con từ ảnh chụp, rồi child workflow `AgentTask`; trong process: chạy ngay), rồi gọi lại đúng bước đó để lấy kết quả. Step có kind `delegation`. Hạn của task con là timeout của Agent đích và không quá hạn của task cha. Huỷ lan xuống task con. Workflow ở tầng trên có lề đóng dài hơn tầng dưới.
- **Context (§5.5):** task con chỉ nhận `goal`, trong khối `<delegated_task>` ở vai user, đánh dấu là việc do Agent khác giao. Task cha nhận `output_summary` của task con ở vai tool, trong khối `<task_result>`. Không gì vào system prompt.
- **Quyền:** task con chạy dưới Agent Version đích với cùng người khởi tạo; Gateway kiểm cả hai ở mỗi lời gọi. **Mức sàn mới:** tool không chỉ đọc do task được giao việc gọi luôn cần người duyệt ([security §4.5](security.md)).
- **Kết thúc:** khi một task kết thúc, mọi task con còn mở của nó bị đóng theo (`cancelled`) cùng step đang mở của chúng. Khi execution đơn Agent kết thúc, output chỉ ghi vào task gốc.

**Lệch thiết kế, có chủ ý:**

- §5.4 nói lời gọi "tới Orchestrator, nơi policy engine kiểm tra". Điểm quyết định là hàm database, không qua `authz.Decide` và không qua Gateway (ADR-0014).
- Delegation tuần tự: một Agent không tự fan-out song song. Chạy song song dùng `mode: planned`.
- `max_depth` là độ sâu tuyệt đối tính từ task gốc, và giới hạn nhỏ nhất trong chuỗi áp cho mọi tầng bên dưới: một Agent giao việc được khi là gốc có thể không giao việc được khi nó được Agent khác gọi tới.
- Không có `context_refs` (chữ ký ở §5.4): task con chỉ nhận `goal`.
- "Phần dư trả về cha" (§5.3) là hệ quả của cách tính: budget của task con là trần, phần nó không tiêu vẫn thuộc task cha.
- Delegation không có công tắc theo project như `planned`: nó được bật bằng việc publish một Agent Version có `delegation` (cần `agent:publish`).

**Nợ của lát này:**

- Worker bị chiếm quyền có thể làm tổng chi của execution vượt budget của task gốc (xem ADR-0014, "Giới hạn đã biết"). Cần Gateway đặt trước vào bucket của cả chuỗi tổ tiên.
- Usage ghi trễ của task con (lượt model trả lời sau khi task con đã đóng) không được tính vào mức đã tiêu của task cha: nhiều nhất một lượt mỗi task con.
- **Thứ tự deploy:** mọi `worker` phải chạy phiên bản có delegation trước khi `api` bắt đầu ghi ảnh chụp. Worker cũ nhận một execution có delegation sẽ bỏ qua bước chờ task con. Quy trình versioning workflow vẫn là nợ chung.
- Không reset được workflow qua một bước delegation (child workflow dùng id cố định, không tạo lại).
- Lỗi hạ tầng ở bước giao việc ra mã `tool_outcome_unknown` dù delegation không có tác dụng phụ bên ngoài.
- `app_api` vẫn chèn được task con trực tiếp (nó có `INSERT` trên `agent_tasks` để tạo execution); hàm quyết định là rào với `app_worker`.
- Chưa có test cho: child workflow hỏng giữa chừng (đường đóng task con mồ côi), approval trong task con trên Temporal, task con hết hạn riêng.

### 5.10 Hiện thực (Phase 2: pause / resume)

- **API:** `POST /v1/executions/{id}/pause` và `/resume` (quyền `execution:cancel`; cả hai idempotent, yêu cầu lặp lại không đổi gì và không ghi audit; 409 `execution_finished` khi đã kết thúc). `Execution.pause_requested_at` hiện yêu cầu đang có, kể cả khi `status` chưa là `paused`.
- **Nguồn sự thật là database:** `agent_executions.pause_requested_at` (kèm người yêu cầu), chỉ process `api` ghi. Task bắt đầu sau khi có yêu cầu (task kế tiếp của kế hoạch, task con được giao việc) cũng thấy nó.
- **Dừng ở ranh giới bước:** vòng lặp đọc yêu cầu cùng lúc với yêu cầu huỷ, trước mỗi lượt model, mỗi tool call, và sau mỗi lần chờ (approval, task con). Bước đang chạy được chạy xong, kể cả tool vừa được duyệt. Task sang `paused`, chờ, rồi trở lại `running` và kiểm lại huỷ / hạn trước khi làm gì khác. Tối đa 200 lần tạm dừng mỗi task.
- **Trạng thái execution** suy từ các task: `waiting_approval` nếu còn task chờ duyệt, không thì `paused` nếu có task tạm dừng, không thì `running`. Gateway vẫn phục vụ task `running` của một execution `paused` / `waiting_approval` (để nó chạy xong bước đang dở); task đang tạm dừng thì không.
- **Trên Temporal:** chờ bằng signal `execution-resumed` (api gửi tới workflow của execution và của từng task còn mở) cộng một timer 2 phút làm lưới an toàn; trạng thái thật luôn đọc lại từ database, lỗi đọc trong lúc chờ được coi là "vẫn tạm dừng". Trạng thái tạm dừng bền qua khởi động lại worker.
- **Versioning workflow (lần đầu dùng):** mốc `control` (`workflow.GetVersion`). Từ version 1, vòng lặp đọc huỷ và tạm dừng bằng một local activity `Control`. Execution bắt đầu trước mốc phát lại theo đường cũ và **không tạm dừng được** (yêu cầu được ghi nhưng không có tác dụng). Lịch sử mẫu tách theo version: `*_v0.json` (đường cũ, không bao giờ ghi lại) và `*_v1.json` (test ghi ra).
- **Ai được chạy tiếp:** người đã yêu cầu tạm dừng tự gỡ được yêu cầu của mình; gỡ yêu cầu của người khác cần quyền `execution:resume` (`org_admin`, `project_admin`, `operator`). Lý do: chạy tiếp là mở lại hoạt động, khác với huỷ và tạm dừng.

**Giới hạn và lựa chọn có chủ ý:**

- **Tạm dừng là công cụ vận hành, không phải kiểm soát an toàn.** Nó dựa vào sự hợp tác của vòng lặp: Gateway không thực thi yêu cầu tạm dừng (nó thực thi yêu cầu huỷ). Muốn chắc chắn không bước nào chạy nữa thì huỷ execution.
- Thời gian tạm dừng tính vào hạn của execution và của task: tạm dừng quá hạn thì kết thúc `timed_out`. Execution tạm dừng vẫn chiếm suất trong trần đồng thời của org.
- Approval có hạn 30 phút và không dời theo tạm dừng: tool đã được duyệt mà execution chạy tiếp sau hạn đó sẽ bị từ chối.
- Trong `planned`, task được bắt đầu trong lúc tạm dừng chuyển sang `running` rồi dừng ngay ở ranh giới đầu tiên; hạn riêng của nó đã bắt đầu tính. Task cha đang chờ task con vẫn `running` cho tới khi task con xong.
- Lần tạm dừng không để lại step; dấu vết là audit `execution.pause` / `execution.resume`.
- **Deploy:** không rollback worker qua mốc `control` khi còn execution version 1 đang chạy (worker cũ không phát lại được lịch sử có mốc). Worker phải lên phiên bản mới trước khi mở endpoint.

### 5.11 Hiện thực (Phase 2: đề xuất kế hoạch bằng LLM)

§5.1–5.2 mô tả `planned` là "một lần gọi LLM tạo kế hoạch". Hiện thực tách làm hai việc, vì kế hoạch do model viết là đầu vào không tin cậy:

- **Đề xuất:** `POST /v1/projects/{id}/plan-proposals` chạy **Agent lập kế hoạch** của project (`settings.orchestration.planner_agent_id`, một Agent bình thường do tenant cấu hình) như một execution `single_agent`. Platform dựng yêu cầu gửi cho nó: khối `<request>` (yêu cầu của người dùng), khối `<available_agents>` (mỗi Agent một dòng JSON: slug, tên, mô tả; tối đa 32, thu hẹp bằng `agent_ids`), và định dạng trả lời. Execution được đánh dấu lúc tạo (`plan_proposals`, kèm danh sách Agent được phép). Không task nào của kế hoạch được tạo.
- **Agent lập kế hoạch bị giới hạn:** Agent Version của nó không được có tool (kể cả tool đọc: kết quả tool là thêm một kênh tiêm chỉ dẫn vào kế hoạch) hay delegation, và phải có `limits.budget` (422 `planner_not_eligible`).
- **Đọc đề xuất:** `GET /v1/executions/{id}/plan` kiểm trả lời bằng code (`orchestrator.ParseProposal`): một object JSON, chỉ các khoá được phép và không khoá trùng, Agent nêu bằng slug thuộc danh sách đã ghi lúc đề xuất và hiện còn hoạt động, rồi các luật của §5.2. Không có gì được sửa giúp. `ready` nghĩa là đúng cấu trúc, không phải nội dung đã được ai xem. Execution không được đánh dấu là lần lập kế hoạch không bao giờ cho ra đề xuất.
- **Chạy đề xuất, hai đường:**
  - Người gọi tự gửi lại kế hoạch qua `mode: planned`: nó được xử lý như kế hoạch do người gọi viết (`plan_source = caller`). Đường này dành cho trường hợp có người xem lại.
  - `POST /v1/executions/{id}/plan/run`: platform tạo execution `planned` thẳng từ đề xuất và ghi `plan_source = model`. Chỉ người đã tạo đề xuất mới chạy được nó, trong vòng 24 giờ (người chạy là người yêu cầu của các approval; không ai viết yêu cầu rồi để người khác chạy để tự duyệt). Khi đó `goal` của từng task được bọc trong khối `<planned_task>` (đánh dấu là do Agent lập kế hoạch viết), và tool không chỉ đọc của các task đó luôn cần người duyệt (luật `model_planned_write_requires_approval` ở Gateway, cùng mức sàn với delegation). Audit nối execution chạy với execution đề xuất.

**Lệch thiết kế, có chủ ý:** không có "một lần gọi" tự phân rã rồi tự chạy không qua mức sàn. Process chạy vòng lặp không có đường nào biến output của model thành task: việc tạo task luôn nằm ở process `api`, từ một kế hoạch đã kiểm. Phương án đã cân nhắc và bỏ: worker tự vật chất hoá kế hoạch qua một hàm database (thêm một điểm quyết định nhiều luật ngoài `api`); activity Temporal chạy trong process `api` (đổi trách nhiệm của ba process); planner là Agent điều phối dùng delegation (tuần tự, không phải kế hoạch dạng dữ liệu).

**Nợ:** client tự động gửi lại đề xuất qua `mode: planned` thì platform không biết kế hoạch do model viết (đã ghi rõ trong mô tả API). `planner_agent_id` chỉ được kiểm định dạng lúc ghi settings; lỗi lộ ra lúc dùng. Tên và mô tả của Agent (sửa được không qua publish) vẫn là dữ liệu mà Agent lập kế hoạch đọc.

### 5.12 Hiện thực (Phase 3: workflow do người dùng định nghĩa, nút `wait` và `approval`, lịch chạy)

Quyết định: [ADR-0015](../adr/0015-workflows-compile-to-planned-executions.md) (còn Đề xuất). Code: `internal/workflow/definitions` (định nghĩa, lần chạy), `internal/workflow/definitions/schedule` (Temporal Schedules), phần nút `wait` trong `internal/agent/runtime` (`plan.go`) và `internal/agent/runtime/durable`.

- **Định nghĩa là dữ liệu.** `workflows.definition` là một DAG các bước: `agent` (`agent_id`, `goal` tĩnh, `budget`, `on_failure`, `timeout_seconds`: thời hạn riêng của bước, chỉ chặt hơn thời hạn của Agent Version), `wait` (`seconds`, 1–3600) và `approval` (`message` tĩnh, `required_approvals` 1–2, `timeout_seconds` 60–86400, mặc định 1800). Không có biến hay template; khoá lạ bị từ chối. Bước `tool` bị từ chối tường minh cho tới khi có hiện thực. Định nghĩa được kiểm khi ghi và kiểm lại ở mỗi lần chạy; đổi `definition` tăng `version`.
- **Một lần chạy là một execution `planned`.** `api` biên dịch bản chụp của định nghĩa (`workflow_executions.definition_snapshot`) thành kế hoạch và tạo execution qua đúng đường của §5.8; mọi kiểm tra ở đó áp dụng (`planned_enabled` của project, Agent còn `active`, model, giá, budget, trần). Không có bộ diễn giải thứ hai. `workflow_executions` là bản chiếu, lấy trạng thái từ execution khi được đọc.
- **Nút `wait`** là một loại nút của kế hoạch, không phải task: trạng thái của nó nằm ở `execution_plan_steps` (`queued → running → completed | cancelled | timed_out | failed`, hoặc `skipped`). API `mode: planned` công khai không tạo được nút này; chỉ đường của workflow. Trên Temporal nó là một timer của workflow cha giữa hai activity `BeginPlanStep` / `FinishPlanStep`; trong process nó là một timer đọc yêu cầu huỷ mỗi 2 giây. Nút bị bỏ qua được ghi bằng `SkipPlanStep`.
  - Như `BeginTask`, các bước này không tin thứ tự do nơi chạy kế hoạch đưa: `BeginPlanStep` kiểm trong database rằng execution còn chạy, chưa bị huỷ, còn hạn, nút có trong kế hoạch đã lưu và mọi bước nó phụ thuộc đã kết thúc mà không chặn nó; `FinishPlanStep(completed)` chỉ được nhận khi nút đã `running` và đã chờ đủ `seconds` theo đồng hồ của database (trừ 2 giây cho độ lệch đồng hồ); `BeginTask` kiểm thêm các nút chờ mà task phụ thuộc.
  - Timer không ngủ quá hạn của execution: chạm hạn thì nút là `timed_out`. Execution mồ côi bị đóng khi có người đọc thì nút còn mở của nó đóng theo.
  - Nút chờ chiếm một suất song song của kế hoạch (`max_parallel_steps`) nhưng không chiếm suất của trần vòng lặp Agent theo org.
  - Nút chờ không đọc yêu cầu tạm dừng: timer vẫn chạy khi execution có yêu cầu tạm dừng, và trong lúc chỉ có nút chờ đang chạy execution vẫn hiện `running`. Task Agent kế tiếp tự dừng ở ranh giới bước đầu của nó. Nhất quán với "thời gian tạm dừng tính vào hạn" (§5.10).
- **Nút `approval`** là cổng chờ người, cũng là một nút của kế hoạch ở `execution_plan_steps`. Khi nút bắt đầu (`BeginPlanStep`), approval của nó được tạo bởi hàm database `plan_step_request_approval` (migration 00033): thông điệp, số người duyệt và hạn lấy từ cấu hình đã chụp của nút, người yêu cầu là người khởi tạo execution. Nội dung của cổng không đi qua nơi chạy kế hoạch và không vào lịch sử Temporal. Trong lúc chờ, execution ở `waiting_approval` (`syncWaiting` đếm cả nút approval đang chạy). Trên Temporal, workflow cha ngủ trên signal `approval-resolved` (api gửi khi có quyết định) và một timer 2 phút (lưới an toàn; signal có thể bị một nút approval khác của cùng kế hoạch nhận); trong process, nút tự đọc trạng thái mỗi giây. Trên Temporal một coroutine duy nhất của workflow cha nhận signal và đánh thức mọi cổng đang chờ (signal chỉ được một nơi nhận). Kết quả của nút do hàm database `plan_step_close_approval` quyết định **và ghi** (cả approval lẫn nút), không do workflow: đủ người duyệt → `completed` (approval thành `consumed`, dùng một lần; quyết định được ghi trong hạn nên thức dậy muộn không làm mất nó); bị từ chối hoặc hết hạn → `failed`; kế hoạch dừng khi còn chờ → approval bị huỷ, nút `cancelled` / `timed_out`; approval còn chờ mà không có lý do dừng → không đổi gì, nơi gọi chờ tiếp (một lần đọc lỗi không đóng cổng đang chờ người duyệt).
  - **Cổng là ranh giới do database giữ, không phải do worker.** `app_worker` có `UPDATE (status)` trên `execution_plan_steps`, nên trigger của bảng từ chối `completed` cho nút approval chưa có approval `consumed` (và cho nút `wait` chưa chờ đủ), với mọi role. `BeginTask` / `BeginPlanStep` kiểm `plan_node_gates_open`: mọi cổng mà bước phụ thuộc, kể cả gián tiếp, đã mở. Gateway kiểm cùng điều đó ở mọi tool call và lượt model (`agent_task_gates_open` trong `GetActiveTask`): worker bị chiếm quyền tự đặt task sau cổng sang `running` vẫn không gọi được gì.
  - Nút approval không nhận `on_failure: continue` (bị từ chối khi kiểm định nghĩa): cổng bị từ chối mà bước sau vẫn chạy thì không phải cổng. Với cổng nằm xa hơn trên chuỗi phụ thuộc, `continue` của một bước ở giữa cũng không mở lại được nó (bước sau bị `BeginTask` từ chối).
  - Khi execution đóng (`FinishPlan`, execution mồ côi, không khởi chạy được), approval còn mở của các cổng bị huỷ: không cổng nào của execution đã đóng còn nhận quyết định.
- **Thử lại theo bước (`max_attempts`, 1–3, chỉ bước `agent`).** Một bước có `max_attempts: N` biên dịch thành một chuỗi N task của kế hoạch: `<id>`, `<id>-try2`, `<id>-try3`. Lần thử k+1 phụ thuộc vào lần k và mang `retry_of`; nó chỉ chạy khi lần k kết thúc `failed` hoặc `timed_out`, và bị bỏ qua (`skipped`) khi lần k hoàn thành, hết budget, bị huỷ hay bị bỏ qua. Bước phụ thuộc vào `<id>` được nối vào lần thử cuối; một nút thử lại bị bỏ qua "mang" kết quả của lần thử trước nó (`orchestrator.Effective`), nên bước sau chạy khi một lần thử nào đó thành công và nhận tóm tắt của đúng lần đó trong `<task_results>`. Các lần thử chưa phải cuối mang `on_failure: continue` (không chặn, không tính vào kết quả của kế hoạch); `on_failure` của bước nằm ở lần thử cuối.
  - Không có bộ đếm hay vòng lặp nào ở nơi chạy: số lần thử là số task đã được chụp vào kế hoạch lúc tạo execution, tính vào trần 16 task. Mỗi lần thử có budget riêng bằng budget của bước (tổng budget của mọi lần thử phải nằm trong budget của execution, nên chi phí xấu nhất được biết trước) và thời hạn riêng; hạn của execution vẫn chặn cả chuỗi. Hết budget không thử lại.
  - Luật nằm ở phần thuần (`orchestrator.Run`, `Validate`, `Aggregate`) và được kiểm lại trong database: `BeginTask` (qua `planDepsDone`, theo kế hoạch đã chụp) từ chối lần thử lại của một bước chưa thất bại, và từ chối bước sau khi chuỗi thử lại chưa cho kết quả hoàn thành.
  - **Thử lại không bao giờ lặp lại một tác động.** Lần thử lại là một task mới, nên khoá idempotency của tool (`task:step_seq`) và chỗ giữ ở Gateway không khử trùng được giữa hai lần thử. Vì vậy database chỉ cho thử lại khi MỌI lần thử trước của bước chắc chắn chưa gây tác động: không tool không-chỉ-đọc nào được Gateway giữ chỗ cho nó (`mcp_tool_claims`, kể cả lời gọi có kết quả không xác định), không step `unknown`, chưa giao việc cho task con. Điểm quyết định là hàm `agent_task_retry_safe` (migration 00036, `SECURITY DEFINER`; chỗ giữ do Gateway ghi, worker không đọc hay sửa được), gọi từ `planDepsDone`. Lần thử lại bị từ chối kết thúc `failed` mà không gọi model hay tool. Hệ quả thực tế: thử lại có tác dụng với bước chỉ đọc (kiểm tra, thu thập, báo cáo); một bước đã ghi rồi mới hỏng thì dừng ở đó cho người xem, đúng luật "không đoán, không tự retry" của §4.
  - Một lần thử nào đó đã `completed` thì bước là hoàn thành, kể cả khi execution dừng trước lúc nút thử lại kịp được ghi `skipped`. Bước `fail_execution` có thử lại chỉ dừng kế hoạch khi nó đã thật sự chạy và không hoàn thành, như bước không có thử lại.
  - Kế hoạch có `retry_of` chạy dưới tên workflow `PlannedExecutionRetry` (cùng hàm với `PlannedExecution`): worker phát hành trước khi có luật này không đăng ký tên đó nên không chạy sai được kế hoạch như vậy; rollback worker khi còn execution có thử lại đang chạy làm chúng chờ tới khi có worker mới.
  - Lần thử lại thấy tóm tắt của lần thất bại trước nó trong `<task_results>` (dữ liệu, không phải chỉ dẫn). Không có backoff giữa các lần thử: muốn chờ thì đặt một bước `wait` (hiện chưa chèn được bước chờ vào giữa chuỗi thử lại).
  - Chỉ đường của workflow tạo ra `retry_of`; API `mode: planned` công khai không nhận nó. Engine (`localEngine`, `planEngine`) không đổi: với kế hoạch không có `retry_of`, thứ tự gọi `Engine` giữ nguyên nên lịch sử cũ phát lại được không cần mốc `GetVersion`. Lịch sử mẫu: `planned_parent_retry_v1.json` (`HQ_UPDATE_REPLAY_FIXTURE=1 go test ./tests/api -run TestDurableWorkflowRetry`).
- **Người chạy và lịch.** Xem ADR-0015 mục 3 và 4, và security §4.6. Tóm tắt: workflow `active` có cron thì có đúng một Temporal Schedule (`workflow-schedule/<org>/<workflow>`, overlap `SKIP`, catchup 1 phút). Khi lịch kích hoạt, workflow `ScheduledWorkflowRun` trên task queue `api-internal` gọi đúng một activity trong process `api`; activity đọc lại workflow, kiểm người chạy, rồi đi qua đúng đường của một lần chạy thủ công.
- **Lịch sử mẫu:** `planned_parent_wait_v1.json`, `planned_parent_wait_skip_v1.json` và `planned_parent_gate_v1.json` (`HQ_UPDATE_REPLAY_FIXTURE=1 go test ./tests/api -run TestDurableWorkflowRun`; test chờ workflow đóng hẳn rồi mới ghi); `internal/workflow/definitions/schedule/testdata/scheduled_run_v1.json` (`… -run TestWorkflowSchedules`). Nhánh của nút `wait` / `approval` chỉ chạy với kế hoạch có nút đó, nên lịch sử cũ của `PlannedExecution` phát lại không đổi và không cần mốc `GetVersion`. Thứ tự deploy: mọi `worker` trước, `api` sau (worker cũ không biết các nút này).

**Lệch thiết kế, có chủ ý:** roadmap ghi workflow "được một workflow Temporal tổng quát diễn giải"; hiện thực dùng lại `PlannedExecution` thay vì viết bộ diễn giải riêng (ADR-0015 mục 2). Không có compensation: `cancel` huỷ execution. Thử lại là các task được chụp trước trong kế hoạch, không phải retry policy của Temporal: mỗi lần thử là một task có step, usage và budget riêng, nhìn thấy được trong database.

**Nợ:**

- Thử lại còn nợ: lỗi tất định (`limit_exceeded`, model từ chối) cũng được thử lại, tốn budget vô ích; mỗi lần thử tính vào độ sâu 8 và trần 16 task, và được đếm như một Agent riêng ở trần song song dù chuỗi chạy tuần tự; hạn của execution vẫn bị cắt ở 2 giờ nên chuỗi dài có thể hết hạn trước lần thử cuối; lần chạy workflow không đặt budget cho execution nên trần chi phí của bước là `max_attempts` × budget của nó; `GET /v1/executions/{id}/tasks` chưa trả `retry_of` (client nhận ra lần thử qua hậu tố `-tryN`); `execution.finish` đếm lần thử thất bại trong `tasks` dù execution `completed`; lỗi mở task (database chập chờn) cũng tiêu một lần thử.
- Chưa có: bước `tool` (cần Gateway nhận lời gọi không gắn Agent Version, cùng bài toán với `direct_tool`), compensation, backoff giữa các lần thử lại, thử lại cho bước `wait` / `approval`, truyền dữ liệu có cấu trúc giữa các bước. Thông báo lỗi kiểm cấu trúc của một định nghĩa có `max_attempts` mang chỉ số của task đã biên dịch, không phải chỉ số của bước.
- Execution đang chờ ở cổng vẫn giữ suất của trần vòng lặp Agent theo org (`min(song song, số bước Agent)`) và cổng chiếm một suất song song của kế hoạch: vài lần chạy nằm chờ duyệt có thể làm org nhận 429 cho execution mới. Đổi cách đếm là đổi `orchestrator.Run` / `CountActiveSlots`, chưa làm.
- Nút approval: người duyệt chỉ thấy thông điệp tĩnh của định nghĩa, chưa thấy tóm tắt kết quả của các bước trước (phải mở execution để xem); thông báo cho người duyệt chỉ có webhook `approval.requested` của org ([security §4.7](security.md)); hạn của cổng không vượt hạn của execution (tổng thời hạn các Agent của kế hoạch), nên cổng "chờ qua đêm" cần execution có hạn đủ dài; `timeout_seconds` nhận tới 86400 nhưng execution `planned` hiện có hạn tối đa 2 giờ; cổng tự hết hạn được workflow phát hiện trễ tối đa 2 phút; chưa có lịch sử mẫu cho cổng bị từ chối / hết hạn trên Temporal (đường trong process có test; hai cổng song song có test trên Temporal).
- Huỷ trên Temporal trong lúc nút chờ đang ngủ chỉ dựa vào `CancelWorkflow`; nếu lời gọi đó lỗi, không ai đọc `cancel_requested_at` thay nút chờ cho tới khi timer hết (tối đa 3600 giây). Cách sửa đã biết: chia timer thành đoạn xen local activity `Control` (đổi workflow, cần fixture mới).
- Chưa có lịch sử mẫu cho huỷ / Halt trong lúc nút chờ đang chạy trên Temporal (đường trong process có test).
- Bản chiếu đồng bộ lười, không có job quét: lần chạy không ai đọc giữ trạng thái cũ. Lần chạy bị bỏ dở mà lịch không thử lại được nữa chỉ thành `failed` khi có người đọc sau 2 phút.
- Đồng bộ lịch sang Temporal không có job đối soát; hàng đợi `api-internal` chưa có trần công bằng theo org; phân quyền namespace Temporal chưa có (ADR còn nợ).
- Một lần chạy thủ công không tạo được execution vẫn để lại một dòng `failed` kèm bản chụp (tới 256 KiB); endpoint chưa có rate limit. Đường theo lịch bị chặn bởi luật tự tắt sau 3 lần.
- `plan_source` của execution do workflow tạo là `caller`; chưa có giá trị riêng để audit / policy phân biệt.

## 6. Quản lý context và memory

**Hiện thực (Phase 2):** bước 1 có từ Phase 1 ở Gateway (cắt kết quả theo `max_result_bytes`; chưa có artifact để đọc lại phần bị cắt). Bước 2 hiện thực ở dạng **deterministic, không gọi LLM** (`internal/agent/runtime/context.go`): khi hội thoại vượt ngân sách của model, kết quả tool cũ được rút gọn còn phần đầu (200 ký tự) kèm một ghi chú của platform, cũ nhất trước, xuống còn khoảng 70% ngân sách (không phải "vừa đủ", để không phá prompt cache ở mọi lượt sau). Chỉ kết quả **thành công** của tool call thường, từ 1 KiB trở lên, mới bị rút gọn. Giữ nguyên: lượt mở đầu, mọi lượt assistant, kết quả của hai lượt gần nhất, và mọi kết quả mang tín hiệu an toàn (bị từ chối, lỗi, "không xác định", kết quả của task được giao việc). Ghi chú nói rõ tool call đã chạy xong và không gợi ý gọi lại: model không được hiểu việc lược bỏ là "chưa thực hiện" rồi lặp lại một hành động có tác dụng phụ. Ngân sách có sàn 16 KiB. Ngân sách là cửa sổ ngữ cảnh nhỏ nhất trong chuỗi model của task trừ `max_output_tokens`, đổi sang byte bằng ước lượng thận trọng 3 byte / token (`Spec.ContextBudgetBytes`, chốt lúc tạo task; model không khai cửa sổ ngữ cảnh thì không nén). Step vẫn giữ kết quả đầy đủ; số kết quả bị lược bỏ ghi vào `routing.context_elided` của lượt model. Hội thoại vốn được dựng lại từ `execution_steps` ở mỗi lượt, nên việc nén không đổi workflow. Chưa có: tóm tắt bằng model (bước 2 theo nghĩa thiết kế và bước 3), artifact cho kết quả lớn, `output_summary` do model viết (hiện là cắt 4000 ký tự), và memory theo session (`agent_memory`). Trần cứng 4 MiB hội thoại lưu của một task vẫn giữ.


Thứ tự áp dụng khi context vượt ngân sách token của model:

1. **Lọc kết quả tool tại nguồn:** tool trả dữ liệu lớn (log, kết quả query) bị cắt theo `max_result_bytes`; phần đầy đủ lưu thành artifact, model nhận tham chiếu và có thể yêu cầu đọc đoạn cụ thể.
2. **Thay kết quả tool cũ bằng tóm tắt** sau khi model đã dùng xong.
3. **Tóm tắt các lượt cũ** bằng model rẻ, giữ nguyên system prompt, mục tiêu task và N lượt gần nhất.

Memory:

| Loại | Lưu ở | Phase |
|---|---|---|
| Lịch sử trong execution | `execution_steps` | 1 |
| Lịch sử session (đầy đủ hoặc tóm tắt) | `agent_memory` kind `session_summary` | 2 |
| Ghi nhớ dài hạn theo Agent / project (fact do người dùng xác nhận) | `agent_memory` kind `fact` | Sau, khi có nhu cầu cụ thể |
| Tìm kiếm ngữ nghĩa (pgvector) | — | Hoãn; không đưa vào cho tới khi có use case đo được lợi ích |

Nội dung memory là dữ liệu, không phải chỉ dẫn: được chèn vào context trong khối đánh dấu rõ ràng và không thể thay đổi quyền (xem [security.md §5](security.md)).

## 7. Shared runtime và isolated runtime

| | Shared (mặc định) | Isolated |
|---|---|---|
| Triển khai | Worker pool chung, task queue theo mức ưu tiên | Task queue riêng + worker riêng (namespace / node pool riêng trên Kubernetes) |
| Dùng khi | Hầu hết Agent | Agent có quyền production hoặc tài chính; tenant yêu cầu cách ly; Agent cần giới hạn tài nguyên riêng |
| Cách ly | Logic (org_id, policy) | Thêm cách ly process, network policy, credential scope |

Chọn runtime là thuộc tính của Agent Version (`runtime.isolation`), ánh xạ sang tên task queue. Code runtime giống hệt nhau.

## 8. Model Router (tóm tắt)

Chi tiết ở [cost-management.md §5](cost-management.md). Runtime hỏi router một lần lúc tạo task và nhận về model cụ thể cùng chuỗi fallback; chuỗi đó cố định cho cả task. Agent cấu hình `policy: fixed` sẽ bỏ qua router.

## 9. Lỗi và retry

| Loại lỗi | Xử lý |
|---|---|
| LLM 429 / 5xx / timeout | Chuyển ngay sang model dự phòng của task nếu có; cả chuỗi lỗi thì thử lại cả chuỗi với backoff + jitter, tối đa 3 vòng ([cost-management §5.3](cost-management.md)) |
| LLM từ chối / lỗi nội dung | Không retry; trả lỗi rõ ràng |
| Tool lỗi tạm thời, tool idempotent | Retry tối đa 2 lần |
| Tool lỗi, tool không idempotent | Không retry; trả lỗi cho model |
| Tool trả lỗi nghiệp vụ | Trả cho model như kết quả tool (`isError`) |
| Output model sai schema tool | Trả lỗi validate cho model, tính vào iteration |
| Worker chết | Temporal giao lại activity; idempotency theo `step_seq` bảo đảm an toàn |

Mọi retry đều có trần. Không có vòng lặp nào trong runtime thiếu điều kiện dừng theo số lần, thời gian và chi phí.
