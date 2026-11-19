package api_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/client"
	tlog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/hqgroups/mcp/internal/adminops"
	"github.com/hqgroups/mcp/internal/agent/runtime"
	"github.com/hqgroups/mcp/internal/agent/runtime/durable"
	"github.com/hqgroups/mcp/internal/ai/provider"
	"github.com/hqgroups/mcp/internal/mcp/registry"
	"github.com/hqgroups/mcp/internal/shared/signedctx"
)

// Cùng digest với deployments/compose/docker-compose.yml.
const temporalImage = "temporalio/temporal@sha256:b228dadf9cdb6c7c02a1394b89dd76f827aac9760e458fce42f0899a10141225"

var quiet = tlog.NewStructuredLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))

func startTemporal(t *testing.T) client.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image: temporalImage,
			Cmd: []string{"server", "start-dev", "--ip", "0.0.0.0", "--headless",
				"--search-attribute", "OrgId=Keyword", "--search-attribute", "ProjectId=Keyword"},
			ExposedPorts: []string{"7233/tcp"},
			WaitingFor:   wait.ForListeningPort("7233/tcp").WithStartupTimeout(90 * time.Second),
		},
		Started: true,
	})
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("start temporal: %v", err)
	}
	endpoint, err := ctr.PortEndpoint(ctx, "7233/tcp", "")
	if err != nil {
		t.Fatal(err)
	}
	// Cổng mở trước khi server sẵn sàng: thử nối lại, có trần.
	var c client.Client
	for i := 0; i < 60; i++ {
		if c, err = client.DialContext(ctx, client.Options{HostPort: endpoint, Logger: quiet, Identity: "test-api"}); err == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("dial temporal: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

// startWorker chạy một worker của shared runtime; hàm trả về dừng nó. stopTimeout ngắn
// để mô phỏng worker chết giữa chừng: activity đang chạy bị bỏ dở, không kịp ghi gì.
func (e *env) startWorker(stopTimeout time.Duration) (stop func()) {
	e.t.Helper()
	w := durable.NewWorker(e.temporal, e.steps, func(o *worker.Options) {
		o.WorkerStopTimeout = stopTimeout
		o.Identity = "test-worker" // không để pid@hostname của máy chạy test vào lịch sử mẫu
	})
	if err := w.Start(); err != nil {
		e.t.Fatalf("worker: %v", err)
	}
	stopped := false
	stop = func() {
		if !stopped {
			stopped = true
			w.Stop()
		}
	}
	e.workerStops = append(e.workerStops, stop)
	e.t.Cleanup(stop)
	return stop
}

// stopWorkers dừng mọi worker test đã khởi động.
func (e *env) stopWorkers() {
	for _, stop := range e.workerStops {
		stop()
	}
	e.workerStops = nil
}

func (e *env) history(orgID uuid.UUID, execID string) *historypb.History {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	it := e.temporal.GetWorkflowHistory(ctx, durable.WorkflowID(orgID, uuid.MustParse(execID)), "", false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	h := &historypb.History{}
	for it.HasNext() {
		ev, err := it.Next()
		if err != nil {
			e.t.Fatalf("history: %v", err)
		}
		h.Events = append(h.Events, ev)
	}
	return h
}

// TestDurableExecutions chạy vòng lặp trên Temporal thật với worker dưới role
// app_worker: execution tiếp tục sau khi worker chết, không ghi trùng step hay usage,
// không gọi lại tool đã chạy; huỷ lan tới activity đang chạy; lịch sử workflow chỉ
// chứa tham chiếu và phát lại được.
func TestDurableExecutions(t *testing.T) {
	e := setup(t, true)
	ctx := context.Background()
	adminA := e.tok("admin-a")
	signer, _ := signedctx.NewSigner(signingKey)
	conn, err := e.db.Admin.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()

	const toolSecretText = "KET-QUA-TOOL-rat-dai-khong-duoc-vao-history"
	up := newUpstream(t, "docker", readTool("list_containers", "Liệt kê container."),
		writeTool("create_ticket", "Tạo ticket."), writeTool("restart_container", "Khởi động lại container."))
	up.reply = toolSecretText
	if _, err := adminops.RegisterManagedMCPServer(ctx, conn.Conn(), signer, adminops.MCPServer{OrgSlug: "org-a", Slug: "docker"}, "Docker", up.srv.URL); err != nil {
		t.Fatal(err)
	}
	enable := true
	for name, risk := range map[string]string{"create_ticket": "write", "restart_container": "destructive"} {
		risk, side := risk, "non_idempotent"
		if err := adminops.ClassifyMCPTool(ctx, conn.Conn(), adminops.MCPServer{OrgSlug: "org-a", Slug: "docker"}, name,
			registry.Classification{Enabled: &enable, RiskLevel: &risk, SideEffect: &side}); err != nil {
			t.Fatal(err)
		}
	}
	post := func(path string, body any, status int) map[string]any {
		t.Helper()
		r := e.do("POST", path, adminA, body, nil)
		if r.status != status {
			t.Fatalf("POST %s: %d %s", path, r.status, r.body)
		}
		return r.json(t)
	}
	serverID := e.do("GET", "/v1/mcp-servers", adminA, nil, nil).json(t)["data"].([]any)[0].(map[string]any)["id"].(string)
	provID := post("/v1/model-providers", map[string]any{"slug": "claude-local", "adapter": "claude-cli"}, 201)["id"].(string)
	modelID := post("/v1/models", map[string]any{"provider_id": provID, "provider_model": "sonnet", "display_name": "Sonnet",
		"tier": "standard", "capabilities": map[string]any{"tool_calling": true, "context_window": 200000}}, 201)["id"].(string)
	post("/v1/models/"+modelID+"/pricing", map[string]any{"currency": "USD", "input_per_mtok": "3.00", "output_per_mtok": "15.00",
		"effective_from": time.Now().UTC().Format(time.RFC3339Nano)}, 201)
	proj := e.createProject(adminA, "durable-a", "Durable A")["id"].(string)
	const systemPrompt = "SYSTEM-PROMPT-khong-duoc-vao-history"
	a := post("/v1/projects/"+proj+"/agents", map[string]any{"slug": "devops", "name": "devops", "system_prompt": systemPrompt,
		"config": map[string]any{
			"model":  map[string]any{"policy": "fixed", "fixed_model_id": modelID},
			"limits": map[string]any{"max_iterations": 4, "max_tool_calls": 6, "timeout_seconds": 180},
			"tools":  []any{map[string]any{"server_id": serverID, "allow": []string{"*"}}},
		}}, 201)
	agentID := a["id"].(string)
	if r := e.do("PUT", "/v1/agents/"+agentID+"/active-version", adminA, map[string]any{"version": 1}, nil); r.status != 200 {
		t.Fatalf("publish: %d %s", r.status, r.body)
	}

	const userMessage = "YEU-CAU-NGUOI-DUNG-khong-duoc-vao-history"
	start := func() string {
		t.Helper()
		r := e.do("POST", "/v1/projects/"+proj+"/executions", adminA, map[string]any{"mode": "single_agent", "agent_id": agentID,
			"input": map[string]any{"message": userMessage}}, map[string]string{"Idempotency-Key": uuid.NewString()})
		if r.status != 202 {
			t.Fatalf("start: %d %s", r.status, r.body)
		}
		return r.json(t)["id"].(string)
	}
	waitDone := func(id string, within time.Duration) map[string]any {
		t.Helper()
		deadline := time.Now().Add(within)
		for time.Now().Before(deadline) {
			x := e.do("GET", "/v1/executions/"+id, adminA, nil, nil).json(t)
			if s := x["status"]; s != "queued" && s != "running" && s != "waiting_approval" {
				return x
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("execution %s không kết thúc trong %s", id, within)
		return nil
	}
	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := e.db.Admin.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Chạy trước khi có worker nào: test gọi thẳng các bước, như một activity.
	t.Run("a paid model turn is recorded even when its step was closed; forged references are rejected", func(t *testing.T) {
		id := start()
		var ref runtime.TaskRef
		if err := e.db.Admin.QueryRow(ctx, `SELECT t.org_id, x.project_id, x.id, t.id, t.agent_id, t.agent_version_id, x.initiator_type, x.initiator_id, x.deadline_at, t.created_at
			FROM agent_tasks t JOIN agent_executions x ON x.id = t.execution_id WHERE x.id = $1`, id).Scan(&ref.OrgID, &ref.ProjectID, &ref.ExecutionID,
			&ref.TaskID, &ref.AgentID, &ref.AgentVersionID, &ref.InitiatorType, &ref.InitiatorID, &ref.Deadline, &ref.Since); err != nil {
			t.Fatal(err)
		}
		if err := e.steps.Begin(ctx, ref); err != nil {
			t.Fatalf("begin: %v", err)
		}
		in := runtime.CallModelInput{Task: ref, StepSeq: 1, MaxOutputTokens: 100,
			Model: runtime.ModelRef{ID: uuid.MustParse(modelID), Adapter: "claude-cli", ProviderModel: "sonnet"}}
		steps := func() int {
			return count(`SELECT count(*) FROM execution_steps WHERE task_id = $1`, ref.TaskID)
		}

		// Tham chiếu giả (đi qua Temporal) không mở được step nào.
		for name, mutate := range map[string]func(r *runtime.TaskRef){
			"since sau lúc tạo task": func(r *runtime.TaskRef) { r.Since = time.Now().Add(time.Hour) },
			"execution khác":         func(r *runtime.TaskRef) { r.ExecutionID = uuid.New() },
			"agent version khác":     func(r *runtime.TaskRef) { r.AgentVersionID = uuid.New() },
			"task của org khác":      func(r *runtime.TaskRef) { r.OrgID = e.orgB },
		} {
			forged := in
			mutate(&forged.Task)
			if _, err := e.steps.CallModel(ctx, forged, runtime.ModelRetry{}); err == nil || !runtime.Permanent(err) {
				t.Errorf("%s: err = %v, muốn bị từ chối vĩnh viễn", name, err)
			}
		}
		// Model ngoài chuỗi đã chọn lúc tạo task cũng bị từ chối.
		outside := in
		outside.Model.ID = uuid.New()
		if _, err := e.steps.CallModel(ctx, outside, runtime.ModelRetry{}); err == nil || !runtime.Permanent(err) {
			t.Errorf("model ngoài chuỗi của task: err = %v", err)
		}
		outside = in
		outside.Fallbacks = []runtime.ModelRef{{ID: uuid.New(), Adapter: "claude-cli", ProviderModel: "khác"}}
		if _, err := e.steps.CallModel(ctx, outside, runtime.ModelRetry{}); err == nil || !runtime.Permanent(err) {
			t.Errorf("model dự phòng ngoài chuỗi của task: err = %v", err)
		}
		if n := steps(); n != 0 || len(e.model.seen()) != 0 {
			t.Fatalf("tham chiếu giả đã mở %d step, gọi model %d lần", n, len(e.model.seen()))
		}
		forged := ref
		forged.InitiatorID = uuid.New()
		if err := e.steps.Finish(ctx, forged, runtime.Outcome{Status: runtime.StatusCompleted}); err == nil || !runtime.Permanent(err) {
			t.Errorf("finish với người khởi tạo giả: %v", err)
		}
		if err := e.steps.Finish(ctx, ref, runtime.Outcome{Status: "queued"}); err == nil {
			t.Error("finish nhận trạng thái không phải trạng thái kết thúc")
		}

		// Lượt model đang chạy thì task bị đóng (huỷ); provider vẫn trả lời sau đó.
		entered, release := make(chan struct{}), make(chan struct{})
		e.model.script(func(context.Context, int, provider.Request) (provider.Response, error) {
			close(entered)
			<-release
			return answer("trả lời muộn"), nil
		})
		done := make(chan error, 1)
		go func() {
			_, err := e.steps.CallModel(ctx, in, runtime.ModelRetry{})
			done <- err
		}()
		<-entered
		if err := e.steps.Finish(ctx, ref, runtime.Outcome{Status: runtime.StatusCancelled, ErrorMessage: "VĂN BẢN GIẢ"}); err != nil {
			t.Fatalf("finish: %v", err)
		}
		close(release)
		if err := <-done; err == nil || !runtime.Permanent(err) {
			t.Errorf("lượt model muộn: err = %v, muốn lỗi vĩnh viễn (step đã đóng)", err)
		}
		if n := count(`SELECT count(*) FROM usage_records WHERE execution_id = $1 AND input_tokens = 2000`, id); n != 1 {
			t.Errorf("usage của lượt model muộn = %d dòng, muốn 1", n)
		}
		if n := count(`SELECT count(*) FROM execution_steps WHERE task_id = $1 AND status = 'failed'`, ref.TaskID); n != 1 || steps() != 1 {
			t.Errorf("step: %d failed / %d tổng, muốn 1 / 1", n, steps())
		}
		x := e.do("GET", "/v1/executions/"+id, adminA, nil, nil)
		if x.json(t)["status"] != "cancelled" || strings.Contains(string(x.body), "VĂN BẢN GIẢ") {
			t.Errorf("execution: %s", x.body)
		}
		// Chạy lại bước đã đóng không gọi model lần nữa.
		before := len(e.model.seen())
		if _, err := e.steps.CallModel(ctx, in, runtime.ModelRetry{}); err == nil || len(e.model.seen()) != before {
			t.Errorf("chạy lại step đã đóng: err = %v, model được gọi thêm %d lần", err, len(e.model.seen())-before)
		}
	})

	// Cũng chạy trước khi có worker: gọi thẳng bước CallTool như một activity bị ngắt rồi chạy lại.
	t.Run("an interrupted write step is never reported as failed or not executed, however often it is retried", func(t *testing.T) {
		id := start()
		var ref runtime.TaskRef
		if err := e.db.Admin.QueryRow(ctx, `SELECT t.org_id, x.project_id, x.id, t.id, t.agent_id, t.agent_version_id, x.initiator_type, x.initiator_id, x.deadline_at, t.created_at
			FROM agent_tasks t JOIN agent_executions x ON x.id = t.execution_id WHERE x.id = $1`, id).Scan(&ref.OrgID, &ref.ProjectID, &ref.ExecutionID,
			&ref.TaskID, &ref.AgentID, &ref.AgentVersionID, &ref.InitiatorType, &ref.InitiatorID, &ref.Deadline, &ref.Since); err != nil {
			t.Fatal(err)
		}
		if err := e.steps.Begin(ctx, ref); err != nil {
			t.Fatalf("begin: %v", err)
		}
		e.model.script(func(context.Context, int, provider.Request) (provider.Response, error) {
			return toolUse("docker.create_ticket"), nil
		})
		if _, err := e.steps.CallModel(ctx, runtime.CallModelInput{Task: ref, StepSeq: 1, MaxOutputTokens: 100, Tools: []string{"docker.create_ticket"},
			Model: runtime.ModelRef{ID: uuid.MustParse(modelID), Adapter: "claude-cli", ProviderModel: "sonnet"}}, runtime.ModelRetry{}); err != nil {
			t.Fatalf("call model: %v", err)
		}
		in := runtime.CallToolInput{Task: ref, StepSeq: 2, ModelStepSeq: 1, Call: runtime.CallRef{ID: "c0", Name: "docker.create_ticket"}}
		calls := func() int {
			up.mu.Lock()
			defer up.mu.Unlock()
			n := 0
			for _, c := range up.calls {
				if strings.HasPrefix(c, "create_ticket ") {
					n++
				}
			}
			return n
		}
		before := calls()
		// Lần một: tool đã tới upstream, rồi bước bị ngắt trước khi có câu trả lời.
		up.mu.Lock()
		up.delay = 2 * time.Second
		up.mu.Unlock()
		short, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		_, err := e.steps.CallTool(short, in, runtime.ToolRetry{})
		cancel()
		up.mu.Lock()
		up.delay = 0
		up.mu.Unlock()
		if err == nil || calls() != before+1 {
			t.Fatalf("lần một: err = %v, tool được gọi %d lần", err, calls()-before)
		}
		if n := count(`SELECT count(*) FROM execution_steps WHERE task_id = $1 AND step_seq = 2 AND status = 'started'`, ref.TaskID); n != 1 {
			t.Fatalf("sau khi bị ngắt, step phải còn mở")
		}
		// Chạy lại (lần thử sau của activity, kể cả ở worker khác): Gateway không gọi lại
		// tool; kết quả là "không xác định", không phải "thất bại".
		for i := 0; i < 3; i++ {
			out, err := e.steps.CallTool(ctx, in, runtime.ToolRetry{LastAttempt: i > 0})
			if err != nil || !out.Unknown {
				t.Errorf("chạy lại lần %d: %+v %v, muốn Unknown", i+1, out, err)
			}
		}
		if calls() != before+1 {
			t.Errorf("tool ghi chạy %d lần cho một bước", calls()-before)
		}
		if n := count(`SELECT count(*) FROM execution_steps WHERE task_id = $1 AND step_seq = 2 AND status = 'unknown'`, ref.TaskID); n != 1 {
			t.Errorf("step không ở trạng thái unknown")
		}
		// Nếu vẫn có lượt model sau đó, nó không được nghe "tool chưa được thực thi".
		e.model.script(func(_ context.Context, _ int, req provider.Request) (provider.Response, error) {
			last := req.Messages[len(req.Messages)-1]
			if strings.Contains(last.Text, "không được thực thi") || !strings.Contains(last.Text, "Không xác định") {
				t.Errorf("model thấy kết quả của step unknown là: %q", last.Text)
			}
			return answer("dừng"), nil
		})
		if _, err := e.steps.CallModel(ctx, runtime.CallModelInput{Task: ref, StepSeq: 3, MaxOutputTokens: 100,
			Model: runtime.ModelRef{ID: uuid.MustParse(modelID), Adapter: "claude-cli", ProviderModel: "sonnet"}}, runtime.ModelRetry{}); err != nil {
			t.Fatalf("call model: %v", err)
		}
		if err := e.steps.Finish(ctx, ref, runtime.Outcome{Status: runtime.StatusFailed, ErrorCode: runtime.CodeToolOutcomeUnknown}); err != nil {
			t.Fatalf("finish: %v", err)
		}
	})

	t.Run("worker dies mid model call; another worker finishes without duplicates", func(t *testing.T) {
		entered := make(chan struct{}, 1)
		e.model.script(func(ctx context.Context, n int, _ provider.Request) (provider.Response, error) {
			switch n {
			case 0: // lượt đầu treo cho tới khi worker bị dừng
				entered <- struct{}{}
				<-ctx.Done()
				return provider.Response{}, ctx.Err()
			case 1:
				return toolUse("docker.list_containers"), nil
			default:
				return answer("xong"), nil
			}
		})
		kill := e.startWorker(500 * time.Millisecond)
		id := start()
		select {
		case <-entered:
		case <-time.After(30 * time.Second):
			t.Fatal("worker không nhận execution")
		}
		// Step của lượt model đã được mở nhưng chưa có kết quả.
		if n := count(`SELECT count(*) FROM execution_steps s JOIN agent_tasks t ON t.id = s.task_id WHERE t.execution_id = $1 AND s.status = 'started'`, id); n != 1 {
			t.Fatalf("step đang mở = %d, muốn 1", n)
		}
		kill()
		if x := e.do("GET", "/v1/executions/"+id, adminA, nil, nil).json(t); x["status"] != "running" {
			t.Fatalf("sau khi worker chết, execution phải còn running: %v", x["status"])
		}

		e.startWorker(5 * time.Second)
		x := waitDone(id, 90*time.Second)
		if x["status"] != "completed" {
			t.Fatalf("execution: %v %v", x["status"], x["error"])
		}
		// Ba step, mỗi step_seq đúng một dòng; hai lượt model thành công → hai usage.
		if n := count(`SELECT count(*) FROM execution_steps s JOIN agent_tasks t ON t.id = s.task_id WHERE t.execution_id = $1`, id); n != 3 {
			t.Errorf("số step = %d, muốn 3", n)
		}
		if n := count(`SELECT count(*) FROM (SELECT step_seq FROM execution_steps s JOIN agent_tasks t ON t.id = s.task_id
			WHERE t.execution_id = $1 GROUP BY step_seq HAVING count(*) > 1) d`, id); n != 0 {
			t.Errorf("có %d step_seq bị ghi trùng", n)
		}
		if n := count(`SELECT count(*) FROM execution_steps s JOIN agent_tasks t ON t.id = s.task_id WHERE t.execution_id = $1 AND s.status <> 'succeeded'`, id); n != 0 {
			t.Errorf("còn %d step chưa succeeded", n)
		}
		if n := count(`SELECT count(*) FROM usage_records WHERE execution_id = $1`, id); n != 2 {
			t.Errorf("usage_records = %d, muốn 2", n)
		}
		up.mu.Lock()
		calls := 0
		for _, c := range up.calls {
			if strings.HasPrefix(c, "list_containers ") {
				calls++
			}
		}
		up.mu.Unlock()
		if calls != 1 {
			t.Errorf("tool được gọi %d lần, muốn 1", calls)
		}
		// Lượt model sau thấy đúng hội thoại dựng lại từ step: yêu cầu, tool call, kết quả tool.
		seen := e.model.seen()
		last := seen[len(seen)-1]
		if last.System != systemPrompt || len(last.Messages) != 3 || last.Messages[0].Text != userMessage ||
			last.Messages[2].Role != provider.RoleTool || !strings.Contains(last.Messages[2].Text, toolSecretText) {
			t.Errorf("hội thoại dựng lại sai: system=%q messages=%+v", last.System, last.Messages)
		}

		// Lịch sử workflow chỉ chứa tham chiếu.
		h := e.history(e.orgA, id)
		raw, err := protojson.Marshal(h)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{userMessage, systemPrompt, toolSecretText, "xong"} {
			if bytes.Contains(raw, []byte(secret)) {
				t.Errorf("lịch sử workflow chứa nội dung %q", secret)
			}
		}
		// Phát lại lịch sử bằng code hiện tại: workflow deterministic.
		rp := worker.NewWorkflowReplayer()
		rp.RegisterWorkflowWithOptions(durable.Workflow, workflow.RegisterOptions{Name: durable.WorkflowName})
		if err := rp.ReplayWorkflowHistory(quiet, h); err != nil {
			t.Errorf("replay: %v", err)
		}
		// Cập nhật lịch sử mẫu cho test replay trong CI (internal/agent/runtime/durable).
		if os.Getenv("HQ_UPDATE_REPLAY_FIXTURE") == "1" {
			// Tên máy ghi lịch sử nằm trong tên sticky queue; thay nó trước khi commit.
			if host, err := os.Hostname(); err == nil && host != "" {
				raw = bytes.ReplaceAll(raw, []byte(host), []byte("fixture-host"))
			}
			//nolint:gosec // lịch sử chỉ chứa tham chiếu, là dữ liệu test được commit
			if err := os.WriteFile("../../internal/agent/runtime/durable/testdata/single_agent_tool_call_v1.json", raw, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	})

	t.Run("cancel reaches the running activity", func(t *testing.T) {
		entered := make(chan struct{}, 1)
		e.model.script(func(ctx context.Context, _ int, _ provider.Request) (provider.Response, error) {
			entered <- struct{}{}
			<-ctx.Done()
			return provider.Response{}, ctx.Err()
		})
		id := start()
		select {
		case <-entered:
		case <-time.After(30 * time.Second):
			t.Fatal("worker không nhận execution")
		}
		began := time.Now()
		if r := e.do("POST", "/v1/executions/"+id+"/cancel", adminA, nil, nil); r.status != 202 && r.status != 200 {
			t.Fatalf("cancel: %d %s", r.status, r.body)
		}
		x := waitDone(id, 30*time.Second)
		if x["status"] != "cancelled" {
			t.Fatalf("execution: %v %v", x["status"], x["error"])
		}
		t.Logf("huỷ lan tới activity sau %s", time.Since(began).Round(time.Millisecond))
		if n := count(`SELECT count(*) FROM agent_tasks WHERE execution_id = $1 AND status <> 'cancelled'`, id); n != 0 {
			t.Errorf("còn %d task chưa cancelled", n)
		}
		// Lượt model bị ngắt không để lại step treo, và không sinh usage.
		if n := count(`SELECT count(*) FROM execution_steps s JOIN agent_tasks t ON t.id = s.task_id WHERE t.execution_id = $1 AND s.status = 'started'`, id); n != 0 {
			t.Errorf("còn %d step đang mở sau khi huỷ", n)
		}
		if n := count(`SELECT count(*) FROM usage_records WHERE execution_id = $1`, id); n != 0 {
			t.Errorf("usage_records = %d, muốn 0", n)
		}
	})

	toolCalls := func(tool string) int {
		up.mu.Lock()
		defer up.mu.Unlock()
		n := 0
		for _, c := range up.calls {
			if strings.HasPrefix(c, tool+" ") {
				n++
			}
		}
		return n
	}

	t.Run("a destructive tool waits for approval across the workflow; the decision wakes it up", func(t *testing.T) {
		e.model.script(func(_ context.Context, n int, _ provider.Request) (provider.Response, error) {
			if n == 0 {
				return toolUse("docker.restart_container"), nil
			}
			return answer("đã khởi động lại"), nil
		})
		id := start()
		var approvalID string
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline) && approvalID == ""; time.Sleep(50 * time.Millisecond) {
			if data := e.do("GET", "/v1/approvals?status=pending&execution_id="+id, adminA, nil, nil).json(t)["data"].([]any); len(data) == 1 {
				approvalID = data[0].(map[string]any)["id"].(string)
			}
		}
		if approvalID == "" {
			t.Fatal("không có approval nào được tạo")
		}
		// Gateway tạo approval trước, vòng lặp ghi waiting_approval ngay sau đó: hai việc
		// không cùng transaction, nên trạng thái được chờ trong một khoảng ngắn có trần.
		x := e.do("GET", "/v1/executions/"+id, adminA, nil, nil).json(t)
		for deadline := time.Now().Add(10 * time.Second); x["status"] != "waiting_approval" && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			x = e.do("GET", "/v1/executions/"+id, adminA, nil, nil).json(t)
		}
		if x["status"] != "waiting_approval" || toolCalls("restart_container") != 0 {
			t.Fatalf("trước khi duyệt: %v, tool chạy %d lần", x["status"], toolCalls("restart_container"))
		}
		decided := time.Now()
		r := e.do("POST", "/v1/approvals/"+approvalID+"/decisions", e.tok("multi-a"), map[string]any{"decision": "approve"},
			map[string]string{"Idempotency-Key": uuid.NewString()})
		want(t, r, 200, "")
		x = waitDone(id, 20*time.Second)
		// Workflow thức dậy nhờ signal, không phải đợi tới lần tự kiểm tra định kỳ (30 giây).
		if took := time.Since(decided); x["status"] != "completed" || took > 15*time.Second {
			t.Fatalf("execution: %v %v sau %s", x["status"], x["error"], took)
		}
		if toolCalls("restart_container") != 1 {
			t.Errorf("tool destructive chạy %d lần, muốn 1", toolCalls("restart_container"))
		}
		// Lịch sử workflow không chứa arguments hay trả lời của model, và vẫn phát lại được.
		h := e.history(e.orgA, id)
		raw, err := protojson.Marshal(h)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte(`"name":"web"`)) || bytes.Contains(raw, []byte("đã khởi động lại")) {
			t.Error("lịch sử workflow chứa arguments của tool hoặc trả lời của model")
		}
		rp := worker.NewWorkflowReplayer()
		rp.RegisterWorkflowWithOptions(durable.Workflow, workflow.RegisterOptions{Name: durable.WorkflowName})
		if err := rp.ReplayWorkflowHistory(quiet, h); err != nil {
			t.Errorf("replay: %v", err)
		}
		if os.Getenv("HQ_UPDATE_REPLAY_FIXTURE") == "1" {
			if host, err := os.Hostname(); err == nil && host != "" {
				raw = bytes.ReplaceAll(raw, []byte(host), []byte("fixture-host"))
			}
			//nolint:gosec // lịch sử chỉ chứa tham chiếu, là dữ liệu test được commit
			if err := os.WriteFile("../../internal/agent/runtime/durable/testdata/single_agent_approval_v1.json", raw, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	})

	t.Run("worker dies while a write tool is in flight: the tool is not run a second time", func(t *testing.T) {
		// Tool ghi được upstream ghi nhận ngay, rồi trả lời chậm: worker bị giết trong lúc chờ.
		up.mu.Lock()
		up.delay = 3 * time.Second
		up.mu.Unlock()
		defer func() {
			up.mu.Lock()
			up.delay = 0
			up.mu.Unlock()
		}()
		e.model.script(func(_ context.Context, n int, _ provider.Request) (provider.Response, error) {
			if n == 0 {
				return toolUse("docker.create_ticket"), nil
			}
			return answer("xong"), nil
		})
		// Thay worker đang chạy bằng một worker sẽ chết ngay khi bị dừng (không kịp chờ tool trả lời).
		e.stopWorkers()
		e.startWorker(200 * time.Millisecond)
		before := toolCalls("create_ticket")
		id := start()
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline) && toolCalls("create_ticket") == before; time.Sleep(20 * time.Millisecond) {
		}
		if toolCalls("create_ticket") != before+1 {
			t.Fatal("tool ghi chưa được gọi")
		}
		// Giết mọi worker đang chạy khi tool còn đang trả lời, rồi dựng worker mới.
		e.stopWorkers()
		e.startWorker(5 * time.Second)
		// Worker mới chạy lại bước đó: Gateway thấy bước đã được giữ chỗ và không gọi tool
		// lần hai; task dừng vì không biết kết quả.
		x := waitDone(id, 90*time.Second)
		if toolCalls("create_ticket") != before+1 {
			t.Errorf("tool ghi chạy %d lần cho một bước", toolCalls("create_ticket")-before)
		}
		errObj, _ := x["error"].(map[string]any)
		if x["status"] != "failed" || errObj["code"] != "tool_outcome_unknown" {
			t.Errorf("execution: %v %v, muốn failed / tool_outcome_unknown", x["status"], x["error"])
		}
		if n := count(`SELECT count(*) FROM execution_steps s JOIN agent_tasks t ON t.id = s.task_id
			WHERE t.execution_id = $1 AND s.kind = 'tool_call' AND s.status = 'unknown'`, id); n != 1 {
			t.Errorf("step UNKNOWN: %d", n)
		}
	})

	t.Run("cancel while the provider is answering: the workflow waits and the turn is billed", func(t *testing.T) {
		entered, release := make(chan struct{}, 1), make(chan struct{})
		e.model.script(func(context.Context, int, provider.Request) (provider.Response, error) {
			entered <- struct{}{}
			<-release // provider không dừng theo yêu cầu huỷ
			return answer("muộn"), nil
		})
		id := start()
		select {
		case <-entered:
		case <-time.After(30 * time.Second):
			t.Fatal("worker không nhận execution")
		}
		if r := e.do("POST", "/v1/executions/"+id+"/cancel", adminA, nil, nil); r.status != 202 && r.status != 200 {
			t.Fatalf("cancel: %d %s", r.status, r.body)
		}
		time.Sleep(300 * time.Millisecond)
		close(release)
		if x := waitDone(id, 60*time.Second); x["status"] != "cancelled" {
			t.Fatalf("execution: %v %v", x["status"], x["error"])
		}
		if n := count(`SELECT count(*) FROM usage_records WHERE execution_id = $1`, id); n != 1 {
			t.Errorf("usage_records = %d, muốn 1 (lượt đã có kết quả phải được ghi)", n)
		}
		if n := count(`SELECT count(*) FROM execution_steps s JOIN agent_tasks t ON t.id = s.task_id WHERE t.execution_id = $1 AND s.status = 'started'`, id); n != 0 {
			t.Errorf("còn %d step đang mở", n)
		}
	})

	t.Run("per-org concurrency cap", func(t *testing.T) {
		e.model.script(func(ctx context.Context, _ int, _ provider.Request) (provider.Response, error) {
			<-ctx.Done()
			return provider.Response{}, ctx.Err()
		})
		// 24 request đồng thời: đúng 8 được nhận, phần còn lại bị từ chối. Trần được đếm
		// dưới khoá theo org nên loạt request song song không lọt qua.
		var (
			mu       sync.Mutex
			ids      []string
			rejected int
			wg       sync.WaitGroup
		)
		for i := 0; i < 24; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r := e.do("POST", "/v1/projects/"+proj+"/executions", adminA, map[string]any{"mode": "single_agent", "agent_id": agentID,
					"input": map[string]any{"message": "song song"}}, map[string]string{"Idempotency-Key": uuid.NewString()})
				mu.Lock()
				defer mu.Unlock()
				switch r.status {
				case 202:
					ids = append(ids, r.json(t)["id"].(string))
				case 429:
					rejected++
				default:
					t.Errorf("start: %d %s", r.status, r.body)
				}
			}()
		}
		wg.Wait()
		if len(ids) != 8 || rejected != 16 {
			t.Errorf("nhận %d, từ chối %d; muốn 8 và 16", len(ids), rejected)
		}
		for _, id := range ids {
			e.do("POST", "/v1/executions/"+id+"/cancel", adminA, nil, nil)
		}
		for _, id := range ids {
			if x := waitDone(id, 60*time.Second); x["status"] != "cancelled" {
				t.Errorf("execution %s: %v", id, x["status"])
			}
		}
	})
}
