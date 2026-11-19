package gateway

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strconv"

	"github.com/google/uuid"
	pgxv5 "github.com/jackc/pgx/v5"

	"github.com/hqgroups/mcp/internal/ai/provider"
	"github.com/hqgroups/mcp/internal/audit"
	"github.com/hqgroups/mcp/internal/platform/identity"
	"github.com/hqgroups/mcp/internal/platform/tenancy"
	apperr "github.com/hqgroups/mcp/internal/shared/errors"
	"github.com/hqgroups/mcp/internal/shared/httpx"
)

// Đường gọi model của Agent nội bộ.
//
// Runtime (process api ở Phase 1, worker từ Phase 2) không giữ khoá của LLM provider:
// chỉ Gateway giải mã được secret (ADR-0008). Vì vậy một lượt gọi model đi qua đây
// bằng execution token của task; Gateway lấy khoá của org (hoặc của platform), gọi
// provider đúng một lượt, và trả kết quả. Vòng lặp, việc ghi step và usage vẫn
// thuộc về runtime.

// MaxModelRequestBytes là trần thân request của một lượt gọi model: hội thoại của
// task (tối đa 4 MiB ở runtime) cộng định nghĩa tool.
const MaxModelRequestBytes = 8 << 20

// ModelRoute là nơi một model được phục vụ.
type ModelRoute struct {
	Adapter       string
	ProviderModel string
	// Platform: provider của platform, dùng khoá của platform.
	Platform bool
	// SecretID: secret chứa khoá của org; nil nếu provider của org chưa có khoá.
	SecretID *uuid.UUID
}

// Models là thứ Gateway cần từ Model Catalog và Agent Catalog để gọi model.
type Models interface {
	// AgentModel trả về giới hạn của một Agent Version và model được ghim của nó
	// (uuid.Nil nếu version dùng tier).
	AgentModel(ctx context.Context, agentVersionID uuid.UUID) (AgentModel, error)
	// Route trả về nơi phục vụ model, nếu nó dùng được ngay lúc này.
	Route(ctx context.Context, modelID uuid.UUID) (ModelRoute, error)
}

// AgentModel là model được ghim và các giới hạn của một Agent Version.
type AgentModel struct {
	ModelID         uuid.UUID
	MaxOutputTokens int // 0 = dùng mặc định của platform
	MaxIterations   int
}

// Mã lỗi của đường gọi model mà runtime dựa vào để quyết định có chuyển model hay không.
const (
	CodeProviderUnavailable = "provider_unavailable" // tạm thời: thử model kế tiếp, rồi thử lại
	CodeModelUnavailable    = "model_unavailable"    // model này không dùng được: thử model kế tiếp
	CodeProviderRejected    = "provider_rejected"    // yêu cầu bị từ chối: không chuyển model
)

// defaultMaxOutputTokens khớp với mặc định của runtime khi Agent Version không đặt.
const defaultMaxOutputTokens = 4096

const permExecutionCreate = "execution:create"

// SecretReader là thứ Gateway cần từ Secrets.
type SecretReader interface {
	Get(ctx context.Context, id uuid.UUID, purpose string) ([]byte, error)
}

// ProviderFactory dựng adapter LLM từ một khoá. Khoá không được ghi vào log hay lỗi.
type ProviderFactory func(adapter, apiKey string) (provider.Provider, error)

// LLMConfig bật đường gọi model.
type LLMConfig struct {
	Models  Models
	Secrets SecretReader // nil: chỉ dùng được khoá của platform
	// PlatformKeys là khoá của platform theo adapter.
	PlatformKeys map[string]string
	Factory      ProviderFactory
	// Budget đặt trước và quyết toán chi phí của từng lượt gọi (cost-management §4.2).
	// Gateway là nơi giữ khoá và thực sự tiêu tiền, nên budget được thực thi ở đây,
	// không dựa vào Runtime. nil: không kiểm budget.
	Budget ModelBudget
}

// BudgetCall là một lượt gọi model sắp thực hiện, cho việc kiểm budget.
type BudgetCall struct {
	OrgID, ProjectID, AgentID uuid.UUID
	TaskID                    uuid.UUID
	UserID                    *uuid.UUID // nil nếu người khởi tạo không phải user
	ModelID                   uuid.UUID
	InputBytes                int
	MaxOutputTokens           int
	// TaskLimit (rỗng = không có) và TaskSpent: budget của task và mức nó đã tiêu.
	TaskLimit, TaskSpent string
}

// ModelBudget là thứ Gateway cần từ Budget Management. Lỗi mã budget_exceeded nghĩa
// là một budget đã chạm ngưỡng cứng; lỗi khác nghĩa là không kiểm được (fail-closed).
// Hàm settle phải được gọi đúng một lần với usage thật (rỗng nếu lượt gọi không có kết quả).
type ModelBudget interface {
	Reserve(ctx context.Context, c BudgetCall) (settle func(ctx context.Context, u provider.Usage), err error)
}

// EnableLLM bật đường gọi model. Cần EnableInternal trước.
func (g *Gateway) EnableLLM(cfg LLMConfig) { g.llm = &cfg }

// RegisterLLMRoutes gắn endpoint gọi model. Như route nội bộ của MCP, nó chỉ nhận
// execution token.
func (g *Gateway) RegisterLLMRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /llm/internal/v1/tasks/{task}/complete", g.serveModel)
}

// ModelCall là thân request của một lượt gọi model.
type ModelCall struct {
	ModelID uuid.UUID        `json:"model_id"`
	Request provider.Request `json:"request"`
}

var (
	// errModelUnavailable: model này không phục vụ được (bị tắt, provider bị tắt, thiếu
	// hoặc sai khoá). Lỗi riêng của một model: runtime chuyển sang model kế tiếp trong
	// chuỗi của task thay vì coi cả lượt là hỏng.
	errModelUnavailable = apperr.New(apperr.KindUnprocessable, CodeModelUnavailable, "model hiện không phục vụ được")
	errNoCredential     = errModelUnavailable
	errCredential       = apperr.New(apperr.KindInternal, "internal_error", "") // lỗi credential không lộ chi tiết
)

func (g *Gateway) serveModel(w http.ResponseWriter, r *http.Request) {
	if g.llm == nil {
		httpx.WriteError(w, r, errNotFound)
		return
	}
	ctx, p, task, taskID, ok := g.authenticateTask(w, r, classModel)
	if !ok {
		return
	}
	// Gateway không tin Runtime: người khởi tạo phải còn quyền chạy execution trong
	// project, và giới hạn của Agent Version được áp lại ở đây.
	if err := g.authz.Require(ctx, permExecutionCreate, &task.ProjectID); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	var call ModelCall
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxModelRequestBytes))
	if err := dec.Decode(&call); err != nil || dec.More() {
		httpx.WriteError(w, r, apperr.New(apperr.KindInvalid, "invalid_request", "thân request không đọc được hoặc quá lớn"))
		return
	}
	// Token chỉ cho phép gọi các model đã chọn cho task lúc nó được tạo (model được ghim
	// của Agent Version, hoặc chuỗi của tier do Model Router chọn): nó không phải là
	// quyền gọi model tuỳ ý bằng khoá của org.
	pinned, err := g.llm.Models.AgentModel(ctx, task.AgentVersionID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	allowed := task.ModelChain
	if allowed == nil && pinned.ModelID != uuid.Nil {
		allowed = []uuid.UUID{pinned.ModelID}
	}
	if !slices.Contains(allowed, call.ModelID) {
		httpx.WriteError(w, r, apperr.New(apperr.KindPermissionDenied, "permission_denied", "model không nằm trong các model của task này"))
		return
	}
	// Số lượt đã hoàn tất của task không vượt max_iterations: token của một task không
	// phải là quyền gọi model không giới hạn bằng khoá của org.
	if pinned.MaxIterations > 0 && task.Iterations >= pinned.MaxIterations {
		httpx.WriteError(w, r, apperr.New(apperr.KindPermissionDenied, "limit_exceeded", "task đã dùng hết max_iterations"))
		return
	}
	maxOut := pinned.MaxOutputTokens
	if maxOut <= 0 {
		maxOut = defaultMaxOutputTokens
	}
	if call.Request.MaxOutputTokens <= 0 || call.Request.MaxOutputTokens > maxOut {
		call.Request.MaxOutputTokens = maxOut
	}
	route, err := g.llm.Models.Route(ctx, call.ModelID)
	if err != nil {
		if e, ok := apperr.From(err); ok && e.Kind == apperr.KindUnprocessable {
			err = errModelUnavailable // model hoặc provider bị tắt sau khi task được tạo
		}
		httpx.WriteError(w, r, err)
		return
	}
	key, err := g.modelKey(ctx, route)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	llm, err := g.llm.Factory(route.Adapter, key)
	if err != nil {
		slog.ErrorContext(ctx, "gateway: không dựng được adapter LLM", "adapter", route.Adapter)
		httpx.WriteError(w, r, errCredential)
		return
	}
	// Định danh model phía provider lấy từ catalog, không lấy từ request.
	call.Request.Model = route.ProviderModel
	// Đặt trước trần chi phí của lượt này ở mọi budget áp dụng (task, agent, user,
	// project, organization) trước khi tiêu tiền; quyết toán theo usage thật sau đó.
	settle := func(context.Context, provider.Usage) {}
	if g.llm.Budget != nil {
		bc := BudgetCall{OrgID: p.OrgID, ProjectID: task.ProjectID, AgentID: task.AgentID, ModelID: call.ModelID,
			MaxOutputTokens: call.Request.MaxOutputTokens, TaskLimit: task.BudgetLimit, TaskSpent: task.EstimatedCost}
		if task.InitiatorType == identity.PrincipalUser {
			bc.UserID = &task.InitiatorID
		}
		raw, merr := json.Marshal(call.Request)
		if merr != nil {
			httpx.WriteError(w, r, apperr.Wrap(merr, apperr.KindInternal, "internal_error", ""))
			return // không ước lượng được chi phí thì không gọi
		}
		bc.InputBytes, bc.TaskID = len(raw), taskID
		if settle, err = g.llm.Budget.Reserve(ctx, bc); err != nil {
			code := "budget_unavailable"
			if e, ok := apperr.From(err); ok && e.Code == "budget_exceeded" {
				code = e.Code
			} else {
				slog.ErrorContext(ctx, "gateway: không kiểm được budget", "error", err, "task_id", r.PathValue("task"))
			}
			// Quyết định từ chối cũng được ghi audit, như mọi lượt gọi lỗi khác.
			if aerr := g.auditModel(ctx, p, task, taskID, call.ModelID, route, provider.Response{}, code); aerr != nil {
				slog.ErrorContext(ctx, "gateway: không ghi được audit cho lượt bị từ chối vì budget", "error", aerr)
			}
			httpx.WriteError(w, r, err)
			return
		}
	}
	resp, err := llm.Complete(ctx, call.Request)
	// Lượt lỗi: adapter trả usage dở dang nếu provider đã bắt đầu xử lý (luồng bị ngắt
	// giữa chừng vẫn bị tính tiền); chưa xử lý gì thì usage rỗng và phần đặt trước được hoàn.
	settle(ctx, resp.Usage)
	if err != nil {
		// Chi tiết của provider vào log; runtime chỉ cần biết có thử lại được không.
		slog.WarnContext(ctx, "gateway: gọi model lỗi", "error", err, "task_id", r.PathValue("task"), "adapter", route.Adapter)
		out := apperr.New(apperr.KindUnprocessable, CodeProviderRejected, "provider từ chối yêu cầu")
		if e, ok := apperr.From(err); apperr.IsRetryable(err) {
			out = apperr.New(apperr.KindUnavailable, CodeProviderUnavailable, "provider tạm thời không phục vụ").AsRetryable()
		} else if ok && e.Code == "provider_auth" {
			out = errModelUnavailable // provider từ chối khoá: riêng của provider này
		}
		// Lượt gọi lỗi cũng để lại dấu vết ở nơi giữ khoá: nó có thể đã bị tính tiền, và
		// số lần thử của một task phải đếm được. Không ghi được audit thì chỉ log: lỗi
		// đang được trả về rồi.
		if aerr := g.auditModel(ctx, p, task, taskID, call.ModelID, route, resp, out.Code); aerr != nil {
			slog.ErrorContext(ctx, "gateway: không ghi được audit cho lượt gọi model lỗi", "error", aerr)
		}
		httpx.WriteError(w, r, out)
		return
	}
	// Mỗi lượt gọi bằng khoá của org / platform để lại dấu vết ở chính nơi giữ khoá,
	// độc lập với usage do runtime ghi. Không ghi nội dung.
	if err := g.auditModel(ctx, p, task, taskID, call.ModelID, route, resp, ""); err != nil {
		slog.ErrorContext(ctx, "gateway: không ghi được audit cho lượt gọi model", "error", err)
		httpx.WriteError(w, r, apperr.Wrap(err, apperr.KindInternal, "internal_error", ""))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

// auditModel ghi một lượt gọi model. failure khác rỗng là mã lỗi của lượt thất bại.
func (g *Gateway) auditModel(ctx context.Context, p identity.Principal, task ActiveTask, taskID, modelID uuid.UUID, route ModelRoute, resp provider.Response, failure string) error {
	ctx, cancel := context.WithTimeout(tenancy.WithOrg(context.WithoutCancel(ctx), p.OrgID), auditTimeout)
	defer cancel()
	credential := "organization"
	if route.Platform {
		credential = "platform"
	}
	outcome := audit.OutcomeSuccess
	if failure != "" {
		outcome = audit.OutcomeError
	}
	return tenancy.InTx(ctx, g.db, func(tx pgxv5.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = '3s'"); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{
			OrgID: p.OrgID, ProjectID: &task.ProjectID, Action: "llm.call", ResourceType: "model", ResourceID: &modelID,
			ExecutionID: &task.ExecutionID,
			Outcome:     outcome,
			Details: map[string]string{
				"error_code":   failure,
				"execution_id": task.ExecutionID.String(), "task_id": taskID.String(), "adapter": route.Adapter, "credential": credential,
				"input_tokens": strconv.FormatInt(resp.Usage.InputTokens, 10), "output_tokens": strconv.FormatInt(resp.Usage.OutputTokens, 10),
				"stop_reason": string(resp.StopReason),
			}})
	})
}

// modelKey trả về khoá dùng cho một lượt gọi: khoá của org cho provider của org,
// khoá của platform cho provider của platform. Hai loại không thay thế nhau: org
// không bao giờ tiêu bằng khoá của platform chỉ vì thiếu khoá của mình.
func (g *Gateway) modelKey(ctx context.Context, route ModelRoute) (string, error) {
	if route.Platform {
		if key := g.llm.PlatformKeys[route.Adapter]; key != "" {
			return key, nil
		}
		return "", errNoCredential
	}
	if route.SecretID == nil || g.llm.Secrets == nil {
		return "", errNoCredential
	}
	key, err := g.llm.Secrets.Get(ctx, *route.SecretID, "llm_call")
	if err != nil {
		slog.ErrorContext(ctx, "gateway: không đọc được khoá của provider", "error", err, "secret_id", *route.SecretID)
		return "", errCredential
	}
	return string(key), nil
}
