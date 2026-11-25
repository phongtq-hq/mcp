# Runbook: managed MCP server Prometheus (chỉ đọc)

Trạng thái: **Đề xuất** · Liên quan: [mcp-platform §6.3, §7](../architecture/mcp-platform.md), [ADR-0004](../adr/0004-mcp-gateway-single-egress.md), [roadmap Phase 4 (đợt 4a)](../roadmap.md)

Code: `internal/integration/prometheus`, binary `cmd/integration-prometheus`. Server chỉ có tool đọc. Agent gọi nó chỉ qua MCP Gateway.

## 1. Tool

| Tool | Việc | Giới hạn |
|---|---|---|
| `query_instant` | PromQL instant query tại một thời điểm | Biểu thức ≤ 2000 byte; ≤ 200 series (mặc định 50); ≤ 500 điểm mỗi series |
| `query_range` | PromQL range query | Khoảng ≤ 7 ngày; step ≥ 10 giây; ≤ 500 điểm mỗi series; ≤ 50 series (mặc định 20) |
| `list_alerts` | Alert đang `firing` / `pending` | ≤ 200 mỗi trang (mặc định 100); annotation cắt ở 512 byte |
| `list_targets` | Sức khoẻ scrape target đang hoạt động | ≤ 200 mỗi trang; chỉ trả label `job` và `instance` |
| `list_metric_names` | Tên metric có dữ liệu trong một giờ gần đây | ≤ 500 mỗi trang (mặc định 100); lọc theo tiền tố |

Mọi kết quả đã tuần tự hoá không quá 64 KiB (bằng `max_result_bytes` mặc định của Registry); phần vượt bị bỏ và `truncated` / `next_offset` báo điều đó. Query được gửi kèm `timeout=20s` để Prometheus tự huỷ; timeout HTTP của server là 25 giây.

Server chỉ gửi `GET` tới `/api/v1/query`, `/api/v1/query_range`, `/api/v1/alerts`, `/api/v1/targets`, `/api/v1/label/__name__/values`. Không có tool nào chạm admin API (xoá series, snapshot), remote write, `/-/reload` hay `/-/quit`.

PromQL là ngôn ngữ truy vấn chỉ đọc, nhưng `query_instant` / `query_range` đọc được **mọi** metric và label mà Prometheus này giữ. Nếu trong đó có dữ liệu không nên tới Agent, chặn ở phía Prometheus (một instance riêng, hoặc proxy lọc theo label), không trông vào tool.

`list_targets` cố ý không trả `discoveredLabels`, label ngoài `job` / `instance`, `scrapeUrl`, `globalUrl`: URL scrape và tham số `__param_*` có thể mang token. Trong `last_error`, query string và userinfo của URL bị che.

## 2. Cấu hình

| Biến | Bắt buộc | Ý nghĩa |
|---|---|---|
| `HQ_PROMETHEUS_URL` | Có | Địa chỉ gốc của Prometheus, ví dụ `http://prometheus:9090` (kèm route prefix nếu có). Không được chứa credential, query hay fragment |
| `HQ_PROMETHEUS_ORG_ID` | Có | Id của organization sở hữu Prometheus này. Tool call thay mặt org khác bị từ chối |
| `HQ_SERVICE_VERIFY_KEY` | Có | Khoá CÔNG KHAI kiểm ngữ cảnh của Gateway (Ed25519, hex 64 ký tự; `hqadmin service-key public`). Server từ chối khởi động nếu được cấp `HQ_SERVICE_SIGNING_KEY` (ADR-0018) |
| `HQ_PROMETHEUS_BEARER_TOKEN_FILE` | Không | Đường dẫn file chứa bearer token gửi cho Prometheus. Có token thì `HQ_PROMETHEUS_URL` bắt buộc là `https` |
| `HQ_HTTP_ADDR` | Không | Địa chỉ lắng nghe, mặc định `:8091` (`make run-prometheus-mcp` lấy từ `HQ_PROMETHEUS_MCP_HTTP_ADDR`) |
| `HQ_ENV` | Có | `local`, `staging` hoặc `production` |

`HQ_PROMETHEUS_URL` là cấu hình của **người vận hành managed server**, không phải URL do tenant nhập: nó không đi qua SSRF guard, và không tool nào nhận URL, host hay token. Server không theo redirect, nên token và truy vấn chỉ tới đúng địa chỉ đã cấu hình. Đừng lấy giá trị biến này từ dữ liệu của tenant.

Một process phục vụ đúng một Prometheus của đúng một org. Slug trong Registry là `prometheus` và cũng là đích (`aud`) của ngữ cảnh ký, nên hiện chưa chạy được hai Prometheus cho cùng một Gateway dưới hai slug khác nhau.

Chạy cục bộ và đăng ký:

```
make run-prometheus-mcp
hqadmin mcp-server register -slug prometheus -name "Prometheus" -endpoint http://localhost:8091/mcp -org <slug org>
hqadmin mcp-server discover -slug prometheus -org <slug org>
hqadmin mcp-tool classify -server prometheus -tool query_instant -risk read -enabled=true -org <slug org>
```

Đăng ký ở phạm vi org sở hữu. Tool mới phát hiện mặc định `destructive` và tắt; bật từng tool sau khi duyệt phân loại ở mục 4.

## 3. Quyền tối thiểu trên Prometheus

- Chỉ cần đọc HTTP API. Không bật `--web.enable-admin-api`, `--web.enable-remote-write-receiver` hay `--web.enable-lifecycle` vì server này; nếu Prometheus đã bật chúng, đặt một reverse proxy chỉ cho `GET` tới năm đường dẫn ở mục 1.
- Bearer token (nếu Prometheus đứng sau proxy xác thực) chỉ cần quyền đọc. File token chỉ process này đọc được.
- Mạng: chỉ process `integration-prometheus` cần tới Prometheus; chỉ Gateway cần tới process này.

### Bắt buộc trước khi cho Agent dùng

- **Giới hạn chi phí truy vấn nằm ở Prometheus, không ở tool.** Các trần của tool (khoảng 7 ngày, bước ≥ 10 giây, 500 điểm) là trần của *trục thời gian kết quả*, không phải của dữ liệu bị quét: một biểu thức như `count_over_time({__name__=~".+"}[365d])` hay một subquery bước 1 giây vẫn hợp lệ với tool. Prometheus phải được chạy với `--query.timeout`, `--query.max-samples` và `--query.max-concurrency` đặt có chủ ý. Nên trỏ integration vào một replica hoặc instance riêng cho Agent, không vào Prometheus đang nuôi alerting. Integration chưa phân tích biểu thức và chưa có trần truy vấn đồng thời của riêng nó.
- **Đường nối Gateway → integration phải là TLS hoặc mạng riêng.** Header ngữ cảnh do Gateway ký sống 2 phút và chưa gắn với body: ai nghe được một header loại `call` thì gọi được tool bất kỳ của server này thay mặt org đó trong thời gian ấy. Ví dụ `http://localhost:8091` chỉ dành cho máy phát triển.
- Integration không dùng proxy từ biến môi trường (`HTTP_PROXY`…): nó chỉ nối thẳng tới `HQ_PROMETHEUS_URL`.
- Thông báo lỗi và cảnh báo của Prometheus được che query string và userinfo của URL trước khi vào kết quả tool; token nằm trong *đường dẫn* của một URL và địa chỉ nội bộ thì không được che.
- Label, tên metric, annotation và thông báo lỗi là văn bản do bên thứ ba kiểm soát (exporter, ứng dụng được scrape): chúng là dữ liệu không tin cậy khi vào context của model. Vì hai tool query đọc được mọi metric, cân nhắc phân loại chúng `data_sensitivity = sensitive` nếu Prometheus chứa metric nghiệp vụ.

## 4. Phân loại đề xuất

Đề xuất để admin duyệt; server không tự bật tool.

| Tool | `risk_level` | `side_effect` | `data_sensitivity` | `egress` | `cacheable` | `max_result_bytes` | `timeout` |
|---|---|---|---|---|---|---|---|
| `query_instant` | `read` | `none` | `internal` | `false` | `false` | 65536 | 30 s |
| `query_range` | `read` | `none` | `internal` | `false` | `false` | 65536 | 30 s |
| `list_alerts` | `read` | `none` | `internal` | `false` | `false` | 65536 | 30 s |
| `list_targets` | `read` | `none` | `internal` | `false` | `false` | 65536 | 30 s |
| `list_metric_names` | `read` | `none` | `internal` | `false` | `false` | 65536 | 30 s |

Lý do: mọi tool chỉ `GET`, không đổi trạng thái Prometheus (`read` / `none`). Metric, label, annotation và địa chỉ target là dữ liệu vận hành nội bộ (`internal`); nếu Prometheus của org chứa metric nghiệp vụ nhạy cảm (doanh thu, định danh khách hàng trong label), nâng hai tool query lên `sensitive`. Không tool nào gửi dữ liệu ra ngoài hạ tầng của org (`egress = false`). Không cache: dữ liệu giám sát đổi theo từng lần scrape và câu hỏi thường là "ngay lúc này". Timeout 30 giây lớn hơn timeout HTTP 25 giây của server.

## 5. Lỗi thường gặp

| Triệu chứng | Nguyên nhân | Xử lý |
|---|---|---|
| Process không khởi động: `HQ_PROMETHEUS_URL is required` / `HQ_PROMETHEUS_ORG_ID is required` | Thiếu cấu hình | Đặt biến ở mục 2 |
| Process không khởi động: `bearer token chỉ được gửi qua https` | Có file token nhưng URL là `http` | Dùng `https`, hoặc bỏ token nếu Prometheus không cần |
| Gateway nhận 401 từ server | Khoá công khai không ứng với khoá ký của Gateway, đồng hồ lệch quá 30 giây, header bị dùng lại hoặc body bị sửa sau khi ký, hoặc slug đăng ký không phải `prometheus` | Đối chiếu `hqadmin service-key public` với `HQ_SERVICE_VERIFY_KEY`, NTP, slug |
| Gateway nhận 403 từ server | Tool call thay mặt org khác `HQ_PROMETHEUS_ORG_ID` | Đăng ký server ở đúng org; không đăng ký ở phạm vi platform |
| Tool lỗi `không gọi được Prometheus API` | Prometheus không tới được, TLS lỗi, hoặc quá 25 giây | Xem log của process (`prometheus api: request lỗi`); log không chứa biểu thức PromQL hay token |
| Tool lỗi `prometheus từ chối credential của server` | Prometheus trả 401 / 403 | Kiểm file token và quyền của nó |
| Tool lỗi `status 3xx (redirect không được theo)` | `HQ_PROMETHEUS_URL` trỏ vào địa chỉ chuyển hướng (thiếu route prefix, http → https) | Sửa URL thành địa chỉ cuối cùng |
| Tool lỗi `prometheus: bad_data: …` / `execution` / `timeout` | Lỗi do Prometheus trả về cho biểu thức đó | Sửa biểu thức, thu hẹp khoảng thời gian |
| Tool lỗi `phản hồi của Prometheus quá lớn` | Phản hồi thô quá 8 MiB (query quá rộng, quá nhiều target) | Thu hẹp query, lọc `scrape_pool` / `prefix` |
| `truncated: true` | Vượt trần series, điểm hoặc 64 KiB | Tổng hợp trong PromQL (`sum by`, `topk`), tăng step, giảm khoảng |
| `list_metric_names` thiếu metric | Chỉ liệt kê metric có dữ liệu trong một giờ gần đây | Dùng `query_instant` với thời điểm cũ hơn |

## 6. Chưa có

Native histogram (điểm histogram bị bỏ, đếm ở `histograms_omitted`); rule và alert không hoạt động (`/api/v1/rules`); metadata của metric; nhiều Prometheus mỗi org; Loki và Grafana (cùng đợt 4a, integration riêng).
