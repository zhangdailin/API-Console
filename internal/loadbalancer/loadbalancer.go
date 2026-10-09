package loadbalancer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/accountpolicy"
	"orchids-api/internal/channel"
	"orchids-api/internal/store"

	"golang.org/x/sync/singleflight"
)

const defaultCacheTTL = 5 * time.Second

// DefaultAccountConcurrency is the per-account in-flight ceiling every provider
// falls back to when the account carries no explicit max_concurrent. A single
// provider slot is too narrow for a gateway that multiplexes a chat turn with
// its client's side calls: with one slot the second concurrent request waits out
// the whole reservation window and then fails as an overload, even though the
// upstream account is healthy. Providers used to differ here (WorkBuddy 3, the
// others 1, Qoder unlimited); they now share one explicit value so the pool's
// capacity is a deliberate setting rather than a per-channel accident.
const DefaultAccountConcurrency int64 = 10

// EffectiveAccountConcurrencyLimit resolves the in-flight ceiling for one
// account. An explicit per-account value always wins; a known provider falls
// back to DefaultAccountConcurrency; an unknown account type stays unlimited so
// a future channel is not silently throttled before it has a documented limit.
func EffectiveAccountConcurrencyLimit(acc *store.Account) int64 {
	if acc == nil {
		return 0
	}
	if acc.MaxConcurrent > 0 {
		return int64(acc.MaxConcurrent)
	}
	if channel.IsSupported(acc.AccountType) {
		return DefaultAccountConcurrency
	}
	return 0
}

type LoadBalancer struct {
	Store           *store.Store
	mu              sync.RWMutex
	cachedAccounts  []*store.Account
	cachedByChannel map[string][]*store.Account
	cacheExpires    time.Time
	cacheTTL        time.Duration
	connTracker     ConnTracker
	sfGroup         singleflight.Group
	// scanCursor rotates the window a large pool is examined through.
	scanCursor int
	// lastSelected remembers when each account was last handed out, for the
	// least-recently-used tie-break. It is kept here rather than on the account
	// because the cached account objects are shared by concurrent callers.
	lastSelected map[int64]time.Time
	selectedMu   sync.Mutex
}

func NewWithCacheTTL(s *store.Store, cacheTTL time.Duration) *LoadBalancer {
	if cacheTTL <= 0 {
		cacheTTL = defaultCacheTTL
	}
	return &LoadBalancer{
		Store:        s,
		cacheTTL:     cacheTTL,
		connTracker:  NewMemoryConnTracker(),
		lastSelected: make(map[int64]time.Time),
	}
}

// accountScanWindow bounds how many accounts one selection examines before it
// falls back to the whole pool.
const accountScanWindow = 64

// rotateScanCursor advances the window start so successive requests cover the
// pool instead of always looking at its head.
func (lb *LoadBalancer) rotateScanCursor(size int) int {
	if size <= 0 {
		return 0
	}
	lb.selectedMu.Lock()
	defer lb.selectedMu.Unlock()
	start := lb.scanCursor % size
	lb.scanCursor = (start + accountScanWindow) % size
	return start
}

// SetConnTracker replaces the default in-memory connection tracker.
func (lb *LoadBalancer) SetConnTracker(ct ConnTracker) { lb.connTracker = ct }

// AccountFilter decides whether one candidate may serve the request. A nil error
// accepts it; a non-nil one withholds it and says why, because the reason is what
// an emptied pool has to report. A throttle means "come back later" and a plan
// that does not cover the model means "this will never work here"; the pool
// cannot tell those apart from the outside, and only the filter knows.
type AccountFilter func(*store.Account) error

// The model-cooldown verdicts a filter can hand back. They are not failures:
// they are the reason a candidate was withheld.
var (
	// ErrModelThrottled withholds a candidate that is cooling down for this
	// model after the upstream asked for a pause.
	ErrModelThrottled = errors.New("model is cooling down on this account")
	// ErrModelUnavailable withholds a candidate whose plan does not cover the
	// model at all.
	ErrModelUnavailable = errors.New("model is not covered by this account's plan")
	// ErrAccountNotEligible withholds a candidate for one of the caller's other
	// rules (a catalog mismatch, a free-model requirement). The pool does not name
	// it, so an emptied pool keeps its existing wording for that shape.
	ErrAccountNotEligible = errors.New("account not eligible for this request")
)

func (lb *LoadBalancer) GetNextAccountExcludingByChannelWithTracker(ctx context.Context, excludeIDs []int64, channel string, tracker ConnTracker) (*store.Account, error) {
	return lb.GetNextAccountExcludingByChannelWithTrackerFilter(ctx, excludeIDs, channel, tracker, nil)
}

func (lb *LoadBalancer) GetNextAccountExcludingByChannelWithTrackerFilter(ctx context.Context, excludeIDs []int64, channel string, tracker ConnTracker, filter AccountFilter) (*store.Account, error) {
	accounts, err := lb.getEnabledAccounts(ctx)
	if err != nil {
		return nil, err
	}
	if channel != "" {
		lb.mu.RLock()
		if lb.cachedByChannel != nil && len(accounts) == len(lb.cachedAccounts) &&
			(len(accounts) == 0 || &accounts[0] == &lb.cachedAccounts[0]) {
			accounts = lb.cachedByChannel[strings.ToLower(channel)]
		}
		lb.mu.RUnlock()
	}

	var filtered []*store.Account
	excludeSet := make(map[int64]bool)
	// Counted per scan pass (see scan below), so the reasons reported for an empty
	// pool describe one pass rather than a window and its fallback added together.
	channelCandidates := 0
	channelMatched := 0
	rateLimitedUnavailable := 0
	allowanceParked := 0
	// Among the candidates the caller's filter withheld, what did it say? The
	// answer decides whether an empty pool invites a retry or reports a model no
	// matching account can serve.
	filterWithheld := 0
	filterThrottled := 0
	filterUnavailable := 0
	for _, id := range excludeIDs {
		excludeSet[id] = true
	}

	// Every channel including Qoder selects from the whole pool. Qoder accounts
	// are equal, interchangeable credentials behind a per-account daily quota,
	// and Qoder fingerprints each account as its own machine, so the pool is
	// meant to be used in parallel: a pinned primary drains one free account's
	// allowance while the rest of the pool sits idle and then serves the whole
	// channel at a single account's failure rate.
	//
	// A large pool is examined in rotating windows rather than in full. Every
	// request paying for a scan and availability check of thousands of accounts
	// is what made the pool expensive to grow; the window wraps, so every
	// account is still reachable, and a window that yields nothing falls back to
	// the full list so correctness never depends on the window size.
	scanned := accounts
	if len(accounts) > accountScanWindow {
		start := lb.rotateScanCursor(len(accounts))
		scanned = make([]*store.Account, 0, accountScanWindow)
		for offset := 0; offset < accountScanWindow; offset++ {
			scanned = append(scanned, accounts[(start+offset)%len(accounts)])
		}
	}

	scan := func(candidates []*store.Account) bool {
		// Reset per pass: the whole-pool fallback re-scans accounts the window
		// already counted, and doubling the counters would let one pool look like
		// two (and flip which reason is reported for an empty one).
		channelCandidates, channelMatched = 0, 0
		rateLimitedUnavailable, allowanceParked = 0, 0
		filterWithheld, filterThrottled, filterUnavailable = 0, 0, 0
		for _, acc := range candidates {
			if excludeSet[acc.ID] {
				continue
			}
			if channel != "" {
				accType := acc.AccountType
				if strings.TrimSpace(accType) == "" {
					continue
				}
				if !strings.EqualFold(accType, channel) && !strings.EqualFold(acc.AgentMode, channel) {
					continue
				}
			}
			// Counted before the caller's filter: the difference between "this
			// channel has no accounts" and "this channel has accounts and the
			// request's model filter rejected all of them" is the whole reason the
			// pool is empty, and it is reported below.
			channelCandidates++
			if filter != nil {
				if rejectErr := filter(acc); rejectErr != nil {
					filterWithheld++
					switch {
					case errors.Is(rejectErr, ErrModelUnavailable):
						filterUnavailable++
					case errors.Is(rejectErr, ErrModelThrottled):
						filterThrottled++
					}
					continue
				}
			}
			channelMatched++
			if !lb.isAccountAvailable(ctx, acc) {
				switch strings.TrimSpace(acc.StatusCode) {
				case "429":
					rateLimitedUnavailable++
				case "402":
					allowanceParked++
				}
				continue
			}
			filtered = append(filtered, acc)
		}
		return len(filtered) > 0
	}
	if !scan(scanned) && len(scanned) != len(accounts) {
		// Nothing usable in this window: fall back to the whole pool once.
		scan(accounts)
	}
	accounts = filtered

	if len(accounts) == 0 {
		// An empty pool has four different causes and they need four different
		// answers: a request whose model is cooling down on every account, a pool
		// that is rate-limited, a pool whose allowance is spent, and a pool that is
		// partly one and partly the other. The first three used to arrive as the
		// bare sentence below, so "every account is cooling down for this model"
		// reached the operator as "no enabled accounts available for channel",
		// which reads like the channel has no accounts at all — and the caller
		// answered it with a 503 instead of a retryable 429.
		//
		// The mixed case is not a formality: WorkBuddy's outage was three accounts
		// rate-limited and four parked for a spent allowance at once. Each
		// group-only rule failed, the bare sentence was reported, the client got a
		// 503 "server fault" with no capacity cause to retry on, and the operator
		// could see neither the reason nor the split.
		switch {
		case channel != "" && channelCandidates > 0 && channelMatched == 0:
			// The caller's filter withheld every candidate, and the reason it gave
			// decides the answer. "Cooling down" invites a retry; a plan that does
			// not cover the model never becomes true by waiting. Reporting the
			// second as the first is what answered a plan refusal with "the
			// requested model is temporarily rate-limited".
			switch {
			case filterUnavailable > 0 && filterThrottled == 0 && filterUnavailable == filterWithheld:
				return nil, fmt.Errorf("no enabled accounts available for channel: %s (the requested model is not covered by any matching account's plan)", channel)
			case filterThrottled > 0 && filterUnavailable > 0:
				return nil, fmt.Errorf("no enabled accounts available for channel: %s (the requested model is cooling down on some matching accounts and not covered by the plans of the rest)", channel)
			}
			// The caller's filter (a per-model cooldown) rejected every candidate.
			return nil, fmt.Errorf("no enabled accounts available for channel: %s (all matching accounts are cooling down for the requested model)", channel)
		case channel != "" && channelMatched > 0 && rateLimitedUnavailable == channelMatched:
			return nil, fmt.Errorf("no enabled accounts available for channel: %s (all matching accounts are rate-limited or cooling down)", channel)
		case channel != "" && channelMatched > 0 && allowanceParked == channelMatched:
			return nil, fmt.Errorf("no enabled accounts available for channel: %s (all matching accounts have exhausted their allowance)", channel)
		case channel != "" && allowanceParked > 0 && rateLimitedUnavailable > 0:
			// A mixed pool: some accounts are cooling down from a throttle, the rest
			// are parked for a spent allowance. Neither group-only rule above can
			// describe it, and falling through to the bare sentence is what let the
			// WorkBuddy outage reach its callers as an unexplained 503.
			//
			// The answer leads with the rate limit on purpose. A throttle clears on
			// its own, so "retry after the cooldown" is the one instruction that can
			// actually succeed; the parked group is reported beside it so the
			// operator sees the split without opening the account table.
			//
			// The wording is chosen so the shared pool-exhaustion rule reads the
			// mixed pool as a capacity problem: "exhausted their allowance" would
			// classify it as a permanent quota verdict, and "out of credits"
			// (IsCreditExhaustion) would do the same on the entrances that pass the
			// selector error as the last upstream error. "Parked for a spent
			// allowance" describes the same accounts without tripping either rule.
			return nil, fmt.Errorf("no enabled accounts available for channel: %s (all matching accounts are rate-limited or cooling down: %d rate-limited, %d parked for a spent allowance)", channel, rateLimitedUnavailable, allowanceParked)
		}
		return nil, fmt.Errorf("no enabled accounts available for channel: %s", channel)
	}

	account := lb.selectAccountWithTracker(accounts, tracker)
	if account == nil {
		return nil, fmt.Errorf("no enabled accounts available for channel: %s (all matching accounts are at their concurrency limit)", channel)
	}

	slog.Debug("Selected account", "id", account.ID, "name", account.Name, "type", account.AccountType)

	return account, nil
}

// InvalidateAccounts drops the cached snapshot entries for the given accounts so
// the next selection reads their current state.
//
// This replaces the old "wait for the five-second TTL" behaviour: a change that
// has been persisted must be visible to the next request, not to the request
// after next. The whole snapshot is re-read on the next miss, so a removed
// account cannot linger in the pool.
func (lb *LoadBalancer) InvalidateAccounts(ids []int64) {
	if lb == nil || len(ids) == 0 {
		return
	}
	changed := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if id != 0 {
			changed[id] = struct{}{}
		}
	}
	if len(changed) == 0 {
		return
	}

	lb.mu.Lock()
	defer lb.mu.Unlock()
	// A change to the pool always invalidates the TTL as well: keeping the deadline
	// would let a later read serve the pre-change snapshot from a slice another
	// caller still holds.
	lb.cacheExpires = time.Time{}
	lb.cachedByChannel = nil
	if len(lb.cachedAccounts) == 0 {
		return
	}
	kept := make([]*store.Account, 0, len(lb.cachedAccounts))
	for _, acc := range lb.cachedAccounts {
		if acc == nil {
			continue
		}
		if _, dirty := changed[acc.ID]; dirty {
			continue
		}
		kept = append(kept, acc)
	}
	lb.cachedAccounts = kept
}

// AccountChanges implements the account-change subscriber contract.
func (lb *LoadBalancer) AccountChanges(ids []int64) { lb.InvalidateAccounts(ids) }

func (lb *LoadBalancer) getEnabledAccounts(ctx context.Context) ([]*store.Account, error) {
	now := time.Now()
	if accounts, ok := lb.cachedAccountsBefore(now); ok {
		return accounts, nil
	}

	// Use singleflight to prevent cache stampede
	val, err, _ := lb.sfGroup.Do("getEnabledAccounts", func() (interface{}, error) {
		// Double check after acquiring singleflight lock
		if accounts, ok := lb.cachedAccountsBefore(now); ok {
			return accounts, nil
		}

		accounts, err := lb.Store.GetEnabledAccounts(ctx)
		if err != nil {
			return nil, err
		}

		lb.mu.Lock()
		lb.cachedAccounts = accounts
		lb.cachedByChannel = make(map[string][]*store.Account)
		for _, acc := range accounts {
			if acc == nil {
				continue
			}
			typ, mode := strings.ToLower(acc.AccountType), strings.ToLower(acc.AgentMode)
			if typ != "" {
				lb.cachedByChannel[typ] = append(lb.cachedByChannel[typ], acc)
			}
			if mode != "" && mode != typ {
				lb.cachedByChannel[mode] = append(lb.cachedByChannel[mode], acc)
			}
		}
		lb.cacheExpires = time.Now().Add(lb.cacheTTL)
		lb.mu.Unlock()

		return accounts, nil
	})

	if err != nil {
		return nil, err
	}
	return val.([]*store.Account), nil
}

// cachedAccountsBefore reports the snapshot while it is still inside its TTL. The
// rule lives here because both the lock-free fast path and the singleflight
// double-check must agree on when a cached read is valid.
func (lb *LoadBalancer) cachedAccountsBefore(now time.Time) ([]*store.Account, bool) {
	lb.mu.RLock()
	defer lb.mu.RUnlock()
	if len(lb.cachedAccounts) > 0 && now.Before(lb.cacheExpires) {
		return lb.cachedAccounts, true
	}
	return nil, false
}

// selectionScratch holds the per-call working slices of one account selection.
// Selection runs on every request that has to be routed, and it used to allocate
// an id slice plus three account slices per call; keeping the buffers in a pool
// makes the steady state allocation-free.
type selectionScratch struct {
	ids     []int64
	best    []int
	unseen  []int
	coldest []int
}

var selectionScratchPool = sync.Pool{
	New: func() any {
		return &selectionScratch{
			ids:     make([]int64, 0, 64),
			best:    make([]int, 0, 16),
			unseen:  make([]int, 0, 16),
			coldest: make([]int, 0, 16),
		}
	},
}

func acquireSelectionScratch() *selectionScratch {
	return selectionScratchPool.Get().(*selectionScratch)
}

func releaseSelectionScratch(s *selectionScratch) {
	if s == nil {
		return
	}
	// Drop oversized buffers rather than park them for the process lifetime.
	if cap(s.ids) > 1<<14 || cap(s.best) > 1<<10 || cap(s.unseen) > 1<<10 || cap(s.coldest) > 1<<10 {
		return
	}
	s.ids = s.ids[:0]
	s.best = s.best[:0]
	s.unseen = s.unseen[:0]
	s.coldest = s.coldest[:0]
	selectionScratchPool.Put(s)
}

func (lb *LoadBalancer) selectAccountWithTracker(accounts []*store.Account, tracker ConnTracker) *store.Account {
	if len(accounts) == 0 {
		return nil
	}
	// Bound count reads and scratch allocations even when availability filtering
	// fell back to a large pool. Probe further windows only when a window is full.
	if len(accounts) > accountScanWindow {
		start := lb.rotateScanCursor(len(accounts))
		var window [accountScanWindow]*store.Account
		for offset := 0; offset < len(accounts); offset += accountScanWindow {
			n := min(accountScanWindow, len(accounts)-offset)
			for i := 0; i < n; i++ {
				window[i] = accounts[(start+offset+i)%len(accounts)]
			}
			if picked := lb.selectAccountWithTracker(window[:n], tracker); picked != nil {
				return picked
			}
		}
		return nil
	}
	if tracker == nil {
		tracker = lb.connTracker
	}
	if tracker == nil {
		tracker = NewMemoryConnTracker()
	}

	scratch := acquireSelectionScratch()
	defer releaseSelectionScratch(scratch)

	// Batch-fetch connection counts. The slice is borrowed from the pool and
	// reused; GetCounts reads it synchronously and does not retain it.
	ids := scratch.ids[:0]
	for _, acc := range accounts {
		ids = append(ids, acc.ID)
	}
	scratch.ids = ids
	connCounts := tracker.GetCounts(ids)

	// Candidates are held as positions into accounts rather than as a second
	// slice of pointers: the position is all the later passes need, and it costs
	// four bytes instead of a slice header plus backing array per candidate set.
	best := scratch.best[:0]
	minScore := float64(-1)
	for i, acc := range accounts {
		weight := acc.Weight
		if weight <= 0 {
			weight = 1
		}

		conns := connCounts[acc.ID]
		if limit := EffectiveAccountConcurrencyLimit(acc); limit > 0 && conns >= limit {
			continue
		}
		score := float64(conns) / float64(weight)

		if len(best) == 0 || score < minScore {
			best = append(best[:0], i)
			minScore = score
		} else if score == minScore {
			best = append(best, i)
		}
	}
	scratch.best = best
	if len(best) == 0 {
		return nil
	}

	// Among equally loaded accounts prefer the one that has been idle longest.
	// Random tie-breaking kept selecting the same low-latency accounts while
	// others were never tried, which shows up as a hot subset in the pool.
	// Accounts never handed out sort first, so a fresh account is tried.
	lb.selectedMu.Lock()
	defer lb.selectedMu.Unlock()
	if lb.lastSelected == nil {
		// A LoadBalancer built as a struct literal (tests, embedders) has no map.
		lb.lastSelected = make(map[int64]time.Time)
	}
	unseen := scratch.unseen[:0]
	coldest := scratch.coldest[:0]
	var oldest time.Time
	for _, i := range best {
		last, seen := lb.lastSelected[accounts[i].ID]
		if !seen {
			unseen = append(unseen, i)
			continue
		}
		switch {
		case len(coldest) == 0 || last.Before(oldest):
			oldest = last
			coldest = append(coldest[:0], i)
		case last.Equal(oldest):
			coldest = append(coldest, i)
		}
	}
	scratch.unseen, scratch.coldest = unseen, coldest

	pool := best
	switch {
	case len(unseen) > 0:
		pool = unseen
	case len(coldest) > 0:
		pool = coldest
	}
	picked := accounts[pool[rand.IntN(len(pool))]]
	lb.lastSelected[picked.ID] = time.Now()
	return picked
}

func (lb *LoadBalancer) AcquireConnection(accountID int64) { lb.connTracker.Acquire(accountID) }

func (lb *LoadBalancer) ReleaseConnection(accountID int64) { lb.connTracker.Release(accountID) }

const (
	// The account-state policy owns these windows; the aliases keep the pool's
	// existing call sites and tests readable while giving every other entrance
	// (scheduler, admin API, account table) the same numbers.
	//
	// 401 cooldown: the token may already have been refreshed, so retry after a
	// short interval.
	retry401Default = accountpolicy.CooldownAuth
	// 402 usually means the balance/credits are gone. It cools for a day by
	// default so the pool stops hammering an account with no allowance.
	retry402Default = accountpolicy.CooldownPayment
	// 403/404 cooldown: the account may be banned or misconfigured, so retry
	// after a longer interval.
	retry403Default = accountpolicy.CooldownBlocked
	// Many Grok 403s are a transient upstream denial or a temporary risk-control
	// decision, so the account must not be blacklisted for long.
	retry403Grok = accountpolicy.CooldownBlockedGro
)

func (lb *LoadBalancer) isAccountAvailable(ctx context.Context, acc *store.Account) bool {
	if !store.AccountAuthActive(acc) {
		return false
	}
	// A paid Build billing snapshot is an authoritative routing signal: do not
	// keep probing an account known to be exhausted before the period resets.
	if isPaidGrokBuildAccount(acc) && acc.GrokBilling.IsExhausted() {
		now := time.Now().UTC()
		periodEnd := acc.GrokBilling.PeriodEnd()
		if periodEnd.IsZero() || now.Before(periodEnd) {
			return false
		}
		// Once the billing period ends, admit exactly one account probe per
		// bounded interval. Without an atomic claim every concurrent request
		// floods the same exhausted account the instant PeriodEnd passes.
		if lb.Store == nil {
			return false
		}
		claimed, err := lb.Store.ClaimGrokPaidQuotaProbe(ctx, acc.ID, now)
		if err != nil || !claimed {
			return false
		}
	}
	status := strings.TrimSpace(acc.StatusCode)
	if status == "" {
		return true
	}

	now := time.Now()
	switch status {
	case store.AccountStatusQoderQuotaExhausted:
		if !strings.EqualFold(strings.TrimSpace(acc.AccountType), "qoder") {
			return false
		}
		// The exhausted verdict is a snapshot, not a fact about right now: it is
		// written by a sync round (and now also by an in-band quota notice) and
		// stays on the row until something refreshes it. Production held six
		// accounts flagged 300/300 while the same accounts answered complete
		// generations, so the flag must never be the only thing that decides.
		//
		// Two signals retire it. A newer snapshot that says the allowance is back
		// is the obvious one. The second is the reset boundary: once the window
		// the snapshot named has passed, the reading describes a window that no
		// longer exists, and holding the account out on it meant it stayed parked
		// until the next sync happened to notice the clock, not until the
		// upstream said so.
		if !acc.QoderQuota.Exhausted && acc.QoderQuota.Remaining > 0 {
			lb.clearAccountStatus(ctx, acc, "Qoder 额度已刷新，恢复完整能力")
		} else if reset := acc.QoderQuota.ResetAt; !reset.IsZero() &&
			!now.Before(reset) && acc.QoderQuota.SyncedAt.Before(reset) {
			// Only a snapshot taken *before* the boundary it names is stale. A
			// reading taken after the reset and still reporting exhaustion is
			// describing the current window, and clearing on it would route
			// requests at an account the upstream just said was spent.
			lb.clearAccountStatus(ctx, acc, "Qoder 额度窗口已重置，恢复完整能力")
		}
		return true
	case store.AccountStatusWorkBuddyQuotaExhausted:
		if !strings.EqualFold(strings.TrimSpace(acc.AccountType), "workbuddy") {
			return false
		}
		if acc.UsageCurrent > 0 {
			lb.clearAccountStatus(ctx, acc, "WorkBuddy 额度已刷新，恢复完整能力")
		}
		return true
	case "401":
		// A refused credential needs operator re-authentication. Legacy rows that
		// only carry StatusCode=401 are also kept out rather than automatically
		// retried with the same dead credential.
		return false
	case "429":
		if acc.LastAttempt.IsZero() {
			return false
		}
		if !accountpolicy.AccountHeld(acc, now) {
			lb.clearAccountStatus(ctx, acc, "429 冷却完成，自动恢复尝试")
			return true
		}
		return false
	case "402":
		// Older Qoder rows used the generic 402 marker before free-only
		// capability states existed. Admit them to the model-aware selector; that
		// selector accepts only an explicitly free current catalog row. This also
		// makes upgrades effective without rewriting Redis by hand.
		if strings.EqualFold(strings.TrimSpace(acc.AccountType), "qoder") {
			return true
		}
		// Paid Build exhaustion recovers at the billing period boundary rather
		// than after an arbitrary 24-hour probe window.
		if isPaidGrokBuildAccount(acc) {
			if periodEnd := acc.GrokBilling.PeriodEnd(); !periodEnd.IsZero() {
				if !now.Before(periodEnd) {
					lb.clearAccountStatus(ctx, acc, "402 账期已结束，恢复尝试")
					return true
				}
				return false
			}
		}
		// 402 usually means the balance/credits are gone. A reset time from the
		// upstream takes precedence when it gives one; otherwise the longer
		// cooldown applies, so the scheduler does not keep hitting the same
		// account that has no allowance.
		if !acc.QuotaResetAt.IsZero() {
			if !now.Before(acc.QuotaResetAt) {
				lb.clearAccountStatus(ctx, acc, "402 冷却完成，自动恢复尝试")
				return true
			}
			return false
		}
		if cooldownElapsed(acc.LastAttempt, retry402Default, now) {
			lb.clearAccountStatus(ctx, acc, "402 冷却完成，自动恢复尝试")
			return true
		}
		return false
	case "403", "404":
		// 403/404 can be a temporary ban or a configuration problem.
		// For Grok, a 403 is often a transient upstream denial, so the account
		// must not be blacklisted for long.
		cooldown := retry403Default
		if strings.EqualFold(acc.AccountType, "grok") {
			cooldown = retry403Grok
		}
		if cooldownElapsed(acc.LastAttempt, cooldown, now) {
			lb.clearAccountStatus(ctx, acc, status+" 冷却完成，自动恢复尝试")
			return true
		}
		return false
	default:
		// Unknown status codes are treated as transient errors with a short cooldown
		// to prevent permanent account exclusion.
		if cooldownElapsed(acc.LastAttempt, retry401Default, now) {
			lb.clearAccountStatus(ctx, acc, status+" 未知状态冷却完成，自动恢复尝试")
			return true
		}
		return false
	}
}

// cooldownElapsed reports whether a status whose only recovery signal is the last
// attempt has served its cooldown. An account with no recorded attempt is never
// admitted by a cooldown expiring.
func cooldownElapsed(lastAttempt time.Time, cooldown time.Duration, now time.Time) bool {
	if lastAttempt.IsZero() {
		return false
	}
	return now.Sub(lastAttempt) >= cooldown
}

func isPaidGrokBuildAccount(acc *store.Account) bool {
	if acc == nil || !strings.EqualFold(strings.TrimSpace(acc.AccountType), "grok") {
		return false
	}
	provider := strings.ToLower(strings.TrimSpace(acc.GrokProvider))
	credential := strings.ToLower(strings.TrimSpace(acc.CredentialType))
	if provider != "build" && !(provider == "" && credential == "oauth") {
		return false
	}
	plan := strings.ToLower(strings.TrimSpace(acc.Subscription))
	return slices.ContainsFunc([]string{"super", "pro", "heavy", "lite", "x_basic", "xbasic", "x_premium", "xpremium", "paid", "team", "enterprise"}, func(paid string) bool { return strings.Contains(plan, paid) })
}

// resetAccountRuntimeState drops the transient routing verdict so the account is
// admitted again on the next selection.
func resetAccountRuntimeState(acc *store.Account) {
	acc.StatusCode = ""
	acc.StatusMessage = ""
	acc.LastAttempt = time.Time{}
	acc.QuotaResetAt = time.Time{}
	acc.RateLimitFailures = 0
}

// cachedAccountLocked returns the cached copy of the account with the given id,
// or nil when the snapshot does not hold it. lb.mu must be held.
func (lb *LoadBalancer) cachedAccountLocked(id int64) *store.Account {
	for _, cached := range lb.cachedAccounts {
		if cached.ID == id {
			return cached
		}
	}
	return nil
}

func (lb *LoadBalancer) clearAccountStatus(ctx context.Context, acc *store.Account, reason string) {
	// Update the cached copy as well so the change reflects immediately.
	lb.mu.Lock()
	resetAccountRuntimeState(acc)
	if cached := lb.cachedAccountLocked(acc.ID); cached != nil {
		resetAccountRuntimeState(cached)
	}
	lb.mu.Unlock()
	lb.persistAccountStatus(ctx, acc, reason)
}

// PersistAppliedAccountStatus publishes and persists an account that was already
// mutated by accountpolicy.Verdict.Apply. Unlike MarkAccountStatus it does not
// increment counters or recompute cooldowns a second time.
func (lb *LoadBalancer) PersistAppliedAccountStatus(ctx context.Context, acc *store.Account, reason string) {
	if acc == nil || lb.Store == nil {
		return
	}
	lb.mu.Lock()
	if cached := lb.cachedAccountLocked(acc.ID); cached != nil {
		cached.StatusCode = acc.StatusCode
		cached.StatusMessage = acc.StatusMessage
		cached.LastAttempt = acc.LastAttempt
		cached.AuthStatus = acc.AuthStatus
		cached.RateLimitFailures = acc.RateLimitFailures
		cached.QuotaResetAt = acc.QuotaResetAt
		cached.VerifiedAt = acc.VerifiedAt
	}
	lb.mu.Unlock()
	lb.persistAccountStatus(ctx, acc, reason)
}

// MarkAccountStatus records an account status (for external callers such as the
// background refresh).
func (lb *LoadBalancer) MarkAccountStatus(ctx context.Context, acc *store.Account, status string) {
	if acc == nil || lb.Store == nil || status == "" {
		return
	}
	lb.mu.Lock()
	now := time.Now()
	acc.StatusCode = status
	acc.LastAttempt = now
	if status != "429" {
		acc.RateLimitFailures = 0
	}
	if status == "401" {
		acc.AuthStatus = store.AccountAuthStatusReauthRequired
	}
	if status == "429" {
		acc.RateLimitFailures++
		cooldown := accountpolicy.RateLimitCooldown(acc.RateLimitFailures)
		if acc.QuotaResetAt.IsZero() || acc.QuotaResetAt.Before(now.Add(cooldown)) {
			acc.QuotaResetAt = now.Add(cooldown)
		}
	}

	// Ensure the cache is updated as well
	if cached := lb.cachedAccountLocked(acc.ID); cached != nil {
		cached.StatusCode = status
		cached.LastAttempt = now
		cached.AuthStatus = acc.AuthStatus
		cached.RateLimitFailures = acc.RateLimitFailures
		cached.QuotaResetAt = acc.QuotaResetAt
	}
	lb.mu.Unlock()
	lb.persistAccountStatus(ctx, acc, "账号状态标记: "+status)
}

func (lb *LoadBalancer) persistAccountStatus(ctx context.Context, acc *store.Account, reason string) {
	if lb.Store == nil {
		return
	}
	if err := lb.Store.UpdateAccount(ctx, acc); err != nil {
		slog.Warn("账号状态更新失败", "account_id", acc.ID, "reason", reason, "error", err)
		return
	}
	slog.Debug("账号状态已更新", "account_id", acc.ID, "status", acc.StatusCode, "reason", reason)
}
