// Package loki là managed MCP server chỉ đọc cho Grafana Loki: đọc dòng log bằng LogQL,
// chạy LogQL metric query trên một khoảng thời gian, và liệt kê tên label / giá trị label.
//
// Server này KHÔNG có tool ghi. Nó chỉ gửi GET tới ba endpoint đọc của HTTP API
// (/loki/api/v1/query_range, /labels, /label/<name>/values); không chạm push, delete,
// rules, tail (websocket), flush hay các endpoint quản trị.
//
// Địa chỉ Loki, credential và tenant của Loki (header X-Scope-OrgID) là cấu hình của
// NGƯỜI VẬN HÀNH managed server, không phải giá trị do tenant hay model đưa vào: không
// tool nào nhận URL, host, token hay tenant.
//
// Dòng log là dữ liệu do ứng dụng bên thứ ba ghi ra: nó không tin cậy và có thể chứa
// secret. Package này KHÔNG lọc secret trong dòng log.
package loki

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// MaxResultBytes là trần kích thước kết quả đã tuần tự hoá của mọi tool. Bằng
	// mặc định max_result_bytes của Registry, để Gateway không phải cắt giữa JSON.
	MaxResultBytes = 64 << 10
	// MaxLogResultBytes là trần của kết quả query_logs (như get_container_logs của
	// integration docker): thấp hơn MaxResultBytes để còn chỗ cho vỏ MCP.
	MaxLogResultBytes = 60 << 10
	// Phần dành cho vỏ kết quả (tên trường, cờ, con trỏ) ngoài danh sách chính.
	envelopeReserve = 4 << 10

	// Trần body thô của phản hồi Loki. Đường log lớn hơn vì 1000 dòng dài vẫn phải đọc
	// được; đường metric và danh sách nhỏ hơn. Cùng với maxConcurrent, đây là trần bộ
	// nhớ của process cho phản hồi chưa giải mã.
	maxLogBody   = 4 << 20
	maxSmallBody = 2 << 20
	// maxConcurrent là số request đồng thời tới Loki của một Client; quá thì tool trả
	// lỗi "đang bận" ngay, không xếp hàng.
	maxConcurrent     = 4
	maxQueryBytes     = 2000
	defaultAPITimeout = 25 * time.Second

	// maxRange là khoảng thời gian tối đa của một truy vấn (log và metric).
	maxRange = 24 * time.Hour

	maxLogLimit     = 1000
	defaultLogLimit = 100
	// MaxLineBytes là độ dài tối đa của một dòng log trong kết quả; phần vượt bị cắt
	// và dòng được đánh dấu line_truncated.
	MaxLineBytes = 2048
	// Một dòng (kèm label) lớn hơn mức này sau khi tuần tự hoá thì bị bỏ label: nhờ
	// vậy mọi dòng đều vừa ngân sách và không dòng nào chặn các dòng sau nó.
	maxEntryBytes = 16 << 10

	maxMetricSeries     = 50
	defaultMetricSeries = 20
	maxPointsPerSeries  = 500
	minStep             = 10 * time.Second

	maxListLimit     = 500
	defaultListLimit = 100
	maxOffset        = 100000
	maxLookbackHours = 24
	defaultLookback  = 6 * time.Hour

	maxLabels          = 32
	maxLabelNameBytes  = 128
	maxLabelValueBytes = 256
	maxErrorBytes      = 512
	maxTenantBytes     = 150

	// tenantLabel là label Loki gắn vào kết quả của truy vấn liên tenant; nó mang tenant
	// id (cấu hình của người vận hành) nên không bao giờ rời server.
	tenantLabel = "__tenant_id__"
)

// Thời điểm ngoài khoảng này bị từ chối: UnixNano tràn ngoài 1678..2262.
var (
	minTime = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	maxTime = time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC)
)

// Hướng đọc log.
const (
	DirectionBackward = "backward"
	DirectionForward  = "forward"
)

var (
	// Tên label của Loki. Chỉ ký tự này nên ghép được vào đường dẫn; vẫn qua
	// url.PathEscape.
	labelNameRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]{0,127}$`)
	// Tenant của Loki (X-Scope-OrgID): một hoặc nhiều id ngăn bằng '|'. Tập ký tự
	// chặt hơn hoặc bằng tập Loki chấp nhận; không có ký tự điều khiển hay khoảng trắng.
	tenantRe = regexp.MustCompile(`^[a-zA-Z0-9!_.*'()-]+(\|[a-zA-Z0-9!_.*'()-]+)*$`)
)

var (
	errUnavailable = errors.New("không gọi được Loki API")
	errDenied      = errors.New("loki từ chối credential của server; kiểm tra cấu hình của managed server")
	errTooLarge    = errors.New("phản hồi của Loki quá lớn; thu hẹp truy vấn, khoảng thời gian hoặc limit")
	errBadResponse = errors.New("loki api: phản hồi không đọc được")
	errBusy        = errors.New("loki đang bận: quá số truy vấn đồng thời của managed server; thử lại sau")
)

// Client gọi HTTP API của Loki. Chỉ dùng GET trên các endpoint đọc.
type Client struct {
	base    string
	token   string
	tenant  string
	tenants []string // các id trong tenant, để che khỏi thông báo lỗi
	hc      *http.Client
	now     func() time.Time
	sem     chan struct{}
}

// Tenants trả về số tenant của Loki mà client này đọc (0 nếu không gửi X-Scope-OrgID).
func (c *Client) Tenants() int { return len(c.tenants) }

// Options là cấu hình phía server của Client.
type Options struct {
	// BearerToken (tuỳ chọn) được gửi trong header Authorization. Nó không bao giờ
	// vào log, kết quả tool hay thông báo lỗi.
	BearerToken string
	// TenantID (tuỳ chọn) được gửi trong header X-Scope-OrgID ở MỌI request. Đây là
	// cấu hình của người vận hành; không tham số tool nào đổi được nó.
	TenantID string
	// AllowMultiTenant cho phép TenantID chứa nhiều id ngăn bằng '|' (truy vấn liên
	// tenant). Mặc định từ chối: một process đọc đúng một tenant của Loki.
	AllowMultiTenant bool
	// RequireHTTPS từ chối địa chỉ http kể cả khi không có token (dùng ngoài local:
	// dòng log đi trên đường nối này).
	RequireHTTPS bool
	// HTTPClient nil thì dùng client mặc định có timeout.
	HTTPClient *http.Client
}

// NewClient tạo client. apiURL là địa chỉ gốc của Loki do người vận hành cấu hình (ví
// dụ https://loki.example:3100, có thể kèm path prefix). URL không được mang
// credential, query hay fragment; có bearer token hoặc RequireHTTPS thì bắt buộc https.
func NewClient(apiURL string, opts Options) (*Client, error) {
	u, err := url.Parse(apiURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("loki: địa chỉ API phải là URL http(s)")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("loki: địa chỉ API không được chứa credential, query hay fragment")
	}
	token := strings.TrimSpace(opts.BearerToken)
	if strings.ContainsFunc(token, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return nil, errors.New("loki: bearer token chứa ký tự điều khiển")
	}
	if token != "" && u.Scheme != "https" {
		return nil, errors.New("loki: bearer token chỉ được gửi qua https")
	}
	if opts.RequireHTTPS && u.Scheme != "https" {
		return nil, errors.New("loki: ngoài môi trường local địa chỉ API phải là https")
	}
	tenant := strings.TrimSpace(opts.TenantID)
	var tenants []string
	if tenant != "" {
		if len(tenant) > maxTenantBytes || !tenantRe.MatchString(tenant) {
			return nil, errors.New("loki: tenant id không hợp lệ (chữ, số và !-_.*'(), tối đa 150 ký tự)")
		}
		tenants = strings.Split(tenant, "|")
		if len(tenants) > 1 && !opts.AllowMultiTenant {
			return nil, errors.New("loki: tenant id chứa '|' (nhiều tenant); chỉ được nhận khi người vận hành bật rõ ràng chế độ nhiều tenant")
		}
		for _, id := range tenants {
			if id == "." || id == ".." {
				return nil, errors.New("loki: tenant id không được là '.' hay '..'")
			}
		}
	}
	// Không dùng proxy từ biến môi trường (HTTP_PROXY…): truy vấn chỉ đi thẳng tới địa chỉ
	// người vận hành đã cấu hình.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	hc := &http.Client{Timeout: defaultAPITimeout, Transport: transport}
	if opts.HTTPClient != nil {
		c := *opts.HTTPClient
		hc = &c
	}
	// Không theo redirect: token, tenant và truy vấn chỉ tới đúng địa chỉ đã cấu hình.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{base: strings.TrimRight(u.String(), "/"), token: token, tenant: tenant, tenants: tenants,
		hc: hc, now: time.Now, sem: make(chan struct{}, maxConcurrent)}, nil
}

// APIError là lỗi do Loki trả về. Message đi vào kết quả tool: với 400 / 422 (LogQL sai,
// vượt giới hạn của Loki) nó là thông báo của Loki đã che và cắt; với mọi mã khác nó là
// một câu cố định, chi tiết chỉ nằm trong log của server.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return "loki: status " + strconv.Itoa(e.Status) + ": " + e.Message
}

// errorMessage rút thông báo lỗi từ body của một phản hồi lỗi. Loki thường trả văn bản
// thuần; một số đường (proxy, bản mới) trả JSON có trường error hoặc message.
func (c *Client) errorMessage(body []byte) string {
	msg := rawErrorMessage(body)
	// Loki nhắc tenant trong một số lỗi; tenant id là cấu hình của người vận hành.
	for _, id := range c.tenants {
		msg = strings.ReplaceAll(msg, id, "[tenant]")
	}
	return scrubError(msg)
}

func rawErrorMessage(body []byte) string {
	var env struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &env) == nil {
		if env.Error != "" {
			body = []byte(env.Error)
		} else if env.Message != "" {
			body = []byte(env.Message)
		}
	}
	// Cắt trước khi chạy regex: body có thể tới vài MiB.
	const scan = 4 * maxErrorBytes
	if len(body) > scan {
		body = body[:scan]
	}
	return strings.TrimSpace(string(body))
}

// get gọi một endpoint đọc và trả về trường data của phong bì. endpoint là tên cố định
// dùng trong log (không chứa truy vấn).
func (c *Client) get(ctx context.Context, endpoint, path string, query url.Values, maxBody int64) (json.RawMessage, error) {
	// Trần truy vấn đồng thời: không xếp hàng, để một Agent gọi dồn dập không giữ bộ nhớ
	// và không chiếm hết hạn mức truy vấn của Loki.
	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	default:
		return nil, errBusy
	}
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, errUnavailable
	}
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.tenant != "" {
		req.Header.Set("X-Scope-OrgID", c.tenant)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		// Lỗi của net/http chứa URL (kèm biểu thức LogQL) và địa chỉ nội bộ: chỉ ghi
		// loại lỗi vào log, không ghi vào kết quả tool. Header không nằm trong lỗi.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		slog.WarnContext(ctx, "loki api: request lỗi", "endpoint", endpoint, "error", err)
		return nil, errUnavailable
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		slog.WarnContext(ctx, "loki api: đọc phản hồi lỗi", "endpoint", endpoint, "error", err)
		return nil, errUnavailable
	}
	if int64(len(body)) > maxBody {
		return nil, errTooLarge
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		// Body của 401 / 403 không vào kết quả: proxy xác thực có thể chép lại credential.
		slog.WarnContext(ctx, "loki api: credential bị từ chối", "endpoint", endpoint, "status", resp.StatusCode)
		return nil, errDenied
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return nil, fmt.Errorf("loki api: status %d (redirect không được theo)", resp.StatusCode)
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnprocessableEntity:
		// Lỗi của chính truy vấn (LogQL sai, vượt giới hạn): người gọi cần đọc được.
		return nil, &APIError{Status: resp.StatusCode, Message: c.errorMessage(body)}
	case resp.StatusCode < 200 || resp.StatusCode >= 400:
		// Lỗi phía Loki (5xx, 429…) có thể chép URL nội bộ kèm credential (object storage,
		// ruler, proxy): kết quả tool chỉ nhận một câu cố định; chi tiết đã che vào log.
		slog.WarnContext(ctx, "loki api: lỗi phía Loki", "endpoint", endpoint, "status", resp.StatusCode, "detail", c.errorMessage(body))
		return nil, &APIError{Status: resp.StatusCode, Message: "lỗi phía Loki; chi tiết nằm trong log của managed server"}
	}
	var env struct {
		Status string          `json:"status"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Status != "success" {
		return nil, errBadResponse
	}
	return env.Data, nil
}

// cut cắt chuỗi về tối đa n byte, giữ UTF-8 hợp lệ.
func cut(s string, n int) string {
	s = strings.ToValidUTF8(s, "�")
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "…"
}

var (
	urlQueryRe = regexp.MustCompile(`\?[^\s"']*`)
	// Userinfo khớp tới '@' CUỐI trước host: mật khẩu có thể chứa '@'.
	urlUserinfoRe = regexp.MustCompile(`://[^/\s"']*@`)
	authSchemeRe  = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]+`)
	// Cặp tên = giá trị / tên: giá trị mà tên gợi ý credential.
	secretPairRe = regexp.MustCompile(`(?i)([\w.-]*(?:password|passwd|pwd|secret|token|key|credential|signature|authorization)[\w.-]*["']?\s*[=:]\s*)("[^"]*"|'[^']*'|[^\s,;&"']+)`)
)

// scrubError che phần có thể mang credential trong thông báo lỗi của Loki (URL của
// object storage, của ruler hay proxy được chép vào lỗi): userinfo và query string của
// URL, giá trị sau Bearer / Basic, và cặp tên = giá trị có tên gợi ý credential. Đây là
// che theo mẫu, không phải bảo đảm.
func scrubError(s string) string {
	s = urlUserinfoRe.ReplaceAllString(s, "://[đã che]@")
	s = urlQueryRe.ReplaceAllString(s, "?[đã che]")
	s = authSchemeRe.ReplaceAllString(s, "$1 [đã che]")
	s = secretPairRe.ReplaceAllString(s, "$1[đã che]")
	return cut(s, maxErrorBytes)
}

// labelSet chép một bộ label với số lượng và độ dài có trần. truncated báo có label bị
// bỏ: vượt maxLabels, hoặc tên trùng nhau sau khi cắt (label sau không ghi đè label
// trước). Label tenantLabel luôn bị bỏ và không tính vào truncated.
func labelSet(in map[string]string) (out map[string]string, truncated bool) {
	out = map[string]string{}
	names := make([]string, 0, len(in))
	for k := range in {
		if k != tenantLabel {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	if len(names) > maxLabels {
		names, truncated = names[:maxLabels], true
	}
	for _, k := range names {
		name := cut(k, maxLabelNameBytes)
		if _, dup := out[name]; dup {
			truncated = true
			continue
		}
		out[name] = cut(in[k], maxLabelValueBytes)
	}
	return out, truncated
}

func size(v any) int {
	b, _ := json.Marshal(v)
	return len(b) + 1 // dấu phẩy ngăn cách
}

// fitPage trả về trang [offset, offset+limit) của items, cắt sớm nếu vượt budget
// byte. next là offset của trang kế, 0 nếu hết. Một phần tử tự nó đã lớn hơn cả ngân
// sách không bao giờ vừa trang nào: nó bị bỏ qua và đếm ở omitted, không làm mất các
// phần tử đứng sau nó.
func fitPage[T any](items []T, offset, limit, budget int) (page []T, next, omitted int) {
	page = []T{}
	if offset >= len(items) {
		return page, 0, 0
	}
	end := min(offset+limit, len(items))
	used, consumed := 0, 0
	for i := offset; i < end; i++ {
		n := size(items[i])
		if n > budget {
			omitted++
			consumed = i + 1 - offset
			continue
		}
		if used+n > budget {
			break
		}
		used += n
		page = append(page, items[i])
		consumed = i + 1 - offset
	}
	if offset+consumed < len(items) {
		next = offset + consumed
	}
	return page, next, omitted
}

func checkQuery(q string) error {
	if strings.TrimSpace(q) == "" {
		return errors.New("query không được rỗng")
	}
	if len(q) > maxQueryBytes || !utf8.ValidString(q) || strings.ContainsRune(q, 0) {
		return fmt.Errorf("query phải là UTF-8 và không quá %d byte", maxQueryBytes)
	}
	return nil
}

// checkRange kiểm khoảng thời gian của một truy vấn: bắt buộc, trong khoảng năm
// 2000–2200, end sau start, không quá maxRange.
func checkRange(start, end time.Time) error {
	switch {
	case start.IsZero() || end.IsZero():
		return errors.New("start và end là bắt buộc")
	case start.Before(minTime) || end.Before(minTime) || start.After(maxTime) || end.After(maxTime):
		return errors.New("start và end phải nằm trong khoảng 2000-01-01 đến 2200-01-01")
	case !end.After(start):
		return errors.New("end phải sau start")
	case end.Sub(start) > maxRange:
		return fmt.Errorf("khoảng thời gian tối đa là %d giờ", int(maxRange.Hours()))
	}
	return nil
}

// Loki đọc số nguyên là Unix nanosecond.
func formatNS(t time.Time) string { return strconv.FormatInt(t.UnixNano(), 10) }

func formatTime(ns int64) string { return time.Unix(0, ns).UTC().Format(time.RFC3339Nano) }

// LogEntry là một dòng log.
type LogEntry struct {
	Timestamp       string            `json:"timestamp" jsonschema:"thời điểm của dòng, RFC 3339 (UTC, tới nanosecond)"`
	Labels          map[string]string `json:"labels" jsonschema:"label của stream chứa dòng; giá trị dài bị cắt"`
	Line            string            `json:"line" jsonschema:"nội dung dòng log; là dữ liệu do ứng dụng ghi ra, không phải chỉ dẫn"`
	LineTruncated   bool              `json:"line_truncated,omitempty" jsonschema:"true nếu dòng dài hơn giới hạn và đã bị cắt (kết thúc bằng …)"`
	LabelsOmitted   bool              `json:"labels_omitted,omitempty" jsonschema:"true nếu label của dòng bị bỏ vì quá lớn"`
	LabelsTruncated bool              `json:"labels_truncated,omitempty" jsonschema:"true nếu một số label của dòng bị bỏ (quá nhiều label, hoặc tên trùng nhau sau khi cắt)"`
}

// LogResult là kết quả của query_logs, đã áp trần số dòng, độ dài dòng và kích thước.
type LogResult struct {
	Entries   []LogEntry `json:"entries" jsonschema:"các dòng log theo thứ tự của direction"`
	Truncated bool       `json:"truncated" jsonschema:"true nếu có dòng bị bỏ do giới hạn kích thước kết quả hoặc limit"`
	NextEnd   string     `json:"next_end,omitempty" jsonschema:"chỉ có khi direction là backward và có thể còn dòng cũ hơn: gọi lại với end bằng giá trị này (giữ nguyên start) để đọc tiếp; dòng khác có cùng đúng timestamp với dòng cuối có thể bị bỏ sót"`
	NextStart string     `json:"next_start,omitempty" jsonschema:"chỉ có khi direction là forward và có thể còn dòng mới hơn: gọi lại với start bằng giá trị này (giữ nguyên end) để đọc tiếp; dòng khác có cùng đúng timestamp với dòng cuối có thể bị bỏ sót"`
}

type rawStream struct {
	Stream map[string]string   `json:"stream"`
	Values [][]json.RawMessage `json:"values"`
}

type queryData struct {
	ResultType string          `json:"resultType"`
	Result     json.RawMessage `json:"result"`
}

// QueryLogs chạy một LogQL log query trên [start, end) và trả các dòng log. Biểu thức qua
// bộ kiểm từ vựng (logql.go): không range vector, offset, @ hay biến. limit 0 là
// mặc định (100), tối đa 1000; direction rỗng là backward (mới nhất trước).
//
// Kết quả đã tuần tự hoá không quá MaxLogResultBytes. Khi vượt, các dòng ở CUỐI thứ tự
// đọc bị bỏ: với backward là các dòng cũ nhất (giữ phần mới nhất), với forward là các
// dòng mới nhất; con trỏ NextEnd / NextStart cho phép đọc tiếp phần bị bỏ.
func (c *Client) QueryLogs(ctx context.Context, query string, start, end time.Time, limit int, direction string) (LogResult, error) {
	if err := checkQuery(query); err != nil {
		return LogResult{}, err
	}
	if err := checkRange(start, end); err != nil {
		return LogResult{}, err
	}
	if err := checkLogExpr(query); err != nil {
		return LogResult{}, err
	}
	if limit < 0 || limit > maxLogLimit {
		return LogResult{}, fmt.Errorf("limit phải từ 1 đến %d", maxLogLimit)
	}
	if limit == 0 {
		limit = defaultLogLimit
	}
	if direction == "" {
		direction = DirectionBackward
	}
	if direction != DirectionBackward && direction != DirectionForward {
		return LogResult{}, errors.New("direction phải là backward hoặc forward")
	}
	q := url.Values{"query": {query}, "start": {formatNS(start)}, "end": {formatNS(end)},
		"limit": {strconv.Itoa(limit)}, "direction": {direction}}
	data, err := c.get(ctx, "query_range", "/loki/api/v1/query_range", q, maxLogBody)
	if err != nil {
		return LogResult{}, err
	}
	var d queryData
	if err := json.Unmarshal(data, &d); err != nil {
		return LogResult{}, errBadResponse
	}
	switch d.ResultType {
	case "streams":
	case "matrix", "vector", "scalar":
		return LogResult{}, errors.New("query là một LogQL metric query; dùng tool query_metric")
	default:
		return LogResult{}, errBadResponse
	}
	var streams []rawStream
	if err := json.Unmarshal(d.Result, &streams); err != nil {
		return LogResult{}, errBadResponse
	}
	type entry struct {
		ns        int64
		labels    map[string]string
		truncated bool
		line      string
	}
	var all []entry
	for _, s := range streams {
		labels, truncated := labelSet(s.Stream)
		for _, v := range s.Values {
			// [ "<unix nanosecond>", "<dòng>" ]; phần tử thứ ba (metadata) nếu có thì bỏ qua.
			var ts, line string
			if len(v) < 2 || json.Unmarshal(v[0], &ts) != nil || json.Unmarshal(v[1], &line) != nil {
				return LogResult{}, errBadResponse
			}
			ns, err := strconv.ParseInt(ts, 10, 64)
			if err != nil {
				return LogResult{}, errBadResponse
			}
			all = append(all, entry{ns: ns, labels: labels, truncated: truncated, line: line})
		}
	}
	// Loki sắp xếp trong từng stream; gộp các stream theo thứ tự của direction.
	sort.SliceStable(all, func(i, j int) bool {
		if direction == DirectionForward {
			return all[i].ns < all[j].ns
		}
		return all[i].ns > all[j].ns
	})
	out := LogResult{Entries: []LogEntry{}}
	// Đủ limit dòng thì có thể còn dòng nữa; Loki không cho biết.
	more := len(all) >= limit
	if len(all) > limit {
		all, out.Truncated = all[:limit], true
	}
	budget := MaxLogResultBytes - envelopeReserve
	var last int64
	for _, e := range all {
		le := LogEntry{Timestamp: formatTime(e.ns), Labels: e.labels, LabelsTruncated: e.truncated,
			Line: cut(e.line, MaxLineBytes), LineTruncated: len(e.line) > MaxLineBytes}
		n := size(le)
		if n > maxEntryBytes {
			le.Labels, le.LabelsOmitted = map[string]string{}, true
			n = size(le)
		}
		if n > budget {
			more, out.Truncated = true, true
			break
		}
		budget -= n
		out.Entries = append(out.Entries, le)
		last = e.ns
	}
	if more && len(out.Entries) > 0 {
		if direction == DirectionBackward {
			// end của Loki là cận loại trừ: lần gọi kế nhận các dòng cũ hơn dòng cuối.
			if next := time.Unix(0, last); next.After(start) {
				out.NextEnd = formatTime(last)
			}
		} else if last < math.MaxInt64 {
			if next := time.Unix(0, last+1); next.Before(end) {
				out.NextStart = formatTime(last + 1)
			}
		}
	}
	return out, nil
}

// Sample là một điểm dữ liệu.
type Sample struct {
	T float64 `json:"t" jsonschema:"thời điểm, giây Unix"`
	V string  `json:"v" jsonschema:"giá trị dạng chuỗi như Loki trả"`
}

// Series là một chuỗi thời gian trong kết quả metric query.
type Series struct {
	Metric          map[string]string `json:"metric" jsonschema:"label của series; giá trị dài bị cắt"`
	LabelsTruncated bool              `json:"labels_truncated,omitempty" jsonschema:"true nếu một số label của series bị bỏ (quá nhiều label, hoặc tên trùng nhau sau khi cắt)"`
	Samples         []Sample          `json:"samples"`
}

// MetricResult là kết quả của một LogQL metric query, đã áp trần số series, số điểm
// và kích thước.
type MetricResult struct {
	Series    []Series `json:"series"`
	Truncated bool     `json:"truncated" jsonschema:"true nếu series hoặc điểm dữ liệu bị bỏ bớt do giới hạn; tổng hợp trong query (sum by, topk) để xem đủ"`
}

func parseSample(raw json.RawMessage) (Sample, bool) {
	var pair []json.RawMessage
	if json.Unmarshal(raw, &pair) != nil || len(pair) != 2 {
		return Sample{}, false
	}
	var s Sample
	if json.Unmarshal(pair[0], &s.T) != nil || json.Unmarshal(pair[1], &s.V) != nil {
		return Sample{}, false
	}
	s.V = cut(s.V, 64)
	return s, true
}

// QueryMetric chạy một LogQL metric query dạng range (rate, count_over_time, sum by...).
// Khoảng start–end cộng cửa sổ [ … ] lớn nhất của biểu thức tối đa 24 giờ (bộ kiểm từ
// vựng ở logql.go; không offset, @, subquery hay biến), step tối thiểu 10 giây, không quá
// 500 điểm mỗi series.
// limit 0 là mặc định (20 series), tối đa 50.
func (c *Client) QueryMetric(ctx context.Context, query string, start, end time.Time, step time.Duration, limit int) (MetricResult, error) {
	if err := checkQuery(query); err != nil {
		return MetricResult{}, err
	}
	if err := checkRange(start, end); err != nil {
		return MetricResult{}, err
	}
	if limit < 0 || limit > maxMetricSeries {
		return MetricResult{}, fmt.Errorf("limit phải từ 1 đến %d", maxMetricSeries)
	}
	if limit == 0 {
		limit = defaultMetricSeries
	}
	span := end.Sub(start)
	if err := checkMetricExpr(query, span); err != nil {
		return MetricResult{}, err
	}
	switch {
	case step < minStep || step > maxRange || step%time.Second != 0:
		return MetricResult{}, fmt.Errorf("step_seconds phải từ %d đến %d", int(minStep.Seconds()), int(maxRange.Seconds()))
	case int(span/step)+1 > maxPointsPerSeries:
		need := int(math.Ceil(span.Seconds() / float64(maxPointsPerSeries-1)))
		return MetricResult{}, fmt.Errorf("quá %d điểm mỗi series; với khoảng thời gian này step_seconds phải từ %d trở lên", maxPointsPerSeries, need)
	}
	q := url.Values{"query": {query}, "start": {formatNS(start)}, "end": {formatNS(end)},
		"step": {strconv.Itoa(int(step.Seconds()))}}
	data, err := c.get(ctx, "query_range", "/loki/api/v1/query_range", q, maxSmallBody)
	if err != nil {
		return MetricResult{}, err
	}
	var d queryData
	if err := json.Unmarshal(data, &d); err != nil {
		return MetricResult{}, errBadResponse
	}
	switch d.ResultType {
	case "matrix":
	case "streams":
		return MetricResult{}, errors.New("query là một LogQL log query; dùng tool query_logs")
	default:
		return MetricResult{}, errBadResponse
	}
	var raw []struct {
		Metric map[string]string `json:"metric"`
		Values []json.RawMessage `json:"values"`
	}
	if err := json.Unmarshal(d.Result, &raw); err != nil {
		return MetricResult{}, errBadResponse
	}
	out := MetricResult{Series: []Series{}}
	if len(raw) > limit {
		raw, out.Truncated = raw[:limit], true
	}
	budget := MaxResultBytes - envelopeReserve
	for _, r := range raw {
		s := Series{Samples: []Sample{}}
		s.Metric, s.LabelsTruncated = labelSet(r.Metric)
		values := r.Values
		// Giữ các điểm mới nhất (Loki trả điểm cũ nhất trước).
		if len(values) > maxPointsPerSeries {
			values, out.Truncated = values[len(values)-maxPointsPerSeries:], true
		}
		for _, v := range values {
			p, ok := parseSample(v)
			if !ok {
				return MetricResult{}, errBadResponse
			}
			s.Samples = append(s.Samples, p)
		}
		// Một series đơn lẻ vượt ngân sách: bỏ bớt điểm cũ. Vòng lặp chia đôi, có trần.
		for size(s) > budget && len(s.Samples) > 1 {
			s.Samples, out.Truncated = s.Samples[len(s.Samples)/2:], true
		}
		n := size(s)
		if n > budget {
			// Series này không vừa phần ngân sách còn lại: bỏ nó, xét tiếp series sau.
			out.Truncated = true
			continue
		}
		budget -= n
		out.Series = append(out.Series, s)
	}
	return out, nil
}

func pageArgs(offset, limit int) (int, int, error) {
	if limit < 0 || limit > maxListLimit {
		return 0, 0, fmt.Errorf("limit phải từ 1 đến %d", maxListLimit)
	}
	if offset < 0 || offset > maxOffset {
		return 0, 0, fmt.Errorf("offset phải từ 0 đến %d", maxOffset)
	}
	if limit == 0 {
		limit = defaultListLimit
	}
	return offset, limit, nil
}

// list gọi một endpoint trả danh sách chuỗi (tên label hoặc giá trị label) trong
// khoảng lookback gần đây, sắp xếp và phân trang.
func (c *Client) list(ctx context.Context, endpoint, path, drop string, lookback time.Duration, offset, limit int) (page []string, total, next, omitted int, err error) {
	offset, limit, err = pageArgs(offset, limit)
	if err != nil {
		return nil, 0, 0, 0, err
	}
	if lookback == 0 {
		lookback = defaultLookback
	}
	if lookback < time.Hour || lookback > maxLookbackHours*time.Hour || lookback%time.Hour != 0 {
		return nil, 0, 0, 0, fmt.Errorf("lookback_hours phải từ 1 đến %d", maxLookbackHours)
	}
	now := c.now()
	q := url.Values{"start": {formatNS(now.Add(-lookback))}, "end": {formatNS(now)}}
	data, err := c.get(ctx, endpoint, path, q, maxSmallBody)
	if err != nil {
		return nil, 0, 0, 0, err
	}
	// Loki bỏ trường data khi không có gì.
	var values []string
	if len(data) > 0 {
		if err := json.Unmarshal(data, &values); err != nil {
			return nil, 0, 0, 0, errBadResponse
		}
	}
	all := make([]string, 0, len(values))
	for _, v := range values {
		if v != drop {
			all = append(all, cut(v, maxLabelValueBytes))
		}
	}
	sort.Strings(all)
	page, next, omitted = fitPage(all, offset, limit, MaxResultBytes-envelopeReserve)
	return page, len(all), next, omitted, nil
}

// Labels liệt kê tên label có trong lookback gần đây (0 là 6 giờ; tối đa 24 giờ, theo
// giờ chẵn), theo thứ tự chữ cái.
func (c *Client) Labels(ctx context.Context, lookback time.Duration, offset, limit int) (page []string, total, next, omitted int, err error) {
	return c.list(ctx, "labels", "/loki/api/v1/labels", tenantLabel, lookback, offset, limit)
}

// LabelValues liệt kê giá trị của một label trong lookback gần đây, theo thứ tự chữ cái.
func (c *Client) LabelValues(ctx context.Context, name string, lookback time.Duration, offset, limit int) (page []string, total, next, omitted int, err error) {
	if !labelNameRe.MatchString(name) {
		return nil, 0, 0, 0, errors.New("label chỉ gồm chữ, số và '_', không bắt đầu bằng số (tối đa 128 ký tự)")
	}
	if name == tenantLabel {
		return nil, 0, 0, 0, errors.New("label này không đọc được qua tool")
	}
	return c.list(ctx, "label_values", "/loki/api/v1/label/"+url.PathEscape(name)+"/values", "", lookback, offset, limit)
}
