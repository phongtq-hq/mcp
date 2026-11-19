package api_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hqgroups/mcp/internal/ai/provider"
	"github.com/hqgroups/mcp/internal/platform/secrets"
	"github.com/hqgroups/mcp/internal/shared/safehttp"
	"github.com/hqgroups/mcp/internal/workflow/notify"
)

type hookCall struct {
	sig, delivery, event string
	body                 []byte
}

// TestNotificationWebhook: thông báo "approval.requested" đi từ trigger của approvals
// qua outbox tới webhook của org, do bộ gửi của gateway ký và gửi.
func TestNotificationWebhook(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	adminA, adminB, viewerA := e.tok("admin-a"), e.tok("admin-b"), e.tok("viewer-a")
	f := newPlannedFixture(t, e, "notify-a")
	mon := f.agent("monitoring", []string{"list_containers"}, nil)
	rep := f.agent("reporter", nil, nil)
	e.model.script(func(_ context.Context, _ int, _ provider.Request) (provider.Response, error) {
		return answer("xong"), nil
	})

	var (
		mu     sync.Mutex
		calls  []hookCall
		status = http.StatusNoContent
	)
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		mu.Lock()
		calls = append(calls, hookCall{sig: r.Header.Get("X-HQ-Signature"), delivery: r.Header.Get("X-HQ-Delivery"), event: r.Header.Get("X-HQ-Event"), body: b})
		st := status
		mu.Unlock()
		w.WriteHeader(st)
	}))
	t.Cleanup(recv.Close)
	received := func() []hookCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]hookCall(nil), calls...)
	}
	gwPool := e.db.PoolAs(t, "app_gateway", 4)
	disp := notify.NewDispatcher(gwPool, secrets.NewReader(gwPool, e.opener), safehttp.Policy{AllowInsecureLocal: true}, nil)
	tick := func() int {
		t.Helper()
		n, err := disp.Tick(ctx)
		if err != nil {
			t.Fatalf("tick: %v", err)
		}
		return n
	}
	deliveries := func(tok string) []any {
		t.Helper()
		r := e.do("GET", "/v1/notification-webhook/deliveries", tok, nil, nil)
		want(t, r, 200, "")
		return r.json(t)["data"].([]any)
	}
	makeDue := func() {
		t.Helper()
		if _, err := e.db.Admin.Exec(ctx, `UPDATE notification_deliveries SET next_attempt_at = now() - interval '1 second' WHERE status = 'pending'`); err != nil {
			t.Fatal(err)
		}
	}
	r := f.createWorkflow(adminA, "notify-gate", gateDef(mon, rep, 1))
	want(t, r, 201, "")
	wfID := r.json(t)["id"].(string)
	f.activate(wfID)
	gate := func() map[string]any {
		t.Helper()
		run := f.runWorkflow(adminA, wfID)
		want(t, run, 202, "")
		return f.waitGate(run.json(t)["id"].(string), 20*time.Second)
	}

	var secret string
	t.Run("configuration: permission, validation, tenant isolation, secret shown once", func(t *testing.T) {
		want(t, e.do("GET", "/v1/notification-webhook", adminA, nil, nil), 404, "not_found")
		want(t, e.do("PUT", "/v1/notification-webhook", viewerA, map[string]any{"url": recv.URL}, nil), 403, "permission_denied")
		want(t, e.do("GET", "/v1/notification-webhook", viewerA, nil, nil), 403, "permission_denied")
		// Chính sách SSRF mặc định: http và địa chỉ nội bộ bị từ chối khi ghi.
		loose := e.api
		e.api = e.strictAPI
		for _, u := range []string{recv.URL, "https://127.0.0.1/hook", "https://169.254.169.254/latest", "https://user:pw@example.com/h", "ftp://example.com/h"} {
			want(t, e.do("PUT", "/v1/notification-webhook", adminA, map[string]any{"url": u}, nil), 422, "validation_failed")
		}
		e.api = loose
		want(t, e.do("PUT", "/v1/notification-webhook", adminA, map[string]any{"url": recv.URL + "/h?token=abc"}, nil), 422, "validation_failed")

		put := e.do("PUT", "/v1/notification-webhook", adminA, map[string]any{"url": recv.URL + "/hook"}, nil)
		want(t, put, 200, "")
		secret, _ = put.json(t)["signing_secret"].(string)
		if !strings.HasPrefix(secret, notify.SecretPrefix) || len(secret) < 40 {
			t.Fatalf("signing_secret = %q", secret)
		}
		get := e.do("GET", "/v1/notification-webhook", adminA, nil, nil)
		want(t, get, 200, "")
		if strings.Contains(string(get.body), secret) || get.json(t)["signing_secret"] != nil {
			t.Fatal("GET trả khoá ký")
		}
		// Org khác không thấy webhook của org A.
		want(t, e.do("GET", "/v1/notification-webhook", adminB, nil, nil), 404, "not_found")
		want(t, e.do("DELETE", "/v1/notification-webhook", adminB, nil, nil), 204, "")
		want(t, e.do("GET", "/v1/notification-webhook", adminA, nil, nil), 200, "")
		// Khoá ký không nằm trong audit hay ở dạng rõ trong bảng secrets.
		var leaked int
		if err := e.db.Admin.QueryRow(ctx, `SELECT (SELECT count(*) FROM audit_logs WHERE details::text LIKE '%' || $1 || '%')
			+ (SELECT count(*) FROM secrets WHERE position(convert_to($1, 'UTF8') in ciphertext) > 0)`, secret).Scan(&leaked); err != nil {
			t.Fatal(err)
		}
		if leaked != 0 {
			t.Fatal("khoá ký lọt vào audit hoặc secrets ở dạng rõ")
		}
	})

	t.Run("an approval is announced once, signed, with identifiers only", func(t *testing.T) {
		ap := gate()
		if n := tick(); n != 1 {
			t.Fatalf("tick xử lý %d dòng, muốn 1", n)
		}
		got := received()
		if len(got) != 1 {
			t.Fatalf("bên nhận có %d request, muốn 1", len(got))
		}
		c := got[0]
		if c.event != notify.EventApprovalRequested || c.delivery == "" {
			t.Fatalf("header: %+v", c)
		}
		// Chữ ký kiểm được bằng khoá đã trả lúc PUT.
		ts, v1, ok := strings.Cut(strings.TrimPrefix(c.sig, "t="), ",v1=")
		sec, err := strconv.ParseInt(ts, 10, 64)
		if !ok || err != nil || v1 == "" {
			t.Fatalf("X-HQ-Signature = %q", c.sig)
		}
		if notify.Sign([]byte(secret), time.Unix(sec, 0), c.body) != c.sig {
			t.Fatal("chữ ký không khớp với khoá ký")
		}
		if notify.Sign([]byte(secret+"x"), time.Unix(sec, 0), c.body) == c.sig {
			t.Fatal("chữ ký không phụ thuộc khoá")
		}
		body := string(c.body)
		if !strings.Contains(body, ap["id"].(string)) || !strings.Contains(body, `"subject_type":"workflow_step"`) {
			t.Fatalf("thân sự kiện: %s", body)
		}
		// Thông điệp của cổng và mọi nội dung khác không rời platform qua webhook.
		if strings.Contains(body, "DUYET") || strings.Contains(body, "message") || strings.Contains(body, "payload") {
			t.Fatalf("thân sự kiện mang nội dung: %s", body)
		}
		ds := deliveries(adminA)
		if len(ds) != 1 || ds[0].(map[string]any)["status"] != "delivered" || ds[0].(map[string]any)["id"] != c.delivery {
			t.Fatalf("deliveries: %v", ds)
		}
		if len(deliveries(adminB)) != 0 {
			t.Fatal("org B thấy lịch sử gửi của org A")
		}
		want(t, e.do("GET", "/v1/notification-webhook/deliveries", viewerA, nil, nil), 403, "permission_denied")
		// Không gửi lại dòng đã xong.
		makeDue()
		if n := tick(); n != 0 || len(received()) != 1 {
			t.Fatalf("gửi lại thông báo đã xong: n=%d", n)
		}
		want(t, f.decide(e.tok("multi-a"), ap["id"].(string), "approve"), 200, "")
	})

	t.Run("a failing receiver is retried; a closed approval is not announced", func(t *testing.T) {
		mu.Lock()
		status, calls = http.StatusInternalServerError, nil
		mu.Unlock()
		ap := gate()
		tick()
		d := deliveries(adminA)[0].(map[string]any)
		if d["status"] != "pending" || d["attempts"] != float64(1) || d["last_http_status"] != float64(500) || d["last_error_code"] != "http_status" {
			t.Fatalf("sau lần thử hỏng: %v", d)
		}
		// Chưa tới hạn thử lại thì không gửi.
		if n := tick(); n != 0 {
			t.Fatalf("thử lại trước hạn: %d", n)
		}
		makeDue()
		tick()
		if n := len(received()); n != 2 {
			t.Fatalf("bên nhận có %d request, muốn 2", n)
		}
		// Approval đã có quyết định: lần thử kế tiếp bỏ thông báo thay vì báo sai.
		want(t, f.decide(e.tok("multi-a"), ap["id"].(string), "reject"), 200, "")
		makeDue()
		tick()
		d = deliveries(adminA)[0].(map[string]any)
		if d["status"] != "dropped" || d["last_error_code"] != "approval_closed" || len(received()) != 2 {
			t.Fatalf("sau khi approval đóng: %v", d)
		}
	})

	t.Run("exhausted attempts fail the delivery; repeated failures disable the webhook", func(t *testing.T) {
		ap := gate()
		for i := 0; i < notify.MaxAttempts+1; i++ {
			makeDue()
			tick()
		}
		d := deliveries(adminA)[0].(map[string]any)
		if d["status"] != "failed" || d["attempts"] != float64(notify.MaxAttempts) {
			t.Fatalf("sau khi hết lượt thử: %v", d)
		}
		wh := e.do("GET", "/v1/notification-webhook", adminA, nil, nil).json(t)
		if wh["consecutive_failures"] != float64(1) || wh["enabled"] != true {
			t.Fatalf("webhook: %v", wh)
		}
		// Đẩy bộ đếm tới sát ngưỡng rồi cho một thông báo nữa hỏng hẳn.
		if _, err := e.db.Admin.Exec(ctx, `UPDATE notification_webhooks SET consecutive_failures = 19 WHERE org_id = $1`, e.orgA); err != nil {
			t.Fatal(err)
		}
		want(t, f.decide(e.tok("multi-a"), ap["id"].(string), "reject"), 200, "")
		ap = gate()
		if _, err := e.db.Admin.Exec(ctx, `UPDATE notification_deliveries SET attempts = 5 WHERE status = 'pending'`); err != nil {
			t.Fatal(err)
		}
		makeDue()
		tick()
		wh = e.do("GET", "/v1/notification-webhook", adminA, nil, nil).json(t)
		if wh["enabled"] != false || wh["disabled_reason"] != "delivery_failures" {
			t.Fatalf("webhook chưa bị tắt: %v", wh)
		}
		var audits int
		if err := e.db.Admin.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE org_id = $1 AND action = 'notification_webhook.auto_disable'`, e.orgA).Scan(&audits); err != nil || audits != 1 {
			t.Fatalf("audit auto_disable = %d (%v)", audits, err)
		}
		want(t, f.decide(e.tok("multi-a"), ap["id"].(string), "reject"), 200, "")
		// Webhook đã tắt: approval mới không xếp thông báo.
		before := len(deliveries(adminA))
		ap = gate()
		if len(deliveries(adminA)) != before {
			t.Fatal("approval mới vẫn xếp thông báo khi webhook đã tắt")
		}
		want(t, f.decide(e.tok("multi-a"), ap["id"].(string), "reject"), 200, "")
		// PUT lại bật webhook với khoá ký mới.
		put := e.do("PUT", "/v1/notification-webhook", adminA, map[string]any{"url": recv.URL + "/hook"}, nil)
		want(t, put, 200, "")
		if s := put.json(t)["signing_secret"].(string); s == secret || put.json(t)["enabled"] != true || put.json(t)["consecutive_failures"] != float64(0) {
			t.Fatalf("PUT lại: %v", put.json(t))
		}
		want(t, e.do("DELETE", "/v1/notification-webhook", adminA, nil, nil), 204, "")
		want(t, e.do("DELETE", "/v1/notification-webhook", adminA, nil, nil), 204, "")
		want(t, e.do("GET", "/v1/notification-webhook", adminA, nil, nil), 404, "not_found")
	})
}
