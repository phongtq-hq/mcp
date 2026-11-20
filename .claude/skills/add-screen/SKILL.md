---
name: add-screen
description: Thêm hoặc mở rộng một màn hình của giao diện web (React + TypeScript ở web/) theo đúng khuôn của dự án — kiểm endpoint trong openapi.yaml trước, feature trong thư mục riêng, gọi API qua client với kiểu sinh sẵn, từ điển vi / en, trạng thái tải / rỗng / lỗi, test, rồi lint + build. Dùng khi thêm màn hình mới (tạo / sửa Agent, workflow, budget, MCP server, API key, biểu đồ chi phí…) hoặc thêm một tab / hành động vào màn hình đã có.
argument-hint: <tên màn hình hoặc việc người dùng cần làm trên màn hình>
---

# Thêm màn hình giao diện web

Màn hình: $ARGUMENTS

Giao diện là một client của Management API (`/v1`), không có quyền hay endpoint riêng. Đọc `.claude/rules/web-ui.md` trước; nó là luật, skill này là trình tự.

## 1. API trước, màn hình sau

- Liệt kê từng thứ màn hình cần đọc và từng hành động nó gửi đi, rồi tìm operation tương ứng trong `.docs/api/openapi.yaml`.
- Thiếu endpoint, thiếu trường, hoặc thiếu bộ lọc / phân trang: **dừng phần giao diện**, nói rõ thiếu gì, và làm endpoint trước bằng `/add-endpoint` (spec-first, có ca truy cập chéo tenant). Không lấp chỗ thiếu bằng cách tải hết rồi lọc ở trình duyệt, không dựng dữ liệu giả.
- Ghi lại quyền mà mỗi operation đòi (`resource:action`): màn hình phải xử lý được 403 / 404 cho từng lời gọi, không đoán quyền từ role.
- Kiểm `CLAUDE.md` (mục giao diện, "Chưa có") và roadmap: màn hình này có nằm trong phạm vi không.

## 2. Thiết kế ngắn trước khi viết

Nêu cho người dùng, vài dòng, và chờ nếu có điểm cần họ quyết định:

- Đường dẫn route và vị trí trong điều hướng (trang cấp cao hay tab của một dự án).
- Thư mục feature: thêm vào `features/<đã có>/` hay tạo `features/<mới>/`.
- Các truy vấn (khoá, endpoint, có tự làm mới không và khi nào dừng) và các hành động (endpoint, xác nhận, truy vấn nào bị làm mới sau đó).
- Thứ gì trên màn hình là **nội dung không tin cậy** (do model, tool hoặc tenant sinh ra) và thứ gì người dùng **dựa vào để quyết định**.
- Có cần thư viện mới không. Nếu có: dừng và chạy `/add-ui-dependency` trước.

Màn hình có form sửa cấu hình (Agent Version, workflow, budget): nói rõ cách xử lý xung đột (`If-Match` / 412) và validate (lỗi 422 của API hiện ở đâu). Validate ở trình duyệt chỉ để tiện; API mới là nơi quyết định.

## 3. Hiện thực

Theo thứ tự:

1. **Kiểu.** `type X = Schemas['X']` từ `@/api/client`. Spec vừa đổi thì `make generate` trước. Không khai báo tay kiểu trùng spec.
2. **Từ điển.** Thêm khoá vào `web/src/i18n/vi.ts` (chuẩn) và `en.ts` cùng lúc, cùng tham số `{tên}`. Không có chuỗi hiển thị cứng trong component.
3. **Feature.** `web/src/features/<tên>/<tên>-page.tsx` (hoặc `-tab.tsx`), `export default` một function component.
   - Đọc: `useQuery({ queryKey: [...], queryFn: () => get<…>(…) })`, bọc phần hiển thị bằng `<Async query={…}>`.
   - Bảng: `DataTable` với `emptyText`. Khung: `Page` / `PageHead`. Trạng thái: `StatusBadge`, `RiskBadge`, `Notice`, `ErrorBox`.
   - Gửi: `api('POST' | …, path, body)`; khoá nút trong lúc gửi; hành động có hậu quả cần xác nhận; hiện kết quả thật của API; rồi `invalidateQueries` đúng khoá.
   - Tham số URL kiểm bằng `isUuid` trước khi ghép vào đường dẫn API.
   - Nội dung không tin cậy chỉ là text node; thứ dùng để ra quyết định qua `visible()` / `json()`.
   - Tự làm mới: `refetchInterval` là hàm, trả `false` khi tài nguyên đã ở trạng thái kết thúc.
4. **Phần dùng chung.** Thứ cần ở hai feature đưa xuống `components/` (trình bày) hoặc `lib/` (thuần, không mạng). Khoá truy vấn dùng ở nhiều nơi đưa vào `api/keys.ts`.
5. **Route.** Thêm `lazy(() => import('@/features/…'))` và `<Route>` vào `web/src/app/routes.tsx`; thêm mục điều hướng ở `app/shell.tsx` (hoặc tab ở `features/projects/project-layout.tsx`).
6. **Kiểu trình bày.** Class trong `web/src/styles/app.css`; không style nội tuyến ngoài giá trị tính lúc chạy; không thêm font / ảnh / script từ origin khác.

Chạm `features/approvals/**`, `lib/approval.ts`, `lib/visible.ts` hoặc `app/query-client.ts`: đọc `app/approval-freshness.test.tsx` trước và giữ nguyên ba bất biến của màn hình duyệt.

## 4. Test

- Vitest + Testing Library, file `*.test.tsx` cạnh màn hình; mock `@/api/client` (xem `features/approvals/approval-detail-page.test.tsx`).
- Tối thiểu: hiển thị dữ liệu trả về; trạng thái rỗng; API trả lỗi (403 và 404) hiện thông báo thay vì trang trắng; mỗi hành động gửi đúng method + đường dẫn + thân, và không gửi khi người dùng huỷ xác nhận.
- Hàm thuần mới trong `lib/` có test riêng.
- Không sửa test có sẵn cho qua; nếu test sai, nói rõ vì sao.

## 5. Hoàn tất

Chạy và báo kết quả thật:

```sh
make ui-lint && make ui-test && make ui-build
make generate-check          # nếu có đổi openapi.yaml
go test ./internal/webui/    # kiểm bản build (không script / style nội tuyến) và quét web/src
```

Xem màn hình thật: `make run-api` + `make ui-dev` (http://localhost:5173/ui/), hoặc cụm đang chạy. Chưa có test đầu-cuối trong trình duyệt: nói rõ phần nào bạn đã tự bấm thử và phần nào chỉ có test đơn vị.

Rồi:

- Cập nhật danh sách màn hình và mục "Chưa có" của giao diện trong `.claude/CLAUDE.md`, và `roadmap.md` nếu màn hình đóng một hạng mục.
- Gọi agent `ui-reviewer` trên các file đã sửa; thêm `security-reviewer` nếu màn hình chạm đăng nhập, token, approval, secret (ô nhập khoá) hoặc hiển thị nội dung do model sinh để người dùng quyết định.
