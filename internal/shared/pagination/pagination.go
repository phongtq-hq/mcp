// Package pagination chứa phần dùng chung của phân trang keyset: cursor mờ và limit.
package pagination

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/google/uuid"

	apperr "github.com/hqgroups/mcp/internal/shared/errors"
)

const (
	DefaultLimit = 50
	MaxLimit     = 200
	maxValueLen  = 1024
)

// ErrInvalidCursor là lỗi 400 cho cursor hỏng hoặc không khớp cách sắp xếp.
var ErrInvalidCursor = apperr.New(apperr.KindInvalid, "invalid_cursor", "cursor không hợp lệ hoặc không khớp với cách sắp xếp")

var errLimit = apperr.New(apperr.KindInvalid, "invalid_parameter", "limit phải từ 1 đến 200")

// Cursor là vị trí của dòng cuối trang trước. Cursor không được ký: nó chỉ là
// giá trị so sánh, không mang quyền; tenant luôn lấy từ context.
type Cursor struct {
	Sort  string    `json:"s"`
	Value string    `json:"v"`
	ID    uuid.UUID `json:"id"`
}

// Encode mã hoá cursor thành chuỗi mờ.
func Encode(c Cursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

// Decode đọc cursor; chuỗi rỗng trả nil. Cursor phải thuộc đúng cách sắp xếp sort.
func Decode(s, sort string) (*Cursor, error) {
	if s == "" {
		return nil, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, ErrInvalidCursor
	}
	var c Cursor
	if err := json.Unmarshal(b, &c); err != nil || c.Sort != sort || c.ID == uuid.Nil ||
		strings.ContainsRune(c.Value, 0) || len(c.Value) > maxValueLen {
		return nil, ErrInvalidCursor
	}
	return &c, nil
}

// Limit đọc tham số limit; chuỗi rỗng trả DefaultLimit.
func Limit(v string) (int, error) {
	if v == "" {
		return DefaultLimit, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > MaxLimit {
		return 0, errLimit
	}
	return n, nil
}
