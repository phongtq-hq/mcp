# ADR-0017: Triển khai trên một máy chủ bằng Docker Compose; Caddy làm cửa vào; Temporal tự vận hành với mTLS

Trạng thái: Chấp thuận · 2026-11-19

## Bối cảnh

Phạm vi Phase 1–3 đã chạy đầu-cuối trên máy cục bộ, nhưng repo chỉ có file compose cho phát triển (`deployments/compose/`: Keycloak `start-dev`, Temporal lưu trong bộ nhớ, mật khẩu cố định). Tiêu chí "Xong khi" của Phase 1–3 đều cần một môi trường staging, và hai quyết định còn mở chặn việc đó: môi trường triển khai đầu tiên, và Temporal tự vận hành hay dịch vụ quản lý ([roadmap.md](../roadmap.md)).

Ràng buộc được nêu khi chốt: **không dùng Kubernetes; chỉ Docker và Docker Compose**; ưu tiên ít tài nguyên, dễ quản lý và đưa lên production được. [overview.md §5](../architecture/overview.md) đã ghi Phase 1–3 chạy được bằng Docker Compose trên một VM.

Code đã đặt sẵn một số điều kiện ngoài `local` mà phương án nào cũng phải thoả: kết nối Temporal phải có TLS (`durable.DialFromEnv`); `gateway` bắt buộc có Redis; `HQ_OIDC_DISCOVERY_URL` bị từ chối (process đọc khoá của IdP qua đúng địa chỉ trong `iss`); provider khoá cục bộ của Secrets bị từ chối khi `HQ_ENV=production` ([ADR-0012](0012-local-key-provider-before-cloud-kms.md)); image chạy thật là distroless, không có shell hay `curl`.

Chưa có số đo tải nào ([roadmap.md §Phase 6](../roadmap.md)); ADR này không cam kết dung lượng hay độ sẵn sàng.

## Quyết định

1. **Một máy chủ, một file compose.** Staging và production dùng cùng `deployments/production/compose.yaml`, khác nhau ở file env của máy chủ (`/etc/hq/hq.env`, quyền 600, không vào repo). File compose của phát triển giữ nguyên và tách biệt: hai file không kế thừa nhau.
2. **Một image, tag bất biến.** CI build một image chứa mọi binary và đẩy lên registry với tag là commit SHA (và `vX.Y.Z` khi gắn tag); không có `latest`. Máy chủ triển khai bằng `bin/hqctl deploy <tag>`; lùi lại là triển khai tag cũ.
3. **Thứ tự cập nhật do compose giữ:** `migrate` → `db-init` → `worker` → `gateway` → `api`, mỗi bước chờ bước trước xong hoặc khoẻ (`depends_on` + healthcheck). Mỗi binary của platform có lệnh tự kiểm `<binary> healthcheck` (gọi `/readyz` của chính nó) vì image không có công cụ nào khác.
4. **Caddy là cửa vào duy nhất** (thành phần hạ tầng mới): chỉ nó publish cổng 80 / 443, tự xin và gia hạn chứng chỉ. Định tuyến theo danh sách cho phép: tên miền ứng dụng → `api` (`/v1` và giao diện ở `/`, `/config.json`, `/assets/*`), tên miền MCP → `gateway` (chỉ `/mcp/v1/*` và metadata OAuth), tên miền đăng nhập → Keycloak (chỉ `/realms`, `/resources`). Đường nội bộ của `gateway`, `/healthz`, `/readyz` và console quản trị của Keycloak không ra Internet.
5. **Ranh giới quyền của ba process được giữ ở tầng mạng và biến môi trường:** không service nào dùng `env_file`, mỗi biến được nội suy cho đúng service được giữ nó; `worker` chỉ nằm trên mạng nội bộ không có đường ra Internet; proxy của Docker socket nằm trên mạng riêng chỉ có `integration-docker`.
6. **Thành phần có trạng thái đi kèm trong compose, bật theo profile** (`postgres`, `redis`, `temporal`, `idp`). Bỏ một profile và trỏ biến tương ứng ra ngoài là đủ để chuyển thành phần đó sang dịch vụ quản lý; code không biết khác biệt.
7. **Temporal tự vận hành** (trả lời quyết định còn mở): image `temporalio/server` một container, lưu trong hai database riêng trên cùng máy chủ PostgreSQL, schema do `temporalio/admin-tools` cùng phiên bản áp ở mỗi lần triển khai. Frontend bật mTLS bằng một CA nội bộ do `hqadmin temporal-certs init` cấp vào một volume; chỉ `api` và `worker` có chứng chỉ client. Platform nhận thêm `HQ_TEMPORAL_TLS_CA_FILE` / `HQ_TEMPORAL_TLS_SERVER_NAME`. Không triển khai Temporal UI.
8. **Keycloak đi kèm chạy ở chế độ production** ([ADR-0011](0011-keycloak-identity-provider.md) còn Đề xuất), lưu trong PostgreSQL, import realm không có user và không có password grant. Đây là mặc định, không phải ràng buộc: platform chỉ cần OIDC chuẩn.
9. **Sao lưu là `pg_dump` theo lịch của máy chủ** (`hqctl backup`), cùng `hqctl maintenance`; lịch đặt bằng cron / systemd timer của máy chủ.

## Phương án đã cân nhắc

- **Kubernetes (k3s một node, hoặc dịch vụ quản lý).** Có rolling update không gián đoạn và tự phục hồi tốt hơn, nhưng thêm một hệ thống phải học và vận hành, tốn tài nguyên nền, và bị loại bởi ràng buộc của người dùng. Vẫn là hướng của Phase 6 khi cần HA nhiều node.
- **Docker Swarm.** Có rolling update và secret gốc với cùng định dạng file, nhưng `depends_on` có điều kiện (thứ tự migration → worker → api) không có trong Swarm, và lợi ích chính của nó là nhiều node, thứ chưa cần.
- **Một file compose chung với phát triển, tách bằng profile hoặc file override.** Ít lặp hơn, nhưng cấu hình dev (mật khẩu cố định, cổng publish, `start-dev`) và cấu hình máy chủ nằm cạnh nhau, và một lệnh thiếu `-f` hay thiếu profile chạy nhầm bản kia. Lặp lại khoảng ba định nghĩa service là giá chấp nhận được.
- **Temporal Cloud.** Không phải vận hành, có HA, và code đã hỗ trợ sẵn (API key + TLS). Không chọn làm mặc định vì có phí hằng tháng (bảng giá hiện hành chưa được kiểm tra) và đưa metadata của workflow ra ngoài; vẫn chuyển sang được bằng cấu hình (quyết định 6).
- **Temporal tự vận hành không TLS trên mạng nội bộ của compose.** Đơn giản hơn, nhưng phải nới điều kiện TLS trong code, và mọi container trên mạng nội bộ ghi được vào Temporal, tức điều khiển được worker.
- **Traefik hoặc nginx làm cửa vào.** Traefik cấu hình bằng nhãn cần đọc Docker socket, thứ không được đặt cạnh container giữ khoá; nginx cần thêm một thành phần xin chứng chỉ.
- **Triển khai tự động bằng Watchtower hoặc CI đẩy qua SSH.** Watchtower cần Docker socket và không giữ thứ tự migration → worker → api; CI có quyền SSH vào máy chủ production là một đường tấn công mới. Triển khai giữ là thao tác có người chạy; tự động hoá để sau.
- **PostgreSQL quản lý ngay từ đầu.** An toàn hơn về sao lưu và failover; không chọn làm mặc định vì ràng buộc "chỉ Docker", nhưng là thay đổi đầu tiên nên làm khi có dữ liệu không được phép mất.

## Hệ quả

- Staging dựng được bằng một lệnh trên một máy; các tiêu chí "trên staging" của Phase 1–3 kiểm được.
- **Một máy chủ là một điểm hỏng duy nhất.** Mất máy là mất dịch vụ; mất đĩa là mất dữ liệu tới bản sao lưu gần nhất. Mỗi lần triển khai có vài giây `api` / `gateway` không trả lời (một replica, không rolling update); execution đang chạy không mất vì state nằm ở Temporal và PostgreSQL.
- PostgreSQL, Temporal và Keycloak dùng chung một máy chủ database: một sự cố của nó dừng cả ba. Bản sao lưu nằm cùng máy cho tới khi người vận hành chép nó đi nơi khác; chưa có diễn tập khôi phục.
- Temporal tự vận hành là thêm việc: nâng phiên bản (schema), số shard lịch sử không đổi được sau lần khởi động đầu, chứng chỉ nội bộ có hạn (chứng chỉ lá được cấp lại khi triển khai; CA phải đổi tay). mTLS thu hẹp khoản nợ "ai ghi được vào namespace Temporal": chỉ bên có chứng chỉ client ghi được, nhưng `api` và `worker` dùng chung một chứng chỉ, chưa tách quyền theo process.
- Lời gọi giữa các container trên mạng nội bộ (tới `gateway`, PostgreSQL, Redis, managed MCP server) không mã hoá; chấp nhận được trên một máy (đọc được mạng đó đã là root của máy), **không** chấp nhận được khi tách ra nhiều máy.
- Khoá và mật khẩu nằm trong một file env trên máy chủ và trong biến môi trường của container: ai có quyền Docker trên máy đọc được. Production vẫn bị chặn bởi provider Cloud KMS (ADR-0012): cho tới khi có nó, cụm production chạy được nhưng không lưu được credential của org.
- Trần bộ nhớ mặc định của các container chưa dựa trên số đo tải. Số đo duy nhất đã có là lúc **nhàn rỗi**, ngay sau khi dựng cụm đầy đủ trên máy lập trình viên (2026-11-19, `docker stats`): cả cụm khoảng 1 GiB, trong đó Keycloak khoảng 580 MiB, PostgreSQL khoảng 250 MiB, Temporal khoảng 120 MiB, ba process của platform cộng lại khoảng 30 MiB. Thành phần đáng cân nhắc nhất nếu cần giảm tài nguyên là Keycloak (dùng IdP sẵn có), không phải ba process.
- Đã kiểm trên máy lập trình viên ở `HQ_ENV=staging` (chứng chỉ của Caddy do CA cục bộ cấp thay cho ACME): dựng cụm từ đầu, mTLS với Temporal, đăng nhập qua Keycloak, gọi `/v1` và một tool Docker qua `gateway` từ ngoài, thứ tự cập nhật, lùi phiên bản, sao lưu. **Chưa kiểm:** ACME với tên miền thật, `HQ_ENV=production`, khôi phục từ bản sao lưu, một execution có gọi model, chạy nhiều ngày.
- Chưa có: metrics và cảnh báo, ký image / SBOM, kiểm thử khôi phục, giới hạn tốc độ ở cửa vào.

## Điều kiện xem xét lại

Khi cần triển khai không gián đoạn hoặc chịu được mất một máy; khi load test (roadmap Phase 6) cho thấy một máy không đủ; khi cần isolated runtime; khi công vận hành Temporal tự vận hành (nâng cấp, sự cố) vượt phí của dịch vụ quản lý; hoặc khi có dữ liệu production mà RPO của `pg_dump` theo lịch không đáp ứng.
