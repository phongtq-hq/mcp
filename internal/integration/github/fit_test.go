package github

import (
	"encoding/base64"
	"net/url"
	"strings"
	"testing"
)

// Qua API công khai mọi mục đều có trần riêng nhỏ hơn ngân sách, nên nhánh "mục tự nó
// quá cỡ" chỉ kiểm được ở đây: mục quá cỡ bị bỏ và đếm, không che các mục đứng sau.
func TestFitSkipsOversizedItems(t *testing.T) {
	items := []string{"a", strings.Repeat("x", 200), "b", "c"}
	kept, oversized, truncated := fit(items, 100)
	if len(kept) != 3 || oversized != 1 || truncated || kept[1] != "b" {
		t.Errorf("mục quá cỡ: kept=%d oversized=%d truncated=%v", len(kept), oversized, truncated)
	}
	kept, oversized, truncated = fit([]string{strings.Repeat("x", 50), strings.Repeat("y", 50), "z"}, 100)
	if len(kept) != 1 || oversized != 0 || !truncated {
		t.Errorf("hết ngân sách: kept=%d oversized=%d truncated=%v", len(kept), oversized, truncated)
	}
	_, info := pageOf([]string{strings.Repeat("x", MaxResultBytes), "ok"}, Page{Page: 3, PerPage: 2}, true)
	if info.OversizedOmitted != 1 || info.Truncated || info.NextPage != 4 {
		t.Errorf("pageOf: %+v", info)
	}
}

// scrubError che token của server ở mọi dạng mà một thông báo lỗi có thể chép lại.
func TestScrubError(t *testing.T) {
	const token = "github_pat_11SCRUB+secret/0123456789=="
	c, err := NewClient("https://api.github.example", Options{Token: token, Repositories: []string{"acme/api"}})
	if err != nil {
		t.Fatal(err)
	}
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	raw := func(s string) string { return base64.RawStdEncoding.EncodeToString([]byte(s)) }
	for name, in := range map[string]string{
		"nguyên văn":                 "bad token " + token,
		"dính liền sau chữ":          "prefix" + token + "suffix",
		"dính sau gạch dưới":         "key_" + token,
		"mã hoá phần trăm (query)":   "at " + url.QueryEscape(token),
		"mã hoá phần trăm (path)":    "at /x/" + url.PathEscape(token) + "/y",
		"base64 của token":           "blob " + b64(token),
		"base64 không đệm":           "blob " + raw(token) + ".",
		"base64 url":                 "blob " + base64.URLEncoding.EncodeToString([]byte(token)),
		"basic x:token":              "Authorization: Basic " + b64("x:"+token),
		"basic x-access-token:token": "sent Basic " + b64("x-access-token:"+token) + " to upstream",
		"basic token:":               "hdr=" + b64(token+":"),
		"basic token:x-oauth-basic":  "hdr=" + b64(token+":x-oauth-basic"),
		"header bearer":              "Authorization: Bearer " + token,
		"header token (kiểu cũ)":     "authorization: token " + token,
		"userinfo của URL":           "GET https://x-access-token:" + token + "@github.internal/acme/api.git failed",
		"query của URL":              "GET https://proxy.internal/x?access_token=" + token + "&y=1 failed",
	} {
		out := c.scrubError(in)
		for _, leak := range []string{token, "SCRUB", url.QueryEscape(token), b64(token), raw(token), raw("x:" + token), raw("x-access-token:" + token), raw(token + ":")} {
			if strings.Contains(out, leak) {
				t.Errorf("%s: còn lộ %q trong %q", name, leak, out)
			}
		}
	}
	// Token GitHub KHÁC token của server, và credential Basic bất kỳ, cũng bị che.
	for name, in := range map[string]string{
		"ghp sau chữ":         "xghp_abcdefghij0123456789",
		"gho":                 "tok=gho_abcdefghij0123456789",
		"ghu":                 "_ghu_abcdefghij0123456789",
		"ghs":                 "9ghs_abcdefghij0123456789",
		"ghr":                 "ghr_abcdefghij0123456789",
		"fine-grained":        "agithub_pat_11ABCDEFG0abcdefghij_klmnopqrstuvwxyz",
		"basic bất kỳ":        "Authorization: Basic " + b64("someone:hunter2-password"),
		"bearer bất kỳ":       "authorization: bearer abcdefghij0123456789",
		"userinfo bất kỳ":     "https://someone:hunter2-password@host/x",
		"query string bất kỳ": "https://host/x?sig=abcdefghij0123456789",
	} {
		out := c.scrubError(in)
		for _, leak := range []string{"abcdefghij", "hunter2", b64("someone:hunter2-password")} {
			if strings.Contains(out, leak) {
				t.Errorf("%s: còn lộ %q trong %q", name, leak, out)
			}
		}
	}
	// Văn bản thường không bị phá, và luôn có trần độ dài.
	if got := c.scrubError("Validation Failed: ref does not exist"); got != "Validation Failed: ref does not exist" {
		t.Errorf("văn bản thường bị đổi: %q", got)
	}
	if got := c.scrubError(strings.Repeat("a ", 5000)); len(got) > maxErrorBytes+4 {
		t.Errorf("thông báo dài phải bị cắt: %d byte", len(got))
	}
}

// Ref ẩn của pull request bị từ chối ở mọi cách viết; branch thường thì không.
func TestValidRef(t *testing.T) {
	for _, bad := range []string{"refs/pull/1/head", "refs/pull/1/merge", "pull/1/head", "REFS/PULL/1/head", "Pull/1/merge", "a..b", "a//b", "-x", "", "a b"} {
		if validRef(bad) {
			t.Errorf("%q phải bị từ chối", bad)
		}
	}
	for _, ok := range []string{"main", "release/1.2", "feature/pull-request-fix", "pulls", "refs/heads/main", "v1.0.0", "0123456789abcdef"} {
		if !validRef(ok) {
			t.Errorf("%q phải được nhận", ok)
		}
	}
}
