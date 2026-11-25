// Package pricingengine là managed MCP server chỉ đọc cho Pricing Engine (PE), hệ
// thống nội bộ giữ giá MUA VÀO của nhà cung cấp và dựng bảng giá xếp hạng offer cho
// từng SKU. Agent dùng nó để khuyến nghị cho người đọc; server không ghi gì vào PE.
//
// Server này KHÔNG có tool ghi. Nó chỉ gửi GET tới /api/v1/prices/{sku}, /products,
// /skus, /skus/{id}, /brands và đúng một POST chỉ đọc tới /api/v1/prices/batch (PE
// dùng POST vì danh sách SKU không vừa query string; không có gì được tạo). Nó không
// chạm /api/v1/fulfillment-events (endpoint ghi), /tombstones, deltaToken, hay bất kỳ
// đường nào dưới /admin.
//
// Địa chỉ PE và API key là cấu hình của NGƯỜI VẬN HÀNH managed server, không phải giá
// trị do tenant hay model đưa vào: không tool nào nhận URL, host hay khoá.
//
// Kết quả là DTO liệt kê từng trường: một trường PE thêm sau này không tự tới Agent.
// Văn bản lỗi của PE không bao giờ được chuyển tiếp; lỗi được suy từ mã trạng thái.
package pricingengine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	// MaxResultBytes là trần kích thước kết quả đã tuần tự hoá của mọi tool. Bằng
	// mặc định max_result_bytes của Registry, để Gateway không phải cắt giữa JSON.
	MaxResultBytes = 64 << 10
	// Phần dành cho vỏ kết quả (tên trường, con trỏ, danh sách SKU) ngoài danh sách chính.
	envelopeReserve = 6 << 10

	maxAPIBody        = 8 << 20
	defaultAPITimeout = 15 * time.Second

	// MaxOfferLimit: PE nhận limit 1–100 (MAX_OFFER_LIMIT).
	MaxOfferLimit = 100
	// MaxBatchSKUs: PE nhận tới 100 SKU mỗi lô; server này nhận tối đa 50 để kết quả còn
	// vừa trần kích thước.
	MaxBatchSKUs = 50
	// MaxPageSize: PE nhận tới 500 dòng mỗi trang; server này nhận tối đa 100.
	MaxPageSize     = 100
	defaultPageSize = 50
	// Số lần gọi lại PE với trang nhỏ hơn khi một trang không vừa trần kích thước.
	maxShrinkRounds = 7

	// callDeadline là hạn TỔNG của một tool call, gồm cả các lần đọc lại trang nhỏ hơn
	// (walk): nhỏ hơn timeout 30 giây đề xuất của tool, để server trả lỗi rõ ràng trước khi
	// Gateway bỏ kết nối.
	callDeadline = 25 * time.Second

	// Hạn mức của process tới PE (xem gate). Key của PE mặc định được 600 request / 60 giây;
	// server tự giữ ở một nửa để một Agent lặp vô ích không dùng hết hạn mức của key (và
	// không đẩy PE vào 429 cho cả những lời gọi khác), và giữ số request đồng thời thấp vì
	// một lô bảng giá là truy vấn nặng nhất của API này.
	defaultRequestsPerMinute = 300
	defaultBurst             = 30
	maxConcurrent            = 8

	maxSKUBytes    = 100
	maxCursorBytes = 512
	maxNameBytes   = 200
	maxCodeBytes   = 100
	maxShortBytes  = 64
	maxFormerCodes = 10
	// Một bảng giá của PE hiếm khi quá vài chục offer; nhiều hơn mức này là phản hồi hỏng.
	maxOffersPerBook = 2000
	maxBrandRegions  = 64

	// Ngắt mạch sau 429: không nêu thời gian thì defaultBlock; luôn trong [minBlock, maxBlock].
	defaultBlock = 10 * time.Second
	minBlock     = time.Second
	maxBlock     = 2 * time.Minute
)

// Các mẫu dưới đây cũng nằm trong input schema của tool (server.go); handler kiểm lại.
// Chúng chỉ dùng cú pháp chung của RE2 và ECMA 262.
const (
	// SKUPattern là SKU_PATTERN của PE: chữ in hoa và số, các nhóm nối bằng một '-' hoặc '_'.
	SKUPattern      = `^[A-Z0-9]+([-_][A-Z0-9]+)*$`
	CurrencyPattern = `^[A-Z]{3}$`
	UUIDPattern     = `^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`
	// RegionPattern theo schema của PE (catalog-api.schema.ts): 2–10 chữ in hoa.
	RegionPattern = `^[A-Z]{2,10}$`
	// CursorPattern: con trỏ của PE là base64url, tối đa 512 ký tự.
	CursorPattern = `^[A-Za-z0-9_-]+$`
	// TimePattern: RFC 3339 với múi giờ tường minh; time.Parse kiểm lại phần còn lại. Mốc
	// thời gian PE trả về (Date.toISOString) cũng theo dạng này.
	TimePattern = `^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,9})?(Z|[+-][0-9]{2}:[0-9]{2})$`
)

var (
	skuRe      = regexp.MustCompile(SKUPattern)
	currencyRe = regexp.MustCompile(CurrencyPattern)
	uuidRe     = regexp.MustCompile(UUIDPattern)
	regionRe   = regexp.MustCompile(RegionPattern)
	cursorRe   = regexp.MustCompile(CursorPattern)
	// Tiền của PE: chuỗi thập phân (numeric(20,6)); không bao giờ đi qua float.
	decimalRe = regexp.MustCompile(`^-?[0-9]{1,20}(\.[0-9]{1,8})?$`)
	reasonRe  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	timeRe    = regexp.MustCompile(TimePattern)

	// Dạng của những giá trị PE trả về. Với enum mà PE có thể thêm giá trị mới (status,
	// availability, productType, mã tiền tệ, region) chỉ kiểm DẠNG, không kiểm tập đóng: PE
	// thêm một giá trị không được làm hỏng tool, nhưng một giá trị không đúng dạng mã thì
	// không phải dữ liệu của trường đó.
	enumOutRe     = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	currencyOutRe = regexp.MustCompile(`^[A-Z0-9]{2,10}$`)
	regionOutRe   = regexp.MustCompile(`^[A-Z0-9]{2,10}$`)
	profileIDRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,99}$`)
)

// Statuses là giá trị của status ở cả SKU và thương hiệu (PRODUCT_STATUSES,
// BRAND_STATUSES trong schema của PE).
var Statuses = []string{"active", "inactive"}

var (
	errUnavailable = errors.New("pricing engine is unreachable; try again later")
	errDenied      = errors.New("pricing engine refused the credential of this server; an operator must check the API key, its scopes and its IP whitelist")
	errTooLarge    = errors.New("pricing engine response is too large; narrow the request")
	errBadResponse = errors.New("pricing engine returned a response this server cannot read")
	errRejected    = errors.New("pricing engine rejected the parameters; a cursor must be the next_cursor of the same tool called with the same filters")
	errRedirect    = errors.New("pricing engine answered with a redirect, which this server does not follow; an operator must check the configured address")
	errSKUNotFound = errors.New("SKU not found: it does not exist or is inactive in the pricing engine")
	errDeadline    = errors.New("pricing engine did not answer within the time allowed for one tool call; try again or narrow the request")
)

// RetryError báo PE tạm thời không phục vụ: bị giới hạn tần suất (429, kể cả khi lời
// gọi bị từ chối cục bộ vì ngắt mạch còn hiệu lực) hoặc đang bảo trì (503). Server
// không tự thử lại và không ngủ chờ. Error() là một đối tượng JSON ổn định.
type RetryError struct {
	// Code là CodeRateLimited, CodeServerRateLimited hoặc CodeMaintenance.
	Code string
	// RetryAfterSeconds là số giây nên chờ; 0 nếu PE không nêu (chỉ với bảo trì).
	RetryAfterSeconds int
}

// Mã lỗi ổn định của RetryError.
const (
	CodeRateLimited = "pricing_engine_rate_limited"
	// CodeServerRateLimited: hạn mức request của chính managed server này đã hết; PE không
	// nhận request nào.
	CodeServerRateLimited = "server_rate_limited"
	CodeMaintenance       = "pricing_engine_maintenance"
)

func (e *RetryError) Error() string {
	out := struct {
		Error             string `json:"error"`
		RetryAfterSeconds int    `json:"retry_after_seconds,omitempty"`
		Message           string `json:"message"`
	}{Error: e.Code, RetryAfterSeconds: e.RetryAfterSeconds,
		Message: "the pricing engine is rate limiting this server; no request is sent until the wait is over, and the server does not retry by itself"}
	switch e.Code {
	case CodeMaintenance:
		out.Message = "the pricing engine is under maintenance; nothing was read, try again later"
	case CodeServerRateLimited:
		out.Message = "this server has used up its own request budget towards the pricing engine; call again later, the server does not queue"
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// Client gọi API v1 của PE. Chỉ dùng GET trên các endpoint đọc và POST /prices/batch.
type Client struct {
	base   string
	apiKey string
	hc     *http.Client
	now    func() time.Time

	mu           sync.Mutex
	blockedUntil time.Time // ngắt mạch: trước thời điểm này không gửi request nào
	tokens       float64   // token bucket của process
	refilled     time.Time
	perMinute    float64
	burst        float64
	sem          chan struct{} // trần request đồng thời tới PE
}

// Options là cấu hình phía server của Client.
type Options struct {
	// APIKey (bắt buộc) là API key của PE, gửi trong header Authorization. Nó không
	// bao giờ vào log, kết quả tool hay thông báo lỗi.
	APIKey string
	// AllowLoopbackHTTP (chỉ đặt khi HQ_ENV=local, và trong test): nhận địa chỉ http nếu
	// host là loopback (localhost, 127.0.0.0/8, ::1). Mọi địa chỉ khác luôn phải là https:
	// khoá và giá mua vào không đi qua mạng ở dạng rõ, kể cả ở local.
	AllowLoopbackHTTP bool
	// RequestsPerMinute và Burst là hạn mức request của process tới PE (token bucket).
	// 0 là mặc định (300 mỗi phút, burst 30).
	RequestsPerMinute int
	Burst             int
	// HTTPClient nil thì dùng client mặc định có timeout.
	HTTPClient *http.Client
	// Now nil thì dùng time.Now.
	Now func() time.Time
}

// NewClient tạo client. apiURL là địa chỉ gốc của PE do người vận hành cấu hình (ví dụ
// https://pe.example, có thể kèm path prefix). URL không được mang credential, query
// hay fragment.
func NewClient(apiURL string, opts Options) (*Client, error) {
	u, err := url.Parse(apiURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("pricing engine: địa chỉ API phải là URL http(s)")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("pricing engine: địa chỉ API không được chứa credential, query hay fragment")
	}
	if u.Scheme != "https" && (!opts.AllowLoopbackHTTP || !isLoopback(u.Hostname())) {
		return nil, errors.New("pricing engine: địa chỉ API phải là https (http chỉ được nhận ở môi trường local với host loopback)")
	}
	key := strings.TrimSpace(opts.APIKey)
	if key == "" {
		return nil, errors.New("pricing engine: thiếu API key")
	}
	for _, r := range key {
		if r <= ' ' || r > '~' {
			return nil, errors.New("pricing engine: API key chứa ký tự không hợp lệ")
		}
	}
	// Không dùng proxy từ biến môi trường (HTTP_PROXY…): request chỉ đi thẳng tới địa chỉ
	// người vận hành đã cấu hình.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	hc := &http.Client{Timeout: defaultAPITimeout, Transport: transport}
	if opts.HTTPClient != nil {
		c := *opts.HTTPClient
		hc = &c
	}
	// Không theo redirect: khoá chỉ tới đúng địa chỉ đã cấu hình.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	c := &Client{base: strings.TrimRight(u.String(), "/"), apiKey: key, hc: hc, now: now,
		sem: make(chan struct{}, maxConcurrent), perMinute: defaultRequestsPerMinute, burst: defaultBurst}
	if opts.RequestsPerMinute > 0 {
		c.perMinute = float64(opts.RequestsPerMinute)
	}
	if opts.Burst > 0 {
		c.burst = float64(opts.Burst)
	}
	c.tokens, c.refilled = c.burst, now()
	return c, nil
}

// isLoopback: host là "localhost" hoặc một địa chỉ IP loopback. Tên khác (kể cả tên phân
// giải về 127.0.0.1) không được coi là loopback: phân giải tên có thể đổi sau khi kiểm.
func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// seconds làm tròn lên thành số giây, tối thiểu 1.
func seconds(d time.Duration) int {
	return max(1, int((d+time.Second-1)/time.Second))
}

// headerSeconds đọc một header mang số giây (Retry-After, X-RateLimit-Reset). Giá trị
// do PE đưa: chỉ nhận số nguyên dương, và áp trần maxBlock.
func headerSeconds(h http.Header, name string) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(h.Get(name)))
	if err != nil || n <= 0 {
		return 0
	}
	return time.Duration(min(n, int(maxBlock/time.Second))) * time.Second
}

// gate quyết định một request có được gửi tới PE không, theo thứ tự: ngắt mạch sau 429,
// hạn mức request của process (token bucket; hết thì từ chối ngay, không ngủ), rồi trần
// request đồng thời (chờ trong thời hạn của ctx). release phải được gọi khi request xong.
func (c *Client) gate(ctx context.Context) (release func(), err error) {
	if err := c.admit(true); err != nil {
		return nil, err
	}
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, errDeadline
	}
	// Trong lúc chờ, một request khác có thể vừa bị PE giới hạn.
	if err := c.admit(false); err != nil {
		<-c.sem
		return nil, err
	}
	return func() { <-c.sem }, nil
}

// admit kiểm ngắt mạch và, nếu spend, lấy một lượt trong token bucket.
func (c *Client) admit(spend bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if now.Before(c.blockedUntil) {
		return &RetryError{Code: CodeRateLimited, RetryAfterSeconds: seconds(c.blockedUntil.Sub(now))}
	}
	if !spend {
		return nil
	}
	if elapsed := now.Sub(c.refilled); elapsed > 0 {
		c.tokens = min(c.burst, c.tokens+elapsed.Minutes()*c.perMinute)
		c.refilled = now
	}
	if c.tokens < 1 {
		wait := time.Duration((1 - c.tokens) / c.perMinute * float64(time.Minute))
		return &RetryError{Code: CodeServerRateLimited, RetryAfterSeconds: seconds(wait)}
	}
	c.tokens--
	return nil
}

// rateLimited đóng ngắt mạch sau một 429: cho tới thời điểm PE nêu (Retry-After, hoặc
// X-RateLimit-Reset; không nêu thì defaultBlock) mọi request bị từ chối cục bộ.
func (c *Client) rateLimited(h http.Header) *RetryError {
	wait := headerSeconds(h, "Retry-After")
	if wait == 0 {
		wait = headerSeconds(h, "X-RateLimit-Reset")
	}
	if wait == 0 {
		wait = defaultBlock
	}
	wait = min(max(wait, minBlock), maxBlock)
	until := c.now().Add(wait)
	c.mu.Lock()
	if until.After(c.blockedUntil) {
		c.blockedUntil = until
	}
	c.mu.Unlock()
	return &RetryError{Code: CodeRateLimited, RetryAfterSeconds: seconds(wait)}
}

// do gửi một request tới PE và trả về thân của phản hồi 200. notFound là lỗi trả về
// khi PE đáp 404. Văn bản của PE ở mọi mã khác 200 bị bỏ.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body []byte, notFound error) ([]byte, error) {
	// Context đã bị huỷ hoặc quá hạn: không gửi thêm request nào tới PE.
	if ctx.Err() != nil {
		return nil, errDeadline
	}
	release, err := c.gate(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, errUnavailable
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		// Lỗi của net/http chứa URL và địa chỉ nội bộ: chỉ ghi loại lỗi vào log, không
		// ghi vào kết quả tool. Header (nơi có khoá) không nằm trong lỗi.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		slog.WarnContext(ctx, "pricing engine api: request lỗi", "path", path, "error", err)
		if ctx.Err() != nil {
			return nil, errDeadline
		}
		return nil, errUnavailable
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIBody+1))
	if err != nil {
		slog.WarnContext(ctx, "pricing engine api: đọc phản hồi lỗi", "path", path, "error", err)
		return nil, errUnavailable
	}
	switch status := resp.StatusCode; {
	case status == http.StatusOK:
		if len(raw) > maxAPIBody {
			return nil, errTooLarge
		}
		return raw, nil
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		slog.WarnContext(ctx, "pricing engine api: credential bị từ chối", "path", path, "status", status)
		return nil, errDenied
	case status == http.StatusTooManyRequests:
		out := c.rateLimited(resp.Header)
		slog.WarnContext(ctx, "pricing engine api: bị giới hạn tần suất", "path", path, "retry_after_seconds", out.RetryAfterSeconds)
		return nil, out
	case status == http.StatusServiceUnavailable:
		out := &RetryError{Code: CodeMaintenance}
		if wait := headerSeconds(resp.Header, "Retry-After"); wait > 0 {
			out.RetryAfterSeconds = seconds(wait)
		}
		return nil, out
	case status == http.StatusNotFound && notFound != nil:
		return nil, notFound
	case status == http.StatusBadRequest:
		return nil, errRejected
	case status >= 300 && status < 400:
		return nil, errRedirect
	default:
		slog.WarnContext(ctx, "pricing engine api: status không mong đợi", "path", path, "status", status)
		return nil, fmt.Errorf("pricing engine returned an unexpected status (%d)", status)
	}
}

// text làm sạch một trường văn bản tự do của PE (tên do người nhập hoặc lấy từ nhà cung
// cấp) rồi cắt về n byte: ký tự điều khiển, xuống dòng, dấu phân cách dòng / đoạn và ký tự
// định dạng Unicode (bidi, zero-width, BOM) thành khoảng trắng. Văn bản này đi vào context
// của model và vào màn hình của người đọc: nó không được tự xuống dòng để giả làm một
// khối khác, hay đảo chiều hiển thị. Đây là lớp giảm thiểu; nội dung vẫn là dữ liệu không
// tin cậy.
func text(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(s, " "))
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	// Trần n tính cả dấu "…" (3 byte).
	return strings.ToValidUTF8(s[:max(0, n-len("…"))], "") + "…"
}

func textPtr(s *string, n int) string {
	if s == nil {
		return ""
	}
	return text(*s, n)
}

// shape gom việc kiểm dạng các trường mã / enum / thời gian của một phản hồi: một giá trị
// sai dạng làm cả phản hồi thành errBadResponse (xem ok), không được chuyển tiếp hay đoán.
type shape struct{ bad bool }

// must trả về s nếu nó khớp re; ngược lại đánh dấu phản hồi hỏng.
func (v *shape) must(re *regexp.Regexp, s string) string {
	if !re.MatchString(s) {
		v.bad = true
		return ""
	}
	return s
}

// opt như must, nhưng null của PE là trường vắng mặt.
func (v *shape) opt(re *regexp.Regexp, s *string) string {
	if s == nil {
		return ""
	}
	return v.must(re, *s)
}

// sku: mã SKU theo SKU_PATTERN của PE, tối đa 100 ký tự.
func (v *shape) sku(s string) string {
	if checkSKU(s) != nil {
		v.bad = true
		return ""
	}
	return s
}

func size(v any) int {
	b, _ := json.Marshal(v)
	return len(b) + 1 // dấu phẩy ngăn cách
}

// decimal trả về một chuỗi tiền của PE nếu nó đúng là chuỗi thập phân.
func decimal(s string) (string, bool) {
	return s, decimalRe.MatchString(s)
}

// ---------------------------------------------------------------------------
// Bảng giá
// ---------------------------------------------------------------------------

// Supplier là nhà cung cấp của một offer.
type Supplier struct {
	Code string `json:"code" jsonschema:"supplier code in the pricing engine"`
	Name string `json:"name"`
}

// ScoreBreakdown là ba thành phần của điểm.
type ScoreBreakdown struct {
	Price   float64  `json:"price" jsonschema:"0-100; 100 for the cheapest eligible offer"`
	Speed   *float64 `json:"speed,omitempty" jsonschema:"0-100 from the supplier's delivery time; absent while there is no fulfillment data"`
	Success *float64 `json:"success,omitempty" jsonschema:"0-100 from the supplier's smoothed success rate; absent while there is no fulfillment data"`
}

// Offer là một offer của nhà cung cấp trong bảng giá của một SKU.
type Offer struct {
	Rank              int             `json:"rank,omitempty" jsonschema:"1-based rank among eligible offers; absent for an offer that is not ranked"`
	Supplier          Supplier        `json:"supplier"`
	SupplierProductID string          `json:"supplier_product_id" jsonschema:"the supplier's own id for the product"`
	Price             string          `json:"price" jsonschema:"supplier purchase price (cost to HQ) as a decimal string; do not parse as a float"`
	Currency          string          `json:"currency" jsonschema:"currency of price"`
	ConvertedPrice    string          `json:"converted_price,omitempty" jsonschema:"price converted to the requested currency, decimal string; absent when no currency was requested or no FX rate exists"`
	Available         bool            `json:"available" jsonschema:"whether the supplier reports the product as in stock"`
	PriceUpdatedAt    string          `json:"price_updated_at" jsonschema:"when the supplier price last changed, RFC 3339"`
	Stale             bool            `json:"stale" jsonschema:"true when the supplier has stopped syncing, so the price may be outdated"`
	Eligible          bool            `json:"eligible" jsonschema:"whether the offer is ranked"`
	IneligibleReason  string          `json:"ineligible_reason,omitempty" jsonschema:"why the offer is not ranked: unavailable, supplier_inactive, stale, no_fx_rate, low_success_rate, superseded; other values mean not ranked"`
	Score             *float64        `json:"score,omitempty" jsonschema:"0-100 weighted score; absent for an offer that is not ranked"`
	ScoreBreakdown    *ScoreBreakdown `json:"score_breakdown,omitempty"`
	LowConfidence     bool            `json:"low_confidence" jsonschema:"true when the supplier has too few orders in the scoring window for the score to be reliable"`
}

// ScoringProfile là profile chấm điểm PE đã dùng để xếp hạng cho API key của server.
type ScoringProfile struct {
	ID      string `json:"id"`
	Version int    `json:"version"`
}

// PriceBook là bảng giá của một SKU: offer hợp lệ theo thứ hạng, rồi offer bị loại.
type PriceBook struct {
	SKU                string         `json:"sku" jsonschema:"the SKU code that was asked for"`
	CanonicalSKU       string         `json:"canonical_sku,omitempty" jsonschema:"present only when the SKU was asked by a former code: the code it carries now"`
	Name               string         `json:"name"`
	GeneratedAt        string         `json:"generated_at" jsonschema:"when the pricing engine built this price book, RFC 3339"`
	ComparisonCurrency string         `json:"comparison_currency" jsonschema:"currency the offers were compared and scored in"`
	ConvertedCurrency  string         `json:"converted_currency,omitempty" jsonschema:"currency of converted_price, when one was requested"`
	ScoringProfile     ScoringProfile `json:"scoring_profile"`
	Offers             []Offer        `json:"offers" jsonschema:"eligible offers by rank, then ineligible ones when requested; empty when the SKU has no usable offer"`
	OffersOmitted      int            `json:"offers_omitted,omitempty" jsonschema:"number of trailing offers left out because the result would exceed the size limit; lower limit to see a complete list"`
}

// View là các tuỳ chọn hiển thị của bảng giá.
type View struct {
	IncludeIneligible bool
	// Limit 0 là toàn bộ.
	Limit    int
	Currency string
}

func (v View) check() error {
	if v.Limit < 0 || v.Limit > MaxOfferLimit {
		return fmt.Errorf("limit must be between 1 and %d", MaxOfferLimit)
	}
	if v.Currency != "" && !currencyRe.MatchString(v.Currency) {
		return errors.New("currency must be a three-letter upper-case ISO 4217 code, for example USD")
	}
	return nil
}

func checkSKU(sku string) error {
	if len(sku) == 0 || len(sku) > maxSKUBytes || !skuRe.MatchString(sku) {
		return errors.New("sku must be upper-case letters and digits in groups joined by a single '-' or '_' (at most 100 characters)")
	}
	return nil
}

// Hình dạng của PE (packages/pricing/src/pricebook.ts: PriceBookResponse, và
// price-api.ts: canonicalSku).
type rawOffer struct {
	Rank              *int `json:"rank"`
	Supplier          struct{ Code, Name string }
	SupplierProductID string   `json:"supplierProductId"`
	Price             string   `json:"price"`
	Currency          string   `json:"currency"`
	ConvertedPrice    *string  `json:"convertedPrice"`
	Available         bool     `json:"available"`
	PriceUpdatedAt    string   `json:"priceUpdatedAt"`
	Stale             bool     `json:"stale"`
	Eligible          bool     `json:"eligible"`
	IneligibleReason  *string  `json:"ineligibleReason"`
	Score             *float64 `json:"score"`
	ScoreBreakdown    *struct {
		Price   float64  `json:"price"`
		Speed   *float64 `json:"speed"`
		Success *float64 `json:"success"`
	} `json:"scoreBreakdown"`
	LowConfidence bool `json:"lowConfidence"`
}

type rawBook struct {
	SKU                string  `json:"sku"`
	CanonicalSKU       *string `json:"canonicalSku"`
	Name               string  `json:"name"`
	GeneratedAt        string  `json:"generatedAt"`
	ComparisonCurrency string  `json:"comparisonCurrency"`
	Currency           *string `json:"currency"`
	ScoringProfile     struct {
		ID      string `json:"id"`
		Version int    `json:"version"`
	} `json:"scoringProfile"`
	Offers []rawOffer `json:"offers"`
}

// toBook chuyển một bảng giá của PE thành DTO. requested là mã SKU đã hỏi: PE trả lời
// đúng mã đó, kể cả khi nó là mã cũ.
func toBook(r rawBook, requested string) (PriceBook, error) {
	if r.SKU != requested || len(r.Offers) > maxOffersPerBook {
		return PriceBook{}, errBadResponse
	}
	var v shape
	out := PriceBook{
		SKU: requested, Name: text(r.Name, maxNameBytes), GeneratedAt: v.must(timeRe, r.GeneratedAt),
		ComparisonCurrency: v.must(currencyOutRe, r.ComparisonCurrency), ConvertedCurrency: v.opt(currencyOutRe, r.Currency),
		ScoringProfile: ScoringProfile{ID: v.must(profileIDRe, r.ScoringProfile.ID), Version: r.ScoringProfile.Version},
		Offers:         make([]Offer, 0, len(r.Offers)),
	}
	if r.CanonicalSKU != nil {
		out.CanonicalSKU = v.sku(*r.CanonicalSKU)
	}
	for _, o := range r.Offers {
		price, ok := decimal(o.Price)
		if !ok {
			return PriceBook{}, errBadResponse
		}
		offer := Offer{
			Supplier:          Supplier{Code: text(o.Supplier.Code, maxCodeBytes), Name: text(o.Supplier.Name, maxNameBytes)},
			SupplierProductID: text(o.SupplierProductID, maxCodeBytes),
			Price:             price, Currency: v.must(currencyOutRe, o.Currency),
			Available: o.Available, PriceUpdatedAt: v.must(timeRe, o.PriceUpdatedAt),
			Stale: o.Stale, Eligible: o.Eligible, Score: o.Score, LowConfidence: o.LowConfidence,
		}
		if o.Rank != nil && *o.Rank > 0 {
			offer.Rank = *o.Rank
		}
		if o.ConvertedPrice != nil {
			if offer.ConvertedPrice, ok = decimal(*o.ConvertedPrice); !ok {
				return PriceBook{}, errBadResponse
			}
		}
		if !o.Eligible {
			// PE có thể thêm lý do mới; giá trị không đúng dạng mã thì thành "other".
			offer.IneligibleReason = "other"
			if o.IneligibleReason != nil && reasonRe.MatchString(*o.IneligibleReason) {
				offer.IneligibleReason = *o.IneligibleReason
			}
		}
		if b := o.ScoreBreakdown; b != nil {
			offer.ScoreBreakdown = &ScoreBreakdown{Price: b.Price, Speed: b.Speed, Success: b.Success}
		}
		out.Offers = append(out.Offers, offer)
	}
	if v.bad {
		return PriceBook{}, errBadResponse
	}
	return out, nil
}

// fitBook bỏ bớt offer ở cuối (offer bị loại đứng sau offer hợp lệ, thứ hạng thấp đứng
// sau thứ hạng cao) cho tới khi bảng giá vừa budget byte.
func fitBook(b PriceBook, budget int) PriceBook {
	if size(b) <= budget {
		return b
	}
	all := b.Offers
	// Tìm số offer lớn nhất còn vừa; kích thước tăng theo số offer nên chia đôi được.
	lo, hi := 0, len(all)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		b.Offers, b.OffersOmitted = all[:mid], len(all)-mid
		if size(b) <= budget {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	b.Offers, b.OffersOmitted = all[:lo], len(all)-lo
	return b
}

// PriceBook đọc bảng giá đã xếp hạng của một SKU (GET /api/v1/prices/{sku}).
func (c *Client) PriceBook(ctx context.Context, sku string, view View) (PriceBook, error) {
	ctx, cancel := context.WithTimeout(ctx, callDeadline)
	defer cancel()
	if err := checkSKU(sku); err != nil {
		return PriceBook{}, err
	}
	if err := view.check(); err != nil {
		return PriceBook{}, err
	}
	q := url.Values{}
	if view.IncludeIneligible {
		q.Set("includeIneligible", "true")
	}
	if view.Limit > 0 {
		q.Set("limit", strconv.Itoa(view.Limit))
	}
	if view.Currency != "" {
		q.Set("currency", view.Currency)
	}
	// checkSKU chỉ cho [A-Z0-9_-]; PathEscape là lớp thứ hai nếu mẫu đó nới ra sau này.
	body, err := c.do(ctx, http.MethodGet, "/api/v1/prices/"+url.PathEscape(sku), q, nil, errSKUNotFound)
	if err != nil {
		return PriceBook{}, err
	}
	var raw rawBook
	if err := json.Unmarshal(body, &raw); err != nil {
		return PriceBook{}, errBadResponse
	}
	book, err := toBook(raw, sku)
	if err != nil {
		return PriceBook{}, err
	}
	return fitBook(book, MaxResultBytes-envelopeReserve), nil
}

// PriceBooks là kết quả của một lô bảng giá.
type PriceBooks struct {
	PriceBooks   []PriceBook `json:"price_books" jsonschema:"one price book per SKU found, in the order the SKUs were asked"`
	NotFound     []string    `json:"not_found" jsonschema:"SKUs that do not exist or are inactive"`
	DeferredSKUs []string    `json:"deferred_skus,omitempty" jsonschema:"SKUs found but left out because the result would exceed the size limit; call again with only these"`
}

// PriceBooks đọc bảng giá của nhiều SKU trong một lần gọi (POST /api/v1/prices/batch;
// một POST chỉ đọc). SKU không tồn tại không làm hỏng cả lô.
func (c *Client) PriceBooks(ctx context.Context, skus []string, view View) (PriceBooks, error) {
	ctx, cancel := context.WithTimeout(ctx, callDeadline)
	defer cancel()
	if len(skus) == 0 || len(skus) > MaxBatchSKUs {
		return PriceBooks{}, fmt.Errorf("skus must hold between 1 and %d SKU codes", MaxBatchSKUs)
	}
	seen := make(map[string]bool, len(skus))
	for _, sku := range skus {
		if err := checkSKU(sku); err != nil {
			return PriceBooks{}, err
		}
		if seen[sku] {
			return PriceBooks{}, errors.New("skus must not repeat a SKU code")
		}
		seen[sku] = true
	}
	if err := view.check(); err != nil {
		return PriceBooks{}, err
	}
	req := struct {
		SKUs              []string `json:"skus"`
		IncludeIneligible bool     `json:"includeIneligible,omitempty"`
		Limit             int      `json:"limit,omitempty"`
		Currency          string   `json:"currency,omitempty"`
	}{SKUs: skus, IncludeIneligible: view.IncludeIneligible, Limit: view.Limit, Currency: view.Currency}
	payload, err := json.Marshal(req)
	if err != nil {
		return PriceBooks{}, errUnavailable
	}
	body, err := c.do(ctx, http.MethodPost, "/api/v1/prices/batch", nil, payload, nil)
	if err != nil {
		return PriceBooks{}, err
	}
	var raw struct {
		Results  map[string]rawBook `json:"results"`
		NotFound []string           `json:"notFound"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return PriceBooks{}, errBadResponse
	}
	missing := make(map[string]bool, len(raw.NotFound))
	for _, sku := range raw.NotFound {
		missing[sku] = true
	}
	out := PriceBooks{PriceBooks: []PriceBook{}, NotFound: []string{}}
	budget := MaxResultBytes - envelopeReserve
	// Duyệt theo thứ tự đã hỏi: chỉ SKU đã hỏi mới vào kết quả, và mỗi SKU phải hoặc có
	// bảng giá hoặc nằm trong notFound.
	for _, sku := range skus {
		r, ok := raw.Results[sku]
		switch {
		case ok:
			book, err := toBook(r, sku)
			if err != nil {
				return PriceBooks{}, err
			}
			// Bảng giá đầu tiên luôn được trả (cắt bớt offer nếu cần); các bảng sau không vừa
			// phần còn lại thì hoãn, để một SKU lớn không che các SKU đứng sau nó.
			if len(out.PriceBooks) == 0 {
				book = fitBook(book, budget)
			}
			if n := size(book); n <= budget {
				budget -= n
				out.PriceBooks = append(out.PriceBooks, book)
			} else {
				out.DeferredSKUs = append(out.DeferredSKUs, sku)
			}
		case missing[sku]:
			out.NotFound = append(out.NotFound, sku)
		default:
			return PriceBooks{}, errBadResponse
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Danh mục
// ---------------------------------------------------------------------------

func checkCursor(cursor string) error {
	if cursor != "" && (len(cursor) > maxCursorBytes || !cursorRe.MatchString(cursor)) {
		return errors.New("cursor must be the next_cursor of a previous call to the same tool")
	}
	return nil
}

func pageSize(n int) (int, error) {
	if n < 0 || n > MaxPageSize {
		return 0, fmt.Errorf("page size must be between 1 and %d", MaxPageSize)
	}
	if n == 0 {
		n = defaultPageSize
	}
	return n, nil
}

func checkStatus(status string) error {
	if status != "" && status != Statuses[0] && status != Statuses[1] {
		return errors.New("status must be active or inactive")
	}
	return nil
}

// walk đọc một trang của PE bằng fetch(n) và, khi trang đã dựng không vừa trần kích
// thước, đọc lại với số dòng ít hơn. Con trỏ của PE là mờ và chỉ đúng ở cuối một trang
// của CHÍNH PE, nên không thể cắt bớt dòng ở đây mà vẫn giữ được con trỏ. Có trần số
// lần đọc lại; một dòng không bao giờ lớn hơn ngân sách vì mọi trường đều có trần.
//
// Mọi lần đọc lại nằm trong hạn tổng callDeadline của tool call, và dừng ngay khi context
// bị huỷ (Gateway bỏ kết nối, execution bị huỷ).
func walk[T any](ctx context.Context, n int, fetch func(ctx context.Context, n int) (T, int, error)) (T, error) {
	var zero T
	ctx, cancel := context.WithTimeout(ctx, callDeadline)
	defer cancel()
	for round := 0; ; round++ {
		if ctx.Err() != nil {
			return zero, errDeadline
		}
		page, rows, err := fetch(ctx, n)
		if err != nil {
			return zero, err
		}
		if size(page) <= MaxResultBytes-envelopeReserve/2 {
			return page, nil
		}
		if rows <= 1 || n <= 1 || round >= maxShrinkRounds {
			return zero, errTooLarge
		}
		n = max(1, min(n, rows)/2)
	}
}

// Product là một dòng của danh mục SKU (GET /api/v1/products); không có giá.
type Product struct {
	SKU                   string `json:"sku"`
	Name                  string `json:"name"`
	Brand                 string `json:"brand,omitempty"`
	Category              string `json:"category,omitempty"`
	FaceValue             string `json:"face_value,omitempty" jsonschema:"denomination of the product as a decimal string; this is not a price"`
	FaceCurrency          string `json:"face_currency,omitempty"`
	Region                string `json:"region,omitempty"`
	Status                string `json:"status" jsonschema:"active or inactive"`
	Availability          string `json:"availability,omitempty" jsonschema:"available, unavailable (mapped but no supplier can sell it now) or unmapped; absent until computed"`
	AvailabilityChangedAt string `json:"availability_changed_at,omitempty" jsonschema:"RFC 3339"`
	UpdatedAt             string `json:"updated_at" jsonschema:"RFC 3339"`
}

// Products là một trang của danh mục SKU.
type Products struct {
	Products   []Product `json:"products" jsonschema:"ordered by updated_at, then sku; inactive SKUs are included"`
	Total      int       `json:"total" jsonschema:"number of SKUs matching updated_since, across all pages"`
	NextCursor string    `json:"next_cursor,omitempty" jsonschema:"pass as cursor to read the next page; absent on the last page"`
}

// optionalDecimal: mệnh giá có thể null; có thì phải là chuỗi thập phân.
func optionalDecimal(s *string) (string, bool) {
	if s == nil {
		return "", true
	}
	return decimal(*s)
}

func nextCursor(s *string) (string, bool) {
	if s == nil {
		return "", true
	}
	return *s, checkCursor(*s) == nil && *s != ""
}

// Products liệt kê danh mục SKU. updatedSince khác không thì chỉ lấy SKU thay đổi sau
// thời điểm đó.
func (c *Client) Products(ctx context.Context, cursor string, size int, updatedSince time.Time) (Products, error) {
	if err := checkCursor(cursor); err != nil {
		return Products{}, err
	}
	n, err := pageSize(size)
	if err != nil {
		return Products{}, err
	}
	return walk(ctx, n, func(ctx context.Context, n int) (Products, int, error) {
		q := url.Values{"pageSize": {strconv.Itoa(n)}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		if !updatedSince.IsZero() {
			q.Set("updatedSince", updatedSince.UTC().Format(time.RFC3339Nano))
		}
		body, err := c.do(ctx, http.MethodGet, "/api/v1/products", q, nil, nil)
		if err != nil {
			return Products{}, 0, err
		}
		var raw struct {
			Data []struct {
				SKU                   string  `json:"sku"`
				Name                  string  `json:"name"`
				Brand                 *string `json:"brand"`
				Category              *string `json:"category"`
				FaceValue             *string `json:"faceValue"`
				FaceCurrency          *string `json:"faceCurrency"`
				Region                *string `json:"region"`
				Status                string  `json:"status"`
				Availability          *string `json:"availability"`
				AvailabilityChangedAt *string `json:"availabilityChangedAt"`
				UpdatedAt             string  `json:"updatedAt"`
			} `json:"data"`
			Meta struct {
				Total      int     `json:"total"`
				NextCursor *string `json:"nextCursor"`
			} `json:"meta"`
		}
		if err := json.Unmarshal(body, &raw); err != nil || len(raw.Data) > n {
			return Products{}, 0, errBadResponse
		}
		next, ok := nextCursor(raw.Meta.NextCursor)
		if !ok {
			return Products{}, 0, errBadResponse
		}
		out := Products{Products: make([]Product, 0, len(raw.Data)), Total: raw.Meta.Total, NextCursor: next}
		var v shape
		for _, p := range raw.Data {
			face, ok := optionalDecimal(p.FaceValue)
			if !ok {
				return Products{}, 0, errBadResponse
			}
			out.Products = append(out.Products, Product{
				SKU: v.sku(p.SKU), Name: text(p.Name, maxNameBytes), Brand: textPtr(p.Brand, maxNameBytes),
				Category: textPtr(p.Category, maxCodeBytes), FaceValue: face, FaceCurrency: v.opt(currencyOutRe, p.FaceCurrency),
				Region: v.opt(regionOutRe, p.Region), Status: v.must(enumOutRe, p.Status), Availability: v.opt(enumOutRe, p.Availability),
				AvailabilityChangedAt: v.opt(timeRe, p.AvailabilityChangedAt), UpdatedAt: v.must(timeRe, p.UpdatedAt),
			})
		}
		if v.bad {
			return Products{}, 0, errBadResponse
		}
		return out, len(out.Products), nil
	})
}

// SKU là một SKU của feed danh mục (GET /api/v1/skus); không có giá.
type SKU struct {
	ID                    string   `json:"id" jsonschema:"UUID, stable across renames"`
	Code                  string   `json:"code" jsonschema:"current SKU code; the value to pass as sku to the price tools"`
	FormerCodes           []string `json:"former_codes" jsonschema:"codes the SKU carried before a rename, oldest first; may be incomplete"`
	Name                  string   `json:"name"`
	BrandID               string   `json:"brand_id,omitempty"`
	BrandName             string   `json:"brand_name,omitempty"`
	Category              string   `json:"category,omitempty"`
	ProductType           string   `json:"product_type,omitempty" jsonschema:"giftcard, topup, currency, dlc, subscription or gamekey; other values may be added"`
	Region                string   `json:"region,omitempty"`
	FaceValue             string   `json:"face_value,omitempty" jsonschema:"denomination of the product as a decimal string; this is not a price"`
	FaceCurrency          string   `json:"face_currency,omitempty"`
	Unit                  string   `json:"unit,omitempty"`
	Status                string   `json:"status" jsonschema:"active or inactive"`
	Availability          string   `json:"availability,omitempty" jsonschema:"available, unavailable (mapped but no supplier can sell it now) or unmapped; absent until computed"`
	AvailabilityChangedAt string   `json:"availability_changed_at,omitempty" jsonschema:"RFC 3339"`
	CreatedAt             string   `json:"created_at" jsonschema:"RFC 3339"`
	UpdatedAt             string   `json:"updated_at" jsonschema:"RFC 3339"`
}

// Hình dạng của PE (catalog-dto.ts: ApiSku).
type rawSKU struct {
	ID                    string   `json:"id"`
	Code                  string   `json:"code"`
	FormerCodes           []string `json:"formerCodes"`
	Name                  string   `json:"name"`
	BrandID               *string  `json:"brandId"`
	BrandName             *string  `json:"brandName"`
	Category              *string  `json:"category"`
	ProductType           *string  `json:"productType"`
	Region                *string  `json:"region"`
	FaceValue             *string  `json:"faceValue"`
	FaceCurrency          *string  `json:"faceCurrency"`
	Unit                  *string  `json:"unit"`
	Status                string   `json:"status"`
	Availability          *string  `json:"availability"`
	AvailabilityChangedAt *string  `json:"availabilityChangedAt"`
	CreatedAt             string   `json:"createdAt"`
	UpdatedAt             string   `json:"updatedAt"`
}

func toSKU(r rawSKU) (SKU, error) {
	face, ok := optionalDecimal(r.FaceValue)
	if !ok || !uuidRe.MatchString(r.ID) {
		return SKU{}, errBadResponse
	}
	var v shape
	former := make([]string, 0, min(len(r.FormerCodes), maxFormerCodes))
	for _, code := range r.FormerCodes {
		if len(former) == maxFormerCodes {
			break
		}
		former = append(former, v.sku(code))
	}
	out := SKU{
		ID: r.ID, Code: v.sku(r.Code), FormerCodes: former, Name: text(r.Name, maxNameBytes),
		BrandID: v.opt(uuidRe, r.BrandID), BrandName: textPtr(r.BrandName, maxNameBytes),
		Category: textPtr(r.Category, maxCodeBytes), ProductType: v.opt(enumOutRe, r.ProductType), Region: v.opt(regionOutRe, r.Region),
		FaceValue: face, FaceCurrency: v.opt(currencyOutRe, r.FaceCurrency), Unit: textPtr(r.Unit, maxShortBytes),
		Status: v.must(enumOutRe, r.Status), Availability: v.opt(enumOutRe, r.Availability),
		AvailabilityChangedAt: v.opt(timeRe, r.AvailabilityChangedAt),
		CreatedAt:             v.must(timeRe, r.CreatedAt), UpdatedAt: v.must(timeRe, r.UpdatedAt),
	}
	if v.bad {
		return SKU{}, errBadResponse
	}
	return out, nil
}

// SKUs là một trang của feed SKU, hoặc kết quả tra một mã.
type SKUs struct {
	SKUs              []SKU  `json:"skus" jsonschema:"ordered by updated_at; inactive SKUs are included unless filtered"`
	NextCursor        string `json:"next_cursor,omitempty" jsonschema:"pass as cursor, with the same filters, to read the next page; absent on the last page"`
	HasMore           bool   `json:"has_more"`
	MatchedFormerCode bool   `json:"matched_former_code,omitempty" jsonschema:"code lookup only: true when the code given is a former code of the SKU returned"`
}

// SKUFilter là bộ lọc của SKUs. Code đứng một mình: tra đúng một SKU theo mã hiện tại
// hoặc mã cũ.
type SKUFilter struct {
	Status  string
	BrandID string
	Region  string
	Code    string
}

type rawFeedMeta struct {
	NextCursor        *string `json:"nextCursor"`
	HasMore           bool    `json:"hasMore"`
	MatchedFormerCode *bool   `json:"matchedFormerCode"`
}

// SKUs liệt kê SKU của feed danh mục, hoặc tra một SKU theo mã.
func (c *Client) SKUs(ctx context.Context, cursor string, limit int, f SKUFilter) (SKUs, error) {
	if err := checkCursor(cursor); err != nil {
		return SKUs{}, err
	}
	n, err := pageSize(limit)
	if err != nil {
		return SKUs{}, err
	}
	if err := checkStatus(f.Status); err != nil {
		return SKUs{}, err
	}
	switch {
	case f.BrandID != "" && !uuidRe.MatchString(f.BrandID):
		return SKUs{}, errors.New("brand_id must be a UUID")
	case f.Region != "" && !regionRe.MatchString(f.Region):
		return SKUs{}, errors.New("region must be 2 to 10 upper-case letters, for example VN or GLOBAL")
	case f.Code != "" && (cursor != "" || limit != 0 || f.Status != "" || f.BrandID != "" || f.Region != ""):
		return SKUs{}, errors.New("sku_code looks up one SKU and cannot be combined with any other parameter")
	}
	if f.Code != "" {
		if err := checkSKU(f.Code); err != nil {
			return SKUs{}, errors.New("sku_code: " + err.Error())
		}
	}
	return walk(ctx, n, func(ctx context.Context, n int) (SKUs, int, error) {
		q := url.Values{}
		if f.Code != "" {
			q.Set("code", f.Code)
		} else {
			q.Set("limit", strconv.Itoa(n))
			for name, v := range map[string]string{"cursor": cursor, "status": f.Status, "brandId": f.BrandID, "region": f.Region} {
				if v != "" {
					q.Set(name, v)
				}
			}
		}
		body, err := c.do(ctx, http.MethodGet, "/api/v1/skus", q, nil, nil)
		if err != nil {
			return SKUs{}, 0, err
		}
		var raw struct {
			Data []rawSKU    `json:"data"`
			Meta rawFeedMeta `json:"meta"`
		}
		if err := json.Unmarshal(body, &raw); err != nil || len(raw.Data) > n {
			return SKUs{}, 0, errBadResponse
		}
		next, ok := nextCursor(raw.Meta.NextCursor)
		if !ok {
			return SKUs{}, 0, errBadResponse
		}
		out := SKUs{SKUs: make([]SKU, 0, len(raw.Data)), NextCursor: next, HasMore: raw.Meta.HasMore,
			MatchedFormerCode: raw.Meta.MatchedFormerCode != nil && *raw.Meta.MatchedFormerCode}
		for _, r := range raw.Data {
			sku, err := toSKU(r)
			if err != nil {
				return SKUs{}, 0, err
			}
			out.SKUs = append(out.SKUs, sku)
		}
		return out, len(out.SKUs), nil
	})
}

// SKU đọc một SKU theo UUID (GET /api/v1/skus/{id}).
func (c *Client) SKU(ctx context.Context, id string) (SKU, error) {
	ctx, cancel := context.WithTimeout(ctx, callDeadline)
	defer cancel()
	if !uuidRe.MatchString(id) {
		return SKU{}, errors.New("id must be the UUID of a SKU (the id field of list_skus)")
	}
	// uuidRe chỉ cho chữ số hex và '-'; PathEscape là lớp thứ hai.
	body, err := c.do(ctx, http.MethodGet, "/api/v1/skus/"+url.PathEscape(strings.ToLower(id)), nil, nil, errSKUNotFound)
	if err != nil {
		return SKU{}, err
	}
	var raw struct {
		Data *rawSKU `json:"data"`
	}
	if err := json.Unmarshal(body, &raw); err != nil || raw.Data == nil || !strings.EqualFold(raw.Data.ID, id) {
		return SKU{}, errBadResponse
	}
	return toSKU(*raw.Data)
}

// Brand là một thương hiệu (game) của feed danh mục. Cố ý KHÔNG gồm nameKey (khoá
// chuẩn hoá nội bộ của PE; tên trường đó cũng bị Gateway che giá trị).
type Brand struct {
	ID        string   `json:"id" jsonschema:"UUID; the value to pass as brand_id to list_skus"`
	Name      string   `json:"name"`
	Status    string   `json:"status" jsonschema:"active or inactive"`
	Regions   []string `json:"regions" jsonschema:"sorted region codes"`
	CreatedAt string   `json:"created_at" jsonschema:"RFC 3339"`
	UpdatedAt string   `json:"updated_at" jsonschema:"RFC 3339"`
}

// Brands là một trang của feed thương hiệu.
type Brands struct {
	Brands     []Brand `json:"brands" jsonschema:"ordered by updated_at; inactive brands are included unless filtered"`
	NextCursor string  `json:"next_cursor,omitempty" jsonschema:"pass as cursor, with the same status filter, to read the next page; absent on the last page"`
	HasMore    bool    `json:"has_more"`
}

// Brands liệt kê thương hiệu của feed danh mục (GET /api/v1/brands).
func (c *Client) Brands(ctx context.Context, cursor string, limit int, status string) (Brands, error) {
	if err := checkCursor(cursor); err != nil {
		return Brands{}, err
	}
	n, err := pageSize(limit)
	if err != nil {
		return Brands{}, err
	}
	if err := checkStatus(status); err != nil {
		return Brands{}, err
	}
	return walk(ctx, n, func(ctx context.Context, n int) (Brands, int, error) {
		q := url.Values{"limit": {strconv.Itoa(n)}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		if status != "" {
			q.Set("status", status)
		}
		body, err := c.do(ctx, http.MethodGet, "/api/v1/brands", q, nil, nil)
		if err != nil {
			return Brands{}, 0, err
		}
		var raw struct {
			Data []struct {
				ID        string   `json:"id"`
				Name      string   `json:"name"`
				Status    string   `json:"status"`
				Regions   []string `json:"regions"`
				CreatedAt string   `json:"createdAt"`
				UpdatedAt string   `json:"updatedAt"`
			} `json:"data"`
			Meta rawFeedMeta `json:"meta"`
		}
		if err := json.Unmarshal(body, &raw); err != nil || len(raw.Data) > n {
			return Brands{}, 0, errBadResponse
		}
		next, ok := nextCursor(raw.Meta.NextCursor)
		if !ok {
			return Brands{}, 0, errBadResponse
		}
		out := Brands{Brands: make([]Brand, 0, len(raw.Data)), NextCursor: next, HasMore: raw.Meta.HasMore}
		var v shape
		for _, b := range raw.Data {
			if !uuidRe.MatchString(b.ID) {
				return Brands{}, 0, errBadResponse
			}
			regions := make([]string, 0, min(len(b.Regions), maxBrandRegions))
			for _, r := range b.Regions {
				if len(regions) == maxBrandRegions {
					break
				}
				regions = append(regions, v.must(regionOutRe, r))
			}
			out.Brands = append(out.Brands, Brand{ID: b.ID, Name: text(b.Name, maxNameBytes), Status: v.must(enumOutRe, b.Status),
				Regions: regions, CreatedAt: v.must(timeRe, b.CreatedAt), UpdatedAt: v.must(timeRe, b.UpdatedAt)})
		}
		if v.bad {
			return Brands{}, 0, errBadResponse
		}
		return out, len(out.Brands), nil
	})
}
