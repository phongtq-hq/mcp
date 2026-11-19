// Package anthropic là adapter LLM dùng Claude qua Anthropic API bằng API key, với
// tool use gốc (ADR-0013). Đây là adapter cho staging và production; phát triển
// cục bộ không có key thì dùng adapter claudecli.
//
// Một lời gọi Complete là đúng một lượt của model: adapter không chạy tool và không
// lặp. Tên model do Model Catalog cung cấp; không có tên model hay giá nào ở đây.
package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	sdk "github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"

	"github.com/hqgroups/mcp/internal/ai/provider"
	apperr "github.com/hqgroups/mcp/internal/shared/errors"
)

// Name là tên adapter, khớp cột model_providers.adapter.
const Name = "anthropic"

const (
	// Tên tool phía API chỉ gồm chữ, số, "_" và "-", tối đa 64 ký tự. Tên của
	// platform có dạng "<server>.<tool>", nên dấu chấm được đổi thành "__" trên dây.
	maxWireName    = 64
	requestTimeout = 10 * time.Minute
)

// Config là cấu hình của adapter.
type Config struct {
	// APIKey là khoá của platform cho Anthropic API. Bắt buộc.
	APIKey string
	// BaseURL chỉ dùng cho test. Rỗng = endpoint chính thức.
	BaseURL string
	// HTTPClient chỉ dùng cho test.
	HTTPClient *http.Client
}

// Provider gọi Anthropic Messages API.
type Provider struct {
	client sdk.Client
}

// New tạo Provider. Khoá không được ghi vào log hay lỗi.
func New(cfg Config) (*Provider, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errors.New("anthropic: thiếu API key")
	}
	opts := []option.RequestOption{
		// Không lấy cấu hình từ biến môi trường ANTHROPIC_* của process: một
		// ANTHROPIC_BASE_URL còn sót (thường gặp trên máy dev) sẽ gửi khoá tới URL đó.
		option.WithoutEnvironmentDefaults(),
		option.WithEnvironmentProduction(),
		option.WithAPIKey(strings.TrimSpace(cfg.APIKey)),
		// Retry thuộc về runtime (có trần, ghi step): SDK không tự thử lại, để không
		// có hai lớp retry nhân lên nhau.
		option.WithMaxRetries(0),
		option.WithRequestTimeout(requestTimeout),
	}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	if cfg.HTTPClient != nil {
		opts = append(opts, option.WithHTTPClient(cfg.HTTPClient))
	}
	return &Provider{client: sdk.NewClient(opts...)}, nil
}

// Name hiện thực provider.Provider.
func (p *Provider) Name() string { return Name }

// names ánh xạ tên tool của platform sang tên trên dây và ngược lại, cho một request.
type names struct {
	toWire   map[string]string
	fromWire map[string]string
}

func wireName(name string) string { return strings.ReplaceAll(name, ".", "__") }

func newNames(tools []provider.Tool) (names, error) {
	n := names{toWire: map[string]string{}, fromWire: map[string]string{}}
	for _, t := range tools {
		w := wireName(t.Name)
		if len(w) > maxWireName || w == "" {
			return names{}, apperr.New(apperr.KindUnprocessable, "tool_name_unsupported",
				fmt.Sprintf("tên tool %q không biểu diễn được với provider này (quá %d ký tự)", t.Name, maxWireName))
		}
		if other, dup := n.fromWire[w]; dup && other != t.Name {
			return names{}, apperr.New(apperr.KindUnprocessable, "tool_name_unsupported",
				fmt.Sprintf("tên tool %q và %q trùng nhau với provider này", other, t.Name))
		}
		n.toWire[t.Name], n.fromWire[w] = w, t.Name
	}
	return n, nil
}

// wire trả về tên trên dây; tên không có trong tập tool (lịch sử cũ) vẫn được đổi theo cùng quy tắc.
func (n names) wire(name string) string {
	if w, ok := n.toWire[name]; ok {
		return w
	}
	return wireName(name)
}

// platform trả về tên của platform; tên model bịa ra được trả nguyên để runtime từ chối.
func (n names) platform(w string) string {
	if name, ok := n.fromWire[w]; ok {
		return name
	}
	return w
}

func toolParams(tools []provider.Tool, n names) ([]sdk.ToolUnionParam, error) {
	out := make([]sdk.ToolUnionParam, 0, len(tools))
	for _, t := range tools {
		var schema map[string]any
		if err := json.Unmarshal(t.InputSchema, &schema); err != nil || schema == nil {
			return nil, apperr.New(apperr.KindUnprocessable, "tool_schema_unsupported", "schema của tool không phải JSON object")
		}
		in := sdk.ToolInputSchemaParam{Properties: schema["properties"], ExtraFields: map[string]any{}}
		if req, ok := schema["required"].([]any); ok {
			for _, r := range req {
				if s, ok := r.(string); ok {
					in.Required = append(in.Required, s)
				}
			}
		}
		for k, v := range schema {
			if k != "type" && k != "properties" && k != "required" {
				in.ExtraFields[k] = v
			}
		}
		tp := sdk.ToolParam{Name: n.wire(t.Name), InputSchema: in}
		if t.Description != "" {
			tp.Description = sdk.String(t.Description)
		}
		out = append(out, sdk.ToolUnionParam{OfTool: &tp})
	}
	return out, nil
}

// messages chuyển hội thoại của runtime sang định dạng Messages API. Kết quả tool
// liên tiếp được gộp vào một lượt user, đúng thứ tự.
func messages(in []provider.Message, n names) ([]sdk.MessageParam, error) {
	var out []sdk.MessageParam
	var results []sdk.ContentBlockParamUnion
	flush := func() {
		if len(results) > 0 {
			out = append(out, sdk.NewUserMessage(results...))
			results = nil
		}
	}
	for _, m := range in {
		switch m.Role {
		case provider.RoleTool:
			results = append(results, sdk.NewToolResultBlock(m.ToolCallID, m.Text, m.IsError))
		case provider.RoleUser:
			flush()
			out = append(out, sdk.NewUserMessage(sdk.NewTextBlock(m.Text)))
		case provider.RoleAssistant:
			flush()
			// Opaque là nội dung nguyên vẹn của lượt này do chính adapter lưu (gồm khối
			// suy luận và chữ ký của nó): API yêu cầu gửi lại y nguyên.
			if blocks, ok := decodeOpaque(m.Opaque); ok {
				out = append(out, sdk.NewAssistantMessage(blocks...))
				continue
			}
			var blocks []sdk.ContentBlockParamUnion
			if m.Text != "" {
				blocks = append(blocks, sdk.NewTextBlock(m.Text))
			}
			for _, c := range m.ToolCalls {
				var input any = map[string]any{}
				if len(c.Arguments) > 0 && json.Unmarshal(c.Arguments, &input) != nil {
					input = map[string]any{}
				}
				blocks = append(blocks, sdk.NewToolUseBlock(c.ID, input, n.wire(c.Name)))
			}
			if len(blocks) == 0 {
				return nil, errors.New("anthropic: lượt assistant rỗng trong hội thoại")
			}
			out = append(out, sdk.NewAssistantMessage(blocks...))
		default:
			return nil, fmt.Errorf("anthropic: vai message không hỗ trợ: %q", m.Role)
		}
	}
	flush()
	return out, nil
}

// opaque là dạng lưu của một lượt assistant do adapter này tạo ra.
type opaque struct {
	Adapter string          `json:"adapter"`
	Content json.RawMessage `json:"content"`
}

func encodeOpaque(msg sdk.MessageParam) json.RawMessage {
	if msg.OfAssistant == nil || len(msg.OfAssistant.Content) == 0 {
		return nil
	}
	content, err := json.Marshal(msg.OfAssistant.Content)
	if err != nil {
		return nil
	}
	b, err := json.Marshal(opaque{Adapter: Name, Content: content})
	if err != nil {
		return nil
	}
	return b
}

func decodeOpaque(raw json.RawMessage) ([]sdk.ContentBlockParamUnion, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var o opaque
	if json.Unmarshal(raw, &o) != nil || o.Adapter != Name {
		return nil, false // lượt do adapter khác tạo: dựng lại từ text và tool call
	}
	var blocks []sdk.ContentBlockParamUnion
	if json.Unmarshal(o.Content, &blocks) != nil || len(blocks) == 0 {
		return nil, false
	}
	return blocks, true
}

// Complete hiện thực provider.Provider.
func (p *Provider) Complete(ctx context.Context, req provider.Request) (provider.Response, error) {
	if req.Model == "" || req.MaxOutputTokens <= 0 {
		return provider.Response{}, errors.New("anthropic: thiếu model hoặc max_output_tokens")
	}
	n, err := newNames(req.Tools)
	if err != nil {
		return provider.Response{}, err
	}
	msgs, err := messages(req.Messages, n)
	if err != nil {
		return provider.Response{}, err
	}
	tools, err := toolParams(req.Tools, n)
	if err != nil {
		return provider.Response{}, err
	}
	params := sdk.MessageNewParams{
		Model:     req.Model,
		MaxTokens: int64(req.MaxOutputTokens),
		Messages:  msgs,
		Tools:     tools,
		// Vòng lặp Agent gửi lại cùng tiền tố (system, tool, lịch sử) ở mỗi lượt: đánh
		// dấu cache tự động để các lượt sau đọc từ cache. Usage báo riêng phần ghi và
		// đọc cache, và ledger tính theo giá riêng của chúng.
		CacheControl: sdk.NewCacheControlEphemeralParam(),
	}
	if req.System != "" {
		params.System = []sdk.TextBlockParam{{Text: req.System}}
	}

	// Streaming: lượt dài không bị giới hạn thời gian của request không streaming.
	stream := p.client.Messages.NewStreaming(ctx, params)
	defer func() { _ = stream.Close() }()
	var msg sdk.Message
	// usage là số token provider đã báo tới thời điểm hiện tại. Luồng bị ngắt giữa chừng
	// (lỗi mạng, huỷ, timeout) vẫn bị tính tiền cho phần đã xử lý, nên lỗi được trả về
	// kèm usage dở dang: nơi giữ budget quyết toán theo nó thay vì hoàn toàn bộ.
	usage := func() provider.Usage {
		return provider.Usage{
			InputTokens: msg.Usage.InputTokens, OutputTokens: msg.Usage.OutputTokens,
			CachedInputTokens: msg.Usage.CacheReadInputTokens, CacheWriteTokens: msg.Usage.CacheCreationInputTokens,
			ReasoningTokens: msg.Usage.OutputTokensDetails.ThinkingTokens,
		}
	}
	for stream.Next() {
		if err := msg.Accumulate(stream.Current()); err != nil {
			return provider.Response{ModelUsed: msg.Model, Usage: usage()}, classify(ctx, err)
		}
	}
	if err := stream.Err(); err != nil {
		return provider.Response{ModelUsed: msg.Model, Usage: usage()}, classify(ctx, err)
	}

	resp := provider.Response{ModelUsed: msg.Model, Usage: usage()}
	var text strings.Builder
	for _, block := range msg.Content {
		switch b := block.AsAny().(type) {
		case sdk.TextBlock:
			text.WriteString(b.Text)
		case sdk.ToolUseBlock:
			args := json.RawMessage(b.JSON.Input.Raw())
			if len(args) == 0 {
				args = b.Input
			}
			resp.ToolCalls = append(resp.ToolCalls, provider.ToolCall{ID: b.ID, Name: n.platform(b.Name), Arguments: args})
		}
	}
	resp.Text = text.String()
	switch msg.StopReason {
	case sdk.StopReasonEndTurn, sdk.StopReasonStopSequence:
		resp.StopReason = provider.StopEndTurn
	case sdk.StopReasonToolUse:
		resp.StopReason = provider.StopToolUse
	case sdk.StopReasonMaxTokens:
		resp.StopReason = provider.StopMaxTokens
	case sdk.StopReasonRefusal:
		resp.StopReason = provider.StopRefusal
	default:
		// pause_turn, model_context_window_exceeded, hoặc giá trị mới: chuyển nguyên
		// để runtime coi là lỗi của lượt, không coi là trả lời cuối.
		resp.StopReason = provider.StopReason(msg.StopReason)
	}
	if len(msg.Content) > 0 {
		resp.Opaque = encodeOpaque(msg.ToParam())
	}
	return resp, nil
}

// classify phân loại lỗi cho runtime: lỗi tạm thời (429, 5xx, quá tải, mạng, timeout)
// được đánh dấu thử lại được; lỗi yêu cầu thì không. Thông báo của provider không được
// chép vào lỗi trả về: nó có thể lặp lại nội dung prompt.
func classify(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var apiErr *sdk.Error
	if errors.As(err, &apiErr) {
		detail := fmt.Sprintf("anthropic: HTTP %d (%s), request %s", apiErr.StatusCode, apiErr.Type(), apiErr.RequestID)
		switch {
		case apiErr.StatusCode == http.StatusTooManyRequests, apiErr.StatusCode >= 500:
			return apperr.Wrap(errors.New(detail), apperr.KindUnavailable, "provider_unavailable", "provider tạm thời không phục vụ").AsRetryable()
		case apiErr.StatusCode == http.StatusUnauthorized, apiErr.StatusCode == http.StatusForbidden:
			return apperr.Wrap(errors.New(detail), apperr.KindInternal, "provider_auth", "provider từ chối thông tin xác thực của platform")
		default:
			return apperr.Wrap(errors.New(detail), apperr.KindUnprocessable, "provider_rejected", "provider từ chối yêu cầu")
		}
	}
	var netErr net.Error
	if errors.As(err, &netErr) || errors.Is(err, context.DeadlineExceeded) {
		return apperr.Wrap(errors.New("anthropic: lỗi mạng hoặc quá thời hạn"), apperr.KindUnavailable, "provider_unavailable", "không kết nối được provider").AsRetryable()
	}
	// Lỗi giữa dòng stream hoặc lỗi giải mã: coi là tạm thời.
	return apperr.Wrap(errors.New("anthropic: luồng phản hồi bị ngắt"), apperr.KindUnavailable, "provider_unavailable", "phản hồi của provider bị ngắt").AsRetryable()
}

// ErrInvalidKey báo provider từ chối API key.
var ErrInvalidKey = errors.New("anthropic: API key không hợp lệ")

// Verifier kiểm một API key bằng một lời gọi có xác thực nhưng không sinh token
// (liệt kê model), trước khi platform lưu key đó.
type Verifier struct {
	// BaseURL chỉ dùng cho test.
	BaseURL string
}

// Verify trả nil nếu provider chấp nhận key, ErrInvalidKey nếu provider từ chối nó,
// và lỗi khác nếu không kiểm tra được (mạng, provider lỗi): khi đó không kết luận
// được gì về key. Key không được chép vào lỗi.
func (v Verifier) Verify(ctx context.Context, apiKey string) error {
	key := strings.TrimSpace(apiKey)
	if key == "" || len(key) > 512 || strings.ContainsFunc(key, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return ErrInvalidKey
	}
	opts := []option.RequestOption{option.WithoutEnvironmentDefaults(), option.WithEnvironmentProduction(),
		option.WithAPIKey(key), option.WithMaxRetries(1), option.WithRequestTimeout(15 * time.Second)}
	if v.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(v.BaseURL))
	}
	client := sdk.NewClient(opts...)
	_, err := client.Models.List(ctx, sdk.ModelListParams{Limit: sdk.Int(1)})
	if err == nil {
		return nil
	}
	var apiErr *sdk.Error
	if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden) {
		return ErrInvalidKey
	}
	if errors.As(err, &apiErr) {
		return fmt.Errorf("anthropic: không kiểm tra được key (HTTP %d)", apiErr.StatusCode)
	}
	return errors.New("anthropic: không kết nối được provider để kiểm tra key")
}
