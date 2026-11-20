# Audit giao diện web trước khi chuyển thành Agent Control Plane

Trạng thái: bản đầu · 2026-11-20 · nhánh `ui/control-plane`

Liên quan: [ADR-0016](adr/0016-react-web-console-with-build-step.md), [security §4.1](architecture/security.md), [openapi.yaml](api/openapi.yaml), `.claude/rules/web-ui.md`, [kế hoạch chuyển đổi](ui-refactor-plan.md), [tiến độ theo lát](ui-progress.md).

Tài liệu này ghi lại hiện trạng của `web/` tại commit `0dd351e`, đối chiếu với yêu cầu "AI Agent Control Plane" (bản yêu cầu gốc viết bằng tiếng Anh, đặt tài liệu ở `docs/`; repo đặt tài liệu ở `.docs/` và viết bằng tiếng Việt, nên hai file nằm ở đây).

## 1. Hiện trạng

| Hạng mục | Hiện có |
|---|---|
| Framework | React 19.3.1, TypeScript 5.9.3 (`strict`, `noUncheckedIndexedAccess`), Vite 8.3.8 |
| Điều hướng | `react-router` 8.5.2, `HashRouter`; bảng route ở `app/routes.tsx`, mọi màn hình nạp lười |
| Dữ liệu máy chủ | `@tanstack/react-query` 5.108.0; mặc định không thử lại, không tải lại khi chọn cửa sổ (một phần của mô hình bảo mật màn hình duyệt) |
| Trạng thái giao diện | `useState` + `useStored` (`sessionStorage`); không có store toàn cục |
| Kiểu API | `api/schema.gen.ts` sinh từ `openapi.yaml` (81 operation) |
| Gọi API | Chỉ ở `api/client.ts` (`Idempotency-Key`, `If-Match`, lỗi RFC 9457), `api/paging.ts` (keyset), `api/events.ts` (SSE đọc bằng `fetch`) |
| Realtime | SSE theo từng execution (`GET /v1/executions/{id}/events`). Không có WebSocket, không có luồng sự kiện cấp organization |
| Xác thực | OIDC Authorization Code + PKCE (`auth/oidc.ts`), token trong `sessionStorage` |
| Phân quyền | Giao diện không quyết định quyền; API trả 403 / 404, lời gọi phụ bọc `soft()` |
| Thư viện giao diện | Không có. CSS thuần + biến CSS (`styles/tokens.css`), biểu tượng và sơ đồ tự vẽ |
| i18n | `vi.ts` (chuẩn) + `en.ts`, kiểu buộc cùng bộ khoá |
| Test | Vitest + Testing Library, 203 test / 29 file trước thay đổi này; chưa có test trong trình duyệt |
| Triển khai | File tĩnh nhúng vào process `api`; CSP `default-src 'none'`, `script-src 'self'`, `style-src 'self'` |

Ràng buộc do máy kiểm (ESLint + test Go): feature không import feature khác; chỉ `api/` và `auth/` gọi mạng; cấm `innerHTML` và tương đương; `import()` động chỉ ở bảng route; import ra ngoài thư mục dùng `@/`.

## 2. Phân loại thành phần

**Dùng lại nguyên vẹn** (logic nghiệp vụ và hợp đồng API, không đổi):

- `api/*`, `auth/*`, `app/query-client.ts`, `lib/*` (định dạng, `visible`, quy tắc approval, SSE, luồng của lần chạy).
- `components/`: `Async`, `DataTable`, `LoadMore`, `Form` + `useSubmit`, `useConfirm`, `Section`, `Skeleton`, `StatusBadge` / `RiskBadge`, `Empty`, `ErrorBox`, `KV`.
- Mọi màn hình trong `features/` (dự án, lần chạy, phê duyệt, mô hình, MCP, chi phí, audit, organization, trợ lý).

**Thiết kế lại** (giữ hành vi, đổi trình bày):

- `styles/tokens.css`: tối là mặc định, bảng màu navy + emerald; sáng thành lựa chọn.
- `app/shell.tsx`: thanh bên phẳng → thanh bên theo nhóm, thu gọn được, có biểu tượng; thêm thanh trên.

**Thay thế:** không có. Không màn hình nào bị xoá.

**Phải giữ nguyên:** ba bất biến của màn hình duyệt (không duyệt khi thiếu nội dung; luôn tải trạng thái mới; trang ra quyết định không tự làm mới và không có chuyển động), mặc định của `QueryClient`, cách dùng lại `Idempotency-Key`.

## 3. Yêu cầu của Control Plane đối chiếu với API

Cột "API" là điều kiện để làm thật. Nơi API chưa có, giao diện **không** dựng dữ liệu giả (rule `web-ui.md`, skill `/add-screen`): mục đó không có trên điều hướng cho tới khi có endpoint.

| Yêu cầu | API hiện có | Kết luận |
|---|---|---|
| Khung trang, điều hướng theo nhóm, breadcrumb, bảng lệnh | Không cần | Làm ngay |
| Agent Inventory | `GET /projects/{id}/agents` (lọc `status`, `q`, `sort`, keyset) | Làm, theo từng dự án. Chưa có liệt kê xuyên dự án |
| Agent Map | Agent, Agent Version (`config.tools`, `delegation`), MCP server, tool, approval, execution, task, step | Làm bằng dữ liệu thật; ghép từ nhiều lời gọi (xem §6) |
| Trạng thái Agent "Running / Idle / Failed / Maintenance" | Chỉ có vòng đời `draft / active / disabled / archived` | Hiện vòng đời thật; "đang chạy" suy từ execution đang chạy |
| Owner / team của Agent | Không có | Hiện dự án và môi trường thay thế |
| Mức rủi ro của Agent | `risk_level` của tool | Suy ra: mức cao nhất trong các tool Agent gọi được |
| Số liệu của server (số request, độ trễ, lỗi gần đây) | Không có | Không hiện |
| Lịch sử gọi của tool | Audit `mcp_tool.call` theo `resource_id` (chỉ `org_admin`) | Hiện khi đọc được audit; không thì nói rõ |
| Node "Policy", trình quản lý policy | Policy là code ở Gateway, không phải dữ liệu | Không có node; quyết định hiện trên cạnh |
| Live Monitor, luồng hoạt động toàn organization | Chỉ có SSE theo execution | Hoãn. Bản đồ có bảng "đang chạy" (đọc định kỳ) |
| Traces, span, waterfall | `trace_id` trên execution; không có API span | Hoãn. Chi tiết lần chạy đã có bước và sơ đồ luồng |
| Evaluations | Không có | Hoãn |
| Secrets & Credentials, Team & Members, Environments | Không có endpoint quản lý | Hoãn |
| API Keys, Budget, Webhook thông báo | Có endpoint, chưa có màn hình | Lát 6 và 9 của `ui-progress.md` |
| Analytics: usage, token, chi phí | `/usage/summary`, `/usage/timeseries`, `/usage/records` | Màn hình Chi phí đã có; biểu đồ là lát 10 |
| Analytics: latency P50 / P95, throughput, reliability | Không có | Hoãn |
| Tìm kiếm chung (Agent, lần chạy, trace, audit) | Không có endpoint tìm chung | Bảng lệnh chỉ tìm màn hình và dự án |
| Environment switcher | Môi trường là thuộc tính của dự án | Không có công tắc toàn cục; lọc theo môi trường ở từng màn hình |

## 4. Stack được yêu cầu đối chiếu với ADR-0016

Bản yêu cầu cho phép giữ stack hiện có khi có ràng buộc tương thích. Ràng buộc ở đây là ADR-0016: mỗi phụ thuộc lúc chạy là mã chạy trên origin giữ token của người dùng, và CSP không được nới.

| Thư viện được yêu cầu | Quyết định | Lý do |
|---|---|---|
| React, TypeScript, Vite, React Router, TanStack Query, Vitest, Testing Library | Đã có | |
| `@xyflow/react` | **Thêm**, 12.12.1 | Xem §5 |
| Dagre / ELK | Không thêm | Xem §5 |
| Tailwind, shadcn/ui, Radix | Không thêm | Hệ token + CSS hiện có đủ dùng; đổi là viết lại mọi màn hình mà không thêm khả năng nào. `<dialog>`, `<details>` của trình duyệt lo bẫy tiêu điểm và đóng bằng Esc |
| Lucide React | Không thêm | Khoảng 35 biểu tượng tự vẽ ở `components/icon.tsx` |
| Zustand | Không thêm | Chưa có trạng thái giao diện nào cần chia sẻ ngoài một trang |
| React Hook Form, Zod | Không thêm | Bộ form hiện có đã xử lý lỗi theo trường của API; API là nơi validate |
| Framer Motion | Không thêm | Rule "Token và chuyển động" đã đánh giá `motion` và từ chối |
| Apache ECharts | Chưa quyết định | Cần cho lát 10 (biểu đồ chi phí). Tooltip của nó dựng HTML: phải đánh giá bằng `/add-ui-dependency` trước |
| TanStack Table | Không thêm | `DataTable` + phân trang keyset đủ; danh sách không tải hết về trình duyệt nên không cần ảo hoá phía client |
| Playwright | Chưa thêm | Lát 11 của `ui-progress.md` |
| Storybook | Không thêm | Chỉ là phụ thuộc lúc build nhưng kéo theo hàng trăm gói |
| Font Inter / Geist | Không nhúng | Dùng nếu máy có sẵn, không thì phông hệ thống (đủ dấu tiếng Việt, không thêm file) |

## 5. Đánh giá phụ thuộc theo `/add-ui-dependency`

**`@xyflow/react` 12.12.1** (lúc chạy; MIT; phát hành 2026-11-02).

- Cần vì: canvas kéo / thu phóng, chọn nhiều, minimap, vẽ cạnh cong giữa các node. Tự viết là vài nghìn dòng. ADR-0016 đã dự liệu một thư viện đồ thị.
- Chọn 12.12.1 thay vì 13.0.2: nhánh 13 mới phát hành 3 ngày, ba bản trong hai ngày.
- Phụ thuộc bắc cầu: 20 gói (`@xyflow/system`, `zustand`, `classcat`, `use-sync-external-store`, 9 gói `d3-*` và kiểu của chúng). Không gói nào có script cài đặt.
- CSP, kiểm bằng cách quét mã của gói trong `node_modules`: không chèn thẻ `<style>`, không `eval` / `new Function`, không gọi mạng, không tải tài nguyên ngoài. `innerHTML` chỉ có trong `d3-selection` (`selection.html()`), và không gói nào trong chuỗi gọi nó. Style của node đặt qua thuộc tính `style` của React (CSSOM), CSP cho phép. CSS của gói (`base.css`) vào file CSS của bản build.
- Giữ liên kết attribution của thư viện (điều kiện dùng bản miễn phí).
- Bundle: thư viện chỉ nằm trong chunk nạp lười của màn hình bản đồ (`agent-map-page`, 222 kB, 70 kB gzip). Chunk chính 309 kB (98 kB gzip).
- `npm audit --audit-level=high`: 0 lỗ hổng. `go test ./internal/webui/`: qua.
- **Chưa kiểm:** chưa mở bản build trong trình duyệt dưới CSP thật. Đây là bước duy nhất bắt được vi phạm CSP lúc chạy.

**Dagre / ELK: không thêm.** Đồ thị của bản đồ đã có tầng (loại node quyết định cột), nên phần chính của hai thư viện này (gán tầng) không dùng tới. Phần còn lại (xếp thứ tự trong tầng, đặt toạ độ) là 80 dòng ở `lib/graph-layout.ts`, có test, bố cục 200 node dưới 500 ms. Xem xét lại khi bản đồ cần đồ thị không có tầng sẵn (ví dụ DAG của workflow).

## 6. Giới hạn đã biết của bản đồ

- Không có endpoint topology: bản đồ đọc dự án → Agent của từng dự án → version của từng Agent (tối đa 6 lời gọi đồng thời, trần 200 Agent). Với organization lớn đây là hàng trăm request mỗi lần mở. Cách sửa đúng là một endpoint ở `/v1` (`/add-endpoint`).
- Quyết định allow / deny trên bản đồ được tính lại ở trình duyệt theo luật của Gateway (`resolveAccess`), để hiển thị. Luật ở Gateway đổi mà hàm này không đổi thì bản đồ nói sai. Gateway vẫn là nơi duy nhất thực thi.
- "Hệ thống đích" là tên máy của endpoint MCP server, không phải hệ thống mà tool chạm tới.
- Không có số lần gọi hay hoạt động gần nhất theo cạnh.
