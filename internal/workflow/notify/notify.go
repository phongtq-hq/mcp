// Package notify gửi thông báo của platform tới webhook của organization (security
// §4.7). Sự kiện hiện có: "approval.requested", để người duyệt biết có việc đang chờ.
//
// Hai nửa, hai process:
//   - Service (process api): cấu hình webhook. api sinh khoá ký và chỉ MÃ HOÁ được nó.
//   - Dispatcher (process gateway): nhận outbox, mở khoá ký và gửi qua SSRF guard.
//     Gateway là đường duy nhất ra ngoài có kèm credential.
//
// Outbox (notification_deliveries) được chèn bởi trigger của bảng approvals trong cùng
// transaction tạo approval, nên package này không có hàm "enqueue": không đường tạo
// approval nào quên được.
package notify

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"

	"github.com/google/uuid"
	pgxv5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hqgroups/mcp/internal/audit"
	"github.com/hqgroups/mcp/internal/platform/identity"
	"github.com/hqgroups/mcp/internal/platform/tenancy"
	apperr "github.com/hqgroups/mcp/internal/shared/errors"
	"github.com/hqgroups/mcp/internal/shared/pagination"
	"github.com/hqgroups/mcp/internal/shared/safehttp"
	"github.com/hqgroups/mcp/internal/workflow/notify/store"
)

const (
	// SecretPrefix là dấu nhận biết khoá ký webhook.
	SecretPrefix = "whsec_"
	secretBytes  = 32
	secretKind   = "webhook_signing_key"
	secretName   = "notification-webhook"
	sortCreated  = "-created_at"

	// EventApprovalRequested được gửi khi một approval được tạo.
	EventApprovalRequested = "approval.requested"
)

// Webhook là cấu hình webhook của một organization. Khoá ký không nằm ở đây.
type Webhook struct {
	URL                 string
	Enabled             bool
	ConsecutiveFailures int
	DisabledReason      *string
	CreatedBy           *uuid.UUID
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// Delivery là một lần gửi trong outbox.
type Delivery struct {
	ID             uuid.UUID
	EventType      string
	ApprovalID     uuid.UUID
	Status         string
	Attempts       int
	NextAttemptAt  *time.Time
	LastHTTPStatus *int
	LastErrorCode  *string
	CreatedAt      time.Time
	FinishedAt     *time.Time
}

// SecretWriter là thứ package này cần từ Secrets ở process api: chỉ mã hoá.
type SecretWriter interface {
	Put(ctx context.Context, tx pgxv5.Tx, orgID uuid.UUID, name, kind string, plaintext []byte, createdBy *uuid.UUID) (uuid.UUID, error)
}

// Service quản lý cấu hình webhook (process api).
type Service struct {
	db      *pgxpool.Pool
	secrets SecretWriter // nil: process chưa được cấu hình để lưu secret
	policy  safehttp.Policy
}

// NewService tạo Service.
func NewService(db *pgxpool.Pool, secrets SecretWriter, policy safehttp.Policy) *Service {
	return &Service{db: db, secrets: secrets, policy: policy}
}

var errNotFound = apperr.New(apperr.KindNotFound, "not_found", "organization chưa cấu hình webhook thông báo")

func invalid(msg string) error {
	return apperr.New(apperr.KindUnprocessable, "validation_failed", msg)
}

func fromRow(urlStr string, enabled bool, failures int32, reason *string, by *uuid.UUID, created, updated time.Time) Webhook {
	return Webhook{URL: urlStr, Enabled: enabled, ConsecutiveFailures: int(failures), DisabledReason: reason, CreatedBy: by,
		CreatedAt: created, UpdatedAt: updated}
}

// Put đặt hoặc thay webhook của organization và sinh khoá ký mới. Khoá ký chỉ được
// trả về ở đây. URL được kiểm cả dạng lẫn địa chỉ phân giải được TRƯỚC khi mở
// transaction; lúc gửi gateway kiểm lại (địa chỉ có thể đổi sau khi ghi).
func (s *Service) Put(ctx context.Context, rawURL string) (Webhook, string, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return Webhook{}, "", tenancy.ErrNoTenant
	}
	if s.secrets == nil {
		return Webhook{}, "", invalid("process này chưa được cấu hình để lưu secret (HQ_SECRETS_PUBLIC_KEY)")
	}
	// Chỉ người dùng đặt được webhook: API key bị lộ không được tự chuyển hướng thông
	// báo duyệt về nơi khác.
	p, ok := identity.PrincipalFrom(ctx)
	if !ok || p.Type != identity.PrincipalUser {
		return Webhook{}, "", apperr.New(apperr.KindPermissionDenied, "permission_denied", "chỉ người dùng đặt được webhook thông báo")
	}
	u, err := s.policy.CheckURL(rawURL)
	if err != nil {
		return Webhook{}, "", invalid("url không được chấp nhận: cần https, cổng 443 hoặc 8443, địa chỉ công khai, không kèm thông tin đăng nhập")
	}
	// Query string đi vào log của mọi proxy trên đường và hiện lại cho mọi người đọc
	// được cấu hình: không phải chỗ cho credential.
	if u.RawQuery != "" {
		return Webhook{}, "", invalid("url không được có query string; bên nhận xác thực bằng chữ ký X-HQ-Signature")
	}
	if _, err := s.policy.Check(ctx, u.String()); err != nil {
		if errors.Is(err, safehttp.ErrBlocked) {
			return Webhook{}, "", invalid("url trỏ tới địa chỉ không được phép hoặc không phân giải được")
		}
		return Webhook{}, "", err
	}
	raw := make([]byte, secretBytes)
	if _, err := rand.Read(raw); err != nil {
		return Webhook{}, "", err
	}
	secret := SecretPrefix + base64.RawURLEncoding.EncodeToString(raw)

	var out Webhook
	err = tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) error {
		// Mỗi lần đặt là một version mới của cùng một secret (bản mã bất biến).
		secretID, err := s.secrets.Put(ctx, tx, orgID, secretName, secretKind, []byte(secret), &p.ID)
		if err != nil {
			return err
		}
		row, err := store.New(tx).UpsertWebhook(ctx, store.UpsertWebhookParams{OrgID: orgID, Url: u.String(), SecretID: secretID, CreatedBy: &p.ID})
		if err != nil {
			return err
		}
		out = fromRow(row.Url, row.Enabled, row.ConsecutiveFailures, row.DisabledReason, row.CreatedBy, row.CreatedAt, row.UpdatedAt)
		// Audit ghi host và hash của URL, không ghi cả URL: đường dẫn của webhook thường
		// mang token của bên nhận, nhưng việc đổi đích trên cùng host phải để lại dấu vết.
		sum := sha256.Sum256([]byte(u.String()))
		return audit.Record(ctx, tx, audit.Event{OrgID: orgID, Action: "notification_webhook.put", ResourceType: "notification_webhook",
			ResourceID: &orgID, Outcome: audit.OutcomeSuccess, Details: map[string]string{"host": u.Host, "url_sha256": hex.EncodeToString(sum[:])}})
	})
	if err != nil {
		return Webhook{}, "", err
	}
	return out, secret, nil
}

// Get trả về cấu hình webhook của organization.
func (s *Service) Get(ctx context.Context) (Webhook, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return Webhook{}, tenancy.ErrNoTenant
	}
	var out Webhook
	err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) error {
		row, err := store.New(tx).GetWebhook(ctx, orgID)
		if errors.Is(err, pgxv5.ErrNoRows) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		out = fromRow(row.Url, row.Enabled, row.ConsecutiveFailures, row.DisabledReason, row.CreatedBy, row.CreatedAt, row.UpdatedAt)
		return nil
	})
	return out, err
}

// Delete gỡ webhook. Lặp lại là no-op. Thông báo còn chờ sẽ bị bộ gửi bỏ.
func (s *Service) Delete(ctx context.Context) error {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return tenancy.ErrNoTenant
	}
	if p, ok := identity.PrincipalFrom(ctx); !ok || p.Type != identity.PrincipalUser {
		return apperr.New(apperr.KindPermissionDenied, "permission_denied", "chỉ người dùng gỡ được webhook thông báo")
	}
	return tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) error {
		n, err := store.New(tx).DeleteWebhook(ctx, orgID)
		if err != nil || n == 0 {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{OrgID: orgID, Action: "notification_webhook.delete", ResourceType: "notification_webhook",
			ResourceID: &orgID, Outcome: audit.OutcomeSuccess})
	})
}

// ListDeliveries trả về lịch sử gửi, mới nhất trước.
func (s *Service) ListDeliveries(ctx context.Context, limit int, cursor string) ([]Delivery, string, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return nil, "", tenancy.ErrNoTenant
	}
	c, err := pagination.Decode(cursor, sortCreated)
	if err != nil {
		return nil, "", err
	}
	arg := store.ListDeliveriesParams{OrgID: orgID, RowLimit: int32(limit) + 1} //nolint:gosec // limit đã được chặn trần
	if c != nil {
		t, err := time.Parse(time.RFC3339Nano, c.Value)
		if err != nil {
			return nil, "", pagination.ErrInvalidCursor
		}
		arg.CursorCreatedAt, arg.CursorID = &t, &c.ID
	}
	var out []Delivery
	err = tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) error {
		rows, err := store.New(tx).ListDeliveries(ctx, arg)
		if err != nil {
			return err
		}
		for _, r := range rows {
			d := Delivery{ID: r.ID, EventType: r.EventType, ApprovalID: r.ApprovalID, Status: r.Status, Attempts: int(r.Attempts),
				LastErrorCode: r.LastErrorCode, CreatedAt: r.CreatedAt, FinishedAt: r.FinishedAt}
			if r.Status == "pending" {
				t := r.NextAttemptAt
				d.NextAttemptAt = &t
			}
			if r.LastHttpStatus != nil {
				v := int(*r.LastHttpStatus)
				d.LastHTTPStatus = &v
			}
			out = append(out, d)
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		next = pagination.Encode(pagination.Cursor{Sort: sortCreated, Value: last.CreatedAt.UTC().Format(time.RFC3339Nano), ID: last.ID})
	}
	return out, next, nil
}
