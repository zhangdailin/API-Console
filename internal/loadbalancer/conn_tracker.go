package loadbalancer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
)

// ConnTracker tracks active connections per account for weighted least-connections selection.
type ConnTracker interface {
	Acquire(accountID int64)
	Release(accountID int64)
	GetCount(accountID int64) int64
	GetCounts(accountIDs []int64) map[int64]int64
}

// LimitedConnTracker atomically reserves a slot when a hard per-account limit
// is configured. It is optional so existing custom trackers remain compatible.
type LimitedConnTracker interface {
	TryAcquire(accountID int64, limit int64) bool
}

// --- Memory Implementation ---

// MemoryConnTracker uses sync.Map with atomic counters (the original implementation).
type MemoryConnTracker struct {
	conns sync.Map // map[int64]*atomic.Int64
}

func NewMemoryConnTracker() *MemoryConnTracker { return &MemoryConnTracker{} }

func (t *MemoryConnTracker) Acquire(accountID int64) {
	val, _ := t.conns.LoadOrStore(accountID, &atomic.Int64{})
	val.(*atomic.Int64).Add(1)
}

func (t *MemoryConnTracker) TryAcquire(accountID int64, limit int64) bool {
	val, _ := t.conns.LoadOrStore(accountID, &atomic.Int64{})
	counter := val.(*atomic.Int64)
	for {
		current := counter.Load()
		if limit > 0 && current >= limit {
			return false
		}
		if counter.CompareAndSwap(current, current+1) {
			return true
		}
	}
}

func (t *MemoryConnTracker) Release(accountID int64) {
	if val, ok := t.conns.Load(accountID); ok {
		counter := val.(*atomic.Int64)
		for {
			current := counter.Load()
			if current <= 0 {
				break
			}
			if counter.CompareAndSwap(current, current-1) {
				break
			}
		}
	}
}

func (t *MemoryConnTracker) GetCount(accountID int64) int64 {
	if val, ok := t.conns.Load(accountID); ok {
		return val.(*atomic.Int64).Load()
	}
	return 0
}

func (t *MemoryConnTracker) GetCounts(accountIDs []int64) map[int64]int64 {
	counts := make(map[int64]int64, len(accountIDs))
	for _, id := range accountIDs {
		counts[id] = t.GetCount(id)
	}
	return counts
}

// --- Redis Implementation ---

// RedisConnTracker uses Redis INCR/DECR for distributed connection counting.
type RedisConnTracker struct {
	client        *redis.Client
	prefix        string
	releaseScript *redis.Script
	acquireScript *redis.Script
	refreshScript *redis.Script
	countScript   *redis.Script
	mu            sync.Mutex
	held          map[int64][]*redisConnLease
	closed        bool
	renewStop     chan struct{}
	renewDone     chan struct{}
	countMu       sync.Mutex
	countCache    map[int64]cachedConnCount
	countGroup    singleflight.Group
}

type cachedConnCount struct {
	count   int64
	expires time.Time
}

const redisConnLeaseTTL = 2 * time.Minute

type redisConnLease struct {
	id   string
	stop chan struct{}
	done chan struct{}
	once sync.Once
}

// stopRenewal ends the heartbeat goroutine. It is idempotent because a lease can
// be released by its own request while a shutdown is releasing every lease.
func (l *redisConnLease) stopRenewal() {
	l.once.Do(func() { close(l.stop) })
}

func NewRedisConnTracker(client *redis.Client, prefix string) *RedisConnTracker {
	t := &RedisConnTracker{
		client:    client,
		prefix:    prefix + "conns:",
		held:      make(map[int64][]*redisConnLease),
		renewStop: make(chan struct{}),
		renewDone: make(chan struct{}),
	}
	// Each request owns one expiring sorted-set member. A crashed process stops
	// renewing its members and Redis reclaims them automatically.
	t.releaseScript = redis.NewScript(`
		return redis.call("ZREM", KEYS[1], ARGV[1])
	`)
	t.acquireScript = redis.NewScript(`
		local key = KEYS[1]
		local limit = tonumber(ARGV[1]) or 0
		local kind = redis.call("TYPE", key).ok
		if kind ~= "none" and kind ~= "zset" then redis.call("DEL", key) end
		redis.call("ZREMRANGEBYSCORE", key, "-inf", ARGV[2])
		local current = redis.call("ZCARD", key)
		if limit > 0 and current >= limit then
			return 0
		end
		redis.call("ZADD", key, ARGV[3], ARGV[4])
		redis.call("PEXPIRE", key, ARGV[5])
		return 1
	`)
	t.refreshScript = redis.NewScript(`
		if redis.call("ZSCORE", KEYS[1], ARGV[1]) == false then return 0 end
		redis.call("ZADD", KEYS[1], "XX", ARGV[2], ARGV[1])
		redis.call("PEXPIRE", KEYS[1], ARGV[3])
		return 1
	`)
	// The count read drops expired members and discards a legacy non-sorted-set
	// key, so an upgraded deployment cannot inherit a string counter.
	t.countScript = redis.NewScript(`
		local kind = redis.call("TYPE", KEYS[1]).ok
		if kind ~= "none" and kind ~= "zset" then redis.call("DEL", KEYS[1]); return 0 end
		redis.call("ZREMRANGEBYSCORE", KEYS[1], "-inf", ARGV[1])
		return redis.call("ZCARD", KEYS[1])
	`)
	go t.renewLoop()
	return t
}

func (t *RedisConnTracker) key(accountID int64) string {
	return fmt.Sprintf("%s%d", t.prefix, accountID)
}

func (t *RedisConnTracker) Acquire(accountID int64) { t.acquire(accountID, 0) }

func (t *RedisConnTracker) TryAcquire(accountID int64, limit int64) bool {
	return t.acquire(accountID, limit)
}

func (t *RedisConnTracker) Release(accountID int64) {
	if t == nil || t.client == nil || accountID == 0 {
		return
	}
	t.mu.Lock()
	list := t.held[accountID]
	if len(list) == 0 {
		t.mu.Unlock()
		return
	}
	lease := list[len(list)-1]
	list = list[:len(list)-1]
	if len(list) == 0 {
		delete(t.held, accountID)
	} else {
		t.held[accountID] = list
	}
	t.mu.Unlock()
	lease.stopRenewal()
	<-lease.done
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := t.releaseScript.Run(ctx, t.client, []string{t.key(accountID)}, lease.id).Err(); err == nil {
		t.adjustCachedCount(accountID, -1)
	}
}

// Close releases every lease owned by this process. Expiry remains the crash
// fallback, but a graceful service restart must not make healthy accounts look
// concurrency-exhausted until the lease TTL elapses.
func (t *RedisConnTracker) Close() {
	if t == nil || t.client == nil {
		return
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	close(t.renewStop)
	held := t.held
	t.held = make(map[int64][]*redisConnLease)
	t.mu.Unlock()
	<-t.renewDone

	for _, leases := range held {
		for _, lease := range leases {
			lease.stopRenewal()
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	pipe := t.client.Pipeline()
	for accountID, leases := range held {
		for _, lease := range leases {
			<-lease.done
			pipe.ZRem(ctx, t.key(accountID), lease.id)
		}
	}
	_, _ = pipe.Exec(ctx)
}

func (t *RedisConnTracker) GetCount(accountID int64) int64 {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	val, err := t.countScript.Run(ctx, t.client, []string{t.key(accountID)}, time.Now().UnixMilli()).Int64()
	if err != nil {
		return 0
	}
	return val
}

func (t *RedisConnTracker) GetCounts(accountIDs []int64) map[int64]int64 {
	result := make(map[int64]int64, len(accountIDs))
	now := time.Now()
	missing := make([]int64, 0, len(accountIDs))
	t.countMu.Lock()
	for _, id := range accountIDs {
		if cached, ok := t.countCache[id]; ok && now.Before(cached.expires) {
			result[id] = cached.count
		} else {
			missing = append(missing, id)
		}
	}
	t.countMu.Unlock()
	if len(missing) == 0 {
		return result
	}
	var key strings.Builder
	for _, id := range missing {
		key.WriteString(strconv.FormatInt(id, 10))
		key.WriteByte(',')
	}
	value, _, _ := t.countGroup.Do(key.String(), func() (any, error) {
		counts := t.fetchCounts(missing)
		t.countMu.Lock()
		if t.countCache == nil || len(t.countCache) > 65536 {
			t.countCache = make(map[int64]cachedConnCount)
		}
		for id, count := range counts {
			t.countCache[id] = cachedConnCount{count, time.Now().Add(50 * time.Millisecond)}
		}
		t.countMu.Unlock()
		return counts, nil
	})
	maps.Copy(result, value.(map[int64]int64))
	return result
}

// Selection counts are short-lived hints; TryAcquire is always authoritative.
func (t *RedisConnTracker) adjustCachedCount(id, delta int64) {
	t.countMu.Lock()
	defer t.countMu.Unlock()
	if cached, ok := t.countCache[id]; ok {
		cached.count = max(0, cached.count+delta)
		t.countCache[id] = cached
	}
}

func (t *RedisConnTracker) fetchCounts(accountIDs []int64) map[int64]int64 {
	result := make(map[int64]int64, len(accountIDs))

	if len(accountIDs) == 0 {
		return result
	}
	// Account selection is on every API request. One failing pipeline must not
	// degrade into N sequential two-second Redis calls and amplify an outage by
	// the account-pool size. Use one bounded batch and fail open with zero counts;
	// TryAcquire remains the authoritative atomic limit check.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	pipe := t.client.Pipeline()
	now := fmt.Sprint(time.Now().UnixMilli())
	commands := make([]*redis.IntCmd, len(accountIDs))
	for i, id := range accountIDs {
		key := t.key(id)
		pipe.ZRemRangeByScore(ctx, key, "-inf", now)
		commands[i] = pipe.ZCard(ctx, key)
	}
	_, err := pipe.Exec(ctx)
	if err != nil {
		for _, id := range accountIDs {
			result[id] = 0
		}
		return result
	}

	for i, command := range commands {
		value, _ := command.Result()
		result[accountIDs[i]] = value
	}
	return result
}

func (t *RedisConnTracker) acquire(accountID, limit int64) bool {
	if t == nil || t.client == nil || accountID == 0 {
		return false
	}
	// A newly started process can briefly race Redis connection establishment or
	// script loading. Retry one time so a transport hiccup is not misreported as
	// a hard account concurrency rejection. A limit rejection (result == 0) is
	// returned immediately and remains subject to the real per-account limit.
	var lease *redisConnLease
	var result int64
	var err error
	for range 2 {
		lease = &redisConnLease{id: newRedisConnLeaseID(), stop: make(chan struct{}), done: make(chan struct{})}
		now := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		result, err = t.acquireScript.Run(ctx, t.client, []string{t.key(accountID)},
			limit, now.UnixMilli(), now.Add(redisConnLeaseTTL).UnixMilli(), lease.id, (redisConnLeaseTTL * 2).Milliseconds()).Int64()
		cancel()
		if err == nil || result == 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err != nil || result != 1 {
		return false
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = t.releaseScript.Run(ctx, t.client, []string{t.key(accountID)}, lease.id).Err()
		return false
	}
	t.held[accountID] = append(t.held[accountID], lease)
	t.mu.Unlock()
	// One process-wide scheduler renews leases; a request owns no heartbeat
	// goroutine or ticker. done remains the release/shutdown lifecycle barrier.
	close(lease.done)
	t.adjustCachedCount(accountID, 1)
	return true
}

func (t *RedisConnTracker) renewLoop() {
	defer close(t.renewDone)
	ticker := time.NewTicker(redisConnLeaseTTL / 3)
	defer ticker.Stop()
	for {
		select {
		case <-t.renewStop:
			return
		case <-ticker.C:
			t.renewBatch()
		}
	}
}

var renewConnBatchScript = redis.NewScript(`
 local renewed = 0
 for i = 3, #ARGV do
  if redis.call("ZSCORE", KEYS[1], ARGV[i]) ~= false then
   redis.call("ZADD", KEYS[1], "XX", ARGV[1], ARGV[i])
   renewed = renewed + 1
  end
 end
 if renewed > 0 then redis.call("PEXPIRE", KEYS[1], ARGV[2]) end
 return renewed
`)

func (t *RedisConnTracker) renewBatch() {
	type batch struct {
		accountID int64
		ids       []string
	}
	t.mu.Lock()
	batches := make([]batch, 0, len(t.held))
	for id, leases := range t.held {
		for start := 0; start < len(leases); start += 256 {
			ids := make([]string, 0, min(256, len(leases)-start))
			for _, lease := range leases[start:min(start+256, len(leases))] {
				ids = append(ids, lease.id)
			}
			batches = append(batches, batch{id, ids})
		}
	}
	t.mu.Unlock()
	for start := 0; start < len(batches); start += 128 {
		select {
		case <-t.renewStop:
			return
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		pipe := t.client.Pipeline()
		for _, b := range batches[start:min(start+128, len(batches))] {
			args := make([]any, 0, len(b.ids)+2)
			args = append(args, time.Now().Add(redisConnLeaseTTL).UnixMilli(), (redisConnLeaseTTL * 2).Milliseconds())
			for _, id := range b.ids {
				args = append(args, id)
			}
			// XX plus ZSCORE prevents a concurrent Release from resurrecting a lease.
			renewConnBatchScript.Eval(ctx, pipe, []string{t.key(b.accountID)}, args...)
		}
		_, _ = pipe.Exec(ctx)
		cancel()
	}
}

func newRedisConnLeaseID() string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err == nil {
		return hex.EncodeToString(buffer)
	}
	return fmt.Sprintf("lease-%d", time.Now().UnixNano())
}
