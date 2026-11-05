package ratelimit

import (
	"context"
	"orchids-api/internal/testutil"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func withTestRedis(t *testing.T) *redisBackend {
	t.Helper()
	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	distributed.Lock()
	previous := distributed.backend
	distributed.backend = &redisBackend{
		client: client,
		prefix: "test:grok:limits:",
		note: redis.NewScript(`
			local current = redis.call("PTTL", KEYS[1])
			local wanted = tonumber(ARGV[1])
			if current < wanted then redis.call("PSETEX", KEYS[1], wanted, "1") end
			return 1
		`),
	}
	backend := distributed.backend
	distributed.Unlock()
	t.Cleanup(func() {
		distributed.Lock()
		distributed.backend = previous
		distributed.Unlock()
		_ = client.Close()
	})
	return backend
}

func TestDistributedCooldownVisibleToAnotherRegistry(t *testing.T) {
	withTestRedis(t)
	first := NewRegistry()
	second := NewRegistry()
	first.Note(ScopeRPM, "shared-team", "grok-4.6", time.Minute)
	remaining := second.RetryAfterFor(ScopeRPM, "shared-team", "grok-4.6")
	testutil.False(t, remaining <= 0, "second process did not observe Redis cooldown")
}

func TestDistributedPacingSerializesReplicas(t *testing.T) {
	backend := withTestRedis(t)
	testutil.NoError(t, backend.waitPacing(context.Background(), "shared-account", 2), "first pacing token failed: %v")
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	err := backend.waitPacing(ctx, "shared-account", 2)
	testutil.Error(t, err)
}

func TestTeamCooldownNoteAndRetry(t *testing.T) {
	registry := NewRegistry()
	registry.Note(ScopeRPS, "team-a", "grok-4.5", 30*time.Second)

	remaining := registry.RetryAfterFor(ScopeRPS, "team-a", "grok-4.5")
	testutil.Falsef(t, remaining <= 0 || remaining > 30*time.Second, "remaining = %s, want in (0, 30s]", remaining)
	// Different team/model must be unaffected.
	testutil.Equal(t, registry.RetryAfterFor(ScopeRPS, "team-b", "grok-4.5"), 0)
	testutil.Equal(t, registry.RetryAfterFor(ScopeRPM, "team-a", "grok-4.5"), 0)
}

func TestTeamCooldownNoteIgnoresEmptyTeamOrModel(t *testing.T) {
	registry := NewRegistry()
	registry.Note(ScopeRPS, "", "grok-4.5", time.Minute)
	registry.Note(ScopeRPS, "team-a", "", time.Minute)
	testutil.Equal(t, registry.RetryAfterFor(ScopeRPS, "", "grok-4.5"), 0)
	testutil.Equal(t, registry.RetryAfterFor(ScopeRPS, "team-a", ""), 0)
}

func TestTeamCooldownExpiry(t *testing.T) {
	registry := NewRegistry()
	registry.Note(ScopeRPM, "team-a", "grok-4.5", time.Nanosecond)
	time.Sleep(time.Millisecond)
	testutil.Equal(t, registry.RetryAfterFor(ScopeRPM, "team-a", "grok-4.5"), 0)
}

func TestTeamCooldownNoteKeepsLongest(t *testing.T) {
	registry := NewRegistry()
	registry.Note(ScopeRPS, "team-a", "grok-4.5", 10*time.Second)
	registry.Note(ScopeRPS, "team-a", "grok-4.5", 5*time.Second) // shorter: keep existing
	first := registry.RetryAfterFor(ScopeRPS, "team-a", "grok-4.5")
	registry.Note(ScopeRPS, "team-a", "grok-4.5", 60*time.Second) // longer: extend
	second := registry.RetryAfterFor(ScopeRPS, "team-a", "grok-4.5")
	testutil.Falsef(t, second <= first, "longer note should extend cooldown: first=%s second=%s", first, second)
}

func TestTeamCooldownConcurrentNote(t *testing.T) {
	registry := NewRegistry()
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			registry.Note(ScopeRPS, "team-conc", "grok-4.5", time.Second)
		})
	}
	wg.Wait()
	remaining := registry.RetryAfterFor(ScopeRPS, "team-conc", "grok-4.5")
	testutil.Falsef(t, remaining <= 0, "concurrent notes should leave an entry, got %s", remaining)
	registry.mu.Lock()
	size := len(registry.entries)
	registry.mu.Unlock()
	testutil.False(t, size > MaxEntries, "registry exceeded max size")
}
