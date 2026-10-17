package responses

import (
	"io"
	"net/http"
	"net/http/httptest"
	"orchids-api/internal/chatwire"
	"strings"
	"testing"
)

func TestBridgeUsesTypedEntryAndPreservesRequestContext(t *testing.T) {
	called := false
	opts := BridgeOptions{ExecuteChat: func(w http.ResponseWriter, r *http.Request, c chatwire.Request, raw []byte) {
		called = true
		if r.Body != http.NoBody || c.Model != "test" || len(raw) == 0 || r.Header.Get("Authorization") != "Bearer test" || r.URL.Path != "/cline/v1/chat/completions" {
			t.Error("typed entry lost request data")
		}
		_, _ = io.WriteString(w, `{"id":"chat-typed","model":"test","choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`)
	}}
	bridge := ResponsesBridgeHandler(func(http.ResponseWriter, *http.Request) { t.Fatal("HTTP decoding fallback invoked") }, opts)
	r := httptest.NewRequest("POST", "/cline/v1/responses", strings.NewReader(`{"model":"test","input":"hi"}`))
	r.Header.Set("Authorization", "Bearer test")
	w := httptest.NewRecorder()
	bridge(w, r)
	if !called || w.Code != 200 || !strings.Contains(w.Body.String(), "hello") {
		t.Fatalf("bridge failed: %d %s", w.Code, w.Body.String())
	}
}
