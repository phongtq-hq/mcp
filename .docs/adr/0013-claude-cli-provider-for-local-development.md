# ADR-0013: Claude làm provider đầu tiên; Claude CLI cho phát triển cục bộ, Anthropic API cho production

Trạng thái: Đề xuất · 2026-11-17

## Bối cảnh

Roadmap Phase 1 ghi "một provider adapter (OpenAI) sau interface `ai.Provider`". Chủ dự án muốn dùng Claude, và muốn chạy được platform trên máy cục bộ ngay, chưa cần API key; lên production mới dùng API key.

Claude Code CLI có chế độ không tương tác (`claude -p`) dùng tài khoản người dùng đang đăng nhập. Đã thử trên CLI 2.1.309: tắt được mọi tool có sẵn (`--tools ""`), thay được system prompt, ép được đầu ra theo JSON Schema (`--json-schema`), và response báo usage theo token.

Vấn đề là CLI vốn là một agent tự chạy vòng lặp, trong khi runtime của platform phải kiểm soát từng bước: policy, approval, budget và audit chen vào giữa mỗi lần model xin gọi tool và lần gọi thật.

## Quyết định

1. Provider đầu tiên là **Claude**, thay cho OpenAI trong roadmap. Hợp đồng `ai.Provider` giữ trung lập với nhà cung cấp.
2. Một lời gọi `Complete` là **đúng một lượt** của model. Provider không tự chạy tool và không tự lặp.
3. Hai adapter sau cùng interface:
   - **`claude-cli`**: chỉ cho phát triển cục bộ. Gọi `claude -p` như một model: không tool có sẵn, không MCP server, không nạp settings hay `CLAUDE.md`, không trạng thái giữa các lượt, chạy trong thư mục tạm rỗng với môi trường tối thiểu (process con không thấy DSN hay khoá của platform). Tool calling được mô phỏng bằng danh sách tool trong system prompt và một JSON Schema "quyết định" (gọi tool hoặc trả lời cuối).
     Cô lập: `--safe-mode --disable-slash-commands --tools "" --strict-mcp-config --setting-sources ""`; mọi giá trị do bên ngoài ảnh hưởng đi theo dạng `--cờ=giá_trị`; hội thoại được đóng gói thành các khối JSON mang một mã ngẫu nhiên cho từng lần gọi, nên nội dung không tin cậy không giả được một lượt; huỷ hoặc quá hạn thì giết cả nhóm process. Đã kiểm với CLI thật: không đọc file trên máy khi nội dung tool nhắc tới nó, và model chỉ thấy đúng một công cụ là trả kết quả có cấu trúc.
   - **`anthropic`**: cho staging và production. Dùng SDK Go chính thức (`anthropic-sdk-go/v2`) với API key, tool use gốc. Đã hiện thực (`internal/ai/provider/anthropic`):
     - Adapter chạy trong process `gateway`, không chạy trong process chạy runtime: chỉ `gateway` giải mã được secret (ADR-0008), nên runtime gửi mỗi lượt gọi model tới Gateway bằng execution token của task (`POST /llm/internal/v1/tasks/{task}/complete`), và Gateway chỉ phục vụ đúng model mà Agent Version của task ghim.
     - Khoá theo org: quản trị viên nhập `api_key` khi tạo / sửa model provider. Khoá được kiểm với Anthropic trước; hợp lệ mới được mã hoá và lưu qua Secrets (sai thì 422, không kiểm tra được thì 503, cả hai đều không lưu gì). Provider của org chỉ dùng khoá của chính org.
     - Khoá của platform (cho provider phạm vi platform) đọc từ `HQ_ANTHROPIC_API_KEY` của process `gateway`. Process `api` không giữ khoá LLM nào.
     - Streaming; SDK không tự retry (retry có trần thuộc về runtime); lỗi 429 / 5xx / mạng được đánh dấu thử lại được; thông báo lỗi của provider không được chép vào lỗi trả về.
     - Nội dung nguyên vẹn của mỗi lượt assistant (gồm khối suy luận và chữ ký) được lưu trong `Opaque` và gửi lại y nguyên ở các lượt sau.
     - Tên tool của platform `<server>.<tool>` được đổi dấu chấm thành `__` trên dây (API chỉ nhận chữ, số, `_`, `-`, tối đa 64 ký tự) và đổi lại khi nhận tool call; tên quá dài hoặc trùng nhau sau khi đổi bị từ chối trước khi gọi.
     - Đánh dấu prompt cache tự động ở mỗi request; usage báo riêng token đọc và ghi cache, ledger tính theo giá riêng của chúng.
     - Không gửi `temperature`; suy luận để theo mặc định của model.
4. Adapter `claude-cli` **chỉ khởi tạo được khi `HQ_ENV` đúng bằng `local`** (danh sách cho phép, không phải danh sách cấm), và process `api` chỉ cho đăng ký provider dùng nó khi có thêm `HQ_ENABLE_CLAUDE_CLI=1`. `HQ_ENV` ngoài `local` / `staging` / `production` bị từ chối lúc khởi động.
5. Tên tool do model trả về được kiểm lại với danh sách được phép ngay trong adapter, dù schema đã giới hạn. Arguments được validate theo schema của tool tại Gateway.

## Phương án đã cân nhắc

- **Để CLI tự chạy vòng lặp, nối MCP của nó vào Gateway.** Tool call vẫn qua Gateway nên quyền vẫn được kiểm, nhưng runtime mất kiểm soát từng bước: không chen được giới hạn iteration, budget hay approval, và không ghi được `execution_steps`. Đây vẫn là cách đúng để dùng Claude CLI như một *MCP client bên ngoài* của Gateway, là một việc khác với provider của runtime.
- **Chỉ làm adapter Anthropic API.** Đúng với production nhất, nhưng không chạy được gì cho tới khi có API key.
- **Provider giả lập cố định cho local.** Không tốn gì, nhưng không cho thấy hành vi thật của model với prompt và tool của Agent. Vẫn dùng cho test tự động.

## Hệ quả

- Chạy được use case dẫn đường trên máy cục bộ với model thật, không cần API key.
- Runtime được viết và thử với đúng hợp đồng mà production dùng; lên production chỉ đổi adapter và cấu hình.
- Hành vi **không giống hệt** API thật, nên adapter `anthropic` vẫn phải được thử riêng trước khi lên production:
  - Token trong usage gồm cả phần CLI tự thêm, và gói thuê bao không tính tiền theo token: chi phí ước tính từ đó chỉ để kiểm tra đường ghi usage, không phải số thật.
  - Không có khối suy luận để gửi lại, không có prompt caching do platform điều khiển, không có `stop_reason` `max_tokens` hay `refusal`.
  - Tool calling mô phỏng: `max_output_tokens` không được áp dụng.
  - Mỗi lượt khởi chạy một process (khoảng vài giây), và bị giới hạn theo hạn mức của tài khoản.
- Adapter chỉ dành cho một lập trình viên dùng tài khoản của chính mình trên máy mình. Không dùng nó để phục vụ người khác; việc đó cần API key.
- Với adapter `anthropic`, cấu hình Agent cần xem lại: các model hiện tại từ chối `temperature`, không cho ép gọi một tool cụ thể, và yêu cầu lịch sử hội thoại chỉ-thêm (ảnh hưởng tới kế hoạch nén context ở Phase 2).

## Điều kiện xem xét lại

Khi có API key và adapter `anthropic` đã chạy ổn định: cân nhắc bỏ adapter `claude-cli` nếu việc duy trì hai adapter gây lệch hành vi khó theo dõi. Khi định dạng đầu ra hoặc cờ của CLI thay đổi làm adapter hỏng.
