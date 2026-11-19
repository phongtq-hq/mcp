---
name: new-integration
description: Lập kế hoạch và dựng một managed MCP server mới (integration với Docker, Kubernetes, GitHub, Prometheus, PostgreSQL, Shopify...) cho MCP Platform — chốt danh sách tool và phân loại rủi ro với người dùng trước, rồi giao hiện thực cho agent integration-builder và review bảo mật. Dùng khi bắt đầu một integration mới hoặc mở rộng đáng kể một integration đã có.
argument-hint: <tên hệ thống đích>
---

# Integration mới

Hệ thống đích: $ARGUMENTS

## 1. Kiểm tra thứ tự và điều kiện

- Đọc mục Phase 4 trong `.docs/roadmap.md`: integration này thuộc đợt nào (4a–4e)? Các điều kiện tiên quyết đã có chưa (Gateway pipeline, Registry, và approval nếu có tool ghi)?
- Tool ghi cần approval (Phase 2). Nếu chưa có approval, integration chỉ gồm tool đọc.
- HQS, X-Purchase, CPV, hệ thống thanh toán (đợt 4e): chưa có đặc tả API. Việc cần làm là khảo sát với đội sở hữu, không phải viết code. Nói rõ điều này và dừng.
- Server STDIO của cộng đồng: chỉ qua STDIO bridge (Phase 4), chỉ platform admin đăng ký. Integration tự viết dùng Streamable HTTP.

## 2. Thiết kế danh sách tool

Đọc `.docs/architecture/mcp-platform.md` §7 và `.claude/rules/integrations.md`. Dựa trên tài liệu API chính thức của hệ thống đích (tra cứu, không dựa trí nhớ), lập bảng:

| Tool | Việc làm | `risk_level` | `side_effect` | `data_sensitivity` | `egress` | Ghi chú |
|---|---|---|---|---|---|---|

Nguyên tắc: một tool một việc; đọc trước, ghi sau; không có tool nhận lệnh, script hay SQL tự do; tool ghi có `idempotency_key` và `dry_run` nếu hệ thống đích cho phép; phân vân thì chọn mức rủi ro cao hơn.

Ghi kèm: quyền tối thiểu cần cấp trên hệ thống đích, và loại credential (ưu tiên credential ngắn hạn do hệ thống đích cấp hơn secret tĩnh).

## 3. Xác nhận với người dùng

Trình bảng tool và phân loại, nêu những tool đã cố ý **không** làm và lý do. Phân loại rủi ro là quyết định của admin: chờ xác nhận trước khi hiện thực tool ghi. Tool đọc có thể tiến hành.

## 4. Hiện thực

Giao cho agent `integration-builder`, kèm: tên hệ thống đích, bảng tool đã chốt, đợt roadmap, đường dẫn integration hiện có để làm khuôn (nếu có), và những gì đã biết về môi trường test.

## 5. Review

Sau khi agent trả về, đọc diff thật (báo cáo của agent mô tả ý định, không thay cho việc kiểm tra). Gọi `security-reviewer` trên integration mới, tập trung: xử lý credential, xác minh header ngữ cảnh, khả năng thực thi lệnh tuỳ ý qua tham số, dữ liệu nhạy cảm trong kết quả.

## 6. Tiêu chí xong (roadmap Phase 4)

- [ ] Qua bộ test hợp đồng dùng chung
- [ ] Mọi tool đã phân loại rủi ro
- [ ] Tool ghi có idempotency, và `dry_run` nếu hệ thống đích cho phép
- [ ] Có runbook

Báo lại từng mục đã đạt hay chưa, kèm bằng chứng (lệnh đã chạy và kết quả).
