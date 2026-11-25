package signedctx_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/hqgroups/mcp/internal/shared/signedctx"
)

func newSigner(t *testing.T, hexSeed string) *signedctx.Signer {
	t.Helper()
	priv, err := signedctx.ParseSigningKey(hexSeed)
	if err != nil {
		t.Fatal(err)
	}
	s, err := signedctx.NewSigner(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSignVerify(t *testing.T) {
	s := newSigner(t, strings.Repeat("ab", 32))
	other := newSigner(t, strings.Repeat("cd", 32))
	v := s.Verifier()
	now := time.Unix(1_800_000_000, 0)
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call"}`)
	c := signedctx.Context{Kind: signedctx.KindCall, Audience: "docker", OrgID: uuid.New(), ProjectID: uuid.New(), Initiator: "user:" + uuid.NewString()}
	tok := s.SignBody(c, body, now)

	got, err := v.Verify(tok, "docker", body, now.Add(time.Minute))
	if err != nil || got.OrgID != c.OrgID || got.ProjectID != c.ProjectID || got.Initiator != c.Initiator || got.ID == "" {
		t.Fatalf("verify: %+v %v", got, err)
	}
	payload, sig, _ := strings.Cut(tok, ".")
	for name, tc := range map[string]struct {
		value, aud string
		body       []byte
		at         time.Time
		verifier   *signedctx.Verifier
	}{
		"đích khác":             {tok, "kubernetes", body, now, v},
		"body khác":             {tok, "docker", []byte(`{"method":"tools/call","x":1}`), now, v},
		"body rỗng":             {tok, "docker", nil, now, v},
		"hết hạn":               {tok, "docker", body, now.Add(signedctx.DefaultTTL + time.Second), v},
		"khoá khác":             {tok, "docker", body, now, other.Verifier()},
		"sửa payload":           {payload + "x." + sig, "docker", body, now, v},
		"bỏ chữ ký":             {payload + ".", "docker", body, now, v},
		"rỗng":                  {"", "docker", body, now, v},
		"phát hành ở tương lai": {s.SignBody(c, body, now.Add(time.Hour)), "docker", body, now, v},
	} {
		if _, err := tc.verifier.Verify(tc.value, tc.aud, tc.body, tc.at); err == nil {
			t.Errorf("%s: phải bị từ chối", name)
		}
	}
	// Thiếu org / project / initiator / hash body không bao giờ hợp lệ.
	for _, bad := range []signedctx.Context{
		{Kind: signedctx.KindCall, Audience: "docker", ProjectID: uuid.New(), Initiator: "u"},
		{Kind: signedctx.KindCall, Audience: "docker", OrgID: uuid.New(), Initiator: "u"},
		{Kind: signedctx.KindCall, Audience: "docker", OrgID: uuid.New(), ProjectID: uuid.New()},
		{Kind: "", Audience: "docker", OrgID: uuid.New(), ProjectID: uuid.New(), Initiator: "u"},
		{Kind: "admin", Audience: "docker", OrgID: uuid.New(), ProjectID: uuid.New(), Initiator: "u"},
	} {
		if _, err := v.Verify(s.SignBody(bad, body, now), "docker", body, now); err == nil {
			t.Errorf("ngữ cảnh thiếu trường phải bị từ chối: %+v", bad)
		}
	}
	if _, err := v.Verify(s.Sign(c, now), "docker", body, now); err == nil {
		t.Error("ngữ cảnh không mang hash body phải bị từ chối")
	}
	// Ngữ cảnh discovery không gắn tenant, nhưng vẫn cần đúng đích và đúng body.
	disc := s.SignBody(signedctx.Context{Kind: signedctx.KindDiscovery, Audience: "docker"}, body, now)
	if _, err := v.Verify(disc, "docker", body, now); err != nil {
		t.Errorf("discovery: %v", err)
	}
	if _, err := v.Verify(disc, "other", body, now); err == nil {
		t.Error("discovery sai đích phải bị từ chối")
	}
}

func TestKeys(t *testing.T) {
	for _, k := range []string{"", "abcd", strings.Repeat("zz", 32), strings.Repeat("ab", 31), strings.Repeat("ab", 33), strings.Repeat("ab", 64)} {
		if _, err := signedctx.ParseSigningKey(k); err == nil {
			t.Errorf("khoá ký %q phải bị từ chối", k)
		}
		if _, err := signedctx.ParseVerifyKey(k); err == nil {
			t.Errorf("khoá kiểm %q phải bị từ chối", k)
		}
	}
	for _, k := range []string{"", "abcd", strings.Repeat("zz", 32), strings.Repeat("ab", 31)} {
		if _, err := signedctx.ParseKey(k); err == nil {
			t.Errorf("khoá HMAC %q phải bị từ chối", k)
		}
	}
	seed, pub, err := signedctx.GenerateSigningKey()
	if err != nil {
		t.Fatal(err)
	}
	s := newSigner(t, seed)
	if s.PublicKeyHex() != pub {
		t.Fatal("khoá công khai không khớp seed")
	}
	pk, err := signedctx.ParseVerifyKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	v, _ := signedctx.NewVerifier(pk)
	now := time.Now()
	c := signedctx.Context{Kind: signedctx.KindDiscovery, Audience: "x"}
	if _, err := v.Verify(s.SignBody(c, nil, now), "x", nil, now); err != nil {
		t.Fatalf("khoá công khai phân tích từ hex không kiểm được chữ ký: %v", err)
	}
}

func TestReplayGuard(t *testing.T) {
	s := newSigner(t, strings.Repeat("ab", 32))
	v := s.Verifier()
	now := time.Now()
	g := signedctx.NewReplayGuard(2)
	ctxOf := func(at time.Time) signedctx.Context {
		c, err := v.Verify(s.SignBody(signedctx.Context{Kind: signedctx.KindDiscovery, Audience: "x"}, nil, at), "x", nil, at)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	a := ctxOf(now)
	if !g.Accept(a, now) {
		t.Fatal("lần đầu phải được nhận")
	}
	if g.Accept(a, now) {
		t.Fatal("jti dùng lần hai phải bị từ chối")
	}
	if !g.Accept(ctxOf(now), now) {
		t.Fatal("jti mới phải được nhận")
	}
	if g.Accept(ctxOf(now), now) {
		t.Fatal("bộ nhớ đầy jti còn hạn: phải từ chối (fail-closed)")
	}
	later := now.Add(signedctx.DefaultTTL + time.Second)
	if !g.Accept(ctxOf(later), later) {
		t.Fatal("jti hết hạn phải được dọn để nhận jti mới")
	}
}

// Transport ký đúng body mà nó gửi đi, và mỗi request một jti.
func TestTransportBindsBody(t *testing.T) {
	s := newSigner(t, strings.Repeat("ab", 32))
	v := s.Verifier()
	var ids []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c, err := v.Verify(r.Header.Get(signedctx.Header), "x", body, time.Now())
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		ids = append(ids, c.ID)
	}))
	defer srv.Close()
	hc := &http.Client{Transport: &signedctx.Transport{Signer: s, Context: signedctx.Context{Kind: signedctx.KindDiscovery, Audience: "x"}}}
	for _, body := range []string{`{"a":1}`, `{"a":2}`, ""} {
		resp, err := hc.Post(srv.URL, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("body %q: %d", body, resp.StatusCode)
		}
	}
	if len(ids) != 3 || ids[0] == ids[1] || ids[1] == ids[2] {
		t.Fatalf("mỗi request phải có jti riêng: %v", ids)
	}
}
