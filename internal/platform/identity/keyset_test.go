package identity

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

func signWith(t *testing.T, key *rsa.PrivateKey, kid string) string {
	t.Helper()
	s, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: kid}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	obj, err := s.Sign([]byte(`{"sub":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := obj.CompactSerialize()
	return tok
}

func TestKeySetRotationAndRefreshLimit(t *testing.T) {
	k1, _ := rsa.GenerateKey(rand.Reader, 2048)
	k2, _ := rsa.GenerateKey(rand.Reader, 2048)
	var served atomic.Pointer[jose.JSONWebKeySet]
	var hits atomic.Int64
	var down atomic.Bool
	served.Store(&jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &k1.PublicKey, KeyID: "k1", Use: "sig"}}})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		if down.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(served.Load())
	}))
	defer srv.Close()

	const interval = 150 * time.Millisecond
	ks := newKeySet(srv.URL, srv.Client(), interval)
	ctx := context.Background()

	if _, err := ks.VerifySignature(ctx, signWith(t, k1, "k1")); err != nil {
		t.Fatalf("token hợp lệ: %v", err)
	}
	// Khoá đã biết, chữ ký sai: không tải lại. kid lạ: không tải lại trong khoảng giới hạn.
	for i := 0; i < 20; i++ {
		if _, err := ks.VerifySignature(ctx, signWith(t, k2, "k1")); err == nil {
			t.Fatal("chữ ký sai được chấp nhận")
		}
		if _, err := ks.VerifySignature(ctx, signWith(t, k2, "k2")); err == nil {
			t.Fatal("khoá chưa công bố được chấp nhận")
		}
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("JWKS được tải %d lần, muốn 1", n)
	}

	// IdP xoay khoá: sau khoảng giới hạn, token ký bằng khoá mới được nhận.
	served.Store(&jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
		{Key: &k1.PublicKey, KeyID: "k1", Use: "sig"}, {Key: &k2.PublicKey, KeyID: "k2", Use: "sig"}}})
	time.Sleep(interval + 50*time.Millisecond)
	if _, err := ks.VerifySignature(ctx, signWith(t, k2, "k2")); err != nil {
		t.Errorf("sau khi xoay khoá: %v", err)
	}

	// IdP lỗi khi đã có khoá: khoá cũ vẫn dùng được.
	down.Store(true)
	time.Sleep(interval + 50*time.Millisecond)
	if _, err := ks.VerifySignature(ctx, signWith(t, k1, "k1")); err != nil {
		t.Errorf("IdP lỗi nhưng khoá đã cache phải còn dùng được: %v", err)
	}

	// IdP lỗi khi chưa có khoá nào: lỗi hạ tầng, không phải "token sai".
	cold := newKeySet(srv.URL, srv.Client(), interval)
	before := hits.Load()
	for i := 0; i < 5; i++ {
		if _, err := cold.VerifySignature(ctx, signWith(t, k1, "k1")); !errors.Is(err, errKeysUnavailable) {
			t.Errorf("muốn errKeysUnavailable, nhận %v", err)
		}
	}
	if n := hits.Load() - before; n != 1 {
		t.Errorf("IdP đang lỗi bị gọi %d lần trong một khoảng giới hạn, muốn 1", n)
	}
}

func TestKeySetRejectsUnsignedAndSymmetric(t *testing.T) {
	ks := newKeySet("http://127.0.0.1:1/unused", http.DefaultClient, time.Hour)
	for name, tok := range map[string]string{
		"alg none": "eyJhbGciOiJub25lIn0.eyJzdWIiOiJ4In0.",
		"hs256":    "eyJhbGciOiJIUzI1NiIsImtpZCI6ImsxIn0.eyJzdWIiOiJ4In0.c2ln",
		"garbage":  "abc",
	} {
		if _, err := ks.VerifySignature(context.Background(), tok); err == nil || errors.Is(err, errKeysUnavailable) {
			t.Errorf("%s: err = %v, muốn bị từ chối vì token sai dạng", name, err)
		}
	}
}

// Process vừa khởi động, chưa có khoá nào: nhiều request hợp lệ tới cùng lúc đều phải qua,
// và JWKS chỉ được tải một lần. (Trước đây request tới trong lúc request đầu đang tải bị từ
// chối 401 "khoá lạ", và giao diện đăng xuất người dùng ngay sau khi đăng nhập.)
func TestKeySetColdStartConcurrentRequests(t *testing.T) {
	k1, _ := rsa.GenerateKey(rand.Reader, 2048)
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		time.Sleep(100 * time.Millisecond) // IdP trả lời chậm: các request khác tới trong lúc này
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &k1.PublicKey, KeyID: "k1", Use: "sig"}}})
	}))
	defer srv.Close()

	ks := newKeySet(srv.URL, srv.Client(), time.Hour)
	token := signWith(t, k1, "k1")
	const n = 16
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = ks.VerifySignature(context.Background(), token)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("request %d với token hợp lệ bị từ chối lúc khởi động nguội: %v", i, err)
		}
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("JWKS được tải %d lần, muốn 1", got)
	}
}

// IdP lỗi lúc khởi động nguội: request đang chờ nhận lỗi hạ tầng, không phải "token sai".
func TestKeySetColdStartIdPDown(t *testing.T) {
	k1, _ := rsa.GenerateKey(rand.Reader, 2048)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	ks := newKeySet(srv.URL, srv.Client(), time.Hour)
	token := signWith(t, k1, "k1")
	errs := make([]error, 8)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = ks.VerifySignature(context.Background(), token)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if !errors.Is(err, errKeysUnavailable) {
			t.Errorf("request %d: muốn errKeysUnavailable, nhận %v", i, err)
		}
	}
}
