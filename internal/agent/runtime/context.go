package runtime

import (
	"strconv"

	"github.com/hqgroups/mcp/internal/ai/provider"
)

// Quản lý context (agent-runtime §6). Hội thoại của một task được dựng lại từ
// execution_steps ở mỗi lượt model; khi nó vượt ngân sách của model, kết quả tool CŨ
// được thay bằng một ghi chú lược bỏ. Việc nén là deterministic (cùng các step cho cùng
// hội thoại), không gọi LLM, và không đổi gì trong database: step vẫn giữ kết quả đầy đủ.
// Tóm tắt bằng model (bước 2–3 của thiết kế) chưa có.

const (
	// keepRecentTurns là số lượt assistant gần nhất mà kết quả tool của chúng không bao
	// giờ bị lược bỏ: model vừa yêu cầu chúng và chưa kịp dùng.
	keepRecentTurns = 2
	// bytesPerToken là ước lượng thận trọng để đổi cửa sổ ngữ cảnh (token) sang byte.
	// Tokenizer cục bộ chỉ để ước lượng; số token thật lấy từ response của provider.
	bytesPerToken = 3
	// minElidable: kết quả nhỏ hơn mức này không bị lược bỏ (lược bỏ không tiết kiệm được gì).
	minElidable = 1024
	// keptPrefixRunes: phần đầu của một kết quả bị lược bỏ được giữ lại. Trạng thái của một
	// hành động (thành công, định danh vừa tạo) thường nằm ở đó.
	keptPrefixRunes = 200
	// minContextBudget: sàn của ngân sách. Giá trị đi qua Temporal; một giá trị nhỏ bất
	// thường không được biến mọi kết quả tool thành ghi chú.
	minContextBudget = 16 << 10
	// Khi vượt ngân sách thì lược bỏ xuống còn chừng này phần của nó, không phải "vừa đủ":
	// nếu không, mỗi lượt sau lại sửa thêm một message ở đầu hội thoại và prompt cache của
	// provider mất ở mọi lượt, đúng lúc hội thoại lớn nhất.
	compactTargetNum, compactTargetDen = 7, 10
)

// contextBudget là ngân sách hội thoại (byte) của một task: cửa sổ ngữ cảnh nhỏ nhất
// trong chuỗi model của nó, trừ phần dành cho câu trả lời. 0 = không biết, không nén.
func contextBudget(minContextWindow, maxOutputTokens int) int {
	if minContextWindow <= 0 {
		return 0
	}
	tokens := minContextWindow - maxOutputTokens
	if tokens <= 0 {
		return 0
	}
	return tokens * bytesPerToken
}

func messageBytes(m provider.Message) int {
	n := len(m.Text) + len(m.Opaque)
	for _, c := range m.ToolCalls {
		n += len(c.Name) + len(c.Arguments)
	}
	return n
}

// elide thay một kết quả tool bằng phần đầu của nó và một ghi chú. Ghi chú nói rõ tool
// call ĐÃ chạy xong: model không được hiểu việc lược bỏ là "chưa thực hiện" rồi lặp lại
// một hành động có tác dụng phụ.
func elide(text string) string {
	return truncate(text, keptPrefixRunes) + "\n[…phần còn lại của kết quả này (" + strconv.Itoa(len(text)) +
		" byte) đã được platform lược bỏ khỏi ngữ cảnh để tiết kiệm chỗ. Tool call này ĐÃ chạy xong; không gọi lại chỉ để xem lại kết quả.]"
}

// compact lược bỏ kết quả tool cũ khi hội thoại vượt budget (byte), cũ nhất trước, xuống
// còn khoảng 70% budget. Chỉ message được đánh dấu elidable mới bị đụng tới: bên gọi
// đánh dấu kết quả THÀNH CÔNG của tool call thường. Lượt mở đầu, mọi lượt assistant,
// kết quả của keepRecentTurns lượt assistant gần nhất, và mọi kết quả mang tín hiệu an
// toàn (bị từ chối, lỗi, không xác định, kết quả của task được giao việc) giữ nguyên.
// Trả về hội thoại (bản sao khi có thay đổi) và số kết quả đã lược bỏ. budget ≤ 0: không
// làm gì. Lược bỏ hết mà vẫn vượt thì hội thoại được trả về như vậy: trần cứng của task
// (MaxContextBytes) xử lý phần còn lại.
func compact(msgs []provider.Message, elidable []bool, budget int) ([]provider.Message, int) {
	if budget <= 0 {
		return msgs, 0
	}
	if budget < minContextBudget {
		budget = minContextBudget
	}
	total := 0
	for _, m := range msgs {
		total += messageBytes(m)
	}
	if total <= budget {
		return msgs, 0
	}
	target := budget / compactTargetDen * compactTargetNum
	// Ranh giới: vị trí của lượt assistant thứ keepRecentTurns tính từ cuối.
	boundary, seen := len(msgs), 0
	for i := len(msgs) - 1; i >= 0 && seen < keepRecentTurns; i-- {
		if msgs[i].Role == provider.RoleAssistant {
			seen++
			boundary = i
		}
	}
	out := append([]provider.Message(nil), msgs...)
	elided := 0
	for i := 0; i < boundary && i < len(elidable) && total > target; i++ {
		m := out[i]
		if !elidable[i] || m.Role != provider.RoleTool || m.IsError || len(m.Text) < minElidable {
			continue
		}
		short := elide(m.Text)
		total -= len(m.Text) - len(short)
		m.Text = short
		out[i] = m
		elided++
	}
	return out, elided
}
