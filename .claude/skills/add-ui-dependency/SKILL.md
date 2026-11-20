---
name: add-ui-dependency
description: Đánh giá và thêm (hoặc nâng phiên bản) một gói npm cho giao diện web ở web/ theo ADR-0016 — có thật cần không, có chạy được dưới CSP hiện tại không, chuỗi cung ứng, ghim phiên bản chính xác, npm audit, ảnh hưởng tới bundle. Dùng trước khi cài bất kỳ thư viện giao diện nào (biểu đồ, form, sơ đồ DAG, ngày giờ, bảng…) hoặc khi nâng React / Vite / TanStack Query / router.
argument-hint: <tên gói, kèm việc cần nó làm>
---

# Thêm phụ thuộc cho giao diện web

Gói: $ARGUMENTS

ADR-0016 quyết định 3 và 7: **thư viện bị giới hạn bởi CSP, không phải ngược lại**, và thêm một phụ thuộc lúc chạy (thứ đi vào bundle chạy trên origin giữ token của người dùng) là thay đổi phải được nêu rõ. Skill này là bước đánh giá đó. Kết luận "không thêm" là một kết quả hợp lệ.

## 1. Có cần không

- Việc cần làm là gì, ở màn hình nào. Hiện có 4 phụ thuộc lúc chạy (`react`, `react-dom`, `react-router`, `@tanstack/react-query`): thứ đã có hoặc API của trình duyệt (`Intl`, `<dialog>`, SVG, `crypto`) làm được không?
- Ước lượng phần tự viết: nếu là vài chục dòng trong `lib/` hoặc `components/` thì viết, không thêm gói.
- Phụ thuộc lúc build (lint, test, kiểu) rủi ro thấp hơn lúc chạy nhưng vẫn là mã chạy cạnh mã nguồn và sinh ra bundle: vẫn đi hết các bước dưới.

## 2. Chạy được dưới CSP không

CSP của `/ui/` (đọc bản thật ở `internal/webui/webui.go`): `default-src 'none'`, script / style chỉ từ chính origin, không `unsafe-inline`, không `unsafe-eval`, không CDN. Kiểm trong tài liệu và mã nguồn của gói (tra cứu, không dựa trí nhớ):

- Có chèn thẻ `<style>` lúc chạy (CSS-in-JS) hoặc cần `style-src 'unsafe-inline'` không?
- Có `eval` / `new Function` / dựng code từ chuỗi không (một số thư viện biểu đồ, template, validate schema)?
- Có tải font, ảnh, worker, wasm hay script từ origin khác, hoặc cần `worker-src` / `blob:` / `data:` không?
- Có đưa HTML từ dữ liệu vào DOM (`innerHTML`, render Markdown / HTML) không? Tooltip và nhãn của thư viện biểu đồ là chỗ hay gặp.
- Có tự gọi mạng (telemetry, kiểm tra bản quyền) không?

Một câu "có" mà không tắt được bằng cấu hình: **không dùng gói này**; tìm gói khác hoặc tự viết. Không đề xuất nới CSP.

## 3. Chuỗi cung ứng

- Giấy phép, ai duy trì, lần phát hành gần nhất, số phụ thuộc bắc cầu nó kéo theo (`npm view <gói> dependencies`; xem thay đổi của `package-lock.json`).
- Có script cài đặt (`preinstall` / `install` / `postinstall`) không: dự án cài bằng `npm ci --ignore-scripts`, gói cần script để hoạt động thì không dùng được.
- Phiên bản: tra phiên bản ổn định hiện hành ở nguồn chính thức; không lấy số từ trí nhớ.

## 4. Trình người dùng trước khi cài

Tóm tắt trong vài dòng: gói, phiên bản, lúc chạy hay lúc build, việc nó làm, kết quả từng mục ở bước 2, số phụ thuộc bắc cầu, phương án không thêm gói. **Chờ người dùng đồng ý với phụ thuộc lúc chạy** rồi mới cài.

## 5. Cài

```sh
cd web
npm install --save-exact --ignore-scripts <gói>@<phiên bản>        # lúc chạy
npm install --save-exact --save-dev --ignore-scripts <gói>@<phiên bản>   # lúc build
```

- `package.json` phải ghi phiên bản chính xác (không `^`, không `~`), như các dòng hiện có. `package-lock.json` vào cùng commit.
- Chỉ bọc thư viện ở **một chỗ** (`components/` cho thứ trình bày, `lib/` cho thứ thuần): feature dùng lớp bọc, không import thẳng gói, để thay gói sau này chạm một file.
- Dữ liệu đưa vào thư viện vẫn theo luật nội dung không tin cậy: chỉ văn bản, qua `visible()` khi là thứ để ra quyết định.

## 6. Kiểm

```sh
cd web && npm audit --audit-level=high       # CI chạy đúng lệnh này
make ui-lint && make ui-test && make ui-build
go test ./internal/webui/                    # bản build không có script / style nội tuyến
```

- So kích thước bundle trước và sau (`ls -l internal/webui/dist/assets`); nêu con số thật.
- Mở màn hình dùng thư viện trong trình duyệt với console đang mở: **không có vi phạm CSP nào**. Đây là bước duy nhất bắt được thư viện chèn style lúc chạy; chưa mở trình duyệt thì nói rõ là chưa kiểm.
- Nâng phiên bản major của React / router / TanStack Query / Vite: đọc ghi chú phát hành, chạy lại `app/approval-freshness.test.tsx` (mặc định của truy vấn là một phần của mô hình bảo mật màn hình duyệt).

## 7. Ghi lại

Nêu trong mô tả thay đổi: gói, phiên bản, lý do, kết quả kiểm CSP. Nếu gói thay đổi một quyết định của ADR-0016 (ví dụ cần một chỉ thị CSP mới) thì đó là ADR mới (`/new-adr`), không phải một dòng trong `package.json`. Gọi agent `ui-reviewer`; thêm `security-reviewer` cho phụ thuộc lúc chạy.
