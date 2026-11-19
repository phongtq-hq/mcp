package gateway

// Bước 5 của pipeline: quyết định một tool call được chạy, bị từ chối, hay cần được
// duyệt (security §4.2, §4.3). Đây là hàm thuần; mức sàn của platform là code ở đây,
// cấu hình của org và của Agent chỉ có thể chặt hơn.

const (
	effectAllow           = "allow"
	effectDeny            = "deny"
	effectRequireApproval = "require_approval"

	riskRead        = "read"
	riskWrite       = "write"
	riskDestructive = "destructive"
	riskFinancial   = "financial"

	envProd = "prod"
)

// policyInput là những gì quyết định cần.
type policyInput struct {
	Enabled     bool
	Risk        string
	Environment string // environment của project: prod | non_prod
	// Internal: lời gọi của một task của Agent nội bộ (có execution, task và step để
	// gắn approval). Client MCP bên ngoài thì không.
	Internal bool
	// AgentRequiresApproval: Agent Version đặt require_approval: always cho tool này.
	AgentRequiresApproval bool
	// Delegated: task được một Agent khác giao việc (delegation). Việc nó làm bắt nguồn từ
	// một `goal` do model khác sinh ra, không phải từ yêu cầu trực tiếp của người dùng.
	Delegated bool
	// ModelPlanned: task thuộc một kế hoạch do Agent lập kế hoạch đề xuất và platform chạy
	// thẳng từ đề xuất: `goal` của nó cũng là output của model.
	ModelPlanned bool
}

// policyDecision là kết quả, kèm quy tắc đã khớp (ghi vào audit).
type policyDecision struct {
	Effect string
	Rule   string
}

// decide áp ma trận rủi ro. Phase 2 mở tool ghi trên non-prod:
//
//	read          tự do (mọi môi trường)
//	write         non-prod: tự do; prod: chưa mở ở phase này
//	destructive   non-prod: một người duyệt, không phải người yêu cầu; prod: chưa mở
//	financial     chưa mở (cần hai người duyệt, xác thực lại, hạn mức số tiền)
//
// Tool không chỉ đọc chỉ chạy được trong một execution: approval và việc chống chạy
// trùng đều gắn với (task, step). Mức rủi ro lạ bị từ chối.
func decide(in policyInput) policyDecision {
	if !in.Enabled {
		return policyDecision{effectDeny, "tool_disabled"}
	}
	switch in.Risk {
	case riskRead:
		if in.AgentRequiresApproval && in.Internal {
			return policyDecision{effectRequireApproval, "agent_requires_approval"}
		}
		return policyDecision{effectAllow, "risk_read"}
	case riskWrite, riskDestructive:
		if !in.Internal {
			return policyDecision{effectDeny, "risk_" + in.Risk + "_requires_execution"}
		}
		if in.Environment == envProd {
			return policyDecision{effectDeny, "risk_" + in.Risk + "_production_not_enabled"}
		}
		if in.Risk == riskDestructive {
			return policyDecision{effectRequireApproval, "risk_destructive_requires_approval"}
		}
		if in.AgentRequiresApproval {
			return policyDecision{effectRequireApproval, "agent_requires_approval"}
		}
		// Mức sàn của platform: tool ghi do một task được giao việc gọi luôn cần người
		// duyệt. Không có nó, prompt injection ở một Agent chỉ đọc sẽ kéo được tool ghi
		// của Agent mà nó giao việc cho, không ai thấy.
		if in.Delegated {
			return policyDecision{effectRequireApproval, "delegated_write_requires_approval"}
		}
		if in.ModelPlanned {
			return policyDecision{effectRequireApproval, "model_planned_write_requires_approval"}
		}
		return policyDecision{effectAllow, "risk_write_non_prod"}
	case riskFinancial:
		return policyDecision{effectDeny, "risk_financial_not_supported"}
	}
	return policyDecision{effectDeny, "risk_unknown"}
}
