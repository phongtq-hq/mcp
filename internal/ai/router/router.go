// Package router là Model Router theo luật và tier (ADR-0010, cost-management §5).
//
// Agent và platform tham chiếu tier (economy, standard, premium), không tham chiếu tên
// model. Ánh xạ tier → chuỗi model (model chính rồi các model dự phòng) là cấu hình
// trong settings của organization và project, khoá "model_routing"; project ghi đè
// organization theo từng tier. Router không dùng LLM để chọn và không biết gì về Agent.
package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/hqgroups/mcp/internal/ai/modelcatalog"
	apperr "github.com/hqgroups/mcp/internal/shared/errors"
)

// Tier hợp lệ, theo thứ tự tăng dần.
const (
	TierEconomy  = "economy"
	TierStandard = "standard"
	TierPremium  = "premium"

	// SettingsKey là khoá của cấu hình định tuyến trong settings của org / project.
	SettingsKey = "model_routing"
	// MaxChain là số model tối đa của một tier: model chính và ba model dự phòng.
	MaxChain = 4
)

// Tiers là các tier hợp lệ.
var Tiers = []string{TierEconomy, TierStandard, TierPremium}

// Purpose của một lời gọi model. Tier mặc định theo purpose (cost-management §5.2).
const (
	PurposeAgentStep     = "agent_step"
	PurposeSummarization = "summarization"
	PurposePlanning      = "planning"
	PurposeAggregation   = "aggregation"
)

// TierFor trả về tier mặc định của một purpose.
func TierFor(purpose string) string {
	switch purpose {
	case PurposeSummarization:
		return TierEconomy
	case PurposePlanning, PurposeAggregation:
		return TierPremium
	default:
		return TierStandard
	}
}

// Config là cấu hình định tuyến của một phạm vi (organization hoặc project).
type Config struct {
	Tiers map[string]TierConfig `json:"tiers"`
}

// TierConfig là chuỗi model của một tier, model chính đứng đầu.
type TierConfig struct {
	Models []uuid.UUID `json:"models"`
}

// invalid là lỗi validate của khoá model_routing. Nó gắn với trường `settings` của thân
// request (PATCH organization / project): thông báo đã nêu đường dẫn đầy đủ bên trong.
func invalid(msg string) error { return apperr.InvalidField("settings", msg) }

// ParseConfig đọc khoá model_routing từ settings. Settings không có khoá này trả Config rỗng.
func ParseConfig(settings json.RawMessage) (Config, error) {
	if len(settings) == 0 {
		return Config{}, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(settings, &obj); err != nil {
		return Config{}, invalid("settings phải là một JSON object")
	}
	raw, ok := obj[SettingsKey]
	if !ok || string(raw) == "null" {
		return Config{}, nil
	}
	// Giải mã từng tầng qua map với tên khoá CHÍNH XÁC. encoding/json khớp tên trường
	// không phân biệt hoa thường; nếu dùng nó, "Tiers" và "tiers" cùng có mặt sẽ được
	// kiểm một đằng (lúc ghi) và dùng một nẻo (sau khi jsonb sắp lại khoá).
	bad := invalid("settings.model_routing không đúng định dạng: {\"tiers\": {\"<tier>\": {\"models\": [\"<model id>\", ...]}}}")
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil || top == nil {
		return Config{}, bad
	}
	cfg := Config{Tiers: map[string]TierConfig{}}
	for key, val := range top {
		if key != "tiers" {
			return Config{}, bad
		}
		var tiers map[string]json.RawMessage
		if err := json.Unmarshal(val, &tiers); err != nil {
			return Config{}, bad
		}
		for tier, tval := range tiers {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(tval, &fields); err != nil || fields == nil {
				return Config{}, bad
			}
			var tc TierConfig
			for field, fval := range fields {
				if field != "models" || json.Unmarshal(fval, &tc.Models) != nil {
					return Config{}, bad
				}
			}
			cfg.Tiers[tier] = tc
		}
	}
	for tier, tc := range cfg.Tiers {
		if !slices.Contains(Tiers, tier) {
			return Config{}, invalid("settings.model_routing.tiers: tier phải là economy, standard hoặc premium")
		}
		if len(tc.Models) == 0 || len(tc.Models) > MaxChain {
			return Config{}, invalid(fmt.Sprintf("settings.model_routing.tiers.%s.models phải có từ 1 đến %d model", tier, MaxChain))
		}
		seen := map[uuid.UUID]bool{}
		for _, id := range tc.Models {
			if id == uuid.Nil || seen[id] {
				return Config{}, invalid(fmt.Sprintf("settings.model_routing.tiers.%s.models chứa model rỗng hoặc lặp lại", tier))
			}
			seen[id] = true
		}
	}
	return cfg, nil
}

// Chain trả về chuỗi model của một tier: cấu hình của project nếu có, không thì của
// organization. Hàm thuần.
func Chain(org, project Config, tier string) []uuid.UUID {
	if tc, ok := project.Tiers[tier]; ok {
		return tc.Models
	}
	return org.Tiers[tier].Models
}

// Requirements là ràng buộc cứng của một lời gọi lên model.
type Requirements struct {
	// ToolCalling: lời gọi kèm tool, model phải hỗ trợ tool calling.
	ToolCalling bool
	// MinContextTokens: cửa sổ ngữ cảnh tối thiểu (0 = không ràng buộc). Model không
	// khai báo context_window không bị loại theo ràng buộc này.
	MinContextTokens int
}

// Candidate là một model trong chuỗi cùng kết quả kiểm tra của Model Catalog.
type Candidate struct {
	ID     uuid.UUID
	Model  modelcatalog.Usable
	Reason string // khác rỗng: model không dùng được, kèm lý do
}

// Skipped là một model trong chuỗi bị bỏ qua.
type Skipped struct {
	ModelID uuid.UUID `json:"model_id"`
	Reason  string    `json:"reason"`
}

// Decision là kết quả định tuyến: các model dùng được theo thứ tự (model chính rồi
// dự phòng) và những model bị bỏ qua.
type Decision struct {
	Tier    string
	Models  []modelcatalog.Usable
	Skipped []Skipped
}

// Select áp ràng buộc cứng lên chuỗi ứng viên, giữ nguyên thứ tự. Hàm thuần.
func Select(tier string, req Requirements, chain []Candidate) Decision {
	d := Decision{Tier: tier}
	for _, c := range chain {
		switch {
		case c.Reason != "":
			d.Skipped = append(d.Skipped, Skipped{ModelID: c.ID, Reason: c.Reason})
		case req.ToolCalling && !c.Model.ToolCalling:
			d.Skipped = append(d.Skipped, Skipped{ModelID: c.ID, Reason: "model không hỗ trợ tool calling"})
		case req.MinContextTokens > 0 && c.Model.ContextWindow > 0 && c.Model.ContextWindow < req.MinContextTokens:
			d.Skipped = append(d.Skipped, Skipped{ModelID: c.ID, Reason: "cửa sổ ngữ cảnh của model quá nhỏ cho lời gọi này"})
		default:
			d.Models = append(d.Models, c.Model)
		}
	}
	return d
}

// Settings là thứ router cần từ Identity & Tenancy: settings của organization hiện
// tại và của một project.
type Settings interface {
	OrganizationSettings(ctx context.Context) (json.RawMessage, error)
	ProjectSettings(ctx context.Context, projectID uuid.UUID) (json.RawMessage, error)
}

// Models là thứ router cần từ Model Catalog.
type Models interface {
	ResolveForUse(ctx context.Context, id uuid.UUID) (modelcatalog.Usable, error)
}

// Service nạp cấu hình và trạng thái model rồi gọi phần thuần của router.
type Service struct {
	settings Settings
	models   Models
}

// NewService tạo Service.
func NewService(settings Settings, models Models) *Service {
	return &Service{settings: settings, models: models}
}

// Route chọn chuỗi model cho một lời gọi thuộc tier đã cho trong một project. Trả lỗi
// validation nếu tier chưa được cấu hình. Decision có thể không còn model nào dùng
// được; người gọi quyết định cách báo lỗi.
func (s *Service) Route(ctx context.Context, projectID uuid.UUID, tier string, req Requirements) (Decision, error) {
	if !slices.Contains(Tiers, tier) {
		return Decision{}, invalid("tier phải là economy, standard hoặc premium")
	}
	orgRaw, err := s.settings.OrganizationSettings(ctx)
	if err != nil {
		return Decision{}, err
	}
	projRaw, err := s.settings.ProjectSettings(ctx, projectID)
	if err != nil {
		return Decision{}, err
	}
	// Cấu hình được kiểm khi ghi qua API. Nếu vẫn hỏng (ghi bằng đường khác, hoặc có từ
	// trước khi có router) thì dừng: âm thầm bỏ qua cấu hình của project sẽ làm nó chạy
	// bằng model của organization mà không ai biết.
	org, err := ParseConfig(orgRaw)
	if err != nil {
		slog.ErrorContext(ctx, "router: settings.model_routing của organization không hợp lệ", "error", err)
		return Decision{}, invalid("cấu hình model_routing của organization không hợp lệ; ghi lại settings để sửa")
	}
	proj, err := ParseConfig(projRaw)
	if err != nil {
		slog.ErrorContext(ctx, "router: settings.model_routing của project không hợp lệ", "error", err, "project_id", projectID)
		return Decision{}, invalid("cấu hình model_routing của project không hợp lệ; ghi lại settings để sửa")
	}
	ids := Chain(org, proj, tier)
	if len(ids) == 0 {
		return Decision{}, invalid("chưa cấu hình model cho tier " + tier + " (settings.model_routing của organization hoặc project)")
	}
	chain := make([]Candidate, 0, len(ids))
	for _, id := range ids {
		c := Candidate{ID: id}
		u, err := s.models.ResolveForUse(ctx, id)
		switch e, ok := apperr.From(err); {
		case err == nil:
			c.Model = u
		case ok && e.Kind == apperr.KindUnprocessable:
			c.Reason = e.Message // model bị tắt, ngừng hỗ trợ, provider tắt, adapter không có
		default:
			return Decision{}, err
		}
		chain = append(chain, c)
	}
	return Select(tier, req, chain), nil
}

// ValidateSettings kiểm khoá model_routing của một settings sắp được ghi: đúng định
// dạng và mọi model tồn tại trong tầm nhìn của tenant. Model đang tắt vẫn được chấp
// nhận: nó bị bỏ qua lúc định tuyến. Trả về chi tiết cho audit: chuỗi model của từng
// tier (id của model, không phải dữ liệu nhạy cảm), để thay đổi định tuyến truy được.
func (s *Service) ValidateSettings(ctx context.Context, settings json.RawMessage) (map[string]string, error) {
	cfg, err := ParseConfig(settings)
	if err != nil {
		return nil, err
	}
	details := map[string]string{}
	for _, tier := range Tiers {
		tc, ok := cfg.Tiers[tier]
		if !ok {
			continue
		}
		ids := make([]string, len(tc.Models))
		for i, id := range tc.Models {
			ids[i] = id.String()
			_, err := s.models.ResolveForUse(ctx, id)
			switch e, ok := apperr.From(err); {
			case errors.Is(err, modelcatalog.ErrModelNotFound):
				return nil, invalid(fmt.Sprintf("settings.model_routing.tiers.%s.models: model %s không tồn tại", tier, id))
			case err == nil, ok && e.Kind == apperr.KindUnprocessable:
				// dùng được, hoặc tồn tại nhưng đang tắt: chấp nhận
			default:
				return nil, err
			}
		}
		details["model_routing."+tier] = strings.Join(ids, ",")
	}
	return details, nil
}
