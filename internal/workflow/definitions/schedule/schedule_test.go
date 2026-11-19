package schedule

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"go.temporal.io/sdk/client"
	tlog "go.temporal.io/sdk/log"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

func TestValid(t *testing.T) {
	for _, ok := range []string{"0 7 * * *", "*/5 * * * *", "*/15 9-17 * * 1-5", "30 2 1 * *", "0 0,12 * * *", "0 */6 * * 0"} {
		if err := Valid(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "* * * * *", "*/4 * * * *", "0,4 * * * *", "58,0 * * * *", "@daily", "@every 1m", "0 7 * * MON", "0 7 * JAN *",
		"0 0 7 * * *", "0 7 * *", " 0 7 * * *", "0  7 * * *", "TZ=UTC 0 7 * * *", "60 7 * * *", "0 24 * * *", "0 7 32 * *", "0 7 * 13 *", "0 7 * * ?",
		"0 7 31 2 *"} {
		if err := Valid(bad); err == nil {
			t.Errorf("%q được chấp nhận", bad)
		}
	}
}

// TestValidKey: khoá của một lần kích hoạt phải là workflow ID mà lịch của đúng workflow
// trong tham chiếu sinh ra.
func TestValidKey(t *testing.T) {
	ref := Ref{OrgID: uuid.MustParse("0190a000-0000-7000-8000-000000000001"), WorkflowID: uuid.MustParse("0190a000-0000-7000-8000-000000000002")}
	other := uuid.MustParse("0190a000-0000-7000-8000-000000000003")
	base := runID(ref.OrgID, ref.WorkflowID)
	if !ValidKey(ref, base+"-2026-11-18T07:00:00Z") {
		t.Error("khoá của lịch bị từ chối")
	}
	for _, bad := range []string{"", base, base + "-", base + "-x", base + "-2026-11-18T07:00:00Z-1", "forged-" + uuid.NewString(),
		runID(other, ref.WorkflowID) + "-2026-11-18T07:00:00Z", runID(ref.OrgID, other) + "-2026-11-18T07:00:00Z"} {
		if ValidKey(ref, bad) {
			t.Errorf("%q được chấp nhận", bad)
		}
	}
	if ValidKey(Ref{}, runID(uuid.Nil, uuid.Nil)+"-2026-11-18T07:00:00Z") {
		t.Error("tham chiếu rỗng được chấp nhận")
	}
}

// TestReplayRecordedHistories phát lại lịch sử mẫu của workflow kích hoạt lịch: đổi
// chuỗi lệnh của Run mà không versioning sẽ làm test này lỗi. Ghi lại lịch sử mẫu bằng
// HQ_UPDATE_REPLAY_FIXTURE=1 go test ./tests/api -run TestWorkflowSchedules.
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
			rp.RegisterWorkflowWithOptions(Run, workflow.RegisterOptions{Name: WorkflowName})
			if err := rp.ReplayWorkflowHistory(logger, h); err != nil {
				t.Fatalf("replay: %v", err)
			}
		})
	}
}
