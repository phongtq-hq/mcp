package pricingengine

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
const Slug = "pricing-engine"

// Version là phiên bản server tự khai trong Registry.
const Version = "0.1.0"

type getPriceBookInput struct {
	SKU               string `json:"sku" jsonschema:"SKU code exactly as list_products or list_skus returns it, for example HQ-FREEFIRE-GC-20"`
	IncludeIneligible bool   `json:"include_ineligible,omitempty" jsonschema:"also return offers excluded from the ranking, with the reason; default false"`
	Limit             int    `json:"limit,omitempty" jsonschema:"only the top N eligible offers (1-100); default all"`
	Currency          string `json:"currency,omitempty" jsonschema:"ISO 4217 code, upper-case; adds converted_price in this currency to each offer"`
}

type getPriceBooksInput struct {
	SKUs              []string `json:"skus" jsonschema:"1 to 50 distinct SKU codes"`
	IncludeIneligible bool     `json:"include_ineligible,omitempty" jsonschema:"also return offers excluded from the ranking, with the reason; default false"`
	Limit             int      `json:"limit,omitempty" jsonschema:"only the top N eligible offers per SKU (1-100); default all"`
	Currency          string   `json:"currency,omitempty" jsonschema:"ISO 4217 code, upper-case; adds converted_price in this currency to each offer"`
}

type listProductsInput struct {
	Cursor       string `json:"cursor,omitempty" jsonschema:"next_cursor of the previous call; omit for the first page"`
	PageSize     int    `json:"page_size,omitempty" jsonschema:"SKUs per page (1-100, default 50)"`
	UpdatedSince string `json:"updated_since,omitempty" jsonschema:"RFC 3339 instant; only SKUs changed after it"`
}

type listSKUsInput struct {
	Cursor  string `json:"cursor,omitempty" jsonschema:"next_cursor of the previous call, with the same filters; omit for the first page"`
	Limit   int    `json:"limit,omitempty" jsonschema:"SKUs per page (1-100, default 50)"`
	Status  string `json:"status,omitempty" jsonschema:"only SKUs with this status"`
	BrandID string `json:"brand_id,omitempty" jsonschema:"only SKUs of this brand (UUID from list_brands)"`
	Region  string `json:"region,omitempty" jsonschema:"only SKUs of this region code, upper-case, for example VN or GLOBAL"`
	Code    string `json:"sku_code,omitempty" jsonschema:"look up one SKU by its current or a former code; cannot be combined with any other parameter"`
}

type getSKUInput struct {
	ID string `json:"id" jsonschema:"UUID of the SKU (the id field of list_skus)"`
}

type listBrandsInput struct {
	Cursor string `json:"cursor,omitempty" jsonschema:"next_cursor of the previous call, with the same status filter; omit for the first page"`
	Limit  int    `json:"limit,omitempty" jsonschema:"brands per page (1-100, default 50)"`
	Status string `json:"status,omitempty" jsonschema:"only brands with this status"`
}

var readOnly = &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true}

// Giới hạn đặt vào input schema của từng trường. Schema là thứ Registry ghim và
// Gateway dùng để kiểm arguments; handler vẫn kiểm lại các giới hạn này.
type bound struct {
	maxLength int
	pattern   string
	enum      []any
	min, max  *float64
	// items khác nil: trường là mảng các chuỗi duy nhất, mỗi phần tử theo items.
	items              *bound
	minItems, maxItems int
	boolean            bool
}

func intRange(lo, hi int) bound {
	return bound{min: jsonschema.Ptr(float64(lo)), max: jsonschema.Ptr(float64(hi))}
}

var (
	skuBound      = bound{maxLength: maxSKUBytes, pattern: SKUPattern}
	currencyBound = bound{maxLength: 3, pattern: CurrencyPattern}
	uuidBound     = bound{maxLength: 36, pattern: UUIDPattern}
	cursorBound   = bound{maxLength: maxCursorBytes, pattern: CursorPattern}
	statusBound   = bound{enum: []any{Statuses[0], Statuses[1]}}
	flagBound     = bound{boolean: true}
	offersBound   = intRange(1, MaxOfferLimit)
	pageBound     = intRange(1, MaxPageSize)
)

func (b bound) apply(p *jsonschema.Schema) {
	if b.maxLength > 0 {
		p.MinLength, p.MaxLength = jsonschema.Ptr(1), jsonschema.Ptr(b.maxLength)
	}
	p.Pattern, p.Enum, p.Minimum, p.Maximum = b.pattern, b.enum, b.min, b.max
	if b.items != nil {
		p.MinItems, p.MaxItems, p.UniqueItems = jsonschema.Ptr(b.minItems), jsonschema.Ptr(b.maxItems), true
		if p.Items == nil {
			panic("pricing engine: input schema: giới hạn items trên trường không phải mảng")
		}
		b.items.apply(p.Items)
	}
}

// inputSchema suy schema từ kiểu Go rồi thêm giới hạn cho từng trường. Mọi trường
// của In phải có giới hạn: trường thiếu làm server không khởi động được.
func inputSchema[In any](bounds map[string]bound) *jsonschema.Schema {
	s, err := jsonschema.For[In](nil)
	if err != nil {
		panic("pricing engine: input schema: " + err.Error())
	}
	if len(s.Properties) != len(bounds) {
		panic("pricing engine: input schema: mỗi trường phải có đúng một giới hạn")
	}
	for name, b := range bounds {
		p := s.Properties[name]
		if p == nil {
			panic("pricing engine: input schema: không có trường " + name)
		}
		b.apply(p)
	}
	return s
}

const advisory = " Đây là GIÁ MUA VÀO của nhà cung cấp (giá vốn của HQ), không phải giá bán, và là dữ liệu mật. Dữ liệu chỉ để tham khảo khi đưa khuyến nghị cho người đọc; server này không đặt hàng và không thay đổi được gì trong Pricing Engine. Tool này chỉ chạy trong một execution của Agent, không gọi trực tiếp được qua Gateway. Chỉ đọc."

// NewServer dựng MCP server với sáu tool chỉ đọc.
func NewServer(client *Client) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "hq-pricing-engine", Version: Version}, nil)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_price_book",
		Description: "Đọc bảng giá đã xếp hạng của một SKU từ Pricing Engine: các offer của nhà cung cấp kèm thứ hạng, nhà cung cấp, giá mua, tình trạng còn hàng, độ mới của giá, có đủ điều kiện hay không và điểm (giá, tốc độ, tỷ lệ thành công). Giá là chuỗi thập phân. Thứ hạng theo profile chấm điểm gán cho API key của server này. Offer ở cuối bị bỏ khi kết quả vượt 64 KiB." + advisory,
		Annotations: readOnly,
		InputSchema: inputSchema[getPriceBookInput](map[string]bound{
			"sku": skuBound, "include_ineligible": flagBound, "limit": offersBound, "currency": currencyBound}),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in getPriceBookInput) (*mcp.CallToolResult, PriceBook, error) {
		out, err := client.PriceBook(ctx, in.SKU, View{IncludeIneligible: in.IncludeIneligible, Limit: in.Limit, Currency: in.Currency})
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_price_books",
		Description: "Đọc bảng giá đã xếp hạng của tối đa 50 SKU từ Pricing Engine trong một lần gọi. SKU không tồn tại hoặc đã ngừng nằm ở not_found và không làm hỏng lời gọi. Kết quả giới hạn 64 KiB: SKU không vừa nằm ở deferred_skus và phải hỏi lại." + advisory,
		Annotations: readOnly,
		InputSchema: inputSchema[getPriceBooksInput](map[string]bound{
			"skus":               {items: &skuBound, minItems: 1, maxItems: MaxBatchSKUs},
			"include_ineligible": flagBound, "limit": offersBound, "currency": currencyBound}),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in getPriceBooksInput) (*mcp.CallToolResult, PriceBooks, error) {
		out, err := client.PriceBooks(ctx, in.SKUs, View{IncludeIneligible: in.IncludeIneligible, Limit: in.Limit, Currency: in.Currency})
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_products",
		Description: "Liệt kê danh mục SKU của Pricing Engine (mã, tên, brand, loại, mệnh giá, region, trạng thái, tình trạng còn hàng), xếp theo lần cập nhật gần nhất, gồm cả SKU đã ngừng. Không có giá. Phân trang bằng cursor / next_cursor; updated_since chỉ trả SKU đổi sau một thời điểm. Dữ liệu tham chiếu nội bộ. Chỉ đọc.",
		Annotations: readOnly,
		InputSchema: inputSchema[listProductsInput](map[string]bound{
			"cursor": cursorBound, "page_size": pageBound, "updated_since": {maxLength: 40, pattern: TimePattern}}),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listProductsInput) (*mcp.CallToolResult, Products, error) {
		var since time.Time
		if in.UpdatedSince != "" {
			var err error
			if since, err = time.Parse(time.RFC3339Nano, in.UpdatedSince); err != nil {
				return nil, Products{}, errors.New("updated_since must be an RFC 3339 instant, for example 2026-11-18T08:00:00Z")
			}
		}
		out, err := client.Products(ctx, in.Cursor, in.PageSize, since)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_skus",
		Description: "Liệt kê SKU của danh mục Pricing Engine kèm id ổn định, mã hiện tại và mã cũ, brand, loại sản phẩm, region, mệnh giá, trạng thái và tình trạng còn hàng; lọc theo trạng thái, brand hoặc region, hoặc tra một SKU theo mã (hiện tại hoặc cũ). Không có giá. Phân trang bằng cursor / next_cursor. Dữ liệu tham chiếu nội bộ. Chỉ đọc.",
		Annotations: readOnly,
		InputSchema: inputSchema[listSKUsInput](map[string]bound{
			"cursor": cursorBound, "limit": pageBound, "status": statusBound, "brand_id": uuidBound,
			"region": {maxLength: 10, pattern: RegionPattern}, "sku_code": skuBound}),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listSKUsInput) (*mcp.CallToolResult, SKUs, error) {
		out, err := client.SKUs(ctx, in.Cursor, in.Limit, SKUFilter{Status: in.Status, BrandID: in.BrandID, Region: in.Region, Code: in.Code})
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_sku",
		Description: "Đọc một SKU của danh mục Pricing Engine theo UUID: mã hiện tại và mã cũ, brand, loại sản phẩm, region, mệnh giá, trạng thái và tình trạng còn hàng. Không có giá. Dữ liệu tham chiếu nội bộ. Chỉ đọc.",
		Annotations: readOnly,
		InputSchema: inputSchema[getSKUInput](map[string]bound{"id": uuidBound}),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in getSKUInput) (*mcp.CallToolResult, SKU, error) {
		out, err := client.SKU(ctx, in.ID)
		return nil, out, err
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_brands",
		Description: "Liệt kê brand (game) của danh mục Pricing Engine kèm id, tên, trạng thái và các region; gồm cả brand đã ngừng nếu không lọc. Phân trang bằng cursor / next_cursor. Dữ liệu tham chiếu nội bộ. Chỉ đọc.",
		Annotations: readOnly,
		InputSchema: inputSchema[listBrandsInput](map[string]bound{
			"cursor": cursorBound, "limit": pageBound, "status": statusBound}),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listBrandsInput) (*mcp.CallToolResult, Brands, error) {
		out, err := client.Brands(ctx, in.Cursor, in.Limit, in.Status)
		return nil, out, err
	})
	return s
}

// ExecutionOnlyTools là các tool chỉ chạy trong một execution của Agent: hai tool trả giá
// mua vào. Client bên ngoài gọi thẳng qua Gateway (token OIDC, API key) bị từ chối, nên mọi
// lần đọc giá vốn đều thuộc một Agent Version đã liệt kê tool đó, có budget và có
// execution_steps. Bốn tool danh mục (không có giá) gọi trực tiếp được.
var ExecutionOnlyTools = []string{"get_price_book", "get_price_books"}

// Handler trả về HTTP handler của server: chỉ nhận request mang ngữ cảnh do Gateway ký cho
// đúng server này; một PE thuộc về đúng một organization (orgID); ngữ cảnh discovery chỉ
// đọc được danh sách tool; ExecutionOnlyTools chỉ chạy khi ngữ cảnh đã ký mang ExecutionID.
// Xem internal/integration/serve.
func Handler(client *Client, signer *signedctx.Signer, orgID uuid.UUID) http.Handler {
	return serve.Handler(Slug, signer, orgID, func() *mcp.Server { return NewServer(client) },
		serve.ExecutionOnly(ExecutionOnlyTools...))
}
