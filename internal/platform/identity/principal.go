// Package identity xác thực request và xác định principal: ai đang gọi và
// thay mặt organization nào. Thuộc bounded context Identity & Tenancy.
package identity

import (
	"context"

	"github.com/google/uuid"
)

// Loại principal.
const (
	PrincipalUser   = "user"
	PrincipalAPIKey = "api_key"
)

// Principal là chủ thể đã xác thực của một request.
type Principal struct {
	Type  string
	ID    uuid.UUID
	OrgID uuid.UUID
}

type principalKey struct{}

// WithPrincipal gắn principal vào context. Chỉ middleware xác thực gọi hàm này.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom trả về principal của request.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok && p.ID != uuid.Nil
}
