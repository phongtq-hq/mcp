# Runbook: managed MCP server GitHub (chỉ đọc)

Trạng thái: **Đề xuất** · Liên quan: [mcp-platform §6.3, §7](../architecture/mcp-platform.md), [ADR-0004](../adr/0004-mcp-gateway-single-egress.md), [roadmap Phase 4 (đợt 4a)](../roadmap.md)

Code: `internal/integration/github`, binary `cmd/integration-github`. Server chỉ có tool đọc. Agent gọi nó chỉ qua MCP Gateway.

## 1. Tool

| Tool | Việc | Giới hạn |
|---|---|---|
| `list_repositories` | Allowlist repository của server (`owner/repo`) | Không gọi GitHub; ≤ 200 repository |
| `list_pull_requests` | Pull request của một repository, mới tạo trước; lọc `state` (`open` / `closed` / `all`) | ≤ 50 mỗi trang (mặc định 20); không trả mô tả |
| `get_pull_request` | Một pull request: mô tả, trạng thái, số commit / file / dòng thêm / dòng xoá, tóm tắt review | Mô tả cắt ở 8 KiB; đọc ≤ 100 review, ≤ 30 người review (vượt thì `incomplete: true`); **không trả diff** |
| `list_issues` | Issue của một repository; lọc `state`, `labels` (mang tất cả các label) | ≤ 50 mỗi trang; ≤ 10 label lọc; pull request bị lọc ra và đếm ở `pull_requests_omitted` |
| `get_issue` | Một issue: nội dung, label, người được giao, số comment | Nội dung cắt ở 8 KiB; không trả comment |
| `list_workflow_runs` | Lần chạy workflow GitHub Actions; lọc `branch`, `status` | ≤ 50 mỗi trang; khi có lọc GitHub trả tối đa 1000 kết quả |
| `get_workflow_run` | Một lần chạy kèm job của lần thử gần nhất, kết luận từng job, tên step thất bại | ≤ 100 job, ≤ 10 step thất bại mỗi job; **không trả log** |
| `list_commits` | Commit, mới nhất trước; `ref` (branch / tag / SHA), `since` / `until` | ≤ 50 mỗi trang; thông điệp cắt ở 512 byte; **không trả email** |
| `get_commit_status` | Combined status và tóm tắt check run của một commit (theo SHA đầy đủ) | ≤ 50 status, ≤ 100 check run; không trả output của check hay URL chi tiết |

Mọi kết quả đã tuần tự hoá không quá 64 KiB (bằng `max_result_bytes` mặc định của Registry). Phân trang bằng `page` / `per_page`, ánh xạ thẳng sang GitHub; `next_page` có khi GitHub báo còn trang kế (header `Link`, `rel="next"`). Trang vượt trần bị cắt phần cuối và mang `truncated: true` (gọi lại với `per_page` nhỏ hơn); mục tự nó vượt trần bị bỏ và đếm ở `oversized_omitted`. Kết quả là struct tường minh: trường GitHub trả mà không nằm trong struct (URL, `diff_url`, email, tên runner, output của check…) không rời server.

Server chỉ gửi `GET`, chỉ tới các đường dẫn sau dưới `/repos/{owner}/{repo}` của repository trong allowlist: `/pulls`, `/pulls/{n}`, `/pulls/{n}/reviews`, `/issues`, `/issues/{n}`, `/actions/runs`, `/actions/runs/{id}`, `/actions/runs/{id}/jobs`, `/commits`, `/commits/{sha}/status`, `/commits/{sha}/check-runs`. Header: `Accept: application/vnd.github+json`, `X-GitHub-Api-Version: 2022-11-28`. Không có tool nào đọc nội dung file, diff, log, secret, cấu hình repository, hay ghi bất cứ thứ gì.

`owner` và `repo` được kiểm bằng regex, đối chiếu allowlist (không phân biệt hoa thường), rồi mới ghép vào đường dẫn bằng `url.PathEscape` theo cách viết đã cấu hình. Repository ngoài allowlist bị từ chối trước khi có request nào đi ra. Ngoài `owner` / `repo`, chỉ số nguyên (`number`, `run_id`) và SHA đầy đủ (40 hoặc 64 ký tự hex) được ghép vào đường dẫn; branch và ref chỉ đi trong query string.

## 2. Cấu hình

| Biến | Bắt buộc | Ý nghĩa |
|---|---|---|
| `HQ_GITHUB_REPOSITORIES` | Có | Allowlist `owner/repo`, phân tách dấu phẩy, tối đa 200. Không có wildcard: mỗi repository phải được liệt kê |
| `HQ_GITHUB_TOKEN_FILE` | Có | Đường dẫn file chứa token GitHub chỉ đọc (xem mục 3). Token không bao giờ vào kết quả tool, thông báo lỗi hay log |
| `HQ_GITHUB_ORG_ID` | Có | Id của organization (của platform) sở hữu tài khoản GitHub này. Tool call thay mặt org khác bị từ chối |
| `HQ_SERVICE_SIGNING_KEY` | Có | Khoá ký ngữ cảnh dùng chung với Gateway (hex, ≥ 32 byte) |
| `HQ_GITHUB_API_URL` | Không | Địa chỉ gốc của REST API. Mặc định `https://api.github.com`; GitHub Enterprise Server: `https://HOST/api/v3`. Bắt buộc `https` trừ khi `HQ_ENV=local`. Không được chứa credential, query hay fragment |
| `HQ_HTTP_ADDR` | Không | Địa chỉ lắng nghe, mặc định `:8094` (`make run-github-mcp` lấy từ `HQ_GITHUB_MCP_HTTP_ADDR`) |
| `HQ_ENV` | Có | `local`, `staging` hoặc `production` |

`HQ_GITHUB_API_URL`, token và allowlist là cấu hình của **người vận hành managed server**, không phải giá trị do tenant nhập: địa chỉ không đi qua SSRF guard, và không tool nào nhận URL, host hay token. Server không theo redirect và không dùng proxy từ biến môi trường (`HTTP_PROXY`…), nên token chỉ tới đúng địa chỉ đã cấu hình. Đừng lấy giá trị các biến này từ dữ liệu của tenant.

Một process phục vụ đúng một token GitHub + một allowlist của đúng một org. Slug trong Registry là `github` và cũng là đích (`aud`) của ngữ cảnh ký, nên hiện chưa chạy được hai tài khoản GitHub cho cùng một Gateway dưới hai slug khác nhau.

Chạy cục bộ (`make run-github-mcp`, hoặc chạy thẳng binary) và đăng ký:

```
HQ_ENV=local HQ_HTTP_ADDR=:8094 HQ_GITHUB_REPOSITORIES=acme/api,acme/web \
  HQ_GITHUB_TOKEN_FILE=/run/secrets/github-token HQ_GITHUB_ORG_ID=<id org> \
  HQ_SERVICE_SIGNING_KEY=<khoá> ./bin/integration-github
hqadmin mcp-server register -slug github -name "GitHub" -endpoint http://localhost:8094/mcp -org <slug org>
hqadmin mcp-server discover -slug github -org <slug org>
hqadmin mcp-tool classify -server github -tool list_pull_requests -risk read -enabled=true -org <slug org>
```

Đăng ký ở phạm vi org sở hữu. Tool mới phát hiện mặc định `destructive` và tắt; bật từng tool sau khi duyệt phân loại ở mục 4.

## 3. Quyền tối thiểu trên GitHub

Khuyến nghị **fine-grained personal access token** (hoặc token của một GitHub App), cấp cho đúng các repository trong allowlist, chỉ quyền đọc:

| Quyền repository (Read-only) | Cho tool |
|---|---|
| Metadata | Bắt buộc với mọi fine-grained token |
| Pull requests | `list_pull_requests`, `get_pull_request` |
| Contents | `get_pull_request` (GitHub yêu cầu thêm cho endpoint này), `list_commits` |
| Issues | `list_issues`, `get_issue` |
| Actions | `list_workflow_runs`, `get_workflow_run` |
| Commit statuses | `get_commit_status` (phần commit status) |

- **Check run:** endpoint `GET /repos/{owner}/{repo}/commits/{ref}/check-runs` không nằm trong bảng quyền của fine-grained personal access token trong tài liệu GitHub (đối chiếu 2026-11-19). Khi GitHub trả 403 cho phần này, `get_commit_status` vẫn trả commit status và đặt `check_runs_unavailable: true`. Cần check run thì dùng token của GitHub App có quyền *Checks: read*.
- Quyền *Contents: read* cho phép token đọc mã nguồn, dù server này không có tool nào đọc nội dung file. Allowlist của server hẹp hơn hay bằng phạm vi của token; **đặt cả hai**: token chỉ cấp cho các repository trong allowlist, để một lỗi ở server không mở repository khác.
- Không dùng classic token có scope `repo`: scope đó gồm cả quyền ghi.
- File token chỉ process này đọc được. Đổi token: ghi file mới rồi khởi động lại process.
- Mạng: chỉ process `integration-github` cần tới GitHub API; chỉ Gateway cần tới process này.

### Bắt buộc trước khi cho Agent dùng

- **Nội dung từ GitHub là dữ liệu không tin cậy (prompt injection).** Tiêu đề và thân của issue / pull request, thông điệp commit, tên branch, tên label, tên job / step, tiêu đề lần chạy workflow và mô tả của commit status đều do người ngoài kiểm soát: với repository công khai, bất kỳ ai có tài khoản GitHub cũng mở được issue. Văn bản đó vào context của model ở vai kết quả tool; nó có thể chứa chỉ dẫn giả ("bỏ qua hướng dẫn trước…"). Server cắt thân ở 8 KiB có đánh dấu (`body_truncated`) nhưng **không lọc nội dung**. Giới hạn hậu quả nằm ở platform: Agent đọc GitHub không nên đồng thời có tool ghi hoặc tool `egress` mà không cần duyệt.
- **Đường nối Gateway → integration phải là TLS hoặc mạng riêng.** Header ngữ cảnh do Gateway ký sống 2 phút và chưa gắn với body: ai nghe được một header loại `call` thì gọi được tool bất kỳ của server này thay mặt org đó trong thời gian ấy. Ví dụ `http://localhost:8094` chỉ dành cho máy phát triển.
- **Hạn mức của token là chung.** Mọi Agent của org dùng chung hạn mức rate limit của một token. Server tự giới hạn (mục "Giới hạn tần suất" bên dưới) nhưng không cache. `get_pull_request`, `get_workflow_run` và `get_commit_status` mỗi lần gọi tốn hai request.
- **Tóm tắt review không phải kết quả branch protection.** Trên repository công khai, bất kỳ tài khoản GitHub nào cũng gửi được review `APPROVED`. `reviews.approved` chỉ đếm người có `author_association` là `OWNER` / `MEMBER` / `COLLABORATOR` và approve trên commit đầu hiện tại (`commit_id` của review bằng `head.sha`); approval của người khác nằm ở `approved_by_outsiders`, approval trên commit cũ (hoặc không rõ commit) nằm ở `approved_stale`. Kể cả vậy, server không đọc quy tắc của repository (số approval bắt buộc, code owner, có huỷ approval khi push hay không): **đừng để Agent quyết định merge hay "đã được duyệt" từ các số này.** Với `incomplete: true` (hơn 100 review hoặc 30 người review) các số đếm chỉ tính phần đã đọc và có thể thiếu review mới nhất.
- **Mạng fork.** GitHub lưu commit của một repository và mọi fork của nó trong cùng một mạng: `get_commit_status` theo SHA, và `list_commits` bắt đầu từ một SHA, trả lời được cho commit chỉ tồn tại ở fork của người ngoài, dưới đường dẫn của repository trong allowlist. Thông điệp commit, tên tác giả và status / check run của commit đó do người ngoài (và CI của họ) kiểm soát; `head_sha` của pull request từ fork là commit như vậy. Server từ chối `refs/pull/*` ở tham số `ref` / `branch`, nhưng không phân biệt được một SHA thuộc branch của repository hay thuộc fork. Kết quả `success` của `get_commit_status` vì thế không chứng minh commit đã qua CI *của repository*.
- **Allowlist theo tên, không theo định danh.** `HQ_GITHUB_REPOSITORIES` ghi `owner/repo`. Nếu repository bị đổi tên hoặc chuyển chủ rồi người khác tạo lại một repository cùng tên (hoặc tài khoản `owner` bị xoá và tên bị đăng ký lại), server sẽ đọc repository MỚI đó với token của org. Phạm vi của fine-grained token (gắn theo repository cụ thể) là lớp chặn chính; khi repository đổi tên, GitHub trả redirect và server báo lỗi thay vì đi theo. Kiểm allowlist định kỳ và mỗi khi đổi tên / chuyển repository. Nợ: ghim `repository_id` và đối chiếu lúc khởi động.
- Chỉ thông báo lỗi 422 của GitHub được chuyển vào kết quả tool, sau khi che: token của server (nguyên văn, mã hoá phần trăm, base64 đứng một mình hoặc trong cặp Basic auth), chuỗi có tiền tố token GitHub (`ghp_`, `gho_`, `ghu_`, `ghs_`, `ghr_`, `github_pat_`), giá trị `Basic` / `Bearer` / `token` của header Authorization, query string và userinfo của URL; rồi cắt ở 256 byte. Với mọi status khác chỉ có số status. Việc che là theo mẫu: secret ở dạng khác (ví dụ token nằm trong *đường dẫn* của một URL) không được nhận ra.
- Repository riêng tư: tên repository, tên branch, login của thành viên là dữ liệu nội bộ; mã nguồn không được trả, nhưng tiêu đề / thân pull request có thể chép mã hoặc secret do người viết dán vào. Server không quét secret; việc che mẫu secret là của Gateway.

### Giới hạn tần suất

Server có ba lớp, đều trong bộ nhớ của process (mất khi khởi động lại, không chia sẻ giữa nhiều bản chạy):

- **Ngắt mạch sau rate limit của GitHub.** Khi GitHub trả 403 / 429 do rate limit, server ghi nhớ thời điểm được gọi lại: theo `x-ratelimit-reset` (primary) hoặc `retry-after` (secondary), trần 1 giờ, sàn 1 giây; GitHub không nêu thì 60 giây. Cho tới lúc đó **mọi** tool call cần GitHub bị từ chối cục bộ với cùng lỗi `github_rate_limited`, không request nào đi ra (tiếp tục gọi khi đang bị giới hạn có thể khiến GitHub khoá token). `list_repositories` vẫn chạy.
- **Hạn mức request của process.** Token bucket 60 request GitHub mỗi phút, burst 10 (hằng số trong code, chưa có biến cấu hình). Hết thì tool call bị từ chối ngay với lỗi `{"error":"server_rate_limited","kind":"request_budget","retry_after_seconds":…}`; server không ngủ chờ, không xếp hàng. Tool `get_*` tốn hai lượt; hết lượt giữa chừng thì cả tool call là lỗi.
- **Trần 4 request đồng thời** tới GitHub; request thứ năm chờ trong thời hạn 25 giây của tool call.

Server không tự thử lại trong bất kỳ trường hợp nào.

## 4. Phân loại đề xuất

Đề xuất để admin duyệt; server không tự bật tool.

| Tool | `risk_level` | `side_effect` | `data_sensitivity` | `egress` | `cacheable` | `max_result_bytes` | `timeout` |
|---|---|---|---|---|---|---|---|
| `list_repositories` | `read` | `none` | `internal` | `false` | `true` | 65536 | 30 s |
| `list_pull_requests` | `read` | `none` | `internal` | `false` | `false` | 65536 | 30 s |
| `get_pull_request` | `read` | `none` | `internal` | `false` | `false` | 65536 | 30 s |
| `list_issues` | `read` | `none` | `internal` | `false` | `false` | 65536 | 30 s |
| `get_issue` | `read` | `none` | `internal` | `false` | `false` | 65536 | 30 s |
| `list_workflow_runs` | `read` | `none` | `internal` | `false` | `false` | 65536 | 30 s |
| `get_workflow_run` | `read` | `none` | `internal` | `false` | `false` | 65536 | 30 s |
| `list_commits` | `read` | `none` | `internal` | `false` | `false` | 65536 | 30 s |
| `get_commit_status` | `read` | `none` | `internal` | `false` | `false` | 65536 | 30 s |

Lý do: mọi tool chỉ `GET`, không đổi trạng thái trên GitHub (`read` / `none`). Dữ liệu là thông tin phát triển nội bộ của org (`internal`); nếu allowlist có repository mà tiêu đề / thân issue thường mang dữ liệu khách hàng hoặc chi tiết lỗ hổng bảo mật, nâng `get_issue`, `get_pull_request` và hai tool liệt kê tương ứng lên `sensitive`. `egress = false`: tool chỉ gửi tới GitHub các tham số lọc ngắn đã kiểm (state, label, branch, thời điểm), không có trường văn bản tự do; lưu ý với github.com, bản thân request rời hạ tầng của org, nên nếu chính sách của org coi tên branch / label là dữ liệu không được ra ngoài thì đặt `egress = true`. Không cache, trừ `list_repositories` (kết quả chỉ đổi khi process khởi động lại): trạng thái pull request, lần chạy CI và check đổi liên tục và câu hỏi thường là "ngay lúc này". Timeout 30 giây lớn hơn trần 25 giây server đặt cho một tool call (mỗi request HTTP tối đa 20 giây).

## 5. Lỗi thường gặp

| Triệu chứng | Nguyên nhân | Xử lý |
|---|---|---|
| Process không khởi động: `HQ_GITHUB_REPOSITORIES is required` / `HQ_GITHUB_TOKEN_FILE is required` / `HQ_GITHUB_ORG_ID is required` | Thiếu cấu hình | Đặt biến ở mục 2 |
| Process không khởi động: `mục allowlist phải có dạng owner/repo, không wildcard` | Allowlist có mục sai dạng (`acme/*`, thiếu owner, có khoảng trắng bên trong) | Liệt kê từng repository |
| Process không khởi động: `địa chỉ API phải dùng https` | `HQ_GITHUB_API_URL` là `http` ngoài `local` | Dùng `https` |
| Gateway nhận 401 từ server | Khoá ký khác nhau giữa Gateway và server, đồng hồ lệch quá 30 giây, hoặc slug đăng ký không phải `github` | Đối chiếu `HQ_SERVICE_SIGNING_KEY`, NTP, slug |
| Gateway nhận 403 từ server | Tool call thay mặt org khác `HQ_GITHUB_ORG_ID` | Đăng ký server ở đúng org; không đăng ký ở phạm vi platform |
| Tool lỗi `repository không nằm trong allowlist của server` | Agent hỏi repository chưa được cấu hình | Thêm vào `HQ_GITHUB_REPOSITORIES` (và vào phạm vi token) rồi khởi động lại, nếu đúng là cần |
| Tool lỗi JSON `{"error":"github_rate_limited",…}` | GitHub trả 403 / 429 do rate limit. `kind: primary` kèm `reset_at` (hết hạn mức theo giờ); `kind: secondary` kèm `retry_after_seconds` nếu GitHub nêu | Server **không tự thử lại** và từ chối cục bộ mọi lời gọi cho tới hết `retry_after_seconds` (xem "Giới hạn tần suất"). Chờ; giảm tần suất gọi; tách token nếu nhiều Agent dùng chung |
| Tool lỗi `github từ chối token của server (401)` | Token sai, hết hạn hoặc bị thu hồi | Cấp token mới, ghi lại file, khởi động lại |
| Tool lỗi `token của server không có quyền … (403)` | Token thiếu quyền đọc cho loại dữ liệu đó | Bổ sung quyền ở mục 3 |
| Tool lỗi `không tìm thấy trên GitHub (404)` | Số pull request / issue / run không tồn tại, **hoặc** token không đọc được repository (GitHub trả 404 thay vì 403 cho repository riêng tư) | Kiểm số; kiểm repository có trong phạm vi token |
| Tool lỗi `github trả redirect và server không theo` | Repository đã đổi tên hoặc chuyển chủ (301), issue đã chuyển sang repository khác, hoặc `HQ_GITHUB_API_URL` trỏ vào địa chỉ chuyển hướng | Cập nhật allowlist theo tên mới; sửa URL thành địa chỉ cuối cùng |
| Tool lỗi `github trả 410` | Issue đã bị xoá, hoặc phiên bản API `2022-11-28` hết hỗ trợ (GitHub công bố tới 2028-03-10) | Kiểm repository; nâng `apiVersion` trong code khi tới hạn |
| Tool lỗi `github: status 422: …` | GitHub từ chối tham số (ví dụ `ref` không tồn tại ở một số endpoint). Thông báo của GitHub được chuyển tiếp sau khi che credential | Sửa tham số |
| Tool lỗi `github: status N (server không chuyển tiếp thông báo…)` | GitHub hoặc proxy đứng trước trả status khác (5xx, 451, 409…). Văn bản của upstream cố ý không vào kết quả tool | Xem log của process (`github api: status không mong đợi`, kèm đường dẫn và status); thử lại sau với 5xx |
| Tool lỗi JSON `{"error":"server_rate_limited",…}` | Hết hạn mức 60 request / phút của chính server | Chờ `retry_after_seconds`; giảm tần suất gọi của Agent |
| Tool lỗi `ref không hợp lệ (refs/pull/* không được nhận)` | Agent đưa ref ẩn của pull request | Dùng tên branch, tag hoặc SHA |
| Process không khởi động: `file không chứa token` / `token là bắt buộc` | File token rỗng hoặc chỉ có khoảng trắng | Ghi token vào file |
| Process không khởi động: `đường dẫn của địa chỉ API chỉ gồm…` / `không được chứa credential, query hay fragment` | `HQ_GITHUB_API_URL` có `?`, `#`, `%xx`, `//`, đoạn `.` / `..` | Dùng đúng dạng `https://HOST` hoặc `https://HOST/api/v3` |
| Tool lỗi `không gọi được GitHub API` | GitHub không tới được, TLS lỗi, hoặc quá thời hạn | Xem log của process (`github api: request lỗi`); log có đường dẫn (owner / repo), không có query hay token |
| Tool lỗi `phản hồi của GitHub quá lớn` | Phản hồi thô quá 8 MiB (trang nhiều pull request có thân rất dài) | Giảm `per_page` |
| `truncated: true` | Trang vượt 64 KiB sau khi tuần tự hoá | Gọi lại với `per_page` nhỏ hơn |
| `check_runs_unavailable: true` | Token không được đọc check run | Xem mục 3 |
| `mergeable` không có trong kết quả | GitHub chưa tính xong khả năng merge | Gọi lại sau |
| `list_issues` trả ít mục hơn `per_page` dù còn `next_page` | Pull request trong trang bị lọc ra (`pull_requests_omitted`) | Đọc tiếp `next_page` |

## 6. Chưa có

Tool ghi (comment, label, merge, chạy lại workflow): đợt sau, cần `idempotency_key` và approval. Comment của issue / pull request; danh sách file thay đổi và diff; nội dung file; log của job; tìm kiếm (`/search`); release, deployment, branch protection; `get_commit_status` theo tên branch (hiện chỉ nhận SHA đầy đủ); lọc `list_workflow_runs` theo `event` / `actor`; sắp xếp khác mặc định; nhiều tài khoản GitHub mỗi org; xác thực bằng GitHub App (token cài đặt tự xoay vòng); cache và conditional request (`ETag`) để tiết kiệm rate limit; biến cấu hình cho hạn mức request của process; hạn mức công bằng theo Agent / execution (hiện một Agent dùng hết hạn mức của cả org); ngắt mạch và hạn mức dùng chung giữa nhiều bản chạy; ghim `repository_id` cho allowlist; phân biệt commit của repository với commit của fork. Chưa chạy với GitHub thật, GitHub Enterprise Server, hay đầu-cuối qua Gateway.
