// Package exectoken phát và kiểm tra execution token: JWT ngắn hạn do platform ký,
// để Agent Runtime xác thực với MCP Gateway thay mặt một task (ADR-0004 mục 4,
// mcp-platform §4).
//
// Token mang ngữ cảnh của task; quyền thực sự vẫn do Gateway quyết định lại ở mỗi
// tool call (quyền người khởi tạo ∩ cấu hình Agent Version ∩ trạng thái tool).
// Token là HS256 với khoá chỉ process phát (api ở Phase 1, worker từ Phase 2) và
// gateway giữ. Nó không được chấp nhận ở endpoint dành cho token OIDC và ngược lại.
package exectoken

import (
	"errors"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/uuid"
)

const (
	issuer   = "hq-platform"
	audience = "hq-gateway-internal"
	// MaxTTL là trần thời hạn của một token, bất kể deadline của task.
	MaxTTL      = time.Hour
	minKeyBytes = 32
)

// Claims là ngữ cảnh của một task.
type Claims struct {
	OrgID          uuid.UUID `json:"org"`
	ProjectID      uuid.UUID `json:"project"`
	ExecutionID    uuid.UUID `json:"execution"`
	TaskID         uuid.UUID `json:"task"`
	AgentVersionID uuid.UUID `json:"agent_version"`
	InitiatorType  string    `json:"initiator_type"`
	InitiatorID    uuid.UUID `json:"initiator_id"`
}

// ErrInvalid báo token thiếu, sai chữ ký, sai đích hoặc hết hạn.
var ErrInvalid = errors.New("exectoken: invalid or expired token")

// Issuer ký và kiểm tra execution token.
type Issuer struct {
	key    []byte
	signer jose.Signer
}

// New tạo Issuer từ khoá ít nhất 32 byte.
func New(key []byte) (*Issuer, error) {
	if len(key) < minKeyBytes {
		return nil, errors.New("exectoken: khoá phải có ít nhất 32 byte")
	}
	k := append([]byte(nil), key...)
	s, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.HS256, Key: k}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		return nil, err
	}
	return &Issuer{key: k, signer: s}, nil
}

type wire struct {
	jwt.Claims
	Claims2 Claims `json:"hq"`
}

// Sign phát token hết hạn tại expires (bị chặn ở MaxTTL kể từ now).
func (i *Issuer) Sign(c Claims, now, expires time.Time) (string, error) {
	if c.OrgID == uuid.Nil || c.ProjectID == uuid.Nil || c.ExecutionID == uuid.Nil || c.TaskID == uuid.Nil ||
		c.AgentVersionID == uuid.Nil || c.InitiatorID == uuid.Nil || c.InitiatorType == "" {
		return "", errors.New("exectoken: thiếu ngữ cảnh")
	}
	if limit := now.Add(MaxTTL); expires.After(limit) {
		expires = limit
	}
	return jwt.Signed(i.signer).Claims(wire{
		Claims: jwt.Claims{Issuer: issuer, Audience: jwt.Audience{audience}, Subject: c.TaskID.String(),
			IssuedAt: jwt.NewNumericDate(now), Expiry: jwt.NewNumericDate(expires)},
		Claims2: c,
	}).Serialize()
}

// Verify kiểm tra chữ ký, đích và thời hạn.
func (i *Issuer) Verify(token string, now time.Time) (Claims, error) {
	if len(token) > 4096 {
		return Claims{}, ErrInvalid
	}
	// Chỉ HS256: token ký bằng thuật toán khác (kể cả token OIDC) bị từ chối ngay ở bước parse.
	parsed, err := jwt.ParseSigned(token, []jose.SignatureAlgorithm{jose.HS256})
	if err != nil {
		return Claims{}, ErrInvalid
	}
	var w wire
	if err := parsed.Claims(i.key, &w); err != nil {
		return Claims{}, ErrInvalid
	}
	if w.Expiry == nil || w.IssuedAt == nil || w.Expiry.Time().Sub(w.IssuedAt.Time()) > MaxTTL+time.Minute {
		return Claims{}, ErrInvalid
	}
	if err := w.ValidateWithLeeway(jwt.Expected{Issuer: issuer, AnyAudience: jwt.Audience{audience}, Time: now}, 0); err != nil {
		return Claims{}, ErrInvalid
	}
	c := w.Claims2
	if c.OrgID == uuid.Nil || c.ProjectID == uuid.Nil || c.ExecutionID == uuid.Nil || c.TaskID == uuid.Nil ||
		c.AgentVersionID == uuid.Nil || c.InitiatorID == uuid.Nil || c.InitiatorType == "" || w.Subject != c.TaskID.String() {
		return Claims{}, ErrInvalid
	}
	return c, nil
}

// Service token: api gọi một thao tác của Gateway không gắn với execution nào (hiện
// là discovery của MCP server). Cùng khoá và cùng cách ký với execution token, nhưng
// đích (audience) khác, nên hai loại token không dùng thay cho nhau được.
const (
	serviceAudience = "hq-gateway-service"
	serviceTTL      = time.Minute
)

// ServiceClaims là ngữ cảnh của một thao tác do api uỷ cho Gateway.
type ServiceClaims struct {
	OrgID     uuid.UUID `json:"org"`
	Purpose   string    `json:"purpose"`  // ví dụ "mcp_discovery"
	Resource  uuid.UUID `json:"resource"` // tài nguyên duy nhất token này áp dụng
	ActorType string    `json:"actor_type"`
	ActorID   uuid.UUID `json:"actor_id"`
}

type serviceWire struct {
	jwt.Claims
	Service ServiceClaims `json:"hq_service"`
}

// SignService phát service token sống một phút.
func (i *Issuer) SignService(c ServiceClaims, now time.Time) (string, error) {
	if c.OrgID == uuid.Nil || c.Resource == uuid.Nil || c.ActorID == uuid.Nil || c.Purpose == "" || c.ActorType == "" {
		return "", errors.New("exectoken: thiếu ngữ cảnh")
	}
	return jwt.Signed(i.signer).Claims(serviceWire{
		Claims: jwt.Claims{Issuer: issuer, Audience: jwt.Audience{serviceAudience}, Subject: c.Resource.String(),
			IssuedAt: jwt.NewNumericDate(now), Expiry: jwt.NewNumericDate(now.Add(serviceTTL))},
		Service: c,
	}).Serialize()
}

// VerifyService kiểm tra một service token cho đúng mục đích.
func (i *Issuer) VerifyService(token, purpose string, now time.Time) (ServiceClaims, error) {
	if len(token) > 4096 {
		return ServiceClaims{}, ErrInvalid
	}
	parsed, err := jwt.ParseSigned(token, []jose.SignatureAlgorithm{jose.HS256})
	if err != nil {
		return ServiceClaims{}, ErrInvalid
	}
	var w serviceWire
	if err := parsed.Claims(i.key, &w); err != nil {
		return ServiceClaims{}, ErrInvalid
	}
	if w.Expiry == nil || w.IssuedAt == nil || w.Expiry.Time().Sub(w.IssuedAt.Time()) > serviceTTL+time.Second {
		return ServiceClaims{}, ErrInvalid
	}
	if err := w.ValidateWithLeeway(jwt.Expected{Issuer: issuer, AnyAudience: jwt.Audience{serviceAudience}, Time: now}, 0); err != nil {
		return ServiceClaims{}, ErrInvalid
	}
	c := w.Service
	if c.OrgID == uuid.Nil || c.Resource == uuid.Nil || c.ActorID == uuid.Nil || c.Purpose != purpose || c.ActorType == "" ||
		w.Subject != c.Resource.String() {
		return ServiceClaims{}, ErrInvalid
	}
	return c, nil
}
