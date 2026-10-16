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

func TestCLISelectionSkipsSharedCooldownBeforeAttempt(t *testing.T) {
	for _, scope := range []RateLimitScope{RateLimitScopeRPM, RateLimitScopeRPS} {
		for _, target := range []string{"grok-4.7", "*"} {
			t.Run(string(scope)+"/"+target, func(t *testing.T) {
				server := httptest.NewServer(http.NotFoundHandler())
				defer server.Close()
				h, base := parityBuildHandler(t, server)
				ctx := withGrokSession(context.Background(), grokSessionContext{Key: t.Name(), Model: "grok-4.7"})
				var accounts []*store.Account
				for i := 0; i < 4; i++ {
					acc := *base
					acc.ID = 0
					acc.TeamID = t.Name() + "-limited"
					if i == 3 {
						acc.TeamID = t.Name() + "-healthy"
					}
					acc.GrokModels, acc.GrokModelsSyncedAt = []string{"grok-4.7", "grok-4.6"}, time.Now()
					if err := h.lb.Store.CreateAccount(ctx, &acc); err != nil {
						t.Fatal(err)
					}
					accounts = append(accounts, &acc)
				}
				teamCooldown.Note(scope, ProviderBuild+":team:"+accounts[0].TeamID, target, time.Minute)
				for _, acc := range accounts[:3] {
					// An existing session must not return to a sibling whose status
					// is still healthy while its team is already throttled.
					h.bindAffinity(ctx, ProviderBuild, acc.ID)
					sess, err := h.openCLIAccountSession(ctx, nil, "grok-4.7")
					if err != nil {
						t.Fatal(err)
					}
					sess.Close()
					if sess.acc.ID != accounts[3].ID {
						t.Fatalf("selected cooling sibling %d instead of healthy account %d", sess.acc.ID, accounts[3].ID)
					}
					if _, err := h.openCLIAccountSessionByID(ctx, acc.ID, "grok-4.7"); err == nil {
						t.Fatal("pinned continuation bypassed team cooldown")
					}
				}
				if target != "*" {
					sess, err := h.openCLIAccountSessionByID(ctx, accounts[0].ID, "grok-4.6")
					if err != nil {
						t.Fatalf("cooldown blocked unrelated model: %v", err)
					}
					sess.Close()
				}
				// When only the cooling team remains, report a throttle rather
				// than a missing model or an empty pool.
				_, err := h.openCLIAccountSession(ctx, []int64{accounts[3].ID}, "grok-4.7")
				if err == nil || classifyGrokPoolFailure(err, "", "").status != http.StatusTooManyRequests {
					t.Fatalf("expected throttled pool, got %v", err)
				}
			})
		}
	}
}

func TestCLISelectionUsesAccountIdentityWithoutTeam(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	h, acc := parityBuildHandler(t, server)
	acc.ID, acc.TeamID = 0, ""
	acc.GrokModels, acc.GrokModelsSyncedAt = []string{"grok-4.7"}, time.Now()
	ctx := context.Background()
	if err := h.lb.Store.CreateAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}
	identity := fmt.Sprintf("%s:account:%d", ProviderBuild, acc.ID)
	key := teamCooldownKey(RateLimitScopeRPM, identity, "grok-4.7")
	teamCooldown.mu.Lock()
	previous, existed := teamCooldown.entries[key]
	teamCooldown.mu.Unlock()
	t.Cleanup(func() {
		teamCooldown.mu.Lock()
		defer teamCooldown.mu.Unlock()
		if existed {
			teamCooldown.entries[key] = previous
		} else {
			delete(teamCooldown.entries, key)
		}
	})
	teamCooldown.Note(RateLimitScopeRPM, identity, "grok-4.7", time.Minute)
	_, err := h.openCLIAccountSession(ctx, nil, "grok-4.7")
	if err == nil || classifyGrokPoolFailure(err, "", "").status != http.StatusTooManyRequests {
		t.Fatalf("expected account cooldown to be honored, got %v", err)
	}
}
