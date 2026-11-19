package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	pgxv5 "github.com/jackc/pgx/v5"

	"github.com/hqgroups/mcp/internal/agent/orchestrator"
	"github.com/hqgroups/mcp/internal/agent/runtime/store"
	"github.com/hqgroups/mcp/internal/ai/provider"
	"github.com/hqgroups/mcp/internal/audit"
	"github.com/hqgroups/mcp/internal/cost/budget"
	"github.com/hqgroups/mcp/internal/platform/tenancy"
	apperr "github.com/hqgroups/mcp/internal/shared/errors"
)

// Delegation (agent-runtime §5.4). Agent không gọi Agent: nó gọi tool nội bộ
// delegate_task, và lời gọi đó là một ĐỀ XUẤT. Quyết định nằm ở hàm
// agent_task_delegate trong database (migration 00023), chạy nguyên tử dưới khoá của
// execution: Agent đích có trong can_delegate_to, độ sâu, chu trình tổ tiên, tổng số
// task, budget lấy từ phần còn lại của task cha. Process chạy vòng lặp không tạo được
// task con bằng đường nào khác.
//
// Mọi thứ task con cần (Agent Version đích, chuỗi model, giới hạn, budget) được kiểm
// và chụp lúc TẠO execution: quyền của một execution cố định khi bắt đầu.
//
// Task con chạy xong rồi task cha mới đi tiếp (tuần tự): mỗi lúc chỉ một vòng lặp của
// chuỗi cha–con hoạt động.

const (
	// ToolDelegate là tên tool nội bộ. Tool của MCP luôn có dạng <server>.<tool>, nên
	// tên không có dấu chấm không trùng được với tool nào của Registry.
	ToolDelegate   = "delegate_task"
	kindDelegation = "delegation"
	// maxDelegationAgents là trần số Agent đích trong ảnh chụp của một execution.
	maxDelegationAgents = 16
)

// delegationPlan là ảnh chụp delegation của một execution sắp được tạo: các bảng
// execution_delegation_sources / execution_delegation_targets (migration 00023).
type delegationPlan struct {
	Sources []delegationSource
	Targets []delegationTarget
	// Unavailable là các Agent đích bị bỏ khỏi ảnh chụp vì không dùng được lúc tạo.
	Unavailable []uuid.UUID
}

type delegationSource struct {
	VersionID     uuid.UUID
	CanDelegateTo []uuid.UUID
	MaxDepth      int
	// BudgetFloor là trần chi phí một lượt model của Agent nguồn: phần nó giữ lại cho
	// chính nó khi giao việc.
	BudgetFloor string
}

type delegationTarget struct {
	AgentID    uuid.UUID
	VersionID  uuid.UUID
	Slug       string
	ModelChain []uuid.UUID
	// Budget là limits.budget của Agent đích (nil = không có); BudgetFloor là trần chi
	// phí của một lượt model của nó: budget của task con không được nhỏ hơn.
	Budget      *string
	BudgetFloor string
	// Spec là cấu hình chạy của task con (Task để trống); TimeoutSeconds là hạn riêng.
	Spec           Spec
	TimeoutSeconds int
}

// floorOf là trần chi phí của một lượt model với cấu hình đã kiểm.
func (s *Service) floorOf(ctx context.Context, p prepared) (string, error) {
	price, err := s.prices.PriceAt(ctx, p.chain[0].ID, time.Now())
	if err != nil {
		return "", err
	}
	return budget.FormatAmount(budget.Estimate(price, 0, p.maxOut)), nil
}

// buildDelegation dựng ảnh chụp delegation cho các task gốc của một execution: mọi
// Agent đích tới được qua can_delegate_to, mỗi Agent được kiểm như một task (thuộc
// project, đang hoạt động, model và giá dùng được). Agent đích không dùng được bị bỏ
// khỏi ảnh chụp (và được ghi vào audit của execution): giao việc cho nó bị từ chối lúc chạy.
func (s *Service) buildDelegation(ctx context.Context, projectID uuid.UUID, roots []prepared) (delegationPlan, error) {
	var plan delegationPlan
	seen := map[uuid.UUID]bool{}  // Agent Version đã xét như một nguồn
	tried := map[uuid.UUID]bool{} // Agent đã xét như một đích
	queue := append([]prepared(nil), roots...)
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		d := p.run.Config.Delegation
		if d == nil || d.CanDelegateTo == nil || len(*d.CanDelegateTo) == 0 || d.MaxDepth == nil || *d.MaxDepth < 1 {
			continue
		}
		if seen[p.run.Version.ID] {
			continue
		}
		seen[p.run.Version.ID] = true
		floor, err := s.floorOf(ctx, p)
		if err != nil {
			return plan, err
		}
		plan.Sources = append(plan.Sources, delegationSource{VersionID: p.run.Version.ID, CanDelegateTo: *d.CanDelegateTo,
			MaxDepth: min(*d.MaxDepth, orchestrator.MaxDelegationDepth), BudgetFloor: floor})
		for _, id := range *d.CanDelegateTo {
			if tried[id] {
				continue
			}
			tried[id] = true
			if len(tried) > maxDelegationAgents {
				return plan, invalid("delegation: các agent có thể được giao việc trong execution này vượt " +
					"trần của platform; thu hẹp can_delegate_to")
			}
			// Agent đích luôn chạy version đang hoạt động của nó.
			tp, err := s.prepare(ctx, projectID, id, nil, false)
			if err != nil {
				switch apperr.KindOf(err) {
				case apperr.KindUnprocessable, apperr.KindConflict, apperr.KindPermissionDenied, apperr.KindNotFound:
					plan.Unavailable = append(plan.Unavailable, id)
					continue
				}
				return plan, err
			}
			floor, err := s.floorOf(ctx, tp)
			if err != nil {
				return plan, err
			}
			t := delegationTarget{AgentID: id, VersionID: tp.run.Version.ID, Slug: tp.run.Agent.Slug, ModelChain: tp.chainIDs(),
				BudgetFloor: floor, Spec: tp.spec(TaskRef{}), TimeoutSeconds: int(tp.timeout / time.Second)}
			if b := tp.run.Config.Limits.Budget; b != nil && b.Currency == "USD" {
				if v, ok := budget.ParseAmount(b.Amount); ok && v > 0 {
					amount := b.Amount
					t.Budget = &amount
				}
			}
			plan.Targets = append(plan.Targets, t)
			queue = append(queue, tp)
		}
	}
	return plan, nil
}

// insert ghi ảnh chụp delegation, trong transaction tạo execution.
func (p delegationPlan) insert(ctx context.Context, q *store.Queries, orgID, executionID uuid.UUID) error {
	for _, src := range p.Sources {
		if err := q.InsertDelegationSource(ctx, store.InsertDelegationSourceParams{OrgID: orgID, ExecutionID: executionID,
			AgentVersionID: src.VersionID, CanDelegateTo: src.CanDelegateTo, MaxDepth: int32(src.MaxDepth), BudgetFloor: src.BudgetFloor}); err != nil { //nolint:gosec // ≤ 5
			return err
		}
	}
	for _, t := range p.Targets {
		if err := q.InsertDelegationTarget(ctx, store.InsertDelegationTargetParams{OrgID: orgID, ExecutionID: executionID,
			AgentID: t.AgentID, AgentVersionID: t.VersionID, Slug: t.Slug, ModelChain: t.ModelChain, Budget: t.Budget,
			BudgetFloor: t.BudgetFloor, Spec: mustJSON(t.Spec), TimeoutSeconds: int32(t.TimeoutSeconds)}); err != nil { //nolint:gosec // thời hạn của agent version
			return err
		}
	}
	return nil
}

// delegationView là vị trí của một task trong cây giao việc và các Agent đích của
// execution, đọc từ database.
type delegationView struct {
	// chain[0] là chính task; các phần tử sau là tổ tiên của nó, gốc ở cuối.
	chain   []store.ListTaskChainRow
	targets []store.ListDelegationTargetsRow
}

// delegationOf đọc vị trí của task trong cây giao việc và ảnh chụp delegation của execution.
func (s *Steps) delegationOf(ctx context.Context, task TaskRef) (delegationView, error) {
	var v delegationView
	err := tenancy.InTx(ctx, s.DB, func(tx pgxv5.Tx) (err error) {
		q := store.New(tx)
		if v.chain, err = q.ListTaskChain(ctx, store.ListTaskChainParams{OrgID: task.OrgID, ID: task.TaskID}); err != nil {
			return err
		}
		if len(v.chain) == 0 || v.chain[0].ExecutionID != task.ExecutionID {
			return errTaskMismatch
		}
		// Task không giao việc được thì không cần đọc danh sách Agent đích.
		if len(v.chain[0].CanDelegateTo) == 0 {
			return nil
		}
		v.targets, err = q.ListDelegationTargets(ctx, store.ListDelegationTargetsParams{OrgID: task.OrgID, ExecutionID: task.ExecutionID})
		return err
	})
	return v, err
}

func (v delegationView) target(agentID uuid.UUID) (store.ListDelegationTargetsRow, bool) {
	for _, t := range v.targets {
		if t.AgentID == agentID {
			return t, true
		}
	}
	return store.ListDelegationTargetsRow{}, false
}

// allowed là các Agent mà task này giao việc được, theo đúng các luật không phụ thuộc
// thời điểm của hàm agent_task_delegate: có trong can_delegate_to của Agent Version nó
// đang chạy và trong ảnh chụp; không phải chính nó hay một tổ tiên (chu trình); và task
// con không vượt max_depth nhỏ nhất của cả chuỗi tổ tiên. Tool chỉ được đưa cho model
// khi có ít nhất một Agent như vậy: model không bị chào một việc chắc chắn bị từ chối.
// Thứ tự theo slug (truy vấn đã sắp) để mô tả tool ổn định giữa các lượt.
func (v delegationView) allowed() []store.ListDelegationTargetsRow {
	if len(v.chain) == 0 || len(v.chain) > orchestrator.MaxDelegationDepth+1 {
		return nil
	}
	self := v.chain[0]
	maxDepth := orchestrator.MaxDelegationDepth
	ancestors := map[uuid.UUID]bool{}
	for _, c := range v.chain {
		maxDepth = min(maxDepth, int(c.MaxDepth))
		ancestors[c.AgentID] = true
	}
	if int(self.Depth)+1 > maxDepth {
		return nil
	}
	var out []store.ListDelegationTargetsRow
	for _, t := range v.targets {
		if slices.Contains(self.CanDelegateTo, t.AgentID) && !ancestors[t.AgentID] {
			out = append(out, t)
		}
	}
	return out
}

// delegateTool là định nghĩa tool delegate_task đưa cho model, hoặc nil nếu task không
// giao việc được cho Agent nào.
func (s *Steps) delegateTool(ctx context.Context, task TaskRef) (*provider.Tool, error) {
	v, err := s.delegationOf(ctx, task)
	if err != nil {
		return nil, err
	}
	targets := v.allowed()
	if len(targets) == 0 {
		return nil, nil
	}
	// Mô tả tool chỉ chứa slug: tên hiển thị của Agent sửa được mà không qua version /
	// publish, nên không được đi vào định nghĩa tool của Agent khác.
	slugs := make([]string, len(targets))
	for i, t := range targets {
		slugs[i] = t.Slug
	}
	schema := mustJSON(map[string]any{
		"type": "object", "additionalProperties": false, "required": []string{"agent", "goal"},
		"properties": map[string]any{
			"agent": map[string]any{"type": "string", "enum": slugs, "description": "Agent nhận việc"},
			"goal":  map[string]any{"type": "string", "description": "Việc cần làm, tự đầy đủ: agent nhận việc không thấy hội thoại này"},
		}})
	return &provider.Tool{Name: ToolDelegate, InputSchema: schema,
		Description: "Giao một việc cho một agent khác và nhận lại tóm tắt kết quả của nó. Agent nhận việc chỉ thấy `goal`. " +
			"Mỗi lần chỉ một việc; việc sau bắt đầu khi việc trước đã xong. Các agent có thể giao việc: " + strings.Join(slugs, ", ") + "."}, nil
}

// delegationMessages là thông báo cho model theo luật đã khớp khi một lần giao việc bị từ chối.
var delegationMessages = map[string]string{
	"task_not_active":           "task không còn chạy",
	"child_running":             "một việc đã giao trước đó chưa xong",
	"delegation_not_configured": "agent này không được giao việc cho agent khác",
	"target_not_allowed":        "agent đích không nằm trong danh sách được giao việc",
	"target_unavailable":        "agent đích hiện không dùng được",
	"max_depth":                 "đã tới độ sâu giao việc tối đa",
	"cycle":                     "agent đích đang chờ kết quả của chính chuỗi giao việc này",
	"task_limit":                "execution đã có số task tối đa",
	"invalid_goal":              "goal trống hoặc quá dài",
	"budget_required":           "không có budget để cấp cho việc được giao",
	"budget_insufficient":       "budget còn lại không đủ cho việc được giao",
	"invalid_arguments":         "arguments của delegate_task không hợp lệ",
}

type delegateArgs struct {
	Agent string `json:"agent"`
	Goal  string `json:"goal"`
}

type delegateResult struct {
	TaskID *uuid.UUID `json:"task_id"`
	Rule   string     `json:"rule"`
}

// delegate xử lý một lời gọi delegate_task: tạo task con (một lần cho mỗi bước), hoặc
// ghi lại việc từ chối; khi task con đã kết thúc thì đóng step với tóm tắt kết quả của
// nó. Trả về DelegatedTaskID khi task con còn phải chạy.
func (s *Steps) delegate(ctx context.Context, in CallToolInput, raw json.RawMessage) (ToolOutcome, error) {
	name := ToolDelegate
	replay, _, err := s.begin(ctx, in.Task, in.StepSeq, store.InsertStepParams{
		Kind: kindDelegation, Status: stepStarted, ToolName: &name, ArgsHash: argsHash(raw), Request: toolRequest(in.Call.ID, raw),
	})
	if errors.Is(err, errStepFinished) {
		// Bước chạy lại sau khi đã đóng: trả lại đúng kết quả đó.
		switch s.stepStatus(ctx, in.Task, in.StepSeq) {
		case stepDenied:
			return ToolOutcome{IsError: true, Denied: true, Bytes: len(msgDenied)}, nil
		case stepFailed:
			return ToolOutcome{IsError: true, Bytes: maxSummaryRunes}, nil
		}
		return ToolOutcome{}, err
	}
	if err != nil {
		return ToolOutcome{}, err
	}
	if replay != nil {
		var res ToolResult
		if err := json.Unmarshal(replay, &res); err != nil {
			return ToolOutcome{}, err
		}
		return ToolOutcome{IsError: res.IsError, Bytes: len(res.Text)}, nil
	}

	fctx, cancel := finishCtx(ctx)
	defer cancel()
	finish := store.FinishStepParams{OrgID: in.Task.OrgID, TaskID: in.Task.TaskID, StepSeq: int32(in.StepSeq), Since: in.Task.Since} //nolint:gosec // step_seq nhỏ
	decidedHere := false
	closeStep := func(status string, res ToolResult, rule string) (ToolOutcome, error) {
		finish.Status = status
		if status == stepDenied {
			finish.Error = mustJSON(stepError{Code: CodeToolDenied, Message: delegationMessages[rule]})
		} else {
			finish.Response = mustJSON(res)
		}
		err := s.inTxRetry(fctx, func(tx pgxv5.Tx) error {
			q := store.New(tx)
			if _, err := q.LockTask(fctx, store.LockTaskParams{OrgID: in.Task.OrgID, ID: in.Task.TaskID}); err != nil {
				return err
			}
			n, err := q.FinishStep(fctx, finish)
			if err != nil || n == 0 {
				return err
			}
			if err := q.AddTaskProgress(fctx, store.AddTaskProgressParams{OrgID: in.Task.OrgID, ID: in.Task.TaskID, ToolCalls: 1}); err != nil {
				return err
			}
			// Từ chối do hàm trong database quyết định đã được chính nó ghi audit. Hai luật
			// dưới đây được quyết định ở đây (arguments hỏng, slug không có trong danh sách
			// đã đưa cho model), trước khi tới hàm.
			if status != stepDenied || !decidedHere {
				return nil
			}
			if _, err := lockExecution(fctx, q, in.Task); err != nil {
				return err // audit ghi người khởi tạo theo tham chiếu: nó phải khớp execution thật
			}
			return audit.Record(fctx, tx, audit.Event{OrgID: in.Task.OrgID, ProjectID: &in.Task.ProjectID, Action: "delegation.deny",
				ResourceType: "task", ResourceID: &in.Task.TaskID, ExecutionID: &in.Task.ExecutionID, Outcome: audit.OutcomeDenied,
				Details: map[string]string{"rule": rule, "step_seq": strconv.Itoa(in.StepSeq)}})
		})
		if err != nil {
			return ToolOutcome{}, err
		}
		return ToolOutcome{IsError: res.IsError || status == stepDenied, Denied: status == stepDenied, Bytes: len(res.Text)}, nil
	}

	// Task con của bước này đã có (bước chạy lại, hoặc lần gọi lại sau khi nó chạy xong)?
	var child store.GetDelegatedTaskRow
	found := true
	err = tenancy.InTx(ctx, s.DB, func(tx pgxv5.Tx) (err error) {
		child, err = store.New(tx).GetDelegatedTask(ctx, store.GetDelegatedTaskParams{OrgID: in.Task.OrgID, ParentTaskID: &in.Task.TaskID, StepSeq: ptr32(in.StepSeq)})
		return err
	})
	if errors.Is(err, pgxv5.ErrNoRows) {
		found, err = false, nil
	}
	if err != nil {
		return ToolOutcome{}, err
	}
	view, err := s.delegationOf(ctx, in.Task)
	if err != nil {
		return ToolOutcome{}, err
	}
	if !found {
		var args delegateArgs
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if dec.Decode(&args) != nil || args.Agent == "" {
			decidedHere = true
			return closeStep(stepDenied, ToolResult{}, "invalid_arguments")
		}
		// Model chọn Agent theo slug trong danh sách đã đưa cho nó; slug khác là từ chối.
		var targetID uuid.UUID
		for _, t := range view.allowed() {
			if t.Slug == args.Agent {
				targetID = t.AgentID
			}
		}
		if targetID == uuid.Nil {
			decidedHere = true
			return closeStep(stepDenied, ToolResult{}, "target_not_allowed")
		}
		// Điểm quyết định: hàm trong database, dưới khoá của execution. Nó tự ghi audit.
		var res delegateResult
		err = tenancy.InTx(ctx, s.DB, func(tx pgxv5.Tx) error {
			out, err := store.New(tx).DelegateTask(ctx, store.DelegateTaskParams{ParentTaskID: in.Task.TaskID, StepSeq: int32(in.StepSeq), //nolint:gosec // step_seq nhỏ
				TargetAgentID: targetID, Goal: truncate(args.Goal, maxDelegationGoalRunes)})
			if err != nil {
				return err
			}
			return json.Unmarshal(out, &res)
		})
		if err != nil {
			return ToolOutcome{}, err
		}
		if res.TaskID == nil {
			if _, known := delegationMessages[res.Rule]; !known {
				slog.WarnContext(ctx, "runtime: delegation bị từ chối với luật không biết", "rule", res.Rule)
				res.Rule = "task_not_active"
			}
			return closeStep(stepDenied, ToolResult{}, res.Rule)
		}
		return ToolOutcome{DelegatedTaskID: res.TaskID.String()}, nil
	}

	if !terminalStatus(child.Status) && child.Status != orchestrator.Skipped {
		return ToolOutcome{DelegatedTaskID: child.ID.String()}, nil
	}
	// Kết quả của task con là output của model khác: dữ liệu không tin cậy, vào hội
	// thoại ở vai tool, trong một khối đánh dấu; chỉ tóm tắt, không phải hội thoại của nó.
	slug := ""
	if t, ok := view.target(child.AgentID); ok {
		slug = t.Slug
	}
	body := "(không có kết quả)"
	if child.OutputSummary != "" {
		body = taskResultTag.ReplaceAllString(child.OutputSummary, "<\\$1")
	}
	text := "<task_result agent=\"" + slug + "\" status=\"" + child.Status + "\">\n" + body + "\n</task_result>"
	status := stepSucceeded
	if child.Status != StatusCompleted {
		status = stepFailed
	}
	return closeStep(status, ToolResult{Text: text, IsError: child.Status != StatusCompleted}, "")
}

// maxDelegationGoalRunes là trần độ dài goal của một lần giao việc.
const maxDelegationGoalRunes = 20_000

func ptr32(n int) *int32 {
	v := int32(n) //nolint:gosec // step_seq nhỏ
	return &v
}

// DelegateSpec dựng cấu hình chạy của một task con từ ảnh chụp trong database. Tham
// chiếu (id của task con) đi qua Temporal: task con phải đúng là con của task cha, và
// mọi thứ nó chạy với lấy từ ảnh chụp, không từ tham chiếu.
func (s *Steps) DelegateSpec(ctx context.Context, in AwaitTaskInput) (PlanTask, error) {
	ctx = taskContext(ctx, in.Task)
	childID, err := uuid.Parse(in.ChildTaskID)
	if err != nil {
		return PlanTask{}, errTaskMismatch
	}
	child := in.Task
	child.TaskID = childID
	var chain []store.ListTaskChainRow
	var targets []store.ListDelegationTargetsRow
	err = tenancy.InTx(ctx, s.DB, func(tx pgxv5.Tx) (err error) {
		q := store.New(tx)
		if chain, err = q.ListTaskChain(ctx, store.ListTaskChainParams{OrgID: in.Task.OrgID, ID: childID}); err != nil {
			return err
		}
		targets, err = q.ListDelegationTargets(ctx, store.ListDelegationTargetsParams{OrgID: in.Task.OrgID, ExecutionID: in.Task.ExecutionID})
		return err
	})
	if err != nil {
		return PlanTask{}, err
	}
	if len(chain) < 2 || chain[0].ExecutionID != in.Task.ExecutionID || chain[0].ParentTaskID == nil || *chain[0].ParentTaskID != in.Task.TaskID {
		return PlanTask{}, errTaskMismatch
	}
	row := chain[0]
	t, ok := delegationView{targets: targets}.target(row.AgentID)
	if !ok || t.AgentVersionID != row.AgentVersionID {
		return PlanTask{}, errTaskMismatch
	}
	var spec Spec
	if err := json.Unmarshal(t.Spec, &spec); err != nil {
		return PlanTask{}, errTaskMismatch
	}
	spec.Task = child
	spec.Task.AgentID, spec.Task.AgentVersionID = row.AgentID, row.AgentVersionID
	if spec.Limits.MaxIterations <= 0 || !slices.Contains(t.ModelChain, spec.Model.ID) {
		return PlanTask{}, errTaskMismatch
	}
	// Hạn của task con: hạn riêng của nó, và không quá hạn của task cha.
	return PlanTask{Spec: spec, OnFailure: orchestrator.Continue, TimeoutSeconds: int(t.TimeoutSeconds), Depth: int(row.Depth)}, nil
}

// AwaitTask chạy task con ngay trong process, tới khi nó kết thúc.
func (e localExecutor) AwaitTask(in AwaitTaskInput) error {
	pt, err := e.steps.DelegateSpec(e.ctx, in)
	if err != nil {
		return err
	}
	spec := pt.Spec
	spec.Task.Deadline = pt.TaskDeadline(time.Now())
	ctx, cancel := context.WithDeadline(e.ctx, spec.Task.Deadline)
	defer cancel()
	out := Outcome{Status: StatusFailed, ErrorCode: CodeInternal, ErrorMessage: "lỗi nội bộ khi chạy task"}
	if err := e.steps.BeginTask(ctx, spec.Task); err != nil {
		slog.WarnContext(ctx, "runtime: không bắt đầu được task con", "error", err, "task_id", spec.Task.TaskID)
		if e.ctx.Err() != nil {
			out = Outcome{Status: StatusCancelled}
		}
	} else {
		out = Run(localExecutor{ctx: ctx, steps: e.steps}, spec)
	}
	// Task con luôn được đóng trước khi task cha đi tiếp.
	return e.steps.FinishTask(context.WithoutCancel(e.ctx), spec.Task, out)
}
