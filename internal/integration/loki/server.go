package loki

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hqgroups/mcp/internal/integration/serve"
	"github.com/hqgroups/mcp/internal/shared/signedctx"
)

// Slug là định danh của server này trong Registry, và là đích (aud) mà ngữ cảnh
// do Gateway ký phải mang.
const Slug = "loki"

// Version là phiên bản server tự khai trong Registry.
const Version = "0.1.0"

type queryLogsInput struct {
	Query     string `json:"query" jsonschema:"LogQL log query, ví dụ {app=\"api\"} |= \"error\""`
	Start     string `json:"start" jsonschema:"đầu khoảng thời gian (gồm cả), RFC 3339"`
	End       string `json:"end" jsonschema:"cuối khoảng thời gian (không gồm), RFC 3339; sau start tối đa 24 giờ"`
	Limit     int    `json:"limit,omitempty" jsonschema:"số dòng tối đa (1-1000, mặc định 100)"`
	Direction string `json:"direction,omitempty" jsonschema:"backward (mặc định): dòng mới nhất trước; forward: dòng cũ nhất trước"`
}

type queryMetricInput struct {
	Query       string `json:"query" jsonschema:"LogQL metric query, ví dụ sum by (level) (count_over_time({app=\"api\"}[5m]))"`
	Start       string `json:"start" jsonschema:"đầu khoảng thời gian, RFC 3339"`
	End         string `json:"end" jsonschema:"cuối khoảng thời gian, RFC 3339; sau start, và khoảng start–end cộng cửa sổ [ … ] lớn nhất của query không quá 24 giờ"`
	StepSeconds int    `json:"step_seconds" jsonschema:"khoảng cách giữa hai điểm, tính bằng giây (từ 10); khoảng thời gian chia step không quá 500 điểm"`
	Limit       int    `json:"limit,omitempty" jsonschema:"số series tối đa (1-50, mặc định 20)"`
}

type listLabelsInput struct {
	LookbackHours int `json:"lookback_hours,omitempty" jsonschema:"chỉ xét log trong số giờ gần đây (1-24, mặc định 6)"`
	Limit         int `json:"limit,omitempty" jsonschema:"số tên tối đa (1-500, mặc định 100)"`
	Offset        int `json:"offset,omitempty" jsonschema:"vị trí bắt đầu; dùng next_offset của lần gọi trước"`
}

type listLabelsOutput struct {
	Labels           []string `json:"labels"`
	Total            int      `json:"total" jsonschema:"số tên label"`
	NextOffset       int      `json:"next_offset,omitempty" jsonschema:"offset của trang kế; không có nếu đã hết"`
	OversizedOmitted int      `json:"oversized_omitted,omitempty" jsonschema:"số phần tử của trang này bị bỏ vì tự nó vượt trần kích thước kết quả"`
}

type listLabelValuesInput struct {
	Label         string `json:"label" jsonschema:"tên label, ví dụ app hoặc namespace"`
	LookbackHours int    `json:"lookback_hours,omitempty" jsonschema:"chỉ xét log trong số giờ gần đây (1-24, mặc định 6)"`
	Limit         int    `json:"limit,omitempty" jsonschema:"số giá trị tối đa (1-500, mặc định 100)"`
	Offset        int    `json:"offset,omitempty" jsonschema:"vị trí bắt đầu; dùng next_offset của lần gọi trước"`
}

type listLabelValuesOutput struct {
	Values           []string `json:"values" jsonschema:"giá trị của label; giá trị dài bị cắt"`
	Total            int      `json:"total" jsonschema:"số giá trị"`
	NextOffset       int      `json:"next_offset,omitempty" jsonschema:"offset của trang kế; không có nếu đã hết"`
	OversizedOmitted int      `json:"oversized_omitted,omitempty" jsonschema:"số phần tử của trang này bị bỏ vì tự nó vượt trần kích thước kết quả"`
}

var readOnly = &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true}

// Giới hạn đặt vào input schema của từng trường. Schema là thứ Registry ghim và
// Gateway dùng để kiểm arguments; handler vẫn kiểm lại các giới hạn này.
type bound struct {
	maxLength int
	pattern   string
	enum      []any
	min, max  *float64
}

func intRange(lo, hi int) bound {
	return bound{min: jsonschema.Ptr(float64(lo)), max: jsonschema.Ptr(float64(hi))}
}

// RFC 3339 với múi giờ tường minh; time.Parse kiểm lại phần còn lại.
const timePattern = `^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,9})?(Z|[+-][0-9]{2}:[0-9]{2})$`

var (
	queryBound    = bound{maxLength: maxQueryBytes}
	timeBound     = bound{maxLength: 40, pattern: timePattern}
	offsetBound   = intRange(0, maxOffset)
	lookbackBound = intRange(1, maxLookbackHours)
	stepBound     = intRange(int(minStep.Seconds()), int(maxRange.Seconds()))
)

// inputSchema suy schema từ kiểu Go rồi thêm giới hạn cho từng trường. Mọi trường
// của In phải có giới hạn: trường thiếu làm server không khởi động được.
func inputSchema[In any](bounds map[string]bound) *jsonschema.Schema {
	s, err := jsonschema.For[In](nil)
	if err != nil {
		panic("loki: input schema: " + err.Error())
	}
	if len(s.Properties) != len(bounds) {
		panic("loki: input schema: mỗi trường phải có đúng một giới hạn")
	}
	for name, b := range bounds {
		p := s.Properties[name]
		if p == nil {
			panic("loki: input schema: không có trường " + name)
		}
		if b.maxLength > 0 {
			p.MinLength, p.MaxLength = jsonschema.Ptr(1), jsonschema.Ptr(b.maxLength)
		}
		p.Pattern, p.Enum, p.Minimum, p.Maximum = b.pattern, b.enum, b.min, b.max
	}
	return s
}

func parseTime(field, value string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, errors.New(field + " phải là thời điểm RFC 3339, ví dụ 2026-11-18T08:00:00Z")
	}
	return t, nil
}

func parseRange(startValue, endValue string) (start, end time.Time, err error) {
	if start, err = parseTime("start", startValue); err != nil {
		return start, end, err
	}
	end, err = parseTime("end", endValue)
	return start, end, err
}

func lookback(hours int) (time.Duration, error) {
	if hours < 0 || hours > maxLookbackHours {
		return 0, errors.New("lookback_hours ngoài khoảng cho phép")
	}
	return time.Duration(hours) * time.Hour, nil
}

// NewServer dựng MCP server với bốn tool chỉ đọc.
func NewServer(client *Client) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "hq-loki", Version: Version}, nil)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "query_logs",
		Description: "Đọc dòng log của Loki bằng một LogQL log query trên một khoảng thời gian (tối đa 24 giờ, tối đa 1000 dòng). Biểu thức không được có range vector ([5m]), offset, @ hay biến. Kết quả không quá 60 KiB: dòng dài bị cắt có đánh dấu, phần vượt bị bỏ và next_end / next_start cho phép đọc tiếp. Nội dung log không được lọc. Chỉ đọc.",
		Annotations: readOnly,
		InputSchema: inputSchema[queryLogsInput](map[string]bound{
			"query": queryBound, "start": timeBound, "end": timeBound, "limit": intRange(1, maxLogLimit),
			"direction": {enum: []any{DirectionBackward, DirectionForward}}}),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in queryLogsInput) (*mcp.CallToolResult, LogResult, error) {
		start, end, err := parseRange(in.Start, in.End)
		if err != nil {
			return nil, LogResult{}, err
		}
		out, err := client.QueryLogs(ctx, in.Query, start, end, in.Limit, in.Direction)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "query_metric",
		Description: "Chạy một LogQL metric query (rate, count_over_time, sum by...) trên một khoảng thời gian. Mỗi cửa sổ [ … ] phải là một duration dạng [5m] hoặc [1h30m]; khoảng start–end cộng cửa sổ lớn nhất không quá 24 giờ; không nhận offset, @, subquery hay biến. Step từ 10 giây, tối đa 500 điểm mỗi series và 50 series. Kết quả bị cắt khi vượt giới hạn. Chỉ đọc.",
		Annotations: readOnly,
		InputSchema: inputSchema[queryMetricInput](map[string]bound{
			"query": queryBound, "start": timeBound, "end": timeBound,
			"step_seconds": stepBound, "limit": intRange(1, maxMetricSeries)}),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in queryMetricInput) (*mcp.CallToolResult, MetricResult, error) {
		start, end, err := parseRange(in.Start, in.End)
		if err != nil {
			return nil, MetricResult{}, err
		}
		if in.StepSeconds < int(minStep.Seconds()) || in.StepSeconds > int(maxRange.Seconds()) {
			return nil, MetricResult{}, errors.New("step_seconds ngoài khoảng cho phép")
		}
		out, err := client.QueryMetric(ctx, in.Query, start, end, time.Duration(in.StepSeconds)*time.Second, in.Limit)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_labels",
		Description: "Liệt kê tên label của các log stream có trong những giờ gần đây (mặc định 6, tối đa 24), theo thứ tự chữ cái. Có phân trang. Chỉ đọc.",
		Annotations: readOnly,
		InputSchema: inputSchema[listLabelsInput](map[string]bound{
			"lookback_hours": lookbackBound, "limit": intRange(1, maxListLimit), "offset": offsetBound}),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listLabelsInput) (*mcp.CallToolResult, listLabelsOutput, error) {
		window, err := lookback(in.LookbackHours)
		if err != nil {
			return nil, listLabelsOutput{}, err
		}
		page, total, next, omitted, err := client.Labels(ctx, window, in.Offset, in.Limit)
		if err != nil {
			return nil, listLabelsOutput{}, err
		}
		return nil, listLabelsOutput{Labels: page, Total: total, NextOffset: next, OversizedOmitted: omitted}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_label_values",
		Description: "Liệt kê giá trị của một label trong những giờ gần đây (mặc định 6, tối đa 24), theo thứ tự chữ cái. Có phân trang. Chỉ đọc.",
		Annotations: readOnly,
		InputSchema: inputSchema[listLabelValuesInput](map[string]bound{
			"label":          {maxLength: 128, pattern: labelNameRe.String()},
			"lookback_hours": lookbackBound, "limit": intRange(1, maxListLimit), "offset": offsetBound}),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listLabelValuesInput) (*mcp.CallToolResult, listLabelValuesOutput, error) {
		window, err := lookback(in.LookbackHours)
		if err != nil {
			return nil, listLabelValuesOutput{}, err
		}
		page, total, next, omitted, err := client.LabelValues(ctx, in.Label, window, in.Offset, in.Limit)
		if err != nil {
			return nil, listLabelValuesOutput{}, err
		}
		return nil, listLabelValuesOutput{Values: page, Total: total, NextOffset: next, OversizedOmitted: omitted}, nil
	})
	return s
}

// DirectTools là các tool client bên ngoài gọi trực tiếp được qua Gateway (serve.Direct);
// tool khác chỉ chạy trong một execution của Agent. query_logs (dòng log) và query_metric (kết quả LogQL theo nhãn) chỉ chạy trong execution.
var DirectTools = []string{"list_labels", "list_label_values"}

// Handler trả về HTTP handler của server: chỉ nhận request mang ngữ cảnh do Gateway ký cho
// đúng server này; một Loki thuộc về đúng một organization (orgID); ngữ cảnh discovery
// chỉ đọc được danh sách tool; tool ngoài DirectTools chỉ chạy trong execution. Xem
// internal/integration/serve.
func Handler(client *Client, verifier *signedctx.Verifier, orgID uuid.UUID) http.Handler {
	return serve.Handler(Slug, verifier, orgID, func() *mcp.Server { return NewServer(client) },
		serve.Direct(DirectTools...))
}
