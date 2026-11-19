package secrets

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	pgxv5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hqgroups/mcp/internal/audit"
	"github.com/hqgroups/mcp/internal/platform/secrets/store"
	"github.com/hqgroups/mcp/internal/platform/tenancy"
	"github.com/hqgroups/mcp/internal/shared/ids"
)

// Kind của secret.
const KindAPIKey = "api_key"

// Writer ghi secret. Đây là nửa của SecretStore mà process api được giữ.
type Writer struct{ sealer Sealer }

// NewWriter tạo Writer.
func NewWriter(sealer Sealer) *Writer { return &Writer{sealer: sealer} }

// Put mã hoá plaintext và ghi nó thành version mới của secret tên name, trong
// transaction tx (đã đặt tenant là orgID). Trả về id của dòng mới. Ghi audit
// "secret.put" trong cùng transaction; audit không chứa giá trị.
func (w *Writer) Put(ctx context.Context, tx pgxv5.Tx, orgID uuid.UUID, name, kind string, plaintext []byte, createdBy *uuid.UUID) (uuid.UUID, error) {
	if w == nil || w.sealer == nil {
		return uuid.Nil, errors.New("secrets: process này chưa được cấu hình để ghi secret")
	}
	// Tuần tự hoá việc cấp version của cùng một secret: hai lần đổi giá trị đồng thời
	// không cùng nhận một số version.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "secret:"+orgID.String()+"/"+name); err != nil {
		return uuid.Nil, err
	}
	q := store.New(tx)
	version, err := q.NextVersion(ctx, store.NextVersionParams{OrgID: orgID, Name: name})
	if err != nil {
		return uuid.Nil, err
	}
	id := ids.New()
	sealed, err := w.sealer.Seal(Binding{OrgID: orgID, SecretID: id, Version: int(version)}, plaintext)
	if err != nil {
		return uuid.Nil, err
	}
	if err := q.InsertSecret(ctx, store.InsertSecretParams{ID: id, OrgID: orgID, Name: name, Kind: kind, Version: version,
		Ciphertext: sealed.Ciphertext, WrappedDek: sealed.WrappedDEK, KekRef: sealed.KEKRef, CreatedBy: createdBy}); err != nil {
		return uuid.Nil, err
	}
	return id, audit.Record(ctx, tx, audit.Event{OrgID: orgID, Action: "secret.put", ResourceType: "secret", ResourceID: &id,
		Outcome: audit.OutcomeSuccess, Details: map[string]string{"name": name, "kind": kind, "version": itoa(int(version))}})
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// Reader đọc plaintext của secret. Chỉ process gateway giữ Reader.
type Reader struct {
	db     *pgxpool.Pool
	opener Opener

	mu    sync.Mutex
	cache map[cacheKey]cached
}

// cacheKey gồm cả org: bản trong cache chỉ trả cho đúng tenant đã mở nó, kể cả khi
// một người gọi về sau lấy id từ nguồn kém chặt hơn.
type cacheKey struct{ org, id uuid.UUID }

type cached struct {
	value   []byte
	expires time.Time
}

// cacheTTL: plaintext đã mở được giữ trong bộ nhớ trong thời gian ngắn, để mỗi lượt
// gọi model không phải đọc database, mở khoá và ghi audit (ADR-0008). Secret là bất
// biến theo id, nên bản trong cache không bao giờ sai; nó chỉ sống lâu hơn việc thu
// hồi tối đa bằng TTL.
const cacheTTL = 5 * time.Minute

// NewReader tạo Reader.
func NewReader(db *pgxpool.Pool, opener Opener) *Reader {
	return &Reader{db: db, opener: opener, cache: map[cacheKey]cached{}}
}

// ErrNotFound báo secret không tồn tại trong tenant.
var ErrNotFound = errors.New("secrets: secret không tồn tại")

// Get trả về plaintext của secret id trong tenant của ctx. Mỗi lần mở khoá thật (không
// lấy từ cache) ghi audit "secret.access" kèm mục đích; không ghi được audit thì
// không trả secret. Người gọi không được ghi giá trị trả về vào log hay lỗi.
func (r *Reader) Get(ctx context.Context, id uuid.UUID, purpose string) ([]byte, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return nil, tenancy.ErrNoTenant
	}
	now := time.Now()
	r.mu.Lock()
	ck := cacheKey{orgID, id}
	if c, ok := r.cache[ck]; ok && now.Before(c.expires) {
		r.mu.Unlock()
		return c.value, nil
	}
	r.mu.Unlock()

	var plaintext []byte
	err := tenancy.InTx(ctx, r.db, func(tx pgxv5.Tx) error {
		row, err := store.New(tx).GetSealed(ctx, store.GetSealedParams{OrgID: orgID, ID: id})
		if errors.Is(err, pgxv5.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		plaintext, err = r.opener.Open(Binding{OrgID: orgID, SecretID: row.ID, Version: int(row.Version)},
			Sealed{Ciphertext: row.Ciphertext, WrappedDEK: row.WrappedDek, KEKRef: row.KekRef})
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{OrgID: orgID, Action: "secret.access", ResourceType: "secret", ResourceID: &id,
			Outcome: audit.OutcomeSuccess, Details: map[string]string{"purpose": purpose}})
	})
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	if len(r.cache) > 10_000 {
		clear(r.cache)
	}
	r.cache[ck] = cached{value: plaintext, expires: now.Add(cacheTTL)}
	r.mu.Unlock()
	return plaintext, nil
}
