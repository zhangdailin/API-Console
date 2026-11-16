package warp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"orchids-api/internal/middleware"
	"orchids-api/internal/store"
)

func toolClaimContext(t *testing.T, id int64, key string) context.Context {
	t.Helper()
	var ctx context.Context
	handler := middleware.APIKeyAuth(func(context.Context, string) (*middleware.APIKeyPrincipal, error) {
		return &middleware.APIKeyPrincipal{ID: id}, nil
	}, func(_ http.ResponseWriter, r *http.Request) { ctx = r.Context() })
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer "+key)
	handler(httptest.NewRecorder(), r)
	if ctx == nil {
		t.Fatal("synthetic API key authentication failed")
	}
	return ctx
}

func toolClaimRedisStore(t *testing.T, mini *miniredis.Miniredis) *bindingStore {
	t.Helper()
	const prefix = "warp-tool-claims-test:"
	s, err := store.New(store.Options{
		RedisAddr: mini.Addr(), RedisPrefix: prefix,
		CredentialEncryptionKey: []byte("01234567890123456789012345678901"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	b := newBindingStore()
	b.redis, b.keyBase = redisBindings{store: s}, prefix
	return b
}

func toolClaimBackends(t *testing.T, test func(*testing.T, *bindingStore)) {
	t.Helper()
	t.Run("local", func(t *testing.T) { test(t, newBindingStore()) })
	t.Run("redis", func(t *testing.T) { test(t, toolClaimRedisStore(t, miniredis.RunT(t))) })
}

func TestToolClaimsConsumeIDOnce(t *testing.T) {
	toolClaimBackends(t, func(t *testing.T, b *bindingStore) {
		ctx := toolClaimContext(t, 1, "synthetic-tool-owner")
		binding := callBinding{ConversationID: "conversation"}
		if err := b.claimToolTurn(ctx, 7, "model-a", []string{"call-1"}, binding); err != nil {
			t.Fatalf("first claim failed: %v", err)
		}
		if err := b.claimToolTurn(ctx, 7, "model-a", []string{"call-1"}, binding); err == nil {
			t.Fatal("consumed tool result was claimed twice")
		}
		binding.ConversationID = "other-conversation"
		if err := b.claimToolTurn(ctx, 7, "model-a", []string{"call-1"}, binding); err == nil {
			t.Fatal("changing the conversation allowed replay of a consumed ID")
		}
	})
}

func TestToolClaimsConcurrentDuplicateAcrossRedisClients(t *testing.T) {
	mini := miniredis.RunT(t)
	clients := []*bindingStore{toolClaimRedisStore(t, mini), toolClaimRedisStore(t, mini)}
	ctx := toolClaimContext(t, 1, "synthetic-tool-owner")
	const contenders = 32
	start := make(chan struct{})
	results := make(chan error, contenders)
	var wg sync.WaitGroup
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(b *bindingStore) {
			defer wg.Done()
			<-start
			results <- b.claimToolTurn(ctx, 7, "model-a", []string{"same-call"}, callBinding{ConversationID: "conversation"})
		}(clients[i%len(clients)])
	}
	close(start)
	wg.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("duplicate claims had %d winners, want exactly one", winners)
	}
}

func TestToolClaimsConcurrentConversationBudgetAcrossRedisClients(t *testing.T) {
	mini := miniredis.RunT(t)
	clients := []*bindingStore{toolClaimRedisStore(t, mini), toolClaimRedisStore(t, mini)}
	ctx := toolClaimContext(t, 1, "synthetic-tool-owner")
	const contenders = 24
	start := make(chan struct{})
	results := make(chan error, contenders)
	var wg sync.WaitGroup
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			results <- clients[index%len(clients)].claimToolTurn(ctx, 7, "model-a", []string{fmt.Sprintf("call-%d", index)}, callBinding{ConversationID: "conversation"})
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
		}
	}
	if winners != toolRoundLimit {
		t.Fatalf("concurrent conversation claims had %d winners, want %d", winners, toolRoundLimit)
	}
}

func TestToolClaimsRedisSurviveBindingStoreRestart(t *testing.T) {
	mini := miniredis.RunT(t)
	ctx := toolClaimContext(t, 1, "synthetic-tool-owner")
	first := toolClaimRedisStore(t, mini)
	binding := callBinding{ConversationID: "conversation"}
	if err := first.claimToolTurn(ctx, 7, "model-a", []string{"call-1"}, binding); err != nil {
		t.Fatal(err)
	}
	if err := first.redis.(redisBindings).store.Close(); err != nil {
		t.Fatal(err)
	}
	restarted := toolClaimRedisStore(t, mini)
	if err := restarted.claimToolTurn(ctx, 7, "model-a", []string{"call-1"}, binding); err == nil {
		t.Fatal("a fresh binding store replayed a persisted claim")
	}
	if err := restarted.claimToolTurn(ctx, 7, "model-a", []string{"call-2"}, binding); err != nil {
		t.Fatalf("restart incorrectly blocked an unused result: %v", err)
	}
}

func TestToolClaimsOwnershipAccountAndModelIsolation(t *testing.T) {
	toolClaimBackends(t, func(t *testing.T, b *bindingStore) {
		owner := toolClaimContext(t, 1, "synthetic-tool-owner")
		other := toolClaimContext(t, 2, "synthetic-tool-owner")
		rotated := toolClaimContext(t, 1, "synthetic-tool-rotated")
		for _, scope := range []struct {
			name    string
			ctx     context.Context
			account int64
			model   string
		}{
			{"original", owner, 7, "model-a"},
			{"other API key ID", other, 7, "model-a"},
			{"rotated API key", rotated, 7, "model-a"},
			{"other account", owner, 8, "model-a"},
			{"other model", owner, 7, "model-b"},
		} {
			binding := callBinding{ConversationID: "same-conversation"}
			// Exhaust each scope so both consumed IDs and conversation counters
			// must be isolated, not just the first claim.
			for round := 0; round < toolRoundLimit; round++ {
				binding.ToolRounds = round
				ids := []string{fmt.Sprintf("same-call-%d", round)}
				if err := b.claimToolTurn(scope.ctx, scope.account, scope.model, ids, binding); err != nil {
					t.Fatalf("%s round %d collided with another scope: %v", scope.name, round, err)
				}
			}
			binding.ToolRounds = toolRoundLimit
			if err := b.claimToolTurn(scope.ctx, scope.account, scope.model, []string{"beyond-limit"}, binding); err == nil {
				t.Fatalf("%s exceeded the fixed tool-turn limit", scope.name)
			}
		}
	})
}

func TestToolClaimsExpiry(t *testing.T) {
	ctx := toolClaimContext(t, 1, "synthetic-tool-owner")
	binding := callBinding{ConversationID: "conversation"}
	t.Run("local", func(t *testing.T) {
		b := newBindingStore()
		if err := b.claimToolTurn(ctx, 7, "model-a", []string{"call-1"}, binding); err != nil {
			t.Fatal(err)
		}
		b.mu.Lock()
		for key := range b.claims {
			b.claims[key] = time.Now().Add(-time.Second)
		}
		b.mu.Unlock()
		if err := b.claimToolTurn(ctx, 7, "model-a", []string{"call-1"}, binding); err != nil {
			t.Fatalf("expired local claim was not reclaimed: %v", err)
		}
	})
	t.Run("redis", func(t *testing.T) {
		mini := miniredis.RunT(t)
		b := toolClaimRedisStore(t, mini)
		for round := 0; round < toolRoundLimit; round++ {
			if err := b.claimToolTurn(ctx, 7, "model-a", []string{fmt.Sprintf("call-%d", round)}, binding); err != nil {
				t.Fatal(err)
			}
		}
		for _, key := range mini.Keys() {
			if ttl := mini.TTL(key); ttl != bindingTTL {
				t.Fatalf("claim state TTL = %v, want %v", ttl, bindingTTL)
			}
		}
		mini.FastForward(bindingTTL - time.Second)
		if err := b.claimToolTurn(ctx, 7, "model-a", []string{"call-0"}, binding); err == nil {
			t.Fatal("claim expired before its TTL")
		}
		mini.FastForward(2 * time.Second)
		if keys := mini.Keys(); len(keys) != 0 {
			t.Fatalf("expired claim state retained %d keys", len(keys))
		}
		if err := b.claimToolTurn(ctx, 7, "model-a", []string{"call-0"}, binding); err != nil {
			t.Fatalf("expired claim or conversation counter blocked a fresh claim: %v", err)
		}
	})
}

func TestToolClaimsRejectBindingAtOrAboveLimit(t *testing.T) {
	toolClaimBackends(t, func(t *testing.T, b *bindingStore) {
		ctx := toolClaimContext(t, 1, "synthetic-tool-owner")
		for _, rounds := range []int{toolRoundLimit, toolRoundLimit + 1} {
			binding := callBinding{ConversationID: "conversation", ToolRounds: rounds}
			if err := b.claimToolTurn(ctx, 7, "model-a", []string{"call-1"}, binding); err == nil {
				t.Fatalf("binding with %d rounds bypassed the limit", rounds)
			}
		}
		if err := b.claimToolTurn(ctx, 7, "model-a", []string{"call-1"}, callBinding{ConversationID: "conversation"}); err != nil {
			t.Fatalf("rejected over-limit binding consumed the result: %v", err)
		}
	})
}

func TestToolClaimsLocalScopedBindingRoundLimit(t *testing.T) {
	ctx := toolClaimContext(t, 1, "synthetic-tool-owner")
	b := newBindingStore()
	for round := 0; round <= toolRoundLimit; round++ {
		id := fmt.Sprintf("call-%d", round)
		if !b.saveScoped(ctx, 7, "model-a", id, callBinding{ConversationID: "conversation", ToolRounds: round}) {
			t.Fatalf("failed to save authoritative binding at round %d", round)
		}
		binding, ok := b.lookupScoped(ctx, 7, "model-a", []string{id})
		if !ok {
			t.Fatalf("failed to restore authoritative binding at round %d", round)
		}
		err := b.claimToolTurn(ctx, 7, "model-a", []string{id}, binding)
		if round < toolRoundLimit && err != nil {
			t.Fatalf("claim at authoritative round %d failed: %v", round, err)
		}
		if round == toolRoundLimit && err == nil {
			t.Fatal("authoritative scoped binding allowed a seventh local turn")
		}
	}
}

func TestToolClaimsRedisStaleSnapshotCannotResetConversationBudget(t *testing.T) {
	if toolRoundLimit != 6 {
		t.Fatalf("tool round limit = %d, want 6", toolRoundLimit)
	}
	mini := miniredis.RunT(t)
	ctx := toolClaimContext(t, 1, "synthetic-tool-owner")
	b := toolClaimRedisStore(t, mini)
	binding := callBinding{ConversationID: "conversation"}
	for round := 0; round < toolRoundLimit; round++ {
		// Deliberately keep the caller's stale snapshot at zero. Redis must
		// enforce the conversation budget independently of this snapshot.
		if err := b.claimToolTurn(ctx, 7, "model-a", []string{fmt.Sprintf("call-%d", round)}, binding); err != nil {
			t.Fatalf("claim %d of six failed: %v", round+1, err)
		}
	}
	b = toolClaimRedisStore(t, mini)
	for attempt := 0; attempt < 2; attempt++ {
		if err := b.claimToolTurn(ctx, 7, "model-a", []string{"call-over-budget"}, binding); err == nil {
			t.Fatal("a stale snapshot reset the persisted conversation budget")
		}
	}
	binding.ConversationID = "new-conversation"
	if err := b.claimToolTurn(ctx, 7, "model-a", []string{"call-over-budget"}, binding); err != nil {
		t.Fatalf("budget rejection consumed an unused ID or blocked another conversation: %v", err)
	}
}

func TestToolClaimsRejectInvalidResultsWithoutConsumingIDs(t *testing.T) {
	toolClaimBackends(t, func(t *testing.T, b *bindingStore) {
		ctx := toolClaimContext(t, 1, "synthetic-tool-owner")
		binding := callBinding{ConversationID: "conversation"}
		for _, ids := range [][]string{nil, {""}, {"call-1", "call-1"}, {"call-1", ""}} {
			if err := b.claimToolTurn(ctx, 7, "model-a", ids, binding); err == nil {
				t.Fatalf("invalid result IDs accepted: %q", ids)
			}
		}
		if err := b.claimToolTurn(context.Background(), 7, "model-a", []string{"call-1"}, binding); err == nil {
			t.Fatal("unauthenticated claim accepted")
		}
		if err := b.claimToolTurn(ctx, 7, "model-a", []string{"call-1"}, binding); err != nil {
			t.Fatalf("invalid attempt consumed the valid ID: %v", err)
		}
		if err := b.claimToolTurn(ctx, 7, "model-a", []string{"call-2", "call-1"}, binding); err == nil {
			t.Fatal("batch containing a consumed result accepted")
		}
		if err := b.claimToolTurn(ctx, 7, "model-a", []string{"call-2"}, binding); err != nil {
			t.Fatalf("rejected batch partially consumed an unused ID: %v", err)
		}
	})
}
