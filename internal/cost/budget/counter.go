package budget

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Bucket là bộ đếm của một budget trong một kỳ.
type Bucket struct {
	// Key là khoá của bộ đếm, có tiền tố org:{id}: (quy ước Redis của platform).
	Key string
	// Limit là hạn mức, tính bằng đơn vị 10^-8 của tiền tệ.
	Limit int64
	// Enforce: vượt hạn mức thì từ chối (hard_action = block). false: chỉ đếm.
	Enforce bool
	// ExpireAt là lúc bộ đếm hết giá trị (sau khi kỳ kết thúc).
	ExpireAt time.Time
}

// Reserved là kết quả của một lần đặt trước.
type Reserved struct {
	OK bool
	// Missing là chỉ số các bucket chưa được khởi tạo (kỳ mới, hoặc bộ đếm bị mất):
	// người gọi nạp mức đã tiêu từ ledger bằng Seed rồi gọi lại.
	Missing []int
	// Exceeded là chỉ số bucket không đủ chỗ (chỉ có nghĩa khi OK = false và Missing rỗng).
	Exceeded int
}

// Counter là bộ đếm nhanh của budget: mức đã tiêu cộng các phần đang đặt trước.
// Postgres (usage_records) là nguồn sự thật; bộ đếm dựng lại được từ đó.
type Counter interface {
	// Reserve đặt trước amount ở MỌI bucket, nguyên tử: hoặc tất cả, hoặc không bucket
	// nào. Phần đặt trước tự hết hạn sau ttl (người đặt chết giữa chừng). Đặt lại cùng id
	// thay thế phần đặt trước cũ của id đó, kể cả khi lần đặt lại bị từ chối.
	Reserve(ctx context.Context, id string, buckets []Bucket, amount int64, ttl time.Duration) (Reserved, error)
	// Seed khởi tạo mức đã tiêu của một bucket nếu nó chưa có.
	Seed(ctx context.Context, b Bucket, spent int64) error
	// Settle bỏ phần đặt trước id và cộng mức tiêu thật vào mọi bucket. Trả về mức đã
	// tiêu mới của từng bucket.
	Settle(ctx context.Context, id string, buckets []Bucket, actual int64) ([]int64, error)
	// Once trả true đúng một lần cho mỗi khoá trong ttl.
	Once(ctx context.Context, key string, ttl time.Duration) (bool, error)
}

// ---- trong bộ nhớ ----

type memBucket struct {
	spent    int64
	reserved map[string]memRes
}

type memRes struct {
	amount int64
	expire time.Time
}

// MemoryCounter là Counter trong bộ nhớ của một process, cho phát triển cục bộ và test.
// Nhiều process thì mỗi process đếm riêng: không dùng ngoài local.
type MemoryCounter struct {
	mu      sync.Mutex
	buckets map[string]*memBucket
	once    map[string]time.Time
	now     func() time.Time
}

// NewMemoryCounter tạo MemoryCounter.
func NewMemoryCounter() *MemoryCounter {
	return &MemoryCounter{buckets: map[string]*memBucket{}, once: map[string]time.Time{}, now: time.Now}
}

func (m *MemoryCounter) reservedOf(b *memBucket, now time.Time) int64 {
	var total int64
	for id, r := range b.reserved {
		if !r.expire.After(now) {
			delete(b.reserved, id) // hết hạn: phần đặt trước tự hoàn
			continue
		}
		total += r.amount
	}
	return total
}

// Reserve hiện thực Counter.
func (m *MemoryCounter) Reserve(_ context.Context, id string, buckets []Bucket, amount int64, ttl time.Duration) (Reserved, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	var missing []int
	for i, b := range buckets {
		if _, ok := m.buckets[b.Key]; !ok {
			missing = append(missing, i)
		}
	}
	if len(missing) > 0 {
		return Reserved{Missing: missing}, nil
	}
	// Đặt lại cùng id thay thế lần đặt trước cũ (lần gọi trước chết giữa chừng rồi được
	// thử lại): phần cũ không được tính hai lần.
	for _, b := range buckets {
		delete(m.buckets[b.Key].reserved, id)
	}
	for i, b := range buckets {
		mb := m.buckets[b.Key]
		if b.Enforce && mb.spent+m.reservedOf(mb, now)+amount > b.Limit {
			return Reserved{Exceeded: i}, nil
		}
	}
	for _, b := range buckets {
		m.buckets[b.Key].reserved[id] = memRes{amount: amount, expire: now.Add(ttl)}
	}
	return Reserved{OK: true}, nil
}

// Seed hiện thực Counter.
func (m *MemoryCounter) Seed(_ context.Context, b Bucket, spent int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.buckets[b.Key]; !ok {
		m.buckets[b.Key] = &memBucket{spent: spent, reserved: map[string]memRes{}}
	}
	return nil
}

// Settle hiện thực Counter.
func (m *MemoryCounter) Settle(_ context.Context, id string, buckets []Bucket, actual int64) ([]int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]int64, len(buckets))
	for i, b := range buckets {
		mb, ok := m.buckets[b.Key]
		if !ok {
			continue // bộ đếm đã mất: lần Reserve sau dựng lại từ ledger, gồm cả lượt này
		}
		delete(mb.reserved, id)
		mb.spent += actual
		out[i] = mb.spent
	}
	return out, nil
}

// Once hiện thực Counter.
func (m *MemoryCounter) Once(_ context.Context, key string, ttl time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if until, ok := m.once[key]; ok && until.After(now) {
		return false, nil
	}
	m.once[key] = now.Add(ttl)
	return true, nil
}

// ---- Redis ----

// Mỗi bucket dùng bốn khoá: <key>:spent (số), <key>:reserved (số), <key>:res (zset
// id → thời điểm hết hạn), <key>:amt (hash id → số tiền). Cả lần đặt trước là một
// script: Redis chạy script nguyên tử.
//
// KEYS: bốn khoá cho mỗi bucket. ARGV: now_ms, expire_ms, id, amount, rồi với mỗi
// bucket: limit, enforce (0/1), expire_at (unix giây).
const reserveScript = `
local now, expire, id, amount = tonumber(ARGV[1]), tonumber(ARGV[2]), ARGV[3], tonumber(ARGV[4])
local n = #KEYS / 4
local missing = {}
for i = 0, n - 1 do
  if redis.call('EXISTS', KEYS[i*4+1]) == 0 then missing[#missing+1] = i end
end
if #missing > 0 then return {-1, unpack(missing)} end
for i = 0, n - 1 do
  local spent, reserved, res, amt = KEYS[i*4+1], KEYS[i*4+2], KEYS[i*4+3], KEYS[i*4+4]
  local own = redis.call('HGET', amt, id)
  if own then
    redis.call('HDEL', amt, id)
    redis.call('ZREM', res, id)
    redis.call('DECRBY', reserved, tonumber(own))
  end
  local stale = redis.call('ZRANGEBYSCORE', res, '-inf', now)
  for _, rid in ipairs(stale) do
    local a = tonumber(redis.call('HGET', amt, rid) or '0')
    redis.call('HDEL', amt, rid)
    redis.call('ZREM', res, rid)
    redis.call('DECRBY', reserved, a)
  end
  local limit, enforce = tonumber(ARGV[5+i*3]), ARGV[6+i*3]
  local used = tonumber(redis.call('GET', spent) or '0') + tonumber(redis.call('GET', reserved) or '0')
  if enforce == '1' and used + amount > limit then return {0, i} end
end
for i = 0, n - 1 do
  local reserved, res, amt = KEYS[i*4+2], KEYS[i*4+3], KEYS[i*4+4]
  local at = tonumber(ARGV[7+i*3])
  redis.call('ZADD', res, expire, id)
  redis.call('HSET', amt, id, amount)
  redis.call('INCRBY', reserved, amount)
  redis.call('EXPIREAT', KEYS[i*4+1], at)
  redis.call('EXPIREAT', reserved, at)
  redis.call('EXPIREAT', res, at)
  redis.call('EXPIREAT', amt, at)
end
return {1}
`

// KEYS: bốn khoá cho mỗi bucket. ARGV: id, actual.
const settleScript = `
local id, actual = ARGV[1], tonumber(ARGV[2])
local n = #KEYS / 4
local out = {}
for i = 0, n - 1 do
  local spent, reserved, res, amt = KEYS[i*4+1], KEYS[i*4+2], KEYS[i*4+3], KEYS[i*4+4]
  local a = redis.call('HGET', amt, id)
  if a then
    redis.call('HDEL', amt, id)
    redis.call('ZREM', res, id)
    redis.call('DECRBY', reserved, tonumber(a))
  end
  if redis.call('EXISTS', spent) == 1 then
    out[i+1] = redis.call('INCRBY', spent, actual)
  else
    out[i+1] = 0
  end
end
return out
`

// RedisCounter là Counter trên Redis, dùng chung cho mọi replica.
type RedisCounter struct {
	rdb     redis.Scripter
	cmd     redis.Cmdable
	reserve *redis.Script
	settle  *redis.Script
}

// NewRedisCounter tạo RedisCounter.
func NewRedisCounter(rdb redis.UniversalClient) *RedisCounter {
	return &RedisCounter{rdb: rdb, cmd: rdb, reserve: redis.NewScript(reserveScript), settle: redis.NewScript(settleScript)}
}

func bucketKeys(buckets []Bucket) []string {
	keys := make([]string, 0, len(buckets)*4)
	for _, b := range buckets {
		keys = append(keys, b.Key+":spent", b.Key+":reserved", b.Key+":res", b.Key+":amt")
	}
	return keys
}

// Reserve hiện thực Counter.
func (r *RedisCounter) Reserve(ctx context.Context, id string, buckets []Bucket, amount int64, ttl time.Duration) (Reserved, error) {
	now := time.Now()
	args := []any{now.UnixMilli(), now.Add(ttl).UnixMilli(), id, amount}
	for _, b := range buckets {
		enforce := 0
		if b.Enforce {
			enforce = 1
		}
		args = append(args, b.Limit, enforce, b.ExpireAt.Unix())
	}
	res, err := r.reserve.Run(ctx, r.rdb, bucketKeys(buckets), args...).Int64Slice()
	if err != nil {
		return Reserved{}, err
	}
	if len(res) == 0 {
		return Reserved{}, errors.New("budget: kết quả đặt trước rỗng")
	}
	switch res[0] {
	case 1:
		return Reserved{OK: true}, nil
	case 0:
		if len(res) < 2 {
			return Reserved{}, errors.New("budget: kết quả đặt trước thiếu chỉ số bucket")
		}
		return Reserved{Exceeded: int(res[1])}, nil
	default:
		out := Reserved{}
		for _, i := range res[1:] {
			out.Missing = append(out.Missing, int(i))
		}
		return out, nil
	}
}

// Seed hiện thực Counter.
func (r *RedisCounter) Seed(ctx context.Context, b Bucket, spent int64) error {
	ttl := time.Until(b.ExpireAt)
	if ttl <= 0 {
		ttl = time.Hour
	}
	return r.cmd.SetNX(ctx, b.Key+":spent", spent, ttl).Err()
}

// Settle hiện thực Counter.
func (r *RedisCounter) Settle(ctx context.Context, id string, buckets []Bucket, actual int64) ([]int64, error) {
	return r.settle.Run(ctx, r.rdb, bucketKeys(buckets), id, actual).Int64Slice()
}

// Once hiện thực Counter.
func (r *RedisCounter) Once(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	return r.cmd.SetNX(ctx, key, 1, ttl).Result()
}
