package handler

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"encoding/json"

	"github.com/alicebob/miniredis/v2"

	"orchids-api/internal/config"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

// planCooldownHandler builds the smallest live pool that can answer a Qoder
// request: one enabled account carrying a cooldown for the requested model, and a
// store that publishes that model.
func planCooldownHandler(t *testing.T, model string, cool func(acc *store.Account)) (*Handler, func() int) {
	t.Helper()
	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{RedisAddr: mini.Addr(), RedisDB: 0, RedisPrefix: "plancooldown:"})
	testutil.NoError(t, err, "store.New() error = %v")
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	acc := &store.Account{Name: "qoder-1", AccountType: "qoder", QoderModelIDs: []string{model}, QoderRefreshToken: "rt", Enabled: true, Weight: 1}
	testutil.NoError(t, s.CreateAccount(ctx, acc), "CreateAccount() error = %v")
	cool(acc)
	testutil.NoError(t, s.UpdateAccount(ctx, acc), "UpdateAccount() error = %v")

	publishModel(t, s, &store.Model{Channel: "Qoder", ModelID: model, BillingTier: "free", BillingSource: "qoder_price_factor"})

	h := NewWithLoadBalancer(&config.Config{DebugEnabled: false, RequestTimeout: 10, MaxRetries: 1}, loadbalancer.NewWithCacheTTL(s, 0))
	// The count has to be read through a closure: returning the value once
	// would freeze it at zero and make every "no upstream call" assertion pass
	// whatever the selection did afterwards.
	var calls atomic.Int64
	h.SetClientFactory(func(*store.Account, *config.Config) UpstreamClient {
		calls.Add(1)
		return &errorUpstreamEdge{err: errors.New("the request should never reach the upstream")}
	})
	t.Cleanup(h.Close)
	return h, func() int { return int(calls.Load()) }
}

func requestModel(t *testing.T, h *Handler, model string) *httptest.ResponseRecorder {
	t.Helper()
	payload := map[string]any{
		"model":    model,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
		"stream":   false,
	}
	body, _ := json.Marshal(payload)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://x/qoder/v1/chat/completions", bytes.NewReader(body))
	h.HandleMessages(rec, req)
	return rec
}

// TestPlanCooldownAnswersModelUnavailable is the regression test for the pool
// that answered a plan verdict with "the requested model is temporarily
// rate-limited".
//
// The cooldown was recorded by an entitlement refusal (Qoder business code 112:
// the account's plan does not cover this model) and held for a day. The selection
// layer could see the deadline but not the verdict behind it, so every request
// for that model was answered with an invitation to retry a condition that no
// amount of waiting changes.
func TestPlanCooldownAnswersModelUnavailable(t *testing.T) {
	h, upstreamCalls := planCooldownHandler(t, "glm-5.3", func(acc *store.Account) {
		store.RecordModelCooldownWithReason(acc, "glm-5.3", time.Now().Add(24*time.Hour), store.ModelCooldownUnavailable)
	})

	rec := requestModel(t, h, "glm-5.3")
	testutil.Equal(t, rec.Code, http.StatusNotFound)
	testutil.MustContain(t, rec.Body.String(), "not available on this channel's accounts")
	testutil.Equal(t, upstreamCalls(), 0)
}

// TestLegacyPlanCooldownIsReadFromItsDeadline covers the accounts that already
// carry a cooldown recorded before the reason was stored. The 27h deadline on
// production account 16 is exactly this shape, and the deadline is the only
// signal left: the two verdicts that create a model cooldown hold for 30 seconds
// and for a day, so a day-long hold cannot be a throttle.
func TestLegacyPlanCooldownIsReadFromItsDeadline(t *testing.T) {
	h, upstreamCalls := planCooldownHandler(t, "ultimate", func(acc *store.Account) {
		acc.ModelCooldowns = map[string]time.Time{"ultimate": time.Now().Add(27 * time.Hour)}
	})

	rec := requestModel(t, h, "ultimate")
	testutil.Equal(t, rec.Code, http.StatusNotFound)
	testutil.Equal(t, upstreamCalls(), 0)
}

// TestThrottledModelCooldownStaysRetryable guards the other direction: a
// short throttle must keep the retryable answer, or a model that is merely busy
// would be reported as permanently unavailable.
func TestThrottledModelCooldownStaysRetryable(t *testing.T) {
	h, upstreamCalls := planCooldownHandler(t, "efficient", func(acc *store.Account) {
		store.RecordModelCooldownWithReason(acc, "efficient", time.Now().Add(30*time.Second), store.ModelCooldownThrottled)
	})

	rec := requestModel(t, h, "efficient")
	testutil.Equal(t, rec.Code, http.StatusTooManyRequests)
	testutil.MustContain(t, rec.Body.String(), "cooling down")
	testutil.Equal(t, upstreamCalls(), 0)
}
