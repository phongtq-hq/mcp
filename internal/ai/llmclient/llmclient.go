// Package llmclient là client của Agent Runtime tới đường gọi model của Gateway.
// Runtime không giữ khoá của LLM provider (ADR-0008): nó gửi một lượt gọi model tới
// Gateway bằng execution token của task, và Gateway gọi provider bằng khoá của org
// hoặc của platform.
package llmclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"

	"github.com/hqgroups/mcp/internal/ai/provider"
	apperr "github.com/hqgroups/mcp/internal/shared/errors"
)

const maxResponseBytes = 16 << 20

// Client gọi endpoint gọi model của Gateway.
type Client struct {
	base string
	hc   *http.Client
}

// New nhận URL gốc của Gateway (cấu hình của người vận hành).
func New(baseURL string) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return nil, errors.New("llmclient: URL của gateway phải là http(s)")
	}
	return &Client{base: strings.TrimRight(baseURL, "/"), hc: &http.Client{
		// Thời hạn do context của task quyết định. Token không đi theo redirect.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

// Complete gửi một lượt gọi model. Lỗi tạm thời của provider hoặc của Gateway được
// đánh dấu thử lại được.
func (c *Client) Complete(ctx context.Context, token string, taskID, modelID uuid.UUID, req provider.Request) (provider.Response, error) {
	body, err := json.Marshal(struct {
		ModelID uuid.UUID        `json:"model_id"`
		Request provider.Request `json:"request"`
	}{modelID, req})
	if err != nil {
		return provider.Response{}, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/llm/internal/v1/tasks/"+taskID.String()+"/complete", bytes.NewReader(body))
	if err != nil {
		return provider.Response{}, err
	}
	hreq.Header.Set("Authorization", "Bearer "+token)
	hreq.Header.Set("Content-Type", "application/json")
	res, err := c.hc.Do(hreq)
	if err != nil {
		if ctx.Err() != nil {
			return provider.Response{}, ctx.Err()
		}
		return provider.Response{}, apperr.Wrap(errors.New("llmclient: không kết nối được gateway"), apperr.KindUnavailable, "gateway_unavailable", "không gọi được Gateway").AsRetryable()
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes+1))
	if err != nil || len(raw) > maxResponseBytes {
		return provider.Response{}, apperr.Wrap(errors.New("llmclient: phản hồi không đọc được hoặc quá lớn"), apperr.KindUnavailable, "gateway_unavailable", "phản hồi của Gateway không dùng được").AsRetryable()
	}
	if res.StatusCode == http.StatusOK {
		var out provider.Response
		if err := json.Unmarshal(raw, &out); err != nil {
			return provider.Response{}, errors.New("llmclient: phản hồi của gateway sai định dạng")
		}
		return out, nil
	}
	var problem struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(raw, &problem)
	// Mã lỗi của Gateway được giữ lại: runtime dựa vào nó để biết lỗi là của model này
	// (chuyển sang model dự phòng) hay của yêu cầu / của chính Gateway (không chuyển).
	code := problem.Code
	if code == "" || len(code) > 64 {
		code = "model_call_failed"
	}
	e := apperr.Wrap(fmt.Errorf("llmclient: gateway trả HTTP %d (%s)", res.StatusCode, problem.Code), apperr.KindUnprocessable, code, "không gọi được model")
	if res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500 {
		return provider.Response{}, e.AsRetryable()
	}
	return provider.Response{}, e
}
