package errors

import (
	"errors"
	"net/http"
	"orchids-api/internal/testutil"
	"strings"
	"testing"
)

// The pool's note says why the channel cannot serve a request, and each reason
// needs its own answer. These are the production shapes: the first is the
// WorkBuddy pool that reached its caller as "no enabled accounts available for
// channel: workbuddy" with a 503, after every account that could serve the
// requested model had gone into a per-model cooldown.
func TestClassifyPoolExhaustion_SelectorReasonsPickTheAnswer(t *testing.T) {
	cases := []struct {
		name         string
		selectErr    error
		lastErr      string
		wantCategory string
		wantStatus   int
		wantMessage  string
	}{
		{
			name:         "every account is cooling down for the requested model",
			selectErr:    errors.New("no enabled accounts available for channel: workbuddy (all matching accounts are cooling down for the requested model)"),
			wantCategory: "rate_limit",
			wantStatus:   http.StatusTooManyRequests,
			wantMessage:  "the requested model is cooling down on this channel",
		},
		{
			name:         "the whole pool is rate limited",
			selectErr:    errors.New("no enabled accounts available for channel: workbuddy (all matching accounts are rate-limited or cooling down)"),
			wantCategory: "rate_limit",
			wantStatus:   http.StatusTooManyRequests,
			wantMessage:  "all available accounts for this channel are currently rate-limited",
		},
		{
			name:         "every account has spent its allowance",
			selectErr:    errors.New("no enabled accounts available for channel: workbuddy (all matching accounts have exhausted their allowance)"),
			wantCategory: "quota_exhausted",
			wantStatus:   http.StatusTooManyRequests,
			wantMessage:  "has exhausted its allowance",
		},
		{
			// The 2026-09-21 WorkBuddy outage shape: three accounts cooling down
			// from a 429 and four parked for a spent allowance at once. The old
			// selector reported this as the bare sentence, which classified to
			// nothing, so the caller got a 503 "server fault" instead of a
			// retryable 429.
			name:         "a pool split between rate limits and a spent allowance stays retryable",
			selectErr:    errors.New("no enabled accounts available for channel: workbuddy (all matching accounts are rate-limited or cooling down: 3 rate-limited, 4 parked for a spent allowance)"),
			wantCategory: "rate_limit",
			wantStatus:   http.StatusTooManyRequests,
			wantMessage:  "currently rate-limited",
		},
		{
			name:         "every account is busy with other requests",
			selectErr:    errors.New("no enabled accounts available for channel: workbuddy (all matching accounts are at their concurrency limit)"),
			wantCategory: "rate_limit",
			wantStatus:   http.StatusTooManyRequests,
			wantMessage:  "busy with other requests",
		},
		{
			name:         "the pool cannot route the requested model",
			selectErr:    errors.New("no enabled accounts available for channel: workbuddy (model gpt-5-6-sol-low is not available in the current workbuddy account pool)"),
			wantCategory: "model_unavailable",
			wantStatus:   http.StatusNotFound,
			wantMessage:  "is not available on this channel's accounts",
		},
		{
			name:         "the upstream error carries the cause",
			selectErr:    errors.New("no enabled accounts available for channel: workbuddy"),
			lastErr:      `upstream API error: status=429, body={"code":"rate-limited"}`,
			wantCategory: "rate_limit",
			wantStatus:   http.StatusTooManyRequests,
			wantMessage:  "currently rate-limited",
		},
		{
			name:         "an exhausted allowance in the upstream error wins over a busy pool",
			selectErr:    errors.New("no enabled accounts available for channel: workbuddy (all matching accounts are cooling down for the requested model)"),
			lastErr:      `workbuddy API error: status=429, message={"error":{"data":{"code":14018,"msg":"Credits exhausted. Please visit the link below to purchase add-on packs"}}}`,
			wantCategory: "quota_exhausted",
			wantStatus:   http.StatusTooManyRequests,
			wantMessage:  "has exhausted its allowance",
		},
		{
			name:       "a failure that names no capacity cause stays for the caller",
			selectErr:  errors.New("grok cli account token is empty"),
			wantStatus: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := ClassifyPoolExhaustion(tc.selectErr, tc.lastErr)
			if tc.wantCategory == "" {
				testutil.Falsef(t, !out.Empty(), "expected no classification, got %+v", out)
				return
			}
			testutil.Equal(t, out.Category, tc.wantCategory)
			testutil.Equal(t, StatusForCategory(out.Category), tc.wantStatus)
			testutil.MustContain(t, out.Message, tc.wantMessage)
		})
	}
}

// TestPoolExhaustionMessagesNeverNamePoolInternals pins the contract every
// entrance relies on: the client-facing text says what happened and what to do,
// and never repeats the selector's "no enabled accounts available for channel"
// note, which names internal state and reads like the channel is unconfigured.
func TestPoolExhaustionMessagesNeverNamePoolInternals(t *testing.T) {
	for _, message := range []string{
		PoolAllowanceMessage,
		PoolModelCooldownMessage,
		PoolRateLimitedMessage,
		PoolBusyMessage,
		PoolModelUnavailableMessage,
		PoolNoAccountsMessage,
		PoolRetriesExhaustedMessage,
	} {
		for _, leaked := range []string{
			"no enabled accounts available",
			"channel:",
			"enabled=false",
			"account_type",
		} {
			testutil.CheckNotContain(t, strings.ToLower(message), leaked)
		}
	}
}
