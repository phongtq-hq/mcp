# Checklist tiến độ: giao diện Agent Control Plane

Trạng thái: đang làm · cập nhật 2026-11-20 · nhánh `ui/control-plane` (chưa commit)

Liên quan: [kế hoạch](ui-refactor-plan.md), [audit](frontend-audit.md), [tiến độ theo lát của màn hình cấu hình](ui-progress.md), [ADR-0016](adr/0016-react-web-console-with-build-step.md).

Quy ước: `[x]` xong và có test; `[~]` xong một phần (ghi rõ phần thiếu); `[ ]` chưa làm. "Chờ API" nghĩa là `/v1` chưa có dữ liệu đó: phải thêm endpoint trước (`/add-endpoint`), giao diện không dựng dữ liệu giả.

Tổng quát: Phase 1 và 3 xong, Phase 2 xong phần đọc, Phase 4–6 chưa bắt đầu, Phase 7 một phần. **Chưa màn hình mới nào được mở trong trình duyệt thật.**

## Phase 1: Nền

- [x] Audit mã nguồn hiện có (`frontend-audit.md`)
- [x] Kế hoạch chuyển đổi (`ui-refactor-plan.md`)
- [x] Design token: tối là mặc định (navy + emerald), sáng qua `data-theme="light"`
- [x] Khung trang: thanh bên theo nhóm, có biểu tượng, thu gọn được; ngăn trượt ở màn hình hẹp
- [x] Thanh trên: breadcrumb, chuông approval đang chờ, đổi sáng / tối, menu tài khoản
- [x] Bảng lệnh Ctrl / Cmd + K
- [~] Tìm kiếm chung: chỉ tìm màn hình và dự án. Agent, lần chạy, trace, audit: chờ API tìm kiếm
- [x] Inspector dùng chung (ngăn chi tiết bên phải)
- [x] Bộ biểu tượng tự vẽ (`components/icon.tsx`)
- [x] Điều hướng một nguồn (`app/navigation.ts`), chỉ liệt kê màn hình có thật
- [ ] Environment switcher: môi trường là thuộc tính của dự án, không có công tắc toàn cục
- [ ] Trung tâm thông báo: mới có chuông đếm approval
- [ ] Font Inter / Geist nhúng kèm: hiện dùng phông của máy

## Phase 2: Quản lý Agent

- [x] Danh mục Agent `/agents`: bảng, tìm, lọc trạng thái, sắp xếp, phân trang keyset (đều phía máy chủ)
- [x] Inspector của Agent: tổng quan, cấu hình version, quyền gọi tool, lần chạy gần đây
- [ ] Liệt kê xuyên dự án: chờ API (hiện chọn từng dự án)
- [ ] Dạng thẻ, saved views, thao tác hàng loạt
- [ ] Lọc theo rủi ro, owner, team: chờ API
- [ ] Trạng thái Running / Idle / Failed / Maintenance: chờ API (hiện có vòng đời `draft / active / disabled / archived`)
- [ ] Trang chi tiết Agent nhiều tab (Activity, Executions, Tools, Permissions, Versions, Analytics)
- [ ] Tạo / sửa Agent, version mới, publish / rollback (lát 4 của `ui-progress.md`)

## Phase 3: Bản đồ Agent

- [x] Tích hợp `@xyflow/react` 12.12.1, chỉ import ở `components/graph.tsx` (ESLint kiểm)
- [x] Mô hình đồ thị có kiểu; tách ảnh chụp API / đồ thị nghiệp vụ / lớp nhìn
- [x] Adapter thực thể → đồ thị (`adapter.ts`), nguồn dữ liệu thật (`source.ts`)
- [x] Node riêng: Control Plane, Agent, MCP server, tool, hệ thống đích
- [ ] Node Policy: chờ API (policy là code ở Gateway, không phải dữ liệu)
- [x] Cạnh riêng: dùng được, cần duyệt, bị từ chối, không dùng được
- [x] Cạnh có chấm chạy chỉ cho lần chạy đang theo dõi
- [x] Bố cục tự động theo tầng (`lib/graph-layout.ts`; không dùng Dagre / ELK)
- [x] Thu phóng, kéo, vừa khung, minimap
- [x] Chọn node, chọn nhiều, kéo node (kể cả kéo cả nhóm)
- [x] Tìm node và đưa vào giữa khung nhìn
- [x] Bộ lọc: dự án, môi trường, trạng thái, rủi ro, loại node, ẩn node không quan hệ
- [x] Mở / thu tool theo server; đặt lại bố cục; hiện tất cả
- [x] Thanh công cụ nổi, chú giải
- [x] Inspector cho Agent, server, tool, cạnh, Control Plane, hệ thống đích
- [x] Bấm một lần chạy đang diễn ra: đánh dấu Agent, tool và đường nối của nó
- [x] Cảnh báo khi bản đồ bị cắt hoặc có phần không đọc được
- [x] Fixture tổng hợp cho test; bố cục 200 node có test
- [~] Số liệu trên node và cạnh: có số đếm từ cấu hình. Số lần gọi, độ trễ, lỗi gần đây: chờ API
- [~] Lịch sử gọi của tool: lấy từ audit, chỉ `org_admin` đọc được
- [ ] Lọc theo loại tool, theo quyết định policy
- [ ] Endpoint topology ở `/v1` (hiện ghép từ nhiều lời gọi, trần 200 Agent)
- [ ] Bảng "đang chạy" gồm cả `waiting_approval` / `paused` / `queued`
- [ ] Dùng luồng SSE thay cho đọc định kỳ khi theo dõi một lần chạy

## Phase 4: Quan sát

- [ ] Live Monitor và luồng hoạt động: chờ API (chỉ có SSE theo từng execution)
- [ ] Lớp sự kiện realtime dùng chung
- [~] Execution Explorer: danh sách theo dự án và chi tiết từng bước đã có từ trước. Danh sách toàn organization: chờ API
- [ ] Cột thời lượng, token, tóm tắt lỗi ở danh sách lần chạy
- [ ] Dòng thời gian theo sự kiện, pause / resume, sơ đồ DAG (lát 7 của `ui-progress.md`)
- [ ] Trace waterfall, span inspector: chờ API

## Phase 5: Bảo mật và quản trị

- [x] Phê duyệt: danh sách, chi tiết, duyệt / từ chối (đã có từ trước, không đổi)
- [x] Nhật ký audit (đã có từ trước)
- [ ] Bộ lọc đầy đủ cho audit (lát 9)
- [ ] Decision Explorer: làm được từ audit (`outcome=denied`)
- [ ] Policies, Permissions & Access: chờ API
- [ ] Secrets & Credentials: chờ API
- [ ] Lịch sử approval (đã duyệt / đã từ chối)

## Phase 6: Phân tích

- [~] Chi phí: màn hình tổng hợp đã có từ trước; chưa có biểu đồ
- [ ] Quyết định thư viện biểu đồ (`/add-ui-dependency`; ECharts chưa được đánh giá)
- [ ] Usage và token theo thời gian, theo Agent, theo model (lát 10)
- [ ] Command Center: thẻ KPI có liên kết lọc, biểu đồ, hoạt động gần đây
- [ ] Performance (P50 / P95 / P99, throughput), Reliability: chờ API

## Phase 7: Tích hợp và chất lượng

- [x] Giữ nguyên tích hợp API và xác thực hiện có; không đổi hợp đồng API
- [x] Giữ nguyên ba bất biến của màn hình duyệt và mặc định của `QueryClient`
- [x] `tsc --noEmit`, ESLint sạch
- [x] Vitest: 221 test / 32 file qua
- [x] `vite build`, `go test ./internal/webui/` qua
- [x] `npm audit --audit-level=high`: 0 lỗ hổng
- [x] Review bằng `ui-reviewer` và `security-reviewer`: không có mục chặn
- [x] Cập nhật `.claude/CLAUDE.md`, `.claude/rules/web-ui.md`, `ui-progress.md`
- [ ] **Mở bản build trong trình duyệt dưới CSP thật** (bố cục, canvas, giao diện sáng, vi phạm CSP lúc chạy)
- [ ] Kiểm bố cục ở cỡ tablet và màn hình hẹp
- [ ] Đo độ tương phản của bảng màu mới
- [ ] Chạy với organization thật có nhiều Agent
- [ ] Test đầu-cuối trong trình duyệt (lát 11)
- [ ] Commit; chốt ADR-0016 kèm ghi nhận `@xyflow/react`

## Mục review còn tồn

- [ ] Chuỗi cứng `Ctrl K` / `Esc`; chuỗi tiếng Anh của thư viện đồ thị cho trình đọc màn hình
- [ ] Menu tài khoản không đóng khi bấm ra ngoài hoặc bấm Esc
- [ ] Bảng lệnh: xác nhận tiêu điểm trở về chỗ cũ khi đóng (cần trình duyệt)
- [ ] Bo góc còn viết số trực tiếp ở vài chỗ thay vì token
- [ ] Ẩn loại node "server" thì tool đang thu gọn không mở được; chọn node bị lọc từ ô tìm thì bộ lọc bị đặt lại lặng lẽ

## Tiêu chí nghiệm thu của bản yêu cầu

- [~] 1. Hệ thiết kế nhất quán: token và khung trang xong; chưa kiểm bằng mắt
- [x] 2. Điều hướng chạy trên mọi màn hình đã có
- [~] 3. Danh mục Agent lọc và xem chi tiết được: theo từng dự án, chưa lọc theo rủi ro
- [x] 4. Bản đồ Agent thao tác được
- [~] 5. Node hiện đúng trạng thái và số liệu: theo dữ liệu API có; thiếu số liệu vận hành
- [x] 6. Chọn node mở inspector
- [~] 7. Luồng điều tra lần chạy: chi tiết từng bước có từ trước; chưa có trace / span
- [~] 8. Quyết định bảo mật dễ hiểu: có trên cạnh của bản đồ (theo cấu hình); chưa có Decision Explorer
- [ ] 9. Trang phân tích có bộ lọc và drill-down nhất quán
- [x] 10. Mọi nút có hành vi thật
- [x] 11. Tích hợp backend hiện có được giữ
- [x] 12. Không đổi hợp đồng API
- [x] 13. Không lỗi TypeScript
- [ ] 14. Không lỗi lúc chạy: chưa chạy trong trình duyệt
- [~] 15. Không route nào hỏng: test route và khung trang qua; chưa bấm thử
- [ ] 16. Dùng được ở desktop và tablet: chưa kiểm
- [x] 17. Trạng thái tải, rỗng, lỗi ở các màn hình mới
- [~] 18. Hiệu năng với dữ liệu lớn: bố cục 200 node dưới 500 ms trong test; chưa đo trong trình duyệt
