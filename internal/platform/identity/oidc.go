package identity

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// Claims là phần của token mà platform dùng. Platform chỉ lấy danh tính từ IdP;
// membership và quyền nằm trong database của platform (ADR-0011).
type Claims struct {
	Issuer  string
	Subject string
	Email   string
	Name    string
	OrgHint string // giá trị claim org (tuỳ chọn), chưa được kiểm chứng với membership
}

// TokenVerifier kiểm tra một bearer token và trả về claims đã xác thực.
type TokenVerifier interface {
	Verify(ctx context.Context, rawToken string) (Claims, error)
}

// OIDCVerifier kiểm tra JWT theo OIDC chuẩn: chữ ký (JWKS), iss, aud, exp.
type OIDCVerifier struct {
	verifier *oidc.IDTokenVerifier
	orgClaim string
}

// NewOIDCVerifier chạy discovery trên issuer. audience là giá trị `aud` mà token
// phải mang: token phát hành cho dịch vụ khác bị từ chối (RFC 8707).
func NewOIDCVerifier(ctx context.Context, issuer, audience, orgClaim string) (*OIDCVerifier, error) {
	return newOIDCVerifier(ctx, issuer, "", audience, orgClaim, defaultJWKSRefresh)
}

// NewOIDCVerifierVia như NewOIDCVerifier, nhưng đọc metadata và khoá của IdP từ
// discoveryURL thay vì từ chính issuer. Dùng khi process không tới được IdP bằng địa chỉ
// mà IdP ghi vào token: trong docker compose, token mang `iss` http://localhost:8180/…
// còn process thấy IdP ở http://keycloak:8180/…. Token vẫn phải mang đúng `iss` = issuer;
// chỉ NƠI LẤY KHOÁ đổi. Vì ai trả lời ở discoveryURL thì quyết định khoá nào được tin,
// người gọi chỉ được dùng hàm này ở môi trường local (cmd/api và cmd/gateway kiểm).
func NewOIDCVerifierVia(ctx context.Context, issuer, discoveryURL, audience, orgClaim string) (*OIDCVerifier, error) {
	return newOIDCVerifier(ctx, issuer, discoveryURL, audience, orgClaim, defaultJWKSRefresh)
}

// DiscoveryOverride đọc HQ_OIDC_DISCOVERY_URL. Biến này chỉ được nhận khi HQ_ENV được ĐẶT
// TƯỜNG MINH là "local": giá trị mặc định của HQ_ENV (cũng là local) không đủ, vì một
// manifest staging chép từ compose mà quên HQ_ENV sẽ lặng lẽ lấy khoá của IdP qua một
// địa chỉ nội bộ không TLS. Ở mọi trường hợp khác, đặt biến này là lỗi cấu hình và
// process không khởi động (không lặng lẽ bỏ qua một thiết lập về nơi lấy khoá).
func DiscoveryOverride(getenv func(string) string) (string, error) {
	v := getenv("HQ_OIDC_DISCOVERY_URL")
	if v != "" && getenv("HQ_ENV") != "local" {
		return "", fmt.Errorf("oidc: HQ_OIDC_DISCOVERY_URL chỉ dùng được khi HQ_ENV được đặt tường minh là local")
	}
	return v, nil
}

func newOIDCVerifier(ctx context.Context, issuer, discoveryURL, audience, orgClaim string, jwksRefresh time.Duration) (*OIDCVerifier, error) {
	if issuer == "" || audience == "" {
		return nil, fmt.Errorf("oidc: issuer and audience are required")
	}
	hc := &http.Client{Timeout: 10 * time.Second}
	dctx, from := oidc.ClientContext(ctx, hc), issuer
	if discoveryURL != "" && discoveryURL != issuer {
		// Metadata ở discoveryURL tự khai một issuer khác (địa chỉ nội bộ của IdP): bỏ
		// kiểm tra "issuer trong metadata = URL discovery", giữ kiểm tra `iss` của token.
		dctx, from = oidc.InsecureIssuerURLContext(dctx, issuer), discoveryURL
	}
	p, err := oidc.NewProvider(dctx, from)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery: %w", err)
	}
	var meta struct {
		JWKSURL string `json:"jwks_uri"`
	}
	if err := p.Claims(&meta); err != nil || meta.JWKSURL == "" {
		return nil, fmt.Errorf("oidc discovery: missing jwks_uri")
	}
	algs := make([]string, len(signingAlgs))
	for i, a := range signingAlgs {
		algs[i] = string(a)
	}
	return &OIDCVerifier{
		verifier: oidc.NewVerifier(issuer, newKeySet(meta.JWKSURL, hc, jwksRefresh),
			&oidc.Config{ClientID: audience, SupportedSigningAlgs: algs}),
		orgClaim: orgClaim,
	}, nil
}

// ErrIdPUnavailable báo không kiểm tra được token vì không lấy được khoá của IdP.
var ErrIdPUnavailable = errors.New("identity: identity provider unavailable")

const (
	maxEmailLen = 320
	maxNameLen  = 200
)

// Verify hiện thực TokenVerifier.
func (v *OIDCVerifier) Verify(ctx context.Context, rawToken string) (Claims, error) {
	tok, err := v.verifier.Verify(ctx, rawToken)
	if err != nil {
		// go-oidc không bọc lỗi của KeySet bằng %w nên phải nhận diện theo thông báo.
		if strings.Contains(err.Error(), errKeysUnavailable.Error()) {
			return Claims{}, ErrIdPUnavailable
		}
		return Claims{}, err
	}
	var raw map[string]any
	if err := tok.Claims(&raw); err != nil {
		return Claims{}, err
	}
	str := func(k string) string { s, _ := raw[k].(string); return s }
	// Keycloak đánh dấu loại token trong claim `typ`. Chỉ access token được dùng
	// làm bearer: ID token và refresh token của một client có cùng `aud` bị từ chối.
	if typ := str("typ"); typ == "ID" || typ == "Refresh" || typ == "Offline" {
		return Claims{}, fmt.Errorf("oidc: token type %q is not an access token", typ)
	}
	c := Claims{
		Issuer: tok.Issuer, Subject: tok.Subject,
		Email: truncate(str("email"), maxEmailLen), Name: truncate(str("name"), maxNameLen),
	}
	if v.orgClaim != "" {
		c.OrgHint = str(v.orgClaim)
	}
	if c.Subject == "" || len(c.Subject) > 255 {
		return Claims{}, fmt.Errorf("oidc: token subject missing or too long")
	}
	return c, nil
}

// truncate cắt chuỗi về tối đa n rune: claim do IdP cấp nhưng không nên tin về độ dài.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
