// Package api_test kiểm tra Management API đầu-cuối: xác thực OIDC, chọn tenant,
// phân quyền, và truy cập chéo tenant trên mọi endpoint. Chạy trên PostgreSQL
// thật dưới role app_api, với một issuer OIDC giả lập trong process.
package api_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/google/uuid"

	"go.temporal.io/sdk/client"

	"github.com/hqgroups/mcp/internal/adminops"
	"github.com/hqgroups/mcp/internal/agent/runtime"
	"github.com/hqgroups/mcp/internal/agent/runtime/durable"
	"github.com/hqgroups/mcp/internal/ai/provider"
	"github.com/hqgroups/mcp/internal/ai/provider/anthropic"
	"github.com/hqgroups/mcp/internal/apiserver"
	"github.com/hqgroups/mcp/internal/audit"
	"github.com/hqgroups/mcp/internal/cost/budget"
	"github.com/hqgroups/mcp/internal/gatewayserver"
	"github.com/hqgroups/mcp/internal/platform/identity"
	"github.com/hqgroups/mcp/internal/platform/secrets"
	"github.com/hqgroups/mcp/internal/runtimewire"
	"github.com/hqgroups/mcp/internal/shared/ids"
	"github.com/hqgroups/mcp/internal/shared/safehttp"
	"github.com/hqgroups/mcp/internal/shared/signedctx"
	"github.com/hqgroups/mcp/internal/testutil/pgtest"
	"github.com/hqgroups/mcp/internal/workflow/definitions/schedule"
)

const audience = "hq-api"

// issuer là một OIDC provider tối thiểu: discovery + JWKS + ký token.
type issuer struct {
	srv      *httptest.Server
	signer   jose.Signer
	jwksHits atomic.Int64
}

func newIssuer(t *testing.T) *issuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwk := jose.JSONWebKey{Key: key, KeyID: "k1", Algorithm: "RS256", Use: "sig"}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jwk},
		(&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	is := &issuer{signer: signer}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                is.srv.URL,
			"jwks_uri":                              is.srv.URL + "/jwks",
			"authorization_endpoint":                is.srv.URL + "/auth",
			"token_endpoint":                        is.srv.URL + "/token",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		is.jwksHits.Add(1)
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{jwk.Public()}})
	})
	is.srv = httptest.NewServer(mux)
	t.Cleanup(is.srv.Close)
	return is
}

func (is *issuer) sign(t *testing.T, s jose.Signer, claims map[string]any) string {
	t.Helper()
	b, _ := json.Marshal(claims)
	obj, err := s.Sign(b)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := obj.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// token phát hành một token hợp lệ cho sub, với các claim bổ sung / ghi đè.
func (is *issuer) token(t *testing.T, sub string, extra map[string]any) string {
	t.Helper()
	c := map[string]any{
		"iss": is.srv.URL, "sub": sub, "aud": audience, "email": sub + "@example.test",
		"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
	}
	for k, v := range extra {
		c[k] = v
	}
	return is.sign(t, is.signer, c)
}

type env struct {
	db         *pgtest.DB
	t          *testing.T
	api        *httptest.Server
	gw         *httptest.Server
	strictAPI  *httptest.Server
	llm        *fakeAnthropic
	model      *scriptedModel
	is         *issuer
	orgA, orgB uuid.UUID
	// Chỉ có khi chạy trên Temporal.
	// budgets là bộ đếm budget dùng chung cho Gateway và runtime trong process.
	budgets     *budget.MemoryCounter
	workerStops []func()
	temporal    client.Client
	steps       *runtime.Steps
	// opener mở secret như process gateway (cho bộ gửi thông báo trong test).
	opener secrets.Opener
}

// setup dựng môi trường test. durableRuntime = true: vòng lặp chạy trên Temporal thật
// (container dev server), worker do test tự khởi động bằng e.startWorker.
func setup(t *testing.T, durableRuntime ...bool) *env {
	t.Helper()
	db := pgtest.Start(t)
	ctx := context.Background()
	is := newIssuer(t)
	e := &env{t: t, db: db, is: is, orgA: ids.New(), orgB: ids.New()}

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := db.Admin.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	exec(`INSERT INTO organizations (id, slug, name) VALUES ($1,'org-a','Org A'), ($2,'org-b','Org B')`, e.orgA, e.orgB)
	// sub → (org, role). "multi" là admin ở cả hai org.
	for _, m := range []struct {
		sub, role string
		orgs      []uuid.UUID
	}{
		{"admin-a", "org_admin", []uuid.UUID{e.orgA}},
		{"admin-b", "org_admin", []uuid.UUID{e.orgB}},
		{"viewer-a", "viewer", []uuid.UUID{e.orgA}},
		{"multi-a", "org_admin", []uuid.UUID{e.orgA}},
		{"dev-a", "developer", []uuid.UUID{e.orgA}},
		{"multi", "org_admin", []uuid.UUID{e.orgA, e.orgB}},
		{"norole-a", "", []uuid.UUID{e.orgA}},
	} {
		uid := ids.New()
		exec(`INSERT INTO users (id, idp_issuer, idp_subject, email) VALUES ($1,$2,$3,$4)`,
			uid, is.srv.URL, m.sub, m.sub+"@example.test")
		for _, org := range m.orgs {
			exec(`INSERT INTO memberships (org_id, user_id) VALUES ($1,$2)`, org, uid)
			if m.role != "" {
				exec(`INSERT INTO role_bindings (org_id, role_id, principal_type, principal_id)
				      SELECT $1, id, 'user', $2 FROM roles WHERE org_id IS NULL AND name = $3`, org, uid, m.role)
			}
		}
	}
	exec(`INSERT INTO users (id, idp_issuer, idp_subject, email, status) VALUES ($1,$2,'disabled','d@example.test','disabled')`,
		ids.New(), is.srv.URL)

	verifier, err := identity.NewOIDCVerifier(ctx, is.srv.URL, audience, "org_id")
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	// Gateway thật (role app_gateway) cho đường tool call của Agent Runtime.
	signer, _ := signedctx.NewSigner(signingKey)
	gwMux := http.NewServeMux()
	e.gw = httptest.NewServer(gwMux)
	t.Cleanup(e.gw.Close)
	// Secrets: api chỉ có khoá công khai (mã hoá), gateway có khoá gốc (giải mã).
	rootHex, publicKey, err := secrets.GenerateLocalRootKey()
	if err != nil {
		t.Fatal(err)
	}
	root, _ := hex.DecodeString(rootHex)
	opener, err := secrets.NewLocalOpener(root, "local")
	if err != nil {
		t.Fatal(err)
	}
	e.opener = opener
	sealer, err := secrets.NewLocalSealer(publicKey, "local")
	if err != nil {
		t.Fatal(err)
	}
	e.llm = newFakeAnthropic(t)
	e.budgets = budget.NewMemoryCounter()
	if err := gatewayserver.Register(gwMux, db.PoolAs(t, "app_gateway", 4), verifier, signer,
		gatewayserver.Config{PublicURL: e.gw.URL, Issuer: is.srv.URL, ExecutionTokenKey: execTokenKey, ServiceTokenKey: svcTokenKey,
			UpstreamPolicy: safehttp.Policy{AllowInsecureLocal: true}, BudgetCounter: e.budgets,
			SecretOpener: opener, PlatformLLMKeys: map[string]string{"anthropic": platformLLMKey},
			LLMFactory: func(_, apiKey string) (provider.Provider, error) {
				return anthropic.New(anthropic.Config{APIKey: apiKey, BaseURL: e.llm.srv.URL})
			}}); err != nil {
		t.Fatalf("gateway: %v", err)
	}

	e.model = &scriptedModel{}
	runCtx, stop := context.WithCancel(ctx)
	apiCfg := apiserver.Config{
		Adapters: []string{"claude-cli"}, Providers: map[string]provider.Provider{"claude-cli": e.model},
		GatewayURL: e.gw.URL, ExecutionTokenKey: execTokenKey, ServiceTokenKey: svcTokenKey, InProcessRuntime: true,
		SecretSealer: sealer, Credentials: fakeCredentials{base: e.llm.srv.URL},
		UpstreamPolicy: safehttp.Policy{AllowInsecureLocal: true}, BudgetCounter: e.budgets,
	}
	if len(durableRuntime) > 0 && durableRuntime[0] {
		e.temporal = startTemporal(t)
		apiCfg.Runner = &durable.Starter{Client: e.temporal}
		apiCfg.InProcessRuntime = false
		// Lịch chạy của workflow: Temporal Schedules + worker api-internal trong "process api".
		apiCfg.WorkflowScheduler = schedule.Scheduler{Client: e.temporal}
		apiCfg.ScheduleHost = func(runs apiserver.ScheduledRuns) error {
			stopSchedules, err := schedule.Start(e.temporal, runs)
			if err == nil {
				t.Cleanup(stopSchedules)
			}
			return err
		}
		// Worker chạy dưới role app_worker, như process worker thật.
		steps, err := runtimewire.New(db.PoolAs(t, "app_worker", 4), e.gw.URL, execTokenKey, map[string]provider.Provider{"claude-cli": e.model}, nil)
		if err != nil {
			t.Fatalf("runtimewire: %v", err)
		}
		e.steps = steps
	}
	h, waitRuns, err := apiserver.New(runCtx, db.PoolAs(t, "app_api", 6), verifier, apiCfg)
	if err != nil {
		t.Fatalf("apiserver: %v", err)
	}
	e.api = httptest.NewServer(h)
	// Một API thứ hai trên cùng database với chính sách SSRF mặc định (nghiêm ngặt).
	strict, waitStrict, err := apiserver.New(runCtx, db.PoolAs(t, "app_api", 2), verifier, apiserver.Config{
		Adapters: []string{"claude-cli"}, GatewayURL: e.gw.URL, ExecutionTokenKey: execTokenKey, ServiceTokenKey: svcTokenKey, SecretSealer: sealer})
	if err != nil {
		t.Fatalf("apiserver: %v", err)
	}
	e.strictAPI = httptest.NewServer(strict)
	t.Cleanup(func() { e.strictAPI.Close(); waitStrict(context.Background()) })
	// Dừng execution nền trước khi đóng database.
	t.Cleanup(func() {
		stop()
		e.api.Close()
		wctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		waitRuns(wctx)
	})
	return e
}

var (
	execTokenKey = []byte(strings.Repeat("e", 32))
	svcTokenKey  = []byte(strings.Repeat("v", 32))
)

// scriptedModel là LLM giả: mỗi lượt gọi hàm do test đặt. Không gọi provider thật.
type scriptedModel struct {
	mu       sync.Mutex
	fn       func(ctx context.Context, n int, req provider.Request) (provider.Response, error)
	requests []provider.Request
}

func (m *scriptedModel) Name() string { return "claude-cli" }

func (m *scriptedModel) script(fn func(ctx context.Context, n int, req provider.Request) (provider.Response, error)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fn, m.requests = fn, nil
}

func (m *scriptedModel) seen() []provider.Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]provider.Request(nil), m.requests...)
}

func (m *scriptedModel) Complete(ctx context.Context, req provider.Request) (provider.Response, error) {
	m.mu.Lock()
	n := len(m.requests)
	m.requests = append(m.requests, req)
	fn := m.fn
	m.mu.Unlock()
	if fn == nil {
		return provider.Response{StopReason: provider.StopEndTurn, Text: "ok", ModelUsed: req.Model}, nil
	}
	return fn(ctx, n, req)
}

type resp struct {
	status int
	header http.Header
	body   []byte
}

func (r resp) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.body, &m); err != nil {
		t.Fatalf("decode %q: %v", r.body, err)
	}
	return m
}

func (r resp) code(t *testing.T) string {
	t.Helper()
	c, _ := r.json(t)["code"].(string)
	return c
}

func (e *env) do(method, path, token string, body any, hdr map[string]string) resp {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		switch b := body.(type) {
		case string:
			rd = strings.NewReader(b)
		default:
			j, _ := json.Marshal(b)
			rd = bytes.NewReader(j)
		}
	}
	req, err := http.NewRequest(method, e.api.URL+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(res.Body)
	return resp{status: res.StatusCode, header: res.Header, body: b}
}

func (e *env) tok(sub string) string { return e.is.token(e.t, sub, nil) }

func (e *env) createProject(token, slug, name string) map[string]any {
	e.t.Helper()
	r := e.do("POST", "/v1/projects", token, map[string]any{"slug": slug, "name": name, "environment": "non_prod"}, nil)
	if r.status != 201 {
		e.t.Fatalf("create project %s: %d %s", slug, r.status, r.body)
	}
	return r.json(e.t)
}

func want(t *testing.T, r resp, status int, code string) {
	t.Helper()
	if r.status != status {
		t.Errorf("status = %d, muốn %d; body: %s", r.status, status, r.body)
		return
	}
	if code != "" {
		if ct := r.header.Get("Content-Type"); ct != "application/problem+json" {
			t.Errorf("Content-Type = %q", ct)
		}
		if got := r.code(t); got != code {
			t.Errorf("code = %q, muốn %q", got, code)
		}
	}
}

func TestAPI(t *testing.T) {
	e := setup(t)
	adminA, adminB, viewerA := e.tok("admin-a"), e.tok("admin-b"), e.tok("viewer-a")

	t.Run("authentication", func(t *testing.T) {
		otherKey, _ := rsa.GenerateKey(rand.Reader, 2048)
		forger, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256,
			Key: jose.JSONWebKey{Key: otherKey, KeyID: "k1"}}, (&jose.SignerOptions{}).WithType("JWT"))
		valid := map[string]any{"iss": e.is.srv.URL, "sub": "admin-a", "aud": audience,
			"exp": time.Now().Add(time.Hour).Unix()}

		cases := map[string]string{
			"no token":       "",
			"garbage":        "not-a-jwt",
			"forged key":     e.is.sign(t, forger, valid),
			"wrong audience": e.is.token(t, "admin-a", map[string]any{"aud": "some-other-service"}),
			"expired":        e.is.token(t, "admin-a", map[string]any{"exp": time.Now().Add(-time.Minute).Unix()}),
			"wrong issuer":   e.is.token(t, "admin-a", map[string]any{"iss": "https://evil.example"}),
		}
		for name, tok := range cases {
			r := e.do("GET", "/v1/organization", tok, nil, nil)
			if r.status != 401 || r.header.Get("WWW-Authenticate") == "" {
				t.Errorf("%s: status=%d www-authenticate=%q body=%s", name, r.status, r.header.Get("WWW-Authenticate"), r.body)
			}
		}
		// alg=none
		none := "eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0." + strings.Split(adminA, ".")[1] + "."
		want(t, e.do("GET", "/v1/organization", none, nil, nil), 401, "unauthenticated")
	})

	t.Run("forged tokens do not hammer the idp", func(t *testing.T) {
		before := e.is.jwksHits.Load()
		for i := 0; i < 25; i++ {
			k, _ := rsa.GenerateKey(rand.Reader, 1024) //nolint:gosec // khoá giả mạo trong test
			for _, kid := range []string{"k1", "made-up-" + uuid.NewString()} {
				s, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256,
					Key: jose.JSONWebKey{Key: k, KeyID: kid}}, (&jose.SignerOptions{}).WithType("JWT"))
				tok := e.is.sign(t, s, map[string]any{"iss": e.is.srv.URL, "sub": "admin-a", "aud": audience,
					"exp": time.Now().Add(time.Hour).Unix()})
				want(t, e.do("GET", "/v1/organization", tok, nil, nil), 401, "unauthenticated")
			}
		}
		if n := e.is.jwksHits.Load() - before; n > 1 {
			t.Errorf("50 token giả mạo gây %d lần tải JWKS, muốn tối đa 1", n)
		}
		// Token Keycloak không phải access token bị từ chối dù chữ ký và aud đúng.
		want(t, e.do("GET", "/v1/organization", e.is.token(t, "admin-a", map[string]any{"typ": "ID"}), nil, nil), 401, "unauthenticated")
	})

	t.Run("tenant resolution", func(t *testing.T) {
		// Token hợp lệ nhưng user chưa thuộc org nào (được tạo ở lần đăng nhập đầu).
		want(t, e.do("GET", "/v1/organization", e.tok("stranger"), nil, nil), 403, "organization_required")
		want(t, e.do("GET", "/v1/organization", e.tok("disabled"), nil, nil), 403, "user_disabled")

		// Thuộc hai org, token không chỉ rõ: từ chối thay vì đoán.
		want(t, e.do("GET", "/v1/organization", e.tok("multi"), nil, nil), 403, "organization_required")
		for org, slug := range map[uuid.UUID]string{e.orgA: "org-a", e.orgB: "org-b"} {
			r := e.do("GET", "/v1/organization", e.is.token(t, "multi", map[string]any{"org_id": org.String()}), nil, nil)
			if r.status != 200 || r.json(t)["slug"] != slug {
				t.Errorf("multi với org_id=%s: %d %s", slug, r.status, r.body)
			}
		}
		// Claim org chỉ là gợi ý: tự khai một org mình không thuộc thì bị từ chối.
		forged := e.is.token(t, "admin-a", map[string]any{"org_id": e.orgB.String()})
		want(t, e.do("GET", "/v1/organization", forged, nil, nil), 403, "organization_required")
		want(t, e.do("GET", "/v1/organization", e.is.token(t, "admin-a", map[string]any{"org_id": "x"}), nil, nil), 403, "organization_required")
	})

	t.Run("organization", func(t *testing.T) {
		r := e.do("GET", "/v1/organization", adminA, nil, nil)
		want(t, r, 200, "")
		if r.json(t)["id"] != e.orgA.String() {
			t.Errorf("admin-a thấy org %v", r.json(t)["id"])
		}
		etag := r.header.Get("ETag")
		if etag == "" {
			t.Fatal("thiếu ETag")
		}
		r = e.do("PATCH", "/v1/organization", adminA, map[string]any{"name": "Org A v2"}, map[string]string{"If-Match": etag})
		want(t, r, 200, "")
		if r.json(t)["name"] != "Org A v2" || r.header.Get("ETag") == etag {
			t.Errorf("sau PATCH: %s etag=%s", r.body, r.header.Get("ETag"))
		}
		// ETag cũ không còn khớp.
		want(t, e.do("PATCH", "/v1/organization", adminA, map[string]any{"name": "x"}, map[string]string{"If-Match": etag}), 412, "precondition_failed")
		want(t, e.do("PATCH", "/v1/organization", adminA, map[string]any{"name": "x"}, map[string]string{"If-Match": "junk"}), 412, "precondition_failed")
		want(t, e.do("PATCH", "/v1/organization", adminA, map[string]any{"name": "  "}, nil), 422, "validation_failed")
		want(t, e.do("PATCH", "/v1/organization", adminA, map[string]any{"settings": []int{1}}, nil), 400, "invalid_json")
		want(t, e.do("PATCH", "/v1/organization", adminA, "{not json", nil), 400, "invalid_json")
		// Org B không bị ảnh hưởng.
		if n := e.do("GET", "/v1/organization", adminB, nil, nil).json(t)["name"]; n != "Org B" {
			t.Errorf("tên org B = %v", n)
		}
	})

	t.Run("authorization", func(t *testing.T) {
		p := e.createProject(adminA, "authz-p", "Authz")
		id := p["id"].(string)
		want(t, e.do("GET", "/v1/projects/"+id, viewerA, nil, nil), 200, "")
		want(t, e.do("GET", "/v1/projects", viewerA, nil, nil), 200, "")
		want(t, e.do("POST", "/v1/projects", viewerA, map[string]any{"slug": "nope", "name": "n", "environment": "prod"}, nil), 403, "permission_denied")
		want(t, e.do("PATCH", "/v1/projects/"+id, viewerA, map[string]any{"name": "n"}, nil), 403, "permission_denied")
		want(t, e.do("DELETE", "/v1/projects/"+id, viewerA, nil, nil), 403, "permission_denied")
		want(t, e.do("PATCH", "/v1/organization", viewerA, map[string]any{"name": "n"}, nil), 403, "permission_denied")
		// Thành viên không có role nào: không có quyền gì.
		want(t, e.do("GET", "/v1/organization", e.tok("norole-a"), nil, nil), 403, "permission_denied")
		// Không được đọc project thì nó "không tồn tại": không dò được id nào có thật.
		want(t, e.do("GET", "/v1/projects/"+id, e.tok("norole-a"), nil, nil), 404, "not_found")
		want(t, e.do("DELETE", "/v1/projects/"+id, e.tok("norole-a"), nil, nil), 404, "not_found")
	})

	t.Run("projects", func(t *testing.T) {
		r := e.do("POST", "/v1/projects", adminA, map[string]any{"slug": "alpha", "name": "Alpha", "environment": "prod", "settings": map[string]any{"k": 1}}, nil)
		want(t, r, 201, "")
		p := r.json(t)
		id := p["id"].(string)
		if p["environment"] != "prod" || p["archived_at"] != nil || r.header.Get("ETag") == "" ||
			r.header.Get("Location") != "/v1/projects/"+id {
			t.Errorf("project mới: %s headers=%v", r.body, r.header)
		}
		if ts, _ := p["created_at"].(string); !strings.HasSuffix(ts, "Z") {
			t.Errorf("created_at không ở UTC: %q", ts)
		}
		if _, leaked := p["org_id"]; leaked {
			t.Error("phản hồi lộ org_id")
		}

		want(t, e.do("POST", "/v1/projects", adminA, map[string]any{"slug": "alpha", "name": "Dup", "environment": "prod"}, nil), 409, "slug_taken")
		for _, bad := range []map[string]any{
			{"slug": "Bad_Slug", "name": "x", "environment": "prod"},
			{"slug": "ok-slug", "name": "", "environment": "prod"},
			{"slug": "ok-slug", "name": "x", "environment": "staging"},
			{"slug": "ok-slug", "name": "x"},
		} {
			want(t, e.do("POST", "/v1/projects", adminA, bad, nil), 422, "validation_failed")
		}

		// Org khác dùng lại cùng slug được: slug chỉ duy nhất trong một org.
		want(t, e.do("POST", "/v1/projects", adminB, map[string]any{"slug": "alpha", "name": "B Alpha", "environment": "prod"}, nil), 201, "")

		etag := r.header.Get("ETag")
		r = e.do("PATCH", "/v1/projects/"+id, adminA, map[string]any{"name": "Alpha 2", "environment": "non_prod"}, map[string]string{"If-Match": etag})
		want(t, r, 200, "")
		if g := r.json(t); g["name"] != "Alpha 2" || g["environment"] != "prod" {
			t.Errorf("PATCH đổi được environment hoặc không đổi tên: %s", r.body)
		}
		want(t, e.do("PATCH", "/v1/projects/"+id, adminA, map[string]any{"name": "z"}, map[string]string{"If-Match": etag}), 412, "precondition_failed")

		want(t, e.do("DELETE", "/v1/projects/"+id, adminA, nil, nil), 204, "")
		want(t, e.do("DELETE", "/v1/projects/"+id, adminA, nil, nil), 204, "") // lặp lại là no-op
		r = e.do("GET", "/v1/projects/"+id, adminA, nil, nil)
		want(t, r, 200, "")
		if r.json(t)["archived_at"] == nil {
			t.Error("project đã lưu trữ phải có archived_at")
		}
		want(t, e.do("PATCH", "/v1/projects/"+id, adminA, map[string]any{"name": "z"}, nil), 409, "project_archived")

		want(t, e.do("GET", "/v1/projects/"+uuid.NewString(), adminA, nil, nil), 404, "not_found")
		want(t, e.do("GET", "/v1/projects/not-a-uuid", adminA, nil, nil), 404, "not_found")
	})

	t.Run("cross tenant", func(t *testing.T) {
		p := e.createProject(adminA, "secret-a", "Secret A")
		id := p["id"].(string)

		// Mọi endpoint theo id: project của org A không tồn tại đối với org B.
		want(t, e.do("GET", "/v1/projects/"+id, adminB, nil, nil), 404, "not_found")
		want(t, e.do("PATCH", "/v1/projects/"+id, adminB, map[string]any{"name": "pwned"}, nil), 404, "not_found")
		want(t, e.do("DELETE", "/v1/projects/"+id, adminB, nil, nil), 404, "not_found")

		// Danh sách của org B không chứa dòng nào của org A.
		r := e.do("GET", "/v1/projects?limit=200", adminB, nil, nil)
		want(t, r, 200, "")
		if strings.Contains(string(r.body), "secret-a") || strings.Contains(string(r.body), id) {
			t.Errorf("danh sách của org B lộ project của org A: %s", r.body)
		}

		// Org A vẫn nguyên vẹn.
		r = e.do("GET", "/v1/projects/"+id, adminA, nil, nil)
		if g := r.json(t); r.status != 200 || g["name"] != "Secret A" || g["archived_at"] != nil {
			t.Errorf("project của org A bị thay đổi: %d %s", r.status, r.body)
		}

		// Organization: mỗi bên chỉ thấy và chỉ sửa được org của mình.
		want(t, e.do("PATCH", "/v1/organization", adminB, map[string]any{"name": "Org B v2"}, nil), 200, "")
		if n := e.do("GET", "/v1/organization", adminA, nil, nil).json(t)["name"]; n == "Org B v2" {
			t.Error("PATCH của org B sửa vào org A")
		}
	})

	t.Run("hostile input is rejected, not a 500", func(t *testing.T) {
		nul := "a\x00b"
		want(t, e.do("POST", "/v1/projects", adminA, map[string]any{"slug": "nul-name", "name": nul, "environment": "prod"}, nil), 422, "validation_failed")
		want(t, e.do("POST", "/v1/projects", adminA, map[string]any{"slug": "nul-set", "name": "x", "environment": "prod", "settings": map[string]any{"k": nul}}, nil), 422, "validation_failed")
		want(t, e.do("POST", "/v1/projects", adminA, map[string]any{"slug": "nul-key", "name": "x", "environment": "prod", "settings": map[string]any{nul: 1}}, nil), 422, "validation_failed")
		want(t, e.do("PATCH", "/v1/organization", adminA, map[string]any{"settings": map[string]any{"k": []any{nul}}}, nil), 422, "validation_failed")
		cur, _ := json.Marshal(map[string]any{"s": "name", "v": nul, "id": uuid.NewString()})
		want(t, e.do("GET", "/v1/projects?sort=name&cursor="+base64.RawURLEncoding.EncodeToString(cur), adminA, nil, nil), 400, "invalid_cursor")
		// Token có email rất dài: user vẫn được tạo, email bị cắt.
		long := e.is.token(t, "long-email", map[string]any{"email": strings.Repeat("x", 30000)})
		want(t, e.do("GET", "/v1/organization", long, nil, nil), 403, "organization_required")
		var n int
		if err := e.db.Admin.QueryRow(context.Background(),
			`SELECT length(email) FROM users WHERE idp_subject = 'long-email'`).Scan(&n); err != nil || n > 320 {
			t.Errorf("độ dài email lưu = %d (err=%v), muốn ≤ 320", n, err)
		}
	})

	t.Run("project-scoped bindings", func(t *testing.T) {
		ctx := context.Background()
		mine := e.createProject(adminA, "scoped-mine", "Mine")["id"].(string)
		other := e.createProject(adminA, "scoped-other", "Other")["id"].(string)
		for sub, role := range map[string]string{"scoped-viewer": "viewer", "scoped-admin": "project_admin"} {
			uid := ids.New()
			for _, q := range []struct {
				sql  string
				args []any
			}{
				{`INSERT INTO users (id, idp_issuer, idp_subject, email) VALUES ($1,$2,$3,'s@x')`, []any{uid, e.is.srv.URL, sub}},
				{`INSERT INTO memberships (org_id, user_id) VALUES ($1,$2)`, []any{e.orgA, uid}},
				{`INSERT INTO role_bindings (org_id, role_id, principal_type, principal_id, project_id)
				  SELECT $1, id, 'user', $2, $3 FROM roles WHERE org_id IS NULL AND name = $4`, []any{e.orgA, uid, mine, role}},
			} {
				if _, err := e.db.Admin.Exec(ctx, q.sql, q.args...); err != nil {
					t.Fatalf("seed: %v", err)
				}
			}
		}
		viewer, padmin := e.tok("scoped-viewer"), e.tok("scoped-admin")

		// Thấy đúng project được gắn, không thấy project khác trong cùng org.
		r := e.do("GET", "/v1/projects?limit=200", viewer, nil, nil)
		want(t, r, 200, "")
		data := r.json(t)["data"].([]any)
		if len(data) != 1 || data[0].(map[string]any)["id"] != mine {
			t.Errorf("scoped viewer thấy: %s", r.body)
		}
		want(t, e.do("GET", "/v1/projects/"+mine, viewer, nil, nil), 200, "")
		want(t, e.do("GET", "/v1/projects/"+other, viewer, nil, nil), 404, "not_found")
		// Thành viên có role đọc được thông tin organization, nhưng không sửa được.
		want(t, e.do("GET", "/v1/organization", viewer, nil, nil), 200, "")
		want(t, e.do("PATCH", "/v1/organization", viewer, map[string]any{"name": "n"}, nil), 403, "permission_denied")
		want(t, e.do("POST", "/v1/projects", viewer, map[string]any{"slug": "n-1", "name": "n", "environment": "prod"}, nil), 403, "permission_denied")
		want(t, e.do("PATCH", "/v1/projects/"+mine, viewer, map[string]any{"name": "n"}, nil), 403, "permission_denied")

		// project_admin theo project: sửa được project của mình, không đụng được project khác.
		want(t, e.do("PATCH", "/v1/projects/"+mine, padmin, map[string]any{"name": "Mine 2"}, nil), 200, "")
		want(t, e.do("PATCH", "/v1/projects/"+other, padmin, map[string]any{"name": "x"}, nil), 404, "not_found")
		want(t, e.do("POST", "/v1/projects", padmin, map[string]any{"slug": "n-2", "name": "n", "environment": "prod"}, nil), 403, "permission_denied")
		// Thành viên không có role nào vẫn không liệt kê được gì.
		want(t, e.do("GET", "/v1/projects", e.tok("norole-a"), nil, nil), 403, "permission_denied")
	})

	t.Run("idempotency key", func(t *testing.T) {
		body := map[string]any{"slug": "idem-1", "name": "Idem", "environment": "non_prod"}
		key := map[string]string{"Idempotency-Key": "k-" + uuid.NewString()}
		first := e.do("POST", "/v1/projects", adminA, body, key)
		want(t, first, 201, "")
		again := e.do("POST", "/v1/projects", adminA, body, key)
		want(t, again, 201, "")
		if string(again.body) != string(first.body) || again.header.Get("ETag") != first.header.Get("ETag") ||
			again.header.Get("Location") != first.header.Get("Location") || again.header.Get("Idempotent-Replayed") != "true" {
			t.Errorf("gửi lại không trả phản hồi ban đầu:\n%s\n%s\n%v", first.body, again.body, again.header)
		}
		// Cùng key, request khác.
		want(t, e.do("POST", "/v1/projects", adminA, map[string]any{"slug": "idem-2", "name": "Idem", "environment": "non_prod"}, key), 422, "idempotency_conflict")
		// Key thuộc về một principal: người khác trong cùng org dùng cùng chuỗi key là
		// một request độc lập, không nhận phản hồi của người trước.
		other := e.do("POST", "/v1/projects", e.tok("multi-a"), body, key)
		want(t, other, 409, "slug_taken")
		if other.header.Get("Idempotent-Replayed") != "" {
			t.Error("phát lại phản hồi của principal khác")
		}
		// Người không có quyền: bị từ chối trước khi nhận key, không để lại dòng nào,
		// và lời từ chối không trở thành câu trả lời cố định của key.
		noRoleKey := map[string]string{"Idempotency-Key": "nr-" + uuid.NewString()}
		for i := 0; i < 5; i++ {
			want(t, e.do("POST", "/v1/projects", e.tok("norole-a"), body, noRoleKey), 403, "permission_denied")
		}
		var stored int
		_ = e.db.Admin.QueryRow(context.Background(), `SELECT count(*) FROM idempotency_keys WHERE key = $1`, noRoleKey["Idempotency-Key"]).Scan(&stored)
		if stored != 0 {
			t.Errorf("request không có quyền để lại %d dòng idempotency", stored)
		}
		want(t, e.do("POST", "/v1/projects", adminA, body, map[string]string{"Idempotency-Key": "bad\xffkey"}), 400, "invalid_parameter")
		want(t, e.do("POST", "/v1/projects", adminA, body, map[string]string{"Idempotency-Key": "has space"}), 400, "invalid_parameter")
		// Không có key: lần hai là xung đột slug như thường.
		want(t, e.do("POST", "/v1/projects", adminA, body, nil), 409, "slug_taken")
		// Phản hồi lỗi 4xx cũng được phát lại y nguyên.
		k2 := map[string]string{"Idempotency-Key": "k-" + uuid.NewString()}
		want(t, e.do("POST", "/v1/projects", adminA, body, k2), 409, "slug_taken")
		want(t, e.do("POST", "/v1/projects", adminA, body, k2), 409, "slug_taken")
		// Key thuộc phạm vi org: org B dùng cùng key là một request độc lập.
		rb := e.do("POST", "/v1/projects", adminB, body, key)
		want(t, rb, 201, "")
		// Dọn để không ảnh hưởng test phân trang của org B.
		want(t, e.do("DELETE", "/v1/projects/"+rb.json(t)["id"].(string), adminB, nil, nil), 204, "")
		want(t, e.do("POST", "/v1/projects", adminA, body, map[string]string{"Idempotency-Key": strings.Repeat("k", 256)}), 400, "invalid_parameter")

		// Nhiều request đồng thời cùng key: đúng một project được tạo.
		ck := map[string]string{"Idempotency-Key": "c-" + uuid.NewString()}
		cbody := map[string]any{"slug": "idem-conc", "name": "C", "environment": "non_prod"}
		codes := make(chan int, 12)
		for i := 0; i < 12; i++ {
			go func() { codes <- e.do("POST", "/v1/projects", adminA, cbody, ck).status }()
		}
		created := 0
		for i := 0; i < 12; i++ {
			switch c := <-codes; c {
			case 201:
				created++ // lần chạy thật, hoặc phát lại sau khi hoàn tất
			case 409: // đang xử lý
			default:
				t.Errorf("mã không mong đợi %d", c)
			}
		}
		var n int
		if err := e.db.Admin.QueryRow(context.Background(), `SELECT count(*) FROM projects WHERE slug = 'idem-conc'`).Scan(&n); err != nil || n != 1 || created == 0 {
			t.Errorf("project tạo ra = %d (err=%v), số phản hồi 201 = %d", n, err, created)
		}
	})

	t.Run("audit trail", func(t *testing.T) {
		ctx := context.Background()
		// Ghi đồng thời trong một org: chuỗi vẫn phải tuyến tính.
		done := make(chan int, 16)
		for i := 0; i < 16; i++ {
			go func(i int) {
				done <- e.do("POST", "/v1/projects", adminA, map[string]any{"slug": fmt.Sprintf("audit-%d", i), "name": "A", "environment": "non_prod"}, nil).status
			}(i)
		}
		for i := 0; i < 16; i++ {
			if c := <-done; c != 201 {
				t.Errorf("tạo đồng thời: %d", c)
			}
		}

		counts := map[string]int{}
		rows, err := e.db.Admin.Query(ctx, `SELECT action || '/' || outcome, count(*) FROM audit_logs WHERE org_id = $1 GROUP BY 1`, e.orgA)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var k string
			var n int
			_ = rows.Scan(&k, &n)
			counts[k] = n
		}
		rows.Close()
		for _, k := range []string{"organization.update/success", "project.create/success", "project.update/success", "project.archive/success", "authz.deny/denied"} {
			if counts[k] == 0 {
				t.Errorf("thiếu audit %q; có: %v", k, counts)
			}
		}
		// Mỗi project đúng một bản ghi tạo; request thất bại (409, 422, 403) không để lại bản ghi tạo.
		var projects int
		_ = e.db.Admin.QueryRow(ctx, `SELECT count(*) FROM projects WHERE org_id = $1`, e.orgA).Scan(&projects)
		if counts["project.create/success"] != projects {
			t.Errorf("project.create = %d, số project = %d", counts["project.create/success"], projects)
		}
		// Bản ghi mang actor, và từ chối quyền mang quy tắc đã khớp.
		var withActor, withRule, leaked int
		_ = e.db.Admin.QueryRow(ctx, `SELECT count(*) FILTER (WHERE actor_type = 'user' AND actor_id IS NOT NULL AND ip IS NOT NULL),
			count(*) FILTER (WHERE action = 'authz.deny' AND details ? 'rule' AND details ? 'permission'),
			count(*) FILTER (WHERE details::text LIKE '%Bearer%' OR details::text LIKE '%eyJ%')
			FROM audit_logs WHERE org_id = $1`, e.orgA).Scan(&withActor, &withRule, &leaked)
		if withActor == 0 || withRule != counts["authz.deny/denied"] || leaked != 0 {
			t.Errorf("actor=%d rule=%d/%d leaked=%d", withActor, withRule, counts["authz.deny/denied"], leaked)
		}

		// Từ chối lặp lại giống hệt nhau được gộp: 30 lần chỉ thêm tối đa một bản ghi.
		var before, after int
		_ = e.db.Admin.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE org_id = $1 AND action = 'authz.deny'`, e.orgA).Scan(&before)
		for i := 0; i < 30; i++ {
			want(t, e.do("PATCH", "/v1/organization", viewerA, map[string]any{"name": "n"}, nil), 403, "permission_denied")
		}
		_ = e.db.Admin.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE org_id = $1 AND action = 'authz.deny'`, e.orgA).Scan(&after)
		if after-before > 1 {
			t.Errorf("30 lần từ chối giống nhau ghi %d bản audit, muốn tối đa 1", after-before)
		}
		// trace_id trong audit không do client đặt được.
		forged := "4bf92f3577b34da6a3ce929d0e0e4736"
		want(t, e.do("POST", "/v1/projects", adminA, map[string]any{"slug": "trace-forge", "name": "T", "environment": "non_prod"},
			map[string]string{"traceparent": "00-" + forged + "-00f067aa0ba902b7-01"}), 201, "")
		var usedForged int
		_ = e.db.Admin.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE trace_id = $1`, forged).Scan(&usedForged)
		if usedForged != 0 {
			t.Error("audit ghi trace_id do client gửi")
		}

		verify := func(tamper string) (int64, error) {
			tx, err := e.db.Admin.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if tamper != "" {
				tag, err := tx.Exec(ctx, tamper, e.orgA)
				if err != nil || tag.RowsAffected() == 0 {
					t.Fatalf("tamper %q: rows=%d err=%v", tamper, tag.RowsAffected(), err)
				}
			}
			return audit.VerifyChain(ctx, tx, e.orgA)
		}
		n, err := verify("")
		if err != nil || n < 20 {
			t.Fatalf("chuỗi nguyên vẹn phải qua: n=%d err=%v", n, err)
		}
		for name, sql := range map[string]string{
			"sửa details":     `UPDATE audit_logs SET details = '{"slug":"forged"}' WHERE id = (SELECT id FROM audit_logs WHERE org_id = $1 AND action = 'project.create' ORDER BY occurred_at LIMIT 1)`,
			"đổi actor":       `UPDATE audit_logs SET actor_id = gen_random_uuid() WHERE id = (SELECT id FROM audit_logs WHERE org_id = $1 ORDER BY occurred_at LIMIT 1 OFFSET 3)`,
			"xoá ở giữa":      `DELETE FROM audit_logs WHERE id = (SELECT id FROM audit_logs WHERE org_id = $1 ORDER BY occurred_at LIMIT 1 OFFSET 5)`,
			"xoá bản ghi đầu": `DELETE FROM audit_logs WHERE id = (SELECT id FROM audit_logs WHERE org_id = $1 ORDER BY occurred_at LIMIT 1)`,
			"đổi thời điểm":   `UPDATE audit_logs SET occurred_at = occurred_at + interval '1 second' WHERE id = (SELECT id FROM audit_logs WHERE org_id = $1 ORDER BY occurred_at DESC LIMIT 1)`,
			"đổi kết quả":     `UPDATE audit_logs SET outcome = 'success' WHERE id = (SELECT id FROM audit_logs WHERE org_id = $1 AND outcome = 'denied' LIMIT 1)`,
		} {
			if _, err := verify(sql); !errors.Is(err, audit.ErrChainBroken) {
				t.Errorf("%s: muốn ErrChainBroken, nhận %v", name, err)
			}
		}
	})

	// Chạy sau cùng trong các test của org A / B vì nó đình chỉ org B.
	t.Run("inactive organization is locked out", func(t *testing.T) {
		ctx := context.Background()
		for _, status := range []string{"suspended", "archived"} {
			if _, err := e.db.Admin.Exec(ctx, `UPDATE organizations SET status = $1 WHERE id = $2`, status, e.orgB); err != nil {
				t.Fatal(err)
			}
			want(t, e.do("GET", "/v1/organization", adminB, nil, nil), 403, "organization_inactive")
			want(t, e.do("POST", "/v1/projects", adminB, map[string]any{"slug": "x-" + status, "name": "x", "environment": "prod"}, nil), 403, "organization_inactive")
			// User thuộc nhiều org vẫn vào được org còn hoạt động, không vào được org bị đình chỉ.
			want(t, e.do("GET", "/v1/organization", e.is.token(t, "multi", map[string]any{"org_id": e.orgB.String()}), nil, nil), 403, "organization_inactive")
			want(t, e.do("GET", "/v1/organization", e.is.token(t, "multi", map[string]any{"org_id": e.orgA.String()}), nil, nil), 200, "")
		}
		// Tenant không tự gỡ đình chỉ được: app_api không có quyền sửa cột status.
		if _, err := e.db.Admin.Exec(ctx, `UPDATE organizations SET status = 'active' WHERE id = $1`, e.orgB); err != nil {
			t.Fatal(err)
		}
		want(t, e.do("GET", "/v1/organization", adminB, nil, nil), 200, "")
	})

	t.Run("bootstrap via admin path", func(t *testing.T) {
		ctx := context.Background()
		conn, err := e.db.Admin.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		in := adminops.BootstrapOrg{Slug: "org-c", Name: "Org C", AdminIssuer: e.is.srv.URL, AdminSubject: "founder-c"}
		orgC, err := adminops.CreateOrganization(ctx, conn.Conn(), in)
		if err != nil {
			t.Fatalf("bootstrap: %v", err)
		}
		// Quản trị viên vừa tạo đăng nhập được và có đủ quyền trong org mới.
		founder := e.tok("founder-c")
		r := e.do("GET", "/v1/organization", founder, nil, nil)
		if r.status != 200 || r.json(t)["id"] != orgC.String() {
			t.Fatalf("founder: %d %s", r.status, r.body)
		}
		e.createProject(founder, "first", "First")
		// Slug trùng bị từ chối và không để lại dữ liệu dở dang.
		if _, err := adminops.CreateOrganization(ctx, conn.Conn(), in); err == nil {
			t.Error("bootstrap trùng slug phải lỗi")
		}
		in.Slug = "Bad Slug"
		if _, err := adminops.CreateOrganization(ctx, conn.Conn(), in); err == nil {
			t.Error("bootstrap slug sai phải lỗi")
		}
	})

	t.Run("list pagination and sort", func(t *testing.T) {
		tokC := adminB // org B: đang có 1 project "alpha" từ test trước
		for _, s := range []string{"bravo", "charlie", "delta", "echo"} {
			e.createProject(tokC, s, strings.ToUpper(s[:1])+s[1:])
		}
		collect := func(query string) []string {
			var names []string
			cursor := ""
			for page := 0; page < 10; page++ {
				r := e.do("GET", "/v1/projects?"+query+"&cursor="+cursor, tokC, nil, nil)
				if r.status != 200 {
					t.Fatalf("list %s: %d %s", query, r.status, r.body)
				}
				m := r.json(t)
				data := m["data"].([]any)
				if len(data) > 2 {
					t.Fatalf("trang có %d dòng, limit=2", len(data))
				}
				for _, d := range data {
					names = append(names, d.(map[string]any)["slug"].(string))
				}
				next, ok := m["next_cursor"].(string)
				if !ok {
					if _, present := m["next_cursor"]; !present {
						t.Fatal("thiếu trường next_cursor")
					}
					return names
				}
				cursor = next
			}
			t.Fatal("phân trang không kết thúc")
			return nil
		}
		eq := func(got []string, want ...string) {
			t.Helper()
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("got %v, muốn %v", got, want)
			}
		}
		eq(collect("limit=2&sort=name"), "alpha", "bravo", "charlie", "delta", "echo")
		eq(collect("limit=2&sort=-name"), "echo", "delta", "charlie", "bravo", "alpha")
		eq(collect("limit=2&sort=created_at"), "alpha", "bravo", "charlie", "delta", "echo")
		eq(collect("limit=2"), "echo", "delta", "charlie", "bravo", "alpha") // mặc định -created_at
		eq(collect("limit=2&environment=prod"), "alpha")

		want(t, e.do("GET", "/v1/projects?limit=0", tokC, nil, nil), 400, "invalid_parameter")
		want(t, e.do("GET", "/v1/projects?limit=201", tokC, nil, nil), 400, "invalid_parameter")
		want(t, e.do("GET", "/v1/projects?sort=slug", tokC, nil, nil), 400, "invalid_parameter")
		want(t, e.do("GET", "/v1/projects?environment=dev", tokC, nil, nil), 400, "invalid_parameter")
		want(t, e.do("GET", "/v1/projects?cursor=%%%", tokC, nil, nil), 400, "invalid_parameter")
		want(t, e.do("GET", "/v1/projects?cursor=bm9wZQ", tokC, nil, nil), 400, "invalid_cursor")
		// Cursor của cách sắp xếp này không dùng được cho cách sắp xếp khác.
		r := e.do("GET", "/v1/projects?limit=2&sort=name", tokC, nil, nil)
		c := r.json(t)["next_cursor"].(string)
		want(t, e.do("GET", "/v1/projects?sort=-name&cursor="+c, tokC, nil, nil), 400, "invalid_cursor")
	})
}
