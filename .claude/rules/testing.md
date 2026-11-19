---
paths:
  - "**/*_test.go"
  - "tests/**"
---

# Kiểm thử

- **Postgres thật** qua testcontainers cho mọi thứ chạm DB. Không mock DB: RLS, khoá ngoại ghép, ràng buộc exclusion chỉ kiểm được trên DB thật. Kết nối test dùng role ứng dụng (không `BYPASSRLS`), không dùng superuser.
- **Cross-tenant:** tạo hai org, thử truy cập chéo trên mọi endpoint (đọc, sửa, xoá, liệt kê). Kỳ vọng 404 và không rò dữ liệu qua danh sách. Endpoint mới thiếu test này thì build lỗi.
- **Policy:** test dạng bảng cho `authz.Decide`, gồm cả mức sàn của platform không bị org ghi đè.
- **Idempotency:** gọi lại cùng `Idempotency-Key`; retry activity không tạo `execution_step` hay `usage_record` trùng; tool ghi không chạy hai lần khi worker bị kill.
- **Temporal (Phase 2+):** test replay trong CI cho mọi thay đổi workflow; test huỷ lan tới task con trong thời hạn xác định.
- **Giới hạn:** test rằng `max_iterations`, `max_tool_calls`, deadline, budget, độ sâu delegation thực sự chặn. Test fan-out delegation bị chặn đúng trần.
- **SSRF:** loopback, private, link-local, `169.254.169.254`, IPv4 ánh xạ trong IPv6, redirect tới địa chỉ nội bộ, DNS rebinding.
- **Approval:** arguments bị sửa sau khi duyệt bị từ chối; approval dùng lại bị từ chối; hết hạn bị từ chối; người duyệt trùng người yêu cầu bị từ chối.
- **Integration:** qua bộ test hợp đồng dùng chung.
- **LLM và MCP server giả lập** có độ trễ cấu hình được; test không gọi provider thật.
- Test kiểm hành vi quan sát được, không kiểm chi tiết hiện thực. Không sửa test cho qua; nếu test sai, nói rõ vì sao.
