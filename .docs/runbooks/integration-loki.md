# Runbook: managed MCP server Loki (chỉ đọc)

Trạng thái: **Đề xuất** · Liên quan: [mcp-platform §6.3, §7](../architecture/mcp-platform.md), [ADR-0004](../adr/0004-mcp-gateway-single-egress.md), [roadmap Phase 4 (đợt 4a)](../roadmap.md), [runbook Prometheus](integration-prometheus.md)

Code: `internal/integration/loki`, binary `cmd/integration-loki`. Server chỉ có tool đọc. Agent gọi nó chỉ qua MCP Gateway.

## 1. Tool

| Tool | Việc | Giới hạn |
|---|---|---|
| `query_logs` | Đọc dòng log bằng một LogQL log query trên `[start, end)` | Biểu thức ≤ 2000 byte, không có `[ … ]`, `offset`, `@`, biến; khoảng ≤ 24 giờ; ≤ 1000 dòng (mặc định 100); mỗi dòng cắt ở 2048 byte; kết quả ≤ 60 KiB |
| `query_metric` | LogQL metric query dạng range (`rate`, `count_over_time`, `sum by`…) | Mỗi `[ … ]` là một duration; **khoảng start–end + cửa sổ `[ … ]` lớn nhất ≤ 24 giờ**; không `offset`, `@`, subquery, biến; step ≥ 10 giây; ≤ 500 điểm mỗi series; ≤ 50 series (mặc định 20) |
| `list_labels` | Tên label của các stream trong những giờ gần đây | Lookback 1–24 giờ (mặc định 6); ≤ 500 mỗi trang (mặc định 100) |
| `list_label_values` | Giá trị của một label trong những giờ gần đây | Như trên; tên label khớp `^[a-zA-Z_][a-zA-Z0-9_]{0,127}$`; giá trị cắt ở 256 byte |

Kết quả đã tuần tự hoá của `query_logs` không quá 60 KiB, của các tool khác không quá 64 KiB (bằng `max_result_bytes` mặc định của Registry); phần vượt bị bỏ ở server, không để Gateway cắt giữa JSON.

`query_logs`:

- Mỗi dòng là `{timestamp, labels, line}`; các stream được gộp theo thời gian, theo `direction` (`backward` mặc định: mới nhất trước).
- Dòng dài hơn 2048 byte bị cắt, kết thúc bằng `…` và mang `line_truncated: true`. Một dòng mà label tự nó quá lớn (trên 16 KiB sau khi tuần tự hoá) bị bỏ label và mang `labels_omitted: true`.
- Khi vượt 60 KiB hoặc vượt `limit`, các dòng ở **cuối thứ tự đọc** bị bỏ và `truncated: true`: với `backward` là các dòng cũ nhất (giữ phần mới nhất); với `forward` là các dòng mới nhất (giữ phần đầu khoảng thời gian).
- Con trỏ đọc tiếp: `next_end` (backward: gọi lại với `end` = giá trị này, giữ `start`) hoặc `next_start` (forward: gọi lại với `start` = giá trị này, giữ `end`). Con trỏ cũng có khi số dòng trả về đúng bằng `limit` (Loki không cho biết còn dòng hay không). Các dòng khác có **cùng đúng timestamp nanosecond** với dòng cuối của trang có thể bị bỏ sót khi đọc tiếp.
- Gửi một metric query vào `query_logs` là lỗi tool trước khi tới Loki (biểu thức có `[ … ]`). Gửi một log query vào `query_metric` là lỗi tool sau khi Loki đã chạy nó (server chỉ biết qua loại kết quả).
- Mỗi dòng mang tối đa 32 label; khi label bị bỏ bớt (quá nhiều, hoặc tên trùng nhau sau khi cắt ở 128 byte) dòng mang `labels_truncated: true`. Series của `query_metric` cũng vậy.

### Bộ kiểm từ vựng cho LogQL

Trần 24 giờ của `start` / `end` tự nó không đủ: một biểu thức kéo được dữ liệu ngoài khoảng đó vào kết quả (đã chứng minh trên Loki thật: `sum by (l) (count_over_time({app="probe"} | regexp "(?P<l>.*)" [3h]))` trả nội dung log cũ qua label; `[30m] offset 2h15m` và `[30d]` cũng vậy). Vì thế cả hai tool query quét biểu thức trước khi gửi (`internal/integration/loki/logql.go`) và từ chối, không gửi gì tới Loki, khi gặp ở **ngoài chuỗi và chú thích**:

- từ `offset` (không phân biệt hoa thường) và ký tự `@`;
- ký tự `$` (biến kiểu `$__interval`);
- một `[ … ]` không phải đúng một duration dạng `<số><đơn vị>` lặp lại với đơn vị `ms`, `s`, `m`, `h`, `d`, `w` (ví dụ `[5m]`, `[1h30m]`); có `:` (subquery); hoặc lớn hơn 24 giờ;
- chuỗi `"…"` hay chuỗi backtick không đóng, xuống dòng trong chuỗi `"…"`, `[` / `]` không khớp;
- với `query_logs`: bất kỳ `[ … ]` nào; với `query_metric`: khoảng start–end cộng cửa sổ lớn nhất vượt 24 giờ.

Đây **không phải parser của LogQL**. Nó cố ý từ chối nhầm một số biểu thức hợp lệ: label tên `offset` (`{offset="1"}`), duration thập phân (`[1.5h]`), đơn vị `y` / `ns` / `us`. Nó cũng chỉ biết những cách dịch thời gian đã liệt kê ở trên; một phiên bản Loki thêm cú pháp mới (hay một cú pháp chúng tôi chưa biết) sẽ không bị nó chặn. Vì vậy giới hạn phía Loki ở mục 3 (`max_query_lookback`, `max_query_length`) vẫn là lớp thứ hai **bắt buộc**, không phải tuỳ chọn. Cú pháp duration mới được đối chiếu ở mức "LogQL dùng cú pháp duration của Prometheus" (tài liệu Loki); tập đơn vị được nhận là một tập con chắc chắn.

Server chỉ gửi `GET` tới `/loki/api/v1/query_range`, `/loki/api/v1/labels`, `/loki/api/v1/label/<name>/values`. Không có tool nào chạm push, delete, rules, tail (websocket), flush hay endpoint quản trị. `start` / `end` luôn được gửi tường minh (Unix nanosecond); server không dùng `since`, `interval`, hay mặc định thời gian của Loki.

### Dòng log là dữ liệu không tin cậy và có thể chứa secret

- **Tool không lọc được secret trong log.** `query_logs` trả nguyên văn những gì ứng dụng đã ghi: token trong URL, header `Authorization` bị log nhầm, chuỗi kết nối, dữ liệu cá nhân của khách hàng. Server không biết dòng nào nhạy cảm. Bước che mẫu secret của Gateway chỉ bắt được các mẫu nó biết; đừng coi đó là bảo đảm.
- Nếu Loki chứa log không nên tới Agent (và tới nhà cung cấp LLM), chặn ở **phía Loki**: tenant riêng cho Agent, chỉ đẩy vào đó các stream đã cho phép, hoặc một proxy giới hạn theo label (ví dụ LBAC của Grafana Enterprise Logs / Cloud; chưa đối chiếu chi tiết). LogQL đọc được **mọi** stream của tenant đã cấu hình.
- Dòng log, label và thông báo lỗi là văn bản do bên thứ ba kiểm soát (ứng dụng, người dùng cuối của ứng dụng): chúng có thể chứa chỉ dẫn nhằm vào model. Chúng vào context ở vai kết quả tool; quyền của execution không đổi theo nội dung log.
- `query_metric` trả label của series; với `sum by (<label>)` trên label lấy từ nội dung log (`| json`, `| logfmt`, `| pattern`), giá trị label **là** nội dung log. Vì vậy nó được phân loại cùng mức với `query_logs`.

## 2. Cấu hình

| Biến | Bắt buộc | Ý nghĩa |
|---|---|---|
| `HQ_LOKI_URL` | Có | Địa chỉ gốc của Loki (query frontend hoặc gateway của Loki), ví dụ `https://loki.example:3100` (kèm path prefix nếu có). Không được chứa credential, query hay fragment. **Bắt buộc `https` khi có bearer token hoặc khi `HQ_ENV` không phải `local`** |
| `HQ_LOKI_ORG_ID` | Có | Id của organization (của MCP Platform) sở hữu Loki này. Tool call thay mặt org khác bị từ chối |
| `HQ_SERVICE_SIGNING_KEY` | Có | Khoá ký ngữ cảnh dùng chung với Gateway (hex, ≥ 32 byte) |
| `HQ_LOKI_BEARER_TOKEN_FILE` | Không | Đường dẫn file chứa bearer token gửi cho Loki (hoặc proxy xác thực đứng trước nó): đúng một token trên một dòng. File rỗng, toàn khoảng trắng, nhiều dòng hoặc có ký tự điều khiển làm process từ chối khởi động |
| `HQ_LOKI_TENANT_ID` | Không | Tenant của Loki, gửi trong header `X-Scope-OrgID` ở mọi request. Bắt buộc nếu Loki chạy `auth_enabled: true` (mặc định của Loki). Chữ, số và `!-_.*'()`, tối đa 150 ký tự. Có `\|` (nhiều tenant) thì bị từ chối trừ khi bật cờ dưới đây |
| `HQ_LOKI_ALLOW_MULTI_TENANT` | Không | `1` cho phép `HQ_LOKI_TENANT_ID` chứa nhiều tenant ngăn bằng `\|` (truy vấn liên tenant). Mặc định tắt; chỉ nhận `1` hoặc `0` |
| `HQ_HTTP_ADDR` | Không | Địa chỉ lắng nghe, mặc định `:8092` (`make run-loki-mcp` lấy từ `HQ_LOKI_MCP_HTTP_ADDR`) |
| `HQ_ENV` | Có | `local`, `staging` hoặc `production`. **Phải đặt tường minh**: không đặt thì integration từ chối khởi động (khác các process khác, vốn mặc định `local`), vì việc bắt buộc `https` phụ thuộc vào nó |

`HQ_LOKI_URL`, token và `HQ_LOKI_TENANT_ID` là cấu hình của **người vận hành managed server**, không phải giá trị do tenant nhập: URL không đi qua SSRF guard, và không tool nào nhận URL, host, token hay tenant. Server không theo redirect và không dùng proxy từ biến môi trường (`HTTP_PROXY`…), nên token, tenant và truy vấn chỉ tới đúng địa chỉ đã cấu hình. Đừng lấy giá trị các biến này từ dữ liệu của tenant.

**Hai khái niệm "org" khác nhau.** `HQ_LOKI_ORG_ID` là organization của platform (UUID), dùng để từ chối tool call của org khác. `HQ_LOKI_TENANT_ID` là tenant *bên trong Loki* (`X-Scope-OrgID`). Server không suy cái sau từ cái trước. `X-Scope-OrgID` **không phải credential**: theo tài liệu Loki, phân quyền không thuộc API của Loki, nên ai tới được Loki ở tầng mạng đều tự đặt được header này. Cách ly giữa các tenant của Loki cần một proxy xác thực đứng trước Loki, không phải header.

Loki cho phép `X-Scope-OrgID` chứa nhiều tenant ngăn bằng `|` (truy vấn liên tenant, khi Loki bật tính năng đó). Server **mặc định từ chối** dạng này; nó chỉ được nhận khi người vận hành đặt `HQ_LOKI_ALLOW_MULTI_TENANT=1`. Mọi tenant trong danh sách đều đọc được bởi org ở `HQ_LOKI_ORG_ID`: chỉ liệt kê tenant thuộc về org đó. Lúc khởi động process ghi log số tenant (`loki_tenants`), không ghi giá trị.

Tenant id không rời server: label `__tenant_id__` (Loki gắn vào kết quả truy vấn liên tenant) bị bỏ khỏi dòng log, series và `list_labels`; `list_label_values` từ chối label đó; trong thông báo lỗi 400 / 422 của Loki, tenant id được thay bằng `[tenant]`. Server không che được tenant id nằm trong *nội dung dòng log*.

Một process phục vụ đúng một Loki của đúng một org. Slug trong Registry là `loki` và cũng là đích (`aud`) của ngữ cảnh ký, nên hiện chưa chạy được hai Loki cho cùng một Gateway dưới hai slug khác nhau.

Chạy cục bộ (`make run-loki-mcp`, hoặc chạy thẳng binary) và đăng ký. Target đó chạy binary với môi trường sạch và chỉ chuyển các biến nó liệt kê: `.env` phải có `HQ_ENV`, và target phải chuyển `HQ_LOKI_ALLOW_MULTI_TENANT` nếu bạn dùng nhiều tenant:

```
make run-loki-mcp
hqadmin mcp-server register -slug loki -name "Loki" -endpoint http://localhost:8092/mcp -org <slug org>
hqadmin mcp-server discover -slug loki -org <slug org>
hqadmin mcp-tool classify -server loki -tool list_labels -risk read -enabled=true -org <slug org>
```

Đăng ký ở phạm vi org sở hữu. Tool mới phát hiện mặc định `destructive` và tắt; bật từng tool sau khi duyệt phân loại ở mục 4.

## 3. Quyền tối thiểu trên Loki

- Chỉ cần đọc ba đường dẫn ở mục 1. Nếu Loki đứng sau một gateway / reverse proxy, chỉ cho process này `GET` tới đúng ba đường dẫn đó; không cho `/loki/api/v1/push`, `/loki/api/v1/delete`, `/loki/api/v1/tail`, ruler hay các endpoint quản trị.
- Bearer token (nếu có proxy xác thực) chỉ cần quyền đọc log của đúng tenant ở `HQ_LOKI_TENANT_ID`. File token chỉ process này đọc được.
- Mạng: chỉ process `integration-loki` cần tới Loki; chỉ Gateway cần tới process này.

### Bắt buộc trước khi cho Agent dùng

- **Chi phí LogQL nằm ở Loki, không ở tool.** Tool giới hạn khoảng dữ liệu một biểu thức chạm tới (24 giờ, gồm cả cửa sổ `[ … ]`; xem "Bộ kiểm từ vựng" ở mục 1), số dòng, số điểm và kích thước *kết quả*, nhưng không giới hạn *lượng* dữ liệu trong 24 giờ đó: `{namespace=~".+"} |~ ".*"` hay `count_over_time({app=~".+"}[1h])` trên 23 giờ đều hợp lệ với tool và có thể quét rất nhiều dữ liệu. Integration chỉ có một trần thô: 4 request đồng thời tới Loki mỗi process (lời gọi vượt trần nhận lỗi `loki đang bận` ngay, không xếp hàng), và phản hồi thô tối đa 4 MiB (log) / 2 MiB (metric, danh sách). Loki phải được đặt giới hạn có chủ ý, tốt nhất là theo tenant (runtime `overrides`) cho tenant mà Agent dùng. Tên các cấu hình dưới đây đã đối chiếu với trang cấu hình chính thức của Loki (khối `limits_config`, bản `latest` ngày 2026-11-19); **ngữ nghĩa chi tiết và tương tác giữa chúng chưa được kiểm trên một Loki thật**, hãy đọc tài liệu của đúng phiên bản đang chạy:

  | Cấu hình (`limits_config`) | Mặc định theo tài liệu | Vì sao cần xem |
  |---|---|---|
  | `query_timeout` | `1m` | Dài hơn timeout HTTP 25 giây của server này. Chúng tôi **không chắc** Loki dừng một truy vấn ngay khi client đóng kết nối; nên đặt giá trị này không lớn hơn nhiều so với 25 giây cho tenant của Agent |
  | `max_query_length` | `30d1h` | **Lớp thứ hai của trần 24 giờ.** Trần khoảng thời gian mà một truy vấn được chạm tới, gồm cả cửa sổ range vector trong biểu thức; đặt quanh 24–25 giờ cho tenant của Agent |
  | `max_query_range` | `0s` (không giới hạn) | Trần cửa sổ `[range]` trong biểu thức |
  | `max_query_lookback` | `0s` (không giới hạn) | **Lớp thứ hai của trần 24 giờ**, và là thứ duy nhất chặn một cú pháp dịch thời gian mà bộ kiểm từ vựng không biết. Giới hạn truy vấn lùi về quá khứ bao xa; lưu ý tool cho phép `start` ở bất kỳ đâu trong quá khứ (chỉ độ rộng bị trần), nên giá trị này cũng quyết định Agent đọc được log cũ tới đâu |
  | `max_entries_limit_per_query` | `5000` | Trần `limit` của log query; tool gửi tối đa 1000 |
  | `max_query_series` | `500` | Trần số series của metric query; tool chỉ trả 50 nhưng Loki vẫn tính đủ |
  | `max_chunks_per_query` | `2000000` | Trần số chunk một truy vấn được đọc |
  | `max_query_bytes_read` | `0B` (không giới hạn) | Trần dữ liệu ước lượng một truy vấn được đọc |
  | `max_querier_bytes_read` | `150GB` | Như trên, cho từng truy vấn con sau khi tách |
  | `max_streams_matchers_per_query` | `1000` | Trần số stream khớp bộ chọn |
  | `max_query_parallelism` | `32` | Số truy vấn con chạy song song cho một truy vấn của tenant |
  | `required_labels` | rỗng | Bắt mọi truy vấn phải có matcher trên các label này; chặn được bộ chọn kiểu `{app=~".+"}` |

  Ngoài `limits_config`: `querier.max_concurrent` (mặc định `4`). Nên trỏ integration vào một tenant (hoặc một cụm đọc) riêng cho Agent, không dùng chung hạn mức với ruler / alerting đang chạy trên log.
- **Đường nối Gateway → integration phải là TLS hoặc mạng riêng.** Header ngữ cảnh do Gateway ký sống 2 phút và chưa gắn với body: ai nghe được một header loại `call` thì gọi được tool bất kỳ của server này thay mặt org đó trong thời gian ấy, tức là đọc được log. Ví dụ `http://localhost:8092` chỉ dành cho máy phát triển.
- **Đường nối integration → Loki là `https` ngoài `local`** (server từ chối khởi động với `http`): dòng log đi trên đường này. Đây là chỗ chặt hơn integration Prometheus.
- Thông báo lỗi của Loki chỉ vào kết quả tool với mã 400 / 422 (lỗi của chính truy vấn). Trước đó nó được che theo mẫu (userinfo và query string của URL, giá trị sau `Bearer` / `Basic`, cặp tên = giá trị có tên gợi ý credential như `password`, `secret`, `token`, `key`, `signature`, và tenant id) rồi cắt ở 512 byte. Che theo mẫu không phải bảo đảm: token nằm trong *đường dẫn* của một URL và địa chỉ nội bộ thì không được che. Với mọi mã lỗi khác (5xx, 429…) kết quả tool chỉ là một câu cố định kèm mã trạng thái; chi tiết (đã che như trên) nằm trong log của process (`loki api: lỗi phía Loki`). Body của phản hồi 401 / 403 không vào kết quả lẫn log.
- `start` / `end` phải nằm trong khoảng 2000-01-01 đến 2200-01-01.
- `list_labels` / `list_label_values` chỉ xét tối đa 24 giờ gần đây; theo tài liệu, Loki có thể dùng khoảng thời gian rộng hơn khoảng được yêu cầu.

## 4. Phân loại đề xuất

Đề xuất để admin duyệt; server không tự bật tool.

| Tool | `risk_level` | `side_effect` | `data_sensitivity` | `egress` | `cacheable` | `max_result_bytes` | `timeout` |
|---|---|---|---|---|---|---|---|
| `query_logs` | `read` | `none` | `sensitive` | `false` | `false` | 65536 | 30 s |
| `query_metric` | `read` | `none` | `sensitive` | `false` | `false` | 65536 | 30 s |
| `list_labels` | `read` | `none` | `internal` | `false` | `false` | 65536 | 30 s |
| `list_label_values` | `read` | `none` | `internal` | `false` | `false` | 65536 | 30 s |

Lý do:

- Mọi tool chỉ `GET`, không đổi trạng thái Loki (`read` / `none`).
- `query_logs` → `sensitive`: dòng log là văn bản tự do của ứng dụng, có thể chứa secret và dữ liệu cá nhân, và tool không lọc được. Chọn mức an toàn hơn; hệ quả mong muốn là execution đã gọi tool này mà gọi tiếp tool `egress` thì cần duyệt (kiểm soát luồng dữ liệu của Phase 4 chưa hiện thực, nên hiện phân loại này chưa được thực thi ở Gateway).
- `query_metric` → `sensitive`: label của series có thể được rút từ nội dung log bằng parser của LogQL, nên kết quả mang được nội dung log.
- `list_labels`, `list_label_values` → `internal`: tên và giá trị label của stream là metadata vận hành (tên service, namespace, pod). Nếu tổ chức đặt định danh khách hàng hay dữ liệu nghiệp vụ vào label của stream, nâng lên `sensitive`.
- `egress = false`: không tool nào gửi dữ liệu ra ngoài hạ tầng của org. `query` được gửi tới Loki của chính org.
- Không cache: log đổi liên tục và câu hỏi thường là "ngay lúc này"; cache kết quả `sensitive` cũng là thêm một nơi giữ nội dung log.
- Timeout 30 giây lớn hơn timeout HTTP 25 giây của server.

## 5. Lỗi thường gặp

| Triệu chứng | Nguyên nhân | Xử lý |
|---|---|---|
| Process không khởi động: `HQ_LOKI_URL is required` / `HQ_LOKI_ORG_ID is required` | Thiếu cấu hình | Đặt biến ở mục 2 |
| Process không khởi động: `bearer token chỉ được gửi qua https` / `ngoài môi trường local địa chỉ API phải là https` | URL là `http` trong khi có token hoặc `HQ_ENV` không phải `local` | Dùng `https` |
| Process không khởi động: `tenant id không hợp lệ` | `HQ_LOKI_TENANT_ID` có ký tự ngoài tập cho phép | Sửa giá trị; nếu Loki thật sự dùng tenant id có ký tự khác, báo lại để nới regex có chủ ý |
| Gateway nhận 401 từ server | Khoá ký khác nhau giữa Gateway và server, đồng hồ lệch quá 30 giây, hoặc slug đăng ký không phải `loki` | Đối chiếu `HQ_SERVICE_SIGNING_KEY`, NTP, slug |
| Gateway nhận 403 từ server | Tool call thay mặt org khác `HQ_LOKI_ORG_ID` | Đăng ký server ở đúng org; không đăng ký ở phạm vi platform |
| Tool lỗi `không gọi được Loki API` | Loki không tới được, TLS lỗi, hoặc quá 25 giây | Xem log của process (`loki api: request lỗi`); log không chứa biểu thức LogQL, dòng log hay token |
| Tool lỗi `loki từ chối credential của server` | Loki / proxy trả 401 hoặc 403 (thường là thiếu hoặc sai `HQ_LOKI_TENANT_ID`, hoặc token) | Kiểm tenant, file token và quyền của nó |
| Tool lỗi `status 3xx (redirect không được theo)` | `HQ_LOKI_URL` trỏ vào địa chỉ chuyển hướng (thiếu path prefix, http → https) | Sửa URL thành địa chỉ cuối cùng |
| Tool lỗi `loki: status 400: parse error …` | LogQL sai | Sửa biểu thức |
| Tool lỗi `loki: status 400: …` nhắc tới giới hạn (ví dụ khoảng thời gian, số series) | Truy vấn vượt một giới hạn của Loki ở mục 3 | Thu hẹp bộ chọn hoặc khoảng thời gian; không nới giới hạn chỉ vì Agent |
| Tool lỗi `loki: status 5xx: lỗi phía Loki; chi tiết nằm trong log của managed server` (hoặc 429…) | Loki quá tải, timeout, lỗi lưu trữ, vượt hạn mức | Xem log của process này (`loki api: lỗi phía Loki`, trường `detail`) và log của Loki |
| Tool lỗi `loki đang bận` | Đã có 4 request tới Loki đang chạy trong process này | Gọi lại sau; nếu thường xuyên, Loki đang chậm hoặc Agent gọi song song quá nhiều |
| Tool lỗi `query không được dùng modifier offset` / `@` / `subquery` / `biến`, `mỗi [ … ] ngoài chuỗi phải là một duration…`, `cửa sổ […] vượt quá 24 giờ`, `khoảng start–end … cộng cửa sổ … không được quá 24 giờ` | Bộ kiểm từ vựng (mục 1) từ chối biểu thức; không có gì được gửi tới Loki | Viết lại biểu thức trong giới hạn; label tên `offset` không dùng được qua tool này |
| Process không khởi động: `HQ_ENV is required` | `HQ_ENV` không được đặt | Đặt tường minh `local`, `staging` hoặc `production` |
| Process không khởi động: `tenant id chứa '\|'` | Nhiều tenant mà chưa bật cờ | Dùng một tenant, hoặc đặt `HQ_LOKI_ALLOW_MULTI_TENANT=1` sau khi chắc mọi tenant thuộc về org |
| Process không khởi động: `HQ_LOKI_BEARER_TOKEN_FILE: file rỗng…` / `token chứa ký tự điều khiển` | File token rỗng, toàn khoảng trắng, nhiều dòng hoặc có byte lạ | File phải chứa đúng một token trên một dòng |
| Tool lỗi `query_logs chỉ nhận LogQL log query…` / `query là một LogQL log query; dùng tool query_logs` | Dùng nhầm tool cho loại truy vấn | Dùng tool được nêu |
| Tool lỗi `phản hồi của Loki quá lớn` | Phản hồi thô quá 4 MiB (log: dòng rất dài × limit lớn) hoặc 2 MiB (metric: quá nhiều series; danh sách) | Giảm `limit`, thu hẹp bộ chọn, tổng hợp trong LogQL |
| `truncated: true` ở `query_logs` | Vượt 60 KiB hoặc `limit` | Đọc tiếp bằng `next_end` / `next_start`, hoặc lọc chặt hơn (`\|=`, label) |
| `truncated: true` ở `query_metric` | Vượt trần series, điểm hoặc 64 KiB | `sum by`, `topk`, tăng step, giảm khoảng |
| `list_label_values` trả rỗng | Label không có giá trị trong lookback, hoặc tên label sai | Tăng `lookback_hours` (tối đa 24); xem `list_labels` |

## 6. Chưa có

- Đã chạy đầu-cuối qua Gateway với một Loki thật (`grafana/loki:3.0.0`, `auth_enabled: false`, `http`; `tests/api/loki_e2e_test.go`). **Chưa chạy** với `auth_enabled: true` (header `X-Scope-OrgID`), bearer token, `https`, truy vấn liên tenant (label `__tenant_id__` mới chỉ kiểm với Loki giả lập), hay một Loki có dữ liệu lớn. Định dạng lỗi của Loki không được tài liệu API mô tả; server đọc được cả văn bản thuần lẫn JSON có trường `error` / `message`.
- Giải mã phản hồi theo luồng (hiện đọc cả body, tối đa 4 MiB, rồi mới giải mã); hàng đợi cho lời gọi vượt trần đồng thời (hiện từ chối ngay).
- Instant query (`/loki/api/v1/query`), `/loki/api/v1/series`, `/loki/api/v1/index/stats`, `/loki/api/v1/index/volume`, `detected_fields`, `patterns`, tail.
- Lọc `list_labels` / `list_label_values` theo bộ chọn stream (tham số `query` của Loki) và theo khoảng thời gian bất kỳ.
- Structured metadata của dòng log (không yêu cầu; nếu Loki trả thì bị bỏ qua).
- Ước lượng chi phí trước khi chạy (`index/stats`); parser LogQL thật (hiện chỉ có bộ kiểm từ vựng).
- Che secret trong dòng log ở integration.
- Nhiều Loki mỗi org; basic auth (Grafana Cloud / GEL dùng basic auth với tenant ở trường user: chưa hỗ trợ, chỉ có bearer token).
- Grafana (cùng đợt 4a, integration riêng).
