// Package orchestrator là phần thuần của Orchestrator (agent-runtime §5): kế hoạch của
// một execution chế độ planned là DỮ LIỆU, được kiểm bằng code trước khi chạy, và
// được chạy bởi một bộ lập lịch deterministic.
//
// Package này không có I/O: nó không đọc đồng hồ, không chạm database, không dùng
// goroutine. Việc chạy một task (vòng lặp của Agent Runtime) nằm sau interface Engine,
// nên cùng một Run chạy trong process lẫn trong một Temporal workflow.
//
// Kế hoạch không hợp lệ bị TỪ CHỐI, không được "sửa giúp".
package orchestrator

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Trần của một kế hoạch. Đây là mức sàn của platform, nằm trong code: cấu hình của
// project chỉ đặt được chặt hơn.
const (
	// MaxTasks là số task tối đa của một kế hoạch.
	MaxTasks = 16
	// MaxDepth là độ dài tối đa của một chuỗi phụ thuộc (số task trên đường dài nhất).
	MaxDepth = 8
	// DefaultParallel là số task chạy đồng thời khi project không đặt.
	DefaultParallel = 4
	// MaxDelegationDepth là độ sâu tối đa của một chuỗi giao việc (task gốc ở độ sâu 0).
	MaxDelegationDepth = 5
	// MaxParallel là trần của max_parallel_tasks.
	MaxParallel  = 8
	maxGoalRunes = 20_000
)

// Cách một task không hoàn thành ảnh hưởng tới phần còn lại của kế hoạch.
const (
	// FailExecution: dừng cả execution.
	FailExecution = "fail_execution"
	// Continue: các task phụ thuộc vẫn chạy (không có kết quả của task này), và
	// execution không bị coi là thất bại vì task này.
	Continue = "continue"
	// SkipDependents (mặc định): các task phụ thuộc bị bỏ qua; phần còn lại chạy tiếp.
	SkipDependents = "skip_dependents"
)

// Trạng thái kết thúc của một task trong kế hoạch.
const (
	Completed      = "completed"
	Failed         = "failed"
	Cancelled      = "cancelled"
	TimedOut       = "timed_out"
	BudgetExceeded = "budget_exceeded"
	Skipped        = "skipped"
)

// CodeTaskFailed là mã lỗi của execution có task không hoàn thành.
const CodeTaskFailed = "task_failed"

// Loại nút của một kế hoạch.
const (
	// KindAgent (hoặc rỗng): một task giao cho một Agent.
	KindAgent = "agent"
	// KindWait: chờ một khoảng thời gian rồi hoàn thành; không gọi model hay tool.
	KindWait = "wait"
	// MaxWaitSeconds là thời gian chờ tối đa của một nút wait.
	MaxWaitSeconds = 3600
	// KindApproval: cổng chờ người duyệt; hoàn thành khi đủ người duyệt, thất bại khi bị
	// từ chối hoặc hết hạn. Không gọi model hay tool.
	KindApproval = "approval"
	// Thời hạn của một nút approval (giây) và độ dài thông điệp cho người duyệt.
	// MinTaskSeconds là thời hạn riêng nhỏ nhất của một task của Agent.
	MinTaskSeconds         = 10
	MinApprovalSeconds     = 60
	MaxApprovalSeconds     = 86400
	maxApprovalMessageRune = 2000
)

// IsStep báo một loại nút không phải task của Agent (wait, approval): nó không có Agent,
// model hay budget, và trạng thái của nó không nằm ở task.
func IsStep(kind string) bool { return kind == KindWait || kind == KindApproval }

// Gate là cấu hình của một nút approval.
type Gate struct {
	// Message là điều người duyệt được hỏi. Văn bản tĩnh của định nghĩa, không phải output của model.
	Message string
	// Required là số người duyệt cần có (1 hoặc 2, là những người khác nhau).
	Required int
	// TimeoutSeconds là thời hạn của approval; hết hạn là từ chối.
	TimeoutSeconds int
}

// Task là một task trong kế hoạch như người gọi (hoặc planner) đưa ra.
type Task struct {
	// Key là định danh của task trong kế hoạch.
	Key string
	// Kind là loại nút: KindAgent (mặc định), KindWait hoặc KindApproval. WaitSeconds chỉ
	// dùng với KindWait, Gate chỉ dùng với KindApproval.
	Kind         string
	WaitSeconds  int
	Gate         *Gate
	AgentID      uuid.UUID
	AgentVersion *int
	Goal         string
	DependsOn    []string
	OnFailure    string
	// Budget là phần budget cấp cho task (chuỗi thập phân, USD); rỗng = theo Agent Version.
	Budget string
	// TimeoutSeconds là thời hạn riêng của task của Agent (0 = theo Agent Version). Chỉ
	// được chặt hơn thời hạn của Agent Version; nơi tạo execution kiểm điều đó.
	TimeoutSeconds int
	// RetryOf (tuỳ chọn) là Key của task mà task này là LẦN THỬ LẠI: nó chỉ chạy khi
	// task kia kết thúc failed hoặc timed_out, và bị bỏ qua khi task kia hoàn thành. Chỉ
	// đường của workflow người dùng tạo ra nó (agent-runtime §5.12); xem Node.RetryOf.
	RetryOf string
}

// Plan là kế hoạch của một execution.
type Plan struct {
	Tasks []Task
	// MaxParallel là số task chạy đồng thời (0 = theo cấu hình của project).
	MaxParallel int
}

// Error là lý do một kế hoạch bị từ chối.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func reject(format string, args ...any) error { return &Error{Msg: fmt.Sprintf(format, args...)} }

var keyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// Node là một task ở dạng bộ lập lịch cần: chỉ số của các task nó phụ thuộc.
type Node struct {
	DependsOn []int  `json:"depends_on,omitempty"`
	OnFailure string `json:"on_failure"`
	// RetryOf (nil = không) là chỉ số của lần thử trước của cùng một bước. Luật:
	//   - lần thử trước completed → nút này bị bỏ qua (không cần thử lại);
	//   - lần thử trước failed / timed_out → nút này chạy;
	//   - mọi trạng thái khác (cancelled, budget_exceeded, skipped) → bị bỏ qua.
	// Một nút thử lại bị bỏ qua mang KẾT QUẢ của lần thử trước (xem Effective): các nút
	// phụ thuộc vào lần thử cuối thấy bước "đã hoàn thành" khi một lần thử nào đó thành công.
	RetryOf *int `json:"retry_of,omitempty"`
}

// Effective trả về trạng thái của nút i theo nghĩa "kết quả của bước": với một nút thử
// lại bị bỏ qua, đó là kết quả của lần thử trước nó (đệ quy theo chuỗi thử lại). Chuỗi
// dài nhất là số nút, nên vòng lặp có trần.
func Effective(nodes []Node, status []string, i int) string {
	// Một lần thử nào đó của bước đã hoàn thành thì bước đã hoàn thành, dù nút thử lại sau
	// nó chưa kịp được bỏ qua (execution dừng ngay sau đó): xem Aggregate.
	for k, hops := i, 0; k >= 0 && k < len(status) && k < len(nodes) && hops < len(nodes); hops++ {
		if status[k] == Completed {
			return Completed
		}
		if nodes[k].RetryOf == nil {
			break
		}
		k = *nodes[k].RetryOf
	}
	for range nodes {
		if i < 0 || i >= len(status) || i >= len(nodes) {
			return ""
		}
		r := nodes[i].RetryOf
		if status[i] != Skipped || r == nil {
			return status[i]
		}
		i = *r
	}
	return ""
}

// retryable báo một lần thử kết thúc ở trạng thái status có được thử lại hay không.
// Hết budget không thử lại (lần sau cũng hết); bị huỷ không thử lại.
func retryable(status string) bool { return status == Failed || status == TimedOut }

// MaxAttempts là số lần thử tối đa của một bước (lần đầu + các lần thử lại).
const MaxAttempts = 3

// Validate kiểm cấu trúc của kế hoạch: số task, định danh, phụ thuộc tồn tại, không
// chu trình, độ sâu. Nó trả về các node theo đúng thứ tự task của kế hoạch. Việc Agent
// có tồn tại và người gọi có được dùng nó hay không do bên gọi kiểm.
func Validate(p Plan) ([]Node, error) {
	n := len(p.Tasks)
	if n == 0 {
		return nil, reject("plan.tasks phải có ít nhất một task")
	}
	if n > MaxTasks {
		return nil, reject("plan.tasks có %d task, tối đa %d", n, MaxTasks)
	}
	if p.MaxParallel < 0 || p.MaxParallel > MaxParallel {
		return nil, reject("plan.max_parallel_tasks phải từ 1 đến %d", MaxParallel)
	}
	index := make(map[string]int, n)
	for i, t := range p.Tasks {
		if !keyPattern.MatchString(t.Key) {
			return nil, reject("plan.tasks[%d].id phải khớp [a-z0-9][a-z0-9_-]{0,31}", i)
		}
		if _, dup := index[t.Key]; dup {
			return nil, reject("plan.tasks[%d].id %q bị trùng", i, t.Key)
		}
		index[t.Key] = i
		switch t.Kind {
		case "", KindAgent:
			if t.AgentID == uuid.Nil {
				return nil, reject("plan.tasks[%d].agent_id là bắt buộc", i)
			}
			if strings.TrimSpace(t.Goal) == "" || utf8.RuneCountInString(t.Goal) > maxGoalRunes || !utf8.ValidString(t.Goal) ||
				strings.ContainsRune(t.Goal, 0) {
				return nil, reject("plan.tasks[%d].goal phải có từ 1 đến %d ký tự", i, maxGoalRunes)
			}
			if t.TimeoutSeconds != 0 && t.TimeoutSeconds < MinTaskSeconds {
				return nil, reject("plan.tasks[%d]: thời hạn của task phải từ %d giây", i, MinTaskSeconds)
			}
		case KindWait:
			if t.WaitSeconds < 1 || t.WaitSeconds > MaxWaitSeconds {
				return nil, reject("plan.tasks[%d]: thời gian chờ phải từ 1 đến %d giây", i, MaxWaitSeconds)
			}
			if t.AgentID != uuid.Nil || t.Goal != "" || t.Budget != "" || t.TimeoutSeconds != 0 {
				return nil, reject("plan.tasks[%d]: nút chờ không có agent, goal, budget hay thời hạn riêng", i)
			}
		case KindApproval:
			g := t.Gate
			if g == nil || g.Required < 1 || g.Required > 2 {
				return nil, reject("plan.tasks[%d]: nút approval cần 1 hoặc 2 người duyệt", i)
			}
			if g.TimeoutSeconds < MinApprovalSeconds || g.TimeoutSeconds > MaxApprovalSeconds {
				return nil, reject("plan.tasks[%d]: thời hạn duyệt phải từ %d đến %d giây", i, MinApprovalSeconds, MaxApprovalSeconds)
			}
			if strings.TrimSpace(g.Message) == "" || utf8.RuneCountInString(g.Message) > maxApprovalMessageRune || !utf8.ValidString(g.Message) ||
				strings.ContainsRune(g.Message, 0) {
				return nil, reject("plan.tasks[%d]: thông điệp cho người duyệt phải có từ 1 đến %d ký tự", i, maxApprovalMessageRune)
			}
			if t.AgentID != uuid.Nil || t.Goal != "" || t.Budget != "" || t.TimeoutSeconds != 0 {
				return nil, reject("plan.tasks[%d]: nút approval không có agent, goal hay budget (thời hạn của nó nằm ở cấu hình cổng)", i)
			}
			// Cổng bị từ chối mà các bước sau vẫn chạy thì không phải là cổng.
			if t.OnFailure == Continue {
				return nil, reject("plan.tasks[%d]: nút approval không dùng on_failure: continue", i)
			}
		default:
			return nil, reject("plan.tasks[%d]: loại nút không hợp lệ", i)
		}
		switch t.OnFailure {
		case "", FailExecution, Continue, SkipDependents:
		default:
			return nil, reject("plan.tasks[%d].on_failure không hợp lệ", i)
		}
	}
	nodes := make([]Node, n)
	for i, t := range p.Tasks {
		nodes[i].OnFailure = t.OnFailure
		if nodes[i].OnFailure == "" {
			nodes[i].OnFailure = SkipDependents
		}
		seen := map[int]bool{}
		for _, key := range t.DependsOn {
			j, ok := index[key]
			switch {
			case !ok:
				return nil, reject("plan.tasks[%d].depends_on tham chiếu task %q không có trong kế hoạch", i, key)
			case j == i:
				return nil, reject("plan.tasks[%d] phụ thuộc vào chính nó", i)
			case seen[j]:
				return nil, reject("plan.tasks[%d].depends_on lặp lại %q", i, key)
			}
			seen[j] = true
			nodes[i].DependsOn = append(nodes[i].DependsOn, j)
		}
	}
	retried := make([]bool, n)
	for i, t := range p.Tasks {
		if t.RetryOf == "" {
			continue
		}
		j, ok := index[t.RetryOf]
		prev := Task{}
		if ok {
			prev = p.Tasks[j]
		}
		switch {
		case !ok || j == i || !slices.Contains(nodes[i].DependsOn, j):
			return nil, reject("plan.tasks[%d]: lần thử lại phải phụ thuộc vào đúng task nó thử lại", i)
		case IsStep(t.Kind) || IsStep(prev.Kind) || t.AgentID != prev.AgentID:
			return nil, reject("plan.tasks[%d]: chỉ task của Agent được thử lại, bằng cùng Agent", i)
		case retried[j]:
			return nil, reject("plan.tasks[%d]: task %q đã có một lần thử lại", i, t.RetryOf)
		// Lần thử chưa phải cuối không được chặn hay làm hỏng kế hoạch: kết quả của bước
		// là kết quả của lần thử cuối.
		case nodes[j].OnFailure != Continue:
			return nil, reject("plan.tasks[%d]: task được thử lại phải có on_failure: continue", i)
		}
		retried[j] = true
		nodes[i].RetryOf = &j
	}
	for i := range nodes {
		// Chuỗi thử lại có trần; và không nút nào khác được phụ thuộc vào một lần thử chưa
		// phải cuối (on_failure: continue của nó sẽ cho nút đó chạy khi lần thử thất bại).
		attempts := 1
		for r := nodes[i].RetryOf; r != nil; r = nodes[*r].RetryOf {
			if attempts++; attempts > MaxAttempts {
				return nil, reject("plan.tasks[%d]: một bước có tối đa %d lần thử", i, MaxAttempts)
			}
		}
		for _, j := range nodes[i].DependsOn {
			if retried[j] && (nodes[i].RetryOf == nil || *nodes[i].RetryOf != j) {
				return nil, reject("plan.tasks[%d]: không phụ thuộc được vào một lần thử chưa phải cuối", i)
			}
		}
	}
	depth, err := depths(nodes)
	if err != nil {
		return nil, err
	}
	for i, d := range depth {
		if d > MaxDepth {
			return nil, reject("plan.tasks[%d] nằm ở độ sâu %d, tối đa %d", i, d, MaxDepth)
		}
	}
	return nodes, nil
}

// depths trả về độ sâu của từng node (1 = không phụ thuộc gì); lỗi nếu có chu trình.
func depths(nodes []Node) ([]int, error) {
	const visiting = -1
	depth := make([]int, len(nodes))
	var visit func(i int) (int, error)
	visit = func(i int) (int, error) {
		switch {
		case depth[i] == visiting:
			return 0, reject("kế hoạch có chu trình phụ thuộc")
		case depth[i] > 0:
			return depth[i], nil
		}
		depth[i] = visiting
		d := 1
		for _, j := range nodes[i].DependsOn {
			dj, err := visit(j)
			if err != nil {
				return 0, err
			}
			if dj+1 > d {
				d = dj + 1
			}
		}
		depth[i] = d
		return d, nil
	}
	for i := range nodes {
		if _, err := visit(i); err != nil {
			return nil, err
		}
	}
	return depth, nil
}

// Makespan trả về thời gian kế hoạch cần khi mọi task dùng hết cost của nó (ví dụ
// thời hạn của task) và nhiều nhất parallel task chạy cùng lúc: nó mô phỏng đúng cách
// Run lập lịch (task đủ điều kiện bắt đầu theo thứ tự của kế hoạch khi còn chỗ). Chỉ
// tính đường dài nhất là chưa đủ: 16 task độc lập với 4 chỗ cần bốn lượt, không phải một.
// nodes phải đã qua Validate.
func Makespan(nodes []Node, cost []int64, parallel int) int64 {
	if parallel <= 0 {
		parallel = DefaultParallel
	}
	n := len(nodes)
	const pending, active, done = 0, 1, 2
	state := make([]int, n)
	finish := make([]int64, n)
	var now int64
	for left, running := n, 0; left > 0; {
		for i := 0; i < n && running < parallel; i++ {
			if state[i] != pending {
				continue
			}
			ready := true
			for _, j := range nodes[i].DependsOn {
				ready = ready && state[j] == done
			}
			if ready {
				state[i], finish[i] = active, now+cost[i]
				running++
			}
		}
		// Tiến tới lúc task đang chạy xong sớm nhất.
		next := int64(-1)
		for i := 0; i < n; i++ {
			if state[i] == active && (next < 0 || finish[i] < next) {
				next = finish[i]
			}
		}
		if next < 0 {
			return now // không còn gì chạy được (không xảy ra với đồ thị đã qua Validate)
		}
		now = next
		for i := 0; i < n; i++ {
			if state[i] == active && finish[i] <= now {
				state[i] = done
				running--
				left--
			}
		}
	}
	return now
}

// Sinks trả về chỉ số các node không có node nào phụ thuộc vào: kết quả của chúng là
// kết quả của kế hoạch.
func Sinks(nodes []Node) []int {
	used := make([]bool, len(nodes))
	for _, n := range nodes {
		for _, j := range n.DependsOn {
			used[j] = true
		}
	}
	var out []int
	for i := range nodes {
		if !used[i] {
			out = append(out, i)
		}
	}
	return out
}

// Engine chạy các task của một kế hoạch. Start không chặn; Wait chặn tới khi một task
// đã Start kết thúc.
type Engine interface {
	// Start bắt đầu chạy task i.
	Start(i int)
	// Wait trả về một task đã Start vừa kết thúc và trạng thái kết thúc của nó.
	Wait() (i int, status string)
	// Skip ghi nhận task i không được chạy.
	Skip(i int)
	// Halt yêu cầu mọi task đang chạy dừng lại (một task fail_execution đã thất bại).
	Halt()
	// Stopped cho biết execution đang bị dừng từ bên ngoài (huỷ, hết hạn): không bắt đầu
	// thêm task nào.
	Stopped() bool
}

// blocks cho biết một task ở trạng thái status có chặn các task phụ thuộc nó hay không.
func blocks(n Node, status string) bool {
	return status != Completed && n.OnFailure != Continue
}

// Run chạy kế hoạch tới khi không còn task nào chạy được, và trả về trạng thái của
// từng task ("" = chưa bao giờ được bắt đầu vì execution dừng). Run luôn kết thúc:
// mỗi vòng hoặc bắt đầu, hoặc bỏ qua, hoặc chờ xong ít nhất một task, và số task hữu hạn.
//
// Run cũng là thân của workflow cha trên Temporal: nó phải deterministic, và mọi thay
// đổi về thứ tự gọi Engine là thay đổi workflow.
func Run(en Engine, nodes []Node, parallel int) []string {
	if parallel <= 0 {
		parallel = DefaultParallel
	}
	// nodes và parallel có thể đến qua lịch sử workflow: không tin chúng hơn mức cần.
	if parallel > MaxParallel {
		parallel = MaxParallel
	}
	status := make([]string, len(nodes))
	if len(nodes) > MaxTasks {
		return status
	}
	for i, n := range nodes {
		for _, j := range n.DependsOn {
			if j < 0 || j >= len(nodes) || j == i {
				return status // tham chiếu hỏng: không chạy gì, bên gọi đóng execution
			}
		}
		if n.RetryOf != nil && !slices.Contains(n.DependsOn, *n.RetryOf) {
			return status
		}
	}
	started := make([]bool, len(nodes))
	running, halted := 0, false
	for {
		stop := halted || en.Stopped()
		// Bỏ qua task có một phụ thuộc không hoàn thành; lặp tới khi ổn định vì việc bỏ
		// qua lan theo chuỗi phụ thuộc. Khi execution đang dừng thì không: task chưa bắt
		// đầu khi đó là bị dừng theo execution (bên gọi đóng chúng), không phải bị bỏ qua
		// vì một phụ thuộc thất bại.
		for changed := !stop; changed; {
			changed = false
			for i, n := range nodes {
				if started[i] || status[i] != "" {
					continue
				}
				for _, j := range n.DependsOn {
					if status[j] == "" {
						continue
					}
					skip := blocks(nodes[j], Effective(nodes, status, j))
					if n.RetryOf != nil && *n.RetryOf == j {
						// Lần thử lại chỉ chạy khi lần thử trước thật sự đã chạy và thất bại.
						skip = !retryable(status[j])
					}
					if skip {
						status[i], changed = Skipped, true
						en.Skip(i)
						// Bước fail_execution không còn lần thử nào và không hoàn thành:
						// dừng kế hoạch như khi lần thử cuối tự thất bại.
						// Chỉ khi bước đã thật sự chạy: một chuỗi bị bỏ qua vì phụ thuộc khác
						// thất bại không dừng kế hoạch, như bước không có thử lại.
						if eff := Effective(nodes, status, i); n.RetryOf != nil && n.OnFailure == FailExecution && eff != Completed && eff != Skipped && !halted {
							halted, stop, changed = true, true, false
							en.Halt()
						}
						break
					}
				}
				if stop {
					break
				}
			}
		}
		// Bắt đầu các task đã đủ điều kiện, theo thứ tự của kế hoạch.
		for i, n := range nodes {
			if stop || running >= parallel {
				break
			}
			if started[i] || status[i] != "" {
				continue
			}
			ready := true
			for _, j := range n.DependsOn {
				if status[j] == "" {
					ready = false
					break
				}
			}
			if ready {
				started[i] = true
				running++
				en.Start(i)
			}
		}
		if running == 0 {
			return status
		}
		i, st := en.Wait()
		if i < 0 || i >= len(nodes) || !started[i] || status[i] != "" {
			// Engine trả về thứ không thể có: dừng thay vì lặp trên trạng thái hỏng.
			en.Halt()
			return status
		}
		if !terminal(st) {
			st = Failed
		}
		status[i] = st
		running--
		if st != Completed && nodes[i].OnFailure == FailExecution && !halted {
			halted = true
			en.Halt()
		}
	}
}

func terminal(s string) bool {
	switch s {
	case Completed, Failed, Cancelled, TimedOut, BudgetExceeded, Skipped:
		return true
	}
	return false
}

// Stop là lý do execution bị dừng từ bên ngoài, nếu có.
type Stop int

const (
	// NotStopped: execution không bị dừng từ bên ngoài.
	NotStopped Stop = iota
	// StopCancelled: execution bị huỷ.
	StopCancelled
	// StopDeadline: execution quá hạn.
	StopDeadline
)

// Aggregate suy ra trạng thái kết thúc của execution từ trạng thái các task. Task chưa
// kết thúc ("" hoặc trạng thái lạ) được coi là không hoàn thành.
//
// Execution hoàn thành khi mọi task hoàn thành, trừ những task được phép thất bại
// (on_failure: continue). Khi không: execution bị huỷ hoặc quá hạn thì mang trạng thái
// đó; còn lại nguyên nhân gốc quyết định trạng thái: mọi task
// thất bại đều vì budget thì là budget_exceeded, đều vì hết hạn thì là timed_out, còn
// lại là failed / task_failed.
func Aggregate(nodes []Node, status []string, stop Stop) (execStatus, code string) {
	causes := map[string]int{}
	bad := 0
	for i, n := range nodes {
		st := ""
		if i < len(status) {
			st = status[i]
		}
		// Nút thử lại mang kết quả của chuỗi thử lại của nó (bị bỏ qua, hoặc chưa kịp được
		// bỏ qua khi execution dừng sau khi một lần thử trước đã hoàn thành).
		if n.RetryOf != nil {
			st = Effective(nodes, status, i)
		}
		if st == Completed || n.OnFailure == Continue {
			continue
		}
		bad++
		// Bị bỏ qua hay bị dừng theo là hệ quả, không phải nguyên nhân.
		if st != Skipped && st != Cancelled && st != "" {
			causes[st]++
		}
	}
	// Mọi task cần hoàn thành đã hoàn thành: kế hoạch xong, kể cả khi yêu cầu huỷ hay hạn
	// của execution đến ngay sau đó (ví dụ việc ghi trạng thái cuối bị trễ qua hạn).
	if bad == 0 {
		return Completed, ""
	}
	switch stop {
	case StopCancelled:
		return Cancelled, ""
	case StopDeadline:
		return TimedOut, ""
	}
	switch {
	case len(causes) == 1 && causes[BudgetExceeded] > 0:
		return BudgetExceeded, BudgetExceeded
	case len(causes) == 1 && causes[TimedOut] > 0:
		return TimedOut, ""
	}
	return Failed, CodeTaskFailed
}

// Settings là khoá `orchestration` trong settings của một project.
type Settings struct {
	// PlannedEnabled bật chế độ planned cho project. Mặc định tắt.
	PlannedEnabled bool `json:"planned_enabled"`
	// MaxParallelTasks là số task chạy đồng thời của một execution (0 = mặc định).
	MaxParallelTasks int `json:"max_parallel_tasks,omitempty"`
	// PlannerAgentID là Agent lập kế hoạch của project: nó đề xuất kế hoạch từ một yêu
	// cầu. nil = project chưa có.
	PlannerAgentID *uuid.UUID `json:"planner_agent_id,omitempty"`
}

// Parallel trả về số task chạy đồng thời theo cấu hình của project.
func (s Settings) Parallel() int {
	if s.MaxParallelTasks <= 0 {
		return DefaultParallel
	}
	return s.MaxParallelTasks
}

// ParseSettings đọc khoá `orchestration` từ settings của project. Thiếu khoá là cấu
// hình mặc định (planned tắt). Khoá lạ hoặc giá trị ngoài khoảng là lỗi: cấu hình hỏng
// không được hiểu thành "bật".
func ParseSettings(settings json.RawMessage) (Settings, error) {
	var out Settings
	if len(settings) == 0 {
		return out, nil
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(settings, &top); err != nil {
		return out, reject("settings phải là một object")
	}
	raw, ok := top["orchestration"]
	if !ok {
		return out, nil
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return Settings{}, reject("settings.orchestration không hợp lệ: chỉ nhận planned_enabled (boolean), max_parallel_tasks (số nguyên) và planner_agent_id (uuid)")
	}
	if out.MaxParallelTasks < 0 || out.MaxParallelTasks > MaxParallel {
		return Settings{}, reject("settings.orchestration.max_parallel_tasks phải từ 1 đến %d", MaxParallel)
	}
	return out, nil
}

// Candidate là một Agent mà kế hoạch được phép dùng.
type Candidate struct {
	ID   uuid.UUID
	Slug string
}

// proposal là kế hoạch ở dạng Agent lập kế hoạch trả lời: Agent được nêu bằng slug.
type proposal struct {
	Tasks []struct {
		ID        string   `json:"id"`
		Agent     string   `json:"agent"`
		Goal      string   `json:"goal"`
		DependsOn []string `json:"depends_on"`
		OnFailure string   `json:"on_failure"`
	} `json:"tasks"`
}

// maxProposalBytes là trần kích thước phần JSON của một đề xuất.
const maxProposalBytes = 256 << 10

// ParseProposal đọc kế hoạch từ trả lời của Agent lập kế hoạch và kiểm nó. Trả lời là
// output của model: đầu vào không tin cậy. Nó phải chứa đúng một object JSON
// {"tasks": [{"id", "agent", "goal", "depends_on", "on_failure"}]} (cho phép văn bản
// hoặc hàng rào code bao quanh); khoá lạ, Agent không có trong danh sách được phép, hay
// cấu trúc sai đều làm đề xuất không hợp lệ. Không có gì được sửa giúp: hoặc kế hoạch
// dùng được nguyên văn, hoặc trả về lý do.
func ParseProposal(answer string, allowed []Candidate) (Plan, []string) {
	start, end := strings.Index(answer, "{"), strings.LastIndex(answer, "}")
	if start < 0 || end <= start {
		return Plan{}, []string{"trả lời không chứa một object JSON"}
	}
	raw := answer[start : end+1]
	if len(raw) > maxProposalBytes {
		return Plan{}, []string{"kế hoạch quá lớn"}
	}
	// encoding/json lấy giá trị CUỐI khi khoá trùng và khớp khoá không phân biệt hoa
	// thường: người đọc trả lời thô có thể thấy một kế hoạch khác với thứ được đọc ra.
	if err := strictKeys(raw); err != nil {
		return Plan{}, []string{err.Error()}
	}
	var p proposal
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return Plan{}, []string{"kế hoạch không đúng định dạng {\"tasks\": [{\"id\", \"agent\", \"goal\", \"depends_on\", \"on_failure\"}]}"}
	}
	// Sau object không được còn gì ngoài khoảng trắng. (Decoder.More trả false khi ký tự
	// kế là "]" hoặc "}", nên không dùng được để phát hiện rác phía sau.)
	if strings.TrimSpace(raw[dec.InputOffset():]) != "" {
		return Plan{}, []string{"trả lời chứa nhiều hơn một object JSON"}
	}
	bySlug := make(map[string]uuid.UUID, len(allowed))
	for _, c := range allowed {
		bySlug[c.Slug] = c.ID
	}
	var problems []string
	plan := Plan{Tasks: make([]Task, len(p.Tasks))}
	for i, t := range p.Tasks {
		id, ok := bySlug[t.Agent]
		if !ok {
			problems = append(problems, fmt.Sprintf("tasks[%d].agent không nằm trong danh sách agent được phép", i))
		}
		plan.Tasks[i] = Task{Key: t.ID, AgentID: id, Goal: t.Goal, DependsOn: t.DependsOn, OnFailure: t.OnFailure}
	}
	if len(problems) > 0 {
		return Plan{}, problems
	}
	if _, err := Validate(plan); err != nil {
		return Plan{}, []string{err.Error()}
	}
	return plan, nil
}

// proposalKeys là các khoá được phép của một đề xuất, đúng chính tả.
var proposalKeys = map[string]bool{"tasks": true, "id": true, "agent": true, "goal": true, "depends_on": true, "on_failure": true}

// strictKeys duyệt JSON theo token và từ chối khoá trùng trong cùng một object, khoá
// không có trong proposalKeys (kể cả khác hoa thường), và JSON lồng quá sâu.
func strictKeys(raw string) error {
	dec := json.NewDecoder(strings.NewReader(raw))
	type frame struct {
		object bool
		keys   map[string]bool
		isKey  bool
	}
	var stack []frame
	for {
		tok, err := dec.Token()
		if err != nil {
			if len(stack) == 0 {
				return nil // hết object đầu tiên; phần còn lại do bước decode kiểm
			}
			return reject("kế hoạch không phải JSON hợp lệ")
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				if len(stack) > 0 && stack[len(stack)-1].object {
					stack[len(stack)-1].isKey = true // giá trị của một khoá vừa bắt đầu
				}
				if len(stack) >= 6 {
					return reject("kế hoạch lồng quá sâu")
				}
				stack = append(stack, frame{object: d == '{', keys: map[string]bool{}, isKey: true})
			default:
				stack = stack[:len(stack)-1]
				if len(stack) == 0 {
					return nil
				}
			}
			continue
		}
		if len(stack) == 0 {
			return reject("kế hoạch không phải một object JSON")
		}
		top := &stack[len(stack)-1]
		if !top.object {
			continue
		}
		if top.isKey {
			key, _ := tok.(string)
			if !proposalKeys[key] {
				return reject("kế hoạch có khoá không được phép: %q", key)
			}
			if top.keys[key] {
				return reject("kế hoạch lặp lại khoá %q", key)
			}
			top.keys[key] = true
			top.isKey = false
			continue
		}
		top.isKey = true // vừa đọc xong một giá trị vô hướng: token kế là khoá
	}
}
