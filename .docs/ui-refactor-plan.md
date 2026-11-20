# Kế hoạch chuyển giao diện thành Agent Control Plane

Trạng thái: đang làm · cập nhật 2026-11-20 · nhánh `ui/control-plane`

Liên quan: [checklist tiến độ](ui-checklist.md), [audit](frontend-audit.md), [tiến độ theo lát](ui-progress.md), [ADR-0016](adr/0016-react-web-console-with-build-step.md), `.claude/rules/web-ui.md`.

## 1. Nguyên tắc

1. Giao diện là một client của `/v1`. Màn hình cần dữ liệu chưa có thì thêm endpoint trước (`/add-endpoint`); không dựng dữ liệu giả, không adapter mock trong sản phẩm. Fixture chỉ dùng cho test.
2. Không màn hình rỗng: mục chưa làm được thì không có trên điều hướng.
3. Không đổi hợp đồng API, không đổi mặc định của `QueryClient`, không đổi ba bất biến của màn hình duyệt.
4. Mỗi phụ thuộc lúc chạy qua `/add-ui-dependency`; CSP không nới.
5. Mỗi phase xong hẳn: kiểu, ESLint, test, build, `go test ./internal/webui/`.

## 2. Kiến trúc thông tin

Điều hướng định nghĩa ở một chỗ (`web/src/app/navigation.ts`), dùng cho thanh bên, breadcrumb và bảng lệnh.

| Nhóm | Mục đang có | Mục dự kiến (chưa hiện) | Chờ gì |
|---|---|---|---|
| Tổng quan | Tổng quan | Live Monitor | Luồng sự kiện cấp organization |
| Điều khiển | Agent, Bản đồ Agent, Dự án (lần chạy, workflow, cài đặt), MCP server và tool, Mô hình | Evaluations; tạo / sửa Agent; đăng ký MCP server | API eval; lát 4, 5 |
| Quan sát | (lần chạy nằm trong Dự án) | Lần chạy toàn organization, Traces, Logs & Events | Liệt kê execution xuyên dự án; API span |
| Bảo mật và quản trị | Phê duyệt, Nhật ký audit | Policies, Permissions & Access, Secrets | Policy thành dữ liệu; endpoint quản lý membership / secret |
| Phân tích | Chi phí | Usage & Tokens (biểu đồ), Performance, Reliability | Lát 10; số đo latency / lỗi |
| Quản trị | Organization | Team & Members, API Keys, Budget, Webhook | Lát 6, 9; endpoint membership |

## 3. Các phase

| # | Phase | Trạng thái | Ghi chú |
|---|---|---|---|
| 1 | Nền: audit, token tối, khung trang, điều hướng, bảng lệnh, inspector | **Xong** | |
| 2 | Quản lý Agent: Inventory + inspector | **Xong phần đọc** | Tạo / sửa Agent, trang chi tiết nhiều tab: lát 4 |
| 3 | Bản đồ Agent | **Xong** (chưa kiểm trong trình duyệt) | |
| 4 | Quan sát: Live Monitor, Execution Explorer, Trace, Span | Chưa làm | Chi tiết lần chạy + sơ đồ luồng + SSE đã có từ trước. Phần còn lại chờ API |
| 5 | Bảo mật: Policies, Access, Decision Explorer | Chưa làm | Phê duyệt và Audit đã có. Decision Explorer làm được từ audit (`outcome=denied`): việc kế tiếp hợp lý |
| 6 | Phân tích | Chưa làm | Cần quyết định thư viện biểu đồ |
| 7 | Tích hợp và chất lượng | Một phần | Test đơn vị và build đã qua; chưa có test trong trình duyệt |

## 4. Đã làm ở lượt này

**Phase 1**

- `styles/tokens.css`: tối là mặc định (nền `#090D16`, bề mặt `#111827`, nhấn emerald `#10D6A0`); sáng bật bằng `data-theme="light"` (`lib/theme.ts`, lưu ở `localStorage` như ngôn ngữ). Component chỉ dùng token theo nghĩa.
- `app/shell.tsx`: thanh bên theo nhóm có biểu tượng, thu gọn được (giữ trong phiên), ngăn trượt ở màn hình hẹp; thanh trên có breadcrumb, nút mở bảng lệnh, chuông approval đang chờ, đổi sáng / tối, menu tài khoản (ngôn ngữ, đăng xuất).
- `app/command-palette.tsx`: Ctrl / Cmd + K, tìm không dấu trong màn hình và dự án, điều khiển bằng phím.
- `components/icon.tsx`, `components/inspector.tsx` (ngăn chi tiết bên phải dùng chung, Esc để đóng).

**Phase 2**

- `/agents` (`features/agents/agents-page.tsx`): chọn dự án, lọc trạng thái, tìm theo tên / slug, sắp xếp (đều do máy chủ làm), phân trang keyset; bấm một dòng mở inspector: tổng quan, cấu hình của version đang hoạt động, quyền gọi tool theo server, lần chạy gần đây.

**Phase 3**

- `/agents/map` (`features/agent-map/`):
  - `model.ts` (kiểu), `source.ts` (đọc API), `adapter.ts` (thực thể → đồ thị), `view.ts` (lọc, thu gọn, tập liên quan, tìm, toạ độ), `lib/graph-layout.ts` (bố cục theo tầng).
  - Node riêng cho Control Plane, Agent, MCP server, tool, hệ thống đích; cạnh riêng với bốn trạng thái (dùng được, cần duyệt, bị từ chối, không dùng được) phân biệt bằng kiểu nét.
  - Thu phóng, kéo, vừa khung, minimap, kéo node, chọn nhiều, tìm rồi đưa node vào giữa, lọc (dự án, môi trường, trạng thái, rủi ro, loại node, ẩn node không quan hệ), mở / thu tool theo server, đặt lại bố cục, chú giải.
  - Chọn node hoặc cạnh: phần không liên quan mờ đi và inspector mở ra. Chọn Agent thì tool bị `deny` hiện nét từ chối.
  - Bảng "đang chạy": lần chạy đang diễn ra (đọc lại mỗi 10 giây); chọn một lần chạy thì Agent và tool nó đã gọi được đánh dấu, có chấm chạy trên cạnh. Bản đồ không chuyển động khi không có gì đang diễn ra.
  - Bản đồ trên 30 tool mở ra với tool thu gọn; trên 120 node chỉ vẽ phần trong khung nhìn.

## 5. Lệch so với bản yêu cầu, và lý do

| Yêu cầu | Đã làm | Lý do |
|---|---|---|
| Tài liệu ở `docs/`, tiếng Anh | `.docs/`, tiếng Việt | Quy ước của repo |
| Stack Tailwind / shadcn / Zustand / RHF / Zod / Framer Motion / TanStack Table / Storybook | Giữ stack hiện có, thêm đúng một thư viện | ADR-0016; xem audit §4 |
| Dagre hoặc ELK | Bố cục tự viết | Audit §5 |
| Mock adapter cho tính năng thiếu API | Không có; mục đó ẩn | Rule `web-ui.md` cấm dữ liệu minh hoạ |
| Mock fixture cho bản đồ | Fixture chỉ trong test (`fixtures.ts`), bản đồ đọc API thật | Như trên |
| `PolicyNode`, trạng thái Running / Idle / Maintenance, owner / team, số liệu theo cạnh | Không có | API không có dữ liệu đó (audit §3) |
| Toàn bộ menu ở mục 4 của bản yêu cầu | 10 mục có thật | "Không hiện trang rỗng" |
| Command Center với biểu đồ, Live Monitor, Trace viewer, Security Center, Analytics | Chưa làm | Phase 4–6 |

## 6. Đã kiểm và chưa kiểm

| Kiểm tra | Kết quả |
|---|---|
| `tsc --noEmit`, ESLint | Sạch |
| Vitest | 221 test / 32 file qua (thêm 18 test) |
| `vite build`, `go test ./internal/webui/` | Qua |
| `npm audit --audit-level=high` | 0 lỗ hổng |

Chưa kiểm:

- **Chưa mở trong trình duyệt.** Bố cục CSS, giao diện sáng, hành vi kéo / thu phóng thật của canvas, minimap, chấm chạy trên cạnh (`offset-path`) và vi phạm CSP lúc chạy mới chỉ được kiểm bằng jsdom và đọc mã. Trong jsdom node của canvas không được đo kích thước, nên test bản đồ không kiểm vị trí vẽ.
- Chưa chạy với một organization thật có hàng trăm Agent; con số 200 node là từ fixture.
- Độ tương phản màu của bảng màu mới chưa đo bằng công cụ.

## 7. Việc kế tiếp, theo thứ tự đề xuất

1. Mở bản build qua `api` trong trình duyệt, console mở: xác nhận không có vi phạm CSP, sửa bố cục.
2. Endpoint topology ở `/v1` thay cho chuỗi lời gọi của `source.ts`.
3. Lát 4 của `ui-progress.md` (tạo / sửa Agent) và trang chi tiết Agent.
4. Decision Explorer từ audit; rồi quyết định thư viện biểu đồ cho Phân tích.
5. ADR-0016 còn ở trạng thái Đề xuất: chốt nó cùng với việc ghi nhận `@xyflow/react`.
