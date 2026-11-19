package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hqgroups/mcp/internal/ai/modelcatalog"
	"github.com/hqgroups/mcp/internal/ai/provider/anthropic"
)

const platformLLMKey = "sk-ant-platform-key-000000"

// fakeAnthropic là Anthropic API giả: chỉ chấp nhận một số khoá, ghi lại khoá của
// từng lượt gọi model, và trả một câu trả lời cố định.
type fakeAnthropic struct {
	mu    sync.Mutex
	valid map[string]bool
	down  bool
	// downModels: các model mà provider trả 500 (để thử chuỗi model dự phòng)
	downModels map[string]bool
	keys       []string // khoá của từng lượt POST /v1/messages
	// model và max_tokens của từng lượt POST /v1/messages
	models    []string
	maxTokens []float64
	// hold khác nil: lượt gọi model chờ tới khi kênh đóng (giữ task ở trạng thái chạy)
	hold chan struct{}
	srv  *httptest.Server
}

func newFakeAnthropic(t *testing.T) *fakeAnthropic {
	f := &fakeAnthropic{valid: map[string]bool{platformLLMKey: true}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.mu.Lock()
		ok, down, hold := f.valid[key], f.down, f.hold
		if r.URL.Path == "/v1/messages" {
			var body struct {
				Model     string  `json:"model"`
				MaxTokens float64 `json:"max_tokens"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.keys = append(f.keys, key)
			f.models = append(f.models, body.Model)
			f.maxTokens = append(f.maxTokens, body.MaxTokens)
			down = down || f.downModels[body.Model]
		}
		f.mu.Unlock()
		if r.URL.Path == "/v1/messages" && hold != nil {
			select {
			case <-hold:
			case <-r.Context().Done():
			}
		}
		w.Header().Set("request-id", "req_fake")
		switch {
		case down:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(500)
			_, _ = fmt.Fprint(w, `{"type":"error","error":{"type":"api_error","message":"down"}}`)
		case !ok:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(401)
			_, _ = fmt.Fprint(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
		case r.URL.Path == "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"data":[],"has_more":false,"first_id":null,"last_id":null}`)
		case r.URL.Path == "/v1/messages":
			w.Header().Set("Content-Type", "text/event-stream")
			for _, ev := range []string{
				`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"served-model","content":[],"stop_reason":null,"usage":{"input_tokens":100,"output_tokens":1}}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"trả lời từ API"}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":20}}`,
				`{"type":"message_stop"}`,
			} {
				typ := strings.SplitN(strings.SplitN(ev, `"type":"`, 2)[1], `"`, 2)[0]
				_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typ, ev)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAnthropic) accept(keys ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range keys {
		f.valid[k] = true
	}
}

// setModelDown làm provider trả lỗi 500 cho một model.
func (f *fakeAnthropic) setModelDown(model string, down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.downModels == nil {
		f.downModels = map[string]bool{}
	}
	f.downModels[model] = down
}

// calledModels trả về model của từng lượt POST /v1/messages từ chỉ số from.
func (f *fakeAnthropic) calledModels(from int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.models[min(from, len(f.models)):]...)
}

func (f *fakeAnthropic) setDown(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down = down
}

func (f *fakeAnthropic) usedKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.keys...)
}

// fakeCredentials kiểm khoá bằng Verifier thật, trỏ vào API giả.
type fakeCredentials struct{ base string }

func (fakeCredentials) Supports(adapter string) bool { return adapter == anthropic.Name }

func (c fakeCredentials) Verify(ctx context.Context, _, apiKey string) error {
	err := anthropic.Verifier{BaseURL: c.base}.Verify(ctx, apiKey)
	if errors.Is(err, anthropic.ErrInvalidKey) {
		return modelcatalog.ErrCredentialInvalid
	}
	return err
}

func (f *fakeAnthropic) setHold(ch chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hold = ch
}

func (f *fakeAnthropic) lastCall() (model string, maxTokens float64, calls int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.models) == 0 {
		return "", 0, 0
	}
	return f.models[len(f.models)-1], f.maxTokens[len(f.maxTokens)-1], len(f.models)
}
