package prometheus

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
const Slug = "prometheus"

// Version là phiên bản server tự khai trong Registry.
const Version = "0.1.0"

type queryInstantInput struct {
	Query string `json:"query" jsonschema:"biểu thức PromQL"`
	Time  string `json:"time,omitempty" jsonschema:"thời điểm đánh giá, RFC 3339; mặc định là hiện tại"`
	Limit int    `json:"limit,omitempty" jsonschema:"số series tối đa (1-200, mặc định 50)"`
}

type queryRangeInput struct {
	Query       string `json:"query" jsonschema:"biểu thức PromQL"`
	Start       string `json:"start" jsonschema:"đầu khoảng thời gian, RFC 3339"`
	End         string `json:"end" jsonschema:"cuối khoảng thời gian, RFC 3339; cách start tối đa 7 ngày"`
	StepSeconds int    `json:"step_seconds" jsonschema:"khoảng cách giữa hai điểm, tính bằng giây (từ 10); khoảng thời gian chia step không quá 500 điểm"`
	Limit       int    `json:"limit,omitempty" jsonschema:"số series tối đa (1-50, mặc định 20)"`
}

type listAlertsInput struct {
	State  string `json:"state,omitempty" jsonschema:"chỉ lấy alert ở trạng thái này; bỏ trống để lấy cả hai"`
	Limit  int    `json:"limit,omitempty" jsonschema:"số alert tối đa (1-200, mặc định 100)"`
	Offset int    `json:"offset,omitempty" jsonschema:"vị trí bắt đầu; dùng next_offset của lần gọi trước"`
}

type listAlertsOutput struct {
	Alerts           []Alert `json:"alerts"`
	Total            int     `json:"total" jsonschema:"số alert khớp bộ lọc"`
	NextOffset       int     `json:"next_offset,omitempty" jsonschema:"offset của trang kế; không có nếu đã hết"`
	OversizedOmitted int     `json:"oversized_omitted,omitempty" jsonschema:"số phần tử của trang này bị bỏ vì tự nó vượt trần kích thước kết quả"`
}

type listTargetsInput struct {
	Health     string `json:"health,omitempty" jsonschema:"chỉ lấy target có sức khoẻ này; bỏ trống để lấy tất cả"`
	ScrapePool string `json:"scrape_pool,omitempty" jsonschema:"chỉ lấy target của scrape pool này"`
	Limit      int    `json:"limit,omitempty" jsonschema:"số target tối đa (1-200, mặc định 100)"`
	Offset     int    `json:"offset,omitempty" jsonschema:"vị trí bắt đầu; dùng next_offset của lần gọi trước"`
}

type listTargetsOutput struct {
	Targets          []Target `json:"targets"`
	Total            int      `json:"total" jsonschema:"số target khớp bộ lọc"`
	NextOffset       int      `json:"next_offset,omitempty" jsonschema:"offset của trang kế; không có nếu đã hết"`
	OversizedOmitted int      `json:"oversized_omitted,omitempty" jsonschema:"số phần tử của trang này bị bỏ vì tự nó vượt trần kích thước kết quả"`
}

type listMetricNamesInput struct {
	Prefix string `json:"prefix,omitempty" jsonschema:"chỉ lấy metric có tên bắt đầu bằng chuỗi này"`
	Limit  int    `json:"limit,omitempty" jsonschema:"số tên tối đa (1-500, mặc định 100)"`
	Offset int    `json:"offset,omitempty" jsonschema:"vị trí bắt đầu; dùng next_offset của lần gọi trước"`
}

type listMetricNamesOutput struct {
	Names            []string `json:"names"`
	Total            int      `json:"total" jsonschema:"số tên khớp bộ lọc"`
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
	queryBound  = bound{maxLength: maxQueryBytes}
	timeBound   = bound{maxLength: 40, pattern: timePattern}
	offsetBound = intRange(0, maxOffset)
)

// inputSchema suy schema từ kiểu Go rồi thêm giới hạn cho từng trường. Mọi trường
// của In phải có giới hạn: trường thiếu làm server không khởi động được.
func inputSchema[In any](bounds map[string]bound) *jsonschema.Schema {
	s, err := jsonschema.For[In](nil)
	if err != nil {
		panic("prometheus: input schema: " + err.Error())
	}
	if len(s.Properties) != len(bounds) {
		panic("prometheus: input schema: mỗi trường phải có đúng một giới hạn")
	}
	for name, b := range bounds {
		p := s.Properties[name]
		if p == nil {
			panic("prometheus: input schema: không có trường " + name)
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

// NewServer dựng MCP server với năm tool chỉ đọc.
func NewServer(client *Client) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "hq-prometheus", Version: Version}, nil)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "query_instant",
		Description: "Chạy một PromQL instant query tại một thời điểm. Trả tối đa 200 series; kết quả bị cắt khi vượt giới hạn. Chỉ đọc.",
		Annotations: readOnly,
		InputSchema: inputSchema[queryInstantInput](map[string]bound{
			"query": queryBound, "time": timeBound, "limit": intRange(1, maxInstantSeries)}),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in queryInstantInput) (*mcp.CallToolResult, QueryResult, error) {
		var at time.Time
		if in.Time != "" {
			var err error
			if at, err = parseTime("time", in.Time); err != nil {
				return nil, QueryResult{}, err
			}
		}
		out, err := client.QueryInstant(ctx, in.Query, at, in.Limit)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "query_range",
		Description: "Chạy một PromQL range query trên một khoảng thời gian (tối đa 7 ngày, step từ 10 giây, tối đa 500 điểm mỗi series và 50 series). Kết quả bị cắt khi vượt giới hạn. Chỉ đọc.",
		Annotations: readOnly,
		InputSchema: inputSchema[queryRangeInput](map[string]bound{
			"query": queryBound, "start": timeBound, "end": timeBound,
			"step_seconds": intRange(int(minStep.Seconds()), int(maxRange.Seconds())), "limit": intRange(1, maxRangeSeries)}),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in queryRangeInput) (*mcp.CallToolResult, QueryResult, error) {
		start, err := parseTime("start", in.Start)
		if err != nil {
			return nil, QueryResult{}, err
		}
		end, err := parseTime("end", in.End)
		if err != nil {
			return nil, QueryResult{}, err
		}
		if in.StepSeconds < int(minStep.Seconds()) || in.StepSeconds > int(maxRange.Seconds()) {
			return nil, QueryResult{}, errors.New("step_seconds ngoài khoảng cho phép")
		}
		out, err := client.QueryRange(ctx, in.Query, start, end, time.Duration(in.StepSeconds)*time.Second, in.Limit)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_alerts",
		Description: "Liệt kê alert đang hoạt động (firing hoặc pending) của Prometheus, firing trước. Có phân trang. Chỉ đọc.",
		Annotations: readOnly,
		InputSchema: inputSchema[listAlertsInput](map[string]bound{
			"state": {enum: []any{"firing", "pending"}}, "limit": intRange(1, maxListLimit), "offset": offsetBound}),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listAlertsInput) (*mcp.CallToolResult, listAlertsOutput, error) {
		page, total, next, omitted, err := client.Alerts(ctx, in.State, in.Offset, in.Limit)
		if err != nil {
			return nil, listAlertsOutput{}, err
		}
		return nil, listAlertsOutput{Alerts: page, Total: total, NextOffset: next, OversizedOmitted: omitted}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_targets",
		Description: "Liệt kê scrape target đang hoạt động kèm sức khoẻ, lỗi và thời điểm scrape gần nhất. Chỉ trả label job và instance; không trả URL scrape hay label khác. Có phân trang. Chỉ đọc.",
		Annotations: readOnly,
		InputSchema: inputSchema[listTargetsInput](map[string]bound{
			"health":      {enum: []any{"up", "down", "unknown"}},
			"scrape_pool": {maxLength: 200, pattern: scrapePoolRe.String()},
			"limit":       intRange(1, maxListLimit), "offset": offsetBound}),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listTargetsInput) (*mcp.CallToolResult, listTargetsOutput, error) {
		page, total, next, omitted, err := client.Targets(ctx, in.Health, in.ScrapePool, in.Offset, in.Limit)
		if err != nil {
			return nil, listTargetsOutput{}, err
		}
		return nil, listTargetsOutput{Targets: page, Total: total, NextOffset: next, OversizedOmitted: omitted}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_metric_names",
		Description: "Liệt kê tên metric có dữ liệu trong một giờ gần đây, theo thứ tự chữ cái, lọc được theo tiền tố. Có phân trang. Chỉ đọc.",
		Annotations: readOnly,
		InputSchema: inputSchema[listMetricNamesInput](map[string]bound{
			"prefix": {maxLength: 128, pattern: prefixRe.String()},
			"limit":  intRange(1, maxNamesLimit), "offset": offsetBound}),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listMetricNamesInput) (*mcp.CallToolResult, listMetricNamesOutput, error) {
		page, total, next, omitted, err := client.MetricNames(ctx, in.Prefix, in.Offset, in.Limit)
		if err != nil {
			return nil, listMetricNamesOutput{}, err
		}
		return nil, listMetricNamesOutput{Names: page, Total: total, NextOffset: next, OversizedOmitted: omitted}, nil
	})
	return s
}

// Handler trả về HTTP handler của server: chỉ nhận request mang ngữ cảnh do Gateway ký cho
// đúng server này; một Prometheus thuộc về đúng một organization (orgID); ngữ cảnh discovery
// chỉ đọc được danh sách tool. Xem internal/integration/serve.
func Handler(client *Client, signer *signedctx.Signer, orgID uuid.UUID) http.Handler {
	return serve.Handler(Slug, signer, orgID, func() *mcp.Server { return NewServer(client) })
}
