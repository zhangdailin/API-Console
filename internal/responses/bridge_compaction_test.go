package responses

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"orchids-api/internal/chatwire"

	"strings"
	"testing"
	"time"

	"orchids-api/internal/middleware"
	"orchids-api/internal/store"
)

func TestBridgeCompactionRoundTripAndIsolation(t *testing.T) {
	for _, channel := range []string{"workbuddy", "qoder", "cline"} {
		t.Run(channel, func(t *testing.T) {
			st := store.NewMemoryResponseStore(0)
			var got chatwire.Request
			calls := 0
			chat := func(w http.ResponseWriter, r *http.Request) {
				calls++
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Fatal(err)
				}
				_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"Keep task A and file main.go"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7}}`)
			}
			opts := BridgeOptions{Store: st}
			path := "/" + channel + "/v1/responses"
			compact := httptest.NewRecorder()
			ResponsesBridgeCompactHandler(chat, opts)(compact, httptest.NewRequest(http.MethodPost, path+"/compact", strings.NewReader(`{"model":"m","input":"task A","instructions":"keep paths"}`)))
			if compact.Code != 200 || got.Stream || got.ToolChoice != "none" {
				t.Fatalf("status=%d body=%s request=%#v", compact.Code, compact.Body.String(), got)
			}
			var result map[string]interface{}
			if err := json.Unmarshal(compact.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			item := result["output"].([]interface{})[0].(map[string]interface{})
			if item["type"] != "compaction" || !strings.HasPrefix(item["encrypted_content"].(string), bridgeCompactionPrefix) {
				t.Fatalf("bad compaction: %v", result)
			}
			input := []interface{}{item, map[string]interface{}{"type": "message", "role": "user", "content": "continue"}}
			body, _ := json.Marshal(map[string]interface{}{"model": "m", "input": input})
			next := httptest.NewRecorder()
			ResponsesBridgeHandler(chat, opts)(next, httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body))))
			if next.Code != 200 || len(got.Messages) != 2 || !strings.Contains(got.Messages[0].Content.(string), "task A") {
				t.Fatalf("continuation=%d %s messages=%v", next.Code, next.Body.String(), got.Messages)
			}
			for _, other := range []string{"/grok/v1/responses", path} {
				model := "m"
				if other == path {
					model = "other-model"
				}
				raw, _ := json.Marshal(map[string]interface{}{"model": model, "input": input})
				bad := httptest.NewRecorder()
				before := calls
				ResponsesBridgeHandler(chat, opts)(bad, httptest.NewRequest(http.MethodPost, other, strings.NewReader(string(raw))))
				if bad.Code != 400 || calls != before {
					t.Fatalf("cross channel/model accepted: %d %s", bad.Code, bad.Body.String())
				}
			}
			id := strings.TrimPrefix(item["encrypted_content"].(string), bridgeCompactionPrefix)
			if err := st.DeleteStoredResponse(context.Background(), id, "anonymous"); err != nil {
				t.Fatal(err)
			}
			missing := httptest.NewRecorder()
			ResponsesBridgeHandler(chat, opts)(missing, httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body))))
			if missing.Code != 400 {
				t.Fatalf("missing state accepted: %d", missing.Code)
			}
		})
	}
}

type failedCompactionStore struct{ Store }

func (failedCompactionStore) GetStoredResponse(context.Context, string, string) (*store.StoredResponse, error) {
	return nil, errors.New("storage offline")
}
func (failedCompactionStore) SaveStoredResponse(context.Context, *store.StoredResponse, time.Duration) error {
	return errors.New("storage offline")
}

func TestCompactionStorageOutageIsNotAClientError(t *testing.T) {
	opts := BridgeOptions{Store: failedCompactionStore{}}
	calls := 0
	chat := func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"summary"},"finish_reason":"stop"}]}`)
	}
	for _, h := range []http.HandlerFunc{ResponsesBridgeHandler(chat, opts), ResponsesBridgeCompactHandler(chat, opts)} {
		w := httptest.NewRecorder()
		h(w, httptest.NewRequest(http.MethodPost, "/qoder/v1/responses", strings.NewReader(`{"model":"m","input":[{"type":"compaction","encrypted_content":"bridge_compact_v1.missing"}]}`)))
		if w.Code != 503 || calls != 0 || !strings.Contains(w.Body.String(), "response_store_unavailable") {
			t.Fatalf("storage error=%d calls=%d %s", w.Code, calls, w.Body.String())
		}
	}
	write := httptest.NewRecorder()
	ResponsesBridgeCompactHandler(chat, opts)(write, httptest.NewRequest(http.MethodPost, "/qoder/v1/responses/compact", strings.NewReader(`{"model":"m","input":"hi"}`)))
	if write.Code != 503 || strings.Contains(write.Body.String(), bridgeCompactionPrefix) {
		t.Fatalf("stored failed compact: %d %s", write.Code, write.Body.String())
	}
}

func TestBridgeCompactionOwnerIsolationAndFailures(t *testing.T) {
	opts := BridgeOptions{Store: store.NewMemoryResponseStore(0)}
	calls := 0
	chat := func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"summary"},"finish_reason":"stop"}]}`)
	}
	call := func(token, body string, h http.HandlerFunc) *httptest.ResponseRecorder {
		wrapped := middleware.APIKeyAuthWithRequest(func(*http.Request) bool { return true }, func(context.Context, string) (*middleware.APIKeyPrincipal, error) {
			return &middleware.APIKeyPrincipal{}, nil
		}, h)
		r := httptest.NewRequest(http.MethodPost, "/qoder/v1/responses", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		wrapped(w, r)
		return w
	}
	created := call("owner-a", `{"model":"m","input":"hi"}`, ResponsesBridgeCompactHandler(chat, opts))
	if created.Code != 200 {
		t.Fatalf("compact=%d %s", created.Code, created.Body.String())
	}
	var response map[string]interface{}
	if err := json.Unmarshal(created.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]interface{}{"model": "m", "input": response["output"]})
	denied := call("owner-b", string(body), ResponsesBridgeHandler(chat, opts))
	if denied.Code != 400 || calls != 1 {
		t.Fatalf("owner isolation failed: %d calls=%d", denied.Code, calls)
	}
	allowed := call("owner-a", string(body), ResponsesBridgeHandler(chat, opts))
	if allowed.Code != 200 || calls != 2 {
		t.Fatalf("owner continuation failed: %d %s", allowed.Code, allowed.Body.String())
	}
	for _, reply := range []string{`{"choices":[{"message":{"content":"partial"},"finish_reason":"length"}]}`, `{"choices":[]}`, `{"error":{"message":"failed"}}`, `not-json`} {
		failed := call("owner-a", `{"model":"m","input":"hi"}`, ResponsesBridgeCompactHandler(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, reply) }, opts))
		if failed.Code != 502 || strings.Contains(failed.Body.String(), bridgeCompactionPrefix) {
			t.Fatalf("failed summary accepted: %d %s", failed.Code, failed.Body.String())
		}
	}
}

func TestBridgeRejectsNonportableHistory(t *testing.T) {
	for _, item := range []interface{}{map[string]interface{}{"type": "local_shell_call"}, map[string]interface{}{"type": "reasoning", "encrypted_content": "foreign-token"}, map[string]interface{}{"type": "function_call", "name": "f"}, "bad"} {
		_, err := InputToMessages([]interface{}{map[string]interface{}{"type": "message", "role": "user", "content": "hi"}, item})
		if err == nil {
			t.Fatalf("silently accepted history: %v", item)
		}
	}
}
