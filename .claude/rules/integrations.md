---
paths:
  - "internal/integration/**"
---

# Managed MCP server (integration)

Nguồn: `.docs/architecture/mcp-platform.md` §6.3, §7 · roadmap Phase 4.

Mỗi integration là một MCP server độc lập trong thư mục riêng, dùng Streamable HTTP.

## Thiết kế tool

- Nhỏ, một việc. Schema chặt: `additionalProperties: false`, enum thay cho chuỗi tự do.
- **Tách tool đọc và tool ghi.**
- **Không có tool "chạy lệnh bất kỳ":** không `exec`, không shell tự do, không `run_sql`. SSH chỉ qua tập lệnh định sẵn. Truy vấn DB chỉ đọc, trên replica, có `statement_timeout` và giới hạn dòng.
- Tool ghi nhận `idempotency_key` và phải tôn trọng nó (managed server bắt buộc hỗ trợ).
- Tool ghi hỗ trợ `dry_run` khi hệ thống đích cho phép, để approval hiển thị được tác động.
- Kết quả có cấu trúc, phân trang, giới hạn kích thước.
- Tên trường không chứa `token`, `secret`, `password`, `key`, `credential`: Gateway che giá trị của các trường đó trong kết quả. Con trỏ phân trang tên `cursor` / `next_cursor`.
- Tool nhận ngôn ngữ truy vấn (PromQL, LogQL…): trần ở tham số không đủ; kiểm cả biểu thức (cửa sổ thời gian, `offset`, subquery) và ghi giới hạn bắt buộc phía hệ thống đích vào runbook.
- Không trả văn bản tự do do hệ thống đích hoặc ứng dụng sinh ra (termination message, thông báo lỗi 5xx) khi chưa xác định nó có thể chép credential hay command line; che bằng regex là lớp giảm thiểu, không phải bảo đảm.
- Mô tả tool trung thực và ổn định: nó đi vào prompt, và thay đổi nó kích hoạt duyệt lại.

## Credential và tenant

- Credential hệ thống đích do server lấy theo ngữ cảnh tenant; **không nhận qua tham số tool**, không trả về trong kết quả.
- Ngữ cảnh tenant (org, project, initiator, execution) đến qua header đã ký từ Gateway; xác minh chữ ký trước khi dùng. Dùng `internal/integration/serve` (`serve.Handler`) cho việc này; không chép đoạn kiểm ngữ cảnh và không phân quyền bằng cách tự đọc body của request MCP (trình đọc JSON của bạn và của SDK có thể hiểu một body theo hai cách).
- **Mặc định mọi tool chỉ chạy trong execution** (ADR-0018): `serve.Handler(slug, verifier, orgID, newServer, serve.Direct(DirectTools...))`. Chỉ khai `Direct` cho tool trả siêu dữ liệu ít nhạy cảm (danh sách, trạng thái, số đo); log, văn bản tự do do bên ngoài viết (issue, event, thông điệp), dữ liệu kinh doanh (giá vốn, khách hàng) thì không. Trong bộ test hợp đồng khai `Direct: true` cho đúng các tool đó: bộ test kiểm cả hai chiều. Gọi tool chỉ-execution trong test của integration bằng `contract.ExecutionTransport` (ngữ cảnh như Gateway ký trên tuyến execution), và dựng handler bằng `contract.TestSigner().Verifier()`.
- Server chỉ giữ khoá CÔNG KHAI: đọc bằng `serve.VerifierFromEnv` (`HQ_SERVICE_VERIFY_KEY`; từ chối khởi động nếu có `HQ_SERVICE_SIGNING_KEY`). Trong compose production, mỗi integration một mạng `mcp-<tên>` chỉ có nó, `gateway`, `hqadmin` (không nằm trên `backend`); cần gọi ra ngoài thì thêm mạng egress riêng.
- Tool trả dữ liệu nhạy cảm còn phải được người vận hành gắn `data_sensitivity = sensitive` (`hqadmin mcp-tool classify -sensitivity sensitive`): Gateway dựa vào nhãn đó để ẩn tool với client bên ngoài và bắt duyệt khi execution đã đọc nó gọi tool `egress`. Ghi bước này vào runbook của integration.
- Kết quả tool vẫn là dữ liệu không tin cậy (có thể chứa nội dung do bên ngoài kiểm soát). Trường mã / enum / thời gian của hệ thống đích: kiểm dạng trước khi trả; trường văn bản tự do: thay ký tự điều khiển, xuống dòng và ký tự định dạng Unicode, có trần độ dài.

## Phân loại (mỗi tool)

`risk_level` (`read` / `write` / `destructive` / `financial`), `side_effect` (`none` / `idempotent` / `non_idempotent`), `data_sensitivity`, `egress`, `cacheable`, `max_result_bytes`, `timeout`. Chọn mức an toàn hơn khi phân vân. Tool ghi lên hệ thống tài chính (HQS, X-Purchase, CPV, thanh toán) luôn `financial`.

## Xong khi

Qua bộ test hợp đồng dùng chung; mọi tool đã phân loại; tool ghi có idempotency và `dry_run` nếu được; có runbook.

## Thứ tự

Đọc trước, ghi sau. Thứ tự đợt theo roadmap Phase 4 (4a → 4e). HQS, X-Purchase, CPV chưa có đặc tả API: không tự suy diễn API của chúng; cần khảo sát với đội sở hữu trước.
