# Architecture Decision Records

Mỗi quyết định kiến trúc quan trọng có một ADR. Định dạng: Bối cảnh → Quyết định → Phương án đã cân nhắc → Hệ quả → Điều kiện xem xét lại.

Trạng thái hợp lệ: `Đề xuất`, `Chấp thuận`, `Thay thế bởi ADR-xxxx`, `Loại bỏ`. ADR đã chấp thuận không sửa nội dung; thay đổi quyết định bằng ADR mới.

| ADR | Quyết định | Trạng thái |
|---|---|---|
| [0001](0001-modular-monolith.md) | Modular monolith: một repo, ba process | Chấp thuận |
| [0002](0002-temporal-durable-execution.md) | Temporal cho durable execution, đưa vào từ Phase 2 | Chấp thuận |
| [0003](0003-defer-message-broker.md) | Hoãn NATS JetStream; dùng transactional outbox | Chấp thuận |
| [0004](0004-mcp-gateway-single-egress.md) | MCP Gateway là đường duy nhất tới tool; không chuyển tiếp token | Chấp thuận |
| [0005](0005-tenant-isolation-rls.md) | Shared schema + `org_id` + Row-Level Security | Chấp thuận |
| [0006](0006-agent-definition-vs-execution-lifecycle.md) | Tách lifecycle của Agent khỏi execution; version bất biến | Chấp thuận |
| [0007](0007-usage-ledger-from-phase-1.md) | Usage ledger từ Phase 1; tách ước tính và chi phí thực | Chấp thuận |
| [0008](0008-secrets-envelope-encryption.md) | Envelope encryption với KMS; hoãn Vault | Chấp thuận |
| [0009](0009-in-process-policy-engine.md) | Policy engine in-process bằng Go; hoãn OPA / Cedar | Chấp thuận |
| [0010](0010-rule-based-model-router.md) | Model Router theo luật và tier | Chấp thuận |
| [0011](0011-keycloak-identity-provider.md) | Keycloak tự vận hành làm IdP; membership và quyền nằm ở platform | Chấp thuận |
| [0012](0012-local-key-provider-before-cloud-kms.md) | Bọc khoá bằng provider cục bộ trước; Cloud KMS trước khi có credential production | Chấp thuận |
| [0013](0013-claude-cli-provider-for-local-development.md) | Claude làm provider đầu tiên; Claude CLI cho local, Anthropic API cho production | Đề xuất |
| [0014](0014-internal-runtime-tools-and-delegation-decision-in-database.md) | Tool nội bộ của runtime; điểm quyết định của delegation là hàm trong database | Đề xuất |
| [0015](0015-workflows-compile-to-planned-executions.md) | Workflow của người dùng biên dịch thành execution `planned`; định dạng định nghĩa; activity tạo lần chạy theo lịch ở `api` | Đề xuất |
| [0016](0016-react-web-console-with-build-step.md) | Giao diện web viết bằng React, có bước build; vẫn là file tĩnh nhúng vào `api`, CSP không đổi | Đề xuất |
| [0017](0017-single-host-docker-compose-deployment.md) | Triển khai trên một máy chủ bằng Docker Compose (không Kubernetes); Caddy làm cửa vào; Temporal tự vận hành với mTLS | Chấp thuận |

Quyết định còn mở, sẽ có ADR khi chốt: nhà cung cấp Cloud KMS (xem ADR-0012); khung đánh giá (eval) cho Agent. (Định dạng định nghĩa workflow: ADR-0015, còn Đề xuất.)
