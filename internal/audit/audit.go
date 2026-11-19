// Package audit ghi audit trail của platform (security §9).
//
// Bản ghi được viết trong CÙNG transaction với thay đổi mà nó mô tả: không có
// thay đổi nào được commit mà thiếu audit, và không có audit cho thay đổi đã
// rollback.
//
// Mỗi bản ghi chứa hash của bản ghi trước trong cùng org. Việc ghi đi qua hàm
// audit_append trong database: hàm tự lấy thời điểm, tự tính hash và tự tuần tự
// hoá theo org; role ứng dụng không có quyền ghi trực tiếp vào bảng.
//
// Phạm vi bảo vệ: sửa một bản ghi, xoá hoặc chèn ở giữa chuỗi bị VerifyChain phát
// hiện. KHÔNG phát hiện được: process ghi audit tự thêm một sự kiện sai sự thật,
// và việc cắt đuôi chuỗi bởi người xoá được dòng. Hai việc đó cần neo chuỗi ra
// kho ngoài (Phase 6).
//
// Audit không chứa secret và không chứa nội dung prompt; Details chỉ gồm tên
// trường, mã quy tắc và định danh.
package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"

	"github.com/google/uuid"
	pgxv5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel/trace"

	"github.com/hqgroups/mcp/internal/audit/store"
)

// Loại actor.
const (
	ActorUser   = "user"
	ActorAPIKey = "api_key"
	ActorAgent  = "agent"
	ActorSystem = "system"
)

// Kết quả của hành động.
const (
	OutcomeSuccess = "success"
	OutcomeDenied  = "denied"
	OutcomeError   = "error"
)

// Actor là chủ thể thực hiện hành động.
type Actor struct {
	Type string
	ID   *uuid.UUID
	// IP là địa chỉ kết nối trực tiếp; zero nếu không có. Sau load balancer đây
	// là địa chỉ của proxy cho tới khi có cấu hình proxy tin cậy.
	IP netip.Addr
}

type actorKey struct{}

// WithActor gắn actor vào context. Middleware xác thực gọi hàm này.
func WithActor(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, actorKey{}, a)
}

func actorFrom(ctx context.Context) Actor {
	if a, ok := ctx.Value(actorKey{}).(Actor); ok && a.Type != "" {
		return a
	}
	return Actor{Type: ActorSystem}
}

// Event là một sự kiện cần ghi. Actor và trace lấy từ context; thời điểm là
// đồng hồ của database.
type Event struct {
	OrgID        uuid.UUID
	ProjectID    *uuid.UUID
	Action       string // "project.create", "authz.deny"...
	ResourceType string
	ResourceID   *uuid.UUID
	// ExecutionID gắn sự kiện với một execution (cột thật, dùng để lọc).
	ExecutionID *uuid.UUID
	Outcome     string
	Details     map[string]string
}

// Record ghi một sự kiện vào audit trail trong transaction tx, vốn phải đã đặt
// tenant bằng OrgID của sự kiện. Việc ghi trong một org được tuần tự hoá tới
// khi tx kết thúc, nên gọi Record ở cuối transaction.
func Record(ctx context.Context, tx pgxv5.Tx, ev Event) error {
	if ev.OrgID == uuid.Nil || ev.Action == "" || ev.ResourceType == "" {
		return errors.New("audit: org, action và resource type là bắt buộc")
	}
	switch ev.Outcome {
	case OutcomeSuccess, OutcomeDenied, OutcomeError:
	default:
		return fmt.Errorf("audit: outcome không hợp lệ %q", ev.Outcome)
	}
	details := ev.Details
	if details == nil {
		details = map[string]string{}
	}
	detailsJSON, err := json.Marshal(details)
	if err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	actor := actorFrom(ctx)
	p := store.AppendEventParams{
		OrgID: ev.OrgID, ProjectID: ev.ProjectID, ActorType: actor.Type, ActorID: actor.ID,
		Action: ev.Action, ResourceType: ev.ResourceType, ResourceID: ev.ResourceID,
		Outcome: ev.Outcome, Details: detailsJSON, ExecutionID: ev.ExecutionID,
	}
	if actor.IP.IsValid() {
		ip := actor.IP.Unmap().WithZone("").String() // inet không nhận zone của IPv6 link-local
		p.Ip = &ip
	}
	if sc := trace.SpanContextFromContext(ctx); sc.HasTraceID() {
		t := sc.TraceID().String()
		p.TraceID = &t
	}
	if _, err := store.New(tx).AppendEvent(ctx, p); err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	return nil
}

// ErrChainBroken báo chuỗi hash của một org không còn nguyên vẹn.
var ErrChainBroken = errors.New("audit: hash chain broken")

const sqlstateChainBroken = "HQA01"

// VerifyChain tính lại toàn bộ chuỗi hash của org trong tx (đã đặt tenant) và trả
// về số bản ghi đã kiểm. Lỗi bọc ErrChainBroken nếu có bản ghi bị sửa, hoặc bị
// xoá hay chèn ở giữa chuỗi. Việc kiểm tra chặn ghi audit của org tới khi tx kết thúc.
func VerifyChain(ctx context.Context, tx pgxv5.Tx, orgID uuid.UUID) (int64, error) {
	n, err := store.New(tx).Verify(ctx, orgID)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == sqlstateChainBroken {
			return 0, fmt.Errorf("%w: %s", ErrChainBroken, pgErr.Message)
		}
		return 0, fmt.Errorf("audit: %w", err)
	}
	return n, nil
}
