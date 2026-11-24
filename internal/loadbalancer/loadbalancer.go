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
	"orchids-api/internal/config"
	"orchids-api/internal/store"

	"golang.org/x/sync/singleflight"
)

const defaultCacheTTL = config.LoadBalancerCacheTTL

// DefaultAccountConcurrency is the per-account in-flight ceiling every provider
// falls back to when the account carries no explicit max_concurrent. A single
// provider slot is too narrow for a gateway that multiplexes a chat turn with
// its client's side calls: with one slot the second concurrent request waits out
// the whole reservation window and then fails as an overload, even though the
// upstream account is healthy. Providers used to differ here (WorkBuddy 3, the
// others 1); they now share one explicit value so the pool's
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
	Store *store.Store
	mu    sync.RWMutex
	// Published rows and their channel lists are immutable. Callers receive a
	// private copy of the selected account, not a pointer into this snapshot.
	cachedAccounts  []*store.Account
	cachedByChannel map[string][]*store.Account
	cacheExpires    time.Time
	cacheGeneration uint64
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

// AccountFilter accepts a read-only candidate or explains why it was withheld.
type AccountFilter func(*store.Account) error

// A filter can report model cooldown, plan mismatch, or other ineligibility.
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
	return lb.getNextAccountWithFilter(ctx, excludeIDs, channel, tracker, filter, nil)
}

// PreparedAccountFilter batches request-scoped observations before filtering a
// scan window. Prepare may be called again for a full-pool fallback. Like Match,
// Prepare must treat the supplied snapshot as read-only.
type PreparedAccountFilter struct {
	Prepare func([]*store.Account) error
	Match   AccountFilter
}

func (lb *LoadBalancer) GetNextAccountWithPreparedFilter(ctx context.Context, excludeIDs []int64, channel string, tracker ConnTracker, filter PreparedAccountFilter) (*store.Account, error) {
	return lb.getNextAccountWithFilter(ctx, excludeIDs, channel, tracker, filter.Match, filter.Prepare)
}

func (lb *LoadBalancer) getNextAccountWithFilter(ctx context.Context, excludeIDs []int64, channel string, tracker ConnTracker, filter AccountFilter, prepare func([]*store.Account) error) (*store.Account, error) {
	if ctx.Err() != nil {
		return nil, context.Cause(ctx)
	}
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
	type scanCounts struct {
		candidates, matched, throttled, spent, restricted, noModel, modelCooldown int
	}
	var counts scanCounts
	for _, id := range excludeIDs {
		excludeSet[id] = true
	}

	// Rotate bounded scan windows and fall back to the full pool if necessary.
	scanned := accounts
	if len(accounts) > accountScanWindow {
		start := lb.rotateScanCursor(len(accounts))
		scanned = make([]*store.Account, 0, accountScanWindow)
		for offset := range accountScanWindow {
			scanned = append(scanned, accounts[(start+offset)%len(accounts)])
		}
	}

	var scanErr error
	scan := func(candidates []*store.Account) bool {
		if ctx.Err() != nil {
			scanErr = context.Cause(ctx)
			return false
		}
		if prepare != nil {
			if err := prepare(candidates); err != nil {
				scanErr = err
				return false
			}
		}
		counts = scanCounts{}
		for _, acc := range candidates {
			if ctx.Err() != nil {
				scanErr = context.Cause(ctx)
				return false
			}
			if acc == nil || excludeSet[acc.ID] {
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
			counts.candidates++
			if filter != nil {
				if rejectErr := filter(acc); rejectErr != nil {
					switch {
					case errors.Is(rejectErr, ErrModelUnavailable):
						counts.noModel++
					case errors.Is(rejectErr, ErrModelThrottled):
						counts.modelCooldown++
					}
					continue
				}
			}
			counts.matched++
			available := lb.availableAccount(ctx, acc)
			if available == nil {
				if store.WorkBuddyRestrictionUntil(acc).After(time.Now()) {
					counts.restricted++
				}
				switch strings.TrimSpace(acc.StatusCode) {
				case "429":
					counts.throttled++
				case "402":
					counts.spent++
				}
				continue
			}
			filtered = append(filtered, available)
		}
		return len(filtered) > 0
	}
	if !scan(scanned) && scanErr == nil && len(scanned) != len(accounts) {
		// Nothing usable in this window: fall back to the whole pool once.
		scan(accounts)
	}
	if scanErr != nil {
		return nil, scanErr
	}
	if len(filtered) == 0 {
		errorPrefix := "no enabled accounts available for channel: " + channel
		var detail string
		switch {
		case channel != "" && counts.candidates > 0 && counts.matched == 0:
			// Model rejection and cooldown have different retry semantics.
			switch {
			case counts.noModel == counts.candidates:
				detail = "the requested model is not covered by any matching account's plan"
			case counts.modelCooldown > 0 && counts.noModel > 0:
				detail = "the requested model is cooling down on some matching accounts and not covered by the plans of the rest"
			default:
				detail = "all matching accounts are cooling down for the requested model"
			}
		case channel == "workbuddy" && counts.matched > 0 && counts.restricted == counts.matched:
			detail = "workbuddy API error: status=403, code=11140, message=all matching accounts are restricted"
		case channel != "" && counts.matched > 0 && counts.throttled == counts.matched:
			detail = "all matching accounts are rate-limited or cooling down"
		case channel != "" && counts.matched > 0 && counts.spent == counts.matched:
			detail = "all matching accounts have exhausted their allowance"
		case channel != "" && counts.spent > 0 && counts.throttled > 0:
			// A mixed pool remains retryable; preserve its rate-limit classification.
			detail = fmt.Sprintf("all matching accounts are rate-limited or cooling down: %d rate-limited, %d parked for a spent allowance", counts.throttled, counts.spent)
		}
		if detail != "" {
			return nil, fmt.Errorf("%s (%s)", errorPrefix, detail)
		}
		return nil, errors.New(errorPrefix)
	}
	account := lb.selectAccountWithTracker(filtered, tracker)
	if account == nil {
		return nil, fmt.Errorf("no enabled accounts available for channel: %s (all matching accounts are at their concurrency limit)", channel)
	}

	slog.Debug("Selected account", "id", account.ID, "name", account.Name, "type", account.AccountType)

	return account.Clone(), nil
}

// InvalidateAccounts forces the next selection to reread changed account rows.
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
	lb.invalidateAccountsLocked(changed)
}

// InvalidateAccountSnapshot invalidates only the observed cached revision. A
// shared provider refresh can return to several concurrent selectors; once one
// has invalidated the old row, the others must not evict the new row again.
func (lb *LoadBalancer) InvalidateAccountSnapshot(observed *store.Account) {
	if lb == nil || observed == nil {
		return
	}
	lb.mu.Lock()
	defer lb.mu.Unlock()
	for _, current := range lb.cachedAccounts {
		if current != nil && current.ID == observed.ID && current.UpdatedAt.Equal(observed.UpdatedAt) {
			lb.invalidateAccountsLocked(map[int64]struct{}{observed.ID: {}})
			return
		}
	}
}

func (lb *LoadBalancer) invalidateAccountsLocked(changed map[int64]struct{}) {
	lb.cacheGeneration++
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
	return lb.getOrLoadEnabledAccounts(ctx, func(ctx context.Context) ([]*store.Account, error) {
		if lb.Store == nil {
			return nil, errors.New("account store is not configured")
		}
		return lb.Store.GetEnabledAccounts(ctx)
	})
}

func (lb *LoadBalancer) getOrLoadEnabledAccounts(ctx context.Context, load func(context.Context) ([]*store.Account, error)) ([]*store.Account, error) {
	type publishedLoad struct {
		rows       []*store.Account
		generation uint64
	}
	const maxLoads = 3
	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			return nil, context.Cause(ctx)
		}
		lb.mu.RLock()
		accounts, valid := lb.cachedAccounts, time.Now().Before(lb.cacheExpires)
		lb.mu.RUnlock()
		if valid {
			return accounts, nil
		}
		if attempt >= maxLoads {
			return nil, errors.New("account pool changed while refreshing")
		}
		result, err, _ := lb.sfGroup.Do("getEnabledAccounts", func() (any, error) {
			lb.mu.RLock()
			valid, generation := time.Now().Before(lb.cacheExpires), lb.cacheGeneration
			current := lb.cachedAccounts
			lb.mu.RUnlock()
			if valid {
				return publishedLoad{rows: current, generation: generation}, nil
			}
			loaded, err := load(ctx)
			if err != nil {
				return nil, err
			}
			byChannel := indexAccountsByChannel(loaded)
			lb.mu.Lock()
			defer lb.mu.Unlock()
			if lb.cacheGeneration == generation {
				lb.cachedAccounts = loaded
				lb.cachedByChannel = byChannel
				ttl := lb.cacheTTL
				if ttl <= 0 {
					ttl = defaultCacheTTL
				}
				lb.cacheExpires = time.Now().Add(ttl)
				return publishedLoad{rows: loaded, generation: generation}, nil
			}
			return nil, nil
		})
		if err != nil {
			return nil, err
		}
		if filled, ok := result.(publishedLoad); ok {
			lb.mu.RLock()
			current := lb.cacheGeneration == filled.generation
			lb.mu.RUnlock()
			if current {
				return filled.rows, nil
			}
		}
		// An invalidation overlapped this load or its notification to waiters.
	}
}

func indexAccountsByChannel(accounts []*store.Account) map[string][]*store.Account {
	byChannel := make(map[string][]*store.Account)
	for _, acc := range accounts {
		if acc == nil {
			continue
		}
		typ, mode := strings.ToLower(acc.AccountType), strings.ToLower(acc.AgentMode)
		if typ != "" {
			byChannel[typ] = append(byChannel[typ], acc)
		}
		if mode != "" && mode != typ {
			byChannel[mode] = append(byChannel[mode], acc)
		}
	}
	return byChannel
}

// The bounded scan reuses ID and tie buffers on the hot path.
type selectionScratch struct {
	ids  []int64
	best []int
}

var selectionScratchPool = sync.Pool{
	New: func() any {
		return &selectionScratch{ids: make([]int64, 0, 64), best: make([]int, 0, 16)}
	},
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
			for i := range n {
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

	scratch := selectionScratchPool.Get().(*selectionScratch)
	defer selectionScratchPool.Put(scratch)

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
	// Filter the tie set in place. Unseen accounts beat any seen account;
	// otherwise keep only the oldest selection timestamp.
	oldest := time.Time{}
	unseen := false
	pool := best[:0]
	for _, i := range best {
		last, seen := lb.lastSelected[accounts[i].ID]
		if seen && unseen {
			continue
		}
		if len(pool) == 0 || (!seen && !unseen) || (seen && last.Before(oldest)) {
			pool, oldest, unseen = pool[:0], last, !seen
		}
		if !seen || last.Equal(oldest) {
			pool = append(pool, i)
		}
	}
	picked := accounts[pool[rand.IntN(len(pool))]]
	lb.lastSelected[picked.ID] = time.Now()
	return picked
}

func (lb *LoadBalancer) ReserveConnection(ctx context.Context, accountID, limit int64) (func(), bool) {
	if lb == nil {
		return nil, true
	}
	return ReserveConnection(ctx, lb.connTracker, accountID, limit)
}

func (lb *LoadBalancer) availableAccount(ctx context.Context, acc *store.Account) *store.Account {
	if !store.AccountAuthActive(acc) {
		return nil
	}
	if until := store.WorkBuddyRestrictionUntil(acc); until.After(time.Now()) {
		return nil
	}
	// A paid Build billing snapshot is an authoritative routing signal: do not
	// keep probing an account known to be exhausted before the period resets.
	if isPaidGrokBuildAccount(acc) && acc.GrokBilling.IsExhausted() {
		now := time.Now().UTC()
		periodEnd := acc.GrokBilling.PeriodEnd()
		if periodEnd.IsZero() || now.Before(periodEnd) {
			return nil
		}
		// Once the billing period ends, admit exactly one account probe per
		// bounded interval. Without an atomic claim every concurrent request
		// floods the same exhausted account the instant PeriodEnd passes.
		if lb.Store == nil {
			return nil
		}
		claimed, err := lb.Store.ClaimGrokPaidQuotaProbe(ctx, acc.ID, now)
		if err != nil || !claimed {
			return nil
		}
	}
	status := strings.TrimSpace(acc.StatusCode)
	if status == "" {
		return acc
	}

	now := time.Now()
	switch status {
	case store.AccountStatusWorkBuddyQuotaExhausted:
		if !strings.EqualFold(strings.TrimSpace(acc.AccountType), "workbuddy") {
			return nil
		}
		if acc.UsageCurrent > 0 {
			return lb.clearAccountStatus(ctx, acc, "WorkBuddy quota refreshed")
		}
		return acc
	case "401":
		// A refused credential needs operator re-authentication. Legacy rows that
		// only carry StatusCode=401 are also kept out rather than automatically
		// retried with the same dead credential.
		return nil
	case "429":
		if acc.LastAttempt.IsZero() {
			return nil
		}
		if !accountpolicy.AccountHeld(acc, now) {
			return lb.clearAccountStatus(ctx, acc, "429 冷却完成，自动恢复尝试")
		}
		return nil
	case "402":
		reset := acc.QuotaResetAt
		reason := "402 冷却完成，自动恢复尝试"
		if isPaidGrokBuildAccount(acc) {
			if periodEnd := acc.GrokBilling.PeriodEnd(); !periodEnd.IsZero() {
				reset, reason = periodEnd, "402 账期已结束，恢复尝试"
			}
		}
		if reset.IsZero() {
			if acc.LastAttempt.IsZero() {
				return nil
			}
			reset = acc.LastAttempt.Add(accountpolicy.CooldownPayment)
		}
		if !now.Before(reset) {
			return lb.clearAccountStatus(ctx, acc, reason)
		}
		return nil
	case "403", "404":
		// 403/404 can be a temporary ban or a configuration problem.
		// For Grok, a 403 is often a transient upstream denial, so the account
		// must not be blacklisted for long.
		cooldown := accountpolicy.CooldownBlocked
		if strings.EqualFold(acc.AccountType, "grok") {
			cooldown = accountpolicy.CooldownBlockedGro
		}
		if cooldownElapsed(acc.LastAttempt, cooldown, now) {
			return lb.clearAccountStatus(ctx, acc, status+" 冷却完成，自动恢复尝试")
		}
		return nil
	default:
		// Unknown status codes are treated as transient errors with a short cooldown
		// to prevent permanent account exclusion.
		if cooldownElapsed(acc.LastAttempt, accountpolicy.CooldownAuth, now) {
			return lb.clearAccountStatus(ctx, acc, status+" 未知状态冷却完成，自动恢复尝试")
		}
		return nil
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
	return accountpolicy.GrokBuildAccountIsPaid(acc)
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

// publishCachedStatus edits routing scalars on a new snapshot. The old slice
// and row may still be used by concurrent selectors.
func (lb *LoadBalancer) publishCachedStatus(id int64, expected *store.Account, update func(*store.Account)) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	for i, cached := range lb.cachedAccounts {
		if cached == nil || cached.ID != id || (expected != nil && cached != expected) {
			continue
		}
		next := *cached
		update(&next)
		accounts := slices.Clone(lb.cachedAccounts)
		accounts[i] = &next
		lb.cachedAccounts = accounts
		lb.cachedByChannel = indexAccountsByChannel(accounts)
		lb.cacheGeneration++
		return
	}
}

func (lb *LoadBalancer) clearAccountStatus(ctx context.Context, acc *store.Account, reason string) *store.Account {
	next := acc.Clone()
	resetAccountRuntimeState(next)
	lb.publishCachedStatus(acc.ID, acc, resetAccountRuntimeState)
	lb.persistAccountStatus(ctx, next, reason)
	return next
}

// PersistAppliedAccountStatus publishes and persists an account that was already
// mutated by accountpolicy.Verdict.Apply. Unlike MarkAccountStatus it does not
// increment counters or recompute cooldowns a second time.
func (lb *LoadBalancer) PersistAppliedAccountStatus(ctx context.Context, acc *store.Account, reason string) {
	if acc == nil || lb.Store == nil {
		return
	}
	if acc.DoubaoSessionUpdate != nil {
		// Commit the generation-bound verdict before publishing it to the
		// routing cache. A stale request must not poison a newer QR session.
		if err := lb.Store.UpdateAccount(ctx, acc); err != nil {
			current, readErr := lb.Store.GetAccount(ctx, acc.ID)
			if readErr != nil || current == nil {
				return
			}
			*acc = *current
		}
		lb.publishCachedStatus(acc.ID, nil, func(cached *store.Account) {
			cached.StatusCode, cached.StatusMessage = acc.StatusCode, acc.StatusMessage
			cached.AuthStatus, cached.LastAttempt, cached.VerifiedAt = acc.AuthStatus, acc.LastAttempt, acc.VerifiedAt
		})
		return
	}
	lb.publishCachedStatus(acc.ID, nil, func(cached *store.Account) {
		cached.StatusCode = acc.StatusCode
		cached.StatusMessage = acc.StatusMessage
		cached.LastAttempt = acc.LastAttempt
		cached.AuthStatus = acc.AuthStatus
		cached.RateLimitFailures = acc.RateLimitFailures
		cached.QuotaResetAt = acc.QuotaResetAt
		cached.VerifiedAt = acc.VerifiedAt
		cached.WorkBuddyRestrictedUntil = acc.WorkBuddyRestrictedUntil
	})
	lb.persistAccountStatus(ctx, acc, reason)
}

// MarkAccountStatus records an account status (for external callers such as the
// background refresh).
func (lb *LoadBalancer) MarkAccountStatus(ctx context.Context, acc *store.Account, status string) {
	if acc == nil || lb.Store == nil || status == "" {
		return
	}
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

	lb.publishCachedStatus(acc.ID, nil, func(cached *store.Account) {
		cached.StatusCode = status
		cached.LastAttempt = now
		cached.AuthStatus = acc.AuthStatus
		cached.RateLimitFailures = acc.RateLimitFailures
		cached.QuotaResetAt = acc.QuotaResetAt
	})
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
	lb.InvalidateAccounts([]int64{acc.ID})
	slog.Debug("账号状态已更新", "account_id", acc.ID, "status", acc.StatusCode, "reason", reason)
}
