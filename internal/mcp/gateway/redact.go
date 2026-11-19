package gateway

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
)

// Bộ lọc che mẫu secret đã biết trong kết quả tool, trước khi kết quả vào context
// của model hay tới client (security §5 mục 6).
//
// Đây là lớp giảm thiểu, không phải bảo đảm: nó chỉ bắt các dạng có hình thù rõ
// ràng. Thứ ngăn secret lộ ra trước hết là managed server không trả chúng (ví dụ
// server Docker không trả biến môi trường của container).
type pattern struct {
	kind string
	re   *regexp.Regexp
	// repl dùng nhóm bắt được; rỗng thì thay cả đoạn khớp.
	repl string
}

var patterns = []pattern{
	{"private_key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?(-----END [A-Z ]*PRIVATE KEY-----|\z)`), ""},
	{"anthropic_key", regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{16,}`), ""},
	{"api_key", regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{24,}`), ""},
	{"github_token", regexp.MustCompile(`\b(ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{30,}|\bgithub_pat_[A-Za-z0-9_]{40,}`), ""},
	{"aws_access_key", regexp.MustCompile(`\b(AKIA|ASIA)[0-9A-Z]{16}\b`), ""},
	{"slack_token", regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`), ""},
	{"jwt", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), ""},
	// scheme://user:password@host → giữ user và host, che mật khẩu.
	{"url_password", regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://[^\s:/@]+:)[^\s@/]+(@)`), "${1}[REDACTED:url_password]${2}"},
	// Authorization: Bearer ... / Basic ... (đứng trước mẫu gán để token không bị bỏ sót).
	{"bearer", regexp.MustCompile(`(?i)\b(bearer\s+)[A-Za-z0-9._~+/-]{16,}=*`), "${1}[REDACTED:bearer]"},
	{"basic_auth", regexp.MustCompile(`(?i)\b(basic\s+)[A-Za-z0-9+/]{16,}=*`), "${1}[REDACTED:basic_auth]"},
	// password=..., POSTGRES_PASSWORD="...", api_key: ... Tên biến có thể mang tiền tố,
	// nên không dùng \b ở đầu.
	{"assigned_secret", regexp.MustCompile(`(?i)((?:password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key)[A-Za-z0-9_-]*["']?\s*[=:]\s*)("[^"\n]{4,}"|'[^'\n]{4,}'|[^\s"',;\[]{4,})`), "${1}[REDACTED:assigned_secret]"},
}

// redact che các mẫu secret trong s và trả về số chỗ đã che.
func redact(s string) (string, int) {
	n := 0
	for _, p := range patterns {
		s = p.re.ReplaceAllStringFunc(s, func(m string) string {
			n++
			if p.repl == "" {
				return "[REDACTED:" + p.kind + "]"
			}
			return p.re.ReplaceAllString(m, p.repl)
		})
	}
	return s, n
}

// secretField khớp tên trường mà giá trị của nó luôn bị che trong kết quả dạng JSON.
var secretField = regexp.MustCompile(`(?i)(password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|credential)`)

// redactText che secret trong kết quả tool. Managed server thường trả JSON đã tuần
// tự hoá; trong chuỗi JSON thô, dấu ngoặc kép và xuống dòng ở dạng \" và \n làm các
// mẫu trượt, nên JSON được giải mã, che trên từng giá trị, rồi tuần tự hoá lại.
func redactText(text string) (string, int) {
	trimmed := strings.TrimSpace(text)
	if trimmed != "" && (trimmed[0] == '{' || trimmed[0] == '[') {
		dec := json.NewDecoder(strings.NewReader(trimmed))
		dec.UseNumber()
		var v any
		if dec.Decode(&v) == nil && !dec.More() {
			n := 0
			v = redactValue(v, &n, 0)
			var b bytes.Buffer
			enc := json.NewEncoder(&b)
			enc.SetEscapeHTML(false)
			if enc.Encode(v) == nil {
				return strings.TrimSuffix(b.String(), "\n"), n
			}
		}
	}
	return redact(text)
}

const maxRedactDepth = 64

func redactValue(v any, n *int, depth int) any {
	if depth > maxRedactDepth {
		*n++
		return "[REDACTED:too_deep]"
	}
	switch x := v.(type) {
	case string:
		s, c := redact(x)
		*n += c
		return s
	case []any:
		for i := range x {
			x[i] = redactValue(x[i], n, depth+1)
		}
	case map[string]any:
		for k, val := range x {
			if s, ok := val.(string); ok && s != "" && secretField.MatchString(k) {
				*n++
				x[k] = "[REDACTED:secret_field]"
				continue
			}
			x[k] = redactValue(val, n, depth+1)
		}
	}
	return v
}
