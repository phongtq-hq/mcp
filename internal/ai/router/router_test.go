package router

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/hqgroups/mcp/internal/ai/modelcatalog"
)

func TestParseConfig(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	ok := fmt.Sprintf(`{"other":1,"model_routing":{"tiers":{"standard":{"models":["%s","%s"]},"economy":{"models":["%s"]}}}}`, a, b, b)
	cfg, err := ParseConfig(json.RawMessage(ok))
	if err != nil || len(cfg.Tiers) != 2 || cfg.Tiers[TierStandard].Models[0] != a || cfg.Tiers[TierStandard].Models[1] != b {
		t.Fatalf("cfg = %+v, err = %v", cfg, err)
	}
	for name, raw := range map[string]string{
		"không có khoá":  `{"other":1}`,
		"null":           `{"model_routing":null}`,
		"settings rỗng":  ``,
		"không có tiers": `{"model_routing":{}}`,
	} {
		if cfg, err := ParseConfig(json.RawMessage(raw)); err != nil || len(cfg.Tiers) != 0 {
			t.Errorf("%s: cfg = %+v, err = %v", name, cfg, err)
		}
	}
	five := ""
	for i := 0; i < 5; i++ {
		five += fmt.Sprintf(`,"%s"`, uuid.New())
	}
	for name, raw := range map[string]string{
		"tier lạ":                    fmt.Sprintf(`{"model_routing":{"tiers":{"ultra":{"models":["%s"]}}}}`, a),
		"không có model":             `{"model_routing":{"tiers":{"standard":{"models":[]}}}}`,
		"quá nhiều model":            `{"model_routing":{"tiers":{"standard":{"models":[` + five[1:] + `]}}}}`,
		"model lặp":                  fmt.Sprintf(`{"model_routing":{"tiers":{"standard":{"models":["%s","%s"]}}}}`, a, a),
		"model rỗng":                 fmt.Sprintf(`{"model_routing":{"tiers":{"standard":{"models":["%s"]}}}}`, uuid.Nil),
		"không phải uuid":            `{"model_routing":{"tiers":{"standard":{"models":["claude-sonnet"]}}}}`,
		"khoá lạ":                    fmt.Sprintf(`{"model_routing":{"tiers":{"standard":{"models":["%s"],"weight":2}}}}`, a),
		"khoá lạ ở gốc":              `{"model_routing":{"tiers":{},"strategy":"smart"}}`,
		"không phải object":          `{"model_routing":[1]}`,
		"khoá Tiers viết hoa":        fmt.Sprintf(`{"model_routing":{"Tiers":{"standard":{"models":["%s"]}}}}`, a),
		"khoá Models viết hoa":       fmt.Sprintf(`{"model_routing":{"tiers":{"standard":{"Models":["%s"]}}}}`, a),
		"khoá trùng khác hoa thường": fmt.Sprintf(`{"model_routing":{"tiers":{"standard":{"models":["%s"]}},"Tiers":{"standard":{"models":["%s"]}}}}`, a, b),
		"tier là null":               `{"model_routing":{"tiers":{"standard":null}}}`,
	} {
		if _, err := ParseConfig(json.RawMessage(raw)); err == nil {
			t.Errorf("%s: được chấp nhận", name)
		}
	}
}

func TestChainProjectOverridesOrgPerTier(t *testing.T) {
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	org := Config{Tiers: map[string]TierConfig{TierStandard: {Models: []uuid.UUID{a, b}}, TierEconomy: {Models: []uuid.UUID{b}}}}
	proj := Config{Tiers: map[string]TierConfig{TierStandard: {Models: []uuid.UUID{c}}}}
	if got := Chain(org, proj, TierStandard); len(got) != 1 || got[0] != c {
		t.Errorf("standard: %v, muốn chuỗi của project", got)
	}
	if got := Chain(org, proj, TierEconomy); len(got) != 1 || got[0] != b {
		t.Errorf("economy: %v, muốn chuỗi của organization", got)
	}
	if got := Chain(org, proj, TierPremium); len(got) != 0 {
		t.Errorf("premium: %v, muốn rỗng", got)
	}
}

func TestSelectKeepsOrderAndAppliesHardConstraints(t *testing.T) {
	ids := []uuid.UUID{uuid.New(), uuid.New(), uuid.New(), uuid.New()}
	chain := []Candidate{
		{ID: ids[0], Reason: "model đang bị tắt"},
		{ID: ids[1], Model: modelcatalog.Usable{ID: ids[1]}}, // không hỗ trợ tool calling
		{ID: ids[2], Model: modelcatalog.Usable{ID: ids[2], ToolCalling: true}},
		{ID: ids[3], Model: modelcatalog.Usable{ID: ids[3], ToolCalling: true}},
	}
	d := Select(TierStandard, Requirements{ToolCalling: true}, chain)
	if len(d.Models) != 2 || d.Models[0].ID != ids[2] || d.Models[1].ID != ids[3] || len(d.Skipped) != 2 || d.Skipped[0].ModelID != ids[0] {
		t.Errorf("cần tool: %+v", d)
	}
	// Cửa sổ ngữ cảnh: model khai báo cửa sổ nhỏ hơn mức cần bị loại; model không khai báo thì không.
	small := []Candidate{
		{ID: ids[0], Model: modelcatalog.Usable{ID: ids[0], ContextWindow: 2000}},
		{ID: ids[1], Model: modelcatalog.Usable{ID: ids[1]}},
		{ID: ids[2], Model: modelcatalog.Usable{ID: ids[2], ContextWindow: 200000}},
	}
	if d := Select(TierStandard, Requirements{MinContextTokens: 8192}, small); len(d.Models) != 2 || d.Models[0].ID != ids[1] || len(d.Skipped) != 1 {
		t.Errorf("context window: %+v", d)
	}
	d = Select(TierStandard, Requirements{}, chain)
	if len(d.Models) != 3 || d.Models[0].ID != ids[1] || len(d.Skipped) != 1 {
		t.Errorf("không cần tool: %+v", d)
	}
}

func TestTierFor(t *testing.T) {
	for purpose, tier := range map[string]string{PurposeAgentStep: TierStandard, PurposeSummarization: TierEconomy,
		PurposePlanning: TierPremium, PurposeAggregation: TierPremium, "": TierStandard, "lạ": TierStandard} {
		if got := TierFor(purpose); got != tier {
			t.Errorf("%q → %s, muốn %s", purpose, got, tier)
		}
	}
}
