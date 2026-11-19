package api_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hqgroups/mcp/internal/ai/provider"
)

// TestContextCompaction: khi hội thoại vượt ngân sách ngữ cảnh của model, kết quả tool cũ
// được lược bỏ khỏi hội thoại gửi cho model (cũ nhất trước, giữ các lượt gần nhất), còn
// step vẫn giữ kết quả đầy đủ.
func TestContextCompaction(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	adminA := e.tok("admin-a")
	f := newPlannedFixture(t, e, "context-a")
	// Model có cửa sổ ngữ cảnh nhỏ: ngân sách hội thoại = (2000 − 256) token × 3 byte, được
	// nâng lên mức sàn 16 KiB.
	provID := e.do("GET", "/v1/model-providers", adminA, nil, nil).json(t)["data"].([]any)[0].(map[string]any)["id"].(string)
	small := f.post("/v1/models", map[string]any{"provider_id": provID, "provider_model": "small", "display_name": "Small",
		"tier": "economy", "capabilities": map[string]any{"tool_calling": true, "context_window": 2000}})["id"].(string)
	f.post("/v1/models/"+small+"/pricing", map[string]any{"currency": "USD", "input_per_mtok": "1.00", "output_per_mtok": "2.00",
		"effective_from": time.Now().UTC().Format(time.RFC3339Nano)})
	f.agent("reader", []string{"list_containers"}, func(cfg map[string]any) {
		cfg["model"] = map[string]any{"policy": "fixed", "fixed_model_id": small, "max_output_tokens": 256}
		cfg["limits"].(map[string]any)["max_iterations"] = 8
		cfg["limits"].(map[string]any)["max_tool_calls"] = 8
	})
	const marker = "DU-LIEU-LON-"
	f.up.mu.Lock()
	f.up.reply = marker + strings.Repeat("L", 6000)
	f.up.mu.Unlock()
	toolResults := func(req provider.Request) (full, elided int) {
		for _, m := range req.Messages {
			if m.Role != provider.RoleTool {
				continue
			}
			// Kết quả bị lược bỏ giữ phần đầu (có marker) và mang ghi chú của platform.
			if strings.Contains(m.Text, "ĐÃ chạy xong") {
				elided++
			} else if strings.Contains(m.Text, marker) {
				full++
			}
		}
		return full, elided
	}
	e.model.script(func(_ context.Context, n int, _ provider.Request) (provider.Response, error) {
		if n < 5 {
			return toolUse("infra.list_containers"), nil
		}
		return answer("XONG"), nil
	})
	id := f.single(adminA, "reader")
	if x := f.waitStatus(id, 30*time.Second, plannedTerminal...); x["status"] != "completed" {
		t.Fatalf("execution: %v", x)
	}
	seen := e.model.seen()
	if len(seen) != 6 {
		t.Fatalf("số lượt model = %d, muốn 6", len(seen))
	}
	// Lượt 2 (một kết quả, ~6 KB) còn trong ngân sách 16 KiB: không lược bỏ gì.
	if full, elided := toolResults(seen[1]); full != 1 || elided != 0 {
		t.Errorf("lượt 2: %d đầy đủ, %d lược bỏ", full, elided)
	}
	// Lượt cuối (năm kết quả): hai lượt gần nhất giữ nguyên, ba kết quả cũ bị lược bỏ,
	// và kết quả bị lược bỏ là các kết quả CŨ NHẤT.
	last := seen[5]
	if full, elided := toolResults(last); full != 2 || elided != 3 {
		t.Fatalf("lượt cuối: %d đầy đủ, %d lược bỏ; muốn 2 và 3", full, elided)
	}
	var order []bool
	for _, m := range last.Messages {
		if m.Role == provider.RoleTool {
			order = append(order, !strings.Contains(m.Text, "ĐÃ chạy xong"))
			// Ghi chú không được gợi ý lặp lại hành động, và phần đầu của kết quả được giữ.
			if strings.Contains(m.Text, "ĐÃ chạy xong") && (!strings.HasPrefix(m.Text, "KET-QUA") && !strings.HasPrefix(m.Text, marker)) {
				t.Errorf("kết quả bị lược bỏ mất phần đầu: %q", m.Text[:60])
			}
		}
	}
	if len(order) != 5 || order[0] || order[1] || order[2] || !order[3] || !order[4] {
		t.Errorf("thứ tự lược bỏ: %v", order)
	}
	if last.Messages[0].Text != "Kiểm tra production" || last.System != "AGENT:reader" {
		t.Errorf("lượt mở đầu / system prompt bị đổi: %q / %q", last.Messages[0].Text, last.System)
	}
	// Step vẫn giữ kết quả đầy đủ, và lượt model ghi lại số kết quả đã lược bỏ.
	task := f.allTasks(id)[0]["id"].(string)
	var fullSteps, elidedSteps int
	if err := e.db.Admin.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE kind = 'tool_call' AND length(response->>'text') > 6000),
		count(*) FILTER (WHERE kind = 'llm_call' AND (routing->>'context_elided')::int > 0)
		FROM execution_steps WHERE task_id = $1`, task).Scan(&fullSteps, &elidedSteps); err != nil {
		t.Fatal(err)
	}
	if fullSteps != 5 || elidedSteps == 0 {
		t.Errorf("step: %d kết quả đầy đủ (muốn 5), %d lượt ghi nhận lược bỏ", fullSteps, elidedSteps)
	}
}
