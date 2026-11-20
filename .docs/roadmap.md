# Roadmap

Trạng thái: **Đề xuất, chờ xác nhận**

Roadmap này giữ sáu phase của đề bài nhưng điều chỉnh nội dung ở bốn điểm. Thời lượng **không** được ước lượng ở đây vì phụ thuộc quy mô đội chưa biết; mỗi phase được định nghĩa bằng tiêu chí hoàn thành kiểm chứng được.

## Điều chỉnh so với đề bài

| # | Điều chỉnh | Lý do |
|---|---|---|
| 1 | **Temporal vào từ Phase 2**, không phải Phase 3 | Orchestrator cần đúng những thứ Temporal cung cấp: chạy song song, chờ, huỷ lan truyền, phục hồi sau restart. Viết Orchestrator ở Phase 2 không có Temporal rồi viết lại ở Phase 3 là làm hai lần. Phase 3 còn lại là workflow do người dùng định nghĩa |
| 2 | **Ghi usage từ Phase 1**; budget từ Phase 2 | Dữ liệu tiêu thụ không ghi lúc phát sinh thì mất. Multi-agent không có trần chi phí là rủi ro tài chính thật |
| 3 | **Bảo mật nền tảng từ Phase 1**; Phase 6 chỉ là hardening | Cách ly tenant, audit và policy tại Gateway không gắn thêm sau được mà không sửa mọi đường code |
| 4 | **Approval cơ bản vào Phase 2** | Không có approval thì Agent chỉ được phép đọc. Phase 4 (integration có tool ghi) phụ thuộc vào nó |

## Phase 0 — Xác nhận thiết kế

- Xác nhận sáu điểm ở [overview §9](architecture/overview.md) và các ADR.
- IdP: Keycloak ([ADR-0011](adr/0011-keycloak-identity-provider.md)). KMS: provider cục bộ trước ([ADR-0012](adr/0012-local-key-provider-before-cloud-kms.md)). Môi trường triển khai đầu tiên: một máy chủ chạy Docker Compose ([ADR-0017](adr/0017-single-host-docker-compose-deployment.md); cấu hình ở `deployments/production/`, [runbook](runbooks/deploy-compose.md)); máy chủ staging thật chưa dựng.
- Use case dẫn đường (đã chốt): **DevOps Agent chỉ đọc trên Docker** ở môi trường staging: liệt kê container, xem trạng thái, đọc log, inspect. Managed MCP server đầu tiên là Docker read-only.
- Dựng khung repo: CI, lint (kể cả luật phụ thuộc giữa bounded context), testcontainers, migration, khung OpenTelemetry.

**Xong khi:** các ADR được chấp thuận hoặc sửa; `make test` chạy trên CI với Postgres thật.

## Phase 1 — Core Platform

Phạm vi:

- `api`: xác thực OIDC + API key, Organization, Project, RBAC cơ bản, Agent và Agent Version, Model Provider, Model, Model Pricing.
- `gateway`: endpoint MCP Streamable HTTP theo spec `2026-07-28`, pipeline đầy đủ 8 bước với policy `allow` / `deny` (chưa có approval), SSRF guard, rate limit.
- MCP Registry: đăng ký, discovery, ghim hash, duyệt version, phân loại tool.
- Agent Runtime tối giản: vòng lặp một Agent chạy **trong process `api`** (goroutine nền; API vẫn trả 202 như hợp đồng OpenAPI), không bền vững qua restart, **chỉ tool `read`**, có đủ giới hạn iteration / tool call / timeout.
- Provider Claude sau interface `ai.Provider`: adapter `claude-cli` cho phát triển cục bộ, adapter `anthropic` (API key) cho staging và production ([ADR-0013](adr/0013-claude-cli-provider-for-local-development.md)).
- Execution token tối giản giữa runtime (trong `api`) và Gateway: [ADR-0004](adr/0004-mcp-gateway-single-egress.md) yêu cầu mọi tool call của Agent nội bộ đi qua Gateway bằng execution token, nên phần này phải có từ Phase 1; Phase 2 chỉ chuyển bên phát token từ `api` sang `worker`.
- Ghi `execution_steps`, `usage_records` (kèm chi phí ước tính), `audit_logs`.
- RLS và bộ test cross-tenant.
- Secret với envelope encryption.
- Một managed MCP server chỉ đọc để kiểm chứng đầu-cuối: Docker read-only.

Cố ý chưa làm: Temporal, multi-agent, approval, tool ghi, budget, UI.

Vòng lặp Phase 1 được viết sau interface `Executor` với các bước rời (`CallModel`, `AuthorizeToolCall`, `CallTool`) nhận và trả dữ liệu tuần tự hoá được. Đây là điều kiện để Phase 2 chuyển chúng thành Temporal activity mà không viết lại logic.

**Trạng thái (2026-11-18):** phạm vi trên đã hiện thực và chạy được đầu-cuối trên máy cục bộ. Chưa làm: triển khai staging (môi trường chưa chọn), và provider Cloud KMS (bắt buộc trước khi lưu credential production, [ADR-0012](adr/0012-local-key-provider-before-cloud-kms.md)).

Số đo ban đầu (`make measure`; mọi thành phần trong một process trên một máy, Postgres trong container, nên là chi phí xử lý, không gồm độ trễ mạng giữa các máy):

| Số đo | Giá trị |
|---|---|
| Tool call thẳng tới managed server | p50 3,6 ms · p95 4,7 ms · p99 4,9 ms |
| Tool call qua Gateway | p50 17,3 ms · p95 19,1 ms · p99 20,2 ms |
| Độ trễ thêm của Gateway | p50 13,6 ms · p95 14,4 ms · p99 15,3 ms |
| Kích thước một step | 661 byte, trong đó nội dung request + response 445 byte (model giả, hội thoại ngắn) |
| Dữ liệu ghi mỗi execution (2 lượt model, 1 tool call) | step 2,0 KB · usage 2 dòng / 0,6 KB · audit 3 dòng / 1,3 KB |

Độ trễ thêm của Gateway chủ yếu là các transaction Postgres của một request (xác thực, tenant, hai lần kiểm quyền, đọc Registry, ghi audit); đây là chỗ tối ưu đầu tiên nếu cần. Chi phí hạ tầng trên mỗi execution tính bằng tiền chưa đo được vì chưa có môi trường triển khai; số dòng và số byte ở trên là đầu vào để tính khi có.

**Xong khi:**

- Tạo Agent bằng API, chạy một yêu cầu chẩn đoán trên staging, nhận kết quả; toàn bộ step, usage, audit truy vấn được.
- Một MCP client bên ngoài kết nối được Gateway bằng OAuth và chỉ thấy tool được phép.
- Test cross-tenant qua cho mọi endpoint.
- Có số đo: độ trễ thêm của Gateway, kích thước trung bình của một step, chi phí hạ tầng trên mỗi execution.

## Phase 2 — Multi-Agent và Durable Execution

Phạm vi:

- Triển khai Temporal; process `worker`. Chuyển vòng lặp Agent thành workflow + activity; execution trở nên bền vững qua restart; thêm luồng sự kiện SSE.
- Orchestrator: chế độ `single_agent` và `planned`, validate kế hoạch, child workflow, chạy song song, lan truyền huỷ, pause / resume.
- Delegation qua Orchestrator với đủ kiểm tra của policy.
- Execution token giữa worker và Gateway (cơ chế đã có từ Phase 1; chuyển bên phát sang `worker`).
- Quản lý context: lọc kết quả tool, tóm tắt, `output_summary`.
- Model Router theo luật; tier; fallback.
- **Approval cơ bản** (một người duyệt, gắn hash arguments) và tool mức `write` / `destructive` trên non-prod.
- **Budget**: reserve / settle, budget theo execution, agent, project, org.

**Trạng thái (2026-11-18):** lát đầu "Temporal + worker" đã hiện thực và chạy trên máy cục bộ: vòng lặp `single_agent` là workflow + activity, process `worker` dưới role `app_worker`, execution token do `worker` phát, test kill worker / huỷ / trần theo org / replay với Temporal dev server thật. Tiêu chí "kill worker" mới được kiểm với lượt model và tool chỉ đọc; phần "không có tool ghi nào chạy hai lần" chờ tool ghi (cần `idempotency_key` và trạng thái `UNKNOWN`). Chưa làm trong gạch đầu dòng thứ nhất: luồng sự kiện SSE. Lát thứ hai "Model Router theo luật; tier; fallback" cũng đã hiện thực (cấu hình tier trong settings, chuỗi model chốt lúc tạo task, dự phòng trong một lượt, ghi quyết định vào step); phần chưa có của router (hạ / nâng tier, circuit breaker, phân loại dữ liệu) ghi ở [cost-management §5.3](architecture/cost-management.md). Lát thứ ba "Budget: reserve / settle" cũng đã hiện thực (budget theo organization / project / user / agent và budget của execution, đặt trước và quyết toán ở Gateway, `BUDGET_EXCEEDED`, 402 cho execution mới, fail-closed); phần lệch và nợ ghi ở [cost-management §4.4](architecture/cost-management.md). Tiêu chí "execution vượt budget dừng với `BUDGET_EXCEEDED`" đã có test tự động; "fan-out delegation bị chặn đúng trần" chờ Orchestrator. Lát thứ tư "Approval cơ bản và tool `write` / `destructive` trên non-prod" cũng đã hiện thực ([security §4.5](architecture/security.md)); tiêu chí "kill worker: không có tool ghi nào chạy hai lần" nay có test tự động trên Temporal thật. Lát thứ năm "Orchestrator" đã có phần `mode: planned` ([agent-runtime §5.8](architecture/agent-runtime.md)): kiểm kế hoạch, workflow cha + child workflow, chạy song song có trần, thất bại từng phần, budget theo task, lan truyền huỷ (tiêu chí "huỷ execution dừng mọi task con trong thời hạn xác định" có test tự động trên Temporal thật). Delegation cũng đã hiện thực ([agent-runtime §5.9](architecture/agent-runtime.md), [ADR-0014](adr/0014-internal-runtime-tools-and-delegation-decision-in-database.md) còn Đề xuất); tiêu chí "test fan-out delegation bị chặn đúng trần" có test tự động (execution không vượt 16 task, ba lần từ chối thì task dừng). Pause / resume đã hiện thực ([agent-runtime §5.10](architecture/agent-runtime.md)), kèm mốc versioning workflow đầu tiên. Planner LLM hiện thực ở dạng đề xuất kế hoạch ([agent-runtime §5.11](architecture/agent-runtime.md)): kế hoạch do model viết chạy qua `mode: planned` (người gọi gửi lại) hoặc qua `POST /executions/{id}/plan/run` (platform chạy, tool ghi luôn cần duyệt). Quản lý context có phần nén deterministic ([agent-runtime §6](architecture/agent-runtime.md)); tóm tắt bằng model và memory theo session chưa làm. Luồng sự kiện SSE đã có ([agent-runtime §4.1](architecture/agent-runtime.md)). Như vậy mọi gạch đầu dòng của phạm vi Phase 2 đã có hiện thực trên máy cục bộ; tiêu chí "Xong khi" thứ nhất (kịch bản bốn Agent **trên staging**) còn chờ môi trường staging.  Quyết định từng chặn việc lên staging (Temporal tự vận hành hay dịch vụ quản lý) nay đã chốt ở [ADR-0017](adr/0017-single-host-docker-compose-deployment.md): tự vận hành trong compose, mTLS bằng CA nội bộ; cụm đã dựng thử đầu-cuối trên máy cục bộ ở `HQ_ENV=staging` (2026-11-19), chưa dựng trên máy chủ thật.

**Xong khi:**

- Kịch bản "kiểm tra production" chạy trên staging với bốn Agent song song, tổng hợp, kế hoạch remediation, approval, thực thi, xác minh.
- Kill `worker` giữa execution: execution tiếp tục sau khi worker lên lại, không có tool ghi nào chạy hai lần (test tự động).
- Huỷ execution dừng mọi task con trong thời hạn xác định.
- Execution vượt budget dừng với `BUDGET_EXCEEDED`; test fan-out delegation bị chặn đúng trần.

## Phase 3 — Workflow

Phạm vi:

- Workflow do người dùng định nghĩa (DAG khai báo: bước agent, tool, approval, wait), được một workflow Temporal tổng quát diễn giải.
- Lịch chạy bằng Temporal Schedules.
- Retry, timeout, compensation theo từng bước.
- Approval nhiều cấp, điều kiện ABAC, hết hạn, thông báo (webhook; email / chat sau).
- Lịch sử thực thi và bản chiếu `workflow_executions`.
- Xoay vòng secret.

**Xong khi:** một workflow theo lịch (ví dụ kiểm tra sức khoẻ hằng ngày, tạo báo cáo, xin duyệt remediation) chạy ổn định nhiều ngày; nâng cấp phiên bản worker khi workflow đang chạy không làm hỏng workflow (có test replay).

**Trạng thái (2026-11-18):** lát "workflow + lịch chạy" đã có hiện thực trên máy cục bộ ([ADR-0015](adr/0015-workflows-compile-to-planned-executions.md), còn Đề xuất; [agent-runtime §5.12](architecture/agent-runtime.md)): định nghĩa có bước `agent`, `wait` và `approval` (cổng chờ 1–2 người duyệt khác nhau), lần chạy là một execution `planned`, lịch bằng Temporal Schedules, bản chiếu `workflow_executions`, test replay cho nút chờ và cho workflow kích hoạt lịch. Lệch câu chữ ở gạch đầu dòng thứ nhất: không viết bộ diễn giải Temporal riêng, dùng lại `PlannedExecution`. Bổ sung (2026-11-19): thông báo cho người duyệt qua webhook của organization (`approval.requested`, ký HMAC, gửi từ `gateway` qua SSRF guard; [security §4.7](architecture/security.md)) và thử lại theo bước `agent` (`max_attempts` 1–3; chỉ thử lại khi các lần thử trước chưa gọi tool ghi hay giao việc; có test replay `planned_parent_retry_v1.json`). Chưa có: bước `tool` (cần Gateway nhận lời gọi không gắn Agent Version; cùng bài toán với `direct_tool`, nên làm chung và cần quyết định thiết kế riêng); compensation theo bước; điều kiện ABAC cho approval (thuộc mô hình role binding có điều kiện của security §4.2, chưa có hiện thực nào ở `authz`; không nên làm riêng cho approval); email / chat cho thông báo; xoay vòng khoá gốc của secret (giá trị của từng secret đã xoay được bằng version mới; xoay khoá gốc chờ quyết định Cloud KMS, ADR-0012). Tiêu chí "chạy ổn định nhiều ngày" cần staging, chưa kiểm được.

## Phase 4 — Integrations

Thứ tự đề xuất theo rủi ro tăng dần và giá trị sớm:

| Đợt | Integration | Ghi chú |
|---|---|---|
| 4a | Docker, Kubernetes, Prometheus / Loki / Grafana, GitHub | Đọc trước, ghi sau; phục vụ DevOps và Monitoring Agent |
| 4b | PostgreSQL, MySQL, Redis | Đọc trên replica; không có tool SQL tự do trên production |
| 4c | Linux / SSH, Cloudflare, GCP, GitLab, CI/CD, Sentry | SSH chỉ qua tập lệnh định sẵn, không shell tự do |
| 4d | Shopify | Đọc trước |
| 4e | HQS, X-Purchase, CPV, hệ thống thanh toán | Sau cùng. Cần đặc tả API và quy tắc nghiệp vụ từ đội sở hữu; mọi tool ghi ở mức `financial` |

Kèm theo: STDIO bridge có sandbox, kiểm soát luồng dữ liệu (`data_sensitivity` / `egress`), bộ test hợp đồng cho integration, isolated runtime cho Agent quyền cao.

**Xong khi (mỗi integration):** qua bộ test hợp đồng; mọi tool đã phân loại rủi ro; tool ghi có idempotency và `dry_run` nếu hệ thống đích cho phép; có runbook.

**Trạng thái (2026-11-18):** đã có bộ test hợp đồng dùng chung cho managed MCP server (`internal/integration/contract`, mọi integration gọi `contract.Run` trong test của nó; [mcp-platform §7](architecture/mcp-platform.md)) và integration **Prometheus chỉ đọc** của đợt 4a (`internal/integration/prometheus`, binary `cmd/integration-prometheus`, runbook `.docs/runbooks/integration-prometheus.md`): `query_instant`, `query_range`, `list_alerts`, `list_targets`, `list_metric_names`. Docker đã chạy qua bộ test hợp đồng. Bổ sung (2026-11-19): ba integration chỉ đọc nữa của đợt 4a: **Loki** (`query_logs`, `query_metric`, `list_labels`, `list_label_values`), **Kubernetes** (`list_namespaces`, `list_pods`, `get_pod`, `get_pod_logs`, `list_deployments`, `get_deployment`, `list_events`, `list_nodes`; không Secret / ConfigMap / env) và **GitHub** (pull request, issue, workflow run, commit, commit status trên allowlist repository); mỗi cái có runbook và target `make run-<tên>-mcp`. Prometheus, Loki và Kubernetes đã chạy đầu-cuối qua Gateway với hệ thống thật trong container (Prometheus 3.0.0, Loki 3.0.0, k3s 1.31); GitHub mới chạy với API giả lập. Review bảo mật của lát này tìm ra và đã sửa: trần 24 giờ của Loki vòng qua được bằng LogQL (`[range]`, `offset`); `get_pod` của Kubernetes trả đuôi log và mảnh command line qua `message` của container; con trỏ phân trang tên `next_page_token` bị Gateway che nên phân trang hỏng (bộ test hợp đồng nay bắt lỗi này); tóm tắt review của GitHub đếm cả approval của người ngoài và approval cũ; GitHub không có ngắt mạch khi bị rate limit. Chưa có: Grafana và các đợt 4b–4d; ghi (mọi integration hiện chỉ đọc); chạy trên staging; STDIO bridge; kiểm soát luồng dữ liệu (`data_sensitivity` / `egress`); isolated runtime. Review của lát này tìm ra và đã sửa một lỗ hổng có cả ở Docker (ngữ cảnh discovery gọi được tool; nay handler nằm ở `internal/integration/serve`), và `get_container_logs` của Docker nay cắt log theo kích thước đã tuần tự hoá (60 KiB, giữ phần mới nhất) thay vì để Gateway cắt giữa chừng JSON.

**Điểm chưa rõ cần đầu vào:** HQS, X-Purchase và CPV là hệ thống nội bộ; thiết kế này chưa biết API, mô hình dữ liệu hay ràng buộc của chúng. Đợt 4e cần một bước khảo sát riêng trước khi ước lượng.

## Phase 5 — Cost Management hoàn chỉnh

Phần ghi và budget đã có. Phase này thêm:

- API tổng hợp và chuỗi thời gian; bảng tổng hợp theo ngày nếu cần.
- Cost Dashboard.
- Đối soát với billing của provider; cảnh báo lệch.
- Forecast.
- Budget theo user, theo ngày; cảnh báo ngưỡng mềm.
- Thêm provider (đa provider thật) và model nội bộ.
- Đánh giá router bằng dữ liệu đã thu: tỉ lệ thành công theo tier theo loại task.
- Cache kết quả tool đọc; tối ưu prompt caching.

**Xong khi:** lệch giữa ước tính và hoá đơn thật của một tháng nằm trong ngưỡng đã đặt; trả lời được "tháng này Agent nào, task loại nào tốn nhất" từ dashboard.

**Giao diện (2026-11-19):** bản đầu của giao diện web đã có ở `/ui/` (từ 2026-11-20 chuyển sang `/`) (tiếng Việt mặc định, có tiếng Anh): tổng quan, dự án, lần chạy, phê duyệt, workflow, chi phí theo Agent / model / dự án / người dùng, MCP, audit. Câu hỏi "tháng này Agent nào tốn nhất" trả lời được từ màn hình Chi phí. Chưa có biểu đồ theo thời gian và dự báo trên giao diện (API đã có), và chưa có màn hình cấu hình (Agent, workflow, budget, MCP server). Giao diện đã được chuyển từ JavaScript thuần sang React + TypeScript có bước build ([ADR-0016](adr/0016-react-web-console-with-build-step.md), còn Đề xuất), giữ nguyên các màn hình, mô hình triển khai và CSP; đây là nền để làm các màn hình còn thiếu.

**Trạng thái (2026-11-18):** lát "API tổng hợp và chuỗi thời gian, forecast" đã có hiện thực trên máy cục bộ (`GET /v1/usage/summary`, `/v1/usage/timeseries`; [cost-management §9](architecture/cost-management.md)): câu hỏi "Agent nào tốn nhất" trả lời được qua API. Chưa có: dashboard (UI), đối soát với billing của provider (cần dữ liệu hoá đơn thật), đa provider thật, đánh giá router (dữ liệu nằm ở `execution_steps.routing`; báo cáo theo thời gian cần một index trên bảng phân vùng đó và dữ liệu chạy thật mới có nghĩa), cache kết quả tool đọc, budget `user` cho execution khởi tạo bằng API key.

## Phase 6 — Production Hardening

- Kiểm thử xâm nhập, tập trung vào: cách ly tenant, vượt approval, SSRF, prompt injection dẫn tới hành động.
- Xuất audit ra kho ngoài; kiểm tra chuỗi hash định kỳ.
- HA: nhiều replica cho `api`, `gateway`, `worker`; Postgres có replica và failover; Temporal HA; Redis HA.
- Backup và khôi phục: **diễn tập khôi phục thật**, đo RPO / RTO đạt được rồi mới cam kết.
- Load test để tìm giới hạn (xem dưới).
- Tự động hoá triển khai: Helm / Kustomize, migration trong pipeline, canary cho `gateway`.
- SLO, cảnh báo, runbook, diễn tập sự cố.
- Ký image, SBOM, quét dependency.

**Bổ sung (2026-11-19):** hạng mục "tự động hoá triển khai" có bản đầu không dùng Kubernetes ([ADR-0017](adr/0017-single-host-docker-compose-deployment.md)): một image với tag bất biến do CI đẩy (`.github/workflows/release.yml`), `deployments/production/compose.yaml` cho một máy chủ, `bin/hqctl deploy <tag>` chạy migration rồi cập nhật worker → gateway → api theo healthcheck, sao lưu `pg_dump`. Đây là triển khai một replica, chưa phải HA; Helm / Kustomize, canary, ký image, SBOM chưa làm.

**Trạng thái (2026-11-18):** lát "kiểm chuỗi hash và xuất audit" đã có (`hqadmin audit verify | export | verify-file`; [security §9](architecture/security.md)). Các hạng mục còn lại cần hạ tầng thật và chưa làm được trên máy cục bộ: kiểm thử xâm nhập trên staging, HA, diễn tập khôi phục, load test, pipeline triển khai, SLO, ký image.

### Kế hoạch benchmark và load test

Chưa đặt mục tiêu throughput. Kế hoạch để có con số:

| Đo gì | Cách đo |
|---|---|
| Điểm bão hoà của `gateway` (tool call / giây, độ trễ p50 / p95 / p99) | MCP server giả lập với độ trễ cấu hình được; tăng tải tới khi p99 vượt ngưỡng |
| Số execution đồng thời trên một `worker` | LLM giả lập có độ trễ và streaming thực tế; đo CPU, RAM, goroutine |
| Thông lượng Temporal (workflow / activity mỗi giây) | Tải tổng hợp; theo dõi độ trễ schedule-to-start |
| Postgres | Tốc độ ghi `execution_steps` và `usage_records`; độ trễ truy vấn báo cáo trên dữ liệu tổng hợp nhiều tháng |
| Độ chính xác budget dưới tải song song | Nhiều execution cùng tiêu một budget; đo mức vượt tối đa |
| Hành vi khi suy giảm | Tắt Redis, làm chậm Postgres, provider trả 429: kiểm tra backpressure và fail-closed |

Kết quả các phép đo này là căn cứ để đặt SLO và quyết định có cần tách service, thêm message broker hay lên Kubernetes hay không.

## Rủi ro cần xử lý

| # | Rủi ro | Ảnh hưởng | Hướng xử lý |
|---|---|---|---|
| R1 | Prompt injection dẫn tới hành động phá hoại | Rất cao | Quyền ngoài model, approval gắn arguments, tách đọc / ghi. Rủi ro còn lại: người duyệt bấm duyệt theo thói quen → thiết kế màn hình duyệt và giới hạn tần suất là việc phải làm, không phải phụ |
| R2 | Đặc tả MCP thay đổi nhanh | Trung bình | Cô lập mọi phụ thuộc giao thức trong `internal/mcp`; theo dõi SDK chính thức; test tương thích với nhiều phiên bản giao thức |
| R3 | Phạm vi quá lớn so với nguồn lực | Cao | Một use case dẫn đường; mỗi phase có tiêu chí xong; cắt integration trước khi cắt kiểm soát |
| R4 | Độ phức tạp vận hành của Temporal | Trung bình | Cân nhắc dịch vụ quản lý ở giai đoạn đầu; giữ history nhỏ; test replay trong CI |
| R5 | Tool không idempotent trên hệ thống ngoài | Cao với financial | Trạng thái `unknown` + dừng chờ người; không tự retry; ưu tiên API đích có idempotency key |
| R6 | Chi phí LLM vượt kiểm soát | Cao | Budget cứng từ Phase 2, trần cấu trúc, cảnh báo |
| R7 | Lỗi cách ly tenant | Rất cao | RLS + test bắt buộc + khoá ngoại ghép |
| R8 | Chất lượng Agent không đủ tin cậy cho tác vụ ghi | Cao | Chưa có trong thiết kế: **bộ đánh giá (eval)** theo kịch bản cho từng Agent, chạy khi đổi prompt hoặc model. Đề xuất bổ sung vào Phase 2 |
| R9 | Hệ thống nội bộ (HQS, X-Purchase, CPV) thiếu API phù hợp | Chưa rõ | Khảo sát sớm, song song Phase 1–2 |
| R10 | Dữ liệu nhạy cảm gửi tới LLM provider bên ngoài | Cao với dữ liệu tài chính | Chính sách provider theo phân loại dữ liệu; model nội bộ cho dữ liệu không được rời hạ tầng; cần quyết định về pháp lý |
