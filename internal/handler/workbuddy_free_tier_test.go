package handler

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"encoding/json"

	"orchids-api/internal/config"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

// workbuddySpentError reproduces the production refusal for a spent balance:
// business code 14018 under a 429, with the upstream's retry hint attached. The
// hint is what made the generic retry-after branch outrank the credit-exhaustion
// verdict in production while the plain-error unit tests stayed green.
type workbuddySpentError struct{}

func (workbuddySpentError) Error() string {
	return `workbuddy API error: status=429, message={"error":{"data":{"code":14018,` +
		`"msg":"Credits exhausted. Please visit the link below to purchase add-on packs"}}}`
}

// A millisecond keeps the retry wait out of the test's runtime; the hint's value
// is covered in internal/accountpolicy.
func (workbuddySpentError) RetryAfter() time.Duration { return time.Millisecond }

// TestSpentAccountKeepsFreeOnlyStateAndScopesTheRefusedModel is the regression
// test for what production does with an account whose WorkBuddy balance is gone.
//
// The account is kept in the pool for its confirmed free models. When the upstream
// refuses that tier too, the capability state used to be replaced by a plain 429 —
// which is re-admitted as fully capable once its cooldown elapses, so metered
// requests reached an account with nothing to spend — and every later request paid
// for the same refusal before switching accounts.
func TestSpentAccountKeepsFreeOnlyStateAndScopesTheRefusedModel(t *testing.T) {
	s := newTestRedisStore(t, "wbfreetier:")

	ctx := context.Background()
	reset := time.Now().Add(12 * time.Hour)
	acc := &store.Account{
		Name:                  "spent@uq.edu.rs",
		AccountType:           "workbuddy",
		WorkBuddyRefreshToken: "rt",
		Enabled:               true,
		Weight:                1,
		StatusCode:            store.AccountStatusWorkBuddyQuotaExhausted,
		WorkBuddyModelIDs:     []string{`{"id":"hy3","credits":"x0.00"}`, `{"id":"glm-5.3"}`},
		WorkBuddyQuota:        store.WorkBuddyQuotaSnapshot{Remaining: 0, Limit: 100, ResetAt: reset},
	}
	testutil.NoError(t, s.CreateAccount(ctx, acc), "CreateAccount() error = %v")
	publishModel(t, s, &store.Model{Channel: "WorkBuddy", ModelID: "hy3"})
	publishModel(t, s, &store.Model{Channel: "WorkBuddy", ModelID: "glm-5.3"})

	h := NewWithLoadBalancer(&config.Config{DebugEnabled: false, RequestTimeout: 10, MaxRetries: 0}, loadbalancer.NewWithCacheTTL(s, 0))
	upstreamCalls := 0
	h.SetClientFactory(func(*store.Account, *config.Config) UpstreamClient {
		upstreamCalls++
		return &errorUpstreamEdge{err: workbuddySpentError{}}
	})

	// The free model is dispatched — that is the whole point of keeping a spent
	// account in the pool — and the upstream refuses it anyway.
	first := workbuddyRequest(t, h, "hy3")
	testutil.Equal(t, upstreamCalls, 1)
	_ = first

	after, err := s.GetAccount(ctx, acc.ID)
	testutil.NoError(t, err, "GetAccount() error = %v")
	testutil.Equal(t, after.StatusCode, store.AccountStatusWorkBuddyQuotaExhausted)
	testutil.Equal(t, store.ModelCooldownKind(after, "hy3", time.Now()), store.ModelCooldownUnavailable)
	if remaining := store.ModelCooldownRemaining(after, "hy3", time.Now()); remaining <= 0 {
		t.Fatal("the refused free model must carry a cooldown")
	} else if remaining > 6*time.Hour+time.Minute {
		// The plan reset is 12h away, but the hold is capped: the reset comes from
		// the upstream's wall clock, whose zone the gateway cannot verify, so a
		// misread boundary must not park the model for hours longer than the
		// refusal deserves.
		t.Fatalf("hy3 remaining = %v, want a hold of at most 6h", remaining)
	}

	// The next request for that model never reaches the upstream.
	second := workbuddyRequest(t, h, "hy3")
	testutil.Equal(t, upstreamCalls, 1)
	testutil.Equal(t, second.Code, http.StatusNotFound)

	// And a metered model was never eligible for a spent account at all.
	workbuddyRequest(t, h, "glm-5.3")
	testutil.Equal(t, upstreamCalls, 1)
}

func workbuddyRequest(t *testing.T, h *Handler, model string) *httptest.ResponseRecorder {
	t.Helper()
	payload := map[string]any{
		"model":    model,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
		"stream":   false,
	}
	body, _ := json.Marshal(payload)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://x/workbuddy/v1/chat/completions", bytes.NewReader(body))
	h.HandleMessages(rec, req)
	return rec
}

// The other direction: a plain throttle on a healthy WorkBuddy account keeps
// serving free models, so this rule cannot strand the free tier generally.
func TestHealthyAccountStillServesFreeModels(t *testing.T) {
	s := newTestRedisStore(t, "wbhealthy:")

	ctx := context.Background()
	acc := &store.Account{
		Name:                  "healthy@uq.edu.rs",
		AccountType:           "workbuddy",
		WorkBuddyRefreshToken: "rt",
		Enabled:               true,
		Weight:                1,
		WorkBuddyModelIDs:     []string{`{"id":"hy3","credits":"x0.00"}`},
	}
	testutil.NoError(t, s.CreateAccount(ctx, acc), "CreateAccount() error = %v")
	publishModel(t, s, &store.Model{Channel: "WorkBuddy", ModelID: "hy3"})

	h := NewWithLoadBalancer(&config.Config{DebugEnabled: false, RequestTimeout: 10, MaxRetries: 0}, loadbalancer.NewWithCacheTTL(s, 0))
	h.SetClientFactory(func(*store.Account, *config.Config) UpstreamClient {
		return &mockUpstreamEdge{events: []upstream.SSEMessage{
			{Type: "model.text-delta", Event: map[string]any{"delta": "ok"}},
			{Type: "model.finish", Event: map[string]any{"finishReason": "stop"}},
		}}
	})

	rec := workbuddyRequest(t, h, "hy3")
	testutil.Equal(t, rec.Code, http.StatusOK)
	after, err := s.GetAccount(ctx, acc.ID)
	testutil.NoError(t, err, "GetAccount() error = %v")
	testutil.Equal(t, store.ModelCooldownKind(after, "hy3", time.Now()), "")
	testutil.MustContain(t, rec.Body.String(), "ok")
}
