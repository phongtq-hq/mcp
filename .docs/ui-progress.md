# Tiến độ hoàn thiện giao diện web

Trạng thái: đang làm · cập nhật 2026-11-20 · nhánh `ui/complete-console` (chưa đẩy lên remote)

Liên quan: [ADR-0016](adr/0016-react-web-console-with-build-step.md) (giao diện React), [security §4.1](architecture/security.md), [openapi.yaml](api/openapi.yaml), luật của phần giao diện ở `.claude/rules/web-ui.md`.

## 1. Mục tiêu

Một quản trị viên làm được mọi việc mà Management API (`/v1`) cho phép ngay trên giao diện (`/`), không cần `curl` hay `hqadmin`. Việc được chia thành 11 lát; mỗi lát xong hẳn (code, test, review, commit) rồi mới sang lát sau.

Thước đo: số operation của `openapi.yaml` có màn hình dùng tới. Hiện **35 / 81** (lúc bắt đầu: 21 / 81). Con số lấy từ một script đối chiếu đường dẫn trong `web/src` với spec, có rà tay hai chỗ script đếm nhầm.

## 2. Các lát

| # | Lát | Trạng thái | Commit |
|---|---|---|---|
| 1 | Nền: `If-Match` / ETag, form, hộp xác nhận, phân trang, bộ đọc luồng sự kiện | Xong | `1a6cb21` |
| 2 | Tạo dự án, Cài đặt dự án, trang Organization, điều hướng theo nhóm | Xong | `bc00cea` |
| — | API: lỗi validate theo trường (`errors[]`), `If-Match` cho `PATCH /models` | Xong | `91e8f6c` |
| — | Client dùng lại `Idempotency-Key` khi gửi lại sau lỗi không rõ kết quả | Xong | `b568d91` |
| 3 | Mô hình: provider, model, bảng giá, định tuyến theo tier | Xong | `643bf95` |
| 4 | Agent và Agent Version: tạo, version mới, publish / rollback, vòng đời | **Kế tiếp** (có bản nháp form cấu hình, chưa vào repo) | |
| 5 | MCP server và tool: đăng ký server ngoài, discovery, duyệt version, phân loại tool | Chưa làm | |
| 6 | Budget | Chưa làm | |
| 7 | Lần chạy: dòng thời gian theo sự kiện, pause / resume, sơ đồ DAG, đề xuất kế hoạch | Chưa làm | |
| 8 | Workflow: soạn định nghĩa, lịch cron, bật / tắt, chi tiết lần chạy | Chưa làm | |
| 9 | API key, webhook thông báo, bộ lọc audit | Chưa làm | |
| 10 | Chi phí: biểu đồ theo thời gian, dự báo, usage records | Chưa làm | |
| 11 | Test đầu-cuối trong trình duyệt cho ba luồng chính | Chưa làm | |

Hai commit khác trên cùng nhánh không thuộc việc giao diện: `5a7f8c4` (triển khai bằng Docker Compose, ADR-0017) và `c0112d8` (rule / agent / skill cho giao diện trong `.claude/`).

## 3. Đã có sau ba lát

**Nền dùng chung** (`web/src`):

- `api/client.ts`: `getWithETag`, `api(..., { ifMatch })`; `ApiError` mang lỗi theo trường; gửi lại đúng một `POST` sau khi mất mạng hoặc 5xx thì dùng lại `Idempotency-Key` cũ.
- `api/paging.ts` + `components/load-more.tsx`: phân trang keyset.
- `api/events.ts` + `lib/sse.ts`: đọc luồng sự kiện của execution bằng `fetch` (không cần đổi API). **Chưa màn hình nào dùng.**
- `components/form.tsx`, `use-submit.ts`, `confirm.tsx`, `sections.tsx`, `settings-form.tsx`, `routing-form.tsx`.

**Màn hình mới:**

- Dự án: tạo dự án; tab Cài đặt (tên, điều phối `mode planned`, định tuyến ghi đè theo tier, settings dạng JSON, lưu trữ).
- Quản trị → Organization: tên và settings.
- Mô hình → Model: danh sách, thêm, sửa; bảng giá (lịch sử, thêm giá).
- Mô hình → Provider: danh sách, thêm, bật / tắt, thay khoá API (chỉ ghi), settings, xoá. Provider của platform chỉ đọc.
- Mô hình → Định tuyến theo tier của organization.

**Thay đổi phía máy chủ đi kèm:**

- `errors[]` trong phản hồi lỗi validate (`apperr.InvalidField`), đã chuyển ở: project / organization (gồm `settings.model_routing`, `settings.orchestration`), provider / model, bảng giá. Các context khác vẫn chỉ trả `detail`.
- `PATCH /v1/models/{id}` nhận `If-Match`.
- `POST /v1/models/{id}/pricing`: `effective_from` tuỳ chọn; thiếu thì áp dụng ngay theo giờ máy chủ.

**Hành vi đáng biết của các màn hình sửa:**

- `PATCH` luôn kèm `If-Match`; máy chủ không gửi ETag thì giao diện từ chối lưu.
- 412 hiện "đã bị sửa ở nơi khác"; 409 / 422 / 403 hiện nguyên thông báo của API.
- Trang có nhiều form cùng sửa một tài nguyên: mục đang sửa dở khoá các mục khác cho tới khi lưu hoặc bỏ.
- Khoá API bị xoá khỏi ô nhập ngay khi máy chủ trả lời từ chối (4xx); chỉ giữ khi kết cục không rõ để gửi lại được.

## 4. Đã kiểm và chưa kiểm

| Kiểm tra | Kết quả |
|---|---|
| `make ui-lint` (kiểu + ESLint) | Sạch |
| `make ui-test` | 138 test qua (19 file) |
| `make ui-build`, `go test ./internal/webui/` | Qua |
| `make lint`, `make generate-check` | Sạch |
| Test API trên PostgreSQL thật (model, project, bảng giá) | Qua |
| `ui-reviewer` cho cả ba lát; `security-reviewer` cho lát 3 | Không có mục chặn; các mục "nên sửa" đã sửa |

Chưa kiểm:

- **Chưa mở màn hình nào trong trình duyệt.** Phiên làm việc không có công cụ điều khiển trình duyệt, và cụm staging cục bộ đang chạy image cũ (tag `staging-20261119`, chưa có các màn hình này). Hành vi thật của `<dialog>` (bẫy tiêu điểm, Esc), bố cục CSS, ô `datetime-local` và vi phạm CSP mới chỉ được kiểm bằng test trên jsdom và đọc code.
- Chưa chạy lại toàn bộ `make test` sau các thay đổi phía máy chủ.

## 5. Giới hạn do API (giao diện không lấp)

- Không có endpoint quản lý thành viên và role: thêm người vào organization và gán quyền vẫn ngoài giao diện. Đã chốt để ngoài phạm vi lần này.
- Không gỡ được tier của một model (máy chủ giữ giá trị cũ khi nhận `null`); chưa đặt được `deprecated_at`.
- `Model` không có trường `scope`: form sửa và form giá của model thuộc platform vẫn hiện, bị từ chối lúc lưu (403).
- Danh sách chọn model trong form định tuyến chỉ gồm 200 mục đầu (có ghi chú trên màn hình).
- `GET /costs/reconciliation` có trong spec nhưng chưa có route: không làm màn hình.
- Phê duyệt: `agent_rationale` và `dry_run_result` chưa có trong API.
- Không có endpoint "tôi có quyền gì": menu hiện đủ mọi mục, người thiếu quyền gặp 403 sau khi bấm.
- Chỉ làm qua `hqadmin` (đường xuyên tenant, cố ý không đưa lên giao diện): tạo organization, đăng ký managed MCP server của platform, kiểm và xuất audit, bảo trì.

## 6. Câu hỏi đang chờ quyết định

1. **Quyền sửa định tuyến của dự án.** `project_admin` (có `project:update`) trỏ được một tier của dự án sang model đắt nhất mà tenant thấy; chỉ budget chặn, có audit. Giữ, hay tách thành quyền riêng?
2. **Thêm `scope` vào `Model` trong spec** để giao diện ẩn form của model thuộc platform, như đã làm với provider?
3. **Lọc tên khoá giống bí mật trong settings của organization / project** (provider đã có bộ lọc này)? Ô JSON tự do nên có thể dán nhầm một khoá vào settings, và settings đọc được bởi mọi người có quyền đọc.

Không câu nào chặn lát 4.

## 7. Operation chưa có màn hình (46)

| Nhóm | Operation |
|---|---|
| Agent (lát 4) | `POST /projects/{id}/agents`, `GET` / `PATCH` / `DELETE /agents/{id}`, `GET` / `POST /agents/{id}/versions`, `GET /agents/{id}/versions/{version}`, `PUT /agents/{id}/active-version` |
| MCP (lát 5) | `POST /mcp-servers`, `GET` / `PATCH` / `DELETE /mcp-servers/{id}`, `POST …/discover`, `GET …/versions`, `POST …/versions/{version}/review`, `GET …/tools`, `GET` / `PATCH /mcp-tools/{id}` |
| Budget (lát 6) | `GET` / `POST /budgets`, `GET` / `PATCH` / `DELETE /budgets/{id}` |
| Lần chạy (lát 7) | `POST /executions/{id}/pause`, `…/resume`, `GET …/events`, `GET /tasks/{id}`, `POST /projects/{id}/plan-proposals`, `GET /executions/{id}/plan`, `POST …/plan/run` |
| Workflow (lát 8) | `POST /projects/{id}/workflows`, `GET` / `PATCH` / `DELETE /workflows/{id}`, `GET /workflow-executions/{id}`, `POST …/cancel` |
| API key, webhook (lát 9) | `GET` / `POST /api-keys`, `DELETE /api-keys/{id}`, `GET` / `PUT` / `DELETE /notification-webhook`, `GET …/deliveries` |
| Chi phí (lát 10) | `GET /usage/timeseries`, `GET /usage/records` |
| Không làm | `GET /costs/reconciliation` (chưa có route) |

## 8. Làm tiếp như thế nào

- Lát 4 trước. Bản nháp form cấu hình Agent Version nằm ngoài repo, trong thư mục tạm của phiên làm việc, và có thể đã mất; viết lại từ schema `AgentConfig` nếu cần. Khi làm lát này, chuyển các lỗi validate của `internal/agent/catalog` sang `apperr.InvalidField` (tên trường dạng `config.limits.max_iterations`).
- Mỗi lát theo đúng trình tự của skill `/add-screen`: kiểm `openapi.yaml` trước, viết màn hình, test Vitest, `make ui-lint && make ui-test && make ui-build`, `go test ./internal/webui/`, gọi `ui-reviewer` (thêm `security-reviewer` khi có secret, đăng nhập hoặc approval), cập nhật `.claude/CLAUDE.md` và file này, rồi commit.
- Hai gói lúc build đã được duyệt nhưng chưa cài: `@testing-library/user-event` (chưa cần tới) và Playwright (cho lát 11). Chưa có gói lúc chạy nào được thêm; biểu đồ và sơ đồ DAG dự định tự viết bằng SVG.
- Trước khi coi một lát là xong về mặt trải nghiệm, cần một lần bấm thử trong trình duyệt: build lại image rồi `hqctl deploy`, hoặc `make run-api` + `make ui-dev`.
