package loki

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// maxRangeWindow là cửa sổ range vector ([5m]) lớn nhất được nhận trong một biểu thức.
const maxRangeWindow = 24 * time.Hour

// Bộ kiểm TỪ VỰNG cho LogQL. Nó tồn tại vì trần khoảng thời gian start / end không đủ:
// một biểu thức tự kéo dữ liệu ngoài khoảng đó vào kết quả bằng cửa sổ range vector lớn
// ([30d]), modifier offset, modifier @, hay subquery. Đây KHÔNG phải parser của LogQL:
// nó chỉ tách chuỗi và chú thích khỏi phần còn lại rồi từ chối những gì không chắc là
// an toàn (fail-closed), kể cả một số biểu thức hợp lệ (ví dụ label tên offset). Giới hạn
// phía Loki (max_query_lookback, max_query_length) vẫn là lớp thứ hai bắt buộc.

var (
	// Chỉ nhận dạng <số><đơn vị> lặp lại, ví dụ 5m, 1h30m. Loki nhận nhiều dạng hơn (số
	// thập phân, y, ns…); chúng bị từ chối ở đây.
	windowRe     = regexp.MustCompile(`^([0-9]{1,9}(ms|s|m|h|d|w))+$`)
	windowPartRe = regexp.MustCompile(`([0-9]{1,9})(ms|s|m|h|d|w)`)
	windowUnits  = map[string]time.Duration{"ms": time.Millisecond, "s": time.Second, "m": time.Minute,
		"h": time.Hour, "d": 24 * time.Hour, "w": 7 * 24 * time.Hour}
)

// parseWindow đọc nội dung của một [ … ]. Trả false nếu không phải một duration dương
// ở dạng được nhận. Kết quả được chặn ở trên maxRangeWindow một chút để không tràn số.
func parseWindow(s string) (time.Duration, bool) {
	if len(s) > 64 || !windowRe.MatchString(s) {
		return 0, false
	}
	var total time.Duration
	for _, m := range windowPartRe.FindAllStringSubmatch(s, -1) {
		n, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			return 0, false
		}
		unit := windowUnits[m[2]]
		if n > int64(2*maxRangeWindow/unit) {
			return 2 * maxRangeWindow, true // chắc chắn quá trần; không nhân để khỏi tràn
		}
		if total += time.Duration(n) * unit; total > 2*maxRangeWindow {
			return 2 * maxRangeWindow, true
		}
	}
	return total, total > 0
}

func identStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func identChar(c byte) bool { return identStart(c) || (c >= '0' && c <= '9') }

// scanLogQL quét biểu thức, bỏ qua nội dung chuỗi ("…" có escape bằng \, và chuỗi thô
// trong dấu backtick) và chú thích (# tới hết dòng). Trả về số [ … ] ngoài chuỗi và cửa
// sổ lớn nhất trong số đó.
func scanLogQL(q string) (ranges int, maxWindow time.Duration, err error) {
	for i := 0; i < len(q); {
		c := q[i]
		switch {
		case c == '"':
			j := i + 1
			for ; j < len(q) && q[j] != '"' && q[j] != '\n'; j++ {
				if q[j] == '\\' {
					j++
				}
			}
			if j >= len(q) || q[j] != '"' {
				return 0, 0, errors.New(`query có chuỗi "…" không đóng (hoặc xuống dòng trong chuỗi)`)
			}
			i = j + 1
		case c == '`':
			end := strings.IndexByte(q[i+1:], '`')
			if end < 0 {
				return 0, 0, errors.New("query có chuỗi thô (backtick) không đóng")
			}
			i += end + 2
		case c == '#':
			end := strings.IndexByte(q[i:], '\n')
			if end < 0 {
				return ranges, maxWindow, nil
			}
			i += end + 1
		case c == '@':
			return 0, 0, errors.New("query không được dùng modifier @ (ký tự @ ngoài chuỗi bị từ chối)")
		case c == '$':
			return 0, 0, errors.New("query không được dùng biến ($__interval, $__range…); ghi duration tường minh")
		case c == '[':
			end := strings.IndexByte(q[i+1:], ']')
			if end < 0 {
				return 0, 0, errors.New("query có [ không đóng")
			}
			body := strings.TrimSpace(q[i+1 : i+1+end])
			if strings.Contains(body, ":") {
				return 0, 0, errors.New("query không được dùng subquery ([range:step])")
			}
			window, ok := parseWindow(body)
			if !ok {
				return 0, 0, errors.New("mỗi [ … ] ngoài chuỗi phải là một duration dạng <số><đơn vị> với đơn vị ms, s, m, h, d, w (ví dụ [5m], [1h30m])")
			}
			if window > maxRangeWindow {
				return 0, 0, fmt.Errorf("cửa sổ [%s] vượt quá %d giờ", cut(body, 32), int(maxRangeWindow.Hours()))
			}
			ranges++
			maxWindow = max(maxWindow, window)
			i += end + 2
		case c == ']':
			return 0, 0, errors.New("query có ] không khớp với [ nào")
		case identStart(c):
			j := i + 1
			for j < len(q) && identChar(q[j]) {
				j++
			}
			if strings.EqualFold(q[i:j], "offset") {
				return 0, 0, errors.New("query không được dùng modifier offset (từ offset ngoài chuỗi bị từ chối, kể cả khi nó là tên label; đặt tên label trong chuỗi không giúp được, hãy bỏ nó khỏi biểu thức)")
			}
			i = j
		case c >= '0' && c <= '9':
			// Số đứng trước một từ (5offset) không che được từ đó: chỉ ăn chữ số.
			for i < len(q) && ((q[i] >= '0' && q[i] <= '9') || q[i] == '.') {
				i++
			}
		default:
			i++
		}
	}
	return ranges, maxWindow, nil
}

// checkLogExpr kiểm biểu thức của query_logs: log query không có range vector.
func checkLogExpr(q string) error {
	ranges, _, err := scanLogQL(q)
	if err != nil {
		return err
	}
	if ranges > 0 {
		return errors.New("query_logs chỉ nhận LogQL log query (không có [ … ] ngoài chuỗi); metric query dùng tool query_metric")
	}
	return nil
}

// checkMetricExpr kiểm biểu thức của query_metric: khoảng start–end cộng cửa sổ lớn nhất
// là khoảng dữ liệu biểu thức thật sự chạm tới, và nó không được quá maxRange.
func checkMetricExpr(q string, span time.Duration) error {
	_, window, err := scanLogQL(q)
	if err != nil {
		return err
	}
	if span+window > maxRange {
		return fmt.Errorf("khoảng start–end (%s) cộng cửa sổ [ … ] lớn nhất (%s) không được quá %d giờ",
			span.Round(time.Second), window, int(maxRange.Hours()))
	}
	return nil
}
