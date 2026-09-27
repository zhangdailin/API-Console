package grok

import (
	"github.com/goccy/go-json"
	"net/http"
	"net/http/httptest"
	shared "orchids-api/internal/handler"
	"strings"
	"testing"
)

func TestResponsesBridgePreservesGenerationControlsForSharedHandler(t *testing.T) {
	called := false
	next := func(w http.ResponseWriter, r *http.Request) {
		called = true
		var req shared.ClaudeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.MaxTokens == nil || *req.MaxTokens != 23 || req.Temperature == nil || *req.Temperature != 0 || req.TopP == nil || *req.TopP != 0.5 {
			t.Fatalf("bridge dropped controls: %+v", req)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}
	rec := httptest.NewRecorder()
	ResponsesBridgeHandler(next, ResponsesBridgeOptions{})(rec, httptest.NewRequest(http.MethodPost, "/qoder/v1/responses", strings.NewReader(`{"model":"m","input":"hi","max_output_tokens":23,"temperature":0,"top_p":0.5,"store":false}`)))
	if rec.Code != 200 || !called {
		t.Fatalf("status=%d called=%v body=%s", rec.Code, called, rec.Body.String())
	}
}
