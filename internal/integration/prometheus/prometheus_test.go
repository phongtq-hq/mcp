package prometheus_test

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
	"github.com/hqgroups/mcp/internal/integration/prometheus"
	"github.com/hqgroups/mcp/internal/shared/signedctx"
)

// recorder ghi lại mọi request mà Prometheus giả lập nhận được.
type recorder struct {
	mu   sync.Mutex
	seen []string
	auth []string
	last map[string]string // path → query string của request gần nhất
}

func (r *recorder) add(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, req.Method+" "+req.URL.Path)
	r.auth = append(r.auth, req.Header.Get("Authorization"))
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

func (r *recorder) lastQuery(path string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last[path]
}

func success(w http.ResponseWriter, data string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"status":"success","data":%s}`, data)
}

func failure(w http.ResponseWriter, status int, errorType, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "error", "errorType": errorType, "error": msg})
}

// vector tạo n series của một instant vector.
func vector(n int) string {
	items := make([]string, n)
	for i := range items {
		items[i] = fmt.Sprintf(`{"metric":{"__name__":"up","job":"node","instance":"host-%03d:9100"},"value":[1700000000.123,"1"]}`, i)
	}
	return `{"resultType":"vector","result":[` + strings.Join(items, ",") + `]}`
}

// matrix tạo n series, mỗi series có points điểm.
func matrix(n, points int) string {
	items := make([]string, n)
	for i := range items {
		values := make([]string, points)
		for j := range values {
			values[j] = fmt.Sprintf(`[%d,"%d.5"]`, 1700000000+j*15, j)
		}
		items[i] = fmt.Sprintf(`{"metric":{"__name__":"http_requests_total","instance":"host-%03d:9100"},"values":[%s]}`, i, strings.Join(values, ","))
	}
	return `{"resultType":"matrix","result":[` + strings.Join(items, ",") + `]}`
}

const targetsBody = `{"activeTargets":[
 {"discoveredLabels":{"__address__":"10.0.0.7:9100","__meta_kubernetes_pod_annotation_vault":"sd-secret","__param_token":"param-secret"},
  "labels":{"instance":"10.0.0.7:9100","job":"node","team_private":"label-secret"},
  "scrapePool":"node","scrapeUrl":"http://10.0.0.7:9100/metrics?token=url-secret","globalUrl":"http://example.internal:9100/metrics?token=url-secret",
  "lastError":"","lastScrape":"2026-11-18T08:00:00.123Z","lastScrapeDuration":0.012,"health":"up","scrapeInterval":"15s","scrapeTimeout":"10s"},
 {"discoveredLabels":{"__address__":"10.0.0.8:8080"},
  "labels":{"instance":"10.0.0.8:8080","job":"api"},
  "scrapePool":"api","scrapeUrl":"http://10.0.0.8:8080/metrics",
  "lastError":"Get \"http://scraper:pw-secret@10.0.0.8:8080/metrics?token=err-secret&x=1\": context deadline exceeded","lastScrape":"2026-11-18T08:00:01Z","lastScrapeDuration":10,"health":"down","scrapeInterval":"15s","scrapeTimeout":"10s"}],
 "droppedTargets":[{"discoveredLabels":{"__address__":"10.0.0.9:1","__param_token":"dropped-secret"},"scrapePool":"node"}]}`

func alertsBody() string {
	return `{"alerts":[
 {"labels":{"alertname":"DiskAlmostFull","severity":"warning","instance":"host-001:9100"},"annotations":{"summary":"disk 91% full"},"state":"pending","activeAt":"2026-11-18T07:59:00Z","value":"9.1e+01"},
 {"labels":{"alertname":"TargetDown","severity":"critical","job":"api"},"annotations":{"description":"` + strings.Repeat("d", 4000) + `"},"state":"firing","activeAt":"2026-11-18T07:00:00Z","value":"1e+00"},
 {"labels":{"alertname":"HighErrorRate","severity":"critical"},"annotations":{},"state":"firing","activeAt":"2026-11-18T07:30:00Z","value":"2.5e-01"}]}`
}

func metricNames() string {
	names := make([]string, 0, 1300)
	for i := 0; i < 1200; i++ {
		names = append(names, fmt.Sprintf(`"node_metric_%04d"`, i))
	}
	for i := 0; i < 100; i++ {
		names = append(names, fmt.Sprintf(`"http_metric_%04d"`, i))
	}
	return "[" + strings.Join(names, ",") + "]"
}

// fakePrometheus là HTTP API tối thiểu của Prometheus, theo định dạng ở
// https://prometheus.io/docs/prometheus/latest/querying/api/. Nó cố ý bỏ qua tham số
// limit (như Prometheus cũ) để trần phía server được kiểm.
func fakePrometheus(rec *recorder) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/query", func(w http.ResponseWriter, r *http.Request) {
		switch q := r.URL.Query().Get("query"); q {
		case "up":
			success(w, vector(2))
		case "many":
			success(w, vector(500))
		case "wide[1h]":
			success(w, matrix(100, 600))
		case "42":
			success(w, `{"resultType":"scalar","result":[1700000000,"42"]}`)
		case "warned":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"success","data":` + vector(1) + `,"warnings":["PromQL warning: something"]}`))
		case "sum(":
			failure(w, http.StatusBadRequest, "bad_data", `invalid parameter "query": 1:5: parse error: unclosed left parenthesis`)
		case "slow":
			failure(w, http.StatusServiceUnavailable, "timeout", "query timed out in expression evaluation")
		default:
			success(w, `{"resultType":"vector","result":[]}`)
		}
	})
	mux.HandleFunc("GET /api/v1/query_range", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		start, _ := strconv.ParseFloat(q.Get("start"), 64)
		end, _ := strconv.ParseFloat(q.Get("end"), 64)
		step, _ := strconv.ParseFloat(q.Get("step"), 64)
		if step <= 0 {
			failure(w, http.StatusBadRequest, "bad_data", "zero or negative query resolution step widths are not accepted")
			return
		}
		success(w, matrix(60, int((end-start)/step)+1))
	})
	mux.HandleFunc("GET /api/v1/alerts", func(w http.ResponseWriter, _ *http.Request) { success(w, alertsBody()) })
	mux.HandleFunc("GET /api/v1/targets", func(w http.ResponseWriter, _ *http.Request) { success(w, targetsBody) })
	mux.HandleFunc("GET /api/v1/label/__name__/values", func(w http.ResponseWriter, _ *http.Request) { success(w, metricNames()) })
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
	r.Header.Set(signedctx.Header, h.signer.Sign(signedctx.Context{Kind: signedctx.KindCall, Audience: prometheus.Slug,
		OrgID: testOrg, ProjectID: uuid.New(), Initiator: "user:" + uuid.NewString()}, time.Now()))
	return http.DefaultTransport.RoundTrip(r)
}

// connect dựng server MCP trên client đã cho và nối một MCP client thật vào, với ngữ
// cảnh ký của org sở hữu.
func connect(t *testing.T, client *prometheus.Client) *mcp.ClientSession {
	t.Helper()
	signer, _ := signedctx.NewSigner(testKey)
	srv := httptest.NewServer(prometheus.Handler(client, signer, testOrg))
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

func newFake(t *testing.T) (*prometheus.Client, *recorder) {
	t.Helper()
	rec := &recorder{last: map[string]string{}}
	api := httptest.NewServer(fakePrometheus(rec))
	t.Cleanup(api.Close)
	client, err := prometheus.NewClient(api.URL, prometheus.Options{})
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

type queryOut struct {
	ResultType string `json:"result_type"`
	Series     []struct {
		Metric  map[string]string `json:"metric"`
		Samples []struct {
			T float64 `json:"t"`
			V string  `json:"v"`
		} `json:"samples"`
	} `json:"series"`
	Truncated bool     `json:"truncated"`
	Warnings  []string `json:"warnings"`
}

type listOut struct {
	Alerts     []map[string]any `json:"alerts"`
	Targets    []map[string]any `json:"targets"`
	Names      []string         `json:"names"`
	Total      int              `json:"total"`
	NextOffset int              `json:"next_offset"`
}

const (
	rangeStart = "2026-11-18T00:00:00Z"
	rangeEnd   = "2026-11-18T01:00:00Z"
)

// Bộ test hợp đồng dùng chung của managed MCP server (mcp-platform §7).
func TestContract(t *testing.T) {
	client, rec := newFake(t)
	signer, _ := signedctx.NewSigner(testKey)
	contract.Run(t, contract.Server{
		Slug:           prometheus.Slug,
		Handler:        prometheus.Handler(client, signer, testOrg),
		SigningKey:     testKey,
		OrgID:          testOrg,
		TargetRequests: rec.count,
		Tools: []contract.Tool{
			{Name: "query_instant", MaxResultBytes: prometheus.MaxResultBytes, Samples: []map[string]any{
				{"query": "up"}, {"query": "42", "time": "2026-11-18T08:00:00Z"},
				{"query": "many", "limit": 200}, {"query": "wide[1h]", "limit": 200}}},
			{Name: "query_range", MaxResultBytes: prometheus.MaxResultBytes, Samples: []map[string]any{
				{"query": "rate(http_requests_total[5m])", "start": rangeStart, "end": rangeEnd, "step_seconds": 60},
				{"query": "rate(http_requests_total[5m])", "start": rangeStart, "end": rangeEnd, "step_seconds": 10, "limit": 50}}},
			{Name: "list_alerts", MaxResultBytes: prometheus.MaxResultBytes, Samples: []map[string]any{
				{}, {"state": "firing", "limit": 200}}},
			{Name: "list_targets", MaxResultBytes: prometheus.MaxResultBytes, Samples: []map[string]any{
				{}, {"health": "down", "scrape_pool": "api"}}},
			{Name: "list_metric_names", MaxResultBytes: prometheus.MaxResultBytes, Samples: []map[string]any{
				{}, {"prefix": "node_", "limit": 500}, {"limit": 500, "offset": 1000}}},
		},
	})
}

func TestQueries(t *testing.T) {
	client, rec := newFake(t)
	sess := connect(t, client)

	res, out := call(t, sess, "query_instant", map[string]any{"query": "up"})
	q := decode[queryOut](t, out)
	if res.IsError || q.ResultType != "vector" || len(q.Series) != 2 || q.Truncated ||
		q.Series[1].Metric["instance"] != "host-001:9100" || q.Series[0].Samples[0].V != "1" || q.Series[0].Samples[0].T != 1700000000.123 {
		t.Errorf("query_instant up: %s", out)
	}
	// Thời điểm được chuyển thành giây Unix; Prometheus tự huỷ query theo timeout được gửi.
	call(t, sess, "query_instant", map[string]any{"query": "up", "time": "2026-11-18T08:00:00+07:00"})
	if got := rec.lastQuery("/api/v1/query"); !strings.Contains(got, "time=1794963600.000") || !strings.Contains(got, "timeout=20s") {
		t.Errorf("tham số gửi cho Prometheus: %s", got)
	}

	_, out = call(t, sess, "query_instant", map[string]any{"query": "42"})
	if q := decode[queryOut](t, out); q.ResultType != "scalar" || len(q.Series) != 1 || q.Series[0].Samples[0].V != "42" {
		t.Errorf("scalar: %s", out)
	}
	_, out = call(t, sess, "query_instant", map[string]any{"query": "warned"})
	if q := decode[queryOut](t, out); len(q.Warnings) != 1 {
		t.Errorf("warnings: %s", out)
	}

	// Trần số series: mặc định 50, tối đa 200, kể cả khi Prometheus bỏ qua tham số limit.
	_, out = call(t, sess, "query_instant", map[string]any{"query": "many"})
	if q := decode[queryOut](t, out); len(q.Series) != 50 || !q.Truncated {
		t.Errorf("mặc định phải cắt ở 50 series: %d truncated=%v", len(q.Series), q.Truncated)
	}
	_, out = call(t, sess, "query_instant", map[string]any{"query": "many", "limit": 200})
	if q := decode[queryOut](t, out); len(q.Series) != 200 || !q.Truncated {
		t.Errorf("limit 200: %d truncated=%v", len(q.Series), q.Truncated)
	}
	// Trần kích thước: 100 series × 600 điểm (~1 MB) bị cắt xuống dưới 64 KiB, mỗi
	// series giữ 500 điểm mới nhất.
	_, out = call(t, sess, "query_instant", map[string]any{"query": "wide[1h]", "limit": 200})
	q = decode[queryOut](t, out)
	if len(out) > prometheus.MaxResultBytes || !q.Truncated || len(q.Series) == 0 || len(q.Series[0].Samples) != 500 ||
		q.Series[0].Samples[499].V != "599.5" {
		t.Errorf("kết quả lớn phải bị cắt: %d byte, %d series, truncated=%v", len(out), len(q.Series), q.Truncated)
	}

	res, out = call(t, sess, "query_range", map[string]any{"query": "up", "start": rangeStart, "end": rangeEnd, "step_seconds": 60})
	q = decode[queryOut](t, out)
	if res.IsError || q.ResultType != "matrix" || len(q.Series) == 0 || len(q.Series[0].Samples) != 61 || len(out) > prometheus.MaxResultBytes {
		t.Errorf("query_range: %d byte, %d series", len(out), len(q.Series))
	}
	if got := rec.lastQuery("/api/v1/query_range"); !strings.Contains(got, "step=60") || !strings.Contains(got, "start=1794960000.000") {
		t.Errorf("tham số query_range: %s", got)
	}

	// Lỗi của Prometheus là lỗi tool, kèm loại lỗi.
	res, out = call(t, sess, "query_instant", map[string]any{"query": "sum("})
	if !res.IsError || !strings.Contains(out, "bad_data") || !strings.Contains(out, "parse error") {
		t.Errorf("lỗi PromQL: %s", out)
	}
	res, out = call(t, sess, "query_instant", map[string]any{"query": "slow"})
	if !res.IsError || !strings.Contains(out, "timeout") {
		t.Errorf("timeout: %s", out)
	}

	// Đầu vào ngoài giới hạn là lỗi tool và không tới Prometheus.
	before := rec.count()
	for name, tc := range map[string]struct {
		tool string
		args map[string]any
	}{
		"query rỗng":            {"query_instant", map[string]any{"query": "  "}},
		"query quá dài":         {"query_instant", map[string]any{"query": strings.Repeat("a", 2001)}},
		"thiếu query":           {"query_instant", map[string]any{}},
		"time không phải RFC":   {"query_instant", map[string]any{"query": "up", "time": "yesterday"}},
		"time không tồn tại":    {"query_instant", map[string]any{"query": "up", "time": "2026-13-45T00:00:00Z"}},
		"limit vượt trần":       {"query_instant", map[string]any{"query": "up", "limit": 201}},
		"limit âm":              {"query_instant", map[string]any{"query": "up", "limit": -1}},
		"trường lạ":             {"query_instant", map[string]any{"query": "up", "timeout": "10m"}},
		"khoảng quá 7 ngày":     {"query_range", map[string]any{"query": "up", "start": "2026-11-01T00:00:00Z", "end": "2026-11-09T00:00:00Z", "step_seconds": 3600}},
		"end trước start":       {"query_range", map[string]any{"query": "up", "start": rangeEnd, "end": rangeStart, "step_seconds": 60}},
		"step dưới 10 giây":     {"query_range", map[string]any{"query": "up", "start": rangeStart, "end": rangeEnd, "step_seconds": 1}},
		"quá 500 điểm":          {"query_range", map[string]any{"query": "up", "start": "2026-11-17T00:00:00Z", "end": "2026-11-18T00:00:00Z", "step_seconds": 60}},
		"thiếu step":            {"query_range", map[string]any{"query": "up", "start": rangeStart, "end": rangeEnd}},
		"range limit vượt trần": {"query_range", map[string]any{"query": "up", "start": rangeStart, "end": rangeEnd, "step_seconds": 60, "limit": 51}},
	} {
		if res, out := call(t, sess, tc.tool, tc.args); !res.IsError {
			t.Errorf("%s: phải là lỗi tool, nhận %.200s", name, out)
		}
	}
	if n := rec.count() - before; n != 0 {
		t.Errorf("đầu vào xấu đã tới Prometheus: %d request", n)
	}
	// Thông báo lỗi chỉ ra step nhỏ nhất dùng được.
	_, out = call(t, sess, "query_range", map[string]any{"query": "up", "start": "2026-11-17T00:00:00Z", "end": "2026-11-18T00:00:00Z", "step_seconds": 60})
	if !strings.Contains(out, "174") {
		t.Errorf("lỗi quá số điểm phải nêu step tối thiểu: %s", out)
	}
}

func TestLists(t *testing.T) {
	client, rec := newFake(t)
	sess := connect(t, client)

	res, out := call(t, sess, "list_alerts", map[string]any{})
	l := decode[listOut](t, out)
	if res.IsError || l.Total != 3 || len(l.Alerts) != 3 || l.NextOffset != 0 ||
		l.Alerts[0]["name"] != "HighErrorRate" || l.Alerts[1]["name"] != "TargetDown" || l.Alerts[2]["state"] != "pending" {
		t.Errorf("list_alerts (firing trước, rồi theo tên): %.400s", out)
	}
	if strings.Contains(out, strings.Repeat("d", 600)) {
		t.Error("annotation dài phải bị cắt")
	}
	_, out = call(t, sess, "list_alerts", map[string]any{"state": "pending"})
	if l := decode[listOut](t, out); l.Total != 1 || l.Alerts[0]["name"] != "DiskAlmostFull" {
		t.Errorf("lọc pending: %s", out)
	}
	_, out = call(t, sess, "list_alerts", map[string]any{"limit": 2})
	if l := decode[listOut](t, out); len(l.Alerts) != 2 || l.NextOffset != 2 || l.Total != 3 {
		t.Errorf("phân trang alert: %.300s", out)
	}

	res, out = call(t, sess, "list_targets", map[string]any{})
	l = decode[listOut](t, out)
	if res.IsError || l.Total != 2 || l.Targets[0]["job"] != "api" || l.Targets[0]["health"] != "down" ||
		l.Targets[1]["instance"] != "10.0.0.7:9100" || !strings.Contains(out, "context deadline exceeded") {
		t.Errorf("list_targets: %s", out)
	}
	// discoveredLabels, label ngoài job / instance, URL scrape và credential trong lỗi
	// scrape không bao giờ rời server.
	for _, secret := range []string{"sd-secret", "param-secret", "label-secret", "url-secret", "err-secret", "pw-secret",
		"dropped-secret", "__meta", "__param", "team_private", "example.internal"} {
		if strings.Contains(out, secret) {
			t.Errorf("list_targets lộ %q: %s", secret, out)
		}
	}
	_, out = call(t, sess, "list_targets", map[string]any{"health": "up", "scrape_pool": "node"})
	if l := decode[listOut](t, out); l.Total != 1 || l.Targets[0]["job"] != "node" {
		t.Errorf("lọc target: %s", out)
	}
	if got := rec.lastQuery("/api/v1/targets"); got != "scrapePool=node&state=active" {
		t.Errorf("tham số targets: %s", got)
	}

	// Tên metric: sắp xếp, phân trang đi hết được, lọc theo tiền tố.
	_, out = call(t, sess, "list_metric_names", map[string]any{})
	l = decode[listOut](t, out)
	if l.Total != 1300 || len(l.Names) != 100 || l.Names[0] != "http_metric_0000" || l.NextOffset != 100 {
		t.Errorf("list_metric_names: total=%d n=%d next=%d", l.Total, len(l.Names), l.NextOffset)
	}
	if got := rec.lastQuery("/api/v1/label/__name__/values"); !strings.Contains(got, "start=") || !strings.Contains(got, "end=") || strings.Contains(got, "match") {
		t.Errorf("tham số label values: %s", got)
	}
	seen, offset := 0, 0
	for range 10 {
		_, out = call(t, sess, "list_metric_names", map[string]any{"limit": 500, "offset": offset})
		l = decode[listOut](t, out)
		seen += len(l.Names)
		if offset = l.NextOffset; offset == 0 {
			break
		}
	}
	if seen != 1300 {
		t.Errorf("phân trang phải đi hết 1300 tên, được %d", seen)
	}
	_, out = call(t, sess, "list_metric_names", map[string]any{"prefix": "http_", "limit": 500})
	if l := decode[listOut](t, out); l.Total != 100 || l.NextOffset != 0 {
		t.Errorf("lọc tiền tố: total=%d", l.Total)
	}
	if got := rec.lastQuery("/api/v1/label/__name__/values"); !strings.Contains(got, "match%5B%5D=%7B__name__%3D~%22http_.%2A%22%7D") {
		t.Errorf("matcher gửi cho Prometheus: %s", got)
	}
	_, out = call(t, sess, "list_metric_names", map[string]any{"offset": 5000})
	if l := decode[listOut](t, out); len(l.Names) != 0 || l.NextOffset != 0 {
		t.Errorf("offset quá cuối: %s", out)
	}

	before := rec.count()
	for name, tc := range map[string]struct {
		tool string
		args map[string]any
	}{
		"state ngoài enum":         {"list_alerts", map[string]any{"state": "inactive"}},
		"limit vượt trần":          {"list_alerts", map[string]any{"limit": 201}},
		"offset âm":                {"list_alerts", map[string]any{"offset": -1}},
		"health ngoài enum":        {"list_targets", map[string]any{"health": "any"}},
		"scrape_pool chèn tham số": {"list_targets", map[string]any{"scrape_pool": "node&state=dropped"}},
		"prefix là regex":          {"list_metric_names", map[string]any{"prefix": "up.*|x"}},
		"prefix chèn matcher":      {"list_metric_names", map[string]any{"prefix": `x"}or{a="`}},
		"trường lạ":                {"list_metric_names", map[string]any{"match": `{job="x"}`}},
	} {
		if res, out := call(t, sess, tc.tool, tc.args); !res.IsError {
			t.Errorf("%s: phải là lỗi tool, nhận %.200s", name, out)
		}
	}
	if n := rec.count() - before; n != 0 {
		t.Errorf("đầu vào xấu đã tới Prometheus: %d request", n)
	}
}

// Server chỉ đọc: đúng năm tool, và chỉ GET tới năm endpoint đọc.
func TestReadOnlySurface(t *testing.T) {
	client, rec := newFake(t)
	sess := connect(t, client)
	list, err := sess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range list.Tools {
		names = append(names, tool.Name)
	}
	if got := strings.Join(names, ","); got != "list_alerts,list_metric_names,list_targets,query_instant,query_range" {
		t.Errorf("danh sách tool: %s", got)
	}
	call(t, sess, "query_instant", map[string]any{"query": "up"})
	call(t, sess, "query_range", map[string]any{"query": "up", "start": rangeStart, "end": rangeEnd, "step_seconds": 60})
	call(t, sess, "list_alerts", map[string]any{})
	call(t, sess, "list_targets", map[string]any{})
	call(t, sess, "list_metric_names", map[string]any{})
	allowed := map[string]bool{"GET /api/v1/query": true, "GET /api/v1/query_range": true, "GET /api/v1/alerts": true,
		"GET /api/v1/targets": true, "GET /api/v1/label/__name__/values": true}
	reqs := rec.requests()
	if len(reqs) != 5 {
		t.Errorf("muốn 5 request, có %v", reqs)
	}
	for _, r := range reqs {
		if !allowed[r] {
			t.Errorf("server gọi Prometheus ngoài phạm vi đọc: %s", r)
		}
	}
}

// Bearer token là cấu hình phía server: được gửi cho Prometheus, không theo redirect
// sang nơi khác, không xuất hiện trong kết quả hay lỗi của tool.
func TestCredentialStaysServerSide(t *testing.T) {
	const token = "prom-bearer-secret"
	var leaked sync.Map
	elsewhere := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		leaked.Store("hit", r.Header.Get("Authorization"))
	}))
	t.Cleanup(elsewhere.Close)

	rec := &recorder{last: map[string]string{}}
	fake := fakePrometheus(rec)
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/targets":
			rec.add(r)
			http.Redirect(w, r, elsewhere.URL+"/api/v1/targets", http.StatusFound)
		case "/api/v1/alerts":
			rec.add(r)
			http.Error(w, "token "+token+" is not allowed", http.StatusUnauthorized)
		default:
			fake.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(api.Close)

	client, err := prometheus.NewClient(api.URL, prometheus.Options{BearerToken: token + "\n", HTTPClient: api.Client()})
	if err != nil {
		t.Fatal(err)
	}
	sess := connect(t, client)

	res, out := call(t, sess, "query_instant", map[string]any{"query": "up"})
	if res.IsError {
		t.Fatalf("query qua https kèm token: %s", out)
	}
	rec.mu.Lock()
	sent := rec.auth[0]
	rec.mu.Unlock()
	if sent != "Bearer "+token {
		t.Errorf("header Authorization gửi cho Prometheus: %q", sent)
	}

	res, redirected := call(t, sess, "list_targets", map[string]any{})
	if !res.IsError {
		t.Errorf("redirect phải là lỗi tool: %s", redirected)
	}
	if v, ok := leaked.Load("hit"); ok {
		t.Errorf("server đã theo redirect sang địa chỉ khác (Authorization: %q)", v)
	}
	res, denied := call(t, sess, "list_alerts", map[string]any{})
	if !res.IsError {
		t.Errorf("401 của Prometheus phải là lỗi tool: %s", denied)
	}
	for _, text := range []string{out, redirected, denied} {
		if strings.Contains(text, token) || strings.Contains(text, elsewhere.URL) || strings.Contains(text, api.URL) {
			t.Errorf("kết quả tool lộ token hoặc địa chỉ nội bộ: %s", text)
		}
	}

	// Cấu hình không an toàn bị từ chối lúc khởi động.
	for name, tc := range map[string]struct {
		url  string
		opts prometheus.Options
	}{
		"token qua http":       {"http://prometheus:9090", prometheus.Options{BearerToken: token}},
		"credential trong URL": {"https://user:pw@prometheus:9090", prometheus.Options{}},
		"URL có query":         {"https://prometheus:9090/?token=x", prometheus.Options{}},
		"không phải http(s)":   {"file:///etc/passwd", prometheus.Options{}},
		"rỗng":                 {"", prometheus.Options{}},
	} {
		if _, err := prometheus.NewClient(tc.url, tc.opts); err == nil {
			t.Errorf("%s: NewClient phải từ chối", name)
		} else if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "pw") {
			t.Errorf("%s: lỗi cấu hình lộ credential: %v", name, err)
		}
	}
}

// Prometheus chết hoặc trả rác: lỗi tool chung chung, không lộ địa chỉ nội bộ.
func TestUnavailable(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	client, err := prometheus.NewClient(dead.URL, prometheus.Options{})
	if err != nil {
		t.Fatal(err)
	}
	sess := connect(t, client)
	res, out := call(t, sess, "query_instant", map[string]any{"query": "up"})
	if !res.IsError || strings.Contains(out, "127.0.0.1") {
		t.Errorf("Prometheus không tới được: %s", out)
	}

	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/alerts") {
			_, _ = w.Write([]byte(strings.Repeat("x", 9<<20)))
			return
		}
		_, _ = w.Write([]byte("<html>login</html>"))
	}))
	t.Cleanup(garbage.Close)
	client, _ = prometheus.NewClient(garbage.URL+"/prefix/", prometheus.Options{})
	sess = connect(t, client)
	if res, out := call(t, sess, "query_instant", map[string]any{"query": "up"}); !res.IsError {
		t.Errorf("phản hồi không phải JSON phải là lỗi tool: %s", out)
	}
	if res, out := call(t, sess, "list_alerts", map[string]any{}); !res.IsError || len(out) > 1024 {
		t.Errorf("phản hồi quá lớn phải là lỗi tool ngắn: %d byte", len(out))
	}
}

// Thông báo lỗi và cảnh báo của Prometheus có thể chép URL nội bộ kèm credential (remote
// read, proxy đứng trước): chúng được che trước khi đi vào kết quả tool. Và một phần tử
// phình to (label do bên thứ ba kiểm soát) không che các phần tử đứng sau nó.
func TestUpstreamTextIsScrubbedAndOversizedItemsDoNotHideOthers(t *testing.T) {
	const leak = `remote_read: remote server https://reader:s3cr3t@thanos.internal:10901/api/v1/read?tenant_token=abc returned HTTP status 500`
	huge := strings.Repeat("<", 250) // mỗi byte thành 6 byte trong JSON
	labels := `"alertname":"Huge"`
	for i := range 64 {
		labels += fmt.Sprintf(`,"l%02d":"%s"`, i, huge)
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/alerts":
			_, _ = w.Write([]byte(`{"status":"success","data":{"alerts":[
				{"labels":{` + labels + `},"annotations":{},"state":"firing","activeAt":"2026-11-18T00:00:00Z","value":"1"},
				{"labels":{"alertname":"Real"},"annotations":{},"state":"firing","activeAt":"2026-11-18T00:00:00Z","value":"1"}]}}`))
		case r.URL.Query().Get("query") == "warned":
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]},"warnings":["` + leak + `"]}`))
		default:
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"status":"error","errorType":"execution","error":"` + leak + `"}`))
		}
	}))
	t.Cleanup(api.Close)
	client, err := prometheus.NewClient(api.URL, prometheus.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_, err = client.QueryInstant(ctx, "up", time.Time{}, 10)
	if err == nil || strings.Contains(err.Error(), "s3cr3t") || strings.Contains(err.Error(), "tenant_token=abc") {
		t.Errorf("lỗi của Prometheus mang credential vào kết quả: %v", err)
	}
	res, err := client.QueryInstant(ctx, "warned", time.Time{}, 10)
	if raw, _ := json.Marshal(res); err != nil || strings.Contains(string(raw), "s3cr3t") || strings.Contains(string(raw), "tenant_token=abc") {
		t.Errorf("cảnh báo của Prometheus mang credential vào kết quả: %s, %v", raw, err)
	}
	page, total, _, omitted, err := client.Alerts(ctx, "", 0, 50)
	if err != nil || total != 2 || omitted != 1 || len(page) != 1 || page[0].Labels["alertname"] != "Real" {
		t.Errorf("alert thật bị che bởi alert phình to: %d alert, total %d, omitted %d, %v", len(page), total, omitted, err)
	}
}
