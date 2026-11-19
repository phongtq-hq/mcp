package runtime

import (
	"strings"
	"testing"

	"github.com/hqgroups/mcp/internal/ai/provider"
)

func TestCompact(t *testing.T) {
	big := "TRANG-THAI: ok " + strings.Repeat("x", 9000)
	var msgs []provider.Message
	var elidable []bool
	add := func(m provider.Message, ok bool) {
		msgs = append(msgs, m)
		elidable = append(elidable, ok)
	}
	add(provider.Message{Role: provider.RoleUser, Text: "yêu cầu"}, false)
	turn := func(id, text string, isErr, ok bool) {
		add(provider.Message{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: id, Name: "srv.read"}}}, false)
		add(provider.Message{Role: provider.RoleTool, ToolCallID: id, Text: text, IsError: isErr}, ok)
	}
	turn("c1", big, false, true)
	// Tín hiệu an toàn: bên gọi không đánh dấu được lược bỏ (từ chối, kết quả task con), và
	// kết quả lỗi không bao giờ bị lược bỏ dù có được đánh dấu.
	denied := "Tool call bị từ chối (" + strings.Repeat("lý do ", 400) + ")"
	turn("c2", denied, true, false)
	turn("c3", "<task_result agent=\"db\" status=\"completed\">"+strings.Repeat("y", 9000)+"</task_result>", false, false)
	turn("c4", big, true, true)
	turn("c5", big, false, true)
	turn("c6", big, false, true)
	turn("c7", big, false, true)

	// Trong ngân sách hoặc không có ngân sách: không đổi gì.
	for _, budget := range []int{0, -1, 1 << 20} {
		if out, n := compact(msgs, elidable, budget); n != 0 || &out[0] != &msgs[0] {
			t.Fatalf("budget %d: lược bỏ %d", budget, n)
		}
	}
	out, n := compact(msgs, elidable, 40<<10)
	// Vượt ngân sách (≈ 56 KB so với 40 KB): lược bỏ xuống ~70%, cũ nhất trước; chỉ kết quả
	// thành công của tool thường; hai lượt gần nhất giữ nguyên; bản gốc không bị sửa.
	if n != 2 || msgs[2].Text != big {
		t.Fatalf("lược bỏ %d kết quả, muốn 2 (c1, c5)", n)
	}
	for _, i := range []int{2, 10} { // c1, c5
		if !strings.HasPrefix(out[i].Text, "TRANG-THAI: ok ") || !strings.Contains(out[i].Text, "ĐÃ chạy xong") || len(out[i].Text) > 600 {
			t.Errorf("message %d: %q", i, out[i].Text[:min(len(out[i].Text), 80)])
		}
	}
	for name, i := range map[string]int{"từ chối": 4, "kết quả task con": 6, "kết quả lỗi": 8, "lượt gần nhất c6": 12, "lượt gần nhất c7": 14} {
		if out[i].Text != msgs[i].Text {
			t.Errorf("%s bị lược bỏ", name)
		}
	}
	// Ghi chú không bảo model gọi lại tool.
	if strings.Contains(strings.ToLower(out[2].Text), "gọi lại tool nếu") {
		t.Errorf("ghi chú gợi ý lặp lại hành động: %q", out[2].Text)
	}
	// Deterministic và đơn điệu: cùng đầu vào cho cùng kết quả.
	again, _ := compact(msgs, elidable, 40<<10)
	for i := range out {
		if out[i].Text != again[i].Text {
			t.Fatal("compact không deterministic")
		}
	}
	// Ngân sách nhỏ bất thường (giá trị đi qua Temporal) bị nâng lên sàn: không biến mọi
	// kết quả thành ghi chú.
	if _, n := compact(msgs[:3], elidable[:3], 1); n != 0 {
		t.Errorf("ngân sách 1 byte lược bỏ %d kết quả của một hội thoại nhỏ", n)
	}
	if got := contextBudget(200_000, 4096); got != (200_000-4096)*bytesPerToken {
		t.Errorf("contextBudget = %d", got)
	}
	if contextBudget(0, 4096) != 0 || contextBudget(1000, 4096) != 0 {
		t.Error("cửa sổ không biết hoặc nhỏ hơn phần trả lời phải cho ngân sách 0")
	}
}
