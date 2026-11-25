# Runbook: managed MCP server Pricing Engine (chỉ đọc)

Trạng thái: **Đề xuất** · Liên quan: [mcp-platform §6.3, §7](../architecture/mcp-platform.md), [ADR-0004](../adr/0004-mcp-gateway-single-egress.md), [roadmap Phase 4 (ngoài các đợt 4a–4e; gần nhất là 4e)](../roadmap.md)

Code: `internal/integration/pricingengine`, binary `cmd/integration-pricing-engine`. Server chỉ có tool đọc. Agent gọi nó chỉ qua MCP Gateway.

Pricing Engine (PE) là hệ thống nội bộ giữ **giá mua vào của nhà cung cấp** (giá vốn của HQ) và dựng bảng giá xếp hạng offer cho từng SKU. Agent dùng integration này để **khuyến nghị cho người đọc**: nó không ra quyết định, không đặt hàng và không ghi gì vào PE.

Hình dạng API được đọc từ mã nguồn và tài liệu của PE (`apps/app/src/routes/api/v1/**`, `packages/pricing/src/pricebook.ts`, `.docs/vi/feature-price-api.md`, `feature-catalog-api.md`). **Chưa chạy với một PE thật** (mục 7).

## 1. Tool

| Tool | Gọi PE | Việc | Giới hạn |
|---|---|---|---|
| `get_price_book` | `GET /api/v1/prices/{sku}` | Bảng giá đã xếp hạng của một SKU | `limit` 1–100 offer hợp lệ; `currency` 3 chữ in hoa; offer cuối bị bỏ (đếm ở `offers_omitted`) khi vượt trần |
| `get_price_books` | `POST /api/v1/prices/batch` | Bảng giá của nhiều SKU | 1–50 SKU không trùng (PE nhận tới 100); SKU không vừa trần nằm ở `deferred_skus` |
| `list_products` | `GET /api/v1/products` | Danh mục SKU, không có giá | `page_size` 1–100 (mặc định 50; PE nhận tới 500); `updated_since` RFC 3339 |
| `list_skus` | `GET /api/v1/skus` | SKU của feed danh mục; lọc `status` / `brand_id` / `region`, hoặc tra một SKU bằng `sku_code` | `limit` 1–100 (mặc định 50); `sku_code` đứng một mình |
| `get_sku` | `GET /api/v1/skus/{id}` | Một SKU theo UUID | — |
| `list_brands` | `GET /api/v1/brands` | Thương hiệu (game) | `limit` 1–100 (mặc định 50); lọc `status` |

**Hai tool giá chỉ chạy trong một execution của Agent.** `get_price_book` và `get_price_books` từ chối tool call không thuộc execution nào (client bên ngoài gọi thẳng qua Gateway bằng token OIDC hay API key) với lỗi "tool này chỉ chạy trong một execution của Agent", trước khi có request nào tới PE. Server nhận biết bằng `ExecutionID` trong ngữ cảnh do Gateway ký (hai tool này không nằm trong `DirectTools`, ADR-0018); Gateway chỉ ký nó cho tool call của Agent Runtime, sau khi đã kiểm task, Agent Version và người khởi tạo. Hệ quả: mỗi lần đọc giá vốn đều thuộc một Agent Version đã liệt kê tool đó, có budget và có `execution_steps`. Bốn tool danh mục (không có giá) gọi trực tiếp được.

Mọi kết quả đã tuần tự hoá không quá 64 KiB (bằng `max_result_bytes` mặc định của Registry). Phân trang bằng `cursor` / `next_cursor`; con trỏ là con trỏ mờ của PE, chuyển nguyên vẹn. Khi một trang của PE không vừa trần, server đọc lại PE với số dòng ít hơn (tối đa 7 lần) thay vì cắt bớt dòng, vì con trỏ của PE chỉ đúng ở cuối một trang của chính PE. Timeout HTTP của một request tới PE là 15 giây; **hạn tổng của một tool call là 25 giây**, tính cả các lần đọc lại trang nhỏ hơn và thời gian chờ suất đồng thời (quá hạn: lỗi `did not answer within the time allowed`). Context bị huỷ (Gateway bỏ kết nối, execution bị huỷ) thì server dừng gọi PE. Thân phản hồi của PE đọc tối đa 8 MiB.

Server chỉ gửi `GET` tới năm đường dẫn đọc ở trên và đúng một `POST /api/v1/prices/batch` (PE dùng POST vì danh sách SKU không vừa query string; không có gì được tạo). **Cố ý không có:** `POST /api/v1/fulfillment-events` (endpoint ghi), `/api/v1/tombstones`, `deltaToken` (đồng bộ bản sao, không phải việc của Agent), `/api/v1/brands/{id}`, tham số `page` (đã ngừng khuyến nghị ở PE), và mọi thứ dưới `/admin` hay cần session.

Kết quả là DTO liệt kê từng trường, tên `snake_case`; trường PE thêm sau này không tự tới Agent. Khác với JSON của PE:

- Tiền (`price`, `converted_price`, `face_value`) giữ nguyên là **chuỗi thập phân**; chuỗi không đúng dạng thập phân làm cả lời gọi lỗi (`cannot read`), không được đoán.
- **Trường mã, enum và thời gian được kiểm dạng**; sai dạng làm cả lời gọi lỗi `cannot read`, giá trị đó không tới Agent: mã SKU (`sku`, `code`, `former_codes`, `canonical_sku`: theo `SKU_PATTERN`), mã tiền tệ (`currency`, `comparison_currency`, `converted_currency`, `face_currency`: 2–10 chữ in hoa hoặc số), `region` / `regions` (2–10 chữ in hoa hoặc số), `status` / `availability` / `product_type` (dạng mã `snake_case`), mọi mốc thời gian (RFC 3339), `scoring_profile.id`, các UUID. Với enum, server kiểm DẠNG chứ không kiểm tập đóng: PE thêm giá trị mới (ví dụ một `product_type`) không làm hỏng tool. Cái giá: **một dòng hỏng trong dữ liệu PE làm hỏng cả trang chứa nó**; xem mục 6.
- **Trường văn bản tự do được làm sạch** (`name`, `supplier.name`, `supplier.code`, `supplier_product_id`, `brand`, `brand_name`, `category`, `unit`): ký tự điều khiển, xuống dòng, dấu phân cách dòng / đoạn và ký tự định dạng Unicode (bidi, zero-width, BOM) thành khoảng trắng, khoảng trắng liền nhau gộp lại. Đây là lớp giảm thiểu, không phải bảo đảm: nội dung vẫn là dữ liệu không tin cậy.
- Giá trị `null` của PE thành trường vắng mặt (`rank`, `score`, `score_breakdown`, `converted_price`, `availability`…). `score: 0` là điểm thật và vẫn có mặt.
- `nameKey` của thương hiệu bị bỏ (khoá chuẩn hoá nội bộ; tên trường chứa `key` cũng bị Gateway che giá trị). `currency` cấp bảng giá của PE thành `converted_currency`. `comparisonPrice`, `etag`, `candidates`, `metricsGlobals` không có trong phản hồi của PE và không có ở đây.
- `results` của lô (map theo SKU) thành mảng `price_books` theo thứ tự đã hỏi; `notFound` thành `not_found`.
- `ineligible_reason` lạ (PE có thể thêm lý do mới) được giữ nếu đúng dạng mã, ngược lại thành `other`.
- Văn bản bị cắt (tên 200 byte; `supplier_product_id`, `supplier.code`, `category` 100 byte; `unit` 64 byte); `former_codes` tối đa 10 mã, `regions` tối đa 64.

Server không tính thống kê nào của riêng nó (không trung bình, không chênh lệch giá): điểm và thứ hạng là của PE, theo profile chấm điểm gán cho API key mà server dùng.

Văn bản lỗi của PE không bao giờ được chuyển cho Agent; lỗi được suy từ mã trạng thái (mục 6).

## 2. Cấu hình

| Biến | Bắt buộc | Ý nghĩa |
|---|---|---|
| `HQ_PRICING_ENGINE_URL` | Có | Địa chỉ gốc của PE, ví dụ `https://pe.example` (kèm path prefix nếu có). Không được chứa credential, query hay fragment. **Luôn phải là `https`**; ngoại lệ duy nhất: `HQ_ENV=local` VÀ host là loopback (`localhost`, `127.0.0.0/8`, `::1`) thì nhận `http` |
| `HQ_PRICING_ENGINE_API_KEY_FILE` | Có (trừ khi dùng biến bên dưới ở `local`) | Đường dẫn file chứa đúng một API key của PE trên một dòng. Khoá không đọc từ biến môi trường. Ngoài `local`, file cho nhóm hoặc người khác đọc (`mode & 077 ≠ 0`) làm process từ chối khởi động: đặt `0600` hoặc `0400` |
| `HQ_PRICING_ENGINE_API_KEY` | Không | **Chỉ ở `HQ_ENV=local`:** khoá đặt thẳng trong biến môi trường, thay cho file, để chạy thử trên máy phát triển. Ngoài `local` biến này làm process từ chối khởi động; đặt cùng lúc với `_FILE` cũng bị từ chối. Khoá trong `.env` dễ lọt hơn file quyền `0600`: đừng commit `.env`, và thu hồi khoá khi thử xong |
| `HQ_PRICING_ENGINE_ORG_ID` | Có | Id của organization sở hữu PE này. Tool call thay mặt org khác bị từ chối |
| `HQ_SERVICE_VERIFY_KEY` | Có | Khoá CÔNG KHAI kiểm ngữ cảnh của Gateway (Ed25519, hex 64 ký tự; `hqadmin service-key public`). Server từ chối khởi động nếu được cấp `HQ_SERVICE_SIGNING_KEY` (ADR-0018) |
| `HQ_ENV` | Có, tường minh | `local`, `staging` hoặc `production`. Process từ chối khởi động nếu thiếu: mặc định `local` sẽ lặng lẽ nới hai kiểm tra (http tới loopback, quyền của file khoá) |
| `HQ_HTTP_ADDR` | Không | Địa chỉ lắng nghe, mặc định `:8095` (`make run-pricing-engine-mcp` lấy từ `HQ_PRICING_ENGINE_MCP_HTTP_ADDR`) |

Các biến này chưa có trong `.env.example` (file đó chưa được git theo dõi ở thời điểm viết runbook); thêm tay vào `.env` khi chạy cục bộ. Integration chưa có trong `deployments/production/compose.yaml` hay compose của phát triển: chạy bằng binary / `make run-pricing-engine-mcp`.

`HQ_PRICING_ENGINE_URL` là cấu hình của **người vận hành managed server**, không phải URL do tenant nhập: nó không đi qua SSRF guard, và không tool nào nhận URL, host hay khoá. Server không theo redirect và không dùng proxy từ biến môi trường (`HTTP_PROXY`…), nên khoá chỉ tới đúng địa chỉ đã cấu hình. Đừng lấy giá trị biến này từ dữ liệu của tenant.

Một process phục vụ đúng một PE của đúng một org. Slug trong Registry là `pricing-engine` và cũng là đích (`aud`) của ngữ cảnh ký; tên tool phía client là `pricing-engine.<tool>`.

Chạy cục bộ (máy phát triển, PE chạy trên chính máy đó):

```
install -m 600 /dev/null ~/.config/hq/pricing-engine.key   # rồi ghi khoá vào file, một dòng
make run-pricing-engine-mcp
hqadmin mcp-server register -slug pricing-engine -name "Pricing Engine" -endpoint http://localhost:8095/mcp -org <slug org>
```

Endpoint `http://localhost:8095/mcp` **chỉ dành cho máy phát triển**. Ở staging / production, endpoint đăng ký với Registry phải là `https` hoặc nằm trên mạng riêng giữa Gateway và integration (mục 4).

### Thứ tự đăng ký an toàn

Với managed server, Registry phân loại `read` / `none` và **bật sẵn** mọi tool mang `readOnlyHint` ngay khi đăng ký (`registry.ApplyDiscovery`); `data_sensitivity` mặc định là `internal`. Lệnh `register` vì vậy chính là lúc cả sáu tool dùng được. Để không có khoảng thời gian nào giá vốn đọc được trước khi admin quyết định, làm theo thứ tự:

1. **Tạo key ở PE nhưng CHƯA gán scope `api_pricing:read`** (chỉ `api_catalog:read`), hoặc chưa đưa IP của máy chạy integration vào whitelist của key. Lúc này hai tool giá và `list_products` trả lỗi `refused the credential` dù có ai gọi.
2. Khởi động integration và chạy `hqadmin mcp-server register` ở **phạm vi org sở hữu** (không ở phạm vi platform).
3. Admin duyệt bảng phân loại ở mục 5 và quyết định bật / tắt từng tool. Tắt tool chưa được duyệt:

   ```
   hqadmin mcp-tool classify -server pricing-engine -tool get_price_book  -enabled=false -org <slug org>
   hqadmin mcp-tool classify -server pricing-engine -tool get_price_books -enabled=false -org <slug org>
   ```
4. Quyết định Agent Version nào được tham chiếu hai tool giá (xem quy ước ở mục 4).
5. **Sau cùng** mới gán `api_pricing:read` (và whitelist IP) cho key ở PE.

Hai tool giá chỉ chạy trong execution, nên kể cả khi đang bật, người có `mcp_tool:call` không đọc được giá bằng cách gọi thẳng Gateway; họ cần một Agent Version liệt kê tool đó và quyền chạy nó.

**Tool đã tắt TỰ BẬT LẠI khi định nghĩa của nó đổi.** Registry chỉ giữ phân loại cũ (kể cả `enabled = false`) khi hash định nghĩa của tool không đổi; khi định nghĩa đổi (nâng phiên bản integration làm đổi mô tả hoặc schema), tool của managed server mang `readOnlyHint` lại được phân loại `read` và **bật**, và có hiệu lực khi version mới được duyệt (`internal/mcp/registry/admin.go`). **Sau mỗi lần duyệt một version của `pricing-engine`, kiểm lại trạng thái hai tool giá** (`GET /v1/mcp-tools`, hoặc màn hình MCP server) và tắt lại nếu cần. Hành vi này của Registry chưa được sửa (mục 7).

## 3. Tạo API key ở PE

Người quản trị PE làm (giao diện admin của PE):

1. **Bảo mật → API keys** (`/admin/security/api-keys/new`): tạo một key **RIÊNG** cho integration này, mỗi môi trường một key (ví dụ `MCP Platform production`). **Không bao giờ dùng chung key với GOX, XPurchase hay hệ thống nào khác:** rate limit, IP whitelist, profile chấm điểm và việc thu hồi đều theo key; dùng chung thì một Agent lặp vô ích làm hệ thống đặt hàng nhận 429, và thu hồi key của integration làm dừng đặt hàng. Khoá gốc (`sk_live_…`) chỉ hiện một lần.
2. **Quyền tính năng → API keys** (`/admin/access-control/feature-permissions`): theo thứ tự ở mục 2 (gán `api_catalog:read` trước, `api_pricing:read` sau cùng), key cuối cùng có **đúng hai scope** `api_pricing:read` (bảng giá và `/products`) và `api_catalog:read` (`/skus`, `/brands`). **Không** gán `api_fulfillment:create` hay scope nào khác: server không dùng, và thiếu scope là lớp bảo đảm phía PE rằng key này không ghi được gì.
3. **IP whitelist** của key: đặt thành địa chỉ đi ra của máy chạy `integration-pricing-engine`. Khoá lọt ra ngoài máy đó thì vô dụng.
4. **Rate limit** của key: nên đặt **thấp hơn mặc định** (mặc định 600 request / 60 giây). Server tự giữ ở 300 request / phút (mục 4), nên một mức quanh 300–400 / 60 giây là đủ và làm PE thành lớp chặn thứ hai.
5. **Profile chấm điểm** của key: nếu PE gán profile riêng cho key, thứ hạng Agent thấy là thứ hạng theo profile đó, có thể khác thứ hạng GOX / XPurchase thấy. Để profile mặc định nếu muốn Agent thấy cùng thứ hạng với hệ thống đặt hàng.
6. Ghi khoá vào file trên máy chạy integration, quyền `600`, chủ sở hữu là user của process; đặt đường dẫn vào `HQ_PRICING_ENGINE_API_KEY_FILE`. Xoay khoá: tạo key mới ở PE, thay file, khởi động lại process, rồi thu hồi key cũ.

## 4. Giới hạn bắt buộc phía PE và hạ tầng

- **Hạn mức riêng của server:** token bucket 300 request / phút (dồn tối đa 30) và tối đa 8 request đồng thời tới PE, tính cho cả process (mọi Agent, mọi execution). Con số cố ý bằng một nửa hạn mức mặc định của key PE. Hết hạn mức: lỗi `server_rate_limited` kèm `retry_after_seconds`, PE không nhận request nào, server không xếp hàng chờ. Lời gọi chờ suất đồng thời thì chờ trong hạn 25 giây của tool call. Hạn mức nằm trong bộ nhớ của process: chạy hai bản sao của integration là gấp đôi hạn mức.
- **Trần của Gateway không thay được hạn mức trên:** Gateway giới hạn 120 request / phút theo từng principal, và **mỗi API key của platform là một principal riêng** với 120 / phút của riêng nó; nhiều key hoặc nhiều người dùng cộng lại vượt xa một con số duy nhất. Trần theo PE là hạn mức của server (ở trên) và rate limit của key PE.
- **Ngắt mạch sau 429.** Khi PE trả `429`, server đóng ngắt mạch: trong thời gian PE nêu ở `Retry-After` (hoặc `X-RateLimit-Reset`; không nêu thì 10 giây; luôn trong khoảng 1–120 giây) nó không gửi request nào và trả lỗi `pricing_engine_rate_limited` kèm `retry_after_seconds`. Nó không tự thử lại và không ngủ chờ. Ngắt mạch nằm trong bộ nhớ của process: khởi động lại là mất.
- **Scope là toàn bộ quyền ở PE:** key có `api_pricing:read` đọc được giá của MỌI SKU và mọi nhà cung cấp. Integration không lọc theo SKU, thương hiệu hay nhà cung cấp. Nếu có phần dữ liệu không nên tới Agent, chặn ở phía PE.
- **Agent nào gọi được tool giá thì thấy giá vốn, và đưa nó đi đâu là việc của Agent Version.** Gateway thực thi nhãn `sensitive` ở mục 5 (ẩn với client bên ngoài; tool `egress` sau khi đọc giá vốn cần duyệt), nhưng tool không `egress` (ví dụ tool ghi nội bộ) và câu trả lời của Agent vẫn mang được giá vốn đi. Kiểm soát hiện có: hai tool giá chỉ chạy trong execution; chính sách project (`hqadmin mcp-server scope`); output của execution cần `execution:read_content`; Agent Version nào tham chiếu `pricing-engine.get_price_book(s)`; ai chạy được Agent đó và đọc được nội dung step (`execution:read_content`); model provider nào nhận nội dung (kết quả tool đi vào prompt gửi tới provider LLM).
- **Quy ước cho tới khi Gateway thực thi `data_sensitivity`:** một Agent Version tham chiếu tool giá **không được kèm tool ghi hay tool `egress`** (tool gửi dữ liệu ra ngoài hạ tầng của org: chat, email, webhook, issue tracker bên ngoài…), và không được nằm trong `can_delegate_to` của / giao việc cho Agent có các tool đó khi kết quả của nó mang giá. Lý do: tên nhà cung cấp và tên SKU là văn bản không tin cậy đi vào context cùng với giá vốn; một Agent vừa đọc được giá vừa ghi / gửi ra ngoài được là đường rò rỉ qua prompt injection. Quy ước này do người duyệt Agent Version giữ; platform chưa kiểm.
- **Đường nối Gateway → integration phải là TLS hoặc mạng riêng.** Header ngữ cảnh do Gateway ký sống 2 phút và chưa gắn với body: ai nghe được một header loại `call` thì gọi được tool bất kỳ của server này thay mặt org đó trong thời gian ấy. Ví dụ `http://localhost:8095` chỉ dành cho máy phát triển.
- **Đường nối integration → PE luôn là `https`.** `http` chỉ được nhận khi `HQ_ENV=local` và host là loopback; `http` tới bất kỳ host nào khác bị từ chối ở mọi môi trường, kể cả `local`.
- Tên SKU, tên nhà cung cấp, tên thương hiệu, `category` là văn bản do người nhập ở PE hoặc lấy từ nhà cung cấp: chúng là dữ liệu không tin cậy khi vào context của model.
- Dữ liệu có độ trễ: PE dựng lại bảng giá trong vài giây sau khi nhà cung cấp sync, đánh dấu `stale` trong vòng 5 phút; `generated_at`, `price_updated_at` và `stale` nằm trong kết quả để người đọc khuyến nghị thấy độ mới. Integration không cache.

## 5. Phân loại đề xuất

Đề xuất để admin duyệt.

| Tool | `risk_level` | `side_effect` | `data_sensitivity` | `egress` | `cacheable` | `max_result_bytes` | `timeout` |
|---|---|---|---|---|---|---|---|
| `get_price_book` | `read` | `none` | `sensitive` | `false` | `false` | 65536 | 30 s |
| `get_price_books` | `read` | `none` | `sensitive` | `false` | `false` | 65536 | 30 s |
| `list_products` | `read` | `none` | `internal` | `false` | `false` | 65536 | 30 s |
| `list_skus` | `read` | `none` | `internal` | `false` | `false` | 65536 | 30 s |
| `get_sku` | `read` | `none` | `internal` | `false` | `false` | 65536 | 30 s |
| `list_brands` | `read` | `none` | `internal` | `false` | `false` | 65536 | 30 s |

Lý do: mọi tool chỉ đọc PE (`read` / `none`); `get_price_books` dùng `POST` nhưng PE không tạo hay đổi gì và không ghi audit từng lượt đọc. Hai tool giá trả giá mua vào theo từng nhà cung cấp, là bí mật thương mại của HQ (`sensitive`); bốn tool danh mục không có giá (`internal`). Không tool nào gửi dữ liệu ra ngoài hạ tầng của org (`egress = false`). Không cache: giá và tồn kho đổi theo từng lượt sync của nhà cung cấp, và một khuyến nghị dựa trên giá cũ là khuyến nghị sai (với danh mục có thể nới thành cache ngắn sau, khi có số đo). Timeout 30 giây lớn hơn **hạn tổng 25 giây** mà server tự đặt cho một tool call (gồm mọi lần đọc lại trang nhỏ hơn của `list_*` và thời gian chờ suất đồng thời; mỗi request tới PE tối đa 15 giây), nên server trả lỗi rõ ràng trước khi Gateway bỏ kết nối. Đừng đặt `timeout` của tool dưới 25 giây.

**Bắt buộc sau khi đăng ký:** gắn nhãn `sensitive` cho hai tool giá, và giới hạn server vào đúng project được dùng giá vốn:

```sh
hqadmin mcp-tool classify -server pricing-engine -tool get_price_book  -sensitivity sensitive -org <slug org>
hqadmin mcp-tool classify -server pricing-engine -tool get_price_books -sensitivity sensitive -org <slug org>
hqadmin mcp-server scope  -slug pricing-engine -org <slug org> -scoped=true -add <slug project>
```

Từ đó Gateway: không cho client bên ngoài thấy hay gọi hai tool này; ghi lại execution nào đã đọc giá vốn, và bắt người duyệt khi execution đó gọi tool `egress` hay tool của MCP server ngoài; chỉ project đã gắn thấy server. Đổi định nghĩa tool (bản integration mới) không làm rơi nhãn (migration 00039).

## 6. Lỗi thường gặp

| Triệu chứng | Nguyên nhân | Xử lý |
|---|---|---|
| Process không khởi động: `HQ_ENV is required` | `HQ_ENV` không đặt tường minh | Đặt `local`, `staging` hoặc `production` |
| Process không khởi động: `địa chỉ API phải là https` | `HQ_PRICING_ENGINE_URL` là `http` ngoài `local`, hoặc ở `local` nhưng host không phải loopback | Dùng `https`; `http` chỉ cho `localhost` / `127.x` / `::1` ở `local` |
| Process không khởi động: `file cho nhóm hoặc người khác đọc` | Ngoài `local`, file khoá có quyền rộng hơn `0600` | `chmod 600` (hoặc `400`), chủ sở hữu là user của process |
| Tool lỗi `tool này chỉ chạy trong một execution của Agent` | `get_price_book(s)` được gọi thẳng qua Gateway, không từ một execution | Đúng thiết kế. Chạy một Agent có tool đó trong `tools` |
| Tool lỗi `server_rate_limited` | Hạn mức 300 request / phút của chính server đã hết | Chờ `retry_after_seconds`; tìm Agent đang lặp. Không nâng được bằng cấu hình |
| Tool lỗi `did not answer within the time allowed` | Quá hạn tổng 25 giây của một tool call (PE chậm, chờ suất đồng thời, hoặc execution bị huỷ) | Thử lại; đặt `limit`, giảm số SKU của lô |
| Process không khởi động: `HQ_PRICING_ENGINE_API_KEY_FILE: …` | Thiếu biến, file không đọc được, rỗng, quá 4 KiB, hoặc có khoảng trắng / nhiều dòng | File chứa đúng một khoá trên một dòng, process đọc được |
| Gateway nhận 401 từ server | Khoá công khai không ứng với khoá ký của Gateway, đồng hồ lệch quá 30 giây, header bị dùng lại hoặc body bị sửa sau khi ký, hoặc slug đăng ký không phải `pricing-engine` | Đối chiếu `hqadmin service-key public` với `HQ_SERVICE_VERIFY_KEY`, NTP, slug |
| Gateway nhận 403 từ server | Tool call thay mặt org khác `HQ_PRICING_ENGINE_ORG_ID` | Đăng ký server ở đúng org; không đăng ký ở phạm vi platform |
| Tool lỗi `SKU not found` | PE trả 404: SKU không tồn tại, đã ngừng (`inactive`), hoặc mã sai dấu (`HQ_A` khác `HQ-A`) | Lấy mã đúng từ `list_products` / `list_skus`; SKU `inactive` có trong danh mục nhưng không có bảng giá |
| Tool lỗi `refused the credential of this server` | PE trả 401 / 403: khoá sai, hết hạn, bị thu hồi, thiếu scope, hoặc IP ngoài whitelist | Xem log (`pricing engine api: credential bị từ chối`, kèm đường dẫn và status). `/prices`, `/products` lỗi mà `/skus`, `/brands` chạy (hoặc ngược lại) là thiếu một trong hai scope |
| Tool lỗi `pricing_engine_rate_limited` | PE trả 429, hoặc ngắt mạch còn hiệu lực | Chờ `retry_after_seconds`. Lặp lại nhiều: rate limit của key ở PE đang thấp hơn 300 / phút của server, hoặc key đang bị dùng chung (không được dùng chung, mục 3) |
| Tool lỗi `pricing_engine_maintenance` | PE trả 503 (đang bảo trì) | Thử lại sau; không có gì được đọc |
| Tool lỗi `rejected the parameters` | PE trả 400: `cursor` của tool khác, hoặc đổi bộ lọc giữa lượt duyệt | Bắt đầu lại không có `cursor`; giữ nguyên bộ lọc khi đi tiếp |
| Tool lỗi `redirect, which this server does not follow` | `HQ_PRICING_ENGINE_URL` trỏ vào địa chỉ chuyển hướng (http → https, thiếu path prefix) | Sửa URL thành địa chỉ cuối cùng |
| Tool lỗi `is unreachable` | PE không tới được, TLS lỗi, hoặc quá 15 giây | Xem log (`pricing engine api: request lỗi`); log không chứa khoá |
| Tool lỗi `cannot read` | Phản hồi 200 của PE không đúng hình dạng mong đợi: giá không phải chuỗi thập phân, `sku` không khớp mã đã hỏi, id không phải UUID, lô thiếu một SKU, hoặc **một trường mã / enum / thời gian sai dạng** (mã SKU cũ không theo `SKU_PATTERN`, region viết thường, mã tiền tệ lạ…) | Nếu chỉ một trang / một SKU lỗi: tìm dòng hỏng ở PE (trang `list_*` chứa nó lỗi cả trang; thu nhỏ `limit` để khoanh vùng) và sửa dữ liệu ở PE. Nếu mọi lời gọi lỗi: PE đã đổi hợp đồng, hoặc URL trỏ vào thứ không phải PE: đối chiếu với `/api/documentation/openapi.json` của PE và cập nhật integration |
| Tool lỗi `too large` | Thân phản hồi của PE quá 8 MiB, hoặc một trang không vừa 64 KiB sau 7 lần thu nhỏ (mỗi lần thu nhỏ là một request, tính vào hạn mức) | Đặt `limit`, giảm số SKU của lô |
| Tool lỗi `unexpected status (N)` | Mã trạng thái khác các mã trên (5xx của PE) | Xem log của PE; văn bản lỗi của PE cố ý không được chuyển tiếp |
| `offers_omitted` > 0 hoặc `deferred_skus` có phần tử | Kết quả vượt 64 KiB | Đặt `limit`; hỏi lại các SKU trong `deferred_skus` |

## 7. Chưa có / chưa kiểm chứng

- **Chưa gọi một PE thật.** Test đơn vị, test hợp đồng và test đầu-cuối qua Gateway (`go test ./tests/api -run TestPricingEngineEndToEnd`: client bên ngoài bị từ chối ở tool giá, một execution đọc được bảng giá) đều dùng PE giả lập có hình dạng JSON chép từ mã nguồn PE. Trước khi cho Agent dùng: chạy với PE staging và đối chiếu từng tool. Việc kiểm dạng chặt ở mục 1 là giả định về dữ liệu thật của PE (ví dụ mọi mã SKU, kể cả mã cũ, theo `SKU_PATTERN`; mọi mã tiền tệ là 2–10 chữ in hoa / số): PE staging là nơi đầu tiên kiểm được nó.
- `ETag` / `If-None-Match` của PE chưa dùng (không cache).
- Hạn mức của server (300 / phút, 8 đồng thời) là hằng trong code, chưa cấu hình được; nằm trong bộ nhớ của một process.
- Test đầu-cuối của đường execution dùng adapter model giả lập trong process; chưa chạy với Temporal `worker` thật cho integration này.
- Nhiều PE mỗi org; lọc theo nhà cung cấp / thương hiệu ở phía integration.
- Thống kê trên giá (chênh lệch, xu hướng): cố ý không có trong bản này.
- Mọi thao tác ghi (`fulfillment-events`): không có và không dự kiến trong integration chỉ đọc này.
