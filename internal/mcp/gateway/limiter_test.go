package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

func TestMemoryLimiterClasses(t *testing.T) {
	l := newMemoryLimiter()
	org, p := uuid.New(), uuid.New()
	ctx := context.Background()
	// Discovery có hạn mức thấp riêng: 6 lần mỗi phút.
	for i := 0; i < perMinute[classDiscovery]; i++ {
		if ok, _, err := l.Allow(ctx, org, p, classDiscovery); !ok || err != nil {
			t.Fatalf("discovery lần %d bị từ chối", i+1)
		}
	}
	if ok, wait, _ := l.Allow(ctx, org, p, classDiscovery); ok || wait <= 0 {
		t.Error("vượt hạn mức discovery mà vẫn được phép")
	}
	if ok, _, _ := l.Allow(ctx, org, p, classMCP); !ok {
		t.Error("tool call bị hạn mức discovery chặn")
	}
	// Lớp khác có hạn mức riêng.
	if ok, _, _ := l.Allow(ctx, org, p, classModel); !ok {
		t.Error("lượt gọi model bị client MCP của cùng principal chặn")
	}
	if _, _, err := l.Allow(ctx, org, p, "nope"); err == nil {
		t.Error("lớp không xác định được chấp nhận")
	}
}

// Token bucket trên Redis thật: hạn mức dùng chung giữa các "replica", tách theo org,
// principal và lớp; không kiểm tra được thì báo lỗi (để Gateway từ chối).
func TestRedisLimiter(t *testing.T) {
	if testing.Short() {
		t.Skip("cần Docker")
	}
	ctx := context.Background()
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{Image: "redis:8-alpine", ExposedPorts: []string{"6379/tcp"},
			WaitingFor: wait.ForLog("Ready to accept connections")},
		Started: true,
	})
	if err != nil {
		t.Fatalf("redis: %v", err)
	}
	t.Cleanup(func() { _ = c.Terminate(context.Background()) })
	host, _ := c.Host(ctx)
	port, _ := c.MappedPort(ctx, "6379/tcp")
	addr := host + ":" + port.Port()
	// Hai client = hai replica của Gateway.
	a, b := redis.NewClient(&redis.Options{Addr: addr}), redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	limits := map[string]int{classMCP: 5, classModel: 5} // dồn 5, nạp 5 mỗi phút
	la, lb := NewRedisLimiter(a), NewRedisLimiter(b)
	la.limits, lb.limits = limits, limits
	org, other, p := uuid.New(), uuid.New(), uuid.New()

	allowed := 0
	var wait time.Duration
	for i := 0; i < 8; i++ {
		l := la
		if i%2 == 1 {
			l = lb
		}
		ok, w, err := l.Allow(ctx, org, p, classMCP)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			allowed++
		} else {
			wait = w
		}
	}
	if allowed != 5 || wait <= 0 || wait > 13*time.Second {
		t.Errorf("hai replica cho qua %d request (muốn 5), chờ %v", allowed, wait)
	}
	for name, args := range map[string][3]any{
		"org khác":       {other, p, classMCP},
		"principal khác": {org, uuid.New(), classMCP},
		"lớp khác":       {org, p, classModel},
	} {
		if ok, _, err := la.Allow(ctx, args[0].(uuid.UUID), args[1].(uuid.UUID), args[2].(string)); !ok || err != nil {
			t.Errorf("%s bị ảnh hưởng bởi hạn mức của principal khác: %v %v", name, ok, err)
		}
	}
	// Bucket nạp lại theo thời gian (hạn mức 120 mỗi phút = 2 mỗi giây).
	fast := NewRedisLimiter(a)
	fast.limits = map[string]int{classModel: 120}
	q := uuid.New()
	for i := 0; i < 120; i++ {
		_, _, _ = fast.Allow(ctx, org, q, classModel)
	}
	if ok, _, _ := fast.Allow(ctx, org, q, classModel); ok {
		t.Error("bucket không cạn")
	}
	time.Sleep(700 * time.Millisecond)
	if ok, _, _ := fast.Allow(ctx, org, q, classModel); !ok {
		t.Error("bucket không nạp lại")
	}
	if _, _, err := la.Allow(ctx, org, p, "nope"); err == nil {
		t.Error("lớp không xác định được chấp nhận")
	}
	// Khoá mang tiền tố tenant và tự hết hạn.
	keys, _ := a.Keys(ctx, "*").Result()
	for _, k := range keys {
		if k[:5] != "org:{" {
			t.Errorf("khoá Redis thiếu tiền tố tenant: %s", k)
		}
		if ttl, _ := a.PTTL(ctx, k).Result(); ttl <= 0 {
			t.Errorf("khoá %s không có hạn", k)
		}
	}
	// Redis không tới được: báo lỗi, không cho qua.
	_ = c.Terminate(ctx)
	if ok, _, err := la.Allow(ctx, org, p, classMCP); ok || err == nil {
		t.Errorf("mất Redis mà vẫn cho qua: ok=%v err=%v", ok, err)
	}
}
