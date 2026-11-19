---
name: review-change
description: Review một thay đổi của MCP Platform bằng các agent chuyên trách (architecture-reviewer, security-reviewer, db-reviewer, runtime-reviewer, ui-reviewer), chỉ gọi những agent khớp với vùng code bị chạm, rồi tổng hợp và kiểm chứng phát hiện. Dùng sau khi hoàn thành một thay đổi đáng kể, trước khi commit hoặc mở PR.
argument-hint: [đường dẫn, commit range, hoặc để trống cho thay đổi chưa commit]
---

# Review thay đổi

Phạm vi: $ARGUMENTS (để trống: toàn bộ thay đổi chưa commit so với nhánh chính).

## 1. Xác định thay đổi

Lấy danh sách file bị chạm (`git status`, `git diff --stat`, hoặc đường dẫn được chỉ định). Repo chưa có git thì dùng các file vừa sửa trong phiên làm việc này, và nói rõ phạm vi đã dùng. Đọc diff để hiểu thay đổi làm gì trước khi giao việc.

## 2. Chọn agent theo vùng bị chạm

| Vùng | Agent |
|---|---|
| Package mới, import giữa các context, `cmd/*`, `go.mod`, cấu hình linter, thay đổi lớn | `architecture-reviewer` |
| `internal/platform/**`, `internal/mcp/**`, `internal/workflow/approval/**`, `internal/audit/**`, endpoint mới, xử lý URL hoặc input ngoài, `internal/integration/**` | `security-reviewer` |
| `migrations/**`, `*.sql`, `internal/**/store/**` | `db-reviewer` |
| `internal/agent/**`, `internal/workflow/**`, `cmd/worker/**`, `internal/cost/**`, `internal/ai/**` | `runtime-reviewer` |
| `web/**`, `internal/webui/**` (thêm `security-reviewer` nếu chạm `web/src/auth/**`, màn hình duyệt, hoặc `web/package.json`) | `ui-reviewer` |

Chỉ gọi agent có vùng bị chạm. Thay đổi chỉ sửa tài liệu hoặc một dòng không cần agent: tự đọc và nhận xét.

Gọi các agent đã chọn **song song trong một lượt**. Brief cho mỗi agent: thay đổi nhằm mục đích gì, danh sách file cần xem, phase hiện tại. Đưa cho agent code để đọc, **không đưa kết luận của bạn** về việc code đúng hay sai.

## 3. Kiểm chứng và tổng hợp

Báo cáo của agent là phát hiện cần kiểm, không phải sự thật:

- Mở đúng vị trí được dẫn và xác nhận vấn đề có thật. Bỏ phát hiện không tái hiện được trong code.
- Gộp phát hiện trùng giữa các agent.
- Phát hiện mâu thuẫn nhau: nêu cả hai, kèm căn cứ từ `.docs/`.

## 4. Báo cáo

Một danh sách duy nhất, nghiêm trọng nhất trước:

- **Chặn:** vỡ cách ly tenant, lộ secret, vượt approval, tool ghi chạy hai lần, vòng lặp không trần, phá ranh giới context, migration khoá bảng hoặc mất dữ liệu, giao diện đưa HTML thô vào DOM / cần nới CSP / phá bất biến màn hình duyệt.
- **Nên sửa**
- **Cần quyết định:** chỗ thiết kế và code mâu thuẫn hoặc thiết kế chưa nói.

Mỗi mục: `đường/dẫn:dòng`, điều gì sai, hậu quả cụ thể, hướng sửa. Nêu rõ agent nào đã chạy và vùng nào không được review.

Không tự sửa code trong skill này trừ khi người dùng yêu cầu. Nếu người dùng muốn sửa, sửa từ mức Chặn xuống và chạy lại test liên quan.
