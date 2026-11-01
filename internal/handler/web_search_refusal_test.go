package handler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"orchids-api/internal/config"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

// TestWebSearchRequestIsRefusedBeforeTheUpstream: Claude Code's WebSearch
// request to WorkBuddy used to be lowered to a plain function; the model named a
// call nobody ran and the search silently returned nothing. It is now refused
// with a 400 that names the reason, before an account or upstream is spent.
func TestWebSearchRequestIsRefusedBeforeTheUpstream(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()
	s := newTestRedisStore(t, "web-search-refusal:")
	acc := &store.Account{AccountType: "workbuddy", Enabled: true, Weight: 1,
		WorkBuddyAccessToken: "test-access", WorkBuddyRefreshToken: "test-refresh",
		WorkBuddyModelIDs: []string{`{"id":"qwen3.8-flash"}`}}
	testutil.NoError(t, s.CreateAccount(t.Context(), acc))
	publishModel(t, s, &store.Model{Channel: "WorkBuddy", ModelID: "qwen3.8-flash"})
	h := NewWithLoadBalancer(&config.Config{WorkBuddyBaseURL: upstream.URL}, loadbalancer.NewWithCacheTTL(s, 0))
	defer h.Close()

	body := `{"model":"qwen3.8-flash","max_tokens":1024,"stream":true,
		"messages":[{"role":"user","content":"Perform a web search for the query: go release"}],
		"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":8}]}`
	req := httptest.NewRequest(http.MethodPost, "http://x/workbuddy/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.HandleMessages(rec, req)

	testutil.Equal(t, rec.Code, http.StatusBadRequest)
	raw, _ := io.ReadAll(rec.Body)
	testutil.MustContain(t, string(raw), "does not support web search")
	testutil.MustContain(t, string(raw), "/grok")
	testutil.Equal(t, calls.Load(), int32(0))
}
