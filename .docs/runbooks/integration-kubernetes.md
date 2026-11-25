# Runbook: managed MCP server Kubernetes (chỉ đọc)

Trạng thái: **Đề xuất** · Liên quan: [mcp-platform §6.3, §7](../architecture/mcp-platform.md), [ADR-0004](../adr/0004-mcp-gateway-single-egress.md), [roadmap Phase 4 (đợt 4a)](../roadmap.md)

Code: `internal/integration/kubernetes`, binary `cmd/integration-kubernetes`. Server chỉ có tool đọc. Agent gọi nó chỉ qua MCP Gateway.

Test của package dùng một kube-apiserver giả lập (`httptest`). Chạy đầu-cuối với k3s thật qua Gateway nằm ở `tests/api/kubernetes_e2e_test.go` (`HQ_TEST_K3S=1`).

## 1. Tool

| Tool | Việc | Giới hạn |
|---|---|---|
| `list_namespaces` | Namespace của cluster (hoặc đúng allowlist của server, khi phạm vi không phải `*`) | ≤ 40 mỗi trang (mặc định 20) |
| `list_pods` | Pod của một namespace: phase, container sẵn sàng, số restart, lý do đang chờ | ≤ 40 mỗi trang; `label_selector` ≤ 512 byte, ≤ 8 điều kiện dạng `key`, `!key`, `key=value`, `key!=value` |
| `get_pod` | Một pod: điều kiện (kèm thông điệp của cluster), từng container (image, trạng thái, lý do, mã thoát, tín hiệu, lần kết thúc trước, cổng, request / limit CPU và bộ nhớ), tên + loại volume. Thông điệp của container chỉ có cho vài lý do chờ (xem dưới) | ≤ 32 container, ≤ 64 volume, ≤ 16 điều kiện |
| `get_pod_logs` | Dòng log cuối của một container | `tail_lines` ≤ 1000 (mặc định 100); `since_seconds` ≤ 604800; log ≤ 60 KiB sau khi tuần tự hoá, giữ phần mới nhất |
| `list_deployments` | Deployment của một namespace: số replica mong muốn / cập nhật / sẵn sàng | ≤ 40 mỗi trang |
| `get_deployment` | Một deployment: replica, chiến lược, điều kiện rollout, tên + image container | ≤ 32 container |
| `list_events` | Event của một namespace, lọc theo đối tượng liên quan và loại. Chỉ trả event của 12 loại đối tượng (Pod, Deployment, ReplicaSet, StatefulSet, DaemonSet, Job, CronJob, Service, Ingress, Node, PersistentVolumeClaim, HorizontalPodAutoscaler); event của loại khác, kể cả Secret và ConfigMap, bị bỏ và đếm ở `filtered_out`. Khi phạm vi không phải `*`, event của Node cũng bị bỏ | ≤ 40 mỗi trang; thông điệp cắt ở 300 byte |
| `list_nodes` | Node: Ready, điều kiện bất thường, vai trò, phiên bản, dung lượng, taint | ≤ 40 mỗi trang; bị từ chối khi phạm vi namespace không phải `*` |

Mọi kết quả đã tuần tự hoá không quá 64 KiB (bằng `max_result_bytes` mặc định của Registry). Danh sách phân trang bằng con trỏ đục của apiserver (`cursor` ← `next_cursor`; apiserver cho nó hết hạn sau khoảng 5 phút, khi đó tool báo "cursor đã hết hạn"). Tên trường cố ý không chứa `token`: Gateway che giá trị của mọi trường JSON có tên khớp `password`, `passwd`, `pwd`, `secret`, `token`, `api_key`, `access_key`, `private_key`, `credential` (`internal/mcp/gateway/redact.go`), nên một `next_page_token` tới model dưới dạng `[REDACTED:secret_field]` và phân trang hỏng. Không trường input / output nào của server này khớp các mẫu đó; giữ điều này khi thêm trường. Một phần tử tự nó vượt 1500 byte bị bỏ và đếm ở `oversized_omitted`; 40 phần tử × 1500 byte luôn vừa một kết quả, nên không trang nào bị bỏ dở.

Server chỉ gửi `GET` tới:

```
/api/v1/namespaces
/api/v1/nodes
/api/v1/namespaces/{ns}/pods
/api/v1/namespaces/{ns}/pods/{name}
/api/v1/namespaces/{ns}/pods/{name}/log
/api/v1/namespaces/{ns}/events
/apis/apps/v1/namespaces/{ns}/deployments
/apis/apps/v1/namespaces/{ns}/deployments/{name}
```

Không có tool đọc Secret hay ConfigMap; không exec, attach, port-forward, proxy; không watch, không `follow` log. Tên namespace, pod, deployment, container được kiểm bằng regex DNS-1123 rồi ghép bằng `url.PathEscape`; `fieldSelector` của event do server dựng từ enum và tên đã kiểm.

**Trường không bao giờ rời server** (không được giải mã từ phản hồi của apiserver): `env` / `envFrom`, `command` / `args`, annotations, labels (trừ ba khoá chuẩn của node: `node-role.kubernetes.io/*`, `topology.kubernetes.io/zone`, `node.kubernetes.io/instance-type`), `managedFields`, nguồn của volume (tên Secret, đường dẫn host…: chỉ trả tên và loại), `volumeMounts`, probe, `imagePullSecrets`, giá trị của taint, danh sách image trên node, `state.terminated.message` của container (nội dung `/dev/termination-log`, do ứng dụng ghi), `status.message` của pod.

**Văn bản tự do của cluster vẫn được trả** (đã cắt, đã qua lớp che bên dưới): thông điệp của event (của các loại đối tượng được phép), `message` của condition (pod và deployment; thông điệp của deployment có thể chép lời từ chối của admission webhook), và `state.waiting.message` của container **chỉ khi** `reason` là `CrashLoopBackOff`, `ImagePullBackOff`, `ErrImagePull`, `ContainerCreating` hoặc `PodInitializing`. Với mọi `reason` khác (`CreateContainerConfigError`, `RunContainerError`…) chỉ trả `reason`: thông điệp của chúng chép tên Secret, tên key và dòng lệnh của container.

**Lớp che là giảm thiểu, không phải bảo đảm.** Trên các văn bản ở trên và trên thông báo lỗi của apiserver, server che những dạng có hình thù rõ: userinfo của URL (tới dấu `@` cuối), query string của URL, giá trị sau `Bearer` / `Basic`, JWT trần, giá trị của phép gán `key=value` / `key: value` / `--key=value` với tên chứa `password`, `passwd`, `pass`, `pwd`, `secret`, `token`, `api-key`, `access-key`, `private-key`, và chính token của server. Một secret không có hình thù (chuỗi trần trong câu, tên Secret trong `secret "x" not found` của một event FailedMount) đi qua nguyên vẹn. Đừng coi lớp này là lý do để mở namespace nhạy cảm.

**Thứ vẫn có thể nhạy cảm:** log của container (`get_pod_logs`) là dữ liệu do ứng dụng ghi, có thể chứa token hay dữ liệu khách hàng. Server không lọc nội dung log (có test ghi nhận hành vi này); việc che mẫu secret là của Gateway và cũng chỉ là giảm thiểu. Xem phân loại ở mục 4. Tên image có thể lộ địa chỉ registry nội bộ. Ngoài ra:

- **`label_selector` là một oracle cho label.** Server không trả label, nhưng model dò được một label (và giá trị của nó) có tồn tại trên pod hay không bằng cách lọc rồi xem kết quả có rỗng không. Đừng để dữ liệu bí mật trong label.
- **Log của chính pod integration đọc được** nếu process này chạy trong một namespace thuộc phạm vi. Log của nó không chứa token, nhưng chứa tên pod / namespace đã được hỏi và thông báo RBAC. Chạy integration trong namespace riêng, ngoài phạm vi.
- **Phần tử bị bỏ vì quá cỡ không phải lúc nào cũng đọc lại được.** Pod và deployment đọc riêng được bằng `get_pod` / `get_deployment`; node và event thì không (không có `get_node` / `get_event`): chúng chỉ được đếm ở `oversized_omitted`.

## 2. Cấu hình

| Biến | Bắt buộc | Ý nghĩa |
|---|---|---|
| `HQ_KUBERNETES_URL` | Có | Địa chỉ gốc của kube-apiserver, ví dụ `https://10.0.0.1:6443` (kèm tiền tố đường dẫn nếu đi qua proxy quản lý cluster). Phải là `https`; `http` chỉ được nhận khi `HQ_ENV=local` (ví dụ `kubectl proxy`). Không được chứa credential, query hay fragment |
| `HQ_KUBERNETES_ORG_ID` | Có | Id của organization sở hữu cluster này. Tool call thay mặt org khác bị từ chối |
| `HQ_KUBERNETES_BEARER_TOKEN_FILE` | Có với `https` | Đường dẫn file chứa token của service account. Với `http` thì phải bỏ trống (token không bao giờ đi qua kết nối không mã hoá) |
| `HQ_KUBERNETES_CA_FILE` | Không | CA (PEM) để xác minh chứng chỉ của apiserver. Bỏ trống thì dùng CA của hệ thống. Không có lựa chọn bỏ qua xác minh TLS |
| `HQ_KUBERNETES_NAMESPACES` | **Có** | Phạm vi namespace. Hoặc một allowlist phân tách bằng dấu phẩy (tối đa 32 tên): mọi tool bị giới hạn trong đó, `list_namespaces` trả đúng danh sách này, `list_nodes` và event của Node bị từ chối. Hoặc đúng một ký tự `*`: mọi namespace mà service account đọc được. Không đặt, để trống, giá trị hỏng, hoặc `*` trộn với tên khác làm process không khởi động: thiếu cấu hình không bao giờ thành "mọi namespace" |
| `HQ_SERVICE_VERIFY_KEY` | Có | Khoá CÔNG KHAI kiểm ngữ cảnh của Gateway (Ed25519, hex 64 ký tự; `hqadmin service-key public`). Server từ chối khởi động nếu được cấp `HQ_SERVICE_SIGNING_KEY` (ADR-0018) |
| `HQ_HTTP_ADDR` | Không | Địa chỉ lắng nghe, mặc định `:8093` (`make run-kubernetes-mcp` lấy từ `HQ_KUBERNETES_MCP_HTTP_ADDR`) |
| `HQ_ENV` | Có | `local`, `staging` hoặc `production` |

Các biến `HQ_KUBERNETES_*` là cấu hình của **người vận hành managed server**, không phải giá trị do tenant nhập: chúng không đi qua SSRF guard, và không tool nào nhận URL, host, token hay kubeconfig. Server không theo redirect và không dùng proxy từ biến môi trường (`HTTPS_PROXY`…), nên token chỉ tới đúng địa chỉ đã cấu hình. Đừng lấy giá trị các biến này từ dữ liệu của tenant.

Lúc khởi động process ghi một dòng log `integration-kubernetes: phạm vi namespace` nêu phạm vi đang áp dụng (số namespace và tên, hoặc "mọi namespace"); dòng này không chứa token.

Token được đọc **một lần lúc khởi động**. Token chiếu (projected) của pod xoay vòng khoảng mỗi giờ sẽ hết hạn trong khi process vẫn giữ bản cũ: dùng token có thời hạn dài hơn chu kỳ khởi động lại của process (`kubectl create token <sa> --duration=…`, hoặc Secret loại `kubernetes.io/service-account-token`), và khởi động lại process khi xoay token.

Một process phục vụ đúng một cluster của đúng một org. Slug trong Registry là `kubernetes` và cũng là đích (`aud`) của ngữ cảnh ký, nên hiện chưa chạy được hai cluster cho cùng một Gateway dưới hai slug khác nhau.

Chạy cục bộ (`make run-kubernetes-mcp`, hoặc chạy thẳng binary) và đăng ký:

```
make run-kubernetes-mcp
hqadmin mcp-server register -slug kubernetes -name "Kubernetes" -endpoint http://localhost:8093/mcp -org <slug org>
hqadmin mcp-server discover -slug kubernetes -org <slug org>
hqadmin mcp-tool classify -server kubernetes -tool list_pods -risk read -enabled=true -org <slug org>
```

Đăng ký ở phạm vi org sở hữu. Tool mới phát hiện mặc định `destructive` và tắt; bật từng tool sau khi duyệt phân loại ở mục 4.

## 3. Quyền tối thiểu trên cluster

Một service account riêng cho server này, chỉ `get` / `list`, không `watch`, không Secret, không ConfigMap, không `pods/exec`:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: hq-mcp-readonly}
rules:
  - apiGroups: [""]
    resources: [namespaces, nodes, pods, pods/log, events]
    verbs: [get, list]
  - apiGroups: [apps]
    resources: [deployments]
    verbs: [get, list]
```

Khi `HQ_KUBERNETES_NAMESPACES` là một allowlist (khuyến nghị), **đừng** gắn ClusterRole này bằng ClusterRoleBinding: tạo một `Role` (chỉ `pods`, `pods/log`, `events`, `deployments`) và `RoleBinding` trong từng namespace được phép. Allowlist của server là lớp thứ hai; RBAC của cluster mới là ranh giới thật. Với cấu hình đó server không cần quyền nào ở cấp cluster. Chỉ đặt `*` khi mọi namespace mà service account đọc được đều chấp nhận được cho Agent, kể cả namespace tạo ra sau này.

Không dùng token của `cluster-admin`, kubeconfig của người, hay service account `default`.

### Bắt buộc trước khi cho Agent dùng

- **Đường nối Gateway → integration phải là TLS hoặc mạng riêng.** Header ngữ cảnh do Gateway ký sống 2 phút và chưa gắn với body: ai nghe được một header loại `call` thì gọi được tool bất kỳ của server này thay mặt org đó trong thời gian ấy. Ví dụ `http://localhost:8093` chỉ dành cho máy phát triển.
- **Log là dữ liệu của ứng dụng.** Nếu namespace chứa workload ghi dữ liệu nhạy cảm ra log, không đưa namespace đó vào allowlist, hoặc không bật `get_pod_logs`.
- Tên pod, thông điệp event, lý do của container và log là văn bản do bên thứ ba kiểm soát (ứng dụng, người có quyền tạo pod): chúng là dữ liệu không tin cậy khi vào context của model.
- **Tải lên apiserver.** Mỗi tool call là một request `list` / `get`. Một process gửi tối đa 8 request đồng thời tới apiserver; request vượt trần chờ tối đa 5 giây rồi nhận lỗi tool "server đang bận". Trần này là của một process, không thay cho API Priority and Fairness của cluster: đặt nó cho service account này nếu cluster nhạy tải.
- **Log không gửi `limitBytes`.** Theo tài liệu của Kubernetes, `limitBytes` là "số byte đọc từ server trước khi kết thúc luồng log": nó cắt phần CUỐI của đoạn đã chọn, tức giữ dòng cũ và bỏ dòng mới nhất, ngược với điều tool hứa. Thay vào đó server gửi `tailLines` (≤ 1000), đọc luồng tới tối đa 8 MiB (quá thì lỗi tool, không trả phần cũ) và giữ 256 KiB cuối trước khi cắt về 60 KiB. Hệ quả: kubelet vẫn phải gửi tới 8 MiB cho một lần gọi xấu nhất.
- File token và file CA chỉ process này đọc được.

## 4. Phân loại đề xuất

Đề xuất để admin duyệt; server không tự bật tool.

| Tool | `risk_level` | `side_effect` | `data_sensitivity` | `egress` | `cacheable` | `max_result_bytes` | `timeout` |
|---|---|---|---|---|---|---|---|
| `list_namespaces` | `read` | `none` | `internal` | `false` | `false` | 65536 | 30 s |
| `list_pods` | `read` | `none` | `internal` | `false` | `false` | 65536 | 30 s |
| `get_pod` | `read` | `none` | `internal` | `false` | `false` | 65536 | 30 s |
| `get_pod_logs` | `read` | `none` | `sensitive` | `false` | `false` | 65536 | 30 s |
| `list_deployments` | `read` | `none` | `internal` | `false` | `false` | 65536 | 30 s |
| `get_deployment` | `read` | `none` | `internal` | `false` | `false` | 65536 | 30 s |
| `list_events` | `read` | `none` | `internal` | `false` | `false` | 65536 | 30 s |
| `list_nodes` | `read` | `none` | `internal` | `false` | `false` | 65536 | 30 s |

Lý do: mọi tool chỉ `GET`, không đổi trạng thái cluster (`read` / `none`). Trạng thái workload, tên image, địa chỉ IP nội bộ và cấu trúc cluster là dữ liệu vận hành nội bộ (`internal`). `get_pod_logs` là `sensitive` vì nội dung log do ứng dụng quyết định và server không lọc được nó; chọn mức an toàn hơn. Không tool nào gửi dữ liệu ra ngoài hạ tầng của org (`egress = false`). Không cache: trạng thái pod và event đổi liên tục và câu hỏi thường là "ngay lúc này"; con trỏ trang cũng hết hạn. Timeout 30 giây lớn hơn timeout HTTP 25 giây của server (danh sách được gửi kèm `timeoutSeconds=20`).

## 5. Lỗi thường gặp

| Triệu chứng | Nguyên nhân | Xử lý |
|---|---|---|
| Process không khởi động: `HQ_KUBERNETES_URL is required` / `HQ_KUBERNETES_ORG_ID is required` | Thiếu cấu hình | Đặt biến ở mục 2 |
| Process không khởi động: `địa chỉ API phải là https` | URL `http` ngoài `HQ_ENV=local` | Dùng `https` |
| Process không khởi động: `cần bearer token của service account` / `bearer token chỉ được gửi qua https` | `https` thiếu file token, hoặc `http` có file token | Đặt / bỏ `HQ_KUBERNETES_BEARER_TOKEN_FILE` cho khớp scheme |
| Process không khởi động: `file CA không chứa chứng chỉ PEM hợp lệ` | `HQ_KUBERNETES_CA_FILE` sai định dạng | Dùng `ca.crt` của cluster |
| Process không khởi động: `HQ_KUBERNETES_NAMESPACES: … phạm vi namespace là bắt buộc` | Biến không đặt hoặc trống | Đặt allowlist, hoặc `*` nếu thật sự muốn mọi namespace |
| Process không khởi động: `HQ_KUBERNETES_NAMESPACES: … tên không hợp lệ` | Allowlist có tên rỗng, viết hoa, ký tự lạ, hoặc `*` trộn với tên khác | Sửa danh sách |
| Process không khởi động: `HQ_KUBERNETES_BEARER_TOKEN_FILE: file rỗng, quá lớn hoặc không đọc được` | File token rỗng, toàn khoảng trắng, hoặc trên 16 KiB | Ghi lại token vào file |
| Gateway nhận 401 từ server | Khoá công khai không ứng với khoá ký của Gateway, đồng hồ lệch quá 30 giây, header bị dùng lại hoặc body bị sửa sau khi ký, hoặc slug đăng ký không phải `kubernetes` | Đối chiếu `hqadmin service-key public` với `HQ_SERVICE_VERIFY_KEY`, NTP, slug |
| Gateway nhận 403 từ server | Tool call thay mặt org khác `HQ_KUBERNETES_ORG_ID` | Đăng ký server ở đúng org; không đăng ký ở phạm vi platform |
| Tool lỗi `không gọi được Kubernetes API` | Apiserver không tới được, chứng chỉ không khớp CA, hoặc quá 25 giây | Xem log của process (`kubernetes api: request lỗi`); log không chứa token |
| Tool lỗi `kubernetes từ chối credential của server` | Apiserver trả 401: token sai hoặc đã hết hạn | Cấp token mới, khởi động lại process |
| Tool lỗi `service account … không có quyền đọc tài nguyên này` | Apiserver trả 403 (RBAC) | Xem log (`kubernetes api: không đủ quyền` kèm thông báo RBAC); bổ sung Role ở mục 3 |
| Tool lỗi `namespace không nằm trong danh sách được phép` | Namespace ngoài `HQ_KUBERNETES_NAMESPACES` | Đúng thiết kế; mở rộng allowlist nếu cần |
| Tool lỗi `server này bị giới hạn theo namespace…` | `list_nodes`, hoặc `list_events` với `involved_kind: Node`, khi phạm vi không phải `*` | Đúng thiết kế |
| Tool lỗi `server đang bận…` | Đã có 8 request đồng thời tới apiserver và không suất nào trống trong 5 giây | Thử lại; nếu lặp lại, xem apiserver có chậm không |
| `filtered_out` > 0 ở `list_events` | Trang có event của loại đối tượng không được trả (Secret, ConfigMap, CRD; Node khi có allowlist) | Đúng thiết kế |
| Phân trang trả `[REDACTED:secret_field]` thay cho con trỏ | Một trường mới có tên khớp mẫu che của Gateway | Đổi tên trường (xem mục 1) |
| Tool lỗi `cursor đã hết hạn` | Con trỏ của apiserver quá khoảng 5 phút (410) | Liệt kê lại từ đầu |
| Tool lỗi `không tìm thấy tài nguyên trong cluster` | 404: sai tên, sai namespace, hoặc pod đã bị thay | Liệt kê lại bằng `list_pods` |
| Tool lỗi `kubernetes: BadRequest: … is waiting to start` / `a container name must be specified` | Lỗi do apiserver trả cho yêu cầu log | Chọn `container`; dùng `previous` cho container đang crash |
| Tool lỗi `status 3xx (redirect không được theo)` | `HQ_KUBERNETES_URL` trỏ vào địa chỉ chuyển hướng | Sửa URL thành địa chỉ cuối cùng |
| Tool lỗi `phản hồi của Kubernetes quá lớn` | Phản hồi thô quá 8 MiB (pod có spec rất lớn), hoặc luồng log quá 8 MiB | Giảm `limit` / `tail_lines`, thêm `since_seconds` |
| Tool lỗi `phản hồi không đọc được` | Phản hồi không phải JSON của Kubernetes (trang đăng nhập của proxy), hoặc thứ đứng trước apiserver bỏ qua `limit` | Kiểm `HQ_KUBERNETES_URL` và proxy ở giữa |
| `oversized_omitted` > 0 | Một phần tử vượt 1500 byte sau khi tuần tự hoá | Pod / deployment: đọc riêng bằng `get_pod` / `get_deployment`. Node / event: không có cách đọc lại qua server này |
| `truncated: true` ở `get_pod_logs` | Log vượt 60 KiB sau khi tuần tự hoá | Giảm `tail_lines`, thêm `since_seconds` |

## 6. Chưa có

Tool ghi (scale, restart rollout, xoá pod: thuộc lát "ghi sau" của đợt 4a); StatefulSet, DaemonSet, Job, CronJob, Service, Ingress, HPA, PVC (mới chỉ lọc được event của chúng); metrics (`metrics.k8s.io`); liệt kê xuyên namespace; label selector dạng tập hợp (`in` / `notin`); lọc pod theo `fieldSelector`; đọc lại file token khi nó xoay vòng; xác thực bằng client certificate hay exec plugin của kubeconfig; nhiều cluster mỗi org; `get_node` / `get_event`; trần request đồng thời theo người khởi tạo (trần hiện tại là của cả process).
