package runtime

import (
	"errors"
	"fmt"
	"testing"

	"github.com/hqgroups/mcp/internal/ai/provider"
)

// fakeExec là Executor chạy theo kịch bản: mỗi lượt model lấy phần tử kế tiếp của turns.
type fakeExec struct {
	turns    []Turn
	modelErr error
	tools    []string
	deny     map[string]bool
	toolErr  map[string]error
	toolSize int

	cancelAt         int // huỷ khi số lượt model đã gọi đạt giá trị này (0 = không)
	cancelNow        bool
	cancelAfterTools int // huỷ khi số tool đã chạy đạt giá trị này (0 = không)
	stopAtModel      int // từ lượt model thứ n trở đi, Stopped trả về stop
	stop             StopKind

	// toolOutcomes: kết quả của từng lần gọi CallTool theo thứ tự (hết thì dùng mặc định).
	toolOutcomes []ToolOutcome
	awaits       []AwaitInput
	awaitErr     error
	cancelOnWait bool
	stopOnWait   StopKind // Stopped trả về giá trị này kể từ khi bước chờ chạy

	listed       bool
	models       []CallModelInput
	auths        []AuthorizeInput
	toolCalls    []CallToolInput
	awaitedTasks []AwaitTaskInput
	// pausedFor: số lần Paused trả true (mỗi lần AwaitResume gỡ một lần).
	pausedFor    int
	resumes      int
	resumeErr    error
	stopOnResume StopKind
}

func (f *fakeExec) AwaitTask(in AwaitTaskInput) error {
	f.awaitedTasks = append(f.awaitedTasks, in)
	return nil
}

func (f *fakeExec) Paused(TaskRef) bool { return f.pausedFor > f.resumes }

func (f *fakeExec) AwaitResume(TaskRef) error {
	f.resumes++
	if f.stopOnResume != NotStopped {
		f.stop, f.stopAtModel = f.stopOnResume, 0
	}
	return f.resumeErr
}

func (f *fakeExec) Stopped() StopKind {
	if f.stop != NotStopped && len(f.models) >= f.stopAtModel {
		return f.stop
	}
	return NotStopped
}

func (f *fakeExec) ListTools(TaskRef) ([]string, error) {
	f.listed = true
	return f.tools, nil
}

func (f *fakeExec) Cancelled(TaskRef) (bool, error) {
	if f.cancelNow || (f.cancelAfterTools > 0 && len(f.toolCalls) >= f.cancelAfterTools) {
		return true, nil
	}
	return f.cancelAt > 0 && len(f.models) >= f.cancelAt, nil
}

func (f *fakeExec) CallModel(in CallModelInput) (Turn, error) {
	n := len(f.models)
	f.models = append(f.models, in)
	if f.modelErr != nil {
		return Turn{}, f.modelErr
	}
	if n >= len(f.turns) {
		// Kịch bản hết: model cứ đòi gọi tool mãi.
		return toolTurn(fmt.Sprintf("loop%d", n), "srv.read"), nil
	}
	return f.turns[n], nil
}

func (f *fakeExec) AuthorizeToolCall(in AuthorizeInput) (Authorization, error) {
	f.auths = append(f.auths, in)
	if f.deny[in.Call.Name] {
		return Authorization{Rule: "risk_write_requires_approval"}, nil
	}
	return Authorization{Allowed: true, Rule: "allowed"}, nil
}

func (f *fakeExec) CallTool(in CallToolInput) (ToolOutcome, error) {
	f.toolCalls = append(f.toolCalls, in)
	if err := f.toolErr[in.Call.Name]; err != nil {
		return ToolOutcome{}, err
	}
	if n := len(f.toolCalls) - 1; n < len(f.toolOutcomes) {
		return f.toolOutcomes[n], nil
	}
	return ToolOutcome{Bytes: f.toolSize}, nil
}

func (f *fakeExec) AwaitApproval(in AwaitInput) error {
	f.awaits = append(f.awaits, in)
	if f.cancelOnWait {
		f.cancelNow = true
	}
	if f.stopOnWait != NotStopped {
		f.stop, f.stopAtModel = f.stopOnWait, 0
	}
	return f.awaitErr
}

func toolTurn(id string, names ...string) Turn {
	t := Turn{StopReason: provider.StopToolUse}
	for i, n := range names {
		t.Calls = append(t.Calls, CallRef{ID: fmt.Sprintf("%s-%d", id, i), Name: n})
	}
	return t
}

func final() Turn { return Turn{StopReason: provider.StopEndTurn, Bytes: 4} }

var readTools = []string{"srv.read", "srv.logs", "srv.write"}

func spec(iter, calls int) Spec {
	return Spec{Limits: Limits{MaxIterations: iter, MaxToolCalls: calls}}
}

func ran(f *fakeExec) string {
	s := ""
	for _, c := range f.toolCalls {
		s += c.Call.Name + " "
	}
	return s
}

func TestRunCompletes(t *testing.T) {
	f := &fakeExec{tools: readTools, turns: []Turn{toolTurn("a", "srv.read", "srv.logs"), final()}}
	out := Run(f, spec(5, 5))
	if out.Status != StatusCompleted || out.Iterations != 2 || out.ToolCalls != 2 || out.FinalStepSeq != 4 {
		t.Fatalf("outcome: %+v", out)
	}
	// step_seq tăng dần, không trùng: model 1, tool 2, tool 3, model 4.
	if f.models[0].StepSeq != 1 || f.models[1].StepSeq != 4 || f.toolCalls[0].StepSeq != 2 || f.toolCalls[1].StepSeq != 3 {
		t.Errorf("step seq: model %d,%d tool %d,%d", f.models[0].StepSeq, f.models[1].StepSeq, f.toolCalls[0].StepSeq, f.toolCalls[1].StepSeq)
	}
	// Mỗi tool call trỏ về đúng lượt model đã yêu cầu nó, bằng id của nó.
	if c := f.toolCalls[1]; c.ModelStepSeq != 1 || c.Call.ID != "a-1" || c.Call.Name != "srv.logs" {
		t.Errorf("tham chiếu tool call: %+v", c)
	}
	// Tập tool cố định từ đầu task được đưa cho mọi lượt model.
	if fmt.Sprint(f.models[1].Tools) != fmt.Sprint(readTools) {
		t.Errorf("tools của lượt hai: %v", f.models[1].Tools)
	}
}

func TestRunLimits(t *testing.T) {
	t.Run("max_iterations", func(t *testing.T) {
		f := &fakeExec{tools: readTools}
		out := Run(f, spec(3, 100))
		if out.Status != StatusFailed || out.ErrorCode != CodeLimitExceeded || out.Iterations != 3 || len(f.models) != 3 || out.FinalStepSeq == 0 {
			t.Errorf("outcome: %+v (%d lượt model)", out, len(f.models))
		}
	})
	t.Run("max_tool_calls", func(t *testing.T) {
		f := &fakeExec{tools: readTools, turns: []Turn{toolTurn("a", "srv.read", "srv.read", "srv.read")}}
		out := Run(f, spec(10, 2))
		if out.Status != StatusFailed || out.ErrorCode != CodeLimitExceeded || len(f.toolCalls) != 2 {
			t.Errorf("outcome: %+v, tool đã chạy: %s", out, ran(f))
		}
	})
	t.Run("context size", func(t *testing.T) {
		f := &fakeExec{tools: readTools, toolSize: 15}
		s := spec(100, 100)
		s.Limits.MaxContextBytes = 60
		out := Run(f, s)
		if out.Status != StatusFailed || out.ErrorCode != CodeLimitExceeded || len(f.toolCalls) > 5 {
			t.Errorf("outcome: %+v sau %d tool call", out, len(f.toolCalls))
		}
	})
	t.Run("deadline", func(t *testing.T) {
		f := &fakeExec{tools: readTools, stop: StopDeadline, stopAtModel: 2}
		out := Run(f, spec(1000, 1000))
		if out.Status != StatusTimedOut || len(f.models) != 2 {
			t.Errorf("outcome: %+v sau %d lượt", out, len(f.models))
		}
	})
	t.Run("deadline while a step is failing", func(t *testing.T) {
		f := &fakeExec{tools: readTools, stop: StopDeadline, stopAtModel: 1, modelErr: errors.New("context deadline exceeded")}
		if out := Run(f, spec(10, 10)); out.Status != StatusTimedOut {
			t.Errorf("outcome: %+v", out)
		}
	})
	t.Run("interrupted", func(t *testing.T) {
		f := &fakeExec{tools: readTools, stop: StopInterrupted, stopAtModel: 2}
		out := Run(f, spec(50, 50))
		if out.Status != StatusFailed || out.ErrorCode != CodeInterrupted {
			t.Errorf("outcome: %+v", out)
		}
	})
	t.Run("cancelled from outside", func(t *testing.T) {
		f := &fakeExec{tools: readTools, stop: StopCancelled, stopAtModel: 1}
		if out := Run(f, spec(50, 50)); out.Status != StatusCancelled || len(f.models) != 1 {
			t.Errorf("outcome: %+v sau %d lượt", out, len(f.models))
		}
	})
	t.Run("cancel", func(t *testing.T) {
		f := &fakeExec{tools: readTools, cancelAt: 2}
		out := Run(f, spec(50, 50))
		if out.Status != StatusCancelled || len(f.models) != 2 {
			t.Errorf("outcome: %+v sau %d lượt", out, len(f.models))
		}
	})
	t.Run("cancel before the first step", func(t *testing.T) {
		f := &fakeExec{tools: readTools, cancelNow: true}
		out := Run(f, spec(50, 50))
		if out.Status != StatusCancelled || f.listed || len(f.models) != 0 {
			t.Errorf("outcome: %+v (listed=%v)", out, f.listed)
		}
	})
	t.Run("cancel between tool calls", func(t *testing.T) {
		f := &fakeExec{tools: readTools, cancelAfterTools: 1, turns: []Turn{toolTurn("a", "srv.read", "srv.logs", "srv.read")}}
		out := Run(f, spec(50, 2))
		if out.Status != StatusCancelled || len(f.toolCalls) != 1 {
			t.Errorf("outcome: %+v, tool đã chạy: %s", out, ran(f))
		}
	})
	t.Run("invalid limits", func(t *testing.T) {
		f := &fakeExec{}
		if out := Run(f, spec(0, 5)); out.Status != StatusFailed || len(f.models) != 0 {
			t.Errorf("outcome: %+v", out)
		}
	})
}

func TestRunToolDecisions(t *testing.T) {
	t.Run("denied call is never executed; the loop goes on", func(t *testing.T) {
		f := &fakeExec{tools: readTools, deny: map[string]bool{"srv.write": true},
			turns: []Turn{toolTurn("a", "srv.write", "srv.read"), final()}}
		out := Run(f, spec(5, 5))
		if out.Status != StatusCompleted || ran(f) != "srv.read " || out.ToolCalls != 2 {
			t.Fatalf("outcome: %+v, tool đã chạy: %s", out, ran(f))
		}
	})
	t.Run("repeated denials stop the task", func(t *testing.T) {
		f := &fakeExec{tools: readTools, deny: map[string]bool{"srv.write": true},
			turns: []Turn{toolTurn("a", "srv.write"), toolTurn("b", "srv.write"), toolTurn("c", "srv.write"), final()}}
		out := Run(f, spec(10, 10))
		if out.Status != StatusFailed || out.ErrorCode != CodeToolDenied || len(f.toolCalls) != 0 {
			t.Errorf("outcome: %+v", out)
		}
	})
	t.Run("tool outside the agent version is denied even if the step would allow it", func(t *testing.T) {
		f := &fakeExec{tools: readTools, turns: []Turn{toolTurn("a", "other.delete_everything"), final()}}
		out := Run(f, spec(5, 5))
		if out.Status != StatusCompleted || len(f.toolCalls) != 0 || len(f.auths) != 1 || f.auths[0].Offered {
			t.Errorf("outcome: %+v, tool đã chạy: %s, auth: %+v", out, ran(f), f.auths)
		}
	})
	t.Run("a tool call that certainly did not run is a result; the loop goes on", func(t *testing.T) {
		f := &fakeExec{tools: readTools, toolOutcomes: []ToolOutcome{{IsError: true, Bytes: 10}},
			turns: []Turn{toolTurn("a", "srv.read"), final()}}
		if out := Run(f, spec(5, 5)); out.Status != StatusCompleted || len(f.models) != 2 {
			t.Fatalf("outcome: %+v", out)
		}
	})
	// Bước CallTool trả LỖI nghĩa là nó không đóng được step: tool có thể đã chạy. Vòng lặp
	// không đi tiếp (model sẽ thấy "chưa thực thi" và có thể lặp lại một hành động có tác dụng phụ).
	t.Run("a tool step that could not be closed stops the task", func(t *testing.T) {
		f := &fakeExec{tools: readTools, toolErr: map[string]error{"srv.write": errors.New("worker lost")},
			turns: []Turn{toolTurn("a", "srv.write", "srv.read"), final()}}
		out := Run(f, spec(5, 5))
		if out.Status != StatusFailed || out.ErrorCode != CodeToolOutcomeUnknown || len(f.models) != 1 || len(f.toolCalls) != 1 {
			t.Fatalf("outcome: %+v, model %d, CallTool %d", out, len(f.models), len(f.toolCalls))
		}
	})
}

func TestRunModelOutcomes(t *testing.T) {
	for name, c := range map[string]struct {
		f      *fakeExec
		status string
		code   string
	}{
		"refusal":                {&fakeExec{turns: []Turn{{StopReason: provider.StopRefusal}}}, StatusFailed, CodeModelRefused},
		"truncated":              {&fakeExec{turns: []Turn{{StopReason: provider.StopMaxTokens}}}, StatusFailed, CodeOutputTruncated},
		"model down":             {&fakeExec{modelErr: errors.New("503 from https://internal.example")}, StatusFailed, CodeModelError},
		"unknown stop reason":    {&fakeExec{turns: []Turn{{StopReason: "pause_turn"}}}, StatusFailed, CodeModelError},
		"empty stop reason":      {&fakeExec{turns: []Turn{{}}}, StatusFailed, CodeModelError},
		"end_turn with calls":    {&fakeExec{tools: readTools, turns: []Turn{{StopReason: provider.StopEndTurn, Calls: []CallRef{{ID: "x", Name: "srv.read"}}}}}, StatusFailed, CodeModelError},
		"truncated tool call":    {&fakeExec{tools: readTools, turns: []Turn{{StopReason: provider.StopMaxTokens, Calls: []CallRef{{ID: "x", Name: "srv.read"}}}}}, StatusFailed, CodeOutputTruncated},
		"tool_use without calls": {&fakeExec{turns: []Turn{{StopReason: provider.StopToolUse}}}, StatusFailed, CodeModelError},
	} {
		out := Run(c.f, spec(5, 5))
		if out.Status != c.status || out.ErrorCode != c.code || len(c.f.toolCalls) != 0 {
			t.Errorf("%s: %+v", name, out)
		}
	}
}

// Lượt model bị platform chặn (budget): task kết thúc ở trạng thái riêng, giữ kết quả
// dở dang là lượt model thành công gần nhất, và không tool nào của lượt bị chặn được chạy.
func TestRunBlockedTurns(t *testing.T) {
	t.Run("budget exceeded on the first turn", func(t *testing.T) {
		f := &fakeExec{tools: readTools, turns: []Turn{{Blocked: BlockedBudget}}}
		out := Run(f, spec(5, 5))
		if out.Status != StatusBudgetExceeded || out.ErrorCode != StatusBudgetExceeded || out.FinalStepSeq != 0 || out.Iterations != 1 ||
			len(f.auths) != 0 || len(f.toolCalls) != 0 {
			t.Errorf("outcome: %+v", out)
		}
	})
	t.Run("budget exceeded after a tool turn keeps the partial result", func(t *testing.T) {
		f := &fakeExec{tools: readTools, turns: []Turn{toolTurn("a", "srv.read"), {Blocked: BlockedBudget}}}
		out := Run(f, spec(5, 5))
		if out.Status != StatusBudgetExceeded || out.FinalStepSeq != 1 || out.ToolCalls != 1 || len(f.models) != 2 || ran(f) != "srv.read " {
			t.Errorf("outcome: %+v, tool đã chạy: %s", out, ran(f))
		}
	})
	t.Run("a blocked turn never runs tool calls it carries", func(t *testing.T) {
		blocked := toolTurn("a", "srv.read")
		blocked.Blocked = BlockedBudget
		f := &fakeExec{tools: readTools, turns: []Turn{blocked}}
		if out := Run(f, spec(5, 5)); out.Status != StatusBudgetExceeded || len(f.toolCalls) != 0 {
			t.Errorf("outcome: %+v, tool đã chạy: %s", out, ran(f))
		}
	})
	t.Run("budget unavailable is a failure with its own code", func(t *testing.T) {
		f := &fakeExec{tools: readTools, turns: []Turn{{Blocked: BlockedBudgetUnavailable}}}
		if out := Run(f, spec(5, 5)); out.Status != StatusFailed || out.ErrorCode != CodeBudgetUnavailable {
			t.Errorf("outcome: %+v", out)
		}
	})
	t.Run("an unknown block reason is not treated as success", func(t *testing.T) {
		f := &fakeExec{tools: readTools, turns: []Turn{{StopReason: provider.StopEndTurn, Blocked: "something"}}}
		if out := Run(f, spec(5, 5)); out.Status != StatusFailed || out.ErrorCode != CodeInternal {
			t.Errorf("outcome: %+v", out)
		}
	})
	// Provider không điều khiển được trạng thái kết thúc: stop reason trùng tên với lý do
	// chặn của platform vẫn chỉ là một stop reason lạ.
	t.Run("a provider stop reason named like a block is just an unknown stop reason", func(t *testing.T) {
		f := &fakeExec{tools: readTools, turns: []Turn{{StopReason: "budget_exceeded"}}}
		if out := Run(f, spec(5, 5)); out.Status != StatusFailed || out.ErrorCode != CodeModelError {
			t.Errorf("outcome: %+v", out)
		}
	})
}

// Tool cần được duyệt: vòng lặp chờ quyết định rồi gọi lại ĐÚNG bước đó; tool không
// chạy lần hai, và quyết định thuộc về bước CallTool (Gateway), không thuộc vòng lặp.
func TestRunApprovals(t *testing.T) {
	wait := ToolOutcome{ApprovalID: "appr-1"}
	t.Run("approved: the same step is called again and the loop goes on", func(t *testing.T) {
		f := &fakeExec{tools: readTools, turns: []Turn{toolTurn("a", "srv.write"), final()},
			toolOutcomes: []ToolOutcome{wait, {Bytes: 5}}}
		out := Run(f, spec(5, 5))
		if out.Status != StatusCompleted || len(f.awaits) != 1 || len(f.toolCalls) != 2 || out.ToolCalls != 1 {
			t.Fatalf("outcome: %+v, awaits %d, CallTool %d", out, len(f.awaits), len(f.toolCalls))
		}
		// Cả hai lần gọi là cùng một bước, và việc chờ gắn với đúng bước và approval đó.
		if f.toolCalls[0].StepSeq != f.toolCalls[1].StepSeq || f.awaits[0].StepSeq != f.toolCalls[0].StepSeq || f.awaits[0].ApprovalID != "appr-1" {
			t.Errorf("bước: gọi %d / %d, chờ %+v", f.toolCalls[0].StepSeq, f.toolCalls[1].StepSeq, f.awaits[0])
		}
		// Lượt model sau dùng step_seq kế tiếp: bước chờ không tiêu thêm số thứ tự.
		if f.models[1].StepSeq != f.toolCalls[0].StepSeq+1 {
			t.Errorf("step_seq của lượt model sau: %d", f.models[1].StepSeq)
		}
	})
	t.Run("rejected: the step is denied, the model sees it, the tool never runs", func(t *testing.T) {
		f := &fakeExec{tools: readTools, turns: []Turn{toolTurn("a", "srv.write"), final()},
			toolOutcomes: []ToolOutcome{wait, {Denied: true, IsError: true}}}
		out := Run(f, spec(5, 5))
		if out.Status != StatusCompleted || len(f.models) != 2 || len(f.awaits) != 1 {
			t.Errorf("outcome: %+v", out)
		}
	})
	t.Run("repeated denials stop the task", func(t *testing.T) {
		denied := ToolOutcome{Denied: true, IsError: true}
		f := &fakeExec{tools: readTools, turns: []Turn{toolTurn("a", "srv.write"), toolTurn("b", "srv.write"), toolTurn("c", "srv.write"), final()},
			toolOutcomes: []ToolOutcome{denied, denied, denied}}
		if out := Run(f, spec(10, 10)); out.Status != StatusFailed || out.ErrorCode != CodeToolDenied {
			t.Errorf("outcome: %+v", out)
		}
	})
	t.Run("cancelled while waiting: the tool is not called again", func(t *testing.T) {
		f := &fakeExec{tools: readTools, turns: []Turn{toolTurn("a", "srv.write"), final()},
			toolOutcomes: []ToolOutcome{wait, {Bytes: 5}}, cancelOnWait: true}
		out := Run(f, spec(5, 5))
		if out.Status != StatusCancelled || len(f.toolCalls) != 1 {
			t.Errorf("outcome: %+v, CallTool %d lần", out, len(f.toolCalls))
		}
	})
	t.Run("deadline while waiting: the task times out and the tool is not called again", func(t *testing.T) {
		f := &fakeExec{tools: readTools, turns: []Turn{toolTurn("a", "srv.write"), final()},
			toolOutcomes: []ToolOutcome{wait, {Bytes: 5}}, stopOnWait: StopDeadline}
		out := Run(f, spec(5, 5))
		if out.Status != StatusTimedOut || len(f.toolCalls) != 1 || len(f.awaits) != 1 {
			t.Errorf("outcome: %+v, CallTool %d lần", out, len(f.toolCalls))
		}
	})
	t.Run("the wait step failing is an internal failure, not a silent approval", func(t *testing.T) {
		f := &fakeExec{tools: readTools, turns: []Turn{toolTurn("a", "srv.write"), final()},
			toolOutcomes: []ToolOutcome{wait, {Bytes: 5}}, awaitErr: errors.New("database unavailable")}
		out := Run(f, spec(5, 5))
		if out.Status != StatusFailed || out.ErrorCode != CodeInternal || len(f.toolCalls) != 1 {
			t.Errorf("outcome: %+v, CallTool %d lần", out, len(f.toolCalls))
		}
	})
	t.Run("a step that still asks for approval after the wait fails the task instead of waiting forever", func(t *testing.T) {
		f := &fakeExec{tools: readTools, turns: []Turn{toolTurn("a", "srv.write"), final()}, toolOutcomes: []ToolOutcome{wait, wait, wait}}
		out := Run(f, spec(5, 5))
		if out.Status != StatusFailed || out.ErrorCode != CodeInternal || len(f.awaits) != 1 || len(f.toolCalls) != 2 {
			t.Errorf("outcome: %+v, awaits %d, CallTool %d", out, len(f.awaits), len(f.toolCalls))
		}
	})
	t.Run("unknown outcome of a write stops the task; nothing else runs", func(t *testing.T) {
		f := &fakeExec{tools: readTools, turns: []Turn{toolTurn("a", "srv.write", "srv.read"), final()},
			toolOutcomes: []ToolOutcome{{Unknown: true, IsError: true}}}
		out := Run(f, spec(5, 5))
		if out.Status != StatusFailed || out.ErrorCode != CodeToolOutcomeUnknown || len(f.toolCalls) != 1 || len(f.models) != 1 {
			t.Errorf("outcome: %+v, CallTool %d, model %d", out, len(f.toolCalls), len(f.models))
		}
	})
}

// Tạm dừng ở ranh giới bước: vòng lặp chờ rồi chạy tiếp đúng chỗ; huỷ hoặc hết hạn trong
// lúc tạm dừng kết thúc task với đúng trạng thái đó, không gọi thêm model.
func TestRunPause(t *testing.T) {
	spec := Spec{Limits: Limits{MaxIterations: 5, MaxToolCalls: 5}}
	t.Run("chờ rồi chạy tiếp", func(t *testing.T) {
		f := &fakeExec{pausedFor: 2, turns: []Turn{{StopReason: provider.StopEndTurn}}}
		out := Run(f, spec)
		if out.Status != StatusCompleted || f.resumes != 2 || len(f.models) != 1 {
			t.Fatalf("%+v, resumes %d, models %d", out, f.resumes, len(f.models))
		}
	})
	t.Run("hết hạn trong lúc tạm dừng", func(t *testing.T) {
		f := &fakeExec{pausedFor: 1, stopOnResume: StopDeadline}
		out := Run(f, spec)
		if out.Status != StatusTimedOut || len(f.models) != 0 {
			t.Fatalf("%+v, models %d", out, len(f.models))
		}
	})
	t.Run("bước chờ lỗi", func(t *testing.T) {
		f := &fakeExec{pausedFor: 1, resumeErr: errors.New("db")}
		out := Run(f, spec)
		if out.Status != StatusFailed || out.ErrorCode != CodeInternal || len(f.models) != 0 {
			t.Fatalf("%+v", out)
		}
	})
	t.Run("tạm dừng mãi có trần", func(t *testing.T) {
		f := &fakeExec{pausedFor: 1 << 30}
		out := Run(f, spec)
		if out.Status != StatusFailed || out.ErrorCode != CodeLimitExceeded || f.resumes != maxPauses {
			t.Fatalf("%+v sau %d lần", out, f.resumes)
		}
	})
}
