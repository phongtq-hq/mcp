package budget

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/hqgroups/mcp/internal/cost/pricing"
)

func TestMoneyIsIntegerArithmetic(t *testing.T) {
	for in, want := range map[string]int64{"0": 0, "1": 100_000_000, "0.00000001": 1, "12.5": 1_250_000_000, "10000000": maxLimitUnits} {
		if got, ok := ParseAmount(in); !ok || got != want {
			t.Errorf("ParseAmount(%q) = %d, %v; muốn %d", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "-1", "1e3", "1.123456789", "abc", "1,5", " 1", "NaN", "12345678901"} {
		if _, ok := ParseAmount(bad); ok {
			t.Errorf("ParseAmount(%q) được chấp nhận", bad)
		}
	}
	if got := FormatAmount(1_250_000_001); got != "12.50000001" {
		t.Errorf("FormatAmount = %s", got)
	}
	// 3 USD / triệu token input, 15 USD / triệu token output.
	cached := "0.30"
	p := pricing.Pricing{InputPerMtok: "3.00", OutputPerMtok: "15.00", CachedInputPerMtok: &cached}
	// 1000 token vào + 100 token ra + 1000 token đọc cache = 0.003 + 0.0015 + 0.0003.
	if got := Actual(p, Usage{InputTokens: 1000, OutputTokens: 100, CachedInputTokens: 1000}); got != 480_000 {
		t.Errorf("Actual = %d, muốn 480000", got)
	}
	// Token ghi cache không có giá riêng thì tính theo giá input; reasoning không có giá riêng thì không tính thêm.
	if got := Actual(p, Usage{CacheWriteTokens: 1000, ReasoningTokens: 5000}); got != 300_000 {
		t.Errorf("Actual (cache write, reasoning) = %d, muốn 300000", got)
	}
	// Ước lượng: 2000 byte ≈ 1000 token vào, cộng trần 4096 token ra.
	if got := Estimate(p, 2000, 4096); got != 300_000+6_144_000 {
		t.Errorf("Estimate = %d", got)
	}
	// Ghi cache đắt hơn input, reasoning đắt hơn output: ước lượng lấy giá cao hơn.
	cw, rs := "3.75", "20"
	if got := Estimate(pricing.Pricing{InputPerMtok: "3.00", OutputPerMtok: "15.00", CacheWritePerMtok: &cw, ReasoningPerMtok: &rs}, 2000, 1000); got != 375_000+2_000_000 {
		t.Errorf("Estimate (cache write, reasoning) = %d", got)
	}
	// Làm tròn lên: một token ở giá nhỏ nhất vẫn tốn ít nhất một đơn vị.
	if got := costUnits(1, "0.00000001"); got != 1 {
		t.Errorf("costUnits làm tròn = %d, muốn 1", got)
	}
}

func TestPeriodBounds(t *testing.T) {
	at := time.Date(2026, 11, 18, 15, 4, 5, 0, time.FixedZone("ICT", 7*3600))
	start, end := PeriodBounds(PeriodDaily, at)
	if !start.Equal(time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)) || !end.Equal(time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("daily: %s → %s", start, end)
	}
	start, end = PeriodBounds(PeriodMonthly, time.Date(2026, 12, 31, 23, 59, 0, 0, time.UTC))
	if !start.Equal(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)) || !end.Equal(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("monthly: %s → %s", start, end)
	}
	if _, end := PeriodBounds(PeriodTotal, at); end != nil {
		t.Errorf("total có cuối kỳ: %s", end)
	}
}

// counterContract là hành vi mọi Counter phải có.
func counterContract(t *testing.T, c Counter, prefix string) {
	t.Helper()
	ctx := context.Background()
	exp := time.Now().Add(time.Hour)
	org := Bucket{Key: prefix + ":org", Limit: 1000, Enforce: true, ExpireAt: exp}
	proj := Bucket{Key: prefix + ":proj", Limit: 300, Enforce: true, ExpireAt: exp}
	watch := Bucket{Key: prefix + ":watch", Limit: 100, Enforce: false, ExpireAt: exp}
	all := []Bucket{org, proj, watch}

	// Bucket chưa khởi tạo: báo thiếu, không đặt gì.
	got, err := c.Reserve(ctx, "r0", all, 100, time.Minute)
	if err != nil || got.OK || len(got.Missing) != 3 {
		t.Fatalf("chưa khởi tạo: %+v, %v", got, err)
	}
	for _, b := range all {
		if err := c.Seed(ctx, b, 50); err != nil {
			t.Fatal(err)
		}
	}
	// Seed lần hai không ghi đè.
	if err := c.Seed(ctx, org, 999); err != nil {
		t.Fatal(err)
	}

	// 50 đã tiêu + 200 đặt trước: đủ ở mọi bucket chặn; bucket chỉ theo dõi không chặn dù vượt.
	if got, err := c.Reserve(ctx, "r1", all, 200, time.Minute); err != nil || !got.OK {
		t.Fatalf("r1: %+v, %v", got, err)
	}
	// Thêm 100 nữa: project (300) không đủ (50 + 200 + 100), và organization KHÔNG bị giữ lại gì.
	got, err = c.Reserve(ctx, "r2", all, 100, time.Minute)
	if err != nil || got.OK || len(got.Missing) != 0 || got.Exceeded != 1 {
		t.Fatalf("r2: %+v, %v", got, err)
	}
	// Nếu r2 đã lỡ giữ 100 ở organization thì lần này (700) sẽ bị từ chối: 50 + 200 + 100 + 700 > 1000.
	if got, err := c.Reserve(ctx, "r3", []Bucket{org}, 700, time.Minute); err != nil || !got.OK {
		t.Fatalf("r3 (kiểm việc hoàn lại khi một bucket từ chối): %+v, %v", got, err)
	}
	// Quyết toán r1 với mức thật 30: phần đặt trước 200 được hoàn, mức đã tiêu thành 80.
	spent, err := c.Settle(ctx, "r1", all, 30)
	if err != nil || len(spent) != 3 || spent[1] != 80 || spent[2] != 80 {
		t.Fatalf("settle r1: %v, %v", spent, err)
	}
	// Giờ project còn chỗ: 80 + 200 ≤ 300.
	if got, err := c.Reserve(ctx, "r4", []Bucket{proj}, 200, time.Minute); err != nil || !got.OK {
		t.Fatalf("r4: %+v, %v", got, err)
	}
	// Quyết toán hai lần không cộng trùng phần hoàn (mức tiêu thật vẫn được cộng: đó là một lượt khác).
	if _, err := c.Settle(ctx, "r4", []Bucket{proj}, 0); err != nil {
		t.Fatal(err)
	}
	if spent, _ := c.Settle(ctx, "r4", []Bucket{proj}, 0); spent[0] != 80 {
		t.Errorf("settle lặp: spent = %d, muốn 80", spent[0])
	}

	// Phần đặt trước hết hạn tự hoàn (người đặt chết giữa chừng).
	small := Bucket{Key: prefix + ":ttl", Limit: 100, Enforce: true, ExpireAt: exp}
	if err := c.Seed(ctx, small, 0); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Reserve(ctx, "dead", []Bucket{small}, 100, 50*time.Millisecond); !got.OK {
		t.Fatal("đặt trước ban đầu bị từ chối")
	}
	if got, _ := c.Reserve(ctx, "next", []Bucket{small}, 100, time.Minute); got.OK {
		t.Fatal("đặt được khi phần trước còn hiệu lực")
	}
	time.Sleep(120 * time.Millisecond)
	if got, err := c.Reserve(ctx, "next", []Bucket{small}, 100, time.Minute); err != nil || !got.OK {
		t.Fatalf("sau khi phần trước hết hạn: %+v, %v", got, err)
	}

	// Đặt lại cùng id (lần gọi trước chết, lần thử lại dùng lại id): thay thế, không cộng dồn.
	again := Bucket{Key: prefix + ":again", Limit: 100, Enforce: true, ExpireAt: exp}
	if err := c.Seed(ctx, again, 0); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if got, err := c.Reserve(ctx, "same", []Bucket{again}, 60, time.Minute); err != nil || !got.OK {
			t.Fatalf("đặt lại cùng id lần %d: %+v, %v", i+1, got, err)
		}
	}
	if got, _ := c.Reserve(ctx, "other", []Bucket{again}, 60, time.Minute); got.OK {
		t.Error("id khác đặt được dù phần của id cũ còn giữ chỗ")
	}
	if spent, err := c.Settle(ctx, "same", []Bucket{again}, 10); err != nil || spent[0] != 10 {
		t.Fatalf("settle sau khi đặt lại: %v, %v", spent, err)
	}
	if got, _ := c.Reserve(ctx, "other", []Bucket{again}, 60, time.Minute); !got.OK {
		t.Error("sau khi quyết toán, phần đặt lại vẫn còn giữ chỗ (rò)")
	}

	// Once: đúng một lần cho mỗi khoá.
	a, _ := c.Once(ctx, prefix+":once", time.Minute)
	b, _ := c.Once(ctx, prefix+":once", time.Minute)
	if !a || b {
		t.Errorf("Once: %v, %v", a, b)
	}
}

func TestMemoryCounter(t *testing.T) { counterContract(t, NewMemoryCounter(), "org:{m}:budget") }

// Bộ đếm trên Redis thật: cùng hợp đồng, dùng chung giữa hai client ("replica").
func TestRedisCounter(t *testing.T) {
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
	rdb := redis.NewClient(&redis.Options{Addr: host + ":" + port.Port()})
	t.Cleanup(func() { _ = rdb.Close() })
	counterContract(t, NewRedisCounter(rdb), "org:{r}:budget")

	// Hai replica cùng đặt vào một bucket: tổng không vượt hạn mức.
	other := redis.NewClient(&redis.Options{Addr: host + ":" + port.Port()})
	t.Cleanup(func() { _ = other.Close() })
	b := Bucket{Key: "org:{r}:budget:shared", Limit: 100, Enforce: true, ExpireAt: time.Now().Add(time.Hour)}
	a1, a2 := NewRedisCounter(rdb), NewRedisCounter(other)
	if err := a1.Seed(ctx, b, 0); err != nil {
		t.Fatal(err)
	}
	ok := 0
	for i := 0; i < 10; i++ {
		ctr := a1
		if i%2 == 1 {
			ctr = a2
		}
		if got, err := ctr.Reserve(ctx, "x"+string(rune('a'+i)), []Bucket{b}, 30, time.Minute); err != nil {
			t.Fatal(err)
		} else if got.OK {
			ok++
		}
	}
	if ok != 3 {
		t.Errorf("đặt được %d lần 30 vào hạn mức 100, muốn 3", ok)
	}
	// Redis không tới được: báo lỗi (để bên gọi fail-closed).
	_ = other.Close()
	if _, err := a2.Reserve(ctx, "z", []Bucket{b}, 1, time.Minute); err == nil {
		t.Error("Redis đã đóng mà Reserve không báo lỗi")
	}
}
