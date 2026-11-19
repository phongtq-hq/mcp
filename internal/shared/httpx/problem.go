// Package httpx chứa phần HTTP dùng chung: lỗi RFC 9457, health, server có graceful shutdown.
package httpx

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"go.opentelemetry.io/otel/trace"

	apperr "github.com/hqgroups/mcp/internal/shared/errors"
)

// Problem là thân lỗi application/problem+json, khớp schema Problem trong openapi.yaml.
type Problem struct {
	Type    string `json:"type"`
	Title   string `json:"title"`
	Status  int    `json:"status"`
	Detail  string `json:"detail,omitempty"`
	Code    string `json:"code"`
	TraceID string `json:"trace_id,omitempty"`
	// Errors là chi tiết theo trường của lỗi validate (400 / 422); không có với lỗi khác.
	Errors []ProblemField `json:"errors,omitempty"`
}

// ProblemField là một mục của `errors[]`.
type ProblemField struct {
	Field   string `json:"field"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

// maxProblemFields là trần số mục `errors[]` của một phản hồi.
const maxProblemFields = 50

var statusByKind = map[apperr.Kind]int{
	apperr.KindInternal:           http.StatusInternalServerError,
	apperr.KindInvalid:            http.StatusBadRequest,
	apperr.KindUnprocessable:      http.StatusUnprocessableEntity,
	apperr.KindUnauthenticated:    http.StatusUnauthorized,
	apperr.KindPermissionDenied:   http.StatusForbidden,
	apperr.KindNotFound:           http.StatusNotFound,
	apperr.KindConflict:           http.StatusConflict,
	apperr.KindPreconditionFailed: http.StatusPreconditionFailed,
	apperr.KindBudgetExceeded:     http.StatusPaymentRequired,
	apperr.KindRateLimited:        http.StatusTooManyRequests,
	apperr.KindUnavailable:        http.StatusServiceUnavailable,
}

// WriteError ghi lỗi dạng problem+json. Lỗi không phân loại và lỗi nội bộ
// không lộ chi tiết ra ngoài; chi tiết chỉ đi vào log.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	p := Problem{
		Type:   "about:blank",
		Status: http.StatusInternalServerError,
		Code:   "internal_error",
	}
	if e, ok := apperr.From(err); ok && e.Kind != apperr.KindInternal {
		p.Status = statusByKind[e.Kind]
		p.Code = e.Code
		p.Detail = e.Message
		// Chi tiết theo trường chỉ đi kèm lỗi của chính request (sai cú pháp / ngữ nghĩa).
		if p.Status == http.StatusBadRequest || p.Status == http.StatusUnprocessableEntity {
			for i, f := range e.Fields {
				if i == maxProblemFields {
					break
				}
				p.Errors = append(p.Errors, ProblemField(f))
			}
		}
	}
	p.Title = http.StatusText(p.Status)
	if sc := trace.SpanContextFromContext(r.Context()); sc.HasTraceID() {
		p.TraceID = sc.TraceID().String()
	}
	if p.Status >= http.StatusInternalServerError {
		slog.ErrorContext(r.Context(), "request failed",
			"error", err, "status", p.Status, "trace_id", p.TraceID)
	}

	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p)
}
