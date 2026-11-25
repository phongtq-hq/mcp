# ADR-0018: Ngữ cảnh ký bất đối xứng gắn với từng request; tool của managed server mặc định chỉ chạy trong execution

Trạng thái: Đề xuất · 2026-11-25

Liên quan: [ADR-0004](0004-mcp-gateway-single-egress.md), [ADR-0017](0017-single-host-docker-compose-deployment.md), [security.md](../architecture/security.md), [mcp-platform.md](../architecture/mcp-platform.md).

## Bối cảnh

Platform sẽ nối nhiều hệ thống nội bộ qua managed MCP server (Pricing Engine với giá mua vào của nhà cung cấp, Prometheus, Loki, Kubernetes, GitHub, Docker; sau này thêm nữa). Một review bảo mật ngày 2026-11-25 đối chiếu đường đọc dữ liệu với hướng dẫn bảo mật của MCP (bản draft, protocol `2026-07-28`) và tìm ra hai lỗ hổng cấu trúc:

1. **Một khoá HMAC dùng chung cho mọi managed server.** `HQ_SERVICE_SIGNING_KEY` nằm ở `gateway`, `hqadmin` và từng integration. Với HMAC, bên kiểm cũng là bên ký được: chiếm được một integration (ví dụ `integration-docker`, vốn phân tích dữ liệu của Docker API) là có khoá để tự ký ngữ cảnh và gọi thẳng mọi integration khác, bỏ qua Gateway, policy, rate limit và audit. Ngữ cảnh lại không gắn với body, không có định danh dùng một lần và sống 2 phút, nên một header bắt được trên đường Gateway → integration (HTTP trong mạng nội bộ) dùng lại được cho một body khác. Mọi integration còn nằm chung mạng `backend` với PostgreSQL, Redis và Temporal.
2. **Tool trả log và văn bản tự do gọi thẳng được từ bên ngoài.** `serve.ExecutionOnly` mới được dùng cho 2 tool giá của Pricing Engine. `loki.query_logs`, `kubernetes.get_pod_logs`, `docker.get_container_logs`, `github.get_issue`… gọi được bằng token OIDC hay API key qua `/mcp/v1/projects/{id}`, không qua Agent Version, budget hay `execution_steps`. Mặc định của `serve.Handler` là mở: integration mới quên khai thì mở.

## Quyết định

1. **Chữ ký Ed25519.** `gateway` và `hqadmin` (discovery) giữ khoá riêng (`HQ_SERVICE_SIGNING_KEY`, seed 32 byte dạng hex); managed server chỉ giữ khoá công khai (`HQ_SERVICE_VERIFY_KEY`) và **từ chối khởi động nếu được cấp khoá ký** (`serve.VerifierFromEnv`). Khoá tạo bằng `hqadmin service-key init`; `hqadmin service-key public` in khoá công khai. Khoá HMAC dài hơn 32 byte của bản cũ bị từ chối, để việc chuyển là một bước có chủ ý. Thư viện chuẩn của Go (`crypto/ed25519`); không thêm phụ thuộc.
2. **Ngữ cảnh gắn với đúng một request.** Thêm hash SHA-256 của body (`bsha`) và định danh dùng một lần (`jti`) vào ngữ cảnh đã ký. `signedctx.Transport` ký lại cho mỗi request. `serve.Handler` đọc body (có trần), so hash, rồi ghi nhận `jti` vào một `ReplayGuard` trong bộ nhớ cho tới khi ngữ cảnh hết hạn (đầy thì từ chối). Body chỉ được đọc để băm, không để phân quyền: luật "không tự đọc body để phân quyền" giữ nguyên.
3. **Mặc định chỉ chạy trong execution.** Tool call không mang `ExecutionID` chỉ chạy được tool khai bằng `serve.Direct(...)`. Mỗi integration khai `DirectTools` (siêu dữ liệu, trạng thái, số đo); log, văn bản tự do do bên ngoài viết và dữ liệu kinh doanh chỉ chạy trong execution. Bộ test hợp đồng kiểm cả hai chiều qua trường `Direct` của `contract.Tool`, và kiểm thêm "ký cho body khác" và "dùng lại header".
4. **Mỗi managed server một mạng riêng** trong `deployments/production/compose.yaml` (`mcp-<tên>`, internal) chỉ có nó, `gateway` và `hqadmin`; nó không nằm trên `backend`. Integration cần gọi ra ngoài (Pricing Engine) có mạng egress riêng.

## Phương án đã cân nhắc

- **Khoá HMAC riêng cho từng server, suy ra bằng HKDF từ một khoá chủ chỉ Gateway giữ.** Cắt được việc một integration ký cho integration khác, nhưng mỗi integration vẫn ký được cho chính nó (giả ngữ cảnh của org / execution bất kỳ của server đó), và mỗi lần thêm server lại phải phát khoá mới. Ed25519 giải quyết cả hai với một khoá công khai chung.
- **mTLS giữa Gateway và từng integration.** Xác thực được kênh nhưng không mang ngữ cảnh tenant; vẫn cần ngữ cảnh ký, và thêm việc cấp và xoay vòng chứng chỉ cho từng server. Có thể làm thêm về sau; không thay được quyết định 1–2.
- **Chỉ thêm `ExecutionOnly` cho từng tool nhạy cảm (giữ mặc định mở).** Ít thay đổi hơn, nhưng mỗi integration mới phải nhớ khai; một lần quên là một đường đọc hàng loạt. Mặc định đóng thì quên là hỏng chức năng (thấy ngay trong test hợp đồng), không phải lộ dữ liệu.
- **Không làm gì ở phía server, chỉ dựa vào policy của Gateway.** Gateway đã thêm mức sàn tương đương cho tool `data_sensitivity = sensitive` (xem Hệ quả), nhưng nó phụ thuộc người vận hành phân loại đúng; lớp ở server là phòng thủ nhiều tầng.

## Hệ quả

- Triển khai: tạo khoá bằng `hqadmin service-key init`, cấp seed cho `gateway` / `hqadmin` và khoá công khai cho mọi integration, deploy cùng lúc (chữ ký cũ không còn được nhận). Ở local, Makefile suy khoá công khai từ `HQ_SERVICE_SIGNING_KEY` nếu `.env` chưa có `HQ_SERVICE_VERIFY_KEY`; seed 32 byte của bản cũ dùng tiếp được.
- `ReplayGuard` nằm trong bộ nhớ của một process: chạy nhiều bản sao của một integration sau cân bằng tải thì một header có thể được nhận một lần ở mỗi bản sao trong thời hạn của nó (một server = một process là mô hình hiện tại).
- Client MCP bên ngoài (Claude Code của người vận hành…) không còn đọc log qua Gateway; việc đó đi qua một Agent. Đây là thay đổi hành vi có chủ ý.
- Cùng đợt (không thuộc quyết định này nhưng cùng review): Gateway thêm mức sàn dữ liệu (tool `sensitive` chỉ chạy trong execution và không hiện với client bên ngoài; execution đã đọc dữ liệu `sensitive` mà gọi tool `egress` hay tool của server ngoài thì cần duyệt, migration 00040); Registry giữ phân loại của người vận hành khi định nghĩa tool đổi (00039); server giới hạn theo project (00041); output của execution cần `execution:read_content`; hạn mức chung theo org ở Gateway; audit ghi `result_items`.
- Chưa giải quyết: kênh Gateway → integration vẫn là HTTP thường trong mạng nội bộ (chữ ký và chống phát lại giảm rủi ro, không mã hoá); worker bị chiếm quyền vẫn ký được execution token cho task đang chạy (khoá HS256 dùng chung giữa `api`, `worker`, `gateway`).

## Điều kiện xem xét lại

- Một integration cần chạy nhiều bản sao: chuyển `ReplayGuard` sang Redis (khoá theo `org:{id}:`), hoặc gắn ngữ cảnh với bản sao.
- Có yêu cầu mã hoá kênh nội bộ (compliance, mạng nhiều máy chủ): thêm mTLS cho Gateway → integration.
- Cần xoay vòng khoá ký không gián đoạn: cho integration nhận hai khoá công khai trong thời gian chuyển.
