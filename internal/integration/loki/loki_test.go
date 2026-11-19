package loki_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hqgroups/mcp/internal/integration/contract"
	"github.com/hqgroups/mcp/internal/integration/loki"
	"github.com/hqgroups/mcp/internal/shared/signedctx"
)

// recorder ghi lại mọi request mà Loki giả lập nhận được.
type recorder struct {
	mu     sync.Mutex
	seen   []string
	auth   []string
	tenant [][]string        // mọi giá trị của header X-Scope-OrgID, theo request
	last   map[string]string // path → query string của request gần nhất
	// release được đóng để thả các truy vấn {app="hang"} đang treo.
	release chan struct{}
}

func newRecorder() *recorder {
	return &recorder{last: map[string]string{}, release: make(chan struct{})}
}

func (r *recorder) add(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, req.Method+" "+req.URL.Path)
	r.auth = append(r.auth, req.Header.Get("Authorization"))
	r.tenant = append(r.tenant, req.Header.Values("X-Scope-OrgID"))
	r.last[req.URL.Path] = req.URL.RawQuery
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.seen)
}

func (r *recorder) requests() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

func (r *recorder) tenants() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]string(nil), r.tenant...)
}

func (r *recorder) lastQuery(path string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last[path]
}

func success(w http.ResponseWriter, data string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"status":"success","data":%s}`, data)
}

// Loki trả lỗi dạng văn bản thuần kèm mã HTTP.
func failure(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(msg))
}

// base là timestamp (nanosecond) của dòng cũ nhất trong các stream giả lập.
const base = int64(1794960000) * 1e9 // 2026-11-18T00:00:00Z

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// stream tạo một stream có n dòng, dòng i ở base + offset + i giây, theo thứ tự mới
// nhất trước (như Loki trả với direction=backward).
func stream(labels string, n int, offset int64, line func(i int) string) string {
	values := make([]string, 0, n)
	for i := n - 1; i >= 0; i-- {
		values = append(values, fmt.Sprintf(`["%d",%s]`, base+offset+int64(i)*1e9, jsonString(line(i))))
	}
	return `{"stream":` + labels + `,"values":[` + strings.Join(values, ",") + `]}`
}

func streams(items ...string) string {
	return `{"resultType":"streams","result":[` + strings.Join(items, ",") + `],"stats":{"summary":{"bytesProcessedPerSecond":1}}}`
}

// matrix tạo n series, mỗi series có points điểm.
func matrix(n, points int) string {
	items := make([]string, n)
	for i := range items {
		values := make([]string, points)
		for j := range values {
			values[j] = fmt.Sprintf(`[%d,"%d.5"]`, 1794960000+j*15, j)
		}
		items[i] = fmt.Sprintf(`{"metric":{"app":"api","level":"lvl-%03d"},"values":[%s]}`, i, strings.Join(values, ","))
	}
	return `{"resultType":"matrix","result":[` + strings.Join(items, ",") + `],"stats":{}}`
}

const leak = `failed to get s3 object: Get "https://minio:s3cr3t@storage.internal:9000/chunks/fake?X-Amz-Signature=abc123": dial tcp 10.0.0.9:9000: i/o timeout`

func names(prefix string, n int) string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf(`"%s_%04d"`, prefix, n-1-i) // không sắp xếp sẵn
	}
	return "[" + strings.Join(out, ",") + "]"
}

// fakeLoki là HTTP API tối thiểu của Loki, theo định dạng ở
// https://grafana.com/docs/loki/latest/reference/loki-http-api/. Nó cố ý bỏ qua tham số
// limit để trần phía server được kiểm.
func fakeLoki(rec *recorder) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /loki/api/v1/query_range", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch query := q.Get("query"); {
		case query == `{app="api"}`:
			// Hai stream xen kẽ nhau theo thời gian.
			success(w, streams(
				stream(`{"app":"api","pod":"api-1"}`, 3, 0, func(i int) string { return fmt.Sprintf("api-1 line %d", i) }),
				stream(`{"app":"api","pod":"api-2"}`, 2, 5e8, func(i int) string { return fmt.Sprintf("api-2 line %d", i) })))
		case query == `{app="big"}`:
			// 1500 dòng × 500 byte: quá limit và quá trần kích thước.
			success(w, streams(stream(`{"app":"big"}`, 1500, 0, func(i int) string {
				return fmt.Sprintf("%06d ", i) + strings.Repeat("x", 493)
			})))
		case query == `{app="long"}`:
			// Dòng dài toàn ký tự bị escape thành 6 byte, kèm UTF-8 hỏng và ký tự điều khiển.
			success(w, streams(stream(`{"app":"long"}`, 3, 0, func(i int) string {
				return "\x00\xff" + strings.Repeat("<", 20000)
			})))
		case query == `{app="fat"}`:
			// Label phình to: 40 label × 250 ký tự bị escape.
			labels := `{"app":"fat"`
			for i := range 40 {
				labels += fmt.Sprintf(`,"l%02d":"%s"`, i, strings.Repeat("<", 250))
			}
			success(w, streams(stream(labels+"}", 2, 0, func(i int) string { return fmt.Sprintf("fat line %d", i) })))
		case query == `{app="none"}`:
			success(w, `{"resultType":"streams","result":[]}`)
		case query == `{app="meta"}`:
			// Phần tử thứ ba (structured metadata) của bản Loki mới được bỏ qua.
			success(w, fmt.Sprintf(`{"resultType":"streams","result":[{"stream":{"app":"meta"},"values":[["%d","with meta",{"structuredMetadata":{"trace_id":"abc"}}]]}]}`, base))
		case query == `{app="secret"}`:
			success(w, streams(stream(`{"app":"secret"}`, 1, 0, func(int) string { return "login ok password=hunter2" })))
		case query == `{app="tenant"}`:
			// Truy vấn liên tenant: Loki gắn label __tenant_id__ vào mỗi stream.
			success(w, streams(stream(`{"app":"tenant","__tenant_id__":"team-a"}`, 2, 0, func(i int) string { return fmt.Sprintf("tenant line %d", i) })))
		case query == `sum by (__tenant_id__) (rate({app="tenant"}[1m]))`:
			success(w, `{"resultType":"matrix","result":[{"metric":{"__tenant_id__":"team-a","app":"tenant"},"values":[[1794960000,"1"]]}]}`)
		case query == `{app="tenant-error"}`:
			failure(w, http.StatusBadRequest, "max entries limit per query exceeded for tenant team-a (user team-a)")
		case query == `{app="hang"}`:
			// Treo tới khi test thả hoặc client bỏ kết nối.
			select {
			case <-rec.release:
			case <-r.Context().Done():
			}
			success(w, `{"resultType":"streams","result":[]}`)
		case query == `{bad`:
			failure(w, http.StatusBadRequest, "parse error at line 1, col 5: syntax error: unexpected $end, expecting = or != or =~ or !~\n")
		case query == `{app="leak"}`:
			failure(w, http.StatusInternalServerError, leak)
		case query == `{app="json-error"}`:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "error", "errorType": "execution", "error": leak})
		case query == `{app="slow"}`:
			failure(w, http.StatusGatewayTimeout, "context deadline exceeded")
		case query == "wide":
			success(w, matrix(100, 600))
		case strings.HasPrefix(query, "{"):
			success(w, `{"resultType":"streams","result":[]}`)
		default:
			start, _ := strconv.ParseInt(q.Get("start"), 10, 64)
			end, _ := strconv.ParseInt(q.Get("end"), 10, 64)
			step, _ := strconv.ParseInt(q.Get("step"), 10, 64)
			if step <= 0 {
				step = 60 // Loki tự chọn step khi không có tham số
			}
			success(w, matrix(60, int((end-start)/1e9/step)+1))
		}
	})
	mux.HandleFunc("GET /loki/api/v1/labels", func(w http.ResponseWriter, _ *http.Request) {
		success(w, `["__tenant_id__",`+names("label", 300)[1:])
	})
	mux.HandleFunc("GET /loki/api/v1/label/{name}/values", func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("name") {
		case "app":
			success(w, names("app", 1300))
		case "huge":
			// Một giá trị tự nó vượt cả ngân sách không tồn tại sau khi cắt 256 byte; kiểm cắt.
			success(w, `["`+strings.Repeat("v", 5000)+`","small"]`)
		default:
			// Loki bỏ trường data khi label không có giá trị.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"success"}`))
		}
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		mux.ServeHTTP(w, r)
	})
}

var (
	testKey = []byte(strings.Repeat("k", 32))
	testOrg = uuid.New()
)

type headerTransport struct{ signer *signedctx.Signer }

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set(signedctx.Header, h.signer.Sign(signedctx.Context{Kind: signedctx.KindCall, Audience: loki.Slug,
		OrgID: testOrg, ProjectID: uuid.New(), Initiator: "user:" + uuid.NewString()}, time.Now()))
	// Header tenant của Loki trên request MCP không có nghĩa gì với server.
	r.Header.Set("X-Scope-OrgID", "from-mcp-request")
	return http.DefaultTransport.RoundTrip(r)
}

// connect dựng server MCP trên client đã cho và nối một MCP client thật vào, với ngữ
// cảnh ký của org sở hữu.
func connect(t *testing.T, client *loki.Client) *mcp.ClientSession {
	t.Helper()
	signer, _ := signedctx.NewSigner(testKey)
	srv := httptest.NewServer(loki.Handler(client, signer, testOrg))
	t.Cleanup(srv.Close)
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	sess, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: srv.URL, DisableStandaloneSSE: true, MaxRetries: -1,
		HTTPClient: &http.Client{Transport: headerTransport{signer}},
	}, nil)
	if err != nil {
		t.Fatalf("không kết nối được với ngữ cảnh hợp lệ: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

func newFake(t *testing.T, opts loki.Options) (*loki.Client, *recorder) {
	t.Helper()
	rec := newRecorder()
	api := httptest.NewServer(fakeLoki(rec))
	t.Cleanup(api.Close)
	client, err := loki.NewClient(api.URL, opts)
	if err != nil {
		t.Fatal(err)
	}
	return client, rec
}

// call gọi một tool và trả về kết quả cùng phần có cấu trúc dạng JSON (hoặc văn bản lỗi).
func call(t *testing.T, sess *mcp.ClientSession, name string, args any) (*mcp.CallToolResult, string) {
	t.Helper()
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	if res.IsError || res.StructuredContent == nil {
		text := ""
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				text += tc.Text
			}
		}
		return res, text
	}
	b, _ := json.Marshal(res.StructuredContent)
	return res, string(b)
}

func decode[T any](t *testing.T, out string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("kết quả không đọc được: %v: %.200s", err, out)
	}
	return v
}

type logsOut struct {
	Entries []struct {
		Timestamp       string            `json:"timestamp"`
		Labels          map[string]string `json:"labels"`
		Line            string            `json:"line"`
		LineTruncated   bool              `json:"line_truncated"`
		LabelsOmitted   bool              `json:"labels_omitted"`
		LabelsTruncated bool              `json:"labels_truncated"`
	} `json:"entries"`
	Truncated bool   `json:"truncated"`
	NextEnd   string `json:"next_end"`
	NextStart string `json:"next_start"`
}

type metricOut struct {
	Series []struct {
		Metric  map[string]string `json:"metric"`
		Samples []struct {
			T float64 `json:"t"`
			V string  `json:"v"`
		} `json:"samples"`
	} `json:"series"`
	Truncated bool `json:"truncated"`
}

type listOut struct {
	Labels     []string `json:"labels"`
	Values     []string `json:"values"`
	Total      int      `json:"total"`
	NextOffset int      `json:"next_offset"`
}

const (
	rangeStart = "2026-11-18T00:00:00Z"
	rangeEnd   = "2026-11-18T01:00:00Z"
	queryPath  = "/loki/api/v1/query_range"
)

func logArgs(query string, extra map[string]any) map[string]any {
	args := map[string]any{"query": query, "start": rangeStart, "end": rangeEnd}
	for k, v := range extra {
		args[k] = v
	}
	return args
}

// Bộ test hợp đồng dùng chung của managed MCP server (mcp-platform §7).
func TestContract(t *testing.T) {
	client, rec := newFake(t, loki.Options{TenantID: "team-a"})
	signer, _ := signedctx.NewSigner(testKey)
	contract.Run(t, contract.Server{
		Slug:           loki.Slug,
		Handler:        loki.Handler(client, signer, testOrg),
		SigningKey:     testKey,
		OrgID:          testOrg,
		TargetRequests: rec.count,
		Tools: []contract.Tool{
			{Name: "query_logs", MaxResultBytes: loki.MaxLogResultBytes, Samples: []map[string]any{
				logArgs(`{app="api"}`, nil), logArgs(`{app="big"}`, map[string]any{"limit": 1000}),
				logArgs(`{app="big"}`, map[string]any{"limit": 1000, "direction": "forward"}),
				logArgs(`{app="long"}`, nil), logArgs(`{app="fat"}`, nil)}},
			{Name: "query_metric", MaxResultBytes: loki.MaxResultBytes, Samples: []map[string]any{
				{"query": `sum by (level) (count_over_time({app="api"}[5m]))`, "start": rangeStart, "end": rangeEnd, "step_seconds": 60},
				{"query": `rate({app="api"}[1m])`, "start": rangeStart, "end": rangeEnd, "step_seconds": 10, "limit": 50},
				{"query": "wide", "start": rangeStart, "end": rangeEnd, "step_seconds": 60, "limit": 50}}},
			{Name: "list_labels", MaxResultBytes: loki.MaxResultBytes, Samples: []map[string]any{
				{}, {"limit": 500, "lookback_hours": 24}}},
			{Name: "list_label_values", MaxResultBytes: loki.MaxResultBytes, Samples: []map[string]any{
				{"label": "app"}, {"label": "app", "limit": 500, "offset": 1000}, {"label": "huge"}}},
		},
	})
}

func TestQueryLogs(t *testing.T) {
	client, rec := newFake(t, loki.Options{})
	sess := connect(t, client)

	// Các stream được gộp theo thời gian, mới nhất trước; mỗi dòng mang label của stream.
	res, out := call(t, sess, "query_logs", logArgs(`{app="api"}`, nil))
	l := decode[logsOut](t, out)
	if res.IsError || len(l.Entries) != 5 || l.Truncated || l.NextEnd != "" || l.NextStart != "" ||
		l.Entries[0].Line != "api-1 line 2" || l.Entries[1].Line != "api-2 line 1" || l.Entries[4].Line != "api-1 line 0" ||
		l.Entries[1].Labels["pod"] != "api-2" || l.Entries[0].Timestamp != "2026-11-18T00:00:02Z" ||
		l.Entries[1].Timestamp != "2026-11-18T00:00:01.5Z" {
		t.Errorf("query_logs: %s", out)
	}
	// start / end được gửi dạng Unix nanosecond; limit và direction có mặc định.
	if got := rec.lastQuery(queryPath); !strings.Contains(got, "start=1794960000000000000") || !strings.Contains(got, "end=1794963600000000000") ||
		!strings.Contains(got, "limit=100") || !strings.Contains(got, "direction=backward") || strings.Contains(got, "step=") {
		t.Errorf("tham số gửi cho Loki: %s", got)
	}
	_, out = call(t, sess, "query_logs", logArgs(`{app="api"}`, map[string]any{"direction": "forward", "limit": 2}))
	l = decode[logsOut](t, out)
	if len(l.Entries) != 2 || !l.Truncated || l.Entries[0].Line != "api-1 line 0" || l.Entries[1].Line != "api-2 line 0" ||
		l.NextStart != "2026-11-18T00:00:00.500000001Z" || l.NextEnd != "" {
		t.Errorf("forward + limit: %s", out)
	}
	if got := rec.lastQuery(queryPath); !strings.Contains(got, "limit=2") || !strings.Contains(got, "direction=forward") {
		t.Errorf("tham số forward: %s", got)
	}
	_, out = call(t, sess, "query_logs", logArgs(`{app="none"}`, nil))
	if l := decode[logsOut](t, out); len(l.Entries) != 0 || l.Truncated || !strings.Contains(out, `"entries":[]`) {
		t.Errorf("không có dòng nào: %s", out)
	}
	_, out = call(t, sess, "query_logs", logArgs(`{app="meta"}`, nil))
	if l := decode[logsOut](t, out); len(l.Entries) != 1 || l.Entries[0].Line != "with meta" {
		t.Errorf("phần tử metadata thứ ba phải được bỏ qua: %s", out)
	}

	// Trần số dòng kể cả khi Loki bỏ qua limit, và trần kích thước 60 KiB: giữ phần mới
	// nhất, con trỏ next_end là timestamp của dòng cũ nhất còn giữ.
	_, out = call(t, sess, "query_logs", logArgs(`{app="big"}`, map[string]any{"limit": 1000}))
	l = decode[logsOut](t, out)
	n := len(l.Entries)
	if len(out) > loki.MaxLogResultBytes || !l.Truncated || n < 50 || n >= 1000 ||
		!strings.HasPrefix(l.Entries[0].Line, "001499 ") || !strings.HasPrefix(l.Entries[n-1].Line, fmt.Sprintf("%06d ", 1500-n)) ||
		l.NextEnd != l.Entries[n-1].Timestamp || l.NextStart != "" {
		t.Errorf("kết quả lớn phải bị cắt, giữ phần mới nhất: %d byte, %d dòng, truncated=%v next_end=%q", len(out), n, l.Truncated, l.NextEnd)
	}
	_, out = call(t, sess, "query_logs", logArgs(`{app="big"}`, map[string]any{"limit": 20}))
	if l := decode[logsOut](t, out); len(l.Entries) != 20 || !l.Truncated || l.NextEnd != l.Entries[19].Timestamp {
		t.Errorf("limit 20 phải được áp ở server: %d dòng, %s", len(l.Entries), l.NextEnd)
	}
	// forward: giữ phần đầu khoảng thời gian, con trỏ đi tiếp về phía mới hơn.
	_, out = call(t, sess, "query_logs", logArgs(`{app="big"}`, map[string]any{"limit": 1000, "direction": "forward"}))
	l = decode[logsOut](t, out)
	if n := len(l.Entries); len(out) > loki.MaxLogResultBytes || !l.Truncated || n < 50 || !strings.HasPrefix(l.Entries[0].Line, "000000 ") ||
		l.NextStart == "" || l.NextEnd != "" {
		t.Errorf("forward lớn: %d byte, %d dòng, next_start=%q", len(out), n, l.NextStart)
	}

	// Dòng quá dài bị cắt có đánh dấu; UTF-8 hỏng không làm hỏng JSON.
	_, out = call(t, sess, "query_logs", logArgs(`{app="long"}`, nil))
	l = decode[logsOut](t, out)
	if len(l.Entries) != 3 || !l.Entries[0].LineTruncated || len(l.Entries[0].Line) > loki.MaxLineBytes+len("…") ||
		!strings.HasSuffix(l.Entries[0].Line, "…") || len(out) > loki.MaxLogResultBytes {
		t.Errorf("dòng dài phải bị cắt có đánh dấu: %d dòng, %d byte", len(l.Entries), len(out))
	}
	// Label phình to: số label và độ dài có trần; dòng vẫn được trả, không che dòng sau.
	_, out = call(t, sess, "query_logs", logArgs(`{app="fat"}`, nil))
	l = decode[logsOut](t, out)
	if len(l.Entries) != 2 || l.Entries[1].Line != "fat line 0" || len(out) > loki.MaxLogResultBytes ||
		(!l.Entries[0].LabelsOmitted && (!l.Entries[0].LabelsTruncated || len(l.Entries[0].Labels) != 32)) {
		t.Errorf("label phình to: %d dòng, %d byte", len(l.Entries), len(out))
	}

	// Tool không lọc nội dung log (runbook nêu rõ); test ghi nhận hành vi đó.
	_, out = call(t, sess, "query_logs", logArgs(`{app="secret"}`, nil))
	if !strings.Contains(out, "password=hunter2") {
		t.Errorf("dòng log được trả nguyên văn: %s", out)
	}

	// Metric query gửi vào query_logs là lỗi tool chỉ sang tool đúng.
	res, out = call(t, sess, "query_logs", logArgs(`rate({app="api"}[1m])`, nil))
	if !res.IsError || !strings.Contains(out, "query_metric") {
		t.Errorf("metric query trong query_logs: %s", out)
	}

	// Đầu vào ngoài giới hạn là lỗi tool và không tới Loki.
	before := rec.count()
	for name, args := range map[string]map[string]any{
		"query rỗng":           logArgs("  ", nil),
		"query quá dài":        logArgs(strings.Repeat("a", 2001), nil),
		"thiếu query":          {"start": rangeStart, "end": rangeEnd},
		"thiếu start":          {"query": `{app="api"}`, "end": rangeEnd},
		"thiếu end":            {"query": `{app="api"}`, "start": rangeStart},
		"start không phải RFC": {"query": `{app="api"}`, "start": "1h ago", "end": rangeEnd},
		"start là epoch":       {"query": `{app="api"}`, "start": "1794960000", "end": rangeEnd},
		"ngày không tồn tại":   {"query": `{app="api"}`, "start": "2026-13-45T00:00:00Z", "end": rangeEnd},
		"khoảng quá 24 giờ":    {"query": `{app="api"}`, "start": "2026-11-17T00:00:00Z", "end": "2026-11-18T00:00:01Z"},
		"end trước start":      {"query": `{app="api"}`, "start": rangeEnd, "end": rangeStart},
		"start trước năm 2000": {"query": `{app="api"}`, "start": "1999-12-31T23:00:00Z", "end": "2000-01-01T01:00:00Z"},
		"năm 0001":             {"query": `{app="api"}`, "start": "0001-01-01T00:00:00Z", "end": "0001-01-01T01:00:00Z"},
		"sau năm 2200":         {"query": `{app="api"}`, "start": "2300-01-01T00:00:00Z", "end": "2300-01-01T01:00:00Z"},
		"năm 9999":             {"query": `{app="api"}`, "start": "9999-12-31T22:00:00Z", "end": "9999-12-31T23:00:00Z"},
		"end bằng start":       {"query": `{app="api"}`, "start": rangeStart, "end": rangeStart},
		"limit vượt trần":      logArgs(`{app="api"}`, map[string]any{"limit": 1001}),
		"limit âm":             logArgs(`{app="api"}`, map[string]any{"limit": -1}),
		"direction ngoài enum": logArgs(`{app="api"}`, map[string]any{"direction": "BACKWARD"}),
		"trường lạ since":      logArgs(`{app="api"}`, map[string]any{"since": "720h"}),
		"trường lạ interval":   logArgs(`{app="api"}`, map[string]any{"interval": "1s"}),
	} {
		if res, out := call(t, sess, "query_logs", args); !res.IsError {
			t.Errorf("%s: phải là lỗi tool, nhận %.200s", name, out)
		}
	}
	if n := rec.count() - before; n != 0 {
		t.Errorf("đầu vào xấu đã tới Loki: %d request", n)
	}
	// Đúng 24 giờ thì được.
	if res, out := call(t, sess, "query_logs", map[string]any{"query": `{app="none"}`, "start": "2026-11-17T00:00:00Z", "end": "2026-11-18T00:00:00Z"}); res.IsError {
		t.Errorf("khoảng đúng 24 giờ phải chạy được: %s", out)
	}
}

// Đọc tiếp bằng next_end đi hết được các dòng, không lặp và không sót (khi timestamp
// không trùng nhau).
func TestQueryLogsCursorWalksBackward(t *testing.T) {
	const total = 700
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		end, _ := strconv.ParseInt(q.Get("end"), 10, 64)
		start, _ := strconv.ParseInt(q.Get("start"), 10, 64)
		limit, _ := strconv.Atoi(q.Get("limit"))
		var values []string
		for i := total - 1; i >= 0 && len(values) < limit; i-- {
			if ts := base + int64(i)*1e9; ts >= start && ts < end {
				values = append(values, fmt.Sprintf(`["%d","%06d %s"]`, ts, i, strings.Repeat("y", 400)))
			}
		}
		success(w, `{"resultType":"streams","result":[{"stream":{"app":"walk"},"values":[`+strings.Join(values, ",")+`]}]}`)
	}))
	t.Cleanup(api.Close)
	client, err := loki.NewClient(api.URL, loki.Options{})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Unix(0, base)
	end := start.Add(time.Hour)
	seen := map[string]bool{}
	for range 50 {
		res, err := client.QueryLogs(context.Background(), `{app="walk"}`, start, end, 500, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range res.Entries {
			if seen[e.Timestamp] {
				t.Fatalf("dòng %s được trả hai lần", e.Timestamp)
			}
			seen[e.Timestamp] = true
		}
		if res.NextEnd == "" {
			break
		}
		if end, err = time.Parse(time.RFC3339Nano, res.NextEnd); err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != total {
		t.Errorf("con trỏ phải đi hết %d dòng, được %d", total, len(seen))
	}
}

func TestQueryMetric(t *testing.T) {
	client, rec := newFake(t, loki.Options{})
	sess := connect(t, client)

	res, out := call(t, sess, "query_metric", map[string]any{"query": `rate({app="api"}[1m])`, "start": rangeStart, "end": rangeEnd, "step_seconds": 60})
	m := decode[metricOut](t, out)
	if res.IsError || len(m.Series) != 20 || !m.Truncated || len(m.Series[0].Samples) != 61 ||
		m.Series[0].Metric["level"] != "lvl-000" || m.Series[0].Samples[1].V != "1.5" || len(out) > loki.MaxResultBytes {
		t.Errorf("query_metric (mặc định 20 series): %d byte, %d series, truncated=%v", len(out), len(m.Series), m.Truncated)
	}
	if got := rec.lastQuery(queryPath); !strings.Contains(got, "step=60") || !strings.Contains(got, "start=1794960000000000000") ||
		strings.Contains(got, "limit=") || strings.Contains(got, "direction=") {
		t.Errorf("tham số query_metric: %s", got)
	}
	// 100 series × 600 điểm bị cắt dưới 64 KiB; mỗi series giữ các điểm mới nhất.
	_, out = call(t, sess, "query_metric", map[string]any{"query": "wide", "start": rangeStart, "end": rangeEnd, "step_seconds": 60, "limit": 50})
	m = decode[metricOut](t, out)
	if len(out) > loki.MaxResultBytes || !m.Truncated || len(m.Series) == 0 || len(m.Series[0].Samples) > 500 ||
		m.Series[0].Samples[len(m.Series[0].Samples)-1].V != "599.5" {
		t.Errorf("kết quả lớn phải bị cắt: %d byte, %d series", len(out), len(m.Series))
	}
	// Log query gửi vào query_metric là lỗi tool chỉ sang tool đúng.
	res, out = call(t, sess, "query_metric", map[string]any{"query": `{app="api"}`, "start": rangeStart, "end": rangeEnd, "step_seconds": 60})
	if !res.IsError || !strings.Contains(out, "query_logs") {
		t.Errorf("log query trong query_metric: %s", out)
	}

	before := rec.count()
	for name, args := range map[string]map[string]any{
		"khoảng quá 24 giờ": {"query": "x", "start": "2026-11-16T00:00:00Z", "end": "2026-11-18T00:00:00Z", "step_seconds": 3600},
		"end trước start":   {"query": "x", "start": rangeEnd, "end": rangeStart, "step_seconds": 60},
		"step dưới 10 giây": {"query": "x", "start": rangeStart, "end": rangeEnd, "step_seconds": 1},
		"quá 500 điểm":      {"query": "x", "start": "2026-11-17T00:00:00Z", "end": "2026-11-18T00:00:00Z", "step_seconds": 60},
		"thiếu step":        {"query": "x", "start": rangeStart, "end": rangeEnd},
		"limit vượt trần":   {"query": "x", "start": rangeStart, "end": rangeEnd, "step_seconds": 60, "limit": 51},
		"trường lạ":         {"query": "x", "start": rangeStart, "end": rangeEnd, "step_seconds": 60, "direction": "forward"},
	} {
		if res, out := call(t, sess, "query_metric", args); !res.IsError {
			t.Errorf("%s: phải là lỗi tool, nhận %.200s", name, out)
		}
	}
	if n := rec.count() - before; n != 0 {
		t.Errorf("đầu vào xấu đã tới Loki: %d request", n)
	}
	// Thông báo lỗi chỉ ra step nhỏ nhất dùng được.
	_, out = call(t, sess, "query_metric", map[string]any{"query": "x", "start": "2026-11-17T00:00:00Z", "end": "2026-11-18T00:00:00Z", "step_seconds": 60})
	if !strings.Contains(out, "174") {
		t.Errorf("lỗi quá số điểm phải nêu step tối thiểu: %s", out)
	}
}

func TestLists(t *testing.T) {
	client, rec := newFake(t, loki.Options{})
	sess := connect(t, client)

	res, out := call(t, sess, "list_labels", map[string]any{})
	l := decode[listOut](t, out)
	if res.IsError || l.Total != 300 || len(l.Labels) != 100 || l.Labels[0] != "label_0000" || l.NextOffset != 100 {
		t.Errorf("list_labels: total=%d n=%d next=%d", l.Total, len(l.Labels), l.NextOffset)
	}
	// Khoảng thời gian gửi cho Loki luôn tường minh và có trần: mặc định 6 giờ.
	window := func(path string) time.Duration {
		q := rec.lastQuery(path)
		var start, end int64
		for _, kv := range strings.Split(q, "&") {
			k, v, _ := strings.Cut(kv, "=")
			n, _ := strconv.ParseInt(v, 10, 64)
			switch k {
			case "start":
				start = n
			case "end":
				end = n
			default:
				t.Errorf("tham số lạ gửi cho Loki: %s", kv)
			}
		}
		return time.Duration(end - start)
	}
	if got := window("/loki/api/v1/labels"); got != 6*time.Hour {
		t.Errorf("lookback mặc định: %s", got)
	}
	call(t, sess, "list_labels", map[string]any{"lookback_hours": 24})
	if got := window("/loki/api/v1/labels"); got != 24*time.Hour {
		t.Errorf("lookback 24 giờ: %s", got)
	}

	// Giá trị label: sắp xếp, phân trang đi hết được.
	seen, offset := 0, 0
	for range 10 {
		_, out = call(t, sess, "list_label_values", map[string]any{"label": "app", "limit": 500, "offset": offset})
		l = decode[listOut](t, out)
		if offset == 0 && (l.Total != 1300 || l.Values[0] != "app_0000") {
			t.Errorf("list_label_values: total=%d đầu=%q", l.Total, l.Values[0])
		}
		seen += len(l.Values)
		if offset = l.NextOffset; offset == 0 {
			break
		}
	}
	if seen != 1300 {
		t.Errorf("phân trang phải đi hết 1300 giá trị, được %d", seen)
	}
	if got := window("/loki/api/v1/label/app/values"); got != 6*time.Hour {
		t.Errorf("lookback của label values: %s", got)
	}
	_, out = call(t, sess, "list_label_values", map[string]any{"label": "huge"})
	if l := decode[listOut](t, out); l.Total != 2 || len(l.Values) != 2 || len(l.Values[1]) > 256+len("…") {
		t.Errorf("giá trị dài phải bị cắt: %.200s", out)
	}
	// Loki bỏ trường data khi không có giá trị.
	res, out = call(t, sess, "list_label_values", map[string]any{"label": "missing"})
	if l := decode[listOut](t, out); res.IsError || l.Total != 0 || !strings.Contains(out, `"values":[]`) {
		t.Errorf("label không có giá trị: %s", out)
	}
	_, out = call(t, sess, "list_labels", map[string]any{"offset": 5000})
	if l := decode[listOut](t, out); len(l.Labels) != 0 || l.NextOffset != 0 {
		t.Errorf("offset quá cuối: %s", out)
	}

	before := rec.count()
	for name, tc := range map[string]struct {
		tool string
		args map[string]any
	}{
		"limit vượt trần":         {"list_labels", map[string]any{"limit": 501}},
		"offset âm":               {"list_labels", map[string]any{"offset": -1}},
		"lookback vượt trần":      {"list_labels", map[string]any{"lookback_hours": 25}},
		"lookback âm":             {"list_labels", map[string]any{"lookback_hours": -1}},
		"trường lạ":               {"list_labels", map[string]any{"query": `{app="x"}`}},
		"thiếu label":             {"list_label_values", map[string]any{}},
		"label đi ngược thư mục":  {"list_label_values", map[string]any{"label": "../../push"}},
		"label có dấu gạch chéo":  {"list_label_values", map[string]any{"label": "app/values"}},
		"label chèn query string": {"list_label_values", map[string]any{"label": "app?start=0"}},
		"label mã hoá phần trăm":  {"list_label_values", map[string]any{"label": "app%2F..%2Fpush"}},
		"label bắt đầu bằng số":   {"list_label_values", map[string]any{"label": "1app"}},
		"label quá dài":           {"list_label_values", map[string]any{"label": strings.Repeat("a", 129)}},
	} {
		if res, out := call(t, sess, tc.tool, tc.args); !res.IsError {
			t.Errorf("%s: phải là lỗi tool, nhận %.200s", name, out)
		}
	}
	if n := rec.count() - before; n != 0 {
		t.Errorf("đầu vào xấu đã tới Loki: %d request", n)
	}
}

// Server chỉ đọc: đúng bốn tool, và chỉ GET tới ba endpoint đọc.
func TestReadOnlySurface(t *testing.T) {
	client, rec := newFake(t, loki.Options{})
	sess := connect(t, client)
	list, err := sess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range list.Tools {
		names = append(names, tool.Name)
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("tool %s phải mang readOnlyHint", tool.Name)
		}
	}
	if got := strings.Join(names, ","); got != "list_label_values,list_labels,query_logs,query_metric" {
		t.Errorf("danh sách tool: %s", got)
	}
	call(t, sess, "query_logs", logArgs(`{app="api"}`, nil))
	call(t, sess, "query_metric", map[string]any{"query": "rate", "start": rangeStart, "end": rangeEnd, "step_seconds": 60})
	call(t, sess, "list_labels", map[string]any{})
	call(t, sess, "list_label_values", map[string]any{"label": "app"})
	want := []string{"GET /loki/api/v1/query_range", "GET /loki/api/v1/query_range", "GET /loki/api/v1/labels", "GET /loki/api/v1/label/app/values"}
	if got := rec.requests(); strings.Join(got, ";") != strings.Join(want, ";") {
		t.Errorf("request tới Loki: %v, muốn %v", got, want)
	}
}

// Tenant của Loki (X-Scope-OrgID) là cấu hình phía server: mọi request mang đúng giá trị
// người vận hành đặt; tham số tool, nội dung query và header của request MCP không đổi
// được nó.
func TestTenantHeaderIsOperatorConfig(t *testing.T) {
	client, rec := newFake(t, loki.Options{TenantID: " team-a\n"})
	sess := connect(t, client)

	call(t, sess, "query_logs", logArgs(`{app="api"}`, nil))
	call(t, sess, "query_logs", logArgs("{app=\"api\"} |= `X-Scope-OrgID: other\r\nX-Scope-OrgID: other`", nil))
	call(t, sess, "query_metric", map[string]any{"query": "rate", "start": rangeStart, "end": rangeEnd, "step_seconds": 60})
	call(t, sess, "list_labels", map[string]any{})
	call(t, sess, "list_label_values", map[string]any{"label": "app"})
	before := rec.count()
	if before != 5 {
		t.Fatalf("muốn 5 request tới Loki, có %d", before)
	}
	// Không tool nào có tham số chọn tenant: mọi tên có thể nghĩ tới đều là trường lạ.
	for _, field := range []string{"tenant", "tenant_id", "org_id", "x_scope_org_id", "X-Scope-OrgID", "scope_org_id"} {
		args := logArgs(`{app="api"}`, map[string]any{field: "other"})
		if res, out := call(t, sess, "query_logs", args); !res.IsError {
			t.Errorf("tham số %s phải bị từ chối: %.200s", field, out)
		}
		if res, out := call(t, sess, "list_label_values", map[string]any{"label": "app", field: "other"}); !res.IsError {
			t.Errorf("list_label_values: tham số %s phải bị từ chối: %.200s", field, out)
		}
	}
	if n := rec.count() - before; n != 0 {
		t.Errorf("request có tham số tenant đã tới Loki: %d", n)
	}
	for i, got := range rec.tenants()[:before] {
		if len(got) != 1 || got[0] != "team-a" {
			t.Errorf("request %d: X-Scope-OrgID = %q, muốn đúng một giá trị team-a", i, got)
		}
	}
	list, err := sess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range list.Tools {
		schema, _ := json.Marshal(tool.InputSchema)
		if s := strings.ToLower(string(schema) + tool.Description); strings.Contains(s, "tenant") || strings.Contains(s, "scope") || strings.Contains(s, "org_id") {
			t.Errorf("tool %s nhắc tới tenant trong schema hoặc mô tả", tool.Name)
		}
	}

	// Tenant id không rời server: label __tenant_id__ bị bỏ khỏi dòng log, series và danh
	// sách label; lỗi 4xx của Loki nhắc tới tenant thì tên bị che; label đó không đọc
	// giá trị được.
	for name, tc := range map[string]struct {
		tool string
		args map[string]any
		want string
	}{
		"dòng log":      {"query_logs", logArgs(`{app="tenant"}`, nil), "tenant line 1"},
		"series":        {"query_metric", map[string]any{"query": `sum by (__tenant_id__) (rate({app="tenant"}[1m]))`, "start": rangeStart, "end": rangeEnd, "step_seconds": 60}, `"app":"tenant"`},
		"danh sách":     {"list_labels", map[string]any{"limit": 500}, "label_0000"},
		"lỗi 4xx":       {"query_logs", logArgs(`{app="tenant-error"}`, nil), "max entries limit"},
		"giá trị label": {"list_label_values", map[string]any{"label": "__tenant_id__"}, ""},
	} {
		_, out := call(t, sess, tc.tool, tc.args)
		if !strings.Contains(out, tc.want) || strings.Contains(out, "team-a") || strings.Contains(out, "__tenant_id__") {
			t.Errorf("%s: tenant id của Loki lọt vào kết quả (hoặc thiếu %q): %.300s", name, tc.want, out)
		}
	}
	if _, out := call(t, sess, "list_labels", map[string]any{}); decode[listOut](t, out).Total != 300 {
		t.Errorf("list_labels phải bỏ __tenant_id__ khỏi tổng: %.200s", out)
	}

	// Không cấu hình tenant thì không gửi header (Loki chạy auth_enabled: false).
	client, rec = newFake(t, loki.Options{})
	sess = connect(t, client)
	call(t, sess, "list_labels", map[string]any{})
	if got := rec.tenants(); len(got) != 1 || len(got[0]) != 0 {
		t.Errorf("không cấu hình tenant mà vẫn gửi X-Scope-OrgID: %q", got)
	}

	// Nhiều tenant ngăn bằng '|' là lựa chọn của người vận hành.
	// Nhiều tenant ngăn bằng '|' chỉ được nhận khi người vận hành bật rõ ràng.
	if _, err := loki.NewClient("http://loki:3100", loki.Options{TenantID: "team-a|team-b"}); err == nil {
		t.Error("nhiều tenant phải bị từ chối khi không bật AllowMultiTenant")
	}
	if c, err := loki.NewClient("http://loki:3100", loki.Options{TenantID: "team-a|team-b", AllowMultiTenant: true}); err != nil || c.Tenants() != 2 {
		t.Errorf("nhiều tenant khi bật AllowMultiTenant: %v", err)
	}
	for name, tenant := range map[string]string{
		"xuống dòng":     "team-a\r\nX-Evil: 1",
		"khoảng trắng":   "team a",
		"dấu gạch chéo":  "team/a",
		"chấm chấm":      "..",
		"chấm trong '|'": "team-a|.",
		"'|' thừa":       "team-a|",
		"quá dài":        strings.Repeat("a", 151),
	} {
		if _, err := loki.NewClient("http://loki:3100", loki.Options{TenantID: tenant}); err == nil {
			t.Errorf("%s: NewClient phải từ chối tenant %q", name, tenant)
		}
	}
}

// Bearer token là cấu hình phía server: được gửi cho Loki, không theo redirect sang nơi
// khác, không xuất hiện trong kết quả hay lỗi của tool.
func TestCredentialStaysServerSide(t *testing.T) {
	const token = "loki-bearer-secret"
	var leaked sync.Map
	elsewhere := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		leaked.Store("hit", r.Header.Get("Authorization")+" "+r.Header.Get("X-Scope-OrgID"))
	}))
	t.Cleanup(elsewhere.Close)

	rec := newRecorder()
	fake := fakeLoki(rec)
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/loki/api/v1/labels":
			rec.add(r)
			http.Redirect(w, r, elsewhere.URL+"/loki/api/v1/labels", http.StatusFound)
		case "/loki/api/v1/label/app/values":
			rec.add(r)
			http.Error(w, "token "+token+" is not allowed for tenant team-a", http.StatusUnauthorized)
		default:
			fake.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(api.Close)

	client, err := loki.NewClient(api.URL, loki.Options{BearerToken: token + "\n", TenantID: "team-a", RequireHTTPS: true, HTTPClient: api.Client()})
	if err != nil {
		t.Fatal(err)
	}
	sess := connect(t, client)

	res, out := call(t, sess, "query_logs", logArgs(`{app="api"}`, nil))
	if res.IsError {
		t.Fatalf("query qua https kèm token: %s", out)
	}
	rec.mu.Lock()
	sent := rec.auth[0]
	rec.mu.Unlock()
	if sent != "Bearer "+token {
		t.Errorf("header Authorization gửi cho Loki: %q", sent)
	}

	res, redirected := call(t, sess, "list_labels", map[string]any{})
	if !res.IsError {
		t.Errorf("redirect phải là lỗi tool: %s", redirected)
	}
	if v, ok := leaked.Load("hit"); ok {
		t.Errorf("server đã theo redirect sang địa chỉ khác (header: %q)", v)
	}
	res, denied := call(t, sess, "list_label_values", map[string]any{"label": "app"})
	if !res.IsError {
		t.Errorf("401 của Loki phải là lỗi tool: %s", denied)
	}
	for _, text := range []string{out, redirected, denied} {
		if strings.Contains(text, token) || strings.Contains(text, elsewhere.URL) || strings.Contains(text, api.URL) || strings.Contains(text, "team-a") {
			t.Errorf("kết quả tool lộ token, tenant hoặc địa chỉ nội bộ: %s", text)
		}
	}

	// Cấu hình không an toàn bị từ chối lúc khởi động.
	for name, tc := range map[string]struct {
		url  string
		opts loki.Options
	}{
		"token qua http":       {"http://loki:3100", loki.Options{BearerToken: token}},
		"http ngoài local":     {"http://loki:3100", loki.Options{RequireHTTPS: true}},
		"credential trong URL": {"https://user:pw@loki:3100", loki.Options{}},
		"URL có query":         {"https://loki:3100/?token=x", loki.Options{}},
		"không phải http(s)":   {"file:///etc/passwd", loki.Options{}},
		"rỗng":                 {"", loki.Options{}},
	} {
		if _, err := loki.NewClient(tc.url, tc.opts); err == nil {
			t.Errorf("%s: NewClient phải từ chối", name)
		} else if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "pw") {
			t.Errorf("%s: lỗi cấu hình lộ credential: %v", name, err)
		}
	}
}

// Lỗi của Loki (văn bản thuần hoặc JSON) là lỗi tool; URL nội bộ kèm credential trong
// thông báo được che, và thông báo có trần độ dài.
func TestUpstreamErrorsAreScrubbed(t *testing.T) {
	client, _ := newFake(t, loki.Options{})
	sess := connect(t, client)

	res, out := call(t, sess, "query_logs", logArgs(`{bad`, nil))
	if !res.IsError || !strings.Contains(out, "400") || !strings.Contains(out, "parse error") {
		t.Errorf("lỗi LogQL: %s", out)
	}
	res, out = call(t, sess, "query_logs", logArgs(`{app="slow"}`, nil))
	if !res.IsError || !strings.Contains(out, "504") {
		t.Errorf("timeout của Loki: %s", out)
	}
	// 5xx: chỉ một câu cố định kèm mã trạng thái; nội dung lỗi của Loki không vào kết quả.
	res, out = call(t, sess, "query_logs", logArgs(`{app="leak"}`, nil))
	if !res.IsError || !strings.Contains(out, "500") || strings.Contains(out, "s3") || strings.Contains(out, "storage.internal") || strings.Contains(out, "10.0.0.9") {
		t.Errorf("lỗi 5xx phải là câu cố định: %s", out)
	}
	if strings.Contains(func() string { _, o := call(t, sess, "query_logs", logArgs(`{app="slow"}`, nil)); return o }(), "deadline") {
		t.Error("lỗi 504 chép nội dung của Loki vào kết quả")
	}
	// 422: thông báo được chuyển, đã che.
	res, out = call(t, sess, "query_logs", logArgs(`{app="json-error"}`, nil))
	if !res.IsError || !strings.Contains(out, "failed to get s3 object") {
		t.Errorf("lỗi 422 phải kèm thông báo của Loki: %s", out)
	}
	for _, secret := range []string{"s3cr3t", "minio:", "X-Amz-Signature", "abc123"} {
		if strings.Contains(out, secret) {
			t.Errorf("lỗi của Loki mang %q vào kết quả: %s", secret, out)
		}
	}

	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(strings.Repeat("E", 1<<20)))
	}))
	t.Cleanup(big.Close)
	client, _ = loki.NewClient(big.URL, loki.Options{})
	_, err := client.QueryLogs(context.Background(), `{app="api"}`, time.Unix(0, base), time.Unix(0, base).Add(time.Hour), 10, "")
	if err == nil || len(err.Error()) > 700 {
		t.Errorf("thông báo lỗi dài phải bị cắt: %d byte", len(fmt.Sprint(err)))
	}
}

// Loki chết hoặc trả rác: lỗi tool chung chung, không lộ địa chỉ nội bộ.
func TestUnavailable(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	client, err := loki.NewClient(dead.URL, loki.Options{})
	if err != nil {
		t.Fatal(err)
	}
	sess := connect(t, client)
	res, out := call(t, sess, "query_logs", logArgs(`{app="api"}`, nil))
	if !res.IsError || strings.Contains(out, "127.0.0.1") || strings.Contains(out, "app=") {
		t.Errorf("Loki không tới được: %s", out)
	}

	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/labels"):
			_, _ = w.Write([]byte(strings.Repeat("x", 9<<20)))
		case strings.HasSuffix(r.URL.Path, "/values"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"not":"a list"}}`))
		case r.URL.Query().Get("query") == "vector":
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[{"stream":{},"values":[["not-a-number","x"]]}]}}`))
		default:
			_, _ = w.Write([]byte("<html>login</html>"))
		}
	}))
	t.Cleanup(garbage.Close)
	client, _ = loki.NewClient(garbage.URL+"/prefix/", loki.Options{})
	sess = connect(t, client)
	if res, out := call(t, sess, "query_logs", logArgs(`{app="api"}`, nil)); !res.IsError {
		t.Errorf("phản hồi không phải JSON phải là lỗi tool: %s", out)
	}
	if res, out := call(t, sess, "query_logs", logArgs("vector", nil)); !res.IsError {
		t.Errorf("timestamp không đọc được phải là lỗi tool: %s", out)
	}
	if res, out := call(t, sess, "list_labels", map[string]any{}); !res.IsError || len(out) > 1024 {
		t.Errorf("phản hồi quá lớn phải là lỗi tool ngắn: %d byte", len(out))
	}
	if res, out := call(t, sess, "list_label_values", map[string]any{"label": "app"}); !res.IsError {
		t.Errorf("data sai kiểu phải là lỗi tool: %s", out)
	}
}

// Trần 24 giờ không vòng qua được bằng LogQL: cửa sổ range vector lớn, offset, @, subquery
// và biến bị từ chối trước khi gửi (bộ kiểm từ vựng, fail-closed), không request nào tới
// Loki. Nội dung chuỗi và chú thích không bị tính.
func TestLogQLGuard(t *testing.T) {
	client, rec := newFake(t, loki.Options{})
	sess := connect(t, client)
	metric := func(query, start, end string) map[string]any {
		return map[string]any{"query": query, "start": start, "end": end, "step_seconds": 60}
	}
	const tenMinEnd = "2026-11-18T00:10:00Z"

	rejected := map[string]struct {
		tool string
		args map[string]any
	}{
		"cửa sổ 3h trên khoảng 23h":      {"query_metric", metric("sum by (l) (count_over_time({app=\"probe\"} | regexp \"(?P<l>.*)\" [3h]))", "2026-11-17T01:00:00Z", "2026-11-18T00:00:00Z")},
		"cửa sổ 30d":                     {"query_metric", metric(`count_over_time({app="probe"}[30d])`, rangeStart, tenMinEnd)},
		"cửa sổ 25h":                     {"query_metric", metric(`count_over_time({app="probe"}[25h])`, rangeStart, tenMinEnd)},
		"cửa sổ 1d1s":                    {"query_metric", metric(`count_over_time({app="probe"}[1d1s])`, rangeStart, tenMinEnd)},
		"cửa sổ tràn số":                 {"query_metric", metric(`count_over_time({app="probe"}[999999999w999999999w])`, rangeStart, tenMinEnd)},
		"cửa sổ 24h cộng khoảng 10 phút": {"query_metric", metric(`count_over_time({app="probe"}[24h])`, rangeStart, tenMinEnd)},
		"offset":                       {"query_metric", metric(`count_over_time({app="probe"}[30m] offset 2h15m)`, rangeStart, tenMinEnd)},
		"OFFSET hoa":                   {"query_metric", metric(`count_over_time({app="probe"}[30m] OFFSET 2h15m)`, rangeStart, tenMinEnd)},
		"OffSet dính ngoặc":            {"query_metric", metric(`count_over_time({app="probe"}[30m]OffSet 2h)`, rangeStart, tenMinEnd)},
		"offset sau chú thích":         {"query_metric", metric("count_over_time({app=\"probe\"}[30m] # it's \"fine\n offset 2h)", rangeStart, tenMinEnd)},
		"offset sau chuỗi có escape":   {"query_metric", metric(`count_over_time({app="a\"b"}[30m] offset 2h)`, rangeStart, tenMinEnd)},
		"offset sau chuỗi backtick":    {"query_metric", metric("count_over_time({app=`a\\`}[30m] offset 2h)", rangeStart, tenMinEnd)},
		"modifier @":                   {"query_metric", metric(`count_over_time({app="probe"}[5m] @ 1700000000)`, rangeStart, tenMinEnd)},
		"subquery":                     {"query_metric", metric(`max_over_time(rate({app="probe"}[1m])[3h:1m])`, rangeStart, tenMinEnd)},
		"biến $__interval":             {"query_metric", metric(`rate({app="probe"}[$__interval])`, rangeStart, tenMinEnd)},
		"biến $__range ngoài ngoặc":    {"query_metric", metric(`rate({app="probe"}[5m]) * $__range_s`, rangeStart, tenMinEnd)},
		"duration thập phân":           {"query_metric", metric(`rate({app="probe"}[1.5h])`, rangeStart, tenMinEnd)},
		"duration đơn vị y":            {"query_metric", metric(`rate({app="probe"}[1y])`, rangeStart, tenMinEnd)},
		"duration không đơn vị":        {"query_metric", metric(`rate({app="probe"}[300])`, rangeStart, tenMinEnd)},
		"duration 0":                   {"query_metric", metric(`rate({app="probe"}[0s])`, rangeStart, tenMinEnd)},
		"duration âm":                  {"query_metric", metric(`rate({app="probe"}[-5m])`, rangeStart, tenMinEnd)},
		"ngoặc vuông rỗng":             {"query_metric", metric(`rate({app="probe"}[])`, rangeStart, tenMinEnd)},
		"hai duration trong một ngoặc": {"query_metric", metric(`rate({app="probe"}[5m 30d])`, rangeStart, tenMinEnd)},
		"ngoặc vuông không đóng":       {"query_metric", metric(`rate({app="probe"}[5m)`, rangeStart, tenMinEnd)},
		"ngoặc đóng thừa":              {"query_metric", metric(`rate({app="probe"}[5m]])`, rangeStart, tenMinEnd)},
		"chuỗi không đóng":             {"query_metric", metric(`rate({app="probe}[5m])`, rangeStart, tenMinEnd)},
		"backtick không đóng":          {"query_metric", metric("rate({app=`probe}[5m])", rangeStart, tenMinEnd)},
		"xuống dòng trong chuỗi":       {"query_metric", metric("rate({app=\"pro\nbe\"}[5m])", rangeStart, tenMinEnd)},
		"logs: range vector":           {"query_logs", logArgs(`count_over_time({app="probe"}[5m])`, nil)},
		"logs: offset":                 {"query_logs", logArgs(`{app="probe"} offset 2h`, nil)},
		"logs: modifier @":             {"query_logs", logArgs(`{app="probe"} @ 1700000000`, nil)},
		"logs: chuỗi không đóng":       {"query_logs", logArgs(`{app="probe"} |= "abc`, nil)},
		"logs: label tên offset":       {"query_logs", logArgs(`{offset="1"}`, nil)}, // dương tính giả đã biết
	}
	for name, tc := range rejected {
		if res, out := call(t, sess, tc.tool, tc.args); !res.IsError {
			t.Errorf("%s: phải bị từ chối, nhận %.200s", name, out)
		}
	}
	if n := rec.count(); n != 0 {
		t.Errorf("biểu thức bị từ chối đã tới Loki: %d request (%v)", n, rec.requests())
	}
	// Thông báo lỗi nói rõ dương tính giả của offset.
	if _, out := call(t, sess, "query_logs", logArgs(`{offset="1"}`, nil)); !strings.Contains(out, "offset") || !strings.Contains(out, "tên label") {
		t.Errorf("lỗi offset phải nói rõ cả trường hợp tên label: %s", out)
	}

	accepted := map[string]struct {
		tool string
		args map[string]any
	}{
		"offset trong chuỗi":           {"query_logs", logArgs(`{app="api"} |= "offset 2h" != "@ 5"`, nil)},
		"[ trong chuỗi và regex":       {"query_logs", logArgs(`{app="api"} |~ "\\[error\\] [30d]" | pattern "<_> [<ts>] <msg>"`, nil)},
		"chuỗi backtick":               {"query_logs", logArgs("{app=\"api\"} |~ `\\[30d\\] offset \"@ $x` | json", nil)},
		"chú thích chứa dấu nháy":      {"query_logs", logArgs("{app=\"api\"} # it's a \"comment` with [30d] offset @\n |= \"x\"", nil)},
		"chú thích ở cuối":             {"query_logs", logArgs(`{app="api"} # offset [30d]`, nil)},
		"tên chứa offset":              {"query_logs", logArgs(`{app="api", my_offset="1", offset_ms="2", offsets="3"}`, nil)},
		"metric 5m":                    {"query_metric", metric(`sum by (level) (rate({app="api"} |= "[x]" [5m]))`, rangeStart, tenMinEnd)},
		"metric 1h30m có khoảng trắng": {"query_metric", metric(`count_over_time({app="api"}[ 1h30m ])`, rangeStart, tenMinEnd)},
		"metric 500ms, 1d, 1w chặn":    {"query_metric", metric(`rate({app="api"}[90000ms]) + rate({app="api"}[2m])`, rangeStart, tenMinEnd)},
		"khoảng 23h cộng cửa sổ 1h":    {"query_metric", map[string]any{"query": `count_over_time({app="api"}[1h])`, "start": "2026-11-17T01:00:00Z", "end": "2026-11-18T00:00:00Z", "step_seconds": 600}},
		"không có range vector":        {"query_metric", metric(`vector(1)`, rangeStart, tenMinEnd)},
	}
	for name, tc := range accepted {
		before := rec.count()
		if res, out := call(t, sess, tc.tool, tc.args); res.IsError {
			t.Errorf("%s: phải được chấp nhận: %s", name, out)
		}
		if n := rec.count() - before; n != 1 {
			t.Errorf("%s: muốn đúng 1 request tới Loki, có %d", name, n)
		}
	}
}

// Trần truy vấn đồng thời: khi đã có 4 truy vấn đang treo ở Loki, lời gọi thứ 5 nhận lỗi
// "đang bận" ngay và không tới Loki; thả ra thì gọi lại được.
func TestConcurrencyLimit(t *testing.T) {
	client, rec := newFake(t, loki.Options{})
	start := time.Unix(0, base)
	end := start.Add(time.Hour)
	const limit = 4
	var wg sync.WaitGroup
	errs := make(chan error, limit)
	for range limit {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := client.QueryLogs(context.Background(), `{app="hang"}`, start, end, 10, "")
			errs <- err
		}()
	}
	deadline := time.Now().Add(10 * time.Second)
	for rec.count() < limit {
		if time.Now().After(deadline) {
			t.Fatalf("chỉ %d / %d truy vấn tới được Loki", rec.count(), limit)
		}
		time.Sleep(5 * time.Millisecond)
	}
	began := time.Now()
	for _, call := range []func() error{
		func() error {
			_, err := client.QueryLogs(context.Background(), `{app="api"}`, start, end, 10, "")
			return err
		},
		func() error {
			_, err := client.QueryMetric(context.Background(), `rate({app="api"}[1m])`, start, end, time.Minute, 0)
			return err
		},
		func() error { _, _, _, _, err := client.Labels(context.Background(), 0, 0, 0); return err },
		func() error { _, _, _, _, err := client.LabelValues(context.Background(), "app", 0, 0, 0); return err },
	} {
		if err := call(); err == nil || !strings.Contains(err.Error(), "đang bận") {
			t.Errorf("lời gọi vượt trần đồng thời phải nhận lỗi đang bận, nhận %v", err)
		}
	}
	if waited := time.Since(began); waited > 2*time.Second {
		t.Errorf("lời gọi vượt trần phải bị từ chối ngay, đã chờ %s", waited)
	}
	if n := rec.count(); n != limit {
		t.Errorf("lời gọi vượt trần đã tới Loki: %d request", n)
	}
	close(rec.release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("truy vấn đang treo phải xong sau khi thả: %v", err)
		}
	}
	if _, err := client.QueryLogs(context.Background(), `{app="api"}`, start, end, 10, ""); err != nil {
		t.Errorf("sau khi thả phải gọi lại được: %v", err)
	}
}
