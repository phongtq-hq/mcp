# ADR-0016: Giao diện web viết bằng React, có bước build; vẫn là file tĩnh nhúng vào `api`

Trạng thái: Đề xuất · 2026-11-19

## Bối cảnh

Bản đầu của giao diện web (`/ui/`) là JavaScript thuần trong `internal/webui/static`: không có bước build, không có phụ thuộc ngoài, nhúng vào binary `api` bằng `go:embed`. Lựa chọn đó chưa từng có ADR; nó chỉ được ghi ở chú thích đầu package và ở [security §4.1](../architecture/security.md). Lý do của nó là đúng: không có chuỗi cung ứng nào phải tin, và mã chạy trong trình duyệt là đúng mã đọc được trong repo.

Phần còn thiếu của giao diện là phần nặng tương tác: màn hình cấu hình (Agent và Agent Version, workflow, budget, MCP server, API key), dòng thời gian của execution theo luồng sự kiện SSE, sơ đồ DAG của `mode: planned`, và biểu đồ theo thời gian của Cost Dashboard ([roadmap Phase 5](../roadmap.md)). Viết các màn hình đó bằng cách dựng DOM tay, mỗi lần tự làm mới dựng lại cả trang, là viết lại một framework: bản JS thuần đã phải tự xử lý lượt dựng cũ đè lượt mới, giữ vị trí cuộn, và mất nội dung form đang gõ mỗi lần trang tự làm mới.

Quyết định này phải có trước khi viết các màn hình đó, vì nó đưa vào repo một ngôn ngữ thứ hai, một công cụ build thứ hai và một chuỗi cung ứng thứ hai (npm).

## Quyết định

1. Giao diện viết bằng **React + TypeScript**, build bằng **Vite**. Mã nguồn ở `web/` (gốc repo); bản build ghi vào `internal/webui/dist` và được nhúng vào binary `api` bằng `go:embed` như trước.
2. **Mô hình triển khai và bảo mật không đổi**: file tĩnh do `api` phục vụ ở `/ui/`, cùng origin với `/v1`; giao diện không có credential hay quyền nào của riêng nó; đăng nhập Authorization Code + PKCE qua public client `hq-web`; CSP giữ nguyên từng chỉ thị (`default-src 'none'`, không `unsafe-inline`, không `unsafe-eval`, không CDN). Không thêm process, không thêm CORS.
3. **Thư viện bị giới hạn bởi CSP, không phải ngược lại.** Một thư viện cần nới CSP (chèn thẻ `<style>` lúc chạy, dựng code từ chuỗi) thì không được dùng. Bản build không nhúng asset thành `data:` URI vào script hay style.
4. **Không đưa HTML thô vào DOM.** `dangerouslySetInnerHTML`, `innerHTML` và các lối tương đương bị cấm trong mã nguồn `web/src`, kiểm ở hai nơi: ESLint (theo cú pháp) và một test Go quét mã nguồn (chạy trong `make test`, không cần Node). Cả hai là bộ lọc theo cú pháp trên mã của dự án: chúng bắt lỗi vô ý, không phải rào chắn với người cố tình lách.
5. **Kiểu của API sinh từ `openapi.yaml`** (`web/src/api/schema.gen.ts`, bằng `openapi-typescript`), cùng nguồn với kiểu phía Go; `make generate-check` bắt lệch.
6. **Luật phụ thuộc trong `web/src`**, kiểm bằng ESLint: feature không import feature khác; chỉ `api/` và `auth/` được gọi mạng (`fetch`, `XMLHttpRequest`, `EventSource`, `WebSocket`, `sendBeacon`); `lib/`, `i18n/`, `components/` và `auth/` không phụ thuộc vào `api/` hay feature; `api/` không phụ thuộc vào feature. Import ra ngoài thư mục của mình phải dùng bí danh `@/` (import `../` bị cấm) và `import()` động chỉ dùng ở bảng route, để các luật trên không lách được bằng đường dẫn.
7. **Phụ thuộc npm**: ghim phiên bản chính xác trong `package.json`, cài bằng `npm ci --ignore-scripts` theo `package-lock.json` (mã của phụ thuộc không chạy lúc cài); CI chạy `npm audit` trên cả phụ thuộc lúc chạy lẫn lúc build, vì công cụ build sinh ra chính bundle chạy trong trình duyệt. Thêm một phụ thuộc lúc chạy (thứ đi vào bundle) là một thay đổi cần được nêu rõ khi review.
8. **Bản build không vào repo.** `internal/webui/dist` chỉ có `.gitkeep`; `make build` và Dockerfile (stage Node riêng) tạo ra nó. Binary build khi chưa có bản build vẫn biên dịch và chạy được: `/ui/` khi đó trả 503 kèm chỉ dẫn, để `go build` và `make test` không cần Node.
9. Công cụ Go bỏ qua `web/node_modules` bằng chỉ thị `ignore` trong `go.mod` (một số gói npm kèm file `.go`).

## Phương án đã cân nhắc

- **Giữ JavaScript thuần.** Không có chuỗi cung ứng và không có bước build. Không chọn vì phần giao diện còn lại là phần nặng tương tác, và chi phí tự viết lại quản lý trạng thái, form, luồng sự kiện và biểu đồ tăng theo từng màn hình.
- **htmx với HTML dựng ở server.** Chỉ cần Go, và phiên bằng cookie `HttpOnly` an toàn hơn trước XSS so với token trong trình duyệt. Không chọn vì mỗi màn hình trở thành một endpoint mới ở `api` (phải lặp lại authz, audit và ca truy cập chéo tenant), hoặc cần một process BFF thứ tư; cả hai đều lệch với thiết kế "giao diện là một client của Management API" ([overview §3](../architecture/overview.md)).
- **Giao diện ở repo riêng, triển khai riêng (CDN / nginx).** Phát hành giao diện độc lập với `api`. Không chọn vì khác origin buộc thêm CORS và nới `connect-src`, và kiểu của client tách khỏi `openapi.yaml`.
- **Commit bản build vào repo.** `go build` không bao giờ cần Node. Không chọn vì bundle đã nén không review được, và không có gì bảo đảm bundle trong repo khớp với mã nguồn bên cạnh nó.
- **Framework khác (Vue, Svelte, Solid).** Đều làm được. React được chọn vì có sẵn thư viện trưởng thành cho hai thứ sẽ cần: trình soạn đồ thị (DAG của kế hoạch, workflow) và biểu đồ.

## Hệ quả

- Viết được các màn hình còn thiếu mà không tự dựng lại framework. Lượt tự làm mới giữ nguyên trạng thái của form; việc bỏ kết quả của lượt tải cũ do thư viện lo.
- Kiểu của request / response được kiểm lúc biên dịch theo `openapi.yaml`; hai từ điển vi / en được buộc cùng bộ khoá bằng kiểu.
- **Chuỗi cung ứng npm là rủi ro mới.** Mã chạy trong trình duyệt, trên origin giữ token của người dùng, giờ gồm cả mã của bên thứ ba. Điều này gồm cả công cụ build (Vite, trình biên dịch, plugin): chúng không vào bundle nhưng sinh ra nó. Giảm thiểu: lockfile, `npm ci --ignore-scripts`, `npm audit` ở CI, số phụ thuộc lúc chạy giữ ở mức tối thiểu, và CSP chặn `fetch` / tải tài nguyên ra ngoài origin và IdP. Các lớp này giảm hậu quả, không loại bỏ rủi ro: mã độc trong bundle vẫn gọi được `/v1` cùng origin bằng phiên của người dùng (kể cả gửi quyết định duyệt thay họ), và CSP hiện tại không chặn việc điều hướng cả trang ra ngoài kèm dữ liệu trên URL. Chưa làm: ghim base image theo digest và GitHub Action theo SHA; chạy `npm audit` theo lịch (hiện chỉ khi push / PR); Trusted Types (`require-trusted-types-for 'script'`) để trình duyệt cấm sink HTML cho cả mã của phụ thuộc, cần thử với React trước.
- **Một bảo đảm yếu đi.** Trước đây test quét chính các file được phục vụ để tìm `innerHTML`. Bundle đã build không quét được (chính React dùng các API đó bên trong), nên giờ chỉ quét mã nguồn của dự án: mã của phụ thuộc không nằm trong phạm vi kiểm này.
- Mã chạy trong trình duyệt không còn là mã đọc được trong repo mà là kết quả build của nó.
- `make build`, `make generate`, CI và Dockerfile cần Node. `go build ./...`, `make build-go` và `make test` thì không; các target `run-*` của process không phục vụ giao diện (gateway, worker, MCP server) dùng `build-go`, nên công cụ build của giao diện không chạy trên đường khởi động process giữ khoá.
- Token vẫn nằm trong `sessionStorage` như trước ([security §4.1](../architecture/security.md)); ADR này không đổi điều đó.
- Realm Keycloak cục bộ đăng ký thêm cho client `hq-web` redirect URI và post-logout URI `http://localhost:5173/ui/` cùng web origin `http://localhost:5173` (CORS phía IdP), cho máy chủ phát triển của Vite. 5173 là cổng mặc định của mọi dự án Vite: thứ gì chạy ở đó trên máy phát triển khi còn phiên SSO đều lấy được token của `hq-web`. Realm của staging / production không được có các mục đó; hiện chỉ có tài liệu nói vậy, chưa có kiểm tra tự động. Máy chủ phát triển của Vite không mang CSP: một thư viện vi phạm CSP chỉ lộ ra khi chạy bản build qua `api`.
- Chưa có test đầu-cuối trong trình duyệt ở CI. Bản chuyển đổi đã được chạy tay trong Chrome dưới đúng CSP với IdP và API giả; việc đó chưa được tự động hoá.

## Điều kiện xem xét lại

- Một phụ thuộc lúc chạy của giao diện bị phát hiện mang mã độc hoặc có lỗ hổng không vá được trong thời gian chấp nhận được.
- Một thư viện cần cho màn hình trong roadmap (biểu đồ, trình soạn đồ thị) không chạy được dưới CSP hiện tại: khi đó quyết định là đổi thư viện hay nới CSP phải được ghi lại, không làm lặng lẽ.
- Giao diện cần phát hành độc lập với `api` (nhiều bản phát hành giao diện giữa hai bản của `api`), hoặc cần phục vụ từ CDN.
- Cần phiên bằng cookie phía server thay cho token trong trình duyệt: khi đó phương án BFF được mở lại.
