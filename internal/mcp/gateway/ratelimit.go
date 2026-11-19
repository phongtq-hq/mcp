package gateway

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// rateLimiter giới hạn số request theo principal bằng cửa sổ cố định.
//
// Bản này nằm trong bộ nhớ của một process: với nhiều replica, giới hạn thực tế là
// tổng của các replica. Thiết kế yêu cầu token bucket trên Redis (theo org,
// principal và tool); việc đó thuộc phần Redis, chưa hiện thực.
type rateLimiter struct {
	limit  int
	window time.Duration

	mu      sync.Mutex
	buckets map[uuid.UUID]*bucket
}

type bucket struct {
	start time.Time
	count int
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{limit: limit, window: window, buckets: map[uuid.UUID]*bucket{}}
}

// allow trả về true nếu principal còn hạn mức; nếu không, trả về thời gian chờ.
func (l *rateLimiter) allow(principal uuid.UUID, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.buckets) > 100_000 { // chặn tăng không giới hạn; mất bộ đếm là an toàn về phía chậm lại sau
		clear(l.buckets)
	}
	b := l.buckets[principal]
	if b == nil || now.Sub(b.start) >= l.window {
		l.buckets[principal] = &bucket{start: now, count: 1}
		return true, 0
	}
	if b.count >= l.limit {
		return false, l.window - now.Sub(b.start)
	}
	b.count++
	return true, 0
}
