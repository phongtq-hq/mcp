// Package runtimewire dựng các bước của Agent Runtime (runtime.Steps) từ đường tới
// MCP Gateway. Hai composition root dùng chung nó: internal/workerserver (vòng lặp chạy
// trên Temporal) và internal/apiserver (vòng lặp chạy trong process khi phát triển cục
// bộ và test).
//
// Các bước không giữ credential nào: tool call và lượt model có khoá ở Gateway đều đi
// qua Gateway bằng execution token.
package runtimewire

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hqgroups/mcp/internal/agent/runtime"
	"github.com/hqgroups/mcp/internal/ai/llmclient"
	"github.com/hqgroups/mcp/internal/ai/modelcatalog"
	"github.com/hqgroups/mcp/internal/ai/provider"
	"github.com/hqgroups/mcp/internal/cost/budget"
	"github.com/hqgroups/mcp/internal/cost/pricing"
	mcpclient "github.com/hqgroups/mcp/internal/mcp/client"
	"github.com/hqgroups/mcp/internal/platform/identity"
	"github.com/hqgroups/mcp/internal/shared/exectoken"
	"github.com/hqgroups/mcp/internal/workflow/approval"
)

// toolGateway nối MCP client của Gateway vào interface của runtime.
type toolGateway struct{ c *mcpclient.Gateway }

func (g toolGateway) ListTools(ctx context.Context, token string, taskID uuid.UUID) ([]provider.Tool, error) {
	tools, err := g.c.ListTools(ctx, token, taskID)
	if err != nil {
		return nil, err
	}
	out := make([]provider.Tool, len(tools))
	for i, t := range tools {
		out[i] = provider.Tool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema}
	}
	return out, nil
}

func (g toolGateway) CallTool(ctx context.Context, token string, taskID uuid.UUID, name string, args json.RawMessage, stepSeq int) (runtime.ToolResult, error) {
	res, err := g.c.CallTool(ctx, token, taskID, name, args, stepSeq)
	if errors.Is(err, mcpclient.ErrNotSent) {
		err = fmt.Errorf("%w: %w", runtime.ErrToolNotSent, err)
	}
	return runtime.ToolResult{Text: res.Text, IsError: res.IsError, Code: res.Code, ApprovalID: res.ApprovalID}, err
}

// ReadOnly hỏi Gateway tool có phải chỉ đọc không. Không hỏi được thì trả lỗi: bên gọi
// coi như không biết, tức không phải chỉ đọc.
func (g toolGateway) ReadOnly(ctx context.Context, token string, taskID uuid.UUID, name string) (bool, error) {
	tools, err := g.c.ListTools(ctx, token, taskID)
	if err != nil {
		return false, err
	}
	for _, t := range tools {
		if t.Name == name {
			return t.ReadOnly, nil
		}
	}
	return false, errors.New("runtimewire: tool không còn trong danh sách của task")
}

// BudgetGate nối Budget Management vào interface của runtime, cho lời gọi tới adapter
// chạy ngay trong process (claude-cli trên máy lập trình viên). Lời gọi qua Gateway
// được kiểm budget ở Gateway.
type BudgetGate struct{ B *budget.Service }

// Reserve hiện thực runtime.BudgetGate.
func (g BudgetGate) Reserve(ctx context.Context, c runtime.BudgetCall) (func(context.Context, provider.Usage), error) {
	call := budget.Call{Scope: budget.Scope{OrgID: c.Task.OrgID, ProjectID: c.Task.ProjectID, AgentID: c.Task.AgentID},
		ModelID: c.ModelID, InputBytes: c.InputBytes, MaxOutputTokens: c.MaxOutputTokens}
	if c.Task.InitiatorType == identity.PrincipalUser {
		call.Scope.UserID = &c.Task.InitiatorID
	}
	call.Key = c.Task.TaskID.String() + ":" + c.ModelID.String()
	if c.TaskLimit != "" {
		call.Task = &budget.TaskBudget{ID: c.Task.TaskID, Limit: c.TaskLimit, Spent: c.TaskSpent}
	}
	res, err := g.B.Reserve(ctx, call)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, u provider.Usage) {
		g.B.Settle(ctx, res, budget.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
			CachedInputTokens: u.CachedInputTokens, CacheWriteTokens: u.CacheWriteTokens, ReasoningTokens: u.ReasoningTokens})
	}, nil
}

// LocalBudget dựng cổng budget cho adapter trong process. Có counter thì kiểm đủ budget
// theo phạm vi và budget của task (process api ở chế độ chạy vòng lặp trong process).
// counter nil thì chỉ kiểm budget của task, bằng dữ liệu thực thi: process worker không
// nối bộ đếm dùng chung và không có quyền đọc bảng budgets.
func LocalBudget(db *pgxpool.Pool, counter budget.Counter) BudgetGate {
	prices := pricing.NewService(db, modelcatalog.NewService(db, nil))
	return BudgetGate{B: budget.NewService(db, nil, prices, counter)}
}

// New dựng các bước. local là adapter chạy ngay trong process (claude-cli trên máy
// lập trình viên); mọi adapter khác được gọi qua Gateway.
// Khi có adapter trong process, lời gọi tới nó được kiểm budget: đủ mọi budget nếu có
// counter, chỉ budget của task nếu counter là nil.
func New(db *pgxpool.Pool, gatewayURL string, tokenKey []byte, local map[string]provider.Provider, counter budget.Counter) (*runtime.Steps, error) {
	if gatewayURL == "" || len(tokenKey) == 0 {
		return nil, errors.New("runtimewire: cần URL nội bộ của Gateway và khoá execution token")
	}
	llm, err := llmclient.New(gatewayURL)
	if err != nil {
		return nil, err
	}
	gw, err := mcpclient.NewGateway(gatewayURL)
	if err != nil {
		return nil, err
	}
	tokens, err := exectoken.New(tokenKey)
	if err != nil {
		return nil, err
	}
	steps := &runtime.Steps{DB: db, Providers: local, Models: llm, Gateway: toolGateway{gw}, Tokens: tokens,
		Approvals: approval.NewService(db)}
	if len(local) > 0 {
		steps.Budget = LocalBudget(db, counter)
	}
	return steps, nil
}
