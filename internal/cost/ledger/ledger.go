// Package ledger là usage ledger (cost-management §2): mỗi lời gọi LLM một dòng
// usage_records, chỉ thêm, kèm chi phí ước tính theo bảng giá hiệu lực lúc ghi.
//
// Ledger ghi trong transaction của người gọi, để usage và execution step của cùng
// một lời gọi được ghi cùng nhau hoặc không ghi gì ("ghi trước, tối ưu sau").
package ledger

import (
	"context"
	"errors"

	"github.com/google/uuid"
	pgxv5 "github.com/jackc/pgx/v5"

	"github.com/hqgroups/mcp/internal/cost/ledger/store"
	"github.com/hqgroups/mcp/internal/shared/ids"
)

// LLMUsage là lượng tiêu thụ của một lời gọi LLM, do provider báo.
type LLMUsage struct {
	OrgID          uuid.UUID
	ProjectID      uuid.UUID
	UserID         *uuid.UUID
	AgentID        uuid.UUID
	AgentVersionID uuid.UUID
	ExecutionID    uuid.UUID
	TaskID         uuid.UUID
	StepSeq        int
	Purpose        string
	ModelID        uuid.UUID

	InputTokens       int64
	OutputTokens      int64
	CachedInputTokens int64
	CacheWriteTokens  int64
	ReasoningTokens   int64
}

// Cost là chi phí ước tính của một dòng usage. Amount rỗng nghĩa là model chưa có
// giá hiệu lực. Tiền là chuỗi thập phân (NUMERIC(18,8)), không float.
type Cost struct {
	Amount   string
	Currency string
}

// RecordLLM ghi một dòng usage trong tx và trả về chi phí ước tính.
func RecordLLM(ctx context.Context, tx pgxv5.Tx, u LLMUsage) (Cost, error) {
	if u.InputTokens < 0 || u.OutputTokens < 0 || u.CachedInputTokens < 0 || u.CacheWriteTokens < 0 || u.ReasoningTokens < 0 {
		return Cost{}, errors.New("ledger: số token âm")
	}
	row, err := store.New(tx).InsertLLMUsage(ctx, store.InsertLLMUsageParams{
		ID: ids.New(), OrgID: u.OrgID, ProjectID: u.ProjectID, UserID: u.UserID, AgentID: &u.AgentID,
		AgentVersionID: &u.AgentVersionID, ExecutionID: &u.ExecutionID, TaskID: &u.TaskID,
		StepSeq: ptr(int32(u.StepSeq)), Purpose: &u.Purpose, ModelID: u.ModelID, //nolint:gosec // step_seq nhỏ
		InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CachedInputTokens: u.CachedInputTokens,
		CacheWriteTokens: u.CacheWriteTokens, ReasoningTokens: u.ReasoningTokens,
	})
	if errors.Is(err, pgxv5.ErrNoRows) {
		return Cost{}, errors.New("ledger: model không tồn tại trong tầm nhìn của tenant")
	}
	if err != nil {
		return Cost{}, err
	}
	return Cost{Amount: row.EstimatedCost, Currency: row.Currency}, nil
}

func ptr[T any](v T) *T { return &v }
