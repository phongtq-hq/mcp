---
paths:
  - "web/**"
  - "internal/webui/**"
---

# Giao diện web (React + TypeScript)

Nguồn: [ADR-0016](../../.docs/adr/0016-react-web-console-with-build-step.md) · `.docs/architecture/security.md` §4.1 (đoạn "Giao diện web") · `web/eslint.config.js` (luật được máy kiểm).

Giao diện là **một client của Management API**: file tĩnh do `api` phục vụ ở gốc của origin (`/`, `/config.json`, `/assets/*`), không có credential, quyền hay endpoint riêng. Thứ gì cần dữ liệu mới thì thêm vào `/v1` trước (`/add-endpoint`), không thêm đường tắt cho giao diện.

## Tầng và phụ thuộc (`web/src`)

| Thư mục | Chứa | Được import |
|---|---|---|
| `lib/`, `i18n/` | Tầng thấp nhất, không gọi mạng: định dạng, `visible()`, quy tắc hiển thị approval, từ điển | không gì của dự án |
| `components/` | Thành phần trình bày dùng chung (`Async`, `DataTable`, `Page`, `ErrorBox`, `StatusBadge`…) | `lib/`, `i18n/` |
| `auth/` | Đăng nhập OIDC (PKCE), phiên | `lib/`, `i18n/` |
| `api/` | Chỗ duy nhất gọi `/v1`: `client.ts`, khoá truy vấn dùng chung, `schema.gen.ts` | `auth/`, `lib/`, `i18n/` |
| `features/<màn hình>/` | Một màn hình một thư mục | mọi tầng trên; **không** feature khác, không `app/` |
| `app/` | Bảng route (nơi duy nhất biết mọi feature), khung trang, đăng nhập | tất cả |

- Import ra ngoài thư mục của mình dùng bí danh `@/`; `../` bị cấm. `import()` động chỉ ở `app/routes.tsx`.
- Hai feature cần chung một thứ: đưa nó xuống `components/` hoặc `lib/`, không import chéo.
- **Không nới ESLint để code qua được.** Luật cản một thay đổi hợp lý là tín hiệu cần sửa ADR-0016, không phải thêm `eslint-disable`.

## Gọi API

- Chỉ qua `src/api/`: `client.ts` (`get`, `api`, `getWithETag`, `soft`), `paging.ts` (`pages`, `rows` cho `useInfiniteQuery`) và `events.ts` (luồng sự kiện của execution, đọc bằng `fetch` vì `EventSource` không gửi được `Authorization`). Không `fetch`, `XMLHttpRequest`, `EventSource`, `WebSocket`, `sendBeacon` ở nơi khác.
- Kiểu request / response lấy từ `Schemas[...]` (sinh từ `openapi.yaml`). Không khai báo lại bằng tay, không sửa `schema.gen.ts`; đổi API thì sửa `openapi.yaml` rồi `make generate`.
- `POST` đã tự mang `Idempotency-Key`. Màn hình sửa đọc tài nguyên bằng `getWithETag` và gửi `PATCH` kèm `{ ifMatch: etag }`; thiếu `ifMatch` là ghi vô điều kiện (máy chủ coi thiếu header là không kiểm), nên chỉ bỏ nó ở endpoint không có ETag. 412 = bản đang sửa đã đổi ở nơi khác (`isStale`, thông báo riêng); 409 là xung đột nghiệp vụ và hiện nguyên thông báo của API.
- `settings` của organization / project được máy chủ THAY cả object mỗi lần ghi: form sửa một khoá phải gửi lại các khoá khác từ bản vừa đọc.
- Lỗi theo trường (`errors[]` của Problem) được máy chủ gửi ở những context đã chuyển sang `apperr.InvalidField` (xem `CLAUDE.md`); context chưa chuyển chỉ có `detail`, hiện ở hộp lỗi chung của form. Form luôn hiện cả hai: lỗi dưới ô khi có, và `detail`. Thêm màn hình form cho một context chưa chuyển thì chuyển các lỗi validate của nó cùng lát.
- `POST` gửi lại đúng request (cùng đường dẫn, cùng thân) sau lỗi không rõ kết quả dùng lại `Idempotency-Key` cũ (`api/client.ts`); không tự sinh khoá ở màn hình.
- Dữ liệu máy chủ đi qua TanStack Query. Khoá truy vấn là mảng đi từ tài nguyên tới chi tiết (`['project', id, 'workflows']`); khoá dùng ở nhiều nơi đặt ở `api/keys.ts`. Sau một thay đổi, `invalidateQueries` đúng khoá bị ảnh hưởng.
- Không đổi mặc định ở `app/query-client.ts` (không thử lại, không tải lại khi chọn cửa sổ hay có mạng): chúng là một phần của mô hình bảo mật màn hình duyệt. Màn hình cần tự làm mới thì đặt `refetchInterval` trên truy vấn của chính nó.
- Danh sách dùng phân trang keyset của API (`limit`, `cursor`, `next_cursor`); không tải hết rồi cắt ở trình duyệt.

## Quyền

- Giao diện **không quyết định quyền**. Không ẩn / hiện theo tên role, không suy ra quyền từ token. API trả 403 / 404 thì hiện thông báo tương ứng (`ErrorBox`); lời gọi phụ có thể thiếu quyền bọc bằng `soft()`.
- Token chỉ ở `auth/` (trong `sessionStorage`). Không đọc token ở feature, không đưa token hay dữ liệu tenant vào `localStorage`, URL hay log.

## Nội dung không tin cậy

Mọi thứ đến từ API có thể do model hoặc hệ thống ngoài sinh ra (tham số tool, kết quả tool, mô tả tool, tên do tenant đặt).

- Chỉ vào trang dưới dạng text node của React. `dangerouslySetInnerHTML`, `innerHTML` và các lối tương đương bị cấm (ESLint + test Go `TestSourceHasNoRawHTMLSinks`).
- Không dựng `href` / `src` từ dữ liệu API mà chưa kiểm scheme; liên kết ra ngoài có `rel="noreferrer"`.
- Không render Markdown / HTML từ dữ liệu; không thêm thư viện làm việc đó.
- Nội dung mà người dùng **dựa vào để quyết định** (tham số tool chờ duyệt, thông điệp của cổng duyệt) hiển thị qua `visible()` / `json()` của `lib/visible.ts` để lộ ký tự ẩn.

## Màn hình duyệt (bất biến)

- Không cho duyệt khi thiếu nội dung (`incomplete()` ở `lib/approval.ts`).
- Luôn tải trạng thái hiện tại trước khi hiện gì; không hiện approval cũ trong lúc tải approval khác.
- Trang chi tiết (nơi ra quyết định) không tự làm mới: nội dung không được đổi dưới con trỏ của người duyệt. Danh sách chờ duyệt thì có tự làm mới; nó không có nút quyết định.
- Sửa `features/approvals/**`, `lib/approval.ts`, `lib/visible.ts` hay `app/query-client.ts` thì đọc và chạy `app/approval-freshness.test.tsx` trước; thêm ca test cho hành vi mới.

## CSP và phụ thuộc

- CSP của giao diện không đổi (`default-src 'none'`, không inline, không `eval`, không CDN). `style={{…}}` chỉ cho giá trị tính lúc chạy (ví dụ độ rộng một thanh phần trăm; React đặt nó qua CSSOM nên CSP cho phép), còn lại dùng class trong `styles/app.css`; không thẻ `<style>` / `<script>` nội tuyến; không nhúng asset thành `data:` URI vào JS / CSS. Thư viện cần nới CSP thì không dùng.
- Thêm phụ thuộc lúc chạy (thứ vào bundle) là thay đổi phải nêu rõ: dùng `/add-ui-dependency`. Phiên bản ghim chính xác, cài bằng `npm ci --ignore-scripts`.
- Bản build (`internal/webui/dist`) không vào repo.

## Token và chuyển động

- Màu, khoảng cách, cỡ chữ, bo góc, đổ bóng, thời lượng và easing là biến CSS ở `styles/tokens.css`. Không viết số thời lượng hay easing trong component hay ở `app.css`; phía JavaScript đọc cùng token qua `lib/motion.ts` (`durationMs`, `reducedMotion`).
- Mỗi hiệu ứng trả lời một câu hỏi của người dùng, ghi ở chú thích của khối CSS đó. Không trả lời được thì không thêm.
- Chỉ đổi `transform`, `opacity` và màu. Không có hiệu ứng nào chặn thao tác; hiệu ứng dài (phát lại luồng) dừng được.
- `prefers-reduced-motion` đưa mọi thời lượng về gần 0 ngay ở token; hiệu ứng lặp (vòng quay, chấm chạy, ánh trượt của khung chờ) tắt hẳn.
- Trang chi tiết phê duyệt nằm trong `.no-motion`: nội dung mà người duyệt dựa vào không có chuyển động.
- Trạng thái hiện bằng hình (`StatusIcon`) và chữ, không chỉ bằng màu. Tải lần đầu dùng `Skeleton` (qua `Async`, `loading="detail"` cho trang chi tiết), không dùng vòng xoay cho nội dung.
- Giao diện tối là mặc định; giao diện sáng là bộ token dưới `:root[data-theme="light"]` (`lib/theme.ts`). Component dùng token theo nghĩa (`--surface`, `--border`, `--ok`…), không viết mã màu: thêm màu là thêm token ở cả hai bộ.
- Không thêm thư viện animation: CSS (`@starting-style`, keyframes) và `lib/use-tween.ts` đủ cho các hiệu ứng hiện có (đã đánh giá `motion` theo `/add-ui-dependency`).
- Sơ đồ luồng (`lib/flow.ts`, `components/flow-diagram.tsx`) chỉ vẽ từ bước thật của lần chạy; không dựng dữ liệu minh hoạ.

## i18n

- Không có chuỗi hiển thị cứng trong component: mọi chuỗi qua `t('khoá')`. `vi.ts` là chuẩn; thêm khoá thì thêm ở `en.ts` cùng lúc, cùng bộ tham số `{tên}` (kiểu và `i18n.test.ts` buộc).
- Giá trị do API trả về mà giao diện có thể chưa biết (trạng thái, loại trigger) dùng `tOr('tiền_tố_' + giá_trị, giá_trị)`.
- Ngày giờ, số, tiền qua `lib/format.ts`. Tiền là chuỗi decimal + `currency` từ API: không `parseFloat` rồi cộng ở trình duyệt.

## Component

- Trang là `export default` (bảng route nạp bằng `lazy`). Function component + hook; `react-hooks/exhaustive-deps` là lỗi, không tắt.
- Trạng thái tải / lỗi qua `<Async query={…}>`; bảng qua `DataTable`; khung trang qua `Page`. Không tự viết lại spinner / hộp lỗi.
- Form: `Form` + `TextField` / `SelectField` / `CheckField` / `JsonField` (`components/form.tsx`) với `useSubmit` (`components/use-submit.ts`); xác nhận bằng `useConfirm` (`components/confirm.tsx`), không `window.confirm` trong code mới; danh sách dài bằng `LoadMore`. Sau khi lưu, form dựng lại từ bản mới đọc (đặt `key` theo ETag), không giữ giá trị của bản cũ.
- Tham số trên URL (`:projectId`…) kiểm dạng (`isUuid`) trước khi ghép vào đường dẫn API.
- Hành động có hậu quả (chạy, huỷ, duyệt) có bước xác nhận, khoá nút trong lúc gửi, và hiện kết quả thật của API (kể cả lỗi).
- Nút là `<button>`, liên kết là `<Link>` / `<a>`; ô nhập có nhãn; không dùng `div` có `onClick` cho thứ bấm được.
- Lựa chọn hiển thị (bộ lọc, khoảng thời gian) giữ qua `useStored` (`lib/use-stored.ts`).
- Mục điều hướng thêm ở `app/navigation.ts` (một nguồn cho thanh bên, breadcrumb, bảng lệnh), và chỉ khi màn hình đã chạy trên API thật: không có mục "sắp có".
- Chi tiết của thứ đang chọn mà không rời màn hình: `Inspector` (`components/inspector.tsx`), không tự viết ngăn bên khác. Biểu tượng lấy từ `components/icon.tsx`.
- Đồ thị: `@xyflow/react` chỉ được import ở `components/graph.tsx`. Dữ liệu vẽ đi qua ba lớp tách nhau như `features/agent-map` (ảnh chụp từ API → đồ thị nghiệp vụ → lớp nhìn + toạ độ); component vẽ không gọi API. Fixture tổng hợp chỉ được import từ file test.

## Test và lệnh

- Vitest + Testing Library (`*.test.ts(x)` cạnh file). Test hành vi người dùng thấy; mock ở biên `@/api/client`, không mock `fetch`. Test của feature tự tạo `QueryClient` (không import `@/app/...`). Lấy ô nhập bằng tên chính xác của nhãn (`getByLabelText('Tên')`). jsdom chặn submit khi ô `required` còn trống, như trình duyệt thật.
- `make ui-lint` (kiểu + ESLint), `make ui-test`, `make ui-build`; sau khi đổi `openapi.yaml`: `make generate`. Test Go của `internal/webui` kiểm bản build và quét `web/src`.
- Chưa có test đầu-cuối trong trình duyệt: nói rõ phần nào chỉ được kiểm bằng test đơn vị.
