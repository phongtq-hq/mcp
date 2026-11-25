// Package signedctx ký và kiểm tra ngữ cảnh mà MCP Gateway gửi cho managed MCP
// server: request này thay mặt org, project và người khởi tạo nào.
//
// Managed server không nhận token của client (Gateway không chuyển tiếp token,
// ADR-0004); nó chỉ tin ngữ cảnh mang chữ ký của Gateway.
//
// Chữ ký là Ed25519 (ADR-0018): chỉ bên ký (gateway, và hqadmin cho discovery) giữ
// khoá riêng; managed server chỉ giữ khoá công khai nên KHÔNG ký được. Một integration
// bị chiếm quyền vì thế không giả được ngữ cảnh để gọi integration khác (trước đây là
// HMAC với một khoá dùng chung: bên kiểm cũng là bên ký được).
//
// Mỗi ngữ cảnh gắn với đúng một request: đích (aud = slug của server), hash SHA-256
// của body và một định danh dùng một lần (jti), thời hạn ngắn. Header bị lộ không dùng
// lại được cho server khác, cho một body khác, hay lần thứ hai (server giữ ReplayGuard).
package signedctx

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Header là tên header HTTP mang ngữ cảnh đã ký.
const Header = "X-HQ-Context"

const (
	minKeyBytes = 32
	// DefaultTTL là thời hạn của một ngữ cảnh: đủ cho một tool call, quá ngắn để dùng lại.
	DefaultTTL = 2 * time.Minute
	maxSkew    = 30 * time.Second
	// MaxSignedBody là trần body mà Transport đọc để tính hash. Request MCP tới managed
	// server nhỏ hơn nhiều (serve.MaxRequestBody là 1 MiB).
	MaxSignedBody = 4 << 20
)

// Loại ngữ cảnh.
const (
	// KindCall: một tool call thay mặt một org, project và người khởi tạo.
	KindCall = "call"
	// KindDiscovery: Registry đọc danh sách tool của server. Không gắn với tenant nào.
	KindDiscovery = "discovery"
)

// Context là ngữ cảnh của một lời gọi từ Gateway tới managed server.
type Context struct {
	Kind        string     `json:"kind"`
	Audience    string     `json:"aud"` // slug của managed server đích
	OrgID       uuid.UUID  `json:"org"`
	ProjectID   uuid.UUID  `json:"project"`
	Initiator   string     `json:"initiator"` // "user:<uuid>" hoặc "api_key:<uuid>"
	ExecutionID *uuid.UUID `json:"execution,omitempty"`
	// BodySHA256 là hash (hex) của đúng body request mang ngữ cảnh này.
	BodySHA256 string `json:"bsha"`
	// ID là định danh dùng một lần của ngữ cảnh (jti). Sign tự sinh nếu rỗng.
	ID        string `json:"jti"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
}

// ErrInvalid báo ngữ cảnh thiếu, sai chữ ký, sai đích, sai body, đã dùng hoặc hết hạn.
// Người gọi không cần và không nên phân biệt các trường hợp với client.
var ErrInvalid = errors.New("signedctx: invalid or expired context")

// ParseKey đọc một khoá HMAC dạng hex (ít nhất 32 byte). Dùng cho khoá đối xứng khác
// của platform (execution token, service token); khoá ký ngữ cảnh dùng ParseSigningKey.
func ParseKey(hexKey string) ([]byte, error) {
	key, err := hex.DecodeString(strings.TrimSpace(hexKey))
	if err != nil || len(key) < minKeyBytes {
		return nil, errors.New("signedctx: khoá phải là chuỗi hex của ít nhất 32 byte")
	}
	return key, nil
}

// ParseSigningKey đọc khoá ký ngữ cảnh: seed Ed25519 32 byte dạng hex (64 ký tự), tạo
// bằng `hqadmin service-key init`. Độ dài khác bị từ chối, kể cả khoá HMAC dài hơn của
// bản cũ: đổi sang chữ ký bất đối xứng phải là một bước có chủ ý (ADR-0018).
func ParseSigningKey(hexKey string) (ed25519.PrivateKey, error) {
	seed, err := hex.DecodeString(strings.TrimSpace(hexKey))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("signedctx: khoá ký phải là seed Ed25519 32 byte dạng hex (tạo bằng hqadmin service-key init)")
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// ParseVerifyKey đọc khoá công khai Ed25519 32 byte dạng hex (`hqadmin service-key public`).
func ParseVerifyKey(hexKey string) (ed25519.PublicKey, error) {
	pub, err := hex.DecodeString(strings.TrimSpace(hexKey))
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return nil, errors.New("signedctx: khoá kiểm phải là khoá công khai Ed25519 32 byte dạng hex (hqadmin service-key public)")
	}
	return ed25519.PublicKey(pub), nil
}

// GenerateSigningKey sinh một seed mới, trả về seed và khoá công khai dạng hex.
func GenerateSigningKey() (seedHex, publicHex string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	return hex.EncodeToString(priv.Seed()), hex.EncodeToString(pub), nil
}

// Signer ký ngữ cảnh bằng khoá riêng Ed25519. Chỉ gateway và hqadmin giữ nó.
type Signer struct {
	priv ed25519.PrivateKey
}

// NewSigner tạo Signer từ khoá riêng (ParseSigningKey).
func NewSigner(priv ed25519.PrivateKey) (*Signer, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, errors.New("signedctx: khoá riêng Ed25519 không hợp lệ")
	}
	return &Signer{priv: append(ed25519.PrivateKey(nil), priv...)}, nil
}

// Verifier trả về bên kiểm ứng với khoá này.
func (s *Signer) Verifier() *Verifier {
	return &Verifier{pub: s.priv.Public().(ed25519.PublicKey)}
}

// PublicKeyHex là khoá công khai dạng hex, để cấu hình cho managed server.
func (s *Signer) PublicKeyHex() string {
	return hex.EncodeToString(s.priv.Public().(ed25519.PublicKey))
}

// BodyHash là hash hex của body, đúng như ngữ cảnh mang trong BodySHA256.
func BodyHash(body []byte) string {
	h := sha256.Sum256(body)
	return hex.EncodeToString(h[:])
}

// Sign đặt thời điểm phát hành, hạn và (nếu rỗng) jti rồi trả về giá trị header. Người
// gọi phải đặt BodySHA256; SignBody làm việc đó.
func (s *Signer) Sign(c Context, now time.Time) string {
	c.IssuedAt = now.Unix()
	c.ExpiresAt = now.Add(DefaultTTL).Unix()
	if c.ID == "" {
		c.ID = uuid.NewString()
	}
	body, _ := json.Marshal(c)
	payload := base64.RawURLEncoding.EncodeToString(body)
	return payload + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(s.priv, []byte(payload)))
}

// SignBody ký ngữ cảnh cho đúng body này, với một jti mới.
func (s *Signer) SignBody(c Context, body []byte, now time.Time) string {
	c.BodySHA256 = BodyHash(body)
	c.ID = ""
	return s.Sign(c, now)
}

// Transport gắn vào mỗi request đi qua nó một ngữ cảnh mới ký cho đúng body của request.
type Transport struct {
	Signer  *Signer
	Context Context
	Base    http.RoundTripper
}

// RoundTrip hiện thực http.RoundTripper.
func (t *Transport) RoundTrip(r *http.Request) (*http.Response, error) {
	var body []byte
	if r.Body != nil && r.Body != http.NoBody {
		var err error
		body, err = io.ReadAll(io.LimitReader(r.Body, MaxSignedBody+1))
		_ = r.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("signedctx: đọc body: %w", err)
		}
		if len(body) > MaxSignedBody {
			return nil, errors.New("signedctx: body vượt trần")
		}
	}
	r = r.Clone(r.Context())
	if body != nil {
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	}
	r.Header.Set(Header, t.Signer.SignBody(t.Context, body, time.Now()))
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(r)
}

// Verifier kiểm ngữ cảnh bằng khoá công khai. Managed server chỉ giữ nó.
type Verifier struct {
	pub ed25519.PublicKey
}

// NewVerifier tạo Verifier từ khoá công khai (ParseVerifyKey).
func NewVerifier(pub ed25519.PublicKey) (*Verifier, error) {
	if len(pub) != ed25519.PublicKeySize {
		return nil, errors.New("signedctx: khoá công khai Ed25519 không hợp lệ")
	}
	return &Verifier{pub: append(ed25519.PublicKey(nil), pub...)}, nil
}

// Verify kiểm chữ ký, đích, hash body và thời hạn của giá trị header. Nó KHÔNG kiểm
// jti đã dùng chưa: việc đó là của ReplayGuard ở server, sau khi body đã khớp.
func (v *Verifier) Verify(value, audience string, body []byte, now time.Time) (Context, error) {
	payload, sig, ok := strings.Cut(value, ".")
	if !ok || len(value) > 4096 {
		return Context{}, ErrInvalid
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || len(got) != ed25519.SignatureSize || !ed25519.Verify(v.pub, []byte(payload), got) {
		return Context{}, ErrInvalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return Context{}, ErrInvalid
	}
	var c Context
	if json.Unmarshal(raw, &c) != nil {
		return Context{}, ErrInvalid
	}
	if c.Audience == "" || c.Audience != audience || c.ID == "" || len(c.ID) > 64 {
		return Context{}, ErrInvalid
	}
	if subtle.ConstantTimeCompare([]byte(c.BodySHA256), []byte(BodyHash(body))) != 1 {
		return Context{}, ErrInvalid
	}
	switch c.Kind {
	case KindCall:
		if c.OrgID == uuid.Nil || c.ProjectID == uuid.Nil || c.Initiator == "" {
			return Context{}, ErrInvalid
		}
	case KindDiscovery:
	default:
		return Context{}, ErrInvalid
	}
	if now.Unix() >= c.ExpiresAt || c.IssuedAt > now.Add(maxSkew).Unix() ||
		c.ExpiresAt-c.IssuedAt > int64(DefaultTTL/time.Second) {
		return Context{}, ErrInvalid
	}
	return c, nil
}

// ReplayGuard nhớ jti của các ngữ cảnh đã nhận cho tới khi chúng hết hạn, để một header
// không dùng được hai lần. Nó nằm trong bộ nhớ của một process: mỗi managed server chạy
// một process (một server = một hệ thống đích = một org). Chạy nhiều bản sao sau cân bằng
// tải thì mỗi bản sao có bộ nhớ riêng và một header có thể được nhận một lần ở mỗi bản sao
// trong thời hạn của nó; ghi rõ điều đó trong runbook nếu có ai làm vậy.
type ReplayGuard struct {
	mu   sync.Mutex
	seen map[string]int64
	max  int
}

// DefaultReplayCapacity là số jti tối đa được nhớ cùng lúc: đủ cho hơn 800 request mỗi
// giây liên tục trong một thời hạn ngữ cảnh. Đầy thì từ chối (fail-closed), không quên bớt.
const DefaultReplayCapacity = 100_000

// NewReplayGuard tạo ReplayGuard với sức chứa capacity (≤ 0 → DefaultReplayCapacity).
func NewReplayGuard(capacity int) *ReplayGuard {
	if capacity <= 0 {
		capacity = DefaultReplayCapacity
	}
	return &ReplayGuard{seen: map[string]int64{}, max: capacity}
}

// Accept ghi nhận jti của c và trả về true nếu đây là lần đầu nó được thấy. Trả về false
// khi jti đã dùng, hoặc khi bộ nhớ đầy các jti còn hạn.
func (g *ReplayGuard) Accept(c Context, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, dup := g.seen[c.ID]; dup {
		return false
	}
	if len(g.seen) >= g.max {
		unix := now.Unix()
		for id, exp := range g.seen {
			if exp <= unix {
				delete(g.seen, id)
			}
		}
		if len(g.seen) >= g.max {
			return false
		}
	}
	g.seen[c.ID] = c.ExpiresAt
	return true
}
