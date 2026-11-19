// Package provider định nghĩa hợp đồng giữa Agent Runtime và một nhà cung cấp LLM.
//
// Một lời gọi Complete là ĐÚNG MỘT lượt của model: nhận hội thoại, trả về văn bản
// hoặc các yêu cầu gọi tool. Provider không bao giờ tự chạy tool và không tự lặp;
// vòng lặp, policy, approval, budget và audit thuộc về platform ("LLM đề xuất,
// platform quyết định").
//
// Mọi kiểu ở đây tuần tự hoá được, để bước CallModel chuyển thành Temporal
// activity ở Phase 2 mà không viết lại (ADR-0002).
package provider

import (
	"context"
	"encoding/json"
)

// Role là vai của một message trong hội thoại.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool" // kết quả của một tool call
)

// StopReason là lý do model dừng lượt.
type StopReason string

const (
	StopEndTurn   StopReason = "end_turn"   // trả lời cuối
	StopToolUse   StopReason = "tool_use"   // yêu cầu gọi tool
	StopMaxTokens StopReason = "max_tokens" // bị cắt vì hết max_output_tokens
	StopRefusal   StopReason = "refusal"    // model từ chối: không retry
)

// Tool là định nghĩa một tool được đưa cho model. Chỉ những tool Agent Version
// được phép mới có mặt ở đây.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// ToolCall là một yêu cầu gọi tool do model đưa ra. Đây là ĐẦU VÀO KHÔNG TIN CẬY:
// tên và arguments phải qua validate và policy trước khi được thực thi.
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// Message là một lượt trong hội thoại.
type Message struct {
	Role Role   `json:"role"`
	Text string `json:"text,omitempty"`
	// ToolCalls: với RoleAssistant, các tool model đã yêu cầu ở lượt đó.
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID, IsError: với RoleTool, kết quả này thuộc lời gọi nào.
	ToolCallID string `json:"tool_call_id,omitempty"`
	IsError    bool   `json:"is_error,omitempty"`
	// Opaque: nội dung riêng của provider cho lượt assistant này (ví dụ khối suy
	// luận), phải được gửi lại NGUYÊN VẸN ở các lượt sau. Runtime lưu và trả lại,
	// không đọc, không sửa. Provider khác bỏ qua nó.
	Opaque json.RawMessage `json:"opaque,omitempty"`
}

// Request là đầu vào của một lượt.
type Request struct {
	// Model là định danh phía provider, do Model Router chọn. Không hardcode.
	Model           string    `json:"model"`
	System          string    `json:"system"`
	Messages        []Message `json:"messages"`
	Tools           []Tool    `json:"tools,omitempty"`
	MaxOutputTokens int       `json:"max_output_tokens"`
}

// Usage là lượng tiêu thụ do provider báo trong response. Không tự đếm.
type Usage struct {
	InputTokens       int64 `json:"input_tokens"`
	OutputTokens      int64 `json:"output_tokens"`
	CachedInputTokens int64 `json:"cached_input_tokens"`
	CacheWriteTokens  int64 `json:"cache_write_tokens"`
	ReasoningTokens   int64 `json:"reasoning_tokens"`
}

// Response là kết quả của một lượt.
type Response struct {
	StopReason StopReason `json:"stop_reason"`
	Text       string     `json:"text,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	// Opaque: xem Message.Opaque. Runtime chép vào message assistant tương ứng.
	Opaque json.RawMessage `json:"opaque,omitempty"`
	Usage  Usage           `json:"usage"`
	// ModelUsed là model thực sự phục vụ (có thể khác Request.Model khi fallback).
	ModelUsed string `json:"model_used"`
}

// Provider là một nhà cung cấp LLM.
//
// Lỗi trả về được phân loại bằng shared/errors: lỗi tạm thời (429, 5xx, timeout)
// đánh dấu retryable; lỗi nội dung thì không. Model từ chối KHÔNG phải lỗi: đó là
// Response với StopRefusal.
type Provider interface {
	// Name là tên adapter, khớp cột model_providers.adapter.
	Name() string
	Complete(ctx context.Context, req Request) (Response, error)
}
