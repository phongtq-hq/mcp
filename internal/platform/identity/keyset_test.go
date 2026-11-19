package identity

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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
