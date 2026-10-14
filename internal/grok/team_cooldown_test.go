package grok

import (
	"context"
	"orchids-api/internal/testutil"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func withTestDistributedGrokLimits(t *testing.T) *redisGrokLimits {
	t.Helper()
	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	distributedGrokLimits.Lock()
	previous := distributedGrokLimits.backend
	distributedGrokLimits.backend = &redisGrokLimits{
		client: client,
		prefix: "test:grok:limits:",
		note: redis.NewScript(`
			local current = redis.call("PTTL", KEYS[1])
			local wanted = tonumber(ARGV[1])
			if current < wanted then redis.call("PSETEX", KEYS[1], wanted, "1") end
			return 1
		`),
	}
	backend := distributedGrokLimits.backend
	distributedGrokLimits.Unlock()
	t.Cleanup(func() {
		distributedGrokLimits.Lock()
		distributedGrokLimits.backend = previous
		distributedGrokLimits.Unlock()
		_ = client.Close()
	})
	return backend
}

func TestDistributedCooldownVisibleToAnotherRegistry(t *testing.T) {
	withTestDistributedGrokLimits(t)
	first := newTeamCooldownRegistry()
	second := newTeamCooldownRegistry()
	first.Note(RateLimitScopeRPM, "shared-team", "grok-4.6", time.Minute)
	remaining := second.RetryAfterFor(RateLimitScopeRPM, "shared-team", "grok-4.6")
	testutil.False(t, remaining <= 0, "second process did not observe Redis cooldown")
}

func TestDistributedPacingSerializesReplicas(t *testing.T) {
	backend := withTestDistributedGrokLimits(t)
	testutil.NoError(t, backend.waitPacing(context.Background(), "shared-account", 2), "first pacing token failed: %v")
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	err := backend.waitPacing(ctx, "shared-account", 2)
	testutil.Error(t, err)
}

func TestTeamCooldownNoteAndRetry(t *testing.T) {
	registry := newTeamCooldownRegistry()
	registry.Note(RateLimitScopeRPS, "team-a", "grok-4.5", 30*time.Second)

	remaining := registry.RetryAfterFor(RateLimitScopeRPS, "team-a", "grok-4.5")
	testutil.Falsef(t, remaining <= 0 || remaining > 30*time.Second, "remaining = %s, want in (0, 30s]", remaining)
	// Different team/model must be unaffected.
	testutil.Equal(t, registry.RetryAfterFor(RateLimitScopeRPS, "team-b", "grok-4.5"), 0)
	testutil.Equal(t, registry.RetryAfterFor(RateLimitScopeRPM, "team-a", "grok-4.5"), 0)
}

func TestTeamCooldownNoteIgnoresEmptyTeamOrModel(t *testing.T) {
	registry := newTeamCooldownRegistry()
	registry.Note(RateLimitScopeRPS, "", "grok-4.5", time.Minute)
	registry.Note(RateLimitScopeRPS, "team-a", "", time.Minute)
	testutil.Equal(t, registry.RetryAfterFor(RateLimitScopeRPS, "", "grok-4.5"), 0)
	testutil.Equal(t, registry.RetryAfterFor(RateLimitScopeRPS, "team-a", ""), 0)
}

func TestTeamCooldownExpiry(t *testing.T) {
	registry := newTeamCooldownRegistry()
	registry.Note(RateLimitScopeRPM, "team-a", "grok-4.5", time.Nanosecond)
	time.Sleep(time.Millisecond)
	testutil.Equal(t, registry.RetryAfterFor(RateLimitScopeRPM, "team-a", "grok-4.5"), 0)
}

func TestTeamCooldownNoteKeepsLongest(t *testing.T) {
	registry := newTeamCooldownRegistry()
	registry.Note(RateLimitScopeRPS, "team-a", "grok-4.5", 10*time.Second)
	registry.Note(RateLimitScopeRPS, "team-a", "grok-4.5", 5*time.Second) // shorter: keep existing
	first := registry.RetryAfterFor(RateLimitScopeRPS, "team-a", "grok-4.5")
	registry.Note(RateLimitScopeRPS, "team-a", "grok-4.5", 60*time.Second) // longer: extend
	second := registry.RetryAfterFor(RateLimitScopeRPS, "team-a", "grok-4.5")
	testutil.Falsef(t, second <= first, "longer note should extend cooldown: first=%s second=%s", first, second)
}

func TestTeamCooldownConcurrentNote(t *testing.T) {
	registry := newTeamCooldownRegistry()
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			registry.Note(RateLimitScopeRPS, "team-conc", "grok-4.5", time.Second)
		})
	}
	wg.Wait()
	remaining := registry.RetryAfterFor(RateLimitScopeRPS, "team-conc", "grok-4.5")
	testutil.Falsef(t, remaining <= 0, "concurrent notes should leave an entry, got %s", remaining)
	registry.mu.Lock()
	size := len(registry.entries)
	registry.mu.Unlock()
	testutil.False(t, size > teamCooldownMaxSize, "registry exceeded max size")
}
