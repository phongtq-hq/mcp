// Package idempotency hiện thực header Idempotency-Key của Management API:
// gửi lại cùng key và cùng request trả về phản hồi ban đầu; cùng key khác
// request trả 422. Key thuộc về một principal trong một org.
//
// Thứ tự: kiểm quyền → nhận key → chạy handler → lưu phản hồi. Quyền được kiểm
// trước cả khi phát lại, nên người đã mất quyền không nhận lại phản hồi cũ, và
// request không có quyền không để lại dòng nào.
//
// Giới hạn: key được nhận và hoàn tất ở transaction riêng, không cùng transaction
// với thao tác. Nếu process chết sau khi thao tác commit nhưng trước khi key hoàn
// tất, key bị coi là bỏ dở sau abandonedAfter và lần gửi lại sẽ CHẠY LẠI handler.
// Vì vậy chỉ bọc thao tác tự khử trùng (tạo theo slug duy nhất, tạo version trùng
// nội dung trả về version cũ). Thao tác có side effect không tự khử trùng (tạo
// execution) phải hoàn tất key trong cùng transaction trước khi dùng package này.
//
// Không bọc endpoint trả secret một lần: phản hồi được lưu trong database.
package idempotency

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	pgxv5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hqgroups/mcp/internal/platform/idempotency/store"
	"github.com/hqgroups/mcp/internal/platform/identity"
	"github.com/hqgroups/mcp/internal/platform/tenancy"
	apperr "github.com/hqgroups/mcp/internal/shared/errors"
	"github.com/hqgroups/mcp/internal/shared/httpx"
)

const (
	header             = "Idempotency-Key"
	maxKeyLen          = 255
	ttlSecs            = 24 * 60 * 60
	abandonedAfterSecs = 120
	maxStoredBody      = 256 << 10
)

var (
	errConflict   = apperr.New(apperr.KindUnprocessable, "idempotency_conflict", "Idempotency-Key đã được dùng cho một request khác")
	errInProgress = apperr.New(apperr.KindConflict, "idempotency_in_progress", "request với Idempotency-Key này đang được xử lý").AsRetryable()
	errBadKey     = apperr.New(apperr.KindInvalid, "invalid_parameter", "Idempotency-Key phải có từ 1 đến 255 ký tự ASCII in được")
)

// Middleware bọc handler bằng xử lý Idempotency-Key.
type Middleware struct {
	db *pgxpool.Pool
}

// New tạo Middleware.
func New(db *pgxpool.Pool) *Middleware { return &Middleware{db: db} }

var replayedHeaders = []string{"Content-Type", "ETag", "Location"}

func validKey(k string) bool {
	if len(k) == 0 || len(k) > maxKeyLen {
		return false
	}
	for i := 0; i < len(k); i++ {
		if k[i] < 0x21 || k[i] > 0x7e {
			return false
		}
	}
	return true
}

type claim struct {
	orgID, principalID uuid.UUID
	key                string
	at                 time.Time
}

// Wrap trả về handler có idempotency. Phải nằm sau middleware xác thực.
// authorize kiểm quyền của request cho thao tác này và chạy trước mọi thứ khác
// khi có header; request không có header đi thẳng tới next (vốn tự kiểm quyền).
func (m *Middleware) Wrap(authorize func(*http.Request) error, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get(header)
		if key == "" {
			next(w, r)
			return
		}
		if !validKey(key) {
			httpx.WriteError(w, r, errBadKey)
			return
		}
		ctx := r.Context()
		orgID, ok := tenancy.OrgFrom(ctx)
		p, pok := identity.PrincipalFrom(ctx)
		if !ok || !pok {
			httpx.WriteError(w, r, apperr.New(apperr.KindUnauthenticated, "unauthenticated", "thiếu thông tin xác thực"))
			return
		}
		if err := authorize(r); err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		body, err := io.ReadAll(r.Body) // đã bị giới hạn bởi LimitBody
		if err != nil {
			httpx.WriteError(w, r, apperr.New(apperr.KindInvalid, "body_too_large", "thân request vượt giới hạn kích thước"))
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		hash := requestHash(r.Method, r.URL.Path, r.URL.RawQuery, body)
		c := claim{orgID: orgID, principalID: p.ID, key: key}

		var prior *store.GetKeyRow
		claimed := false
		err = tenancy.InTx(ctx, m.db, func(tx pgxv5.Tx) error {
			q := store.New(tx)
			if err := q.DeleteStaleKeys(ctx, store.DeleteStaleKeysParams{
				OrgID: orgID, PrincipalID: p.ID, Key: key, AbandonedAfterSecs: abandonedAfterSecs}); err != nil {
				return err
			}
			at, err := q.ClaimKey(ctx, store.ClaimKeyParams{
				OrgID: orgID, PrincipalID: p.ID, Key: key, RequestHash: hash, TtlSecs: ttlSecs})
			if err == nil {
				c.at, claimed = at, true
				return nil
			}
			if !errors.Is(err, pgxv5.ErrNoRows) {
				return err
			}
			row, err := q.GetKey(ctx, store.GetKeyParams{OrgID: orgID, PrincipalID: p.ID, Key: key})
			if errors.Is(err, pgxv5.ErrNoRows) {
				return nil // chủ key vừa nhả nó: coi như đang xử lý, client thử lại
			}
			if err != nil {
				return err
			}
			prior = &row
			return nil
		})
		if err != nil {
			httpx.WriteError(w, r, apperr.Wrap(err, apperr.KindInternal, "internal_error", ""))
			return
		}
		if !claimed {
			replay(w, r, prior, hash)
			return
		}

		rec := &recorder{ResponseWriter: w, status: http.StatusOK}
		done := false
		defer func() {
			if !done { // handler panic: nhả key để client thử lại được
				m.finish(r, c, nil)
			}
		}()
		next(rec, r)
		done = true
		m.finish(r, c, rec)
	}
}

func requestHash(method, path, query string, body []byte) []byte {
	h := sha256.New()
	h.Write([]byte(method + "\n" + path + "\n" + query + "\n"))
	h.Write(body)
	return h.Sum(nil)
}

func replay(w http.ResponseWriter, r *http.Request, prior *store.GetKeyRow, hash []byte) {
	if prior != nil && !bytes.Equal(prior.RequestHash, hash) {
		httpx.WriteError(w, r, errConflict)
		return
	}
	if prior == nil || prior.Status != "completed" || prior.ResponseCode == nil {
		w.Header().Set("Retry-After", "1")
		httpx.WriteError(w, r, errInProgress)
		return
	}
	var headers map[string]string
	if err := json.Unmarshal(prior.ResponseHeaders, &headers); err != nil {
		httpx.WriteError(w, r, apperr.Wrap(err, apperr.KindInternal, "internal_error", ""))
		return
	}
	for k, v := range headers {
		w.Header().Set(k, v)
	}
	w.Header().Set("Idempotent-Replayed", "true")
	w.WriteHeader(int(*prior.ResponseCode))
	_, _ = w.Write(prior.ResponseBody)
}

// storable cho biết một phản hồi có đáng lưu để phát lại không. Lỗi 5xx có thể
// tạm thời. 401/403/404 phụ thuộc quyền tại thời điểm gọi và không được trở
// thành câu trả lời cố định của key.
func storable(status int) bool {
	switch {
	case status >= http.StatusInternalServerError:
		return false
	case status == http.StatusUnauthorized, status == http.StatusForbidden, status == http.StatusNotFound:
		return false
	}
	return true
}

// finish hoàn tất key với phản hồi đã ghi, hoặc nhả key nếu không có phản hồi
// dùng lại được: lần gửi lại sẽ chạy lại handler.
func (m *Middleware) finish(r *http.Request, c claim, rec *recorder) {
	// Request có thể đã bị huỷ; việc hoàn tất key vẫn phải chạy.
	ctx := tenancy.WithOrg(contextWithoutCancel(r), c.orgID)
	keep := rec != nil && storable(rec.status) && !rec.overflow
	err := tenancy.InTx(ctx, m.db, func(tx pgxv5.Tx) error {
		q := store.New(tx)
		if !keep {
			return q.ReleaseKey(ctx, store.ReleaseKeyParams{
				OrgID: c.orgID, PrincipalID: c.principalID, Key: c.key, ClaimedAt: c.at})
		}
		headers := map[string]string{}
		for _, h := range replayedHeaders {
			if v := rec.Header().Get(h); v != "" {
				headers[h] = v
			}
		}
		hb, err := json.Marshal(headers)
		if err != nil {
			return err
		}
		code := int32(rec.status) //nolint:gosec // mã HTTP
		return q.CompleteKey(ctx, store.CompleteKeyParams{
			OrgID: c.orgID, PrincipalID: c.principalID, Key: c.key, ClaimedAt: c.at,
			ResponseCode: &code, ResponseHeaders: hb, ResponseBody: rec.body.Bytes()})
	})
	if err != nil {
		httpx.LogError(ctx, "idempotency: không hoàn tất được key", err)
	}
}

type recorder struct {
	http.ResponseWriter
	status   int
	body     bytes.Buffer
	overflow bool
}

func (r *recorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.body.Len()+len(b) > maxStoredBody {
		r.overflow = true
	} else {
		r.body.Write(b)
	}
	return r.ResponseWriter.Write(b)
}
