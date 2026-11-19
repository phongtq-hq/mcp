package gateway

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hqgroups/mcp/internal/mcp/registry"
)

func TestRedact(t *testing.T) {
	for name, c := range map[string]struct{ in, secret string }{
		"private key":   {"-----BEGIN RSA PRIVATE KEY-----\nMIIEabc\n-----END RSA PRIVATE KEY-----", "MIIEabc"},
		"truncated key": {"-----BEGIN PRIVATE KEY-----\nMIIEabc", "MIIEabc"},
		"anthropic":     {"key=sk-ant-api03-abcdefghijklmnopqrstu", "abcdefghijklmnop"},
		"github":        {"ghp_abcdefghijklmnopqrstuvwxyz0123456789", "abcdefghijklmnop"},
		"aws":           {"id AKIAIOSFODNN7EXAMPLE here", "AKIAIOSFODNN7EXAMPLE"},
		"jwt":           {"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0In0.c2lnbmF0dXJlMTIz", "c2lnbmF0dXJl"},
		"url password":  {"postgres://app:hunter2@db:5432/x", "hunter2"},
		"assignment":    {`POSTGRES_PASSWORD="s3cr3t value"`, "s3cr3t"},
		"bearer":        {"Authorization: Bearer abcdefghijklmnopqrstuvwx", "abcdefghijklmnop"},
	} {
		out, n := redact(c.in)
		if n == 0 || strings.Contains(out, c.secret) {
			t.Errorf("%s: không che được: %q", name, out)
		}
	}
	if out, n := redact("container web is running on port 8080"); n != 0 || out != "container web is running on port 8080" {
		t.Errorf("văn bản thường bị đổi: %q", out)
	}
}

func TestSanitize(t *testing.T) {
	res := &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: strings.Repeat("é", 100)},
			&mcp.ImageContent{Data: []byte("x"), MIMEType: "image/png"},
		},
		StructuredContent: map[string]any{"token": "should-not-pass-through"},
	}
	out, o := sanitize(res, 51) // cắt giữa một ký tự 2 byte
	text := out.Content[0].(*mcp.TextContent).Text
	if !o.truncated || len(out.Content) != 1 || out.StructuredContent != nil {
		t.Fatalf("sanitize: %+v %+v", out, o)
	}
	if !strings.HasPrefix(text, strings.Repeat("é", 25)+"\n[") || strings.Contains(text, "�") {
		t.Errorf("cắt sai ranh giới UTF-8: %q", text)
	}
	if !strings.Contains(text, "không phải văn bản") {
		t.Error("thiếu ghi chú về nội dung bị bỏ")
	}
}

func TestValidateArguments(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","additionalProperties":false,"required":["name"],"properties":{"name":{"type":"string","maxLength":5}}}`)
	for in, ok := range map[string]bool{
		`{"name":"web"}`: true, `{}`: false, ``: false, `{"name":"toolong"}`: false,
		`{"name":"a","x":1}`: false, `[1]`: false, `null`: false, `"s"`: false,
	} {
		if _, err := validateArguments(schema, json.RawMessage(in)); (err == nil) != ok {
			t.Errorf("%q: err = %v, muốn ok = %v", in, err, ok)
		}
	}
	if _, err := validateArguments(schema, json.RawMessage(`{"name":"`+strings.Repeat("a", maxArgumentBytes)+`"}`)); err == nil {
		t.Error("arguments quá lớn được chấp nhận")
	}
}

func TestRateLimiter(t *testing.T) {
	l := newRateLimiter(2, time.Minute)
	a, b, now := uuid.New(), uuid.New(), time.Now()
	if ok, _ := l.allow(a, now); !ok {
		t.Fatal("lần 1")
	}
	if ok, _ := l.allow(a, now); !ok {
		t.Fatal("lần 2")
	}
	if ok, wait := l.allow(a, now.Add(10*time.Second)); ok || wait != 50*time.Second {
		t.Errorf("lần 3: ok=%v wait=%v", ok, wait)
	}
	if ok, _ := l.allow(b, now); !ok {
		t.Error("principal khác bị ảnh hưởng")
	}
	if ok, _ := l.allow(a, now.Add(time.Minute)); !ok {
		t.Error("cửa sổ mới không mở lại")
	}
}

// Managed server trả JSON đã tuần tự hoá: secret nằm sau \" và \n vẫn phải bị che.
func TestSanitizeRedactsInsideJSON(t *testing.T) {
	logs := "AKIAIOSFODNN7EXAMPLE first line\n\tsk-abcdefghijklmnopqrstuvwxyz012345\n{\"db_password\":\"hunter2-super-secret\"}\nPOSTGRES_PASSWORD=topsecret99"
	payload, _ := json.Marshal(map[string]any{"name": "web", "logs": logs, "api_token": "plain-value-123", "count": 3})
	out, o := sanitize(&mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(payload)}}}, 1<<20)
	text := out.Content[0].(*mcp.TextContent).Text
	for _, secret := range []string{"AKIAIOSFODNN7EXAMPLE", "sk-abcdefghijklmnop", "hunter2-super-secret", "topsecret99", "plain-value-123"} {
		if strings.Contains(text, secret) {
			t.Errorf("secret %q lọt qua trong JSON: %s", secret, text)
		}
	}
	var back map[string]any
	if err := json.Unmarshal([]byte(text), &back); err != nil || back["name"] != "web" || o.redacted < 5 {
		t.Errorf("kết quả phải vẫn là JSON hợp lệ và giữ dữ liệu thường: %v %s (%d chỗ che)", err, text, o.redacted)
	}
}

func TestLimitedBody(t *testing.T) {
	b := &limitedBody{ReadCloser: io.NopCloser(strings.NewReader(strings.Repeat("x", 100))), left: 10}
	got, err := io.ReadAll(b)
	if len(got) != 10 || !errors.Is(err, errUpstreamTooLarge) {
		t.Errorf("đọc %d byte, err = %v", len(got), err)
	}
}

func TestAllowedByRules(t *testing.T) {
	s1, s2 := uuid.New(), uuid.New()
	tool := func(server uuid.UUID, name string) registry.Tool { return registry.Tool{ServerID: server, Name: name} }
	for name, c := range map[string]struct {
		rules []ToolRule
		tool  registry.Tool
		want  bool
	}{
		"no rules":                 {nil, tool(s1, "a"), false},
		"named":                    {[]ToolRule{{ServerID: s1, Allow: []string{"a"}}}, tool(s1, "a"), true},
		"not named":                {[]ToolRule{{ServerID: s1, Allow: []string{"a"}}}, tool(s1, "b"), false},
		"other server":             {[]ToolRule{{ServerID: s1, Allow: []string{"*"}}}, tool(s2, "a"), false},
		"wildcard":                 {[]ToolRule{{ServerID: s1, Allow: []string{"*"}}}, tool(s1, "b"), true},
		"deny beats wildcard":      {[]ToolRule{{ServerID: s1, Allow: []string{"*"}, Deny: []string{"b"}}}, tool(s1, "b"), false},
		"deny wildcard":            {[]ToolRule{{ServerID: s1, Allow: []string{"a"}, Deny: []string{"*"}}}, tool(s1, "a"), false},
		"deny wins across entries": {[]ToolRule{{ServerID: s1, Allow: []string{"*"}, Deny: []string{"x"}}, {ServerID: s1, Allow: []string{"x"}}}, tool(s1, "x"), false},
		"deny on another server":   {[]ToolRule{{ServerID: s2, Allow: []string{"*"}, Deny: []string{"a"}}, {ServerID: s1, Allow: []string{"a"}}}, tool(s1, "a"), true},
	} {
		if got := allowedByRules(c.rules, c.tool); got != c.want {
			t.Errorf("%s: %v, muốn %v", name, got, c.want)
		}
	}
}
