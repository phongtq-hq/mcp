package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	pgxv5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hqgroups/mcp/internal/audit"
	"github.com/hqgroups/mcp/internal/platform/tenancy"
	"github.com/hqgroups/mcp/internal/shared/safehttp"
	"github.com/hqgroups/mcp/internal/workflow/notify/store"
)

const (
	pollInterval     = 2 * time.Second
	claimBatch       = 16
	sendWorkers      = 4
	sendTimeout      = 5 * time.Second
	maxResponseBytes = 4 << 10
	// MaxAttempts khớp với trần trong hàm notification_claim (migration 00035).
	MaxAttempts = 6
	// disableThreshold: số thông báo hỏng hẳn liên tiếp trước khi webhook bị tự tắt.
	disableThreshold = 20

	statusDelivered = "delivered"
	statusFailed    = "failed"
	statusDropped   = "dropped"

	codeBlocked     = "blocked"
	codeTimeout     = "timeout"
	codeConnect     = "connect"
	codeHTTPStatus  = "http_status"
	codeSecret      = "secret_unavailable"
	codeNotConfig   = "not_configured"
	codeApprovalEnd = "approval_closed"
)

// SecretReader là thứ bộ gửi cần từ Secrets ở process gateway.
type SecretReader interface {
	Get(ctx context.Context, id uuid.UUID, purpose string) ([]byte, error)
}

// Dispatcher gửi các thông báo trong outbox (process gateway). Nhiều replica chạy
// cùng lúc được: mỗi dòng được một replica thuê qua notification_claim.
type Dispatcher struct {
	db      *pgxpool.Pool
	secrets SecretReader
	client  *http.Client
	log     *slog.Logger
	now     func() time.Time
}

// NewDispatcher tạo Dispatcher. Mọi request đi qua policy SSRF (kiểm địa chỉ lúc nối,
// không theo redirect, giới hạn kích thước phản hồi).
func NewDispatcher(db *pgxpool.Pool, secrets SecretReader, policy safehttp.Policy, log *slog.Logger) *Dispatcher {
	if log == nil {
		log = slog.Default()
	}
	return &Dispatcher{db: db, secrets: secrets, client: policy.Client(sendTimeout, maxResponseBytes), log: log, now: time.Now}
}

// Run chạy vòng gửi cho tới khi ctx bị huỷ.
func (d *Dispatcher) Run(ctx context.Context) {
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := d.Tick(ctx); err != nil && ctx.Err() == nil {
				d.log.WarnContext(ctx, "notify: không nhận được lô thông báo", "error", err)
			}
		}
	}
}

// Tick nhận một lô và gửi nó; trả về số dòng đã xử lý. Tách khỏi Run để test gọi được
// không cần đồng hồ.
func (d *Dispatcher) Tick(ctx context.Context) (int, error) {
	var rows []store.ClaimDeliveriesRow
	// Transaction này không đặt app.org_id: hàm notification_claim là đường đọc xuyên
	// tenant duy nhất, và nó chỉ trả định danh.
	err := pgxv5.BeginFunc(ctx, d.db, func(tx pgxv5.Tx) error {
		var err error
		rows, err = store.New(tx).ClaimDeliveries(ctx, claimBatch)
		return err
	})
	if err != nil {
		return 0, err
	}
	sem := make(chan struct{}, sendWorkers)
	var wg sync.WaitGroup
	for _, r := range rows {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if err := d.deliver(tenancy.WithOrg(ctx, r.OrgID), r); err != nil && ctx.Err() == nil {
				d.log.WarnContext(ctx, "notify: lần gửi không ghi được kết quả", "org_id", r.OrgID, "delivery_id", r.ID, "error", err)
			}
		}()
	}
	wg.Wait()
	return len(rows), nil
}

type eventBody struct {
	ID             uuid.UUID `json:"id"`
	Type           string    `json:"type"`
	CreatedAt      time.Time `json:"created_at"`
	OrganizationID uuid.UUID `json:"organization_id"`
	Data           eventData `json:"data"`
}

type eventData struct {
	ApprovalID        uuid.UUID  `json:"approval_id"`
	ProjectID         uuid.UUID  `json:"project_id"`
	ExecutionID       *uuid.UUID `json:"execution_id,omitempty"`
	SubjectType       string     `json:"subject_type"`
	RiskLevel         string     `json:"risk_level"`
	RequiredApprovals int        `json:"required_approvals"`
	ExpiresAt         time.Time  `json:"expires_at"`
}

// Sign trả về giá trị header X-HQ-Signature cho thân request tại thời điểm ts.
func Sign(secret []byte, ts time.Time, body []byte) string {
	t := strconv.FormatInt(ts.Unix(), 10)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(t))
	mac.Write([]byte{'.'})
	mac.Write(body)
	return "t=" + t + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

func (d *Dispatcher) deliver(ctx context.Context, row store.ClaimDeliveriesRow) error {
	var (
		wh       store.NotificationWebhook
		ap       store.GetDeliveryApprovalRow
		dropCode string
	)
	err := tenancy.InTx(ctx, d.db, func(tx pgxv5.Tx) error {
		q := store.New(tx)
		var err error
		wh, err = q.GetWebhook(ctx, row.OrgID)
		if errors.Is(err, pgxv5.ErrNoRows) || (err == nil && !wh.Enabled) {
			dropCode = codeNotConfig
			return nil
		}
		if err != nil {
			return err
		}
		ap, err = q.GetDeliveryApproval(ctx, store.GetDeliveryApprovalParams{OrgID: row.OrgID, ID: row.ApprovalID})
		// Approval đã có quyết định, bị huỷ hoặc hết hạn: báo "đang chờ duyệt" lúc này là sai.
		if errors.Is(err, pgxv5.ErrNoRows) || (err == nil && (ap.Status != "pending" || ap.Expired)) {
			dropCode = codeApprovalEnd
			return nil
		}
		return err
	})
	if err != nil {
		return err
	}
	if dropCode != "" {
		return d.finish(ctx, row, statusDropped, nil, &dropCode, false)
	}

	status, code := d.send(ctx, row, wh, ap)
	if code == "" {
		return d.finish(ctx, row, statusDelivered, status, nil, false)
	}
	if int(row.Attempts) >= MaxAttempts {
		return d.finish(ctx, row, statusFailed, status, &code, true)
	}
	return tenancy.InTx(ctx, d.db, func(tx pgxv5.Tx) error {
		_, err := store.New(tx).NoteDeliveryAttempt(ctx, store.NoteDeliveryAttemptParams{OrgID: row.OrgID, ID: row.ID, HttpStatus: status, ErrorCode: &code})
		return err
	})
}

// send thực hiện đúng một request. Trả về mã HTTP (nếu có) và mã lỗi cố định; mã lỗi
// rỗng là thành công. Thông báo lỗi của bên nhận không được giữ lại: nó do bên ngoài
// kiểm soát và có thể mang nội dung nội bộ của họ.
func (d *Dispatcher) send(ctx context.Context, row store.ClaimDeliveriesRow, wh store.NotificationWebhook, ap store.GetDeliveryApprovalRow) (*int32, string) {
	secret, err := d.secrets.Get(ctx, wh.SecretID, "notification_webhook")
	if err != nil {
		return nil, codeSecret
	}
	body, err := json.Marshal(eventBody{ID: row.ID, Type: EventApprovalRequested, CreatedAt: ap.CreatedAt.UTC(), OrganizationID: row.OrgID,
		Data: eventData{ApprovalID: ap.ID, ProjectID: ap.ProjectID, ExecutionID: ap.ExecutionID, SubjectType: ap.SubjectType,
			RiskLevel: ap.RiskLevel, RequiredApprovals: int(ap.RequiredApprovals), ExpiresAt: ap.ExpiresAt.UTC()}})
	if err != nil {
		return nil, codeConnect
	}
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, wh.Url, bytes.NewReader(body))
	if err != nil {
		return nil, codeBlocked
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "mcp-platform-webhook/1")
	req.Header.Set("X-HQ-Event", EventApprovalRequested)
	req.Header.Set("X-HQ-Delivery", row.ID.String())
	req.Header.Set("X-HQ-Signature", Sign(secret, d.now(), body))
	resp, err := d.client.Do(req)
	if err != nil {
		var ne net.Error
		switch {
		case errors.Is(err, safehttp.ErrBlocked):
			return nil, codeBlocked
		case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()):
			return nil, codeTimeout
		default:
			return nil, codeConnect
		}
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
	st := int32(resp.StatusCode) //nolint:gosec // mã HTTP
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return &st, ""
	}
	return &st, codeHTTPStatus
}

// finish đóng một lần gửi và cập nhật bộ đếm lỗi của webhook. Webhook hỏng hẳn
// disableThreshold lần liên tiếp bị tắt, kèm audit: platform không gõ mãi vào một địa
// chỉ đã chết, và quản trị viên thấy lý do.
func (d *Dispatcher) finish(ctx context.Context, row store.ClaimDeliveriesRow, status string, httpStatus *int32, code *string, countFailure bool) error {
	return tenancy.InTx(ctx, d.db, func(tx pgxv5.Tx) error {
		q := store.New(tx)
		if status == statusDelivered || countFailure {
			if err := q.LockWebhook(ctx, row.OrgID); err != nil {
				return err
			}
		}
		n, err := q.FinishDelivery(ctx, store.FinishDeliveryParams{OrgID: row.OrgID, ID: row.ID, Status: status, HttpStatus: httpStatus, ErrorCode: code})
		if err != nil || n == 0 {
			return err
		}
		switch {
		case status == statusDelivered:
			return q.ResetWebhookFailures(ctx, row.OrgID)
		case countFailure:
			w, err := q.BumpWebhookFailures(ctx, store.BumpWebhookFailuresParams{OrgID: row.OrgID, Threshold: disableThreshold})
			if errors.Is(err, pgxv5.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			if !w.Enabled && w.ConsecutiveFailures == disableThreshold {
				if _, err := q.DropPendingDeliveries(ctx, row.OrgID); err != nil {
					return err
				}
				return audit.Record(ctx, tx, audit.Event{OrgID: row.OrgID, Action: "notification_webhook.auto_disable",
					ResourceType: "notification_webhook", ResourceID: &row.OrgID, Outcome: audit.OutcomeSuccess,
					Details: map[string]string{"reason": "delivery_failures", "consecutive_failures": strconv.Itoa(disableThreshold)}})
			}
		}
		return nil
	})
}
