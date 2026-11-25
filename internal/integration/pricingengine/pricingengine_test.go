package pricingengine_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hqgroups/mcp/internal/integration/contract"
	"github.com/hqgroups/mcp/internal/integration/pricingengine"
	"github.com/hqgroups/mcp/internal/shared/signedctx"
)

// testAPIKey là khoá mà PE giả lập đòi; không test nào được thấy nó trong kết quả hay lỗi.
const testAPIKey = "sk_live_test_0123456789abcdef"

// leak là văn bản PE giả lập đặt vào thân của mọi phản hồi lỗi; nó không bao giờ được
// tới kết quả tool.
const leak = "upstream-internal-detail"

const (
	skuID   = "0198a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b"
	brandID = "0198a1b2-c3d4-7e5f-8a9b-000000000001"
)

type request struct {
	method, path, query, auth, body string
}

type recorder struct {
	mu   sync.Mutex
	seen []request
}

func (r *recorder) add(req *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(req.Body, 1<<20))
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, request{req.Method, req.URL.Path, req.URL.RawQuery, req.Header.Get("Authorization"), string(body)})
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.seen)
}

func (r *recorder) all() []request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]request(nil), r.seen...)
}

func (r *recorder) last() request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seen[len(r.seen)-1]
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-RateLimit-Limit", "600")
	w.Header().Set("X-RateLimit-Remaining", "599")
	w.Header().Set("X-RateLimit-Reset", "60")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"message": message, "detail": leak})
}

// offer dựng một offer đúng hình dạng PriceBookOfferView của PE.
func offer(rank int, supplier string, price string, eligible bool, view map[string]any) map[string]any {
	o := map[string]any{
		"supplier":          map[string]any{"code": "SUP_" + supplier, "name": "Supplier " + supplier},
		"supplierProductId": strings.ToLower(supplier) + "-123",
		"price":             price, "currency": "USD", "available": eligible,
		"priceUpdatedAt": "2026-10-23T07:55:00.000Z", "stale": false, "eligible": eligible, "lowConfidence": rank == 2,
	}
	if eligible {
		o["rank"], o["score"] = rank, 92.3
		o["scoreBreakdown"] = map[string]any{"price": 100, "speed": 63.2, "success": 60.2}
		if rank == 2 {
			o["scoreBreakdown"] = map[string]any{"price": 0, "speed": nil, "success": nil}
			o["score"] = 0
		}
	} else {
		o["rank"], o["score"], o["scoreBreakdown"], o["ineligibleReason"] = nil, nil, nil, "unavailable"
	}
	if view["currency"] != nil {
		o["convertedPrice"] = "15.640000"
		if rank == 2 {
			o["convertedPrice"] = nil
		}
	}
	return o
}

// book dựng bảng giá của một SKU, hoặc nil nếu PE giả lập không biết SKU đó.
func book(sku string, view map[string]any) map[string]any {
	var offers []any
	out := map[string]any{"sku": sku, "name": "Free Fire Gift Card $20", "generatedAt": "2026-10-23T08:00:05.000Z",
		"comparisonCurrency": "USD", "scoringProfile": map[string]any{"id": "default", "version": 3}}
	switch {
	case sku == "HQ-FREEFIRE-GC-20" || sku == "HQ_OLD" || regexp.MustCompile(`^HQ-N[0-9]+$`).MatchString(sku):
		offers = []any{offer(1, "A", "17.000000", true, view), offer(2, "B", "18.700000", true, view)}
		if view["includeIneligible"] == true {
			offers = append(offers, offer(0, "C", "16.000000", false, view))
		}
		if sku == "HQ_OLD" {
			out["canonicalSku"] = "HQ-FREEFIRE-GC-20"
		}
	case sku == "HQ-EMPTY":
		offers = []any{}
	case strings.HasPrefix(sku, "HQ-BIG"):
		// Bảng giá lớn hơn trần kết quả: 400 offer với tên nhà cung cấp dài.
		for i := 1; i <= 400; i++ {
			o := offer(i, fmt.Sprintf("S%03d", i), "17.000000", true, view)
			o["supplier"] = map[string]any{"code": fmt.Sprintf("SUP_%03d", i), "name": strings.Repeat("Nhà cung cấp ", 30)}
			offers = append(offers, o)
		}
	default:
		return nil
	}
	if n, ok := view["limit"].(int); ok && n > 0 && n < len(offers) && !strings.HasPrefix(sku, "HQ-BIG") {
		offers = offers[:n]
	}
	if view["currency"] != nil {
		out["currency"] = view["currency"]
	}
	out["offers"] = offers
	return out
}

func cursorOffset(q string) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(q, "c"))
	return n
}

func intParam(r *http.Request, name string, def int) int {
	if n, err := strconv.Atoi(r.URL.Query().Get(name)); err == nil && n > 0 {
		return n
	}
	return def
}

func apiSKU(i int) map[string]any {
	id := fmt.Sprintf("0198a1b2-c3d4-7e5f-8a9b-%012d", i)
	if i == 0 {
		id = skuID
	}
	return map[string]any{
		"id": id, "code": fmt.Sprintf("HQ-SKU-%03d", i), "formerCodes": []string{fmt.Sprintf("HQ_OLD_%03d", i)},
		"name": "SKU " + strconv.Itoa(i), "brandId": brandID, "brandName": "Free Fire", "category": "Gift Card",
		"productType": "giftcard", "region": "GLOBAL", "faceValue": "20.000000", "faceCurrency": "USD", "unit": nil,
		"status": "active", "availability": "available", "availabilityChangedAt": "2026-10-23T08:00:00.000Z",
		"createdAt": "2026-10-01T00:00:00.000Z", "updatedAt": "2026-10-23T08:00:00.000Z",
	}
}

const (
	fakeProducts = 300
	// Từ dòng này trở đi tên, thương hiệu và danh mục dài: 100 dòng không vừa trần kết quả.
	wideFrom = 200
	fakeSKUs = 120
)

// fakePE là PE giả lập, trả đúng hình dạng JSON của các endpoint v1 (đọc từ mã nguồn PE:
// routes/api/v1/**/+server.ts, catalog-dto.ts, pricebook.ts).
func fakePE(rec *recorder) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/prices/{sku}", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		view := map[string]any{}
		if q.Get("includeIneligible") == "true" {
			view["includeIneligible"] = true
		}
		if n := intParam(r, "limit", 0); n > 0 {
			view["limit"] = n
		}
		if c := q.Get("currency"); c != "" {
			view["currency"] = c
		}
		switch sku := r.PathValue("sku"); sku {
		case "ERR-401":
			http.Error(w, "Unauthorized: Invalid API Key "+leak, http.StatusUnauthorized)
		case "ERR-403":
			fail(w, http.StatusForbidden, "errors_forbidden")
		case "ERR-429":
			w.Header().Set("Retry-After", "7")
			http.Error(w, "Too Many Requests "+leak, http.StatusTooManyRequests)
		case "ERR-503":
			w.Header().Set("Retry-After", "30")
			fail(w, http.StatusServiceUnavailable, "maintenance_desc")
		case "ERR-500":
			fail(w, http.StatusInternalServerError, "boom "+testAPIKey)
		case "ERR-302":
			http.Redirect(w, r, "/api/v1/prices/HQ-FREEFIRE-GC-20", http.StatusFound)
		case "ERR-HUGE":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"sku":"ERR-HUGE","name":"` + strings.Repeat("x", 9<<20) + `"}`))
		case "ERR-FLOAT":
			b := book("HQ-FREEFIRE-GC-20", view)
			b["sku"] = sku
			b["offers"].([]any)[0].(map[string]any)["price"] = 17.5
			writeJSON(w, http.StatusOK, b)
		default:
			if b := book(sku, view); b != nil {
				writeJSON(w, http.StatusOK, b)
				return
			}
			fail(w, http.StatusNotFound, "errors_sku_not_found")
		}
	})
	mux.HandleFunc("POST /api/v1/prices/batch", func(w http.ResponseWriter, r *http.Request) {
		last := rec.last()
		var in struct {
			SKUs              []string `json:"skus"`
			IncludeIneligible bool     `json:"includeIneligible"`
			Limit             int      `json:"limit"`
			Currency          string   `json:"currency"`
		}
		if err := json.Unmarshal([]byte(last.body), &in); err != nil || len(in.SKUs) == 0 {
			fail(w, http.StatusBadRequest, "errors_invalid_parameters")
			return
		}
		view := map[string]any{"limit": in.Limit}
		if in.IncludeIneligible {
			view["includeIneligible"] = true
		}
		if in.Currency != "" {
			view["currency"] = in.Currency
		}
		results, notFound := map[string]any{}, []string{}
		for _, sku := range in.SKUs {
			if b := book(sku, view); b != nil {
				results[sku] = b
			} else {
				notFound = append(notFound, sku)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"results": results, "notFound": notFound})
	})
	mux.HandleFunc("GET /api/v1/products", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if c := q.Get("cursor"); c != "" && !regexp.MustCompile(`^c[0-9]+$`).MatchString(c) {
			fail(w, http.StatusBadRequest, "errors_invalid_parameters")
			return
		}
		start, n := cursorOffset(q.Get("cursor")), intParam(r, "pageSize", 100)
		data := []any{}
		for i := start; i < min(start+n, fakeProducts); i++ {
			name, brand, category := "Product "+strconv.Itoa(i), any("Free Fire"), any("Gift Card")
			if i >= wideFrom {
				name, brand, category = strings.Repeat("Tên dài ", 60), strings.Repeat("Hãng ", 80), strings.Repeat("Loại ", 40)
			}
			data = append(data, map[string]any{
				"sku": fmt.Sprintf("HQ-P-%03d", i), "name": name, "brand": brand, "category": category,
				"faceValue": "20.000000", "faceCurrency": "USD", "region": "GLOBAL", "status": "active",
				"availability": nil, "availabilityChangedAt": nil, "updatedAt": "2026-10-23T08:00:00.000Z",
			})
		}
		var next any
		if start+n < fakeProducts {
			next = "c" + strconv.Itoa(start+n)
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": data, "meta": map[string]any{
			"total": fakeProducts, "page": 1, "pageSize": n, "totalPages": 3, "nextCursor": next}})
	})
	mux.HandleFunc("GET /api/v1/skus", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if code := q.Get("code"); code != "" {
			data, former := []any{}, false
			switch code {
			case "HQ-SKU-000":
				data = []any{apiSKU(0)}
			case "HQ_OLD_000":
				data, former = []any{apiSKU(0)}, true
			}
			writeJSON(w, http.StatusOK, map[string]any{"data": data, "meta": map[string]any{
				"nextCursor": nil, "hasMore": false, "deltaToken": nil, "matchedFormerCode": former}})
			return
		}
		start, n := cursorOffset(q.Get("cursor")), intParam(r, "limit", 100)
		data := []any{}
		for i := start; i < min(start+n, fakeSKUs); i++ {
			data = append(data, apiSKU(i))
		}
		meta := map[string]any{"nextCursor": nil, "hasMore": false, "deltaToken": "ZGVsdGE", "matchedFormerCode": nil}
		if start+n < fakeSKUs {
			meta = map[string]any{"nextCursor": "c" + strconv.Itoa(start+n), "hasMore": true, "deltaToken": nil, "matchedFormerCode": nil}
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": data, "meta": meta})
	})
	mux.HandleFunc("GET /api/v1/skus/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != skuID {
			fail(w, http.StatusNotFound, "errors_sku_not_found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": apiSKU(0)})
	})
	mux.HandleFunc("GET /api/v1/brands", func(w http.ResponseWriter, r *http.Request) {
		start, n := cursorOffset(r.URL.Query().Get("cursor")), intParam(r, "limit", 100)
		data := []any{}
		for i := start; i < min(start+n, 3); i++ {
			data = append(data, map[string]any{
				"id": fmt.Sprintf("0198a1b2-c3d4-7e5f-8a9b-%012d", i+1), "name": "Brand " + strconv.Itoa(i), "nameKey": "brand" + strconv.Itoa(i),
				"status": "active", "regions": []string{"GLOBAL", "VN"},
				"createdAt": "2026-10-01T00:00:00.000Z", "updatedAt": "2026-10-23T08:00:00.000Z",
			})
		}
		meta := map[string]any{"nextCursor": nil, "hasMore": false, "deltaToken": "ZGVsdGE"}
		if start+n < 3 {
			meta = map[string]any{"nextCursor": "c" + strconv.Itoa(start+n), "hasMore": true, "deltaToken": nil}
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": data, "meta": meta})
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		if r.Header.Get("Authorization") != "Bearer "+testAPIKey {
			fail(w, http.StatusUnauthorized, "errors_api_key_header_required")
			return
		}
		mux.ServeHTTP(w, r)
	})
}

var (
	testSigner = contract.TestSigner()
	testOrg    = uuid.New()
)

// headerTransport ký ngữ cảnh của org sở hữu cho mỗi request. execution: ngữ cảnh mang
// ExecutionID (tool call của Agent Runtime); không thì là client bên ngoài gọi trực tiếp.
type headerTransport struct {
	signer    *signedctx.Signer
	execution bool
}

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c := signedctx.Context{Kind: signedctx.KindCall, Audience: pricingengine.Slug,
		OrgID: testOrg, ProjectID: uuid.New(), Initiator: "user:" + uuid.NewString()}
	if h.execution {
		id := uuid.New()
		c.ExecutionID = &id
	}
	return (&signedctx.Transport{Signer: h.signer, Context: c}).RoundTrip(r)
}

func session(t *testing.T, client *pricingengine.Client, execution bool) *mcp.ClientSession {
	t.Helper()
	signer := testSigner
	srv := httptest.NewServer(pricingengine.Handler(client, signer.Verifier(), testOrg))
	t.Cleanup(srv.Close)
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	sess, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: srv.URL, DisableStandaloneSSE: true, MaxRetries: -1,
		HTTPClient: &http.Client{Transport: headerTransport{signer, execution}},
	}, nil)
	if err != nil {
		t.Fatalf("không kết nối được với ngữ cảnh hợp lệ: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

// connect dựng server MCP trên client đã cho và nối một MCP client thật vào, với ngữ cảnh
// ký của org sở hữu TRONG một execution (đường của Agent Runtime).
func connect(t *testing.T, client *pricingengine.Client) *mcp.ClientSession {
	t.Helper()
	return session(t, client, true)
}

// connectDirect: như connect nhưng không có execution (client bên ngoài gọi qua Gateway).
func connectDirect(t *testing.T, client *pricingengine.Client) *mcp.ClientSession {
	t.Helper()
	return session(t, client, false)
}

// clock là đồng hồ giả của client (ngắt mạch).
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newFake(t *testing.T) (*pricingengine.Client, *recorder, *clock) {
	t.Helper()
	rec := &recorder{}
	api := httptest.NewServer(fakePE(rec))
	t.Cleanup(api.Close)
	clk := &clock{now: time.Date(2026, 11, 23, 8, 0, 0, 0, time.UTC)}
	client, err := pricingengine.NewClient(api.URL, pricingengine.Options{APIKey: testAPIKey, AllowLoopbackHTTP: true, Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	return client, rec, clk
}

// call gọi một tool và trả về kết quả cùng phần có cấu trúc dạng JSON (hoặc văn bản lỗi).
func call(t *testing.T, sess *mcp.ClientSession, name string, args any) (*mcp.CallToolResult, string) {
	t.Helper()
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		// Arguments không qua schema là lỗi giao thức; coi như kết quả lỗi.
		return &mcp.CallToolResult{IsError: true}, err.Error()
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

func manySKUs(prefix string, n int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s%d", prefix, i)
	}
	return out
}

// Bộ test hợp đồng dùng chung của managed MCP server (mcp-platform §7).
func TestContract(t *testing.T) {
	client, rec, _ := newFake(t)
	signer := testSigner
	maxBytes := pricingengine.MaxResultBytes
	contract.Run(t, contract.Server{
		Slug:           pricingengine.Slug,
		Handler:        pricingengine.Handler(client, signer.Verifier(), testOrg),
		Signer:         signer,
		OrgID:          testOrg,
		TargetRequests: rec.count,
		Tools: []contract.Tool{
			{Name: "get_price_book", MaxResultBytes: maxBytes, Samples: []map[string]any{
				{"sku": "HQ-FREEFIRE-GC-20"}, {"sku": "HQ-FREEFIRE-GC-20", "include_ineligible": true, "limit": 100, "currency": "EUR"},
				{"sku": "HQ-BIG", "include_ineligible": true}}},
			{Name: "get_price_books", MaxResultBytes: maxBytes, Samples: []map[string]any{
				{"skus": []any{"HQ-FREEFIRE-GC-20", "HQ-UNKNOWN"}},
				{"skus": manySKUs("HQ-N", 50), "include_ineligible": true, "currency": "EUR"},
				{"skus": manySKUs("HQ-BIG", 8)}}},
			{Name: "list_products", Direct: true, MaxResultBytes: maxBytes, Samples: []map[string]any{
				{}, {"page_size": 100, "updated_since": "2026-10-22T00:00:00Z"}, {"cursor": "c200", "page_size": 100}}},
			{Name: "list_skus", Direct: true, MaxResultBytes: maxBytes, Samples: []map[string]any{
				{}, {"limit": 100, "status": "active", "brand_id": brandID, "region": "GLOBAL"}, {"sku_code": "HQ-SKU-000"}}},
			{Name: "get_sku", Direct: true, MaxResultBytes: maxBytes, Samples: []map[string]any{{"id": skuID}}},
			{Name: "list_brands", Direct: true, MaxResultBytes: maxBytes, Samples: []map[string]any{{}, {"limit": 100, "status": "inactive"}}},
		},
	})
}

type offerOut struct {
	Rank     int `json:"rank"`
	Supplier struct {
		Code string `json:"code"`
		Name string `json:"name"`
	} `json:"supplier"`
	SupplierProductID string   `json:"supplier_product_id"`
	Price             string   `json:"price"`
	Currency          string   `json:"currency"`
	ConvertedPrice    string   `json:"converted_price"`
	Available         bool     `json:"available"`
	PriceUpdatedAt    string   `json:"price_updated_at"`
	Stale             bool     `json:"stale"`
	Eligible          bool     `json:"eligible"`
	IneligibleReason  string   `json:"ineligible_reason"`
	Score             *float64 `json:"score"`
	ScoreBreakdown    *struct {
		Price   float64  `json:"price"`
		Speed   *float64 `json:"speed"`
		Success *float64 `json:"success"`
	} `json:"score_breakdown"`
	LowConfidence bool `json:"low_confidence"`
}

type bookOut struct {
	SKU                string `json:"sku"`
	CanonicalSKU       string `json:"canonical_sku"`
	Name               string `json:"name"`
	GeneratedAt        string `json:"generated_at"`
	ComparisonCurrency string `json:"comparison_currency"`
	ConvertedCurrency  string `json:"converted_currency"`
	ScoringProfile     struct {
		ID      string `json:"id"`
		Version int    `json:"version"`
	} `json:"scoring_profile"`
	Offers        []offerOut `json:"offers"`
	OffersOmitted int        `json:"offers_omitted"`
}

func TestPriceBook(t *testing.T) {
	client, rec, _ := newFake(t)
	sess := connect(t, client)

	res, out := call(t, sess, "get_price_book", map[string]any{"sku": "HQ-FREEFIRE-GC-20"})
	b := decode[bookOut](t, out)
	if res.IsError || b.SKU != "HQ-FREEFIRE-GC-20" || b.Name != "Free Fire Gift Card $20" || b.GeneratedAt != "2026-10-23T08:00:05.000Z" ||
		b.ComparisonCurrency != "USD" || b.ScoringProfile.ID != "default" || b.ScoringProfile.Version != 3 || len(b.Offers) != 2 || b.CanonicalSKU != "" {
		t.Fatalf("bảng giá: %s", out)
	}
	first, second := b.Offers[0], b.Offers[1]
	if first.Rank != 1 || first.Supplier.Code != "SUP_A" || first.Supplier.Name != "Supplier A" || first.SupplierProductID != "a-123" ||
		first.Price != "17.000000" || first.Currency != "USD" || !first.Available || first.PriceUpdatedAt != "2026-10-23T07:55:00.000Z" ||
		first.Stale || !first.Eligible || first.Score == nil || *first.Score != 92.3 || first.LowConfidence ||
		first.ScoreBreakdown == nil || first.ScoreBreakdown.Price != 100 || *first.ScoreBreakdown.Speed != 63.2 || *first.ScoreBreakdown.Success != 60.2 {
		t.Errorf("offer 1: %+v", first)
	}
	// Điểm 0 là điểm thật, khác với "không có điểm"; speed / success null thì vắng mặt.
	if second.Score == nil || *second.Score != 0 || second.ScoreBreakdown == nil || second.ScoreBreakdown.Speed != nil || !second.LowConfidence {
		t.Errorf("offer 2: %+v", second)
	}
	// Tiền là chuỗi trong JSON, không phải số.
	if !strings.Contains(out, `"price":"17.000000"`) {
		t.Errorf("price phải là chuỗi thập phân: %s", out)
	}
	if got := rec.last(); got.method != "GET" || got.path != "/api/v1/prices/HQ-FREEFIRE-GC-20" || got.query != "" {
		t.Errorf("request: %+v", got)
	}

	res, out = call(t, sess, "get_price_book", map[string]any{"sku": "HQ_OLD", "include_ineligible": true, "limit": 3, "currency": "EUR"})
	b = decode[bookOut](t, out)
	if res.IsError || b.SKU != "HQ_OLD" || b.CanonicalSKU != "HQ-FREEFIRE-GC-20" || b.ConvertedCurrency != "EUR" || len(b.Offers) != 3 {
		t.Fatalf("mã cũ: %s", out)
	}
	if b.Offers[0].ConvertedPrice != "15.640000" || b.Offers[1].ConvertedPrice != "" {
		t.Errorf("converted_price: %+v", b.Offers[:2])
	}
	if last := b.Offers[2]; last.Eligible || last.Rank != 0 || last.IneligibleReason != "unavailable" || last.Score != nil || last.ScoreBreakdown != nil {
		t.Errorf("offer bị loại: %+v", last)
	}
	if got := rec.last(); got.query != "currency=EUR&includeIneligible=true&limit=3" {
		t.Errorf("query gửi tới PE: %q", got.query)
	}

	res, out = call(t, sess, "get_price_book", map[string]any{"sku": "HQ-EMPTY"})
	if b = decode[bookOut](t, out); res.IsError || b.Offers == nil || len(b.Offers) != 0 || !strings.Contains(out, `"offers":[]`) {
		t.Errorf("SKU chưa có offer phải trả offers: []: %s", out)
	}

	// Bảng giá lớn hơn trần: offer ở cuối bị bỏ và được đếm.
	res, out = call(t, sess, "get_price_book", map[string]any{"sku": "HQ-BIG"})
	b = decode[bookOut](t, out)
	if res.IsError || len(out) > pricingengine.MaxResultBytes || b.OffersOmitted == 0 || len(b.Offers)+b.OffersOmitted != 400 || b.Offers[0].Rank != 1 {
		t.Errorf("bảng giá lớn: %d byte, %d offer, bỏ %d", len(out), len(b.Offers), b.OffersOmitted)
	}
}

type booksOut struct {
	PriceBooks   []bookOut `json:"price_books"`
	NotFound     []string  `json:"not_found"`
	DeferredSKUs []string  `json:"deferred_skus"`
}

func TestPriceBooks(t *testing.T) {
	client, rec, _ := newFake(t)
	sess := connect(t, client)

	res, out := call(t, sess, "get_price_books", map[string]any{
		"skus": []any{"HQ-UNKNOWN", "HQ_OLD", "HQ-FREEFIRE-GC-20"}, "limit": 1, "currency": "EUR"})
	b := decode[booksOut](t, out)
	if res.IsError || len(b.PriceBooks) != 2 || b.PriceBooks[0].SKU != "HQ_OLD" || b.PriceBooks[0].CanonicalSKU != "HQ-FREEFIRE-GC-20" ||
		b.PriceBooks[1].SKU != "HQ-FREEFIRE-GC-20" || len(b.PriceBooks[1].Offers) != 1 || len(b.NotFound) != 1 || b.NotFound[0] != "HQ-UNKNOWN" || len(b.DeferredSKUs) != 0 {
		t.Fatalf("lô: %s", out)
	}
	got := rec.last()
	if got.method != "POST" || got.path != "/api/v1/prices/batch" ||
		got.body != `{"skus":["HQ-UNKNOWN","HQ_OLD","HQ-FREEFIRE-GC-20"],"limit":1,"currency":"EUR"}` {
		t.Errorf("request gửi tới PE: %+v", got)
	}

	// Lô lớn hơn trần: SKU đầu được trả (cắt bớt offer), các SKU không vừa được hoãn, không mất SKU nào.
	res, out = call(t, sess, "get_price_books", map[string]any{"skus": []any{"HQ-BIG1", "HQ-FREEFIRE-GC-20", "HQ-BIG2", "HQ-UNKNOWN"}})
	b = decode[booksOut](t, out)
	if res.IsError || len(out) > pricingengine.MaxResultBytes || len(b.PriceBooks) != 1 || b.PriceBooks[0].SKU != "HQ-BIG1" || b.PriceBooks[0].OffersOmitted == 0 ||
		strings.Join(b.DeferredSKUs, ",") != "HQ-FREEFIRE-GC-20,HQ-BIG2" || strings.Join(b.NotFound, ",") != "HQ-UNKNOWN" {
		t.Errorf("lô lớn: %d byte, books=%d deferred=%v not_found=%v", len(out), len(b.PriceBooks), b.DeferredSKUs, b.NotFound)
	}
}

type productsOut struct {
	Products []struct {
		SKU       string `json:"sku"`
		Name      string `json:"name"`
		Brand     string `json:"brand"`
		FaceValue string `json:"face_value"`
		Status    string `json:"status"`
		UpdatedAt string `json:"updated_at"`
	} `json:"products"`
	Total      int    `json:"total"`
	NextCursor string `json:"next_cursor"`
}

type skuOut struct {
	ID          string   `json:"id"`
	Code        string   `json:"code"`
	FormerCodes []string `json:"former_codes"`
	Name        string   `json:"name"`
	BrandID     string   `json:"brand_id"`
	BrandName   string   `json:"brand_name"`
	ProductType string   `json:"product_type"`
	Region      string   `json:"region"`
	FaceValue   string   `json:"face_value"`
	Unit        string   `json:"unit"`
	Status      string   `json:"status"`
}

type skusOut struct {
	SKUs              []skuOut `json:"skus"`
	NextCursor        string   `json:"next_cursor"`
	HasMore           bool     `json:"has_more"`
	MatchedFormerCode bool     `json:"matched_former_code"`
}

type brandsOut struct {
	Brands []struct {
		ID      string   `json:"id"`
		Name    string   `json:"name"`
		Status  string   `json:"status"`
		Regions []string `json:"regions"`
	} `json:"brands"`
	NextCursor string `json:"next_cursor"`
	HasMore    bool   `json:"has_more"`
}

func TestCatalogue(t *testing.T) {
	client, rec, _ := newFake(t)
	sess := connect(t, client)

	// list_products: phân trang bằng cursor / next_cursor.
	res, out := call(t, sess, "list_products", map[string]any{"page_size": 100, "updated_since": "2026-10-22T07:00:00+07:00"})
	p := decode[productsOut](t, out)
	if res.IsError || len(p.Products) != 100 || p.Total != fakeProducts || p.NextCursor != "c100" || p.Products[0].SKU != "HQ-P-000" ||
		p.Products[0].FaceValue != "20.000000" || p.Products[0].Brand != "Free Fire" {
		t.Fatalf("list_products: %.300s", out)
	}
	if got := rec.last(); got.path != "/api/v1/products" || got.query != "pageSize=100&updatedSince=2026-10-22T00%3A00%3A00Z" {
		t.Errorf("request: %+v", got)
	}
	res, out = call(t, sess, "list_products", map[string]any{"cursor": p.NextCursor, "page_size": 100})
	if p = decode[productsOut](t, out); res.IsError || p.Products[0].SKU != "HQ-P-100" || p.NextCursor != "c200" {
		t.Fatalf("trang hai: %.300s", out)
	}
	// Trang có dòng dài không vừa trần: server đọc lại PE với trang nhỏ hơn, nên con trỏ
	// vẫn là con trỏ của PE và không dòng nào bị bỏ sót.
	before := rec.count()
	res, out = call(t, sess, "list_products", map[string]any{"cursor": "c200", "page_size": 100})
	p = decode[productsOut](t, out)
	if res.IsError || len(out) > pricingengine.MaxResultBytes || len(p.Products) != 50 || p.NextCursor != "c250" || p.Products[0].SKU != "HQ-P-200" {
		t.Fatalf("trang dài: %d byte, %d dòng, next=%q", len(out), len(p.Products), p.NextCursor)
	}
	if n := rec.count() - before; n != 2 || rec.last().query != "cursor=c200&pageSize=50" {
		t.Errorf("muốn 2 request (100 rồi 50), nhận %d; cuối: %q", n, rec.last().query)
	}
	if len(p.Products[0].Name) > 210 {
		t.Errorf("tên không được cắt: %d byte", len(p.Products[0].Name))
	}
	res, out = call(t, sess, "list_products", map[string]any{"cursor": "c250", "page_size": 50})
	if p = decode[productsOut](t, out); res.IsError || len(p.Products) != 50 || p.NextCursor != "" {
		t.Errorf("trang cuối phải không có next_cursor: %d dòng, next=%q", len(p.Products), p.NextCursor)
	}

	// list_skus: bộ lọc, phân trang, tra theo mã.
	res, out = call(t, sess, "list_skus", map[string]any{"limit": 100, "status": "active", "brand_id": brandID, "region": "GLOBAL"})
	s := decode[skusOut](t, out)
	if res.IsError || len(s.SKUs) != 100 || !s.HasMore || s.NextCursor != "c100" || s.MatchedFormerCode {
		t.Fatalf("list_skus: %.300s", out)
	}
	if first := s.SKUs[0]; first.ID != skuID || first.Code != "HQ-SKU-000" || len(first.FormerCodes) != 1 || first.BrandID != brandID ||
		first.BrandName != "Free Fire" || first.ProductType != "giftcard" || first.FaceValue != "20.000000" || first.Unit != "" || first.Status != "active" {
		t.Errorf("sku: %+v", first)
	}
	if got := rec.last(); got.path != "/api/v1/skus" || got.query != "brandId="+brandID+"&limit=100&region=GLOBAL&status=active" {
		t.Errorf("request: %+v", got)
	}
	res, out = call(t, sess, "list_skus", map[string]any{"cursor": "c100", "limit": 100, "status": "active", "brand_id": brandID, "region": "GLOBAL"})
	if s = decode[skusOut](t, out); res.IsError || len(s.SKUs) != 20 || s.HasMore || s.NextCursor != "" {
		t.Errorf("trang cuối: %.200s", out)
	}
	// deltaToken của PE không bao giờ tới Agent.
	if strings.Contains(out, "ZGVsdGE") || strings.Contains(strings.ToLower(out), "delta") {
		t.Errorf("kết quả lộ deltaToken: %.300s", out)
	}
	res, out = call(t, sess, "list_skus", map[string]any{"sku_code": "HQ_OLD_000"})
	if s = decode[skusOut](t, out); res.IsError || len(s.SKUs) != 1 || s.SKUs[0].ID != skuID || !s.MatchedFormerCode {
		t.Errorf("tra theo mã cũ: %s", out)
	}
	if got := rec.last(); got.query != "code=HQ_OLD_000" {
		t.Errorf("tra theo mã phải gửi đúng một tham số: %q", got.query)
	}
	res, out = call(t, sess, "list_skus", map[string]any{"sku_code": "HQ-NOPE"})
	if s = decode[skusOut](t, out); res.IsError || len(s.SKUs) != 0 {
		t.Errorf("mã không có: %s", out)
	}

	// get_sku
	res, out = call(t, sess, "get_sku", map[string]any{"id": strings.ToUpper(skuID)})
	if k := decode[skuOut](t, out); res.IsError || k.ID != skuID || k.Code != "HQ-SKU-000" {
		t.Errorf("get_sku: %s", out)
	}
	if got := rec.last(); got.method != "GET" || got.path != "/api/v1/skus/"+skuID {
		t.Errorf("request: %+v", got)
	}
	if res, out = call(t, sess, "get_sku", map[string]any{"id": brandID}); !res.IsError || !strings.Contains(out, "SKU not found") {
		t.Errorf("SKU không có phải là lỗi 'SKU not found': %s", out)
	}

	// list_brands: nameKey của PE không được chuyển tiếp.
	res, out = call(t, sess, "list_brands", map[string]any{"limit": 2})
	b := decode[brandsOut](t, out)
	if res.IsError || len(b.Brands) != 2 || b.Brands[0].Name != "Brand 0" || strings.Join(b.Brands[0].Regions, ",") != "GLOBAL,VN" || !b.HasMore || b.NextCursor != "c2" {
		t.Fatalf("list_brands: %s", out)
	}
	if strings.Contains(strings.ToLower(out), "key") || strings.Contains(out, "brand0") {
		t.Errorf("kết quả mang nameKey của PE: %s", out)
	}
	res, out = call(t, sess, "list_brands", map[string]any{"cursor": "c2", "limit": 2, "status": "active"})
	if res.IsError || !strings.Contains(out, `"has_more":false`) || strings.Contains(out, "next_cursor") {
		t.Errorf("trang cuối: %s", out)
	}
	if got := rec.last(); got.query != "cursor=c2&limit=2&status=active" {
		t.Errorf("request: %+v", got)
	}
}

// Lỗi của PE được suy từ mã trạng thái; văn bản của PE và khoá không bao giờ tới Agent.
func TestUpstreamErrors(t *testing.T) {
	client, rec, clk := newFake(t)
	sess := connect(t, client)
	clean := func(name, out string) {
		t.Helper()
		for _, bad := range []string{leak, testAPIKey, "sk_live", "errors_", "maintenance_desc", "127.0.0.1"} {
			if strings.Contains(out, bad) {
				t.Errorf("%s: lỗi lộ %q: %s", name, bad, out)
			}
		}
	}
	for sku, want := range map[string]string{
		"HQ-UNKNOWN": "SKU not found",
		"ERR-401":    "refused the credential",
		"ERR-403":    "refused the credential",
		"ERR-500":    "unexpected status (500)",
		"ERR-302":    "redirect",
		"ERR-HUGE":   "too large",
		"ERR-FLOAT":  "cannot read",
	} {
		res, out := call(t, sess, "get_price_book", map[string]any{"sku": sku})
		if !res.IsError || !strings.Contains(out, want) {
			t.Errorf("%s: muốn lỗi chứa %q, nhận %s", sku, want, out)
		}
		clean(sku, out)
	}
	// Redirect không được theo: đích của nó không nhận request nào.
	for _, r := range rec.all() {
		if r.path == "/api/v1/prices/HQ-FREEFIRE-GC-20" {
			t.Error("server đã theo redirect của PE")
		}
	}

	// 503: bảo trì, kèm thời gian thử lại; không đóng ngắt mạch.
	res, out := call(t, sess, "get_price_book", map[string]any{"sku": "ERR-503"})
	if !res.IsError || !strings.Contains(out, `"error":"pricing_engine_maintenance"`) || !strings.Contains(out, `"retry_after_seconds":30`) {
		t.Errorf("503: %s", out)
	}
	clean("503", out)
	if res, out = call(t, sess, "get_sku", map[string]any{"id": skuID}); res.IsError {
		t.Errorf("sau 503 các lời gọi khác vẫn phải chạy: %s", out)
	}

	// 429: lỗi tạm thời kèm thời gian thử lại, và ngắt mạch: trong lúc chờ không request nào tới PE.
	res, out = call(t, sess, "get_price_book", map[string]any{"sku": "ERR-429"})
	if !res.IsError || !strings.Contains(out, `"error":"pricing_engine_rate_limited"`) || !strings.Contains(out, `"retry_after_seconds":7`) {
		t.Errorf("429: %s", out)
	}
	clean("429", out)
	before := rec.count()
	clk.advance(3 * time.Second)
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"get_price_book", map[string]any{"sku": "HQ-FREEFIRE-GC-20"}},
		{"get_price_books", map[string]any{"skus": []any{"HQ-FREEFIRE-GC-20"}}},
		{"list_products", map[string]any{}}, {"list_skus", map[string]any{}},
		{"get_sku", map[string]any{"id": skuID}}, {"list_brands", map[string]any{}},
	} {
		res, out = call(t, sess, tc.tool, tc.args)
		if !res.IsError || !strings.Contains(out, `"error":"pricing_engine_rate_limited"`) || !strings.Contains(out, `"retry_after_seconds":4`) {
			t.Errorf("%s trong lúc ngắt mạch: %s", tc.tool, out)
		}
	}
	if n := rec.count() - before; n != 0 {
		t.Errorf("ngắt mạch còn hiệu lực mà %d request vẫn tới PE", n)
	}
	clk.advance(5 * time.Second)
	if res, out = call(t, sess, "get_price_book", map[string]any{"sku": "HQ-FREEFIRE-GC-20"}); res.IsError {
		t.Errorf("hết thời gian chờ thì phải gọi lại được: %s", out)
	}
}

// Đầu vào sai dạng bị từ chối trước khi có request nào tới PE: qua schema (đường của
// Gateway và MCP SDK) và qua chính client (lớp thứ hai, nếu schema bị nới).
func TestInvalidInputNeverReachesUpstream(t *testing.T) {
	client, rec, _ := newFake(t)
	sess := connect(t, client)
	longSKU := strings.Repeat("A", 101)
	for i, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"get_price_book", map[string]any{}},
		{"get_price_book", map[string]any{"sku": "../admin"}},
		{"get_price_book", map[string]any{"sku": "HQ/A"}},
		{"get_price_book", map[string]any{"sku": "HQ-A?includeIneligible=true"}},
		{"get_price_book", map[string]any{"sku": "HQ-A#x"}},
		{"get_price_book", map[string]any{"sku": "HQ..A"}},
		{"get_price_book", map[string]any{"sku": "HQ--A"}},
		{"get_price_book", map[string]any{"sku": "hq-a"}},
		{"get_price_book", map[string]any{"sku": "HQ-A%2F"}},
		{"get_price_book", map[string]any{"sku": "HQ-A\n"}},
		{"get_price_book", map[string]any{"sku": longSKU}},
		{"get_price_book", map[string]any{"sku": "HQ-A", "limit": 0}},
		{"get_price_book", map[string]any{"sku": "HQ-A", "limit": 101}},
		{"get_price_book", map[string]any{"sku": "HQ-A", "currency": "usd"}},
		{"get_price_book", map[string]any{"sku": "HQ-A", "currency": "USDT"}},
		{"get_price_book", map[string]any{"sku": "HQ-A", "include_ineligible": "true"}},
		{"get_price_book", map[string]any{"sku": "HQ-A", "url": "https://evil.example"}},
		{"get_price_books", map[string]any{"skus": []any{}}},
		{"get_price_books", map[string]any{"skus": manySKUs("HQ-N", 51)}},
		{"get_price_books", map[string]any{"skus": []any{"HQ-A", "HQ-A"}}},
		{"get_price_books", map[string]any{"skus": []any{"HQ-A", "../x"}}},
		{"get_price_books", map[string]any{"skus": "HQ-A"}},
		{"list_products", map[string]any{"page_size": 101}},
		{"list_products", map[string]any{"cursor": "a b"}},
		{"list_products", map[string]any{"cursor": strings.Repeat("a", 513)}},
		{"list_products", map[string]any{"updated_since": "2026-10-22"}},
		{"list_products", map[string]any{"updated_since": "2026-13-45T00:00:00Z"}},
		{"list_products", map[string]any{"delta_token": "x"}},
		{"list_skus", map[string]any{"status": "deleted"}},
		{"list_skus", map[string]any{"brand_id": "not-a-uuid"}},
		{"list_skus", map[string]any{"region": "vn"}},
		{"list_skus", map[string]any{"limit": 500}},
		{"list_skus", map[string]any{"sku_code": "HQ-SKU-000", "status": "active"}},
		{"list_skus", map[string]any{"sku_code": "HQ-SKU-000", "limit": 10}},
		{"list_skus", map[string]any{"sku_code": "hq sku"}},
		{"get_sku", map[string]any{}},
		{"get_sku", map[string]any{"id": "HQ-SKU-000"}},
		{"get_sku", map[string]any{"id": skuID + "/../../admin"}},
		{"get_sku", map[string]any{"id": "../../../admin/settings/aaaaaaaaaaaa"}},
		{"list_brands", map[string]any{"status": "ACTIVE"}},
		{"list_brands", map[string]any{"limit": -1}},
	} {
		if res, out := call(t, sess, tc.tool, tc.args); !res.IsError {
			t.Errorf("ca %d: %s %v được chấp nhận: %.200s", i, tc.tool, tc.args, out)
		}
	}

	ctx := context.Background()
	for _, sku := range []string{"", "../admin", "HQ/A", "HQ-A?x=1", "HQ-A#x", "..", "HQ..A", "hq-a", "HQ-A ", longSKU} {
		if _, err := client.PriceBook(ctx, sku, pricingengine.View{}); err == nil {
			t.Errorf("client.PriceBook(%q) được chấp nhận", sku)
		}
		if _, err := client.PriceBooks(ctx, []string{"HQ-A", sku}, pricingengine.View{}); err == nil {
			t.Errorf("client.PriceBooks(%q) được chấp nhận", sku)
		}
		if sku != "" {
			if _, err := client.SKUs(ctx, "", 0, pricingengine.SKUFilter{Code: sku}); err == nil {
				t.Errorf("client.SKUs(code=%q) được chấp nhận", sku)
			}
		}
	}
	for _, id := range []string{"", "..", skuID + "/x", "0198a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5g", "%2e%2e"} {
		if _, err := client.SKU(ctx, id); err == nil {
			t.Errorf("client.SKU(%q) được chấp nhận", id)
		}
	}
	if _, err := client.PriceBook(ctx, "HQ-A", pricingengine.View{Limit: 101}); err == nil {
		t.Error("limit 101 được chấp nhận")
	}
	if _, err := client.PriceBook(ctx, "HQ-A", pricingengine.View{Currency: "usd"}); err == nil {
		t.Error("currency usd được chấp nhận")
	}
	if _, err := client.Products(ctx, "a/b", 0, time.Time{}); err == nil {
		t.Error("cursor sai dạng được chấp nhận")
	}
	if _, err := client.Brands(ctx, "", 0, "deleted"); err == nil {
		t.Error("status lạ được chấp nhận")
	}
	if _, err := client.SKUs(ctx, "", 101, pricingengine.SKUFilter{}); err == nil {
		t.Error("limit 101 được chấp nhận")
	}
	if n := rec.count(); n != 0 {
		t.Errorf("đầu vào sai dạng vẫn tạo %d request tới PE: %+v", n, rec.all())
	}
}

// Bề mặt chỉ đọc: mọi request tới PE là GET tới năm đường đọc, hoặc POST tới đúng
// /prices/batch; khoá chỉ đi trong header Authorization; không tool nào nhận URL hay khoá.
func TestReadOnlySurface(t *testing.T) {
	client, rec, _ := newFake(t)
	sess := connect(t, client)
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"get_price_book", map[string]any{"sku": "HQ-FREEFIRE-GC-20", "include_ineligible": true, "currency": "EUR"}},
		{"get_price_book", map[string]any{"sku": "HQ-UNKNOWN"}},
		{"get_price_books", map[string]any{"skus": []any{"HQ-FREEFIRE-GC-20", "HQ-UNKNOWN"}}},
		{"list_products", map[string]any{"cursor": "c200", "page_size": 100}},
		{"list_skus", map[string]any{"status": "inactive"}}, {"list_skus", map[string]any{"sku_code": "HQ-SKU-000"}},
		{"get_sku", map[string]any{"id": skuID}}, {"list_brands", map[string]any{}},
	} {
		_, out := call(t, sess, tc.tool, tc.args)
		if strings.Contains(out, testAPIKey) || strings.Contains(out, "sk_live") || strings.Contains(out, "Bearer") {
			t.Errorf("%s: kết quả lộ khoá: %.200s", tc.tool, out)
		}
	}
	allowed := regexp.MustCompile(`^(GET /api/v1/(prices/[A-Z0-9_-]+|products|skus|skus/[0-9a-f-]{36}|brands)|POST /api/v1/prices/batch)$`)
	seen := rec.all()
	if len(seen) < 8 {
		t.Fatalf("muốn ≥ 8 request tới PE, nhận %d", len(seen))
	}
	for _, r := range seen {
		if !allowed.MatchString(r.method + " " + r.path) {
			t.Errorf("request ngoài bề mặt chỉ đọc: %s %s", r.method, r.path)
		}
		if r.auth != "Bearer "+testAPIKey {
			t.Errorf("%s %s: thiếu header Authorization", r.method, r.path)
		}
		if strings.Contains(r.query, testAPIKey) || strings.Contains(r.body, testAPIKey) || strings.Contains(strings.ToLower(r.query), "deltatoken") {
			t.Errorf("%s %s: khoá hoặc deltaToken nằm trong query / body", r.method, r.path)
		}
		if r.method == http.MethodGet && r.body != "" {
			t.Errorf("GET %s có body", r.path)
		}
	}

	tools, err := sess.ListTools(context.Background(), nil)
	if err != nil || len(tools.Tools) != 6 {
		t.Fatalf("tools/list: %v, %d tool", err, len(tools.Tools))
	}
	forbidden := regexp.MustCompile(`(?i)(token|secret|password|key|credential|url|host|endpoint)`)
	for _, tool := range tools.Tools {
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("%s: thiếu readOnlyHint", tool.Name)
		}
		for _, schema := range []any{tool.InputSchema, tool.OutputSchema} {
			raw, _ := json.Marshal(schema)
			var walk func(node any)
			walk = func(node any) {
				switch v := node.(type) {
				case map[string]any:
					if props, ok := v["properties"].(map[string]any); ok {
						for name := range props {
							if forbidden.MatchString(name) {
								t.Errorf("%s: tên trường bị cấm %q", tool.Name, name)
							}
						}
					}
					for _, sub := range v {
						walk(sub)
					}
				case []any:
					for _, sub := range v {
						walk(sub)
					}
				}
			}
			var node any
			_ = json.Unmarshal(raw, &node)
			walk(node)
		}
		if !strings.Contains(tool.Description, "Chỉ đọc.") {
			t.Errorf("%s: mô tả phải nói rõ là chỉ đọc", tool.Name)
		}
		if strings.HasPrefix(tool.Name, "get_price") && !strings.Contains(tool.Description, "GIÁ MUA VÀO của nhà cung cấp") {
			t.Errorf("%s: mô tả phải nói rõ đây là giá mua vào của nhà cung cấp", tool.Name)
		}
	}
}

func TestNewClient(t *testing.T) {
	local := pricingengine.Options{APIKey: testAPIKey, AllowLoopbackHTTP: true}
	strict := pricingengine.Options{APIKey: testAPIKey}
	bad := map[string]struct {
		url  string
		opts pricingengine.Options
	}{
		"thiếu khoá":                        {"https://pe.example", pricingengine.Options{}},
		"khoá chỉ có khoảng trắng":          {"https://pe.example", pricingengine.Options{APIKey: " \n"}},
		"khoá có xuống dòng":                {"https://pe.example", pricingengine.Options{APIKey: "sk_live_a\nX-Evil: 1"}},
		"http ngoài local":                  {"http://pe.example", strict},
		"http loopback ngoài local":         {"http://127.0.0.1:3001", strict},
		"http localhost ngoài local":        {"http://localhost:3001", strict},
		"local: http tới tên nội bộ":        {"http://pe.internal:3001", local},
		"local: http tới IP riêng":          {"http://10.0.0.5:3001", local},
		"local: http tới IP link-local":     {"http://169.254.169.254", local},
		"local: http tới 0.0.0.0":           {"http://0.0.0.0:3001", local},
		"local: tên giả loopback":           {"http://127.0.0.1.evil.example", local},
		"local: localhost là tên miền con":  {"http://localhost.evil.example", local},
		"local: userinfo che host":          {"http://localhost@evil.example", local},
		"local: IPv4 ánh xạ không loopback": {"http://[::ffff:10.0.0.5]:3001", local},
		"không phải http(s)":                {"ftp://pe.example", strict},
		"credential trong URL":              {"https://user:pass@pe.example", strict},
		"query trong URL":                   {"https://pe.example/?a=1", strict},
		"rỗng":                              {"", strict},
	}
	for name, tc := range bad {
		_, err := pricingengine.NewClient(tc.url, tc.opts)
		if err == nil {
			t.Errorf("%s: phải bị từ chối", name)
		} else if strings.Contains(err.Error(), "sk_live") {
			t.Errorf("%s: lỗi lộ khoá: %v", name, err)
		}
	}
	for name, tc := range map[string]struct {
		url  string
		opts pricingengine.Options
	}{
		"https ngoài local":      {"https://pe.example/prefix/", pricingengine.Options{APIKey: testAPIKey + "\n"}},
		"https ở local":          {"https://pe.internal", local},
		"local: http localhost":  {"http://localhost:3001", local},
		"local: http LOCALHOST":  {"http://LOCALHOST:3001", local},
		"local: http 127.0.0.1":  {"http://127.0.0.1:3001", local},
		"local: http 127.8.9.1":  {"http://127.8.9.1", local},
		"local: http [::1]":      {"http://[::1]:3001", local},
		"local: IPv4 ánh xạ 127": {"http://[::ffff:127.0.0.1]:3001", local},
	} {
		if _, err := pricingengine.NewClient(tc.url, tc.opts); err != nil {
			t.Errorf("%s: phải được chấp nhận: %v", name, err)
		}
	}
}

// PE không tới được: lỗi chung, không lộ địa chỉ nội bộ.
func TestUnavailable(t *testing.T) {
	api := httptest.NewServer(http.NotFoundHandler())
	addr := api.URL
	api.Close()
	client, err := pricingengine.NewClient(addr, pricingengine.Options{APIKey: testAPIKey, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	sess := connect(t, client)
	res, out := call(t, sess, "list_brands", map[string]any{})
	if !res.IsError || !strings.Contains(out, "unreachable") {
		t.Errorf("muốn lỗi 'unreachable': %s", out)
	}
	if strings.Contains(out, strings.TrimPrefix(addr, "http://")) || strings.Contains(out, testAPIKey) {
		t.Errorf("lỗi lộ địa chỉ hoặc khoá: %s", out)
	}
}
