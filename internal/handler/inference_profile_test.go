package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

type budgetFailureClient struct {
	accountID int64
	attempts  *[]int64
	failure   error
}

func (c *budgetFailureClient) SendRequestWithPayload(ctx context.Context, _ upstream.UpstreamRequest, _ func(upstream.SSEMessage), _ *debug.Logger) error {
	finish, err := upstream.BeginAttempt(ctx)
	if err != nil {
		return err
	}
	*c.attempts = append(*c.attempts, c.accountID)
	finish(c.failure)
	return c.failure
}

func TestInteractiveProfileStopsAfterThreeDifferentAccounts(t *testing.T) {
	h, s, _ := setupModelValidationHandler(t)
	cfg := &config.Config{InferenceProfile: "interactive"}
	config.ApplyHardcoded(cfg)
	cfg.RetryDelay = 0
	h.SetConfig(cfg)
	ctx := context.Background()
	for range 5 {
		testutil.NoError(t, s.CreateAccount(ctx, &store.Account{AccountType: "workbuddy", WorkBuddyAccessToken: "test-token", Enabled: true, Weight: 1}))
	}
	testutil.NoError(t, s.CreateModel(ctx, &store.Model{Channel: "WorkBuddy", ModelID: "test-model", Name: "test-model", Status: store.ModelStatusAvailable}))
	var attempts []int64
	h.SetClientFactory(func(acc *store.Account, _ *config.Config) UpstreamClient {
		return &budgetFailureClient{acc.ID, &attempts, errors.New("upstream HTTP 429: too many requests")}
	})
	rec := httptest.NewRecorder()
	h.HandleMessages(rec, httptest.NewRequest(http.MethodPost, "/workbuddy/v1/messages", strings.NewReader(`{"model":"test-model","messages":[{"role":"user","content":"hello"}],"stream":false}`)))
	testutil.Equal(t, len(attempts), 3)
	testutil.Equal(t, rec.Code, http.StatusTooManyRequests)
	seen := map[int64]bool{}
	for _, id := range attempts {
		seen[id] = true
	}
	testutil.Equal(t, len(seen), 3)
}

type budgetRetryHint struct{ wait time.Duration }

func (e budgetRetryHint) Error() string             { return "upstream HTTP 429: too many requests" }
func (e budgetRetryHint) RetryAfter() time.Duration { return e.wait }

func TestRetryAfterBeyondRequestDeadlineIsPreservedWithoutSleeping(t *testing.T) {
	h := NewWithLoadBalancer(&config.Config{MaxRetries: 3}, nil)
	var attempts []int64
	h.client = &budgetFailureClient{0, &attempts, budgetRetryHint{time.Minute}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rec := httptest.NewRecorder()
	h.HandleMessages(rec, httptest.NewRequest(http.MethodPost, "/workbuddy/v1/messages", strings.NewReader(`{"model":"test-model","messages":[{"role":"user","content":"hello"}],"stream":false}`)).WithContext(ctx))
	testutil.Equal(t, len(attempts), 1)
	testutil.Equal(t, rec.Code, http.StatusTooManyRequests)
	testutil.Equal(t, rec.Header().Get("Retry-After"), "60")
	testutil.Equal(t, ctx.Err(), error(nil))
}
