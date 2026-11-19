# ADR-0014: Tool nội bộ của runtime, và điểm quyết định của delegation nằm trong database

Trạng thái: Đề xuất · 2026-11-18

## Bối cảnh

[agent-runtime §5.4](../architecture/agent-runtime.md) yêu cầu: Agent không gọi Agent; nó gọi tool nội bộ `delegate_task`, và lời gọi đó "tới Orchestrator, nơi policy engine kiểm tra" (Agent đích, độ sâu, chu trình, số task, budget).

Hai quyết định đã chấp thuận nói về đúng chỗ này:

- [ADR-0004](0004-mcp-gateway-single-egress.md) §1: mọi tool call đi qua MCP Gateway, không có ngoại lệ.
- [ADR-0009](0009-in-process-policy-engine.md) §1, §3: policy engine là một package Go, với một hàm quyết định duy nhất.

Khi hiện thực delegation, câu hỏi là quyết định "có được giao việc hay không" chạy ở process nào. Vòng lặp của Agent chạy ở `worker`, process ít được tin nhất: nó xử lý output của model, và role database của nó (`app_worker`) cố ý không tạo được task. Việc tạo task con phải nguyên tử với các luật đếm (số task của execution, phần budget còn lại của task cha) và phải idempotent theo bước của task cha, vì bước có thể chạy lại sau khi worker chết.

## Quyết định

1. **Tool nội bộ của runtime** là tool mà việc thực hiện nó không có I/O nào ngoài database của chính platform. Tool nội bộ không đi qua MCP Gateway: không có gì "ra ngoài" để Gateway làm trung gian. Mọi tool khác vẫn theo ADR-0004, không ngoại lệ. Tool nội bộ có tên không chứa dấu chấm (tool của Registry luôn là `<server>.<tool>`), do runtime thêm vào danh sách tool của model, và danh sách các tool nội bộ nằm ở một chỗ trong `internal/agent/runtime`. `delegate_task` là tool nội bộ đầu tiên.
2. **Điểm quyết định của delegation là hàm database `agent_task_delegate`** (`SECURITY DEFINER`, migration 00023). Nó chạy dưới khoá của execution và hoặc tạo task con, hoặc trả về luật đã từ chối. Không role ứng dụng nào tạo được task con bằng đường khác. Đây là **ngoại lệ tường minh với ADR-0009 §3**: các luật của delegation không đi qua `authz.Decide`.
3. **Mọi thứ task con cần được chụp lúc tạo execution**, bởi process `api`, vào hai bảng chỉ thêm (`execution_delegation_sources`, `execution_delegation_targets`): Agent Version nào được giao việc cho Agent nào, version đang hoạt động của từng Agent đích, chuỗi model, giới hạn, budget. Hàm quyết định đọc ảnh chụp đó và các dòng task của execution; nó không cần Model Catalog hay bảng giá.
4. **Hàm tự ghi audit** cho mọi quyết định, cho phép lẫn từ chối, kèm luật đã khớp; actor là người khởi tạo của execution đọc từ database. Việc ghi dấu vết không phụ thuộc process đã gọi.
5. **Hàm đọc hai bảng của Agent Catalog** (`agents`, `agent_versions`), ở mức SQL: trạng thái và version đang hoạt động của Agent đích (tắt hay rollback một Agent là giảm quyền, phải có hiệu lực với task con chưa tạo), và system prompt của Agent Version đích (bất biến, nên không chụp vào ảnh chụp). Đây là ngoại lệ tường minh với luật "không join xuyên context" của [ADR-0001](0001-modular-monolith.md), giới hạn trong hàm này.
6. Lời gọi tool và model **của task con** vẫn đi qua Gateway và được kiểm lại ở mỗi lời gọi theo Agent Version đích và người khởi tạo. Delegation không tạo ra quyền nào Gateway không kiểm.

## Phương án đã cân nhắc

- **Worker tự quyết định bằng code Go rồi chèn task con.** Đơn giản nhất và luật viết bằng Go, nhưng process ít được tin nhất tự cấp việc cho mình, và phải bỏ rào "worker không tạo được task".
- **`delegate_task` là tool dựng sẵn của Gateway.** Dùng lại được xác thực execution token, rate limit, kiểm người khởi tạo, audit. Nhưng Gateway phải chứa logic tạo task và có quyền chèn vào `agent_tasks` (trái ADR-0004 §6: Gateway không chứa logic nghiệp vụ), luật phụ thuộc `mcp → agent` phải nới, và process giữ khoá gốc của secret có thêm bề mặt. Lợi ích thật nhỏ: thứ nó chặn thêm chỉ là việc tạo tối đa 16 dòng task, vì mọi lời gọi của task con đằng nào cũng qua Gateway.
- **Endpoint nội bộ ở `api`.** Luật viết bằng Go và resolve được Agent đích lúc giao việc. Nhưng thêm một đường mạng `worker → api`, thêm một loại token, và đưa `api` lên đường nóng của vòng lặp; vẫn cần khoá execution để nguyên tử.

## Hệ quả

- Luật của delegation viết bằng PL/pgSQL, có phiên bản theo migration, tenant không ghi đè được, và chỉ kiểm được bằng test trên PostgreSQL thật. Trần của platform (độ sâu, số task) xuất hiện ở cả hàm lẫn code Go; test phải giữ chúng bằng nhau.
- Hàm nhận tham số từ worker và tin `app.org_id` mà worker tự đặt. Nó không kiểm người khởi tạo còn hoạt động, không rate limit. Những gì hàm tự giữ: mỗi task con gắn với một step delegation đang mở của task cha; task cha không có task con nào khác đang mở (tuần tự là luật của điểm quyết định, không chỉ của vòng lặp); phần budget còn lại tính từ cây task.
- **Giới hạn đã biết với một worker bị chiếm quyền:** mức đã tiêu của một task (`estimated_cost`) do worker ghi, và bộ đếm budget ở Gateway theo từng task. Worker bị chiếm quyền có thể không ghi chi phí của task con rồi giao việc tiếp, làm tổng chi của một execution vượt budget của task gốc (tối đa 15 lần budget của Agent đích; budget theo organization / project / agent / user ở Gateway vẫn chặn). Sửa triệt để là để Gateway đặt trước vào bucket của cả chuỗi tổ tiên: nợ đã ghi ở [cost-management §4.4](../architecture/cost-management.md).
- Agent đích cố định version từ lúc execution bắt đầu: publish version mới của nó không ảnh hưởng execution đang chạy. Agent đích bị tắt, hoặc bị rollback khỏi version đã chụp, thì không được giao việc nữa.
- Mức sàn đi kèm ở Gateway: tool không chỉ đọc do một task được giao việc gọi luôn cần người duyệt ([security §4.5](../architecture/security.md)). Không có nó, prompt injection ở một Agent chỉ đọc kéo được tool ghi của Agent mà nó giao việc cho.
- Tool nội bộ kế tiếp (`read_task_results`, agent-runtime §5.5) theo cùng quy tắc ở mục 1, không cần ADR riêng.

## Điều kiện xem xét lại

- Luật của delegation cần thuộc tính ngoài context Execution (ví dụ điều kiện ABAC theo người khởi tạo, phân loại dữ liệu): khi đó quyết định phải về một nơi có `authz`.
- Một tool nội bộ cần gọi ra ngoài database của platform: nó không còn là tool nội bộ, và phải đi qua Gateway.
- Số luật hoặc độ phức tạp của hàm vượt mức kiểm soát được bằng test bảng trên database.
