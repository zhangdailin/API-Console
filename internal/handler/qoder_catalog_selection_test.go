package handler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/config"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func TestQoderSelectionSkipsUnsupportedPreferredAccount(t *testing.T) {
	s := newTestRedisStore(t, "qoder-catalog-selection:")
	free := &store.Account{AccountType: "qoder", Enabled: true, Weight: 1,
		QoderAccessToken: "test-access", QoderRefreshToken: "test-refresh",
		QoderModelIDs: []string{`{"key":"qfmodel","name":"Qwen3.8-Flash"}`}}
	paid := &store.Account{AccountType: "qoder", Enabled: true, Weight: 1,
		QoderAccessToken: "other-access", QoderRefreshToken: "other-refresh",
		QoderModelIDs: []string{`{"key":"qmodel_38max","name":"Qwen3.8-Max"}`}}
	testutil.NoError(t, s.CreateAccount(t.Context(), free))
	testutil.NoError(t, s.CreateAccount(t.Context(), paid))
	h := NewWithLoadBalancer(&config.Config{RequestTimeout: 10}, loadbalancer.NewWithCacheTTL(s, 0))
	defer h.Close()
	selected, err := h.selectAccountRecordWithOptions(t.Context(), "qoder", nil, accountSelectionOptions{
		ModelID: "qwen3.8-max", PreferredAccountID: free.ID,
	})
	testutil.NoError(t, err)
	testutil.Equal(t, selected.ID, paid.ID)

	// A missing snapshot also cannot resolve the requested model locally.
	free.QoderModelIDs = nil
	testutil.NoError(t, s.UpdateAccount(t.Context(), free))
	selected, err = h.selectAccountRecordWithOptions(t.Context(), "qoder", nil, accountSelectionOptions{
		ModelID: "qwen3.8-max", PreferredAccountID: free.ID,
	})
	testutil.NoError(t, err)
	testutil.Equal(t, selected.ID, paid.ID)
}

func TestQoderLongRetryHintReturnsWithoutEarlyReplay(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"code":"10605","message":"queue"}`)
	}))
	defer upstream.Close()
	s := newTestRedisStore(t, "qoder-long-retry:")
	acc := &store.Account{AccountType: "qoder", Enabled: true, Weight: 1,
		QoderUserID: "test-user", QoderMachineID: "test-machine",
		QoderAccessToken: "test-access", QoderRefreshToken: "test-refresh",
		QoderModelIDs: []string{`{"key":"qfmodel","name":"Qwen3.8-Flash"}`}}
	testutil.NoError(t, s.CreateAccount(t.Context(), acc))
	publishModel(t, s, &store.Model{Channel: "Qoder", ModelID: "qwen3.8-flash"})
	h := NewWithLoadBalancer(&config.Config{QoderInferenceURL: upstream.URL, MaxRetries: 3,
		RetryDelay: 1, SharedRefusalWaitBudgetMs: 100}, loadbalancer.NewWithCacheTTL(s, 0))
	defer h.Close()
	req := httptest.NewRequest(http.MethodPost, "http://x/qoder/v1/messages", strings.NewReader(
		`{"model":"qwen3.8-flash","max_tokens":16,"messages":[{"role":"user","content":"OK"}],"stream":false}`))
	rec := httptest.NewRecorder()
	h.HandleMessages(rec, req)
	testutil.Equal(t, calls, 1)
	testutil.Equal(t, rec.Code, http.StatusTooManyRequests)
	testutil.Equal(t, rec.Header().Get("Retry-After"), "120")
}

func TestQoderUnsupportedPoolDoesNotAdvertiseCooldown(t *testing.T) {
	s := newTestRedisStore(t, "qoder-unsupported-pool:")
	acc := &store.Account{AccountType: "qoder", Enabled: true, Weight: 1,
		QoderAccessToken: "test-access", QoderModelIDs: []string{"qwen3.8-flash"}}
	testutil.NoError(t, s.CreateAccount(t.Context(), acc))
	publishModel(t, s, &store.Model{Channel: "Qoder", ModelID: "qwen3.8-max"})
	h := NewWithLoadBalancer(&config.Config{RequestTimeout: 10}, loadbalancer.NewWithCacheTTL(s, 0))
	defer h.Close()
	req := httptest.NewRequest(http.MethodPost, "/qoder/v1/messages", strings.NewReader(
		`{"model":"qwen3.8-max","max_tokens":16,"messages":[{"role":"user","content":"OK"}]}`))
	rec := httptest.NewRecorder()
	h.HandleMessages(rec, req)
	testutil.Equal(t, rec.Code, http.StatusNotFound)
	testutil.MustContain(t, rec.Body.String(), "not available on this channel's accounts")
	testutil.Equal(t, rec.Header().Get("Retry-After"), "")
}
