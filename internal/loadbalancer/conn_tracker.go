package loadbalancer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
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
	Reserve(ctx context.Context, accountID, limit int64) (release func(), ok bool)
	GetCount(accountID int64) int64
	GetCounts(accountIDs []int64) map[int64]int64
}

// ErrConnectionLeaseLost means Redis no longer accounts for a live request.
var ErrConnectionLeaseLost = errors.New("connection reservation expired or was removed")

type connLeaseScopeKey struct{}
type connLeaseScope struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
}

// WithConnLeaseScope allows losing any tracked slot to cancel the logical
// request. Releasing an account normally (including before a retry) does not
// cancel the request. Nested callers share the original scope.
func WithConnLeaseScope(ctx context.Context) (context.Context, func()) {
	if scope, ok := ctx.Value(connLeaseScopeKey{}).(connLeaseScope); ok &&
		ctx.Done() != nil && (scope.ctx.Err() == nil || ctx.Err() != nil) {
		return ctx, func() {}
	}
	ctx, cancel := context.WithCancelCause(ctx)
	return context.WithValue(ctx, connLeaseScopeKey{}, connLeaseScope{ctx, cancel}), func() { cancel(nil) }
}

// ReserveConnection returns a release tied to this slot, never to another
// request using the same account.
func ReserveConnection(ctx context.Context, tracker ConnTracker, id, limit int64) (func(), bool) {
	if tracker == nil || id == 0 {
		return nil, true
	}
	return tracker.Reserve(ctx, id, limit)
}

// --- Memory Implementation ---

// MemoryConnTracker uses sync.Map with atomic counters (the original implementation).
type MemoryConnTracker struct {
	conns sync.Map // map[int64]*atomic.Int64
}

func NewMemoryConnTracker() *MemoryConnTracker { return &MemoryConnTracker{} }

func (t *MemoryConnTracker) Reserve(ctx context.Context, accountID, limit int64) (func(), bool) {
	if ctx.Err() != nil || accountID == 0 {
		return nil, false
	}
	val, _ := t.conns.LoadOrStore(accountID, &atomic.Int64{})
	counter := val.(*atomic.Int64)
	for {
		current := counter.Load()
		if limit > 0 && current >= limit {
			return nil, false
		}
		if counter.CompareAndSwap(current, current+1) {
			return sync.OnceFunc(func() { counter.Add(-1) }), true
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

// RedisConnTracker holds one expiring Redis sorted-set member per request.
type RedisConnTracker struct {
	client        *redis.Client
	prefix        string
	releaseScript *redis.Script
	acquireScript *redis.Script
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
	id      string
	expires time.Time
	timer   *time.Timer
	lost    context.CancelCauseFunc
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
		if redis.call("ZSCORE", key, ARGV[4]) ~= false then return 1 end
		local current = redis.call("ZCARD", key)
		if limit > 0 and current >= limit then
			return 0
		end
		redis.call("ZADD", key, ARGV[3], ARGV[4])
		redis.call("PEXPIRE", key, ARGV[5])
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

func (t *RedisConnTracker) Reserve(ctx context.Context, accountID, limit int64) (func(), bool) {
	lease := t.acquire(ctx, accountID, limit)
	if lease == nil {
		return nil, false
	}
	return sync.OnceFunc(func() {
		t.mu.Lock()
		removed := t.removeLeaseLocked(accountID, lease)
		t.mu.Unlock()
		if removed {
			t.releaseMember(accountID, lease.id)
		}
	}), true
}

func (t *RedisConnTracker) removeLeaseLocked(accountID int64, lease *redisConnLease) bool {
	leases := t.held[accountID]
	i := slices.Index(leases, lease)
	if i < 0 {
		return false
	}
	if lease.timer != nil {
		lease.timer.Stop()
	}
	leases = slices.Delete(leases, i, i+1)
	if len(leases) == 0 {
		delete(t.held, accountID)
	} else {
		t.held[accountID] = leases
	}
	return true
}

func (t *RedisConnTracker) releaseMember(accountID int64, member string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if n, err := t.releaseScript.Run(ctx, t.client, []string{t.key(accountID)}, member).Int64(); err == nil && n > 0 {
		t.adjustCachedCount(accountID, -1)
	}
}

func (t *RedisConnTracker) expireLease(accountID int64, lease *redisConnLease, confirmed bool) {
	t.mu.Lock()
	if !confirmed && time.Now().Before(lease.expires) {
		if slices.Contains(t.held[accountID], lease) {
			lease.timer.Reset(time.Until(lease.expires))
		}
		t.mu.Unlock()
		return
	}
	removed := t.removeLeaseLocked(accountID, lease)
	t.mu.Unlock()
	if removed {
		t.adjustCachedCount(accountID, -1)
		if lease.lost != nil {
			lease.lost(ErrConnectionLeaseLost)
		}
		if !confirmed {
			// If a renewal reply was lost, Redis may still have the member.
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = t.releaseScript.Run(ctx, t.client, []string{t.key(accountID)}, lease.id).Err()
		}
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

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	pipe := t.client.Pipeline()
	for accountID, leases := range held {
		for _, lease := range leases {
			lease.timer.Stop()
			if lease.lost != nil {
				lease.lost(ErrConnectionLeaseLost)
			}
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
	// Account scan windows and preference selection can reorder identical sets.
	// Canonical keys allow concurrent callers to share one Redis round trip.
	slices.Sort(missing)
	missing = slices.Compact(missing)
	var key strings.Builder
	for _, id := range missing {
		key.WriteString(strconv.FormatInt(id, 10))
		key.WriteByte(',')
	}
	value, _, _ := t.countGroup.Do(key.String(), func() (any, error) {
		// Another flight may have filled the cache after our initial snapshot.
		// Recheck inside the flight to avoid back-to-back identical pipelines.
		counts := make(map[int64]int64, len(missing))
		pending := make([]int64, 0, len(missing))
		now := time.Now()
		t.countMu.Lock()
		for _, id := range missing {
			if cached, ok := t.countCache[id]; ok && now.Before(cached.expires) {
				counts[id] = cached.count
			} else {
				pending = append(pending, id)
			}
		}
		t.countMu.Unlock()
		fetched := t.fetchCounts(pending)
		maps.Copy(counts, fetched)
		t.countMu.Lock()
		if t.countCache == nil || len(t.countCache) > 65536 {
			t.countCache = make(map[int64]cachedConnCount)
		}
		for id, count := range fetched {
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

func (t *RedisConnTracker) acquire(ctx context.Context, accountID, limit int64) *redisConnLease {
	if t == nil || t.client == nil || accountID == 0 || ctx.Err() != nil {
		return nil
	}
	scope, _ := ctx.Value(connLeaseScopeKey{}).(connLeaseScope)
	start := time.Now()
	lease := &redisConnLease{id: newRedisConnLeaseID(), lost: scope.cancel, expires: start.Add(redisConnLeaseTTL)}
	var result int64
	var err error
	// A timeout may hide a committed acquisition. Retries use the same member.
	for range 2 {
		now := time.Now()
		call, cancel := context.WithTimeout(ctx, 2*time.Second)
		result, err = t.acquireScript.Run(call, t.client, []string{t.key(accountID)},
			limit, now.UnixMilli(), lease.expires.UnixMilli(), lease.id, (redisConnLeaseTTL * 2).Milliseconds()).Int64()
		cancel()
		if err == nil || ctx.Err() != nil {
			break
		}
	}
	if err != nil || (result == 1 && ctx.Err() != nil) {
		t.releaseMember(accountID, lease.id)
		return nil
	}
	if result != 1 {
		return nil
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		t.releaseMember(accountID, lease.id)
		return nil
	}
	t.held[accountID] = append(t.held[accountID], lease)
	lease.timer = time.AfterFunc(time.Until(lease.expires), func() { t.expireLease(accountID, lease, false) })
	t.mu.Unlock()
	t.adjustCachedCount(accountID, 1)
	return lease
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
 local missing = {}
 local now = tonumber(ARGV[3])
 for i = 4, #ARGV do
  local score = redis.call("ZSCORE", KEYS[1], ARGV[i])
  if score and tonumber(score) > now then
   redis.call("ZADD", KEYS[1], "XX", ARGV[1], ARGV[i])
  else
   table.insert(missing, ARGV[i])
  end
 end
 if #missing < #ARGV - 3 then redis.call("PEXPIRE", KEYS[1], ARGV[2]) end
 return missing
`)

func (t *RedisConnTracker) renewBatch() {
	type batch struct {
		accountID int64
		leases    []*redisConnLease
		cmd       *redis.Cmd
	}
	t.mu.Lock()
	batches := make([]batch, 0, len(t.held))
	for id, leases := range t.held {
		for start := 0; start < len(leases); start += 256 {
			batches = append(batches, batch{accountID: id, leases: slices.Clone(leases[start:min(start+256, len(leases))])})
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
		now := time.Now()
		expiry := now.Add(redisConnLeaseTTL)
		active := batches[start:min(start+128, len(batches))]
		for i := range active {
			b := &active[i]
			args := make([]any, 0, len(b.leases)+3)
			args = append(args, expiry.UnixMilli(), (redisConnLeaseTTL * 2).Milliseconds(), now.UnixMilli())
			for _, lease := range b.leases {
				args = append(args, lease.id)
			}
			b.cmd = renewConnBatchScript.Eval(ctx, pipe, []string{t.key(b.accountID)}, args...)
		}
		_, _ = pipe.Exec(ctx)
		cancel()
		for _, b := range active {
			missing, err := b.cmd.StringSlice()
			if err != nil {
				continue // Keep the last proven deadline on transient failures.
			}
			for _, lease := range b.leases {
				if slices.Contains(missing, lease.id) {
					t.expireLease(b.accountID, lease, true)
					continue
				}
				t.mu.Lock()
				if time.Now().Before(lease.expires) && slices.Contains(t.held[b.accountID], lease) && expiry.After(lease.expires) {
					lease.expires = expiry
					lease.timer.Reset(time.Until(expiry))
				}
				t.mu.Unlock()
			}
		}
	}
}

func newRedisConnLeaseID() string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err == nil {
		return hex.EncodeToString(buffer)
	}
	return fmt.Sprintf("lease-%d", time.Now().UnixNano())
}
