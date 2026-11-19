// Package signedctx ký và kiểm tra ngữ cảnh mà MCP Gateway gửi cho managed MCP
// server: request này thay mặt org, project và người khởi tạo nào.
//
// Managed server không nhận token của client (Gateway không chuyển tiếp token,
// ADR-0004); nó chỉ tin ngữ cảnh mang chữ ký của Gateway. Chữ ký là HMAC-SHA256
// với một khoá dùng chung giữa Gateway và các managed server, có đích (aud) và
// thời hạn ngắn để một header bị lộ không dùng lại được cho server khác hay về sau.
package signedctx

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
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
	IssuedAt    int64      `json:"iat"`
	ExpiresAt   int64      `json:"exp"`
}

// ErrInvalid báo ngữ cảnh thiếu, sai chữ ký, sai đích hoặc hết hạn. Người gọi
// không cần và không nên phân biệt các trường hợp với client.
var ErrInvalid = errors.New("signedctx: invalid or expired context")

// Signer ký và kiểm tra ngữ cảnh bằng một khoá dùng chung.
type Signer struct {
	key []byte
}

// ParseKey đọc khoá dạng hex (ít nhất 32 byte). Khoá ngắn hoặc rỗng bị từ chối:
// không có giá trị mặc định.
func ParseKey(hexKey string) ([]byte, error) {
	key, err := hex.DecodeString(strings.TrimSpace(hexKey))
	if err != nil || len(key) < minKeyBytes {
		return nil, errors.New("signedctx: khoá phải là chuỗi hex của ít nhất 32 byte")
	}
	return key, nil
}

// NewSigner tạo Signer.
func NewSigner(key []byte) (*Signer, error) {
	if len(key) < minKeyBytes {
		return nil, errors.New("signedctx: khoá phải có ít nhất 32 byte")
	}
	return &Signer{key: append([]byte(nil), key...)}, nil
}

func (s *Signer) mac(payload string) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(payload))
	return m.Sum(nil)
}

// Transport gắn một ngữ cảnh mới ký vào mỗi request đi qua nó.
type Transport struct {
	Signer  *Signer
	Context Context
	Base    http.RoundTripper
}

// RoundTrip hiện thực http.RoundTripper.
func (t *Transport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set(Header, t.Signer.Sign(t.Context, time.Now()))
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(r)
}

// Sign đặt thời điểm phát hành và hạn rồi trả về giá trị header.
func (s *Signer) Sign(c Context, now time.Time) string {
	c.IssuedAt = now.Unix()
	c.ExpiresAt = now.Add(DefaultTTL).Unix()
	body, _ := json.Marshal(c)
	payload := base64.RawURLEncoding.EncodeToString(body)
	return payload + "." + base64.RawURLEncoding.EncodeToString(s.mac(payload))
}

// Verify kiểm tra chữ ký, đích và thời hạn của giá trị header.
func (s *Signer) Verify(value, audience string, now time.Time) (Context, error) {
	payload, sig, ok := strings.Cut(value, ".")
	if !ok || len(value) > 4096 {
		return Context{}, ErrInvalid
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, s.mac(payload)) {
		return Context{}, ErrInvalid
	}
	body, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return Context{}, ErrInvalid
	}
	var c Context
	if json.Unmarshal(body, &c) != nil {
		return Context{}, ErrInvalid
	}
	if c.Audience == "" || c.Audience != audience {
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
