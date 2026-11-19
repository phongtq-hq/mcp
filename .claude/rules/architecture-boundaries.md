---
paths:
  - "internal/**"
  - "cmd/**"
  - "go.mod"
  - ".golangci.yml"
---

# Ranh giới bounded context

Nguồn: `.docs/architecture/overview.md` §2, §8 · ADR-0001.

## Bố cục

```
cmd/        api/  gateway/  worker/          (không có cmd/scheduler: dùng Temporal Schedules)
internal/
  platform/     identity/ tenancy/ authz/ secrets/ config/
  agent/        catalog/ runtime/ (runtime/durable: vòng lặp trên Temporal) orchestrator/ (phần thuần: kiểm kế hoạch, lập lịch) memory/
  mcp/          gateway/ registry/ transport/ client/
  workflow/     definitions/ (definitions/schedule: lịch chạy trên Temporal Schedules, ADR-0015) approval/ notify/
  ai/           provider/ modelcatalog/ router/ prompt/
  cost/         ledger/ pricing/ budget/ reconcile/
  integration/  <mỗi managed MCP server một thư mục>
  audit/  observability/
  shared/       ids/ errors/ httpx/ pgx/
```

Không tạo `planner/`, `modelrouter/`, `policy/`, `mcp/discovery`, `mcp/server`: chúng đã được gộp có chủ ý (overview §8).

Ngoài các context: `apiserver/`, `gatewayserver/`, `workerserver/` là composition root của từng process (không import lẫn nhau); `runtimewire/` dựng các bước của Agent Runtime cho `workerserver` và cho `apiserver` ở chế độ local; `adminops/` là đường xuyên tenant.

## Luật phụ thuộc

1. Context khác chỉ dùng interface công khai, **không import package `store`** của context khác.
2. Interface khai báo ở **phía bên dùng**, nhỏ, đúng những method bên dùng cần.
3. Không join SQL xuyên context. Ngoại lệ duy nhất: truy vấn báo cáo của Cost và Audit.
4. Chiều phụ thuộc theo bảng ở overview §2. `cost` không biết Agent hoạt động thế nào; `ai/router` không biết gì về Agent; `mcp/gateway` không chứa logic nghiệp vụ.
5. `shared/` không import bất kỳ context nào.
6. Phụ thuộc vào giao thức MCP và SDK bị cô lập trong `internal/mcp` (rủi ro R2).
7. Temporal SDK chỉ được import trong `internal/agent/runtime/durable`, `internal/workerserver` và `internal/workflow/definitions/schedule` (lịch chạy của workflow, ADR-0015 còn Đề xuất) (rủi ro R4); `cmd/gateway` không link Temporal. Kiểm bằng `depguard` và `tests/arch`.
8. `internal/workflow` không import `internal/agent/runtime` hay `internal/agent/catalog`: nó dùng Agent Runtime qua interface khai báo ở bên dùng (nối ở `apiserver`); chỉ kiểu `Plan` của `agent/orchestrator` được import. `cmd/worker` và `cmd/gateway` không link `internal/workflow/definitions`.

Luật 1 và 3 được kiểm bằng `depguard`. Khi thêm package mới, cập nhật cấu hình linter cùng lúc. **Không nới linter để code qua được**; nếu luật cản một thay đổi hợp lý, đó là tín hiệu cần ADR.

## Ba process

| Process | Được phép | Không được |
|---|---|---|
| `api` | Mã hoá secret, khởi tạo execution, truy vấn; poll task queue Temporal `api-internal` để tạo lần chạy workflow theo lịch (ADR-0015) | Giải mã secret; gọi MCP server; tin tham số nhận từ Temporal (chỉ nhận định danh, đọc lại từ database) |
| `gateway` | Giải mã secret, gọi upstream MCP | Chứa logic nghiệp vụ; giữ state theo session |
| `worker` | Gọi LLM, gọi `gateway` bằng execution token | Bất kỳ quyền nào với secret; gọi thẳng MCP server hay hệ thống đích |

Mỗi process dùng role DB riêng (`app_api`, `app_gateway`, `app_worker`). Wiring trong `cmd/*` phải phản ánh đúng bảng này: đừng inject `SecretStore` có `Get` vào `api` hay `worker`.

## Hạ tầng mới

Thư viện thông thường thì thêm bình thường. Thành phần hạ tầng mới (broker, vault, policy engine ngoài, vector store) cần ADR nêu vấn đề mà thành phần hiện có không giải quyết được.
