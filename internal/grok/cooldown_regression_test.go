package grok

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"orchids-api/internal/store"
)

func TestFreeQuotaCooldownRequiresExplicitRejection(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		wantHold bool
	}{
		{"transient outage", 503, `{"code":"service_unavailable","error":"Free usage for model grok-4.7 is temporarily unavailable; retry shortly."}`, false},
		{"permission refusal", 403, `{"code":"permission_denied","error":"Free usage for model grok-4.7 is unavailable on this route."}`, false},
		{"generic throttle", 429, `{"code":"rate_limited","error":"Free usage for model grok-4.7 is temporarily busy."}`, false},
		{"message mentions exhaustion code", 429, `{"error":"subscription:free-usage-exhausted; free usage for model grok-4.7"}`, false},
		{"wrong status", 503, freeUsageRejectionFixture, false},
		{"explicit quota", 429, freeUsageRejectionFixture, true},
		{"explicit payment refusal", 402, freeUsageRejectionFixture, true},
		{"nested quota refusal", 403, `{"error":{"code":"subscription:free-usage-exhausted","message":"You've used all the included free usage for model grok-4.7."}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "2")
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(w, tc.body)
			}))
			defer upstream.Close()
			h, acc := parityBuildHandler(t, upstream)
			acc.ID, acc.TeamID = 0, t.Name()
			acc.GrokModels, acc.GrokModelsSyncedAt = []string{"grok-4.7", "grok-4.6"}, time.Now()
			ctx := context.Background()
			if err := h.lb.Store.CreateAccount(ctx, acc); err != nil {
				t.Fatal(err)
			}
			_, err := h.buildClient().doResponsesAt(ctx, acc, "/responses", map[string]any{"model": "grok-4.7", "input": "hello"})
			if upstreamStatus(err) != tc.status {
				t.Fatalf("unexpected error: %v", err)
			}
			saved, err := h.lb.Store.GetAccount(ctx, acc.ID)
			if err != nil {
				t.Fatal(err)
			}
			hold := store.ModelCooldownRemaining(saved, "grok-4.7", time.Now())
			if tc.wantHold && hold < 23*time.Hour {
				t.Fatalf("explicit refusal not retained: %v", hold)
			}
			if !tc.wantHold && hold > time.Minute {
				t.Fatalf("non-exhaustion error created long cooldown: %v", hold)
			}
			if !saved.GrokFreeQuota.ResetAt.IsZero() || store.ModelCooldownRemaining(saved, "grok-4.6", time.Now()) > 0 {
				t.Fatal("model refusal poisoned unrelated models")
			}
		})
	}
}
