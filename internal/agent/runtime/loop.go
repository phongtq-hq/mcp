// Package runtime là Agent Runtime: vòng lặp của một task (agent-runtime §4).
//
// Vòng lặp (Run) là logic thuần và deterministic: nó không đọc đồng hồ, không chạm
// database, không gọi mạng, không dùng goroutine, không duyệt map. Mọi I/O nằm sau
// interface Executor. Cùng một Run chạy theo hai cách:
//
//   - trong process (phát triển cục bộ, test): các bước là lời gọi hàm;
//   - trong một Temporal workflow (ADR-0002): các bước là activity.
//
// Vòng lặp chỉ giữ THAM CHIẾU: số thứ tự step, tên tool, kích thước. Nội dung (prompt,
// arguments, kết quả tool, trả lời của model) nằm trong execution_steps và được từng
// bước tự đọc lại. Nhờ vậy lịch sử workflow không chứa prompt, kết quả tool hay secret.
//
// "LLM đề xuất, platform quyết định": tool call do model yêu cầu là đầu vào không
// tin cậy. Nó đi qua AuthorizeToolCall rồi qua MCP Gateway, nơi quyền được quyết
// định lại. Kết quả tool vào hội thoại ở vai tool, không bao giờ vào system prompt.
package runtime

import (
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/hqgroups/mcp/internal/ai/provider"
)

// Trạng thái kết thúc của một task / execution.
const (
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
	StatusTimedOut  = "timed_out"
	// StatusWaitingApproval không phải trạng thái kết thúc: task đang chờ người duyệt.
	StatusWaitingApproval = "waiting_approval"
	// StatusPaused không phải trạng thái kết thúc: task tạm dừng ở ranh giới bước theo yêu cầu.
	StatusPaused = "paused"
	// StatusBudgetExceeded: một budget chạm ngưỡng cứng; task dừng kèm kết quả dở dang.
	StatusBudgetExceeded = "budget_exceeded"
	// Lý do một lượt model không được gọi (Turn.Blocked).
	BlockedBudget            = "budget_exceeded"    // một budget đã chạm ngưỡng cứng
	BlockedBudgetUnavailable = "budget_unavailable" // không kiểm được budget (fail-closed)
)

// Mã lỗi ổn định của một task thất bại.
const (
	CodeLimitExceeded   = "limit_exceeded"
	CodeToolDenied      = "tool_denied"
	CodeModelRefused    = "model_refused"
	CodeOutputTruncated = "output_truncated"
	CodeModelError      = "model_error"
	CodeInterrupted     = "interrupted"
	// CodeToolOutcomeUnknown: không xác định được một tool ghi đã chạy hay chưa.
	CodeToolOutcomeUnknown = "tool_outcome_unknown"
	// CodeBudgetUnavailable: bộ đếm budget không sẵn sàng quá lâu và có budget chặn cứng.
	CodeBudgetUnavailable = "budget_unavailable"
	CodeInternal          = "internal_error"
)

// maxPauses: tổng số lần một task tạm dừng rồi chạy tiếp. Mỗi lần chờ đã bị chặn bởi
// hạn của task; trần này chặn việc bật tắt tạm dừng liên tục làm lịch sử workflow phình
// không giới hạn.
const maxPauses = 200

// maxDenials: số lần tool bị từ chối trước khi task dừng. Một lần từ chối được trả
// về cho model để nó chọn hướng khác; lặp lại là dấu hiệu model không thoát ra được.
const maxDenials = 3

// DefaultMaxContextBytes là trần hội thoại khi Agent Version không đặt.
const DefaultMaxContextBytes = 4 << 20

// TaskRef định danh một task và ngữ cảnh của nó.
type TaskRef struct {
	OrgID          uuid.UUID `json:"org_id"`
	ProjectID      uuid.UUID `json:"project_id"`
	ExecutionID    uuid.UUID `json:"execution_id"`
	TaskID         uuid.UUID `json:"task_id"`
	AgentID        uuid.UUID `json:"agent_id"`
	AgentVersionID uuid.UUID `json:"agent_version_id"`
	InitiatorType  string    `json:"initiator_type"`
	InitiatorID    uuid.UUID `json:"initiator_id"`
	// Deadline là hạn của task; execution token không sống quá hạn này.
	Deadline time.Time `json:"deadline"`
	// Since là cận dưới thời điểm tạo của mọi step của task (để truy vấn chỉ chạm
	// đúng phân vùng tháng cần thiết).
	Since time.Time `json:"since"`
}

// ModelRef là model đã được chọn cho task.
type ModelRef struct {
	ID            uuid.UUID `json:"id"`
	Adapter       string    `json:"adapter"`
	ProviderModel string    `json:"provider_model"`
}

// Limits là trần của một task. MaxIterations phải dương.
type Limits struct {
	MaxIterations int `json:"max_iterations"`
	MaxToolCalls  int `json:"max_tool_calls"`
	// MaxContextBytes là trần tổng kích thước hội thoại của task (0 = mặc định). Kết
	// quả tool do bên ngoài kiểm soát; không có trần này, một task giữ được hàng GiB
	// và gửi lại toàn bộ cho provider ở mỗi lượt.
	MaxContextBytes int `json:"max_context_bytes"`
}

// Spec là mọi thứ vòng lặp cần để chạy một task. Nó được dựng một lần lúc bắt đầu
// từ Agent Version; không có đường nào để task tự sửa nó. Spec không chứa nội dung:
// system prompt và yêu cầu của người dùng được các bước đọc từ database.
type Spec struct {
	Task  TaskRef  `json:"task"`
	Model ModelRef `json:"model"`
	// Fallbacks là các model dự phòng theo thứ tự, do Model Router chọn lúc tạo task.
	// Tier là tier đã định tuyến; rỗng khi Agent Version ghim model (policy fixed).
	Fallbacks       []ModelRef `json:"fallbacks,omitempty"`
	Tier            string     `json:"tier,omitempty"`
	MaxOutputTokens int        `json:"max_output_tokens"`
	Limits          Limits     `json:"limits"`
	// ContextBudgetBytes là ngân sách hội thoại của task (0 = không nén): vượt nó thì kết
	// quả tool cũ được lược bỏ khi dựng hội thoại cho lượt model.
	ContextBudgetBytes int `json:"context_budget_bytes,omitempty"`
}

// CallModelInput là đầu vào của bước CallModel.
type CallModelInput struct {
	Task    TaskRef  `json:"task"`
	StepSeq int      `json:"step_seq"`
	Model   ModelRef `json:"model"`
	// Fallbacks và Tier: xem Spec.
	Fallbacks       []ModelRef `json:"fallbacks,omitempty"`
	Tier            string     `json:"tier,omitempty"`
	MaxOutputTokens int        `json:"max_output_tokens"`
	// Tools là tên các tool của task, cố định từ lúc bắt đầu. Bước này chỉ đưa cho
	// model những tool có tên trong danh sách và hiện vẫn được Gateway cho phép.
	Tools []string `json:"tools"`
	// ContextBudgetBytes: xem Spec.
	ContextBudgetBytes int `json:"context_budget_bytes,omitempty"`
}

// CallRef là tham chiếu tới một tool call trong một lượt của model. Arguments nằm
// trong step của lượt đó.
type CallRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Turn là kết quả của một lượt model, ở dạng tham chiếu.
type Turn struct {
	StopReason provider.StopReason `json:"stop_reason"`
	Calls      []CallRef           `json:"calls,omitempty"`
	// Bytes là kích thước phần lượt này thêm vào hội thoại.
	Bytes int `json:"bytes"`
	// Blocked khác rỗng: lượt này KHÔNG được gọi, do platform chặn (BlockedBudget…). Đây
	// là trường riêng, không phải một giá trị của StopReason: StopReason đến từ provider
	// và không được phép điều khiển trạng thái kết thúc của task.
	Blocked string `json:"blocked,omitempty"`
}

// AuthorizeInput là đầu vào của bước AuthorizeToolCall.
type AuthorizeInput struct {
	Task    TaskRef `json:"task"`
	StepSeq int     `json:"step_seq"`
	// ModelStepSeq là step của lượt model đã yêu cầu tool call này.
	ModelStepSeq int     `json:"model_step_seq"`
	Call         CallRef `json:"call"`
	// Offered: tool này có trong tập đã đưa cho model hay không.
	Offered bool `json:"offered"`
}

// Authorization là quyết định cho một tool call, kèm quy tắc đã khớp.
type Authorization struct {
	Allowed bool   `json:"allowed"`
	Rule    string `json:"rule"`
}

// CallToolInput là đầu vào của bước CallTool.
type CallToolInput struct {
	Task         TaskRef `json:"task"`
	StepSeq      int     `json:"step_seq"`
	ModelStepSeq int     `json:"model_step_seq"`
	Call         CallRef `json:"call"`
}

// ToolOutcome là kết quả của một tool call, ở dạng tham chiếu.
type ToolOutcome struct {
	IsError bool `json:"is_error"`
	Bytes   int  `json:"bytes"`
	// ApprovalID khác rỗng: tool CHƯA chạy, nó đang chờ approval này. Vòng lặp chờ quyết
	// định rồi gọi lại đúng bước đó.
	ApprovalID string `json:"approval_id,omitempty"`
	// Denied: tool không được chạy (policy từ chối, approval bị từ chối / hết hạn).
	Denied bool `json:"denied,omitempty"`
	// DelegatedTaskID khác rỗng: bước này đã giao việc cho một task con (delegation) và
	// task đó chưa kết thúc. Vòng lặp chạy task con rồi gọi lại đúng bước đó để lấy kết quả.
	DelegatedTaskID string `json:"delegated_task_id,omitempty"`
	// Unknown: không xác định được tool đã chạy hay chưa (lần gọi bị ngắt sau khi đã
	// gửi). Không đoán, không thử lại: task dừng.
	Unknown bool `json:"unknown,omitempty"`
}

// AwaitInput là đầu vào của bước chờ một approval.
type AwaitInput struct {
	Task       TaskRef `json:"task"`
	StepSeq    int     `json:"step_seq"`
	ApprovalID string  `json:"approval_id"`
}

// AwaitTaskInput là đầu vào của bước chạy một task con (delegation) tới khi nó kết thúc.
type AwaitTaskInput struct {
	Task        TaskRef `json:"task"`
	StepSeq     int     `json:"step_seq"`
	ChildTaskID string  `json:"child_task_id"`
}

// StopKind cho biết vì sao các bước không chạy tiếp được.
type StopKind int

const (
	// NotStopped: task vẫn được chạy.
	NotStopped StopKind = iota
	// StopDeadline: task đã quá hạn.
	StopDeadline
	// StopInterrupted: môi trường chạy task đang dừng (process tắt).
	StopInterrupted
	// StopCancelled: task bị huỷ từ bên ngoài.
	StopCancelled
)

// Executor là các bước có I/O của vòng lặp. Mỗi bước idempotent theo
// (task_id, step_seq): gọi lại với cùng step_seq không tạo step hay usage trùng.
// Executor mang ngữ cảnh chạy của riêng nó (context của goroutine, hoặc của workflow).
type Executor interface {
	// ListTools trả về tên các tool của task: cấu hình Agent Version ∩ quyền người
	// khởi tạo ∩ trạng thái tool trong Registry. Tập này cố định cho cả task.
	ListTools(task TaskRef) ([]string, error)
	// Cancelled cho biết đã có yêu cầu huỷ.
	Cancelled(task TaskRef) (bool, error)
	CallModel(in CallModelInput) (Turn, error)
	AuthorizeToolCall(in AuthorizeInput) (Authorization, error)
	CallTool(in CallToolInput) (ToolOutcome, error)
	// AwaitApproval chờ tới khi approval của một tool call có quyết định, hết hạn, hoặc
	// task bị dừng. Trong lúc chờ, task ở trạng thái waiting_approval. Quyết định không
	// được trả về đây: Gateway là nơi xét nó khi bước CallTool được gọi lại.
	AwaitApproval(in AwaitInput) error
	// AwaitTask chạy task con mà một bước delegation vừa tạo, tới khi nó kết thúc hoặc
	// task cha bị dừng. Kết quả của task con không được trả về đây: bước CallTool được
	// gọi lại và đọc nó từ database.
	AwaitTask(in AwaitTaskInput) error
	// Paused cho biết execution đang có yêu cầu tạm dừng. Vòng lặp chỉ hỏi nó ngay sau
	// Cancelled, ở ranh giới bước. (Trên Temporal hai yêu cầu được đọc trong một lần; trong
	// process đây là một lần đọc database riêng.)
	Paused(task TaskRef) bool
	// AwaitResume giữ task ở trạng thái paused tới khi yêu cầu tạm dừng được gỡ, execution
	// bị huỷ, hoặc task bị dừng.
	AwaitResume(task TaskRef) error
	// Stopped cho biết task có đang bị dừng từ bên ngoài hay không. Vòng lặp hỏi nó
	// trước mỗi bước và khi một bước lỗi, để phân biệt hết hạn / bị ngắt / bị huỷ với lỗi thật.
	Stopped() StopKind
}

// Outcome là kết quả của một task. Trả lời cuối của model nằm trong step FinalStepSeq.
type Outcome struct {
	Status       string `json:"status"`
	ErrorCode    string `json:"error_code,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
	Iterations   int    `json:"iterations"`
	ToolCalls    int    `json:"tool_calls"`
	// FinalStepSeq là step của lượt model gần nhất (0 nếu chưa có lượt nào): nơi lấy
	// trả lời cuối, hoặc phần dở dang khi task dừng vì giới hạn.
	FinalStepSeq int `json:"final_step_seq"`
}

// Run chạy vòng lặp của một task tới khi dừng. Run luôn trả về một Outcome ở trạng
// thái kết thúc và không bao giờ lặp vô hạn: mỗi vòng tiêu một iteration, và
// iteration có trần.
//
// Run cũng là thân của workflow trên Temporal (package durable): nó phải deterministic
// (không đồng hồ, không random, không goroutine, không duyệt map), và mọi thay đổi về
// thứ tự hay số lần gọi Executor là thay đổi workflow, cần versioning và lịch sử mẫu
// mới cho test replay.
func Run(ex Executor, spec Spec) Outcome {
	out := Outcome{}
	fail := func(code, msg string) Outcome {
		out.Status, out.ErrorCode, out.ErrorMessage = StatusFailed, code, msg
		return out
	}
	// halted trả về kết quả nếu task đang bị dừng từ bên ngoài.
	halted := func() (Outcome, bool) {
		switch ex.Stopped() {
		case StopDeadline:
			out.Status, out.ErrorCode, out.ErrorMessage = StatusTimedOut, "", "task vượt thời hạn"
			return out, true
		case StopInterrupted:
			return fail(CodeInterrupted, "task bị ngắt trước khi xong"), true
		case StopCancelled:
			out.Status = StatusCancelled
			return out, true
		}
		return out, false
	}
	// stopped ánh xạ lỗi của một bước sang trạng thái kết thúc.
	stopped := func(code string) Outcome {
		if o, ok := halted(); ok {
			return o
		}
		// Yêu cầu huỷ làm các bước đang chạy thất bại (Gateway từ chối task đã bị huỷ):
		// đó là huỷ, không phải lỗi.
		if c, err := ex.Cancelled(spec.Task); err == nil && c {
			out.Status = StatusCancelled
			return out
		}
		// Chi tiết lỗi nằm trong step và log; kết quả của task chỉ mang thông báo chung.
		if code == CodeModelError {
			return fail(code, "không gọi được model")
		}
		return fail(code, "lỗi nội bộ khi chạy task")
	}
	if spec.Limits.MaxIterations <= 0 || spec.Limits.MaxToolCalls < 0 {
		return fail(CodeInternal, "giới hạn của task không hợp lệ")
	}
	maxContext := spec.Limits.MaxContextBytes
	if maxContext <= 0 {
		maxContext = DefaultMaxContextBytes
	}

	pauses := 0
	// Huỷ được kiểm trước mọi bước có I/O, kể cả bước đầu tiên: execution bị huỷ ngay
	// sau khi tạo phải kết thúc là cancelled, không phải lỗi.
	cancelled := func() (Outcome, bool) {
		if o, ok := halted(); ok {
			return o, true
		}
		c, err := ex.Cancelled(spec.Task)
		if err != nil {
			return stopped(CodeInternal), true
		}
		if c {
			out.Status = StatusCancelled
			return out, true
		}
		// Tạm dừng ở ranh giới bước: bước trước đã ghi xong, bước sau chưa bắt đầu. Sau khi
		// chạy tiếp, yêu cầu huỷ và hạn được kiểm lại trước khi làm gì khác.
		for ; ex.Paused(spec.Task); pauses++ {
			if pauses >= maxPauses {
				return fail(CodeLimitExceeded, "task tạm dừng và chạy tiếp quá nhiều lần"), true
			}
			if err := ex.AwaitResume(spec.Task); err != nil {
				return stopped(CodeInternal), true
			}
			if o, ok := halted(); ok {
				return o, true
			}
			c, err := ex.Cancelled(spec.Task)
			if err != nil {
				return stopped(CodeInternal), true
			}
			if c {
				out.Status = StatusCancelled
				return out, true
			}
		}
		return out, false
	}
	if o, ok := cancelled(); ok {
		return o
	}
	tools, err := ex.ListTools(spec.Task)
	if err != nil {
		return stopped(CodeInternal)
	}
	offered := func(name string) bool {
		for _, t := range tools {
			if t == name {
				return true
			}
		}
		return false
	}

	seq, denials, contextBytes := 0, 0, 0
	for {
		// 1. Giới hạn và yêu cầu dừng, trước mỗi lượt gọi model.
		if o, ok := cancelled(); ok {
			return o
		}
		if out.Iterations >= spec.Limits.MaxIterations {
			return fail(CodeLimitExceeded, fmt.Sprintf("vượt max_iterations (%d)", spec.Limits.MaxIterations))
		}
		out.Iterations++

		// 2. Một lượt của model. Bước này tự dựng hội thoại từ các step trước.
		seq++
		modelSeq := seq
		turn, err := ex.CallModel(CallModelInput{Task: spec.Task, StepSeq: modelSeq, Model: spec.Model,
			Fallbacks: spec.Fallbacks, Tier: spec.Tier,
			MaxOutputTokens: spec.MaxOutputTokens, Tools: tools, ContextBudgetBytes: spec.ContextBudgetBytes})
		if err != nil {
			return stopped(CodeModelError)
		}
		// Lượt không được gọi vì budget: task dừng ở trạng thái riêng, và kết quả dở dang
		// là lượt model thành công gần nhất (FinalStepSeq giữ nguyên).
		switch turn.Blocked {
		case "":
		case BlockedBudget:
			out.Status, out.ErrorCode, out.ErrorMessage = StatusBudgetExceeded, StatusBudgetExceeded, "task vượt budget"
			return out
		case BlockedBudgetUnavailable:
			return fail(CodeBudgetUnavailable, "không kiểm tra được budget")
		default:
			return fail(CodeInternal, "lượt model bị chặn với lý do không xác định")
		}
		out.FinalStepSeq = modelSeq
		contextBytes += turn.Bytes
		// Yêu cầu huỷ đến trong lúc model đang trả lời: lượt đó đã được ghi (và tính tiền),
		// nhưng task kết thúc là cancelled, không phải completed.
		if ex.Stopped() == StopCancelled {
			out.Status = StatusCancelled
			return out
		}

		// 3. Model dừng. Chỉ end_turn mới là trả lời cuối; lý do lạ không bao giờ được
		// coi là thành công, và lượt bị cắt không được chạy tool call (có thể đã bị cắt dở).
		switch turn.StopReason {
		case provider.StopEndTurn:
			if len(turn.Calls) > 0 {
				return fail(CodeModelError, "model báo kết thúc nhưng vẫn kèm tool call")
			}
			out.Status = StatusCompleted
			return out
		case provider.StopRefusal:
			return fail(CodeModelRefused, "model từ chối yêu cầu")
		case provider.StopMaxTokens:
			return fail(CodeOutputTruncated, "trả lời bị cắt vì hết max_output_tokens")
		case provider.StopToolUse:
		default:
			return fail(CodeModelError, "model dừng với lý do không xác định")
		}
		if len(turn.Calls) == 0 {
			return fail(CodeModelError, "model yêu cầu gọi tool nhưng không kèm tool call nào")
		}

		// 4. Tool call, tuần tự theo thứ tự model đưa ra: audit có thứ tự xác định và
		// một lần dừng chặn được các bước sau.
		for _, call := range turn.Calls {
			if out.ToolCalls >= spec.Limits.MaxToolCalls {
				return fail(CodeLimitExceeded, fmt.Sprintf("vượt max_tool_calls (%d)", spec.Limits.MaxToolCalls))
			}
			if o, ok := cancelled(); ok {
				return o
			}
			out.ToolCalls++
			seq++
			known := offered(call.Name)
			// Bước này cũng ghi lại quyết định từ chối. Model chỉ được gọi tool đã được
			// đưa cho nó: tên khác bị từ chối bất kể bước này trả về gì.
			auth, err := ex.AuthorizeToolCall(AuthorizeInput{Task: spec.Task, StepSeq: seq, ModelStepSeq: modelSeq, Call: call, Offered: known})
			if err != nil {
				return stopped(CodeInternal)
			}
			if !known || !auth.Allowed {
				denials++
				if denials >= maxDenials {
					return fail(CodeToolDenied, fmt.Sprintf("tool bị từ chối %d lần", denials))
				}
				continue
			}
			in := CallToolInput{Task: spec.Task, StepSeq: seq, ModelStepSeq: modelSeq, Call: call}
			res, err := ex.CallTool(in)
			// Tool cần được duyệt: chờ quyết định rồi gọi lại ĐÚNG bước đó. Gateway xét
			// approval ở lần gọi lại: được duyệt thì tool chạy, không thì bước bị từ chối.
			// Chỉ chờ một lần cho mỗi tool call.
			for waits := 0; err == nil && res.ApprovalID != ""; waits++ {
				if waits >= 1 {
					return fail(CodeInternal, "tool call vẫn chờ phê duyệt sau khi đã có quyết định")
				}
				if werr := ex.AwaitApproval(AwaitInput{Task: spec.Task, StepSeq: seq, ApprovalID: res.ApprovalID}); werr != nil {
					return stopped(CodeInternal)
				}
				// Bị huỷ hoặc hết hạn trong lúc chờ: không gọi lại tool.
				if o, ok := cancelled(); ok {
					return o
				}
				res, err = ex.CallTool(in)
			}
			// Delegation: bước đã tạo một task con. Chạy nó tới khi kết thúc rồi gọi lại ĐÚNG
			// bước đó để lấy kết quả. Chỉ một lần cho mỗi tool call.
			for runs := 0; err == nil && res.DelegatedTaskID != ""; runs++ {
				if runs >= 1 {
					return fail(CodeInternal, "task con vẫn chưa kết thúc sau khi đã chạy")
				}
				if werr := ex.AwaitTask(AwaitTaskInput{Task: spec.Task, StepSeq: seq, ChildTaskID: res.DelegatedTaskID}); werr != nil {
					return stopped(CodeInternal)
				}
				if o, ok := cancelled(); ok {
					return o
				}
				res, err = ex.CallTool(in)
			}
			if err != nil {
				if o, ok := halted(); ok {
					return o
				}
				// Bước CallTool chỉ trả lỗi khi nó KHÔNG đóng được step (mọi lần thử đều bị
				// ngắt, hoặc không ghi được kết quả): tool có thể đã chạy. Đi tiếp thì model
				// sẽ thấy "chưa thực thi" và có thể lặp lại một hành động có tác dụng phụ.
				// (Tool call thất bại mà chắc chắn chưa chạy là một kết quả, không phải lỗi.)
				return fail(CodeToolOutcomeUnknown, "không xác định được một tool call đã chạy hay chưa")
			}
			if res.Unknown {
				return fail(CodeToolOutcomeUnknown, "không xác định được một tool call đã chạy hay chưa")
			}
			if res.Denied {
				denials++
				if denials >= maxDenials {
					return fail(CodeToolDenied, fmt.Sprintf("tool bị từ chối %d lần", denials))
				}
			}
			if contextBytes += res.Bytes; contextBytes > maxContext {
				return fail(CodeLimitExceeded, fmt.Sprintf("hội thoại vượt %d byte", maxContext))
			}
		}
	}
}
