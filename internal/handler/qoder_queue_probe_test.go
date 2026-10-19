package handler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"orchids-api/internal/config"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

// TestQoderQueueRefusalMovesToAnIdleAccount covers a queue refusal (10605)
// that is not shared by the whole pool. The request used to wait it out on the
// refused account and answer 429 once the wait budget ran out, while another
// account sat idle. It now moves to an untried account first, and records
// nothing against the refused one.
func TestQoderQueueRefusalMovesToAnIdleAccount(t *testing.T) {
	var mu sync.Mutex
	var users []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		users = append(users, r.Header.Get("Cosy-User"))
		first := len(users) == 1
		mu.Unlock()
		if first {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"code":"10605","message":"queue"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data:{"body":"{\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}","statusCodeValue":200}`+"\n\nevent:finish\ndata:{}\n\n")
	}))
	defer upstream.Close()

	s := newTestRedisStore(t, "qoder-queue-probe:")
	var accounts []*store.Account
	for _, user := range []string{"user-a", "user-b"} {
		acc := &store.Account{AccountType: "qoder", Enabled: true, Weight: 1,
			QoderUserID: user, QoderMachineID: "machine-" + user,
			QoderAccessToken: "access-" + user, QoderRefreshToken: "refresh-" + user,
			QoderModelIDs: []string{`{"key":"qfmodel","name":"Qwen3.8-Flash"}`}}
		testutil.NoError(t, s.CreateAccount(t.Context(), acc))
		accounts = append(accounts, acc)
	}
	publishModel(t, s, &store.Model{Channel: "Qoder", ModelID: "qwen3.8-flash"})
	h := NewWithLoadBalancer(&config.Config{QoderInferenceURL: upstream.URL, MaxRetries: 3,
		RetryDelay: 1, SharedRefusalWaitBudgetMs: 100}, loadbalancer.NewWithCacheTTL(s, 0))
	defer h.Close()

	req := httptest.NewRequest(http.MethodPost, "http://x/qoder/v1/messages", strings.NewReader(
		`{"model":"qwen3.8-flash","max_tokens":16,"messages":[{"role":"user","content":"OK"}],"stream":false}`))
	rec := httptest.NewRecorder()
	h.HandleMessages(rec, req)

	testutil.Equal(t, rec.Code, http.StatusOK)
	mu.Lock()
	defer mu.Unlock()
	testutil.Equal(t, len(users), 2)
	testutil.Falsef(t, users[0] == users[1], "the retry stayed on the refused account: %v", users)
	for _, acc := range accounts {
		stored, err := s.GetAccount(t.Context(), acc.ID)
		testutil.NoError(t, err)
		testutil.Falsef(t, stored.StatusCode != "" || len(stored.ModelCooldowns) > 0,
			"queue refusal parked account %s: status=%q cooldowns=%v", acc.QoderUserID, stored.StatusCode, stored.ModelCooldowns)
	}
}

// TestQoderSharedQueueRefusalStillEndsInOne429 covers the gate the wait-in-place
// policy was written for: every account is refused. Each account is tried once,
// none is parked, and the caller gets the upstream's 429 with its hint.
func TestQoderSharedQueueRefusalStillEndsInOne429(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"code":"10605","message":"queue"}`)
	}))
	defer upstream.Close()

	s := newTestRedisStore(t, "qoder-queue-shared:")
	var accounts []*store.Account
	for _, user := range []string{"user-a", "user-b", "user-c"} {
		acc := &store.Account{AccountType: "qoder", Enabled: true, Weight: 1,
			QoderUserID: user, QoderMachineID: "machine-" + user,
			QoderAccessToken: "access-" + user, QoderRefreshToken: "refresh-" + user,
			QoderModelIDs: []string{`{"key":"qfmodel","name":"Qwen3.8-Flash"}`}}
		testutil.NoError(t, s.CreateAccount(t.Context(), acc))
		accounts = append(accounts, acc)
	}
	publishModel(t, s, &store.Model{Channel: "Qoder", ModelID: "qwen3.8-flash"})
	h := NewWithLoadBalancer(&config.Config{QoderInferenceURL: upstream.URL, MaxRetries: 10,
		RetryDelay: 1, SharedRefusalWaitBudgetMs: 100}, loadbalancer.NewWithCacheTTL(s, 0))
	defer h.Close()

	req := httptest.NewRequest(http.MethodPost, "http://x/qoder/v1/messages", strings.NewReader(
		`{"model":"qwen3.8-flash","max_tokens":16,"messages":[{"role":"user","content":"OK"}],"stream":false}`))
	rec := httptest.NewRecorder()
	h.HandleMessages(rec, req)

	testutil.Equal(t, rec.Code, http.StatusTooManyRequests)
	testutil.Equal(t, rec.Header().Get("Retry-After"), "120")
	mu.Lock()
	testutil.Equal(t, calls, len(accounts))
	mu.Unlock()
	for _, acc := range accounts {
		stored, err := s.GetAccount(t.Context(), acc.ID)
		testutil.NoError(t, err)
		testutil.Falsef(t, stored.StatusCode != "" || len(stored.ModelCooldowns) > 0,
			"shared queue refusal parked account %s", acc.QoderUserID)
	}
}
