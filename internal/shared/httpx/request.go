package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	apperr "github.com/hqgroups/mcp/internal/shared/errors"
)

// DecodeJSON đọc thân request JSON vào v. Trường lạ được bỏ qua (theo quy ước API).
func DecodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return apperr.New(apperr.KindInvalid, "body_too_large", "thân request vượt giới hạn kích thước")
		}
		return apperr.New(apperr.KindInvalid, "invalid_json", "thân request không phải JSON hợp lệ")
	}
	if dec.More() {
		return apperr.New(apperr.KindInvalid, "invalid_json", "thân request chứa nhiều hơn một giá trị JSON")
	}
	return nil
}

// WriteJSON ghi phản hồi JSON.
func WriteJSON(w http.ResponseWriter, status int, v any) { writeJSON(w, status, v) }

// ETag tạo ETag mạnh từ thời điểm sửa cuối (độ chính xác micro giây của PostgreSQL).
func ETag(updatedAt time.Time) string {
	return `"` + strconv.FormatInt(updatedAt.UnixMicro(), 36) + `"`
}

// IfMatch đọc header If-Match. Trả nil nếu không có hoặc là "*": cập nhật vô điều kiện.
// ETag không đọc được thì không thể khớp, nên trả lỗi 412.
func IfMatch(r *http.Request) (*time.Time, error) {
	h := r.Header.Get("If-Match")
	if h == "" || h == "*" {
		return nil, nil
	}
	if len(h) < 3 || h[0] != '"' || h[len(h)-1] != '"' {
		return nil, ErrPreconditionFailed
	}
	us, err := strconv.ParseInt(h[1:len(h)-1], 36, 64)
	if err != nil {
		return nil, ErrPreconditionFailed
	}
	t := time.UnixMicro(us)
	return &t, nil
}

// ErrPreconditionFailed là lỗi 412 khi ETag lệch.
var ErrPreconditionFailed = apperr.New(apperr.KindPreconditionFailed, "precondition_failed",
	"tài nguyên đã thay đổi; tải lại và thử lại với ETag mới")

// LogError ghi một lỗi nội bộ không trả được cho client.
func LogError(ctx context.Context, msg string, err error) {
	slog.ErrorContext(ctx, msg, "error", err)
}
