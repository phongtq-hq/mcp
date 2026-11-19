// Package approval là phê duyệt của con người cho một hành động cụ thể (security §4.4).
//
// Một approval gắn với đúng một hành động: tool call ở (task, step_seq) với đúng tool và
// đúng hash của arguments đã chuẩn hoá. Nó có hạn (hết hạn là từ chối) và dùng một lần.
// Gateway tạo approval khi policy yêu cầu duyệt và tiêu thụ nó ngay trước khi thực thi;
// người duyệt quyết định qua Management API; người duyệt không được là người yêu cầu.
package approval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	pgxv5 "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hqgroups/mcp/internal/audit"
	"github.com/hqgroups/mcp/internal/platform/identity"
	"github.com/hqgroups/mcp/internal/platform/tenancy"
	apperr "github.com/hqgroups/mcp/internal/shared/errors"
	"github.com/hqgroups/mcp/internal/shared/ids"
	"github.com/hqgroups/mcp/internal/workflow/approval/store"
)

// Trạng thái của một approval.
const (
	StatusPending   = "pending"
	StatusApproved  = "approved"
	StatusRejected  = "rejected"
	StatusExpired   = "expired"
	StatusCancelled = "cancelled"
	StatusConsumed  = "consumed"

	// StatusLimit không phải trạng thái lưu: execution đã có quá nhiều approval đang chờ.
	StatusLimit = "limit"
	// maxPendingPerExecution là trần số approval đang chờ của một execution.
	maxPendingPerExecution = 10

	// StatusMismatch không phải trạng thái lưu: approval của bước này là cho một hành
	// động KHÁC (tool hoặc arguments khác). Nó không cho phép hành động đang hỏi.
	StatusMismatch = "mismatch"

	DecisionApprove = "approve"
	DecisionReject  = "reject"

	maxCommentRunes = 2000
)

var (
	errNotFound = apperr.New(apperr.KindNotFound, "not_found", "tài nguyên không tồn tại")
	// ErrNotPending: approval đã được quyết định, đã hết hạn hoặc đã bị huỷ.
	ErrNotPending = apperr.New(apperr.KindConflict, "approval_not_pending", "approval không còn chờ quyết định")
	// ErrAlreadyDecided: mỗi người duyệt quyết định một approval một lần.
	ErrAlreadyDecided = apperr.New(apperr.KindConflict, "already_decided", "bạn đã quyết định approval này rồi")
	// ErrSelfApproval: người yêu cầu không tự duyệt hành động của mình.
	ErrSelfApproval = apperr.New(apperr.KindPermissionDenied, "permission_denied", "người yêu cầu không được tự duyệt hành động của mình")
)

func invalid(msg string) error { return apperr.New(apperr.KindUnprocessable, "validation_failed", msg) }
func internal(err error) error { return apperr.Wrap(err, apperr.KindInternal, "internal_error", "") }

// Decision là một quyết định của người duyệt.
type Decision struct {
	UserID    uuid.UUID `json:"user_id"`
	Decision  string    `json:"decision"`
	Comment   string    `json:"comment,omitempty"`
	DecidedAt time.Time `json:"decided_at"`
}

// Payload là những gì người duyệt thấy.
type Payload struct {
	// Message và PlanKey: approval của một bước workflow (subject_type workflow_step).
	Message    string          `json:"message,omitempty"`
	PlanKey    string          `json:"plan_key,omitempty"`
	ToolName   string          `json:"tool_name"`
	ServerName string          `json:"server_name"`
	Arguments  json.RawMessage `json:"arguments"`
	AgentID    uuid.UUID       `json:"agent_id"`
	// Redactions là số chỗ trong Arguments đã bị che (mẫu secret). Khác 0 nghĩa là người
	// duyệt KHÔNG thấy toàn bộ hành động.
	Redactions int `json:"redactions,omitempty"`
}

// Approval là một yêu cầu phê duyệt.
type Approval struct {
	ID              uuid.UUID
	ProjectID       uuid.UUID
	ExecutionID     *uuid.UUID
	TaskID          *uuid.UUID
	StepSeq         *int
	SubjectType     string
	ToolID          *uuid.UUID
	Payload         Payload
	RiskLevel       string
	Required        int
	Decisions       []Decision
	Status          string
	RequestedByType string
	RequestedBy     *uuid.UUID
	// RequestedByOwner là con người đứng sau người yêu cầu (người tạo API key).
	RequestedByOwner *uuid.UUID
	ExpiresAt        time.Time
	ResolvedAt       *time.Time
	CreatedAt        time.Time
}

// Notifier báo cho nơi đang chờ (vòng lặp của execution) rằng một approval vừa được
// quyết định, để nó không phải đợi tới lần kiểm tra định kỳ.
type Notifier interface {
	ApprovalResolved(ctx context.Context, orgID, executionID, taskID uuid.UUID)
}

// Service quản lý approval.
type Service struct {
	db       *pgxpool.Pool
	notifier Notifier
}

// NewService tạo Service.
func NewService(db *pgxpool.Pool) *Service { return &Service{db: db} }

// WithNotifier đặt nơi nhận thông báo khi approval được quyết định.
func (s *Service) WithNotifier(n Notifier) *Service {
	s.notifier = n
	return s
}

func fromRow(id, projectID uuid.UUID, executionID, taskID *uuid.UUID, stepSeq *int32, subject string, toolID *uuid.UUID,
	payload []byte, risk string, required int32, decisions []byte, status, reqType string, reqBy, reqOwner *uuid.UUID,
	expires time.Time, expired bool, resolved *time.Time, created time.Time) Approval {
	a := Approval{ID: id, ProjectID: projectID, ExecutionID: executionID, TaskID: taskID, SubjectType: subject, ToolID: toolID,
		RiskLevel: risk, Required: int(required), Status: status, RequestedByType: reqType, RequestedBy: reqBy, RequestedByOwner: reqOwner,
		ExpiresAt: expires.UTC(), ResolvedAt: resolved, CreatedAt: created.UTC()}
	if stepSeq != nil {
		v := int(*stepSeq)
		a.StepSeq = &v
	}
	_ = json.Unmarshal(payload, &a.Payload)
	_ = json.Unmarshal(decisions, &a.Decisions)
	// Approval còn pending / approved nhưng đã quá hạn (theo đồng hồ của database) được
	// trình bày là expired: hết hạn là từ chối, kể cả khi chưa ai chạm vào dòng đó.
	if expired && (a.Status == StatusPending || a.Status == StatusApproved) {
		a.Status = StatusExpired
	}
	return a
}

func fromGet(r store.GetApprovalRow) Approval {
	return fromRow(r.ID, r.ProjectID, r.ExecutionID, r.TaskID, r.StepSeq, r.SubjectType, r.ToolID, r.Payload, r.RiskLevel,
		r.RequiredApprovals, r.Decisions, r.Status, r.RequestedByType, r.RequestedBy, r.RequestedByOwner, r.ExpiresAt, r.Expired, r.ResolvedAt, r.CreatedAt)
}

// Request là một tool call cần được duyệt.
type Request struct {
	ProjectID       uuid.UUID
	ExecutionID     uuid.UUID
	TaskID          uuid.UUID
	StepSeq         int
	ToolID          uuid.UUID
	ToolName        string
	ServerName      string
	RiskLevel       string
	ArgsHash        []byte
	Arguments       json.RawMessage // đã che secret; đây là thứ người duyệt thấy
	Redactions      int
	AgentID         uuid.UUID
	RequestedByType string
	RequestedBy     uuid.UUID
	// RequestedByOwner là con người đứng sau người yêu cầu; người này không được tự duyệt.
	RequestedByOwner uuid.UUID
	ExpiresAt        time.Time
}

// State là kết quả của Resolve.
type State struct {
	ID uuid.UUID
	// Status: pending, rejected, expired, cancelled, mismatch, hoặc consumed.
	Status string
	// Granted: hành động được phép thực thi. Đúng khi approval vừa được tiêu thụ ở lần gọi
	// này, hoặc đã được tiêu thụ trước đó cho CHÍNH hành động này (lần gọi trước bị ngắt
	// trước khi thực thi; việc chống chạy trùng thuộc về bên gọi).
	Granted bool
	// Created: approval vừa được tạo ở lần gọi này.
	Created bool
}

// Resolve trả lời câu hỏi "hành động này đã được duyệt chưa" cho một tool call, và
// tạo approval nếu chưa có. Approval đã duyệt được tiêu thụ nguyên tử ngay tại đây:
// nó không dùng được cho hành động thứ hai. Org lấy từ context.
func (s *Service) Resolve(ctx context.Context, r Request) (State, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return State{}, internal(tenancy.ErrNoTenant)
	}
	if !r.ExpiresAt.After(time.Now()) {
		return State{Status: StatusExpired}, nil
	}
	payload, err := json.Marshal(Payload{ToolName: r.ToolName, ServerName: r.ServerName, Arguments: r.Arguments, AgentID: r.AgentID, Redactions: r.Redactions})
	if err != nil {
		return State{}, internal(err)
	}
	var out State
	err = tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) error {
		q := store.New(tx)
		// Trần số approval đang chờ của một execution: một vòng lặp (hoặc một worker) hỏng
		// không dội được yêu cầu vào người duyệt. Chỉ áp cho approval MỚI.
		if _, err := q.LockApprovalForStep(ctx, store.LockApprovalForStepParams{OrgID: orgID, TaskID: &r.TaskID, StepSeq: ptr(int32(r.StepSeq))}); errors.Is(err, pgxv5.ErrNoRows) { //nolint:gosec // step_seq nhỏ
			n, err := q.CountPendingApprovals(ctx, store.CountPendingApprovalsParams{OrgID: orgID, ExecutionID: &r.ExecutionID})
			if err != nil {
				return err
			}
			if n >= maxPendingPerExecution {
				out = State{Status: StatusLimit}
				return nil
			}
		} else if err != nil {
			return err
		}
		id, err := q.InsertApproval(ctx, store.InsertApprovalParams{ID: ids.New(), OrgID: orgID, ProjectID: r.ProjectID,
			ExecutionID: &r.ExecutionID, TaskID: &r.TaskID, StepSeq: ptr(int32(r.StepSeq)), ToolID: &r.ToolID, ToolName: &r.ToolName, //nolint:gosec // step_seq nhỏ
			ArgsHash: r.ArgsHash, Payload: payload, RiskLevel: r.RiskLevel, RequestedByType: r.RequestedByType,
			RequestedBy: &r.RequestedBy, RequestedByOwner: &r.RequestedByOwner, ExpiresAt: r.ExpiresAt})
		if err == nil {
			out = State{ID: id, Status: StatusPending, Created: true}
			return audit.Record(ctx, tx, audit.Event{OrgID: orgID, ProjectID: &r.ProjectID, Action: "approval.request",
				ResourceType: "approval", ResourceID: &id, ExecutionID: &r.ExecutionID, Outcome: audit.OutcomeSuccess,
				Details: map[string]string{"tool": r.ToolName, "risk_level": r.RiskLevel, "task_id": r.TaskID.String()}})
		}
		if !errors.Is(err, pgxv5.ErrNoRows) {
			return err
		}
		// Đã có approval cho bước này: xét nó dưới khoá dòng.
		cur, err := q.LockApprovalForStep(ctx, store.LockApprovalForStepParams{OrgID: orgID, TaskID: &r.TaskID, StepSeq: ptr(int32(r.StepSeq))}) //nolint:gosec // step_seq nhỏ
		if err != nil {
			return err
		}
		out = State{ID: cur.ID, Status: cur.Status}
		// Approval là cho một hành động cụ thể: tool khác, arguments khác, hoặc tool đã được
		// phân loại lại sang mức rủi ro khác với lúc người duyệt xem thì không khớp.
		if cur.ToolID == nil || *cur.ToolID != r.ToolID || !bytes.Equal(cur.ArgsHash, r.ArgsHash) || cur.RiskLevel != r.RiskLevel {
			out.Status = StatusMismatch
			return nil
		}
		expired := cur.Expired
		switch {
		case cur.Status == StatusConsumed:
			out.Granted = true
		case (cur.Status == StatusPending || cur.Status == StatusApproved) && expired:
			out.Status = StatusExpired
			_, err = q.SetApprovalStatus(ctx, store.SetApprovalStatusParams{OrgID: orgID, ID: cur.ID, Status: StatusExpired, FromStatus: cur.Status})
		case cur.Status == StatusApproved:
			n, err := q.SetApprovalStatus(ctx, store.SetApprovalStatusParams{OrgID: orgID, ID: cur.ID, Status: StatusConsumed, FromStatus: StatusApproved})
			if err != nil {
				return err
			}
			if n == 1 {
				out.Status, out.Granted = StatusConsumed, true
				return audit.Record(ctx, tx, audit.Event{OrgID: orgID, ProjectID: &r.ProjectID, Action: "approval.consume",
					ResourceType: "approval", ResourceID: &cur.ID, ExecutionID: &r.ExecutionID, Outcome: audit.OutcomeSuccess,
					Details: map[string]string{"tool": r.ToolName, "task_id": r.TaskID.String()}})
			}
		}
		return err
	})
	if err != nil {
		return State{}, internal(err)
	}
	return out, nil
}

func ptr[T any](v T) *T { return &v }

// Get trả về một approval của tenant.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (Approval, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return Approval{}, internal(tenancy.ErrNoTenant)
	}
	var row store.GetApprovalRow
	err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) (err error) {
		row, err = store.New(tx).GetApproval(ctx, store.GetApprovalParams{OrgID: orgID, ID: id})
		return err
	})
	if errors.Is(err, pgxv5.ErrNoRows) {
		return Approval{}, errNotFound
	}
	if err != nil {
		return Approval{}, internal(err)
	}
	return fromGet(row), nil
}

// ListFilter lọc danh sách approval.
type ListFilter struct {
	ProjectID      *uuid.UUID
	ExecutionID    *uuid.UUID
	Status         *string
	NotRequestedBy *uuid.UUID
	AfterCreated   *time.Time
	AfterID        *uuid.UUID
	Limit          int
}

// List liệt kê approval của organization, mới nhất trước. Trả thêm một dòng so với
// Limit nếu còn trang sau.
func (s *Service) List(ctx context.Context, f ListFilter) ([]Approval, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return nil, internal(tenancy.ErrNoTenant)
	}
	var out []Approval
	err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) error {
		rows, err := store.New(tx).ListApprovals(ctx, store.ListApprovalsParams{OrgID: orgID, ProjectID: f.ProjectID, ExecutionID: f.ExecutionID,
			Status: f.Status, NotRequestedBy: f.NotRequestedBy, AfterCreated: f.AfterCreated, AfterID: f.AfterID, RowLimit: int32(f.Limit + 1)}) //nolint:gosec // limit ≤ 200
		if err != nil {
			return err
		}
		for _, r := range rows {
			out = append(out, fromRow(r.ID, r.ProjectID, r.ExecutionID, r.TaskID, r.StepSeq, r.SubjectType, r.ToolID, r.Payload, r.RiskLevel,
				r.RequiredApprovals, r.Decisions, r.Status, r.RequestedByType, r.RequestedBy, r.RequestedByOwner, r.ExpiresAt, r.Expired, r.ResolvedAt, r.CreatedAt))
		}
		return nil
	})
	if err != nil {
		return nil, internal(err)
	}
	return out, nil
}

// Decide ghi quyết định của người duyệt. Chỉ user quyết định được, người yêu cầu không
// tự duyệt, và approval phải còn pending và chưa hết hạn. Quyền approval:decide do
// handler kiểm trước khi gọi.
func (s *Service) Decide(ctx context.Context, id uuid.UUID, decision, comment string) (Approval, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	p, pok := identity.PrincipalFrom(ctx)
	if !ok || !pok {
		return Approval{}, internal(tenancy.ErrNoTenant)
	}
	if p.Type != identity.PrincipalUser {
		return Approval{}, apperr.New(apperr.KindPermissionDenied, "permission_denied", "chỉ người dùng mới quyết định được approval")
	}
	if decision != DecisionApprove && decision != DecisionReject {
		return Approval{}, invalid("decision phải là approve hoặc reject")
	}
	if utf8.RuneCountInString(comment) > maxCommentRunes || !utf8.ValidString(comment) || bytes.ContainsRune([]byte(comment), 0) {
		return Approval{}, invalid("comment tối đa 2000 ký tự")
	}
	var out Approval
	err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) error {
		q := store.New(tx)
		cur, err := q.LockApproval(ctx, store.LockApprovalParams{OrgID: orgID, ID: id})
		if err != nil {
			return err
		}
		// Người duyệt ≠ người yêu cầu, ở mọi môi trường. Với execution do API key khởi
		// tạo, người yêu cầu là người đã tạo key đó.
		if (cur.RequestedBy != nil && *cur.RequestedBy == p.ID) || (cur.RequestedByOwner != nil && *cur.RequestedByOwner == p.ID) {
			return ErrSelfApproval
		}
		// Người đã viết hành động được duyệt (definition của lần chạy workflow) cũng không tự duyệt.
		if cur.ExcludedApprover != nil && *cur.ExcludedApprover == p.ID {
			return ErrSelfApproval
		}
		if cur.Status != StatusPending || cur.Expired {
			return ErrNotPending
		}
		// Mỗi người duyệt quyết định một lần: approval cần hai người duyệt là hai người khác nhau.
		var prior []Decision
		_ = json.Unmarshal(cur.Decisions, &prior)
		for _, d := range prior {
			if d.UserID == p.ID {
				return ErrAlreadyDecided
			}
		}
		status := StatusApproved
		if decision == DecisionReject {
			status = StatusRejected
		}
		d, err := json.Marshal(Decision{UserID: p.ID, Decision: decision, Comment: comment, DecidedAt: time.Now().UTC()})
		if err != nil {
			return err
		}
		var n int64
		if decision == DecisionApprove && len(prior)+1 < int(cur.RequiredApprovals) {
			// Chưa đủ số người duyệt: ghi quyết định, approval vẫn chờ người kế tiếp.
			n, err = q.AppendApprovalDecision(ctx, store.AppendApprovalDecisionParams{OrgID: orgID, ID: id, Decision: d})
		} else {
			n, err = q.DecideApproval(ctx, store.DecideApprovalParams{OrgID: orgID, ID: id, Status: status, Decision: d})
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotPending
		}
		row, err := q.GetApproval(ctx, store.GetApprovalParams{OrgID: orgID, ID: id})
		if err != nil {
			return err
		}
		out = fromGet(row)
		return audit.Record(ctx, tx, audit.Event{OrgID: orgID, ProjectID: &out.ProjectID, Action: "approval.decide",
			ResourceType: "approval", ResourceID: &id, ExecutionID: out.ExecutionID, Outcome: audit.OutcomeSuccess,
			Details: map[string]string{"decision": decision, "tool": out.Payload.ToolName, "risk_level": out.RiskLevel,
				"subject_type": out.SubjectType, "status": out.Status,
				"decisions": strconv.Itoa(len(out.Decisions)), "required_approvals": strconv.Itoa(out.Required)}})
	})
	switch {
	case errors.Is(err, pgxv5.ErrNoRows):
		return Approval{}, errNotFound
	case errors.Is(err, ErrSelfApproval), errors.Is(err, ErrNotPending), errors.Is(err, ErrAlreadyDecided):
		return Approval{}, err
	case err != nil:
		return Approval{}, internal(err)
	}
	if s.notifier != nil && out.ExecutionID != nil && out.Status != StatusPending {
		taskID := uuid.Nil
		if out.TaskID != nil {
			taskID = *out.TaskID
		}
		s.notifier.ApprovalResolved(ctx, orgID, *out.ExecutionID, taskID)
	}
	return out, nil
}

// CancelForExecution huỷ mọi approval còn chờ hoặc đã duyệt nhưng chưa dùng của một
// execution (execution bị huỷ: không hành động nào của nó còn được thực thi).
func (s *Service) CancelForExecution(ctx context.Context, executionID uuid.UUID) error {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return internal(tenancy.ErrNoTenant)
	}
	err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) error {
		_, err := store.New(tx).CancelApprovalsOfExecution(ctx, store.CancelApprovalsOfExecutionParams{OrgID: orgID, ExecutionID: &executionID})
		return err
	})
	if err != nil {
		return internal(err)
	}
	return nil
}

// Status trả về trạng thái của approval của một task, cho nơi đang chờ nó. Approval
// còn pending / approved nhưng đã quá hạn được báo là expired.
func (s *Service) Status(ctx context.Context, taskID, id uuid.UUID) (string, error) {
	orgID, ok := tenancy.OrgFrom(ctx)
	if !ok {
		return "", internal(tenancy.ErrNoTenant)
	}
	var row store.ApprovalStatusRow
	err := tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) (err error) {
		row, err = store.New(tx).ApprovalStatus(ctx, store.ApprovalStatusParams{OrgID: orgID, ID: id, TaskID: &taskID})
		return err
	})
	if errors.Is(err, pgxv5.ErrNoRows) {
		return "", errNotFound
	}
	if err != nil {
		return "", internal(err)
	}
	if (row.Status == StatusPending || row.Status == StatusApproved) && row.Expired {
		return StatusExpired, nil
	}
	return row.Status, nil
}
