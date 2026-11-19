package orchestrator

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func task(key string, deps ...string) Task {
	return Task{Key: key, AgentID: uuid.New(), Goal: "làm việc " + key, DependsOn: deps}
}

func TestValidate(t *testing.T) {
	many := make([]Task, MaxTasks+1)
	for i := range many {
		many[i] = task("t" + string(rune('a'+i)))
	}
	chain := make([]Task, MaxDepth+1)
	for i := range chain {
		chain[i] = task("c" + string(rune('a'+i)))
		if i > 0 {
			chain[i].DependsOn = []string{chain[i-1].Key}
		}
	}
	badFailure := task("a")
	badFailure.OnFailure = "retry"
	noAgent := task("a")
	noAgent.AgentID = uuid.Nil
	noGoal := task("a")
	noGoal.Goal = "  "
	cases := []struct {
		name string
		plan Plan
		want string // "" = hợp lệ; khác rỗng = đoạn phải có trong lỗi
	}{
		{"hợp lệ", Plan{Tasks: []Task{task("a"), task("b"), task("c", "a", "b")}}, ""},
		{"rỗng", Plan{}, "ít nhất một task"},
		{"quá nhiều task", Plan{Tasks: many}, "tối đa"},
		{"id sai dạng", Plan{Tasks: []Task{task("A b")}}, "phải khớp"},
		{"id trùng", Plan{Tasks: []Task{task("a"), task("a")}}, "bị trùng"},
		{"phụ thuộc không tồn tại", Plan{Tasks: []Task{task("a", "x")}}, "không có trong kế hoạch"},
		{"tự phụ thuộc", Plan{Tasks: []Task{task("a", "a")}}, "chính nó"},
		{"phụ thuộc lặp", Plan{Tasks: []Task{task("a"), task("b", "a", "a")}}, "lặp lại"},
		{"chu trình", Plan{Tasks: []Task{task("a", "c"), task("b", "a"), task("c", "b")}}, "chu trình"},
		{"quá sâu", Plan{Tasks: chain}, "độ sâu"},
		{"on_failure lạ", Plan{Tasks: []Task{badFailure}}, "on_failure"},
		{"thiếu agent", Plan{Tasks: []Task{noAgent}}, "agent_id"},
		{"thiếu goal", Plan{Tasks: []Task{noGoal}}, "goal"},
		{"max_parallel quá lớn", Plan{Tasks: []Task{task("a")}, MaxParallel: MaxParallel + 1}, "max_parallel_tasks"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			nodes, err := Validate(c.plan)
			if c.want == "" {
				if err != nil {
					t.Fatalf("kế hoạch hợp lệ bị từ chối: %v", err)
				}
				if len(nodes) != len(c.plan.Tasks) || nodes[0].OnFailure != SkipDependents {
					t.Fatalf("nodes = %+v", nodes)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("lỗi = %v, muốn chứa %q", err, c.want)
			}
		})
	}
}

// fakeEngine chạy task "tức thì": Wait trả về task đã Start sớm nhất, với trạng thái đã
// định sẵn. Nó ghi lại thứ tự và số task chạy đồng thời.
type fakeEngine struct {
	outcome   map[int]string
	queue     []int
	started   []int
	skipped   []int
	maxAtOnce int
	halts     int
	stopAfter int // > 0: Stopped() trả true sau khi đã Start chừng này task
}

func (f *fakeEngine) Start(i int) {
	f.started = append(f.started, i)
	f.queue = append(f.queue, i)
	if len(f.queue) > f.maxAtOnce {
		f.maxAtOnce = len(f.queue)
	}
}

func (f *fakeEngine) Wait() (int, string) {
	i := f.queue[0]
	f.queue = f.queue[1:]
	st, ok := f.outcome[i]
	if !ok {
		st = Completed
	}
	if f.halts > 0 && st == Completed {
		st = Cancelled // kế hoạch đã dừng: task đang chạy bị huỷ
	}
	return i, st
}

func (f *fakeEngine) Skip(i int) { f.skipped = append(f.skipped, i) }
func (f *fakeEngine) Halt()      { f.halts++ }
func (f *fakeEngine) Stopped() bool {
	return f.stopAfter > 0 && len(f.started) >= f.stopAfter
}

func mustNodes(t *testing.T, tasks ...Task) []Node {
	t.Helper()
	nodes, err := Validate(Plan{Tasks: tasks})
	if err != nil {
		t.Fatal(err)
	}
	return nodes
}

func TestRun(t *testing.T) {
	// a, b, c, d độc lập → e phụ thuộc cả bốn → f phụ thuộc e.
	diamond := func(t *testing.T) []Node {
		return mustNodes(t, task("a"), task("b"), task("c"), task("d"), task("e", "a", "b", "c", "d"), task("f", "e"))
	}
	t.Run("chạy đúng thứ tự phụ thuộc và đúng trần song song", func(t *testing.T) {
		en := &fakeEngine{}
		got := Run(en, diamond(t), 2)
		if !slices.Equal(got, []string{Completed, Completed, Completed, Completed, Completed, Completed}) {
			t.Fatalf("status = %v", got)
		}
		if en.maxAtOnce != 2 {
			t.Fatalf("chạy đồng thời tối đa %d task, muốn 2", en.maxAtOnce)
		}
		if pos := slices.Index(en.started, 4); pos != 4 {
			t.Fatalf("task e bắt đầu ở vị trí %d, phải sau cả bốn task nó phụ thuộc: %v", pos, en.started)
		}
	})
	t.Run("skip_dependents bỏ qua cả chuỗi phụ thuộc, phần còn lại chạy tiếp", func(t *testing.T) {
		en := &fakeEngine{outcome: map[int]string{1: Failed}}
		got := Run(en, diamond(t), 4)
		if !slices.Equal(got, []string{Completed, Failed, Completed, Completed, Skipped, Skipped}) {
			t.Fatalf("status = %v", got)
		}
		if !slices.Equal(en.skipped, []int{4, 5}) || en.halts != 0 {
			t.Fatalf("skipped = %v, halts = %d", en.skipped, en.halts)
		}
	})
	t.Run("continue cho task phụ thuộc chạy tiếp", func(t *testing.T) {
		nodes := diamond(t)
		nodes[1].OnFailure = Continue
		en := &fakeEngine{outcome: map[int]string{1: BudgetExceeded}}
		got := Run(en, nodes, 4)
		if !slices.Equal(got, []string{Completed, BudgetExceeded, Completed, Completed, Completed, Completed}) {
			t.Fatalf("status = %v", got)
		}
		if st, _ := Aggregate(nodes, got, NotStopped); st != Completed {
			t.Fatalf("execution = %s, muốn completed (task được phép thất bại)", st)
		}
	})
	t.Run("fail_execution dừng kế hoạch và không bắt đầu thêm task", func(t *testing.T) {
		nodes := mustNodes(t, task("a"), task("b"), task("c"), task("d", "c"))
		nodes[0].OnFailure = FailExecution
		en := &fakeEngine{outcome: map[int]string{0: Failed}}
		got := Run(en, nodes, 2)
		// a thất bại → Halt; b đang chạy bị huỷ; c, d không bao giờ bắt đầu.
		if !slices.Equal(got, []string{Failed, Cancelled, "", ""}) {
			t.Fatalf("status = %v", got)
		}
		if en.halts != 1 || len(en.started) != 2 {
			t.Fatalf("halts = %d, started = %v", en.halts, en.started)
		}
		if st, code := Aggregate(nodes, got, NotStopped); st != Failed || code != CodeTaskFailed {
			t.Fatalf("execution = %s / %s", st, code)
		}
	})
	t.Run("bị dừng từ bên ngoài thì không bắt đầu thêm task", func(t *testing.T) {
		en := &fakeEngine{stopAfter: 2}
		got := Run(en, diamond(t), 2)
		if len(en.started) != 2 || got[4] != "" {
			t.Fatalf("started = %v, status = %v", en.started, got)
		}
	})
}

func TestRunRejectsBrokenReferences(t *testing.T) {
	en := &fakeEngine{}
	got := Run(en, []Node{{DependsOn: []int{5}, OnFailure: SkipDependents}, {OnFailure: SkipDependents}}, 2)
	if len(en.started) != 0 || got[0] != "" || got[1] != "" {
		t.Fatalf("kế hoạch có tham chiếu hỏng vẫn chạy: started %v, status %v", en.started, got)
	}
	// Trần song song của platform thắng giá trị nhận được.
	en = &fakeEngine{}
	Run(en, make([]Node, MaxTasks), 1000)
	if en.maxAtOnce != MaxParallel {
		t.Fatalf("chạy đồng thời %d task, trần là %d", en.maxAtOnce, MaxParallel)
	}
}

func TestAggregate(t *testing.T) {
	nodes := []Node{{OnFailure: SkipDependents}, {OnFailure: SkipDependents}, {DependsOn: []int{0, 1}, OnFailure: SkipDependents}}
	cases := []struct {
		name       string
		status     []string
		stop       Stop
		want, code string
	}{
		{"mọi task xong", []string{Completed, Completed, Completed}, NotStopped, Completed, ""},
		{"một task lỗi", []string{Completed, Failed, Skipped}, NotStopped, Failed, CodeTaskFailed},
		{"chỉ vì budget", []string{BudgetExceeded, Completed, Skipped}, NotStopped, BudgetExceeded, BudgetExceeded},
		{"chỉ vì hết hạn", []string{TimedOut, Completed, Skipped}, NotStopped, TimedOut, ""},
		{"nhiều nguyên nhân", []string{TimedOut, BudgetExceeded, Skipped}, NotStopped, Failed, CodeTaskFailed},
		{"task chưa kết thúc", []string{Completed, "", ""}, NotStopped, Failed, CodeTaskFailed},
		{"bị huỷ khi còn task dở", []string{Completed, Cancelled, ""}, StopCancelled, Cancelled, ""},
		// Mọi task đã xong trước khi yêu cầu huỷ / hạn có hiệu lực: kết quả là completed.
		{"huỷ đến sau khi mọi task xong", []string{Completed, Completed, Completed}, StopCancelled, Completed, ""},
		{"hạn đến sau khi mọi task xong", []string{Completed, Completed, Completed}, StopDeadline, Completed, ""},
		{"quá hạn", []string{Completed, Cancelled, ""}, StopDeadline, TimedOut, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got, code := Aggregate(nodes, c.status, c.stop); got != c.want || code != c.code {
				t.Fatalf("= %s / %s, muốn %s / %s", got, code, c.want, c.code)
			}
		})
	}
}

func TestMakespanAndSinks(t *testing.T) {
	nodes := mustNodes(t, task("a"), task("b"), task("c", "a", "b"), task("d"))
	if got := Makespan(nodes, []int64{10, 30, 5, 20}, 4); got != 35 {
		t.Fatalf("makespan = %d, muốn 35 (b → c)", got)
	}
	// Một chỗ: các task nối đuôi nhau.
	if got := Makespan(nodes, []int64{10, 30, 5, 20}, 1); got != 65 {
		t.Fatalf("makespan tuần tự = %d, muốn 65", got)
	}
	// 16 task độc lập, 4 chỗ: bốn lượt, không phải một.
	wide := make([]Node, 16)
	costs := make([]int64, 16)
	for i := range costs {
		costs[i] = 600
	}
	if got := Makespan(wide, costs, 4); got != 2400 {
		t.Fatalf("makespan 16 task / 4 chỗ = %d, muốn 2400", got)
	}
	if got := Sinks(nodes); !slices.Equal(got, []int{2, 3}) {
		t.Fatalf("sinks = %v", got)
	}
}

func TestParseSettings(t *testing.T) {
	cases := []struct {
		in      string
		enabled bool
		par     int
		bad     bool
	}{
		{``, false, DefaultParallel, false},
		{`{}`, false, DefaultParallel, false},
		{`{"model_routing":{}}`, false, DefaultParallel, false},
		{`{"orchestration":{"planned_enabled":true}}`, true, DefaultParallel, false},
		{`{"orchestration":{"planned_enabled":true,"max_parallel_tasks":2}}`, true, 2, false},
		{`{"orchestration":{"planned_enabled":"yes"}}`, false, 0, true},
		{`{"orchestration":{"planned":true}}`, false, 0, true}, // khoá lạ không được hiểu thành "bật"
		{`{"orchestration":{"planned_enabled":true,"max_parallel_tasks":99}}`, false, 0, true},
		{`[1]`, false, 0, true},
	}
	for _, c := range cases {
		got, err := ParseSettings(json.RawMessage(c.in))
		if (err != nil) != c.bad {
			t.Fatalf("%s: err = %v", c.in, err)
		}
		if err == nil && (got.PlannedEnabled != c.enabled || got.Parallel() != c.par) {
			t.Fatalf("%s: = %+v", c.in, got)
		}
	}
}

func TestParseProposal(t *testing.T) {
	mon, db := uuid.New(), uuid.New()
	allowed := []Candidate{{ID: mon, Slug: "monitoring"}, {ID: db, Slug: "database"}}
	good := `{"tasks":[{"id":"a","agent":"monitoring","goal":"xem metric","depends_on":[]},{"id":"b","agent":"database","goal":"xem db","depends_on":["a"],"on_failure":"continue"}]}`
	plan, problems := ParseProposal("Đây là kế hoạch:\n```json\n"+good+"\n```\n", allowed)
	if len(problems) != 0 || len(plan.Tasks) != 2 || plan.Tasks[0].AgentID != mon || plan.Tasks[1].AgentID != db ||
		plan.Tasks[1].DependsOn[0] != "a" || plan.Tasks[1].OnFailure != Continue {
		t.Fatalf("plan = %+v, problems = %v", plan, problems)
	}
	// Không có gì được sửa giúp: mỗi trường hợp dưới đây là một đề xuất không dùng được.
	for name, answer := range map[string]string{
		"không có JSON":         "Tôi nghĩ nên hỏi monitoring trước.",
		"agent ngoài danh sách": `{"tasks":[{"id":"a","agent":"billing","goal":"g"}]}`,
		"agent bằng id":         `{"tasks":[{"id":"a","agent":"` + mon.String() + `","goal":"g"}]}`,
		"khoá lạ":               `{"tasks":[{"id":"a","agent":"monitoring","goal":"g","budget":"999"}]}`,
		"khoá lạ ở gốc":         `{"tasks":[{"id":"a","agent":"monitoring","goal":"g"}],"max_parallel_tasks":64}`,
		"chu trình":             `{"tasks":[{"id":"a","agent":"monitoring","goal":"g","depends_on":["b"]},{"id":"b","agent":"database","goal":"g","depends_on":["a"]}]}`,
		"không có task":         `{"tasks":[]}`,
		"id sai dạng":           `{"tasks":[{"id":"Task 1","agent":"monitoring","goal":"g"}]}`,
		"on_failure lạ":         `{"tasks":[{"id":"a","agent":"monitoring","goal":"g","on_failure":"retry"}]}`,
		"hai object":            `{"tasks":[{"id":"a","agent":"monitoring","goal":"g"}]} {"tasks":[]}`,
		// Khoá trùng và khoá khác hoa thường: thứ người đọc thấy phải là thứ được đọc ra.
		"khoá tasks trùng":    `{"tasks":[{"id":"a","agent":"monitoring","goal":"g"}],"tasks":[{"id":"b","agent":"database","goal":"g"}]}`,
		"khoá goal trùng":     `{"tasks":[{"id":"a","agent":"monitoring","goal":"vô hại","goal":"khác"}]}`,
		"khoá sai hoa thường": `{"Tasks":[{"id":"a","agent":"monitoring","goal":"g"}]}`,
		"rác sau object":      `{"tasks":[{"id":"a","agent":"monitoring","goal":"g"}]} ] {"tasks":[{"id":"b","agent":"database","goal":"khác"}]}`,
		"lồng quá sâu":        `{"tasks":[{"id":"a","agent":"monitoring","goal":"g","depends_on":[[[[[["x"]]]]]]}]}`,
		"quá lớn":             `{"tasks":[{"id":"a","agent":"monitoring","goal":"` + strings.Repeat("x", maxProposalBytes) + `"}]}`,
	} {
		if plan, problems := ParseProposal(answer, allowed); len(problems) == 0 || len(plan.Tasks) != 0 {
			t.Errorf("%s: được chấp nhận: %+v", name, plan)
		}
	}
}

// retryPlan: up → s (thử tối đa 3 lần: s, s-r2, s-r3) → down. Bước s có on_failure là
// onFailure; hai lần thử đầu mang continue như nơi biên dịch workflow tạo ra.
func retryPlan(t *testing.T, onFailure string) []Node {
	t.Helper()
	agent := uuid.New()
	attempt := func(key, retryOf, fail string, deps ...string) Task {
		return Task{Key: key, AgentID: agent, Goal: "g", DependsOn: deps, OnFailure: fail, RetryOf: retryOf}
	}
	return mustNodes(t, task("up"),
		attempt("s", "", Continue, "up"),
		attempt("s-r2", "s", Continue, "up", "s"),
		attempt("s-r3", "s-r2", onFailure, "up", "s-r2"),
		task("down", "s-r3"))
}

func TestRunRetries(t *testing.T) {
	cases := []struct {
		name      string
		onFailure string
		outcome   map[int]string
		want      []string
		started   []int
		halts     int
		exec      string
	}{
		{"lần đầu thành công: không thử lại, bước sau chạy", SkipDependents, nil,
			[]string{Completed, Completed, Skipped, Skipped, Completed}, []int{0, 1, 4}, 0, Completed},
		{"lần hai thành công", SkipDependents, map[int]string{1: Failed},
			[]string{Completed, Failed, Completed, Skipped, Completed}, []int{0, 1, 2, 4}, 0, Completed},
		{"hết hạn cũng được thử lại; lần ba thành công", SkipDependents, map[int]string{1: TimedOut, 2: Failed},
			[]string{Completed, TimedOut, Failed, Completed, Completed}, []int{0, 1, 2, 3, 4}, 0, Completed},
		{"cả ba lần thất bại: bước sau bị bỏ qua", SkipDependents, map[int]string{1: Failed, 2: Failed, 3: Failed},
			[]string{Completed, Failed, Failed, Failed, Skipped}, []int{0, 1, 2, 3}, 0, Failed},
		{"cả ba lần thất bại với continue: bước sau vẫn chạy", Continue, map[int]string{1: Failed, 2: Failed, 3: Failed},
			[]string{Completed, Failed, Failed, Failed, Completed}, []int{0, 1, 2, 3, 4}, 0, Completed},
		{"cả ba lần thất bại với fail_execution: dừng kế hoạch", FailExecution, map[int]string{1: Failed, 2: Failed, 3: Failed},
			[]string{Completed, Failed, Failed, Failed, ""}, []int{0, 1, 2, 3}, 1, Failed},
		{"hết budget không thử lại", SkipDependents, map[int]string{1: BudgetExceeded},
			[]string{Completed, BudgetExceeded, Skipped, Skipped, Skipped}, []int{0, 1}, 0, BudgetExceeded},
		{"bị huỷ không thử lại; fail_execution dừng kế hoạch", FailExecution, map[int]string{1: Cancelled},
			[]string{Completed, Cancelled, Skipped, Skipped, ""}, []int{0, 1}, 1, Failed},
		{"bước trước thất bại: không lần thử nào chạy", SkipDependents, map[int]string{0: Failed},
			[]string{Failed, Skipped, Skipped, Skipped, Skipped}, []int{0}, 0, Failed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			nodes := retryPlan(t, c.onFailure)
			en := &fakeEngine{outcome: c.outcome}
			got := Run(en, nodes, 4)
			if !slices.Equal(got, c.want) {
				t.Fatalf("status = %v, muốn %v", got, c.want)
			}
			if !slices.Equal(en.started, c.started) || en.halts != c.halts {
				t.Fatalf("started = %v (muốn %v), halts = %d (muốn %d)", en.started, c.started, en.halts, c.halts)
			}
			if st, _ := Aggregate(nodes, got, NotStopped); st != c.exec {
				t.Fatalf("execution = %s, muốn %s", st, c.exec)
			}
		})
	}
}

func TestValidateRetries(t *testing.T) {
	agent := uuid.New()
	a := func(key, retryOf, fail string, deps ...string) Task {
		return Task{Key: key, AgentID: agent, Goal: "g", DependsOn: deps, OnFailure: fail, RetryOf: retryOf}
	}
	other := a("b", "a", "", "a")
	other.AgentID = uuid.New()
	wait := Task{Key: "w", Kind: KindWait, WaitSeconds: 1, OnFailure: Continue}
	cases := []struct {
		name  string
		tasks []Task
		want  string
	}{
		{"hợp lệ", []Task{a("a", "", Continue), a("b", "a", "", "a"), a("c", "", "", "b")}, ""},
		{"không phụ thuộc vào task được thử lại", []Task{a("a", "", Continue), a("b", "a", "")}, "phụ thuộc vào đúng task"},
		{"task không tồn tại", []Task{a("b", "x", "")}, "phụ thuộc vào đúng task"},
		{"Agent khác", []Task{a("a", "", Continue), other}, "cùng Agent"},
		{"thử lại một nút chờ", []Task{wait, a("b", "w", "", "w")}, "cùng Agent"},
		{"task được thử lại không mang continue", []Task{a("a", "", ""), a("b", "a", "", "a")}, "on_failure: continue"},
		{"hai lần thử lại của cùng một task", []Task{a("a", "", Continue), a("b", "a", "", "a"), a("c", "a", "", "a")}, "đã có một lần thử lại"},
		{"quá số lần thử", []Task{a("a", "", Continue), a("b", "a", Continue, "a"), a("c", "b", Continue, "b"), a("d", "c", "", "c")}, "lần thử"},
		{"phụ thuộc vào lần thử chưa phải cuối", []Task{a("a", "", Continue), a("b", "a", "", "a"), a("c", "", "", "a")}, "chưa phải cuối"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Validate(Plan{Tasks: c.tasks})
			if (c.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.want)) {
				t.Fatalf("lỗi = %v, muốn chứa %q", err, c.want)
			}
		})
	}
}

// Một bước đã hoàn thành vẫn là hoàn thành khi execution dừng trước lúc nút thử lại của
// nó được bỏ qua (nút đó bị đóng cancelled / timed_out theo execution).
func TestAggregateRetryAfterStop(t *testing.T) {
	agent := uuid.New()
	nodes := mustNodes(t,
		Task{Key: "a", AgentID: agent, Goal: "g", OnFailure: Continue},
		Task{Key: "a-try2", AgentID: agent, Goal: "g", DependsOn: []string{"a"}, RetryOf: "a"})
	for _, closed := range []string{Cancelled, TimedOut, ""} {
		if st, _ := Aggregate(nodes, []string{Completed, closed}, StopCancelled); st != Completed {
			t.Errorf("nút thử lại %q sau lần thử thành công: execution = %s, muốn completed", closed, st)
		}
	}
	if st, _ := Aggregate(nodes, []string{Failed, Cancelled}, StopCancelled); st != Cancelled {
		t.Errorf("lần thử thất bại rồi bị huỷ: execution = %s, muốn cancelled", st)
	}
}

// Thêm max_attempts cho một bước fail_execution không đổi hành vi của nhánh độc lập: khi
// bước bị bỏ qua vì phụ thuộc thất bại (chưa hề chạy), kế hoạch không bị dừng.
func TestRunRetrySkippedByUpstreamDoesNotHalt(t *testing.T) {
	agent := uuid.New()
	nodes := mustNodes(t, task("x"), task("y"),
		Task{Key: "s", AgentID: agent, Goal: "g", DependsOn: []string{"x"}, OnFailure: Continue},
		Task{Key: "s-try2", AgentID: agent, Goal: "g", DependsOn: []string{"x", "s"}, OnFailure: FailExecution, RetryOf: "s"})
	en := &fakeEngine{outcome: map[int]string{0: Failed}}
	got := Run(en, nodes, 4)
	if en.halts != 0 || !slices.Equal(got, []string{Failed, Completed, Skipped, Skipped}) {
		t.Fatalf("status = %v, halts = %d; muốn y hoàn thành và không Halt", got, en.halts)
	}
}
