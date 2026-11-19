// Package errors định nghĩa lỗi có phân loại dùng chung giữa các bounded context.
//
// Phân loại (Kind) quyết định mã HTTP ở biên API và việc có được retry hay không.
// Code là mã ổn định cho máy đọc, đi ra ngoài qua trường `code` của problem+json.
package errors

import (
	stderrors "errors"
	"fmt"
)

// Kind là loại lỗi ở mức ngữ nghĩa, độc lập với giao thức.
type Kind int

const (
	KindInternal      Kind = iota
	KindInvalid            // sai cú pháp
	KindUnprocessable      // đúng cú pháp, sai ngữ nghĩa
	KindUnauthenticated
	KindPermissionDenied
	KindNotFound // kể cả tài nguyên của tenant khác
	KindConflict
	KindPreconditionFailed
	KindBudgetExceeded
	KindRateLimited
	KindUnavailable
)

// Error là lỗi có phân loại.
type Error struct {
	Kind      Kind
	Code      string // mã ổn định, ví dụ "validation_failed"
	Message   string // an toàn để trả cho client
	Retryable bool
	// Fields là chi tiết theo trường của một lỗi validate (đi ra ngoài qua `errors[]` của
	// problem+json). Rỗng với mọi lỗi khác.
	Fields []FieldError
	cause  error
}

// FieldError là một lỗi validate gắn với một trường của thân request. Field là tên trường
// như client gửi (đường dẫn dùng dấu chấm cho trường lồng nhau, ví dụ "settings.orchestration").
type FieldError struct {
	Field   string
	Code    string // tuỳ chọn; mã ổn định cho máy đọc
	Message string // an toàn để trả cho client
}

func (e *Error) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.cause }

// New tạo lỗi có phân loại.
func New(kind Kind, code, message string) *Error {
	return &Error{Kind: kind, Code: code, Message: message}
}

// Wrap bọc một lỗi gốc. Lỗi gốc không bao giờ được trả cho client.
func Wrap(cause error, kind Kind, code, message string) *Error {
	return &Error{Kind: kind, Code: code, Message: message, cause: cause}
}

// InvalidField tạo lỗi validate (422, `validation_failed`) gắn với một trường. Message là
// thông báo của cả lỗi lẫn của trường: client không đọc `errors[]` vẫn thấy đủ.
func InvalidField(field, message string) *Error {
	return &Error{Kind: KindUnprocessable, Code: "validation_failed", Message: message,
		Fields: []FieldError{{Field: field, Message: message}}}
}

// AsRetryable đánh dấu lỗi là có thể thử lại.
func (e *Error) AsRetryable() *Error {
	e.Retryable = true
	return e
}

// From trích *Error từ chuỗi lỗi; ok = false nếu không có.
func From(err error) (*Error, bool) {
	var e *Error
	if stderrors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// KindOf trả về loại của lỗi; lỗi không phân loại là KindInternal.
func KindOf(err error) Kind {
	if e, ok := From(err); ok {
		return e.Kind
	}
	return KindInternal
}

// IsRetryable cho biết lỗi có được đánh dấu thử lại được không.
// Lỗi không phân loại mặc định là không: retry phải là quyết định tường minh.
func IsRetryable(err error) bool {
	e, ok := From(err)
	return ok && e.Retryable
}
