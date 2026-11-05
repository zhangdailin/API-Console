package ratelimit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// redisBackend shares pacing and team/model cooldowns across replicas.
type redisBackend struct {
	client *redis.Client
	prefix string
	note   *redis.Script
	pace   *redis.Script
}

var distributed struct {
	sync.RWMutex
	backend *redisBackend
}

// ConfigureRedis shares cooldowns and pacing through Redis. Both planes call it
// with the same store; the last call wins, which is the same client. It
// returns a function restoring the previous backend, for tests.
func ConfigureRedis(client *redis.Client, prefix string) (restore func()) {
	if client == nil {
		return func() {}
	}
	backend := &redisBackend{
		client: client,
		prefix: prefix + "grok:limits:",
		note: redis.NewScript(`
			local current = redis.call("PTTL", KEYS[1])
			local wanted = tonumber(ARGV[1])
			if current < wanted then redis.call("PSETEX", KEYS[1], wanted, "1") end
			return 1
		`),
		// One script call both tries the slot and reports its remaining wait.
		// This halves Redis traffic under contention and removes the 10ms polling
		// fallback caused by the SetNX/PTTL race around key expiry.
		pace: redis.NewScript(`
			if redis.call("SET", KEYS[1], "1", "PX", ARGV[1], "NX") then
				return 0
			end
			local remaining = redis.call("PTTL", KEYS[1])
			if remaining < 1 then remaining = 1 end
			return remaining
		`),
	}
	distributed.Lock()
	previous := distributed.backend
	distributed.backend = backend
	distributed.Unlock()
	return func() {
		distributed.Lock()
		distributed.backend = previous
		distributed.Unlock()
	}
}

func sharedBackend() *redisBackend {
	distributed.RLock()
	backend := distributed.backend
	distributed.RUnlock()
	return backend
}

func (b *redisBackend) key(kind string, parts ...string) string {
	hash := sha256.Sum256([]byte(fmt.Sprint(parts)))
	return b.prefix + kind + ":" + hex.EncodeToString(hash[:16])
}

func (b *redisBackend) noteCooldown(scope Scope, identity, model string, duration time.Duration) error {
	if b == nil || b.client == nil || duration <= 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	return b.note.Run(ctx, b.client, []string{b.key("cooldown", string(scope), identity, model)}, duration.Milliseconds()).Err()
}

func (b *redisBackend) cooldownRemaining(scope Scope, identity, model string) time.Duration {
	if b == nil || b.client == nil {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	remaining, err := b.client.PTTL(ctx, b.key("cooldown", string(scope), identity, model)).Result()
	if err != nil || remaining <= 0 {
		return 0
	}
	return remaining
}

func (b *redisBackend) waitPacing(ctx context.Context, identity string, rate float64) error {
	if b == nil || b.client == nil || rate <= 0 {
		return nil
	}
	interval := max(time.Duration(float64(time.Second)/rate), time.Millisecond)
	key := b.key("pace", identity)
	pace := b.pace
	if pace == nil {
		pace = redis.NewScript(`
			if redis.call("SET", KEYS[1], "1", "PX", ARGV[1], "NX") then return 0 end
			local remaining = redis.call("PTTL", KEYS[1])
			if remaining < 1 then remaining = 1 end
			return remaining
		`)
	}
	for {
		remainingMS, err := pace.Run(ctx, b.client, []string{key}, interval.Milliseconds()).Int64()
		if err != nil {
			return err
		}
		if remainingMS == 0 {
			return nil
		}
		// Spread waiters across the last 10% of the window. Without jitter every
		// replica wakes on the same millisecond and all but one immediately collide.
		remaining := time.Duration(remainingMS) * time.Millisecond
		jitterRange := min(remaining/10, 25*time.Millisecond)
		if jitterRange > 0 {
			digest := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", identity, time.Now().UnixNano())))
			remaining += time.Duration(digest[0]) * jitterRange / 255
		}
		timer := time.NewTimer(remaining)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// cooldownsRemaining reads one selection window in one network round trip.
// The timeout is bounded by the caller so canceled requests do no further I/O.
func (b *redisBackend) cooldownsRemaining(ctx context.Context, queries []Query) (map[Query]time.Duration, error) {
	result := make(map[Query]time.Duration, len(queries))
	if ctx.Err() != nil {
		return result, context.Cause(ctx)
	}
	if b == nil || b.client == nil || len(queries) == 0 {
		return result, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	commands := make([]*redis.DurationCmd, len(queries))
	_, err := b.client.Pipelined(ctx, func(pipe redis.Pipeliner) error {
		for i, q := range queries {
			commands[i] = pipe.PTTL(ctx, b.key("cooldown", string(q.Scope), q.Identity, q.Model))
		}
		return nil
	})
	if err != nil {
		return result, err
	}
	for i, q := range queries {
		if remaining := commands[i].Val(); remaining > 0 {
			result[q] = remaining
		}
	}
	return result, nil
}
