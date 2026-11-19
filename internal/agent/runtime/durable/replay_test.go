package durable_test

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"go.temporal.io/sdk/client"
	tlog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/hqgroups/mcp/internal/agent/runtime/durable"
)

// TestReplayRecordedHistories phát lại các lịch sử workflow đã ghi từ Temporal thật
// bằng code hiện tại. Thay đổi làm workflow không còn deterministic với execution
// đang chạy (đổi thứ tự bước, thêm / bớt activity mà không versioning) làm test này
// lỗi. Không cần Docker.
//
// Ghi lại lịch sử mẫu sau một thay đổi CÓ versioning:
//
//	HQ_UPDATE_REPLAY_FIXTURE=1 go test ./tests/api -run TestDurableExecutions
//
// Tên file mang version của mốc versioning (changeControl): *_v0.json là lịch sử của
// execution bắt đầu TRƯỚC mốc (đường cũ, không tạm dừng được) và không bao giờ được ghi
// lại; test chỉ ghi ra *_v1.json. Giữ *_v0.json chừng nào còn execution chạy bằng đường
// đó: chúng là thứ duy nhất bảo vệ nhánh version mặc định.
func TestReplayRecordedHistories(t *testing.T) {
	files, err := filepath.Glob("testdata/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("không có lịch sử mẫu trong testdata: %v", err)
	}
	logger := tlog.NewStructuredLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			r, err := os.Open(f)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = r.Close() }()
			h, err := client.HistoryFromJSON(r, client.HistoryJSONOptions{})
			if err != nil {
				t.Fatal(err)
			}
			rp := worker.NewWorkflowReplayer()
			rp.RegisterWorkflowWithOptions(durable.Workflow, workflow.RegisterOptions{Name: durable.WorkflowName})
			rp.RegisterWorkflowWithOptions(durable.PlanWorkflow, workflow.RegisterOptions{Name: durable.PlanWorkflowName})
			rp.RegisterWorkflowWithOptions(durable.PlanWorkflow, workflow.RegisterOptions{Name: durable.PlanRetryWorkflowName})
			rp.RegisterWorkflowWithOptions(durable.TaskWorkflow, workflow.RegisterOptions{Name: durable.TaskWorkflowName})
			if err := rp.ReplayWorkflowHistory(logger, h); err != nil {
				t.Fatalf("replay: %v", err)
			}
		})
	}
}
