---
name: phase-check
description: Đánh giá tiến độ của một phase trong roadmap MCP Platform bằng cách đối chiếu từng hạng mục phạm vi và từng tiêu chí "Xong khi" với code, test và tài liệu thực tế trong repo. Dùng khi hỏi "Phase N xong chưa", "còn thiếu gì", "làm gì tiếp theo", hoặc trước khi chuyển sang phase kế tiếp.
argument-hint: [số phase, mặc định là phase hiện tại]
---

# Kiểm tra tiến độ phase

Phase cần kiểm tra: $ARGUMENTS (nếu để trống: xác định phase hiện tại từ trạng thái repo).

## Cách làm

1. Đọc mục phase tương ứng trong `.docs/roadmap.md`: danh sách **Phạm vi**, phần **Cố ý chưa làm**, và các tiêu chí **Xong khi**.

2. Với mỗi hạng mục phạm vi và mỗi tiêu chí, tìm **bằng chứng trong repo**: code hiện thực, test phủ nó, migration, cấu hình CI. Chạy test liên quan khi chạy được. Phân loại:

   - **Đạt:** có code và có test đang qua (hoặc bằng chứng tương đương đã kiểm).
   - **Một phần:** có code nhưng thiếu test, hoặc thiếu một nhánh mà thiết kế yêu cầu. Nêu cụ thể thiếu gì.
   - **Chưa có.**
   - **Không kiểm được từ repo:** tiêu chí cần môi trường thật (ví dụ "chạy một yêu cầu chẩn đoán trên staging", "có số đo độ trễ Gateway"). Nêu cần gì để kiểm.

   Sự tồn tại của một file hay một hàm không phải bằng chứng là tính năng hoạt động. Không suy "đạt" từ tên file.

3. Kiểm tra phần **Cố ý chưa làm**: có thứ nào đã bị làm sớm không? Đó là rủi ro phạm vi (R3), cần nêu.

4. Kiểm tra các bất biến mà phase đó bắt buộc (bảng `security.md` §11 liệt kê theo phase): ví dụ Phase 1 phải có OIDC, RBAC cơ bản, `org_id` + RLS, audit log, secret mã hoá, SSRF guard, policy tại Gateway, test cross-tenant.

5. Với Phase 0: ngoài khung repo, kiểm tra trạng thái từng ADR trong `.docs/adr/README.md` và sáu điểm cần xác nhận ở `overview.md` §9. ADR còn `Đề xuất` là việc của người dùng, không phải thứ có thể tự hoàn thành.

## Báo cáo

Mở đầu bằng một câu kết luận: phase đã xong, gần xong, hay còn xa, và điều lớn nhất còn chặn.

Sau đó một bảng:

| Hạng mục / tiêu chí | Trạng thái | Bằng chứng hoặc phần thiếu |
|---|---|---|

Kết thúc bằng **ba việc nên làm tiếp theo**, xếp theo thứ tự, ưu tiên thứ mở khoá cho thứ khác và thứ không thể backfill (usage, audit, cách ly tenant).

Không ước lượng thời lượng. Không tính phần trăm hoàn thành: các hạng mục không cùng kích thước, con số sẽ gây hiểu lầm.
