package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// errKeysUnavailable báo không tải được JWKS từ IdP: lỗi hạ tầng, không phải token sai.
var errKeysUnavailable = errors.New("identity: idp signing keys unavailable")

var signingAlgs = []jose.SignatureAlgorithm{
	jose.RS256, jose.RS384, jose.RS512,
	jose.PS256, jose.PS384, jose.PS512,
	jose.ES256, jose.ES384, jose.ES512,
	jose.EdDSA,
}

const (
	maxJWKSBytes       = 1 << 20
	defaultJWKSRefresh = 30 * time.Second
)

// keySet là JWKS của IdP được cache trong bộ nhớ.
//
// JWKS chỉ được tải lại khi token mang `kid` chưa có trong cache (IdP xoay khoá),
// và không quá một lần mỗi minRefresh. Token sai chữ ký với `kid` đã biết, hoặc
// một loạt token giả mạo với `kid` bịa, không khiến process gọi ra IdP theo từng
// request (threat T10).
type keySet struct {
	url        string
	hc         *http.Client
	minRefresh time.Duration

	mu        sync.RWMutex
	keys      map[string]jose.JSONWebKey
	lastFetch time.Time

	fetching sync.Mutex // chỉ một lần tải tại một thời điểm; request khác không chờ, trừ khi chưa có khoá nào
}

func newKeySet(url string, hc *http.Client, minRefresh time.Duration) *keySet {
	return &keySet{url: url, hc: hc, minRefresh: minRefresh, keys: map[string]jose.JSONWebKey{}}
}

// VerifySignature hiện thực oidc.KeySet.
func (k *keySet) VerifySignature(ctx context.Context, jwt string) ([]byte, error) {
	jws, err := jose.ParseSigned(jwt, signingAlgs)
	if err != nil {
		return nil, fmt.Errorf("malformed jwt: %w", err)
	}
	if len(jws.Signatures) != 1 {
		return nil, errors.New("jwt must have exactly one signature")
	}
	kid := jws.Signatures[0].Header.KeyID

	key, ok := k.lookup(kid)
	if !ok {
		if err := k.refresh(ctx); err != nil {
			return nil, err
		}
		if key, ok = k.lookup(kid); !ok {
			return nil, errors.New("unknown signing key")
		}
	}
	return jws.Verify(key)
}

func (k *keySet) lookup(kid string) (jose.JSONWebKey, bool) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	key, ok := k.keys[kid]
	return key, ok
}

func (k *keySet) empty() bool {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return len(k.keys) == 0
}

// refresh tải lại JWKS nếu đã qua minRefresh kể từ lần thử trước. Trong khoảng
// đó, hoặc khi một lần tải khác đang chạy, nó trả về ngay với cache hiện có; riêng khi
// cache còn rỗng (process vừa khởi động) thì chờ lần tải đang chạy.
func (k *keySet) refresh(ctx context.Context) error {
	if !k.fetching.TryLock() {
		if !k.empty() {
			return nil
		}
		// Khởi động nguội: chưa có khoá nào và một request khác đang tải JWKS. Trả về ngay
		// ở đây sẽ từ chối một token HỢP LỆ là "khoá lạ" (401), và client đăng xuất người
		// dùng vừa đăng nhập. Chờ đúng lần tải đang chạy (thời gian của nó có trần: timeout
		// của HTTP client); không gọi thêm lần nào ra IdP, nên giới hạn của T10 không đổi.
		k.fetching.Lock()
		k.fetching.Unlock() //nolint:staticcheck // khoá chỉ dùng để chờ lần tải đang chạy xong
		if k.empty() {
			return errKeysUnavailable
		}
		return nil
	}
	defer k.fetching.Unlock()

	k.mu.RLock()
	tooSoon := !k.lastFetch.IsZero() && time.Since(k.lastFetch) < k.minRefresh
	empty := len(k.keys) == 0
	k.mu.RUnlock()
	if tooSoon {
		if empty {
			return errKeysUnavailable // lần tải gần nhất đã hỏng và chưa tới lúc thử lại
		}
		return nil
	}

	keys, err := k.fetch(ctx)
	k.mu.Lock()
	defer k.mu.Unlock()
	k.lastFetch = time.Now() // tính cả lần hỏng: IdP đang lỗi thì không dồn thêm tải
	if err != nil {
		if len(k.keys) == 0 {
			return fmt.Errorf("%w: %v", errKeysUnavailable, err) //nolint:errorlint // chỉ giữ sentinel
		}
		return nil // giữ khoá cũ; token mang kid lạ sẽ bị từ chối
	}
	k.keys = keys
	return nil
}

func (k *keySet) fetch(ctx context.Context) (map[string]jose.JSONWebKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := k.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jwks: status %d", resp.StatusCode)
	}
	var set jose.JSONWebKeySet
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJWKSBytes)).Decode(&set); err != nil {
		return nil, fmt.Errorf("jwks: %w", err)
	}
	keys := make(map[string]jose.JSONWebKey, len(set.Keys))
	for _, key := range set.Keys {
		if key.Use != "" && key.Use != "sig" {
			continue
		}
		if key.Valid() && key.IsPublic() {
			keys[key.KeyID] = key
		}
	}
	if len(keys) == 0 {
		return nil, errors.New("jwks: no usable signing keys")
	}
	return keys, nil
}
