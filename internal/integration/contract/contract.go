// Package contract là bộ test hợp đồng dùng chung cho managed MCP server
// (mcp-platform §7, roadmap Phase 4). Mọi integration trong internal/integration
// gọi contract.Run trong test của nó, với server dựng trên một hệ thống đích giả lập.
//
// Bộ test nói chuyện với server qua đúng đường mà Gateway dùng: MCP Streamable HTTP
// kèm header ngữ cảnh đã ký (internal/shared/signedctx). Nó kiểm những điều chuẩn
// kiểm được tự động; phần còn lại (xem ghi chú ở mcp-platform §7) vẫn cần người review.
//
// Đây là package test: code sản phẩm không import nó (luật depguard
// no-integration-contract-in-prod).
package contract

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hqgroups/mcp/internal/shared/signedctx"
)

// Server mô tả một managed MCP server cần kiểm.
type Server struct {
	// Slug là định danh của server trong Registry, cũng là đích (aud) của ngữ cảnh ký.
	Slug string
	// Handler là HTTP handler của server, dựng với Signer.Verifier() và OrgID bên dưới,
	// trên một hệ thống đích giả lập.
	Handler http.Handler
	// Signer đóng vai Gateway: bộ test ký ngữ cảnh bằng nó. Handler chỉ được giữ khoá
	// công khai tương ứng.
	Signer *signedctx.Signer
	// OrgID là organization mà server phục vụ. uuid.Nil nghĩa là server không gắn với
	// một org; khi đó phần kiểm "org khác bị từ chối" bị bỏ qua (và việc cách ly tenant
	// của server phải được review bằng tay).
	OrgID uuid.UUID
	// TargetRequests trả về số request mà hệ thống đích giả lập đã nhận. Bộ test dùng
	// nó để khẳng định request bị từ chối không chạm tới hệ thống đích.
	TargetRequests func() int
	// Tools là đúng tập tool mà server phải công bố.
	Tools []Tool
}

// Tool là những gì integration khai về một tool.
type Tool struct {
	Name string
	// Direct: tool được gọi trực tiếp (không thuộc execution), tức server khai nó bằng
	// serve.Direct. Bộ test kiểm cả hai chiều: tool Direct chạy được khi gọi trực tiếp,
	// tool không Direct bị từ chối mà không chạm tới hệ thống đích.
	Direct bool
	// Write: tool có tác động lên hệ thống đích. Tool không khai Write phải mang
	// readOnlyHint và không được có tham số của tool ghi.
	Write bool
	// NoDryRun là lý do một tool ghi không hỗ trợ dry_run (hệ thống đích không cho
	// phép). Rỗng thì tool ghi bắt buộc có dry_run.
	NoDryRun string
	// MaxResultBytes là trần kích thước kết quả đề xuất cho Registry. Mọi kết quả
	// của Samples phải nằm trong trần này (đo như Gateway đo: phần văn bản; và đo cả
	// phần có cấu trúc).
	MaxResultBytes int
	// Samples là các bộ arguments hợp lệ, gồm cả bộ tạo ra kết quả lớn nhất mà hệ
	// thống đích giả lập trả được. Với tool ghi, bộ test tự thêm idempotency_key.
	Samples []map[string]any
	// Effects (bắt buộc với tool ghi) trả về số tác động đã xảy ra trên hệ thống đích
	// giả lập, để kiểm idempotency_key và dry_run.
	Effects func() int
}

// Run chạy bộ test hợp đồng. Mỗi vi phạm là một lỗi test.
func Run(t *testing.T, srv Server) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if srv.OrgID == uuid.Nil {
		t.Log("contract: server không gắn với một org; bỏ qua phần kiểm tool call của org khác")
	}
	for _, v := range Check(ctx, srv) {
		t.Error("contract: " + v)
	}
}

// Check trả về danh sách vi phạm (rỗng nếu server đạt). Run là cách dùng thông
// thường; Check tồn tại để chính bộ test này được kiểm bằng server cố ý sai.
func Check(ctx context.Context, srv Server) []string {
	c := &checker{srv: srv}
	if srv.Slug == "" || srv.Handler == nil || srv.Signer == nil || srv.TargetRequests == nil || len(srv.Tools) == 0 {
		return []string{"khai báo thiếu: cần Slug, Handler, Signer, TargetRequests và ít nhất một tool"}
	}
	c.signer = srv.Signer
	hs := httptest.NewServer(srv.Handler)
	defer hs.Close()
	c.url = hs.URL

	tools := c.checkDefinitions(ctx)
	c.checkSignature(ctx)
	if tools != nil {
		c.checkDiscovery(ctx, len(tools))
		c.checkOtherOrg(ctx)
		c.checkDirect(ctx)
		c.checkCalls(ctx, tools)
	}
	return c.violations
}

type checker struct {
	srv        Server
	signer     *signedctx.Signer
	url        string
	violations []string
}

func (c *checker) failf(format string, args ...any) {
	c.violations = append(c.violations, fmt.Sprintf(format, args...))
}

// callCtx là ngữ cảnh tool call của org; inExecution thêm ExecutionID như Gateway làm
// trên tuyến của Agent Runtime.
func (c *checker) callCtx(org uuid.UUID, inExecution bool) signedctx.Context {
	if org == uuid.Nil {
		org = uuid.New() // server không gắn org: org nào cũng là tenant hợp lệ
	}
	sc := signedctx.Context{Kind: signedctx.KindCall, Audience: c.srv.Slug, OrgID: org,
		ProjectID: uuid.New(), Initiator: "user:" + uuid.NewString()}
	if inExecution {
		exec := uuid.New()
		sc.ExecutionID = &exec
	}
	return sc
}

func (c *checker) discoveryCtx() signedctx.Context {
	return signedctx.Context{Kind: signedctx.KindDiscovery, Audience: c.srv.Slug}
}

// signed trả về hàm ký sc cho đúng body (cho post).
func (c *checker) signed(sc signedctx.Context) func(body string) string {
	return func(body string) string { return c.signer.SignBody(sc, []byte(body), time.Now()) }
}

// fixed trả về đúng một giá trị header bất kể body (cho các ca chữ ký sai).
func fixed(header string) func(string) string { return func(string) string { return header } }

// session nối một MCP client thật vào server; mỗi request mang một ngữ cảnh mới ký cho
// đúng body của nó, như Gateway.
func (c *checker) session(ctx context.Context, sc signedctx.Context) (*mcp.ClientSession, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "contract", Version: "0"}, nil)
	return client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: c.url, DisableStandaloneSSE: true, MaxRetries: -1,
		HTTPClient: &http.Client{Transport: &signedctx.Transport{Signer: c.signer, Context: sc}, Timeout: time.Minute},
	}, nil)
}

// post gửi một thông điệp JSON-RPC thô, không qua SDK.
func (c *checker) post(ctx context.Context, sign func(body string) string, body string) (int, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, strings.NewReader(body))
	if err != nil {
		return 0, err.Error()
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if header := sign(body); header != "" {
		req.Header.Set(signedctx.Header, header)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(b)
}

func (c *checker) declared(name string) *Tool {
	for i := range c.srv.Tools {
		if c.srv.Tools[i].Name == name {
			return &c.srv.Tools[i]
		}
	}
	return nil
}

func rpc(method string, params any) string {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	return string(b)
}

// refused báo một phản hồi thô là lời từ chối: mã HTTP không phải 2xx, hoặc một phản hồi
// JSON-RPC có "error" ở cấp cao nhất, hoặc kết quả tool mang isError. Một kết quả thành
// công có trường tên "error" ở đâu đó bên trong KHÔNG phải lời từ chối.
func refused(status int, resp string) bool {
	if status < 200 || status >= 300 {
		return true
	}
	one := func(raw json.RawMessage) bool {
		var msg struct {
			Error  json.RawMessage `json:"error"`
			Result *struct {
				IsError bool `json:"isError"`
			} `json:"result"`
		}
		if json.Unmarshal(raw, &msg) != nil {
			return false
		}
		return (len(msg.Error) > 0 && string(msg.Error) != "null") || (msg.Result != nil && msg.Result.IsError)
	}
	var batch []json.RawMessage
	if json.Unmarshal([]byte(resp), &batch) == nil && len(batch) > 0 {
		for _, raw := range batch {
			if !one(raw) {
				return false
			}
		}
		return true
	}
	return one(json.RawMessage(resp))
}

func truncateBody(s string) string {
	if len(s) > 160 {
		return s[:160] + "…"
	}
	return s
}

func (c *checker) firstCall() string {
	t := c.srv.Tools[0]
	var args map[string]any
	if len(t.Samples) > 0 {
		args = t.Samples[0]
	}
	return rpc("tools/call", map[string]any{"name": t.Name, "arguments": withKey(t, args)})
}

func withKey(t Tool, args map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range args {
		out[k] = v
	}
	if t.Write {
		out["idempotency_key"] = uuid.NewString()
	}
	return out
}

// checkSignature: request không có ngữ cảnh, hoặc ngữ cảnh không do Gateway ký cho
// đúng server này và đúng body này, hoặc đã dùng một lần, bị từ chối trước khi chạm tới
// hệ thống đích.
func (c *checker) checkSignature(ctx context.Context) {
	otherPriv, _ := signedctx.ParseSigningKey(strings.Repeat("5a", 32))
	other, _ := signedctx.NewSigner(otherPriv)
	call := c.callCtx(c.srv.OrgID, true)
	wrongAud := call
	wrongAud.Audience = c.srv.Slug + "-other"
	expired := func(body string) string {
		return c.signer.SignBody(call, []byte(body), time.Now().Add(-time.Hour))
	}
	// Đổi nội dung nhưng giữ chữ ký cũ.
	forged := func(body string) string {
		valid := c.signer.SignBody(call, []byte(body), time.Now())
		_, sig, _ := strings.Cut(valid, ".")
		f := call
		f.OrgID = uuid.New()
		f.BodySHA256 = signedctx.BodyHash([]byte(body))
		f.ID = uuid.NewString()
		raw, _ := json.Marshal(f)
		return base64.RawURLEncoding.EncodeToString(raw) + "." + sig
	}
	noSig := func(body string) string {
		return strings.SplitN(c.signer.SignBody(call, []byte(body), time.Now()), ".", 2)[0] + "."
	}
	otherBody := func(string) string { return c.signer.SignBody(call, []byte(`{"jsonrpc":"2.0"}`), time.Now()) }

	cases := []struct {
		name string
		sign func(string) string
	}{
		{"không có ngữ cảnh", fixed("")},
		{"ngữ cảnh rác", fixed("abc.def")},
		{"ký bằng khoá khác", func(b string) string { return other.SignBody(call, []byte(b), time.Now()) }},
		{"đích là server khác", c.signed(wrongAud)},
		{"đã hết hạn", expired},
		{"nội dung bị sửa sau khi ký", forged},
		{"mất chữ ký", noSig},
		{"ký cho body khác", otherBody},
	}
	bodies := []string{rpc("tools/list", map[string]any{}), c.firstCall()}
	for _, tc := range cases {
		for _, body := range bodies {
			c.expectUnauthorized(ctx, tc.name, tc.sign, body)
		}
	}
	// Dùng lại đúng header của một request đã được nhận.
	for _, body := range bodies {
		header := c.signer.SignBody(call, []byte(body), time.Now())
		if status, _ := c.post(ctx, fixed(header), body); status != http.StatusOK {
			c.failf("chữ ký: request hợp lệ bị từ chối (status %d)", status)
			continue
		}
		c.expectUnauthorized(ctx, "dùng lại header", fixed(header), body)
	}
}

func (c *checker) expectUnauthorized(ctx context.Context, name string, sign func(string) string, body string) {
	before := c.srv.TargetRequests()
	status, resp := c.post(ctx, sign, body)
	if status != http.StatusUnauthorized && status != http.StatusForbidden {
		c.failf("chữ ký: %s: muốn 401 / 403, nhận %d", name, status)
	}
	if n := c.srv.TargetRequests() - before; n != 0 {
		c.failf("chữ ký: %s: %d request đã tới hệ thống đích", name, n)
	}
	for _, t := range c.srv.Tools {
		if strings.Contains(resp, t.Name) {
			c.failf("chữ ký: %s: phản hồi từ chối lộ tên tool %s", name, t.Name)
		}
	}
}

// checkDiscovery: ngữ cảnh discovery (không gắn tenant) liệt kê được tool nhưng
// không gọi được tool nào.
func (c *checker) checkDiscovery(ctx context.Context, want int) {
	sess, err := c.session(ctx, c.discoveryCtx())
	if err != nil {
		c.failf("discovery: không kết nối được bằng ngữ cảnh discovery: %v", err)
		return
	}
	defer func() { _ = sess.Close() }()
	if list, err := sess.ListTools(ctx, nil); err != nil || len(list.Tools) != want {
		c.failf("discovery: tools/list phải trả %d tool (lỗi: %v)", want, err)
	}
	before := c.srv.TargetRequests()
	for _, t := range c.srv.Tools {
		for _, args := range t.Samples {
			res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: t.Name, Arguments: withKey(t, args)})
			if err == nil && !res.IsError {
				c.failf("discovery: ngữ cảnh discovery gọi được tool %s", t.Name)
			}
		}
	}
	// Cùng điều đó, không qua SDK: một thông điệp thô, một batch, và các body mà hai trình
	// đọc JSON hiểu khác nhau. Server phân quyền bằng cách tự đọc "method" trong body (thay
	// vì trên method mà MCP SDK đã đọc ra) sẽ thấy tools/list trong khi SDK chạy tools/call:
	// encoding/json khớp tên khoá không phân biệt hoa thường, SDK thì có.
	call := c.firstCall()
	bodies := []string{call, "[" + call + "]"}
	for _, decoy := range []string{`"Method":"tools/list"`, `"METHOD":"tools/list"`, `"method ":"tools/list"`} {
		bodies = append(bodies,
			strings.Replace(call, `"method":"tools/call"`, `"method":"tools/call",`+decoy, 1), // mồi đứng sau
			strings.Replace(call, `"method":"tools/call"`, decoy+`,"method":"tools/call"`, 1)) // mồi đứng trước
	}
	for _, body := range bodies {
		if status, resp := c.post(ctx, c.signed(c.discoveryCtx()), body); !refused(status, resp) {
			c.failf("discovery: tools/call thô được chấp nhận (status %d): %s", status, truncateBody(body))
		}
	}
	if n := c.srv.TargetRequests() - before; n != 0 {
		c.failf("discovery: %d request đã tới hệ thống đích", n)
	}
}

// checkOtherOrg: server gắn với một org từ chối tool call thay mặt org khác, kể cả
// khi chữ ký hợp lệ.
func (c *checker) checkOtherOrg(ctx context.Context) {
	if c.srv.OrgID == uuid.Nil {
		return
	}
	before := c.srv.TargetRequests()
	stranger := c.callCtx(uuid.New(), true)
	if sess, err := c.session(ctx, stranger); err == nil {
		defer func() { _ = sess.Close() }()
		for _, t := range c.srv.Tools {
			for _, args := range t.Samples {
				res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: t.Name, Arguments: withKey(t, args)})
				if err == nil && !res.IsError {
					c.failf("tenant: org khác gọi được tool %s", t.Name)
				}
			}
		}
	}
	if status, resp := c.post(ctx, c.signed(c.callCtx(uuid.New(), true)), c.firstCall()); !refused(status, resp) {
		c.failf("tenant: tools/call thô của org khác được chấp nhận (status %d)", status)
	}
	if n := c.srv.TargetRequests() - before; n != 0 {
		c.failf("tenant: %d request của org khác đã tới hệ thống đích", n)
	}
}

var (
	// Động từ + danh từ, snake_case: ít nhất hai từ.
	toolNameRe = regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)+$`)
	fieldRe    = regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`)
)

const maxDescriptionBytes = 1024

// Tên tool cấm: tool "chạy lệnh bất kỳ". So theo từng từ của tên.
var forbiddenToolWords = []string{"exec", "execute", "shell", "bash", "eval", "sql", "script", "command", "cmd"}

// Tên trường cấm. Credential không đi qua tham số tool; không có trường nhận lệnh,
// script hay câu SQL tự do. Danh sách là heuristic theo tên: nó bắt lỗi vô ý, không
// thay được review (một trường tên khác vẫn có thể mang cùng thứ đó).
var forbiddenFields = map[string]string{
	"password": "credential", "passwd": "credential", "secret": "credential", "token": "credential",
	"api_key": "credential", "apikey": "credential", "access_key": "credential", "secret_key": "credential",
	"private_key": "credential", "ssh_key": "credential", "client_secret": "credential",
	"credential": "credential", "credentials": "credential", "authorization": "credential",
	"auth": "credential", "bearer": "credential", "cookie": "credential", "dsn": "credential",
	"connection_string": "credential", "kubeconfig": "credential",
	"command": "lệnh tự do", "cmd": "lệnh tự do", "exec": "lệnh tự do", "shell": "lệnh tự do",
	"script": "lệnh tự do", "code": "lệnh tự do", "argv": "lệnh tự do", "args": "lệnh tự do",
	"eval": "lệnh tự do", "stdin": "lệnh tự do",
	"sql": "SQL tự do", "statement": "SQL tự do", "raw_sql": "SQL tự do", "raw_query": "SQL tự do",
	"url": "đích do người gọi chọn", "endpoint": "đích do người gọi chọn",
	"base_url": "đích do người gọi chọn", "headers": "đích do người gọi chọn",
}

var forbiddenSuffixes = map[string]string{
	"_password": "credential", "_secret": "credential", "_credential": "credential", "_credentials": "credential",
	"_api_key": "credential", "_private_key": "credential", "_token": "credential",
	"_sql": "SQL tự do", "_command": "lệnh tự do", "_script": "lệnh tự do", "_url": "đích do người gọi chọn",
}

// gatewayRedacted là các tên trường mà Gateway che GIÁ TRỊ trong mọi kết quả dạng JSON
// (internal/mcp/gateway/redact.go, secretField; chép lại ở đây vì package này không
// import Gateway). Một trường hợp pháp mang tên như vậy sẽ tới model ở dạng
// "[REDACTED:secret_field]": con trỏ phân trang tên next_page_token đã hỏng đúng theo
// cách đó, nên con trỏ phải tên cursor / next_cursor.
var gatewayRedacted = regexp.MustCompile(`(?i)(password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|credential)`)

func forbiddenField(name string) string {
	n := strings.ToLower(name)
	if why, ok := forbiddenFields[n]; ok {
		return why
	}
	if gatewayRedacted.MatchString(n) {
		return "tên trường bị Gateway che giá trị (con trỏ phân trang phải tên cursor / next_cursor)"
	}
	for suffix, why := range forbiddenSuffixes {
		if strings.HasSuffix(n, suffix) {
			return why
		}
	}
	return ""
}

func hasType(schema map[string]any, want string) bool {
	switch t := schema["type"].(type) {
	case string:
		return t == want
	case []any:
		return slices.Contains(t, any(want))
	}
	return false
}

// walkOutputNames kiểm tên trường của output schema: không trường nào mang tên mà
// Gateway che giá trị (xem gatewayRedacted).
func (c *checker) walkOutputNames(tool, path string, node any, depth int) {
	schema, ok := node.(map[string]any)
	if !ok || depth > 16 {
		return
	}
	props, _ := schema["properties"].(map[string]any)
	for name, sub := range props {
		if gatewayRedacted.MatchString(name) {
			c.failf("schema: %s: trường kết quả %s.%s bị Gateway che giá trị; đổi tên (con trỏ phân trang: next_cursor)", tool, path, name)
		}
		c.walkOutputNames(tool, path+"."+name, sub, depth+1)
	}
	c.walkOutputNames(tool, path+"[items]", schema["items"], depth+1)
	for _, key := range []string{"anyOf", "oneOf", "allOf"} {
		list, _ := schema[key].([]any)
		for _, sub := range list {
			c.walkOutputNames(tool, path+"["+key+"]", sub, depth+1)
		}
	}
}

// walkSchema kiểm mọi nút object của một input schema: không nhận trường lạ, tên
// trường snake_case và không nằm trong danh sách cấm.
func (c *checker) walkSchema(tool, path string, node any, depth int) {
	schema, ok := node.(map[string]any)
	if !ok || depth > 16 {
		return
	}
	props, _ := schema["properties"].(map[string]any)
	if hasType(schema, "object") || props != nil {
		if ap, ok := schema["additionalProperties"].(bool); !ok || ap {
			c.failf("schema: %s%s phải có additionalProperties: false", tool, path)
		}
		if _, ok := schema["patternProperties"]; ok {
			c.failf("schema: %s%s dùng patternProperties (trường tên tự do)", tool, path)
		}
	}
	for name, sub := range props {
		if !fieldRe.MatchString(name) {
			c.failf("schema: %s%s.%s: tên trường phải là snake_case", tool, path, name)
		}
		if why := forbiddenField(name); why != "" {
			c.failf("schema: %s%s.%s: tên trường bị cấm (%s)", tool, path, name, why)
		}
		c.walkSchema(tool, path+"."+name, sub, depth+1)
	}
	for _, key := range []string{"items", "additionalProperties", "not"} {
		c.walkSchema(tool, path+"["+key+"]", schema[key], depth+1)
	}
	for _, key := range []string{"anyOf", "oneOf", "allOf", "prefixItems"} {
		list, _ := schema[key].([]any)
		for _, sub := range list {
			c.walkSchema(tool, path+"["+key+"]", sub, depth+1)
		}
	}
	for _, key := range []string{"$defs", "definitions"} {
		defs, _ := schema[key].(map[string]any)
		for name, sub := range defs {
			c.walkSchema(tool, path+"["+key+"/"+name+"]", sub, depth+1)
		}
	}
}

func schemaMap(v any) map[string]any {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	return m
}

// checkDefinitions đọc tools/list bằng ngữ cảnh của org sở hữu và kiểm định nghĩa
// từng tool. Trả về schema theo tên tool, hoặc nil nếu không đọc được.
func (c *checker) checkDefinitions(ctx context.Context) map[string]map[string]any {
	sess, err := c.session(ctx, c.callCtx(c.srv.OrgID, true))
	if err != nil {
		c.failf("không kết nối được bằng ngữ cảnh hợp lệ: %v", err)
		return nil
	}
	defer func() { _ = sess.Close() }()
	list, err := sess.ListTools(ctx, nil)
	if err != nil {
		c.failf("tools/list: %v", err)
		return nil
	}
	schemas := map[string]map[string]any{}
	for _, tool := range list.Tools {
		name := tool.Name
		decl := c.declared(name)
		if decl == nil {
			c.failf("tool %s được công bố nhưng không được khai trong test hợp đồng", name)
			continue
		}
		if _, dup := schemas[name]; dup {
			c.failf("tool %s được công bố hai lần", name)
		}
		if !toolNameRe.MatchString(name) {
			c.failf("tool %s: tên phải là động từ + danh từ, snake_case", name)
		}
		for _, w := range strings.Split(name, "_") {
			if slices.Contains(forbiddenToolWords, w) {
				c.failf("tool %s: tên gợi ý tool chạy lệnh / SQL tự do (%q)", name, w)
			}
		}
		if d := strings.TrimSpace(tool.Description); d == "" || len(d) > maxDescriptionBytes {
			c.failf("tool %s: mô tả phải có và không quá %d byte (đang %d)", name, maxDescriptionBytes, len(d))
		}
		schema := schemaMap(tool.InputSchema)
		schemas[name] = schema
		if !hasType(schema, "object") {
			c.failf("schema: %s: input schema phải có type object", name)
		}
		c.walkSchema(name, "", schema, 0)
		// Tên trường của KẾT QUẢ: giá trị của trường bị Gateway che thì không tới được model.
		c.walkOutputNames(name, "", schemaMap(tool.OutputSchema), 0)

		readOnly := tool.Annotations != nil && tool.Annotations.ReadOnlyHint
		props, _ := schema["properties"].(map[string]any)
		required, _ := schema["required"].([]any)
		_, hasKey := props["idempotency_key"]
		_, hasDry := props["dry_run"]
		if !decl.Write {
			if !readOnly {
				c.failf("tool %s: tool đọc phải mang readOnlyHint", name)
			}
			if hasKey || hasDry {
				c.failf("tool %s: tool đọc có idempotency_key / dry_run; nếu nó ghi thì phải khai Write", name)
			}
		} else {
			if readOnly {
				c.failf("tool %s: tool ghi mang readOnlyHint", name)
			}
			if key := schemaMap(props["idempotency_key"]); !hasType(key, "string") || !slices.Contains(required, any("idempotency_key")) {
				c.failf("tool %s: tool ghi phải có idempotency_key (string, bắt buộc)", name)
			}
			if dry := schemaMap(props["dry_run"]); decl.NoDryRun == "" && !hasType(dry, "boolean") {
				c.failf("tool %s: tool ghi phải có dry_run (boolean), hoặc khai NoDryRun kèm lý do", name)
			}
			if decl.Effects == nil {
				c.failf("tool %s: tool ghi phải khai Effects để kiểm idempotency", name)
			}
		}
		if decl.MaxResultBytes <= 0 {
			c.failf("tool %s: chưa khai MaxResultBytes", name)
		}
		if len(decl.Samples) == 0 {
			c.failf("tool %s: chưa khai Samples", name)
		}
	}
	for _, t := range c.srv.Tools {
		if _, ok := schemas[t.Name]; !ok {
			c.failf("tool %s được khai trong test hợp đồng nhưng server không công bố", t.Name)
		}
	}
	return schemas
}

// oversized là một chuỗi dài hơn mọi giá trị hợp lý của một tham số tool, nhưng vẫn
// nằm trong trần thân request.
var oversized = strings.Repeat("a", 256<<10)

func resultSizes(res *mcp.CallToolResult) (text, structured int) {
	for _, content := range res.Content {
		if tc, ok := content.(*mcp.TextContent); ok {
			text += len(tc.Text)
			continue
		}
		b, _ := json.Marshal(content)
		text += len(b)
	}
	if res.StructuredContent != nil {
		b, _ := json.Marshal(res.StructuredContent)
		structured = len(b)
	}
	return text, structured
}

// checkDirect: tool call của org sở hữu nhưng KHÔNG thuộc execution (client bên ngoài gọi
// thẳng qua Gateway). Tool khai Direct chạy được; tool khác bị từ chối và không chạm tới
// hệ thống đích. Mặc định của serve.Handler là chỉ-execution: khai sai theo chiều nào bộ
// test cũng bắt được.
func (c *checker) checkDirect(ctx context.Context) {
	sess, err := c.session(ctx, c.callCtx(c.srv.OrgID, false))
	if err != nil {
		c.failf("trực tiếp: không kết nối được: %v", err)
		return
	}
	defer func() { _ = sess.Close() }()
	for _, t := range c.srv.Tools {
		if len(t.Samples) == 0 {
			continue
		}
		before, done := c.srv.TargetRequests(), 0
		if t.Effects != nil {
			done = t.Effects()
		}
		res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: t.Name, Arguments: withKey(t, t.Samples[0])})
		ok := err == nil && !res.IsError
		switch {
		case t.Direct && !ok:
			c.failf("trực tiếp: tool %s khai Direct nhưng gọi trực tiếp bị từ chối (lỗi: %v)", t.Name, err)
		case !t.Direct && ok:
			c.failf("trực tiếp: tool %s không khai Direct nhưng gọi trực tiếp được; khai serve.Direct hoặc sửa khai báo", t.Name)
		case !t.Direct:
			if n := c.srv.TargetRequests() - before; n != 0 {
				c.failf("trực tiếp: tool %s bị từ chối nhưng %d request đã tới hệ thống đích", t.Name, n)
			}
			if t.Effects != nil && t.Effects() != done {
				c.failf("trực tiếp: tool %s bị từ chối nhưng đã gây tác động", t.Name)
			}
		}
	}
}

// checkCalls gọi từng tool bằng ngữ cảnh của org sở hữu: kết quả trong trần, trường
// lạ và chuỗi quá dài bị từ chối trước khi tới hệ thống đích, tool ghi tôn trọng
// idempotency_key và dry_run.
func (c *checker) checkCalls(ctx context.Context, schemas map[string]map[string]any) {
	sess, err := c.session(ctx, c.callCtx(c.srv.OrgID, true))
	if err != nil {
		c.failf("không kết nối được bằng ngữ cảnh hợp lệ: %v", err)
		return
	}
	defer func() { _ = sess.Close() }()
	rejected := func(t Tool, args map[string]any) bool {
		res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: t.Name, Arguments: args})
		return err != nil || res.IsError
	}
	effects := func(t Tool) int {
		if t.Effects == nil {
			return 0
		}
		return t.Effects()
	}

	for _, t := range c.srv.Tools {
		schema, ok := schemas[t.Name]
		if !ok || len(t.Samples) == 0 {
			continue
		}
		for i, args := range t.Samples {
			res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: t.Name, Arguments: withKey(t, args)})
			if err != nil || res.IsError {
				c.failf("tool %s: mẫu %d phải chạy được (lỗi: %v)", t.Name, i, err)
				continue
			}
			if res.StructuredContent == nil {
				c.failf("tool %s: mẫu %d: kết quả phải có cấu trúc (structuredContent)", t.Name, i)
			}
			if text, structured := resultSizes(res); text > t.MaxResultBytes || structured > t.MaxResultBytes {
				c.failf("tool %s: mẫu %d: kết quả %d byte (văn bản) / %d byte (có cấu trúc) vượt trần %d",
					t.Name, i, text, structured, t.MaxResultBytes)
			}
		}

		// Trường lạ và chuỗi quá dài: bị từ chối, không tới hệ thống đích, không gây tác động.
		requests, done := c.srv.TargetRequests(), effects(t)
		unknown := withKey(t, t.Samples[0])
		unknown["contract_unknown_field"] = "x"
		if !rejected(t, unknown) {
			c.failf("tool %s: arguments có trường lạ được chấp nhận", t.Name)
		}
		props, _ := schema["properties"].(map[string]any)
		for name, p := range props {
			prop, _ := p.(map[string]any)
			if !hasType(prop, "string") || prop["enum"] != nil || name == "idempotency_key" {
				continue
			}
			long := withKey(t, t.Samples[0])
			long[name] = oversized
			if !rejected(t, long) {
				c.failf("tool %s: tham số %s nhận chuỗi %d byte; cần giới hạn độ dài", t.Name, name, len(oversized))
			}
		}
		if n := c.srv.TargetRequests() - requests; n != 0 {
			c.failf("tool %s: arguments không hợp lệ vẫn tạo %d request tới hệ thống đích", t.Name, n)
		}
		if n := effects(t) - done; n != 0 {
			c.failf("tool %s: arguments không hợp lệ vẫn gây %d tác động", t.Name, n)
		}

		if !t.Write || t.Effects == nil {
			continue
		}
		// Cùng idempotency_key hai lần: đúng một tác động, và lần hai không phải lỗi.
		args := withKey(t, t.Samples[0])
		before := effects(t)
		for range 2 {
			if rejected(t, args) {
				c.failf("tool %s: gọi lại cùng idempotency_key phải trả kết quả, không phải lỗi", t.Name)
			}
		}
		if n := effects(t) - before; n != 1 {
			c.failf("tool %s: hai lần gọi cùng idempotency_key gây %d tác động, muốn đúng 1", t.Name, n)
		}
		if t.NoDryRun == "" {
			dry := withKey(t, t.Samples[0])
			dry["dry_run"] = true
			before = effects(t)
			if rejected(t, dry) {
				c.failf("tool %s: dry_run phải trả mô tả tác động dự kiến, không phải lỗi", t.Name)
			}
			if n := effects(t) - before; n != 0 {
				c.failf("tool %s: dry_run gây %d tác động", t.Name, n)
			}
		}
	}
}

// TestSigner trả về một Signer cố định cho test của integration: test dựng Handler bằng
// TestSigner().Verifier() và khai Server.Signer = TestSigner().
func TestSigner() *signedctx.Signer {
	priv, err := signedctx.ParseSigningKey(strings.Repeat("6b", 32))
	if err != nil {
		panic(err)
	}
	s, err := signedctx.NewSigner(priv)
	if err != nil {
		panic(err)
	}
	return s
}

// ExecutionTransport ký mỗi request như Gateway ký trên tuyến của Agent Runtime: ngữ cảnh
// tool call của org, kèm một ExecutionID, cho đúng body của request. Dùng trong test của
// integration để gọi tool qua MCP client thật. base nil là http.DefaultTransport.
func ExecutionTransport(signer *signedctx.Signer, slug string, org uuid.UUID, base http.RoundTripper) http.RoundTripper {
	exec := uuid.New()
	return &signedctx.Transport{Signer: signer, Base: base, Context: signedctx.Context{
		Kind: signedctx.KindCall, Audience: slug, OrgID: org, ProjectID: uuid.New(),
		Initiator: "user:" + uuid.NewString(), ExecutionID: &exec}}
}
