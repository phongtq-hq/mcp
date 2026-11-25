package gateway

import (
	"context"
	_ "embed"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/hqgroups/mcp/internal/platform/identity"
	apperr "github.com/hqgroups/mcp/internal/shared/errors"
	"github.com/hqgroups/mcp/internal/shared/httpx"
)

// Lớp request được giới hạn riêng: client MCP ồn ào không làm lượt gọi model của
// execution đang chạy bị từ chối, và ngược lại.
const (
	classMCP       = "mcp"
	classModel     = "model"
	classDiscovery = "discovery"
)

// perMinute là hạn mức của từng lớp, tính theo request mỗi phút cho mỗi principal.
// Discovery là thao tác quản trị: mỗi lần là một loạt request ra một host ngoài, nên
// hạn mức của nó thấp hơn nhiều.
//
// Mỗi lớp còn một hạn mức chung cho cả organization (khoá "<lớp>:org", principal rỗng):
// một org có nhiều API key / người dùng không nhân được hạn mức để đọc hàng loạt dữ liệu
// của hệ thống nội bộ (review bảo mật 2026-11-25). Hạn mức org đủ cho 8 execution song
// song (trần theo org của runtime) cộng client bên ngoài. Chỉ áp cho client bên ngoài và
// discovery (xem limit).
var perMinute = map[string]int{
	classMCP: defaultRateLimit, classModel: defaultRateLimit, classDiscovery: 6,
	classMCP + orgSuffix: 10 * defaultRateLimit, classModel + orgSuffix: 10 * defaultRateLimit, classDiscovery + orgSuffix: 30,
}

// orgSuffix là hậu tố của lớp hạn mức chung cho cả organization.
const orgSuffix = ":org"

// Limiter giới hạn tần suất request theo organization, principal và lớp request.
type Limiter interface {
	// Allow trả về false kèm thời gian nên chờ nếu vượt hạn mức. Lỗi nghĩa là không
	// kiểm tra được hạn mức.
	Allow(ctx context.Context, orgID, principalID uuid.UUID, class string) (ok bool, retryAfter time.Duration, err error)
}

// memoryLimiter là Limiter trong bộ nhớ của một process, cho phát triển cục bộ và
// test. Với nhiều replica, hạn mức thực tế là tổng của các replica.
type memoryLimiter struct {
	classes map[string]*rateLimiter
}

func newMemoryLimiter() *memoryLimiter {
	m := &memoryLimiter{classes: map[string]*rateLimiter{}}
	for class, limit := range perMinute {
		m.classes[class] = newRateLimiter(limit, time.Minute)
	}
	return m
}

func (m *memoryLimiter) Allow(_ context.Context, orgID, principalID uuid.UUID, class string) (bool, time.Duration, error) {
	l, ok := m.classes[class]
	if !ok {
		return false, 0, fmt.Errorf("gateway: lớp rate limit không xác định: %q", class)
	}
	key := principalID
	if key == uuid.Nil {
		key = orgID // hạn mức chung của organization
	}
	allowed, wait := l.allow(key, time.Now())
	return allowed, wait, nil
}

// tokenBucket là token bucket nguyên tử trên Redis. Thời gian lấy từ chính Redis, nên
// các replica của Gateway không cần đồng hồ khớp nhau.
//
//go:embed tokenbucket.lua
var tokenBucket string

// RedisLimiter là token bucket dùng chung cho mọi replica của Gateway (mcp-platform
// §2). Redis chỉ giữ bộ đếm: mất nó thì hạn mức đầy lại, không mất dữ liệu nào.
type RedisLimiter struct {
	rdb    redis.Scripter
	script *redis.Script
	limits map[string]int // request mỗi phút theo lớp; cũng là số request dồn được
}

// NewRedisLimiter tạo limiter với hạn mức mặc định của từng lớp.
func NewRedisLimiter(rdb redis.Scripter) *RedisLimiter {
	return &RedisLimiter{rdb: rdb, script: redis.NewScript(tokenBucket), limits: perMinute}
}

// Allow hiện thực Limiter. Khoá mang tiền tố org:{id}: như mọi khoá Redis của platform.
func (l *RedisLimiter) Allow(ctx context.Context, orgID, principalID uuid.UUID, class string) (bool, time.Duration, error) {
	limit, ok := l.limits[class]
	if !ok || limit < 1 {
		return false, 0, fmt.Errorf("gateway: lớp rate limit không xác định: %q", class)
	}
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	key := "org:{" + orgID.String() + "}:rl:" + class + ":" + principalID.String()
	res, err := l.script.Run(ctx, l.rdb, []string{key}, limit, strconv.FormatFloat(float64(limit)/60, 'f', 6, 64)).Int64Slice()
	if err != nil || len(res) != 2 {
		return false, 0, fmt.Errorf("gateway: rate limit trên Redis: %w", err)
	}
	return res[0] == 1, time.Duration(res[1]) * time.Millisecond, nil
}

// SetLimiter thay limiter mặc định (trong bộ nhớ) bằng limiter dùng chung giữa các replica.
func (g *Gateway) SetLimiter(l Limiter) { g.limits = l }

// limit áp hạn mức cho principal. Trả về false nếu đã ghi phản hồi lỗi. Không kiểm tra
// được hạn mức thì từ chối (fail-closed): rate limit là một lớp bảo vệ, không phải tối ưu.
//
// orgWide: áp thêm hạn mức chung của organization. Chỉ cho client bên ngoài (token OIDC,
// API key) và discovery: một org có nhiều API key không nhân được hạn mức để đọc hàng loạt.
// Lời gọi của execution không bị áp, để vài client ồn ào không làm mọi execution của org
// nhận 429; execution đã có trần riêng (8 vòng lặp Agent đồng thời mỗi org).
func (g *Gateway) limit(w http.ResponseWriter, r *http.Request, p identity.Principal, class string, orgWide bool) bool {
	ok, wait, err := g.limits.Allow(r.Context(), p.OrgID, p.ID, class)
	if err == nil && ok && orgWide {
		// Hạn mức chung của organization (principal rỗng).
		ok, wait, err = g.limits.Allow(r.Context(), p.OrgID, uuid.Nil, class+orgSuffix)
	}
	if err != nil {
		httpx.LogError(r.Context(), "gateway: không kiểm tra được rate limit", err)
		w.Header().Set("Retry-After", "1")
		httpx.WriteError(w, r, apperr.New(apperr.KindUnavailable, "rate_limit_unavailable", "tạm thời không kiểm tra được hạn mức request").AsRetryable())
		return false
	}
	if !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		httpx.WriteError(w, r, apperr.New(apperr.KindRateLimited, "rate_limited", "quá giới hạn request").AsRetryable())
		return false
	}
	return true
}
