package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	pgxv5 "github.com/jackc/pgx/v5"

	"github.com/hqgroups/mcp/internal/agent/runtime/store"
	"github.com/hqgroups/mcp/internal/platform/identity"
	"github.com/hqgroups/mcp/internal/platform/tenancy"
	apperr "github.com/hqgroups/mcp/internal/shared/errors"
)

// Luồng sự kiện của một execution (Server-Sent Events). Nguồn là database: handler đọc
// trạng thái của execution, các task và các step theo nhịp, và phát một sự kiện cho mỗi
// thay đổi. Không có thành phần hạ tầng mới (broker, LISTEN / NOTIFY): số luồng có trần,
// và mỗi nhịp là vài truy vấn theo index.
//
// Sự kiện chỉ mang định danh và trạng thái: không có prompt, arguments hay kết quả tool
// (những thứ đó cần execution:read_content qua endpoint step). Mỗi lần nối là một lần
// phát lại từ đầu trạng thái hiện tại rồi mới tới thay đổi: không có Last-Event-ID.

var (
	// EventsPoll là nhịp đọc database của một luồng; EventsMaxDuration là thời gian sống
	// tối đa của một luồng (client nối lại sau đó). Là biến để test rút ngắn.
	EventsPoll        = time.Second
	EventsMaxDuration = 5 * time.Minute
)

const (
	eventsHeartbeat    = 15 * time.Second
	eventsWriteTimeout = 10 * time.Second
	// Trần số luồng đồng thời: của cả process, của một organization (một tenant không
	// chiếm hết luồng của process, kể cả bằng nhiều API key) và của một principal.
	maxEventStreams          = 64
	maxEventStreamsPerOrg    = 16
	maxEventStreamsPerCaller = 4
	maxStepsPerPoll          = 500
	// eventsRecheck: khoảng giữa hai lần kiểm lại quyền đọc của người đang nghe luồng.
	eventsRecheck = 30 * time.Second
	minEventsPoll = 10 * time.Millisecond
)

// eventStreams đếm luồng sự kiện đang mở.
type eventStreams struct {
	mu    sync.Mutex
	total int
	byKey map[uuid.UUID]int // theo organization và theo principal (hai không gian id không trùng nhau)
}

func (e *eventStreams) acquire(org, principal uuid.UUID) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.byKey == nil {
		e.byKey = map[uuid.UUID]int{}
	}
	if e.total >= maxEventStreams || e.byKey[org] >= maxEventStreamsPerOrg || e.byKey[principal] >= maxEventStreamsPerCaller {
		return false
	}
	e.total++
	e.byKey[org]++
	e.byKey[principal]++
	return true
}

func (e *eventStreams) release(org, principal uuid.UUID) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.total--
	for _, k := range []uuid.UUID{org, principal} {
		if e.byKey[k]--; e.byKey[k] <= 0 {
			delete(e.byKey, k)
		}
	}
}

// Các sự kiện. Trường là tham chiếu và trạng thái, không có nội dung.
type executionEvent struct {
	ID               uuid.UUID  `json:"id"`
	Status           string     `json:"status"`
	EstimatedCost    string     `json:"estimated_cost"`
	PauseRequestedAt *time.Time `json:"pause_requested_at"`
	CancelRequested  bool       `json:"cancel_requested"`
	FinishedAt       *time.Time `json:"finished_at"`
}

type taskEvent struct {
	ID           uuid.UUID  `json:"id"`
	Status       string     `json:"status"`
	PlanKey      *string    `json:"plan_key"`
	ParentTaskID *uuid.UUID `json:"parent_task_id"`
	AgentID      uuid.UUID  `json:"agent_id"`
	Depth        int32      `json:"depth"`
}

type stepEvent struct {
	TaskID     uuid.UUID  `json:"task_id"`
	StepSeq    int32      `json:"step_seq"`
	Kind       string     `json:"kind"`
	Status     string     `json:"status"`
	ToolName   *string    `json:"tool_name"`
	ApprovalID *uuid.UUID `json:"approval_id"`
}

// eventState là những gì một luồng đã phát: để chỉ phát thay đổi.
type eventState struct {
	execution string
	tasks     map[uuid.UUID]string
	// steps: trạng thái đã phát của từng step; fromSeq: step_seq nhỏ nhất của mỗi task
	// còn có thể đổi (step chưa kết thúc), hoặc step kế tiếp chưa thấy.
	steps   map[uuid.UUID]map[int32]string
	fromSeq map[uuid.UUID]int32
}

type sseEvent struct {
	name string
	data any
}

// pollEvents đọc trạng thái hiện tại và trả về các sự kiện cho những gì đã đổi so với st.
func (s *Service) pollEvents(ctx context.Context, orgID, executionID uuid.UUID, st *eventState) (events []sseEvent, done bool, err error) {
	var exec store.GetExecutionStateRow
	var tasks []store.ListTaskStatesRow
	var steps []store.ListStepStatesRow
	err = tenancy.InTx(ctx, s.db, func(tx pgxv5.Tx) error {
		q := store.New(tx)
		var err error
		if exec, err = q.GetExecutionState(ctx, store.GetExecutionStateParams{OrgID: orgID, ID: executionID}); err != nil {
			return err
		}
		if tasks, err = q.ListTaskStates(ctx, store.ListTaskStatesParams{OrgID: orgID, ExecutionID: executionID}); err != nil {
			return err
		}
		ids := make([]uuid.UUID, len(tasks))
		from := make([]int32, len(tasks))
		for i, t := range tasks {
			ids[i] = t.ID
			if from[i] = st.fromSeq[t.ID]; from[i] < 1 {
				from[i] = 1
			}
		}
		if len(ids) == 0 {
			return nil
		}
		steps, err = q.ListStepStates(ctx, store.ListStepStatesParams{OrgID: orgID, TaskIds: ids, FromSeqs: from,
			Since: exec.CreatedAt.Add(-time.Minute), RowLimit: maxStepsPerPoll})
		return err
	})
	if err != nil {
		return nil, false, err
	}
	for _, t := range tasks {
		if st.tasks[t.ID] != t.Status {
			st.tasks[t.ID] = t.Status
			events = append(events, sseEvent{"task", taskEvent{ID: t.ID, Status: t.Status, PlanKey: t.PlanKey, ParentTaskID: t.ParentTaskID, AgentID: t.AgentID, Depth: t.Depth}})
		}
	}
	for _, sp := range steps {
		seen := st.steps[sp.TaskID]
		if seen == nil {
			seen = map[int32]string{}
			st.steps[sp.TaskID] = seen
		}
		// Step đang chờ duyệt đổi approval_id mà không đổi status: khoá gồm cả hai.
		key := sp.Status
		if sp.ApprovalID != nil {
			key += ":" + sp.ApprovalID.String()
		}
		if seen[sp.StepSeq] != key {
			seen[sp.StepSeq] = key
			events = append(events, sseEvent{"step", stepEvent{TaskID: sp.TaskID, StepSeq: sp.StepSeq, Kind: sp.Kind, Status: sp.Status, ToolName: sp.ToolName, ApprovalID: sp.ApprovalID}})
		}
	}
	// Mốc đọc lần sau của mỗi task: step chưa kết thúc nhỏ nhất, hoặc ngay sau step lớn nhất đã thấy.
	for task, seen := range st.steps {
		next, open := int32(1), int32(0)
		for seq, key := range seen {
			if seq >= next {
				next = seq + 1
			}
			if len(key) >= len(stepStarted) && key[:len(stepStarted)] == stepStarted && (open == 0 || seq < open) {
				open = seq
			}
		}
		if open > 0 {
			next = open
		}
		st.fromSeq[task] = next
	}
	ev := executionEvent{ID: executionID, Status: exec.Status, EstimatedCost: exec.EstimatedCost, PauseRequestedAt: exec.PauseRequestedAt,
		CancelRequested: exec.CancelRequestedAt != nil, FinishedAt: exec.FinishedAt}
	if key := string(mustJSON(ev)); key != st.execution {
		st.execution = key
		// Sự kiện của execution đi SAU task và step của cùng nhịp: khi client thấy trạng thái
		// kết thúc thì mọi thay đổi trước đó đã được phát.
		events = append(events, sseEvent{"execution", ev})
	}
	// Còn step chưa đọc hết (nhịp này chạm trần số dòng): chưa được coi là xong, kể cả
	// khi execution đã kết thúc; nhịp sau đọc tiếp từ mốc mới.
	return events, !active(exec.Status) && len(steps) < maxStepsPerPoll, nil
}

// events là handler của GET /v1/executions/{executionId}/events.
func (h *handlers) events(w http.ResponseWriter, r *http.Request) error {
	e, err := h.load(r)
	if err != nil {
		return err
	}
	orgID, ok := tenancy.OrgFrom(r.Context())
	p, pok := identity.PrincipalFrom(r.Context())
	if !ok || !pok {
		return internal(tenancy.ErrNoTenant)
	}
	if !h.streams.acquire(orgID, p.ID) {
		w.Header().Set("Retry-After", "5")
		return apperr.New(apperr.KindRateLimited, "rate_limited", "đang có quá nhiều luồng sự kiện; thử lại sau").AsRetryable()
	}
	defer h.streams.release(orgID, p.ID)

	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	write := func(format string, args ...any) error {
		// Client đọc chậm không được giữ luồng (và kết nối database của các nhịp sau) vô hạn.
		if err := rc.SetWriteDeadline(time.Now().Add(eventsWriteTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return err
		}
		if _, err := fmt.Fprintf(w, format, args...); err != nil {
			return err
		}
		return rc.Flush()
	}
	ctx, cancel := context.WithTimeout(r.Context(), EventsMaxDuration)
	defer cancel()
	st := &eventState{tasks: map[uuid.UUID]string{}, steps: map[uuid.UUID]map[int32]string{}, fromSeq: map[uuid.UUID]int32{}}
	ticker := time.NewTicker(max(EventsPoll, minEventsPoll))
	defer ticker.Stop()
	lastWrite, lastCheck := time.Now(), time.Now()
	n := 0
	for {
		// Luồng sống nhiều phút: quyền đọc được kiểm lại định kỳ (ngoài transaction đọc),
		// để người bị gỡ quyền không tiếp tục theo dõi execution tới hết thời gian sống của luồng.
		if time.Since(lastCheck) >= eventsRecheck {
			lastCheck = time.Now()
			if err := h.requireHiding(ctx, permExecRead, e.ProjectID); err != nil {
				_ = write("event: error\ndata: {\"code\":\"permission_denied\"}\n\n")
				return nil
			}
		}
		events, done, err := h.svc.pollEvents(ctx, orgID, e.ID, st)
		if err != nil {
			if ctx.Err() == nil {
				// Lỗi đọc: báo cho client rồi đóng luồng; nó nối lại và được phát lại từ đầu.
				_ = write("event: error\ndata: {\"code\":\"internal_error\"}\n\n")
			} else if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				_ = write("event: reconnect\ndata: {}\n\n")
			}
			return nil
		}
		for _, ev := range events {
			n++
			data, _ := json.Marshal(ev.data)
			if err := write("id: %d\nevent: %s\ndata: %s\n\n", n, ev.name, data); err != nil {
				return nil // client đã ngắt
			}
			lastWrite = time.Now()
		}
		if done {
			_ = write("event: end\ndata: {}\n\n")
			return nil
		}
		if time.Since(lastWrite) >= eventsHeartbeat {
			if err := write(": ping\n\n"); err != nil {
				return nil
			}
			lastWrite = time.Now()
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				_ = write("event: reconnect\ndata: {}\n\n")
			}
			return nil
		case <-ticker.C:
		}
	}
}
