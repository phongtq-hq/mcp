package gateway

import (
	"encoding/json"
	"testing"
)

// Ma trận rủi ro: test dạng bảng cho mọi tổ hợp, gồm cả mức sàn mà cấu hình của Agent
// không nới được.
func TestPolicyMatrix(t *testing.T) {
	type c struct {
		risk, env        string
		internal, always bool
		effect, rule     string
	}
	for _, tc := range []c{
		{"read", "non_prod", false, false, effectAllow, "risk_read"},
		{"read", "prod", false, false, effectAllow, "risk_read"},
		{"read", "prod", true, false, effectAllow, "risk_read"},
		{"read", "non_prod", true, true, effectRequireApproval, "agent_requires_approval"},
		// Client MCP bên ngoài không có execution để gắn approval: require_approval của Agent không áp, tool đọc vẫn tự do.
		{"read", "non_prod", false, true, effectAllow, "risk_read"},

		{"write", "non_prod", true, false, effectAllow, "risk_write_non_prod"},
		{"write", "non_prod", true, true, effectRequireApproval, "agent_requires_approval"},
		{"write", "non_prod", false, false, effectDeny, "risk_write_requires_execution"},
		{"write", "prod", true, false, effectDeny, "risk_write_production_not_enabled"},
		{"write", "prod", true, true, effectDeny, "risk_write_production_not_enabled"},
		{"write", "prod", false, false, effectDeny, "risk_write_requires_execution"},

		{"destructive", "non_prod", true, false, effectRequireApproval, "risk_destructive_requires_approval"},
		{"destructive", "non_prod", true, true, effectRequireApproval, "risk_destructive_requires_approval"},
		{"destructive", "non_prod", false, false, effectDeny, "risk_destructive_requires_execution"},
		{"destructive", "prod", true, false, effectDeny, "risk_destructive_production_not_enabled"},

		{"financial", "non_prod", true, false, effectDeny, "risk_financial_not_supported"},
		{"financial", "non_prod", true, true, effectDeny, "risk_financial_not_supported"},
		{"financial", "prod", true, false, effectDeny, "risk_financial_not_supported"},

		{"", "non_prod", true, false, effectDeny, "risk_unknown"},
		{"admin", "non_prod", true, false, effectDeny, "risk_unknown"},
		// Môi trường lạ không được coi là non-prod "tự do" cho destructive.
		{"destructive", "staging", true, false, effectRequireApproval, "risk_destructive_requires_approval"},
	} {
		got := decide(policyInput{Enabled: true, Risk: tc.risk, Environment: tc.env, Internal: tc.internal, AgentRequiresApproval: tc.always})
		if got.Effect != tc.effect || got.Rule != tc.rule {
			t.Errorf("%+v → %+v", tc, got)
		}
	}
	// Mức sàn cho task được giao việc (delegation): tool ghi luôn cần người duyệt; tool đọc
	// không đổi; những gì đã bị từ chối vẫn bị từ chối.
	for _, tc := range []c{
		{"read", "non_prod", true, false, effectAllow, "risk_read"},
		{"write", "non_prod", true, false, effectRequireApproval, "delegated_write_requires_approval"},
		{"write", "non_prod", true, true, effectRequireApproval, "agent_requires_approval"},
		{"write", "prod", true, false, effectDeny, "risk_write_production_not_enabled"},
		{"destructive", "non_prod", true, false, effectRequireApproval, "risk_destructive_requires_approval"},
		{"financial", "non_prod", true, false, effectDeny, "risk_financial_not_supported"},
	} {
		got := decide(policyInput{Enabled: true, Risk: tc.risk, Environment: tc.env, Internal: tc.internal, AgentRequiresApproval: tc.always, Delegated: true})
		if got.Effect != tc.effect || got.Rule != tc.rule {
			t.Errorf("delegated %+v → %+v", tc, got)
		}
	}
	// Cùng mức sàn cho task của một kế hoạch do model đề xuất.
	if got := decide(policyInput{Enabled: true, Risk: "write", Environment: "non_prod", Internal: true, ModelPlanned: true}); got.Effect != effectRequireApproval || got.Rule != "model_planned_write_requires_approval" {
		t.Errorf("model planned write → %+v", got)
	}
	if got := decide(policyInput{Enabled: true, Risk: "read", Environment: "non_prod", Internal: true, ModelPlanned: true}); got.Effect != effectAllow {
		t.Errorf("model planned read → %+v", got)
	}
	// Tool bị tắt bị từ chối bất kể mức rủi ro.
	for _, risk := range []string{"read", "write", "destructive", "financial"} {
		if got := decide(policyInput{Enabled: false, Risk: risk, Environment: "non_prod", Internal: true}); got.Effect != effectDeny || got.Rule != "tool_disabled" {
			t.Errorf("tool tắt, %s → %+v", risk, got)
		}
	}
}

// Mức sàn về dữ liệu (security §5.4): tool sensitive chỉ chạy trong execution; execution đã
// đọc dữ liệu sensitive thì tool egress cần người duyệt; không gì đã bị từ chối được nới ra.
func TestDataFlowFloors(t *testing.T) {
	for name, tc := range map[string]struct {
		in           policyInput
		effect, rule string
		disabled     bool
	}{
		"sensitive gọi trực tiếp":          {policyInput{Risk: "read", Sensitive: true}, effectDeny, "sensitive_requires_execution", false},
		"sensitive trong execution":        {policyInput{Risk: "read", Sensitive: true, Internal: true}, effectAllow, "risk_read", false},
		"egress chưa đọc sensitive":        {policyInput{Risk: "read", Internal: true, Egress: true}, effectAllow, "risk_read", false},
		"egress sau sensitive":             {policyInput{Risk: "read", Internal: true, Egress: true, ReadSensitive: true}, effectRequireApproval, "sensitive_then_egress_requires_approval", false},
		"egress ghi sau sensitive":         {policyInput{Risk: "write", Environment: "non_prod", Internal: true, Egress: true, ReadSensitive: true}, effectRequireApproval, "sensitive_then_egress_requires_approval", false},
		"tool không egress sau sensitive":  {policyInput{Risk: "read", Internal: true, ReadSensitive: true}, effectAllow, "risk_read", false},
		"egress destructive vẫn cần duyệt": {policyInput{Risk: "destructive", Environment: "non_prod", Internal: true, Egress: true, ReadSensitive: true}, effectRequireApproval, "risk_destructive_requires_approval", false},
		"egress production vẫn bị từ chối": {policyInput{Risk: "write", Environment: "prod", Internal: true, Egress: true, ReadSensitive: true}, effectDeny, "risk_write_production_not_enabled", false},
		"sensitive tắt vẫn là tắt":         {policyInput{Risk: "read", Sensitive: true}, effectDeny, "tool_disabled", true},
	} {
		in := tc.in
		in.Enabled = !tc.disabled
		if got := decide(in); got.Effect != tc.effect || got.Rule != tc.rule {
			t.Errorf("%s: %+v", name, got)
		}
	}
}

// Số nguyên ngoài ±2^53 bị làm tròn khi giải mã JSON: tool sẽ chạy với giá trị khác giá
// trị được yêu cầu. Chúng bị từ chối thay vì âm thầm đổi.
func TestExactNumbers(t *testing.T) {
	for raw, ok := range map[string]bool{
		`{"n":1}`:                          true,
		`{"n":-12.5,"m":[1,2,{"k":0}]}`:    true,
		`{"n":9007199254740992}`:           true, // đúng 2^53
		`{"n":9007199254740993}`:           false,
		`{"n":-9007199254740993}`:          false,
		`{"a":{"b":[123456789012345678]}}`: false,
		`{"s":"9007199254740993"}`:         true, // chuỗi không phải số
		`{"n":1e3}`:                        true,
	} {
		if err := exactNumbers(json.RawMessage(raw)); (err == nil) != ok {
			t.Errorf("%s: err = %v, muốn hợp lệ = %v", raw, err, ok)
		}
	}
}
