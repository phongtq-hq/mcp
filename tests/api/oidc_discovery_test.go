package api_test

import (
	"context"
	"strings"
	"testing"

	"github.com/hqgroups/mcp/internal/platform/identity"
)

// TestOIDCDiscoveryOverride: process đọc khoá của IdP qua một địa chỉ khác với issuer
// ghi trong token (docker compose: token mang http://localhost:8180, process thấy IdP ở
// http://keycloak:8180). Token vẫn phải mang đúng issuer đã cấu hình; và thiết lập này
// chỉ được nhận ở môi trường local.
func TestOIDCDiscoveryOverride(t *testing.T) {
	ctx := context.Background()
	is := newIssuer(t)
	// Cùng một IdP, tới bằng tên khác: metadata của nó vẫn tự khai issuer là is.srv.URL.
	other := strings.Replace(is.srv.URL, "127.0.0.1", "localhost", 1)
	if other == is.srv.URL {
		t.Skip("IdP giả không nghe trên 127.0.0.1")
	}
	tok := is.token(t, "someone", nil)

	// Không có ghi đè: discovery qua tên khác bị từ chối (issuer trong metadata không khớp).
	if _, err := identity.NewOIDCVerifier(ctx, other, audience, "org_id"); err == nil {
		t.Fatal("discovery với issuer không khớp metadata được chấp nhận")
	}
	// Có ghi đè: khoá lấy qua tên khác, token của đúng issuer được nhận.
	v, err := identity.NewOIDCVerifierVia(ctx, is.srv.URL, other, audience, "org_id")
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	if _, err := v.Verify(ctx, tok); err != nil {
		t.Fatalf("token của đúng issuer bị từ chối: %v", err)
	}
	// Ghi đè KHÔNG nới kiểm tra `iss`: cấu hình issuer khác thì token này bị từ chối.
	strict, err := identity.NewOIDCVerifierVia(ctx, "https://idp.example.test/realms/hq", other, audience, "org_id")
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	if _, err := strict.Verify(ctx, tok); err == nil {
		t.Fatal("token mang issuer khác với cấu hình được chấp nhận")
	}

	env := func(hqEnv, override string) func(string) string {
		return func(k string) string {
			switch k {
			case "HQ_ENV":
				return hqEnv
			case "HQ_OIDC_DISCOVERY_URL":
				return override
			}
			return ""
		}
	}
	const via = "http://keycloak:8180/realms/hq"
	if got, err := identity.DiscoveryOverride(env("local", via)); err != nil || got != via {
		t.Fatalf("local: %q, %v", got, err)
	}
	// HQ_ENV không đặt (mặc định của platform là local) KHÔNG đủ: phải đặt tường minh.
	for _, e := range []string{"", "staging", "production", "Local", "local "} {
		if _, err := identity.DiscoveryOverride(env(e, via)); err == nil {
			t.Errorf("HQ_ENV=%q: HQ_OIDC_DISCOVERY_URL được chấp nhận", e)
		}
		if got, err := identity.DiscoveryOverride(env(e, "")); err != nil || got != "" {
			t.Errorf("HQ_ENV=%q không đặt biến: %q, %v", e, got, err)
		}
	}
}
