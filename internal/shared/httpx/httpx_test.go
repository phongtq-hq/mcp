package httpx_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apperr "github.com/hqgroups/mcp/internal/shared/errors"
	"github.com/hqgroups/mcp/internal/shared/httpx"
)

func decodeProblem(t *testing.T, rec *httptest.ResponseRecorder) httpx.Problem {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q", ct)
	}
	var p httpx.Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return p
}

func TestWriteErrorMapsKindToStatus(t *testing.T) {
	cases := []struct {
		kind   apperr.Kind
		status int
	}{
		{apperr.KindInvalid, 400},
		{apperr.KindUnauthenticated, 401},
		{apperr.KindBudgetExceeded, 402},
		{apperr.KindPermissionDenied, 403},
		{apperr.KindNotFound, 404},
		{apperr.KindConflict, 409},
		{apperr.KindPreconditionFailed, 412},
		{apperr.KindUnprocessable, 422},
		{apperr.KindRateLimited, 429},
		{apperr.KindUnavailable, 503},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		httpx.WriteError(rec, httptest.NewRequest("GET", "/", nil), apperr.New(c.kind, "some_code", "thông báo"))
		p := decodeProblem(t, rec)
		if rec.Code != c.status || p.Status != c.status || p.Code != "some_code" || p.Detail != "thông báo" {
			t.Errorf("kind %d: status=%d body=%+v, muốn %d", c.kind, rec.Code, p, c.status)
		}
	}
}

func TestWriteErrorDoesNotLeakInternalDetail(t *testing.T) {
	secret := "postgres://user:hunter2@db/prod"
	for name, err := range map[string]error{
		"unclassified":   errors.New(secret),
		"wrapped":        apperr.Wrap(errors.New(secret), apperr.KindNotFound, "not_found", "không tồn tại"),
		"internal kind":  apperr.New(apperr.KindInternal, "db_down", secret),
		"wrapped stdlib": errors.Join(errors.New(secret)),
	} {
		rec := httptest.NewRecorder()
		httpx.WriteError(rec, httptest.NewRequest("GET", "/", nil), err)
		if strings.Contains(rec.Body.String(), "hunter2") {
			t.Errorf("%s: phản hồi lộ chi tiết nội bộ: %s", name, rec.Body.String())
		}
	}
}

func TestWriteErrorFieldErrors(t *testing.T) {
	problem := func(err error) (int, map[string]any) {
		rec := httptest.NewRecorder()
		httpx.WriteError(rec, httptest.NewRequest("POST", "/", nil), err)
		var p map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		return rec.Code, p
	}
	// Lỗi validate gắn với trường: `errors[]` mang trường và thông báo; `detail` vẫn đủ nghĩa.
	status, p := problem(apperr.InvalidField("slug", "slug phải khớp mẫu"))
	errs, _ := p["errors"].([]any)
	if status != 422 || p["code"] != "validation_failed" || p["detail"] != "slug phải khớp mẫu" || len(errs) != 1 {
		t.Fatalf("status %d, problem %v", status, p)
	}
	if f := errs[0].(map[string]any); f["field"] != "slug" || f["message"] != "slug phải khớp mẫu" {
		t.Errorf("errors[0] = %v", f)
	} else if _, has := f["code"]; has {
		t.Errorf("code rỗng không được xuất hiện: %v", f)
	}
	// Lỗi không có chi tiết theo trường: không có khoá `errors`.
	if _, p := problem(apperr.New(apperr.KindUnprocessable, "validation_failed", "x")); p["errors"] != nil {
		t.Errorf("errors phải vắng mặt: %v", p)
	}
	// Chi tiết theo trường không đi kèm lỗi không phải của request (409, 500).
	conflict := apperr.New(apperr.KindConflict, "slug_taken", "đã dùng")
	conflict.Fields = []apperr.FieldError{{Field: "slug", Message: "đã dùng"}}
	if status, p := problem(conflict); status != 409 || p["errors"] != nil {
		t.Errorf("409 không mang errors[]: %d %v", status, p)
	}
	internal := apperr.Wrap(errors.New("dsn=secret"), apperr.KindInternal, "internal_error", "x")
	internal.Fields = []apperr.FieldError{{Field: "f", Message: "dsn=secret"}}
	if status, p := problem(internal); status != 500 || p["errors"] != nil {
		t.Errorf("500 không mang errors[]: %d %v", status, p)
	}
	// Có trần số mục.
	many := apperr.InvalidField("f0", "m")
	for i := 0; i < 200; i++ {
		many.Fields = append(many.Fields, apperr.FieldError{Field: "f", Message: "m"})
	}
	if _, p := problem(many); len(p["errors"].([]any)) != 50 {
		t.Errorf("trần errors[]: %d", len(p["errors"].([]any)))
	}
}

func TestRecoverTurnsPanicInto500(t *testing.T) {
	h := httpx.Recover(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("bí mật") }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	p := decodeProblem(t, rec)
	if rec.Code != 500 || p.Code != "internal_error" || strings.Contains(rec.Body.String(), "bí mật") {
		t.Errorf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestLimitBody(t *testing.T) {
	h := httpx.LimitBody(4)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
		}
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/", strings.NewReader("0123456789")))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, muốn 413", rec.Code)
	}
}

func TestHealth(t *testing.T) {
	mux := http.NewServeMux()
	httpx.RegisterHealth(mux, map[string]httpx.Check{
		"good": func(context.Context) error { return nil },
		"bad":  func(context.Context) error { return errors.New("dial tcp 10.0.0.5:5432: refused") },
	})

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Errorf("/healthz = %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != 503 {
		t.Errorf("/readyz = %d, muốn 503", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "10.0.0.5") {
		t.Errorf("/readyz lộ chi tiết phụ thuộc: %s", rec.Body.String())
	}
	var got map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["good"] != "ok" || got["bad"] != "unavailable" {
		t.Errorf("body = %v", got)
	}
}

func TestServeDrainsInFlightRequestOnShutdown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = w.Write([]byte("done"))
	})

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- httpx.ServeListener(ctx, ln, h, 5*time.Second) }()

	type result struct {
		body string
		err  error
	}
	resc := make(chan result, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/")
		if err != nil {
			resc <- result{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		resc <- result{body: string(b), err: err}
	}()

	<-started
	cancel() // yêu cầu dừng khi request còn đang xử lý
	select {
	case err := <-served:
		t.Fatalf("server dừng trước khi request xong: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)

	if r := <-resc; r.err != nil || r.body != "done" {
		t.Errorf("request đang xử lý bị cắt: body=%q err=%v", r.body, r.err)
	}
	if err := <-served; err != nil {
		t.Errorf("Serve trả lỗi sau shutdown sạch: %v", err)
	}
}
