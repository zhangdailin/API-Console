package loadbalancer

import (
	"context"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/accountpolicy"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

type fixedConnTracker struct {
	counts map[int64]int64
}

func (t *fixedConnTracker) Acquire(accountID int64) {}

func (t *fixedConnTracker) Release(accountID int64) {}

func (t *fixedConnTracker) GetCount(accountID int64) int64 {
	if t == nil {
		return 0
	}
	return t.counts[accountID]
}

func (t *fixedConnTracker) GetCounts(accountIDs []int64) map[int64]int64 {
	out := make(map[int64]int64, len(accountIDs))
	for _, id := range accountIDs {
		out[id] = t.GetCount(id)
	}
	return out
}

func TestSelectAccount_Distribution(t *testing.T) {
	lb := &LoadBalancer{connTracker: NewMemoryConnTracker()}
	accounts := []*store.Account{
		{ID: 1, Name: "Acc1", Weight: 1},
		{ID: 2, Name: "Acc2", Weight: 1},
		{ID: 3, Name: "Acc3", Weight: 1},
	}

	counts := make(map[int64]int)
	iterations := 1000

	for i := 0; i < iterations; i++ {
		acc := lb.selectAccountWithTracker(accounts, nil)
		testutil.False(t, acc == nil, "selectAccount returned nil")
		counts[acc.ID]++
	}

	testutil.CheckFalsef(t, len(counts) < 2, "Expected distribution across multiple accounts, but only got %d accounts", len(counts))

	t.Logf("Counts after %d iterations: %+v", iterations, counts)

	// Ensure each account got a reasonable number of hits (rough check)
	for id, count := range counts {
		testutil.CheckFalsef(t, count < 200, "Account %d got suspiciously low hits: %d", id, count)
	}
}

func TestSelectAccount_WeightedDistribution(t *testing.T) {
	lb := &LoadBalancer{connTracker: NewMemoryConnTracker()}
	// Acc1 has weight 10, Acc2 has weight 1
	// With 0 active conns, the score for both is 0/10 = 0 and 0/1 = 0.
	// So they should still be tied and picked randomly.
	accounts := []*store.Account{
		{ID: 1, Name: "Acc1", Weight: 10},
		{ID: 2, Name: "Acc2", Weight: 1},
	}

	counts := make(map[int64]int)
	iterations := 1000

	for i := 0; i < iterations; i++ {
		acc := lb.selectAccountWithTracker(accounts, nil)
		counts[acc.ID]++
	}

	testutil.CheckFalsef(t, counts[1] == 0 || counts[2] == 0, "Expected both accounts to be picked when tied at score 0, got counts: %+v", counts)
}

func TestSelectAccount_ActiveConnections(t *testing.T) {
	lb := &LoadBalancer{connTracker: NewMemoryConnTracker()}
	acc1 := &store.Account{ID: 1, Name: "Acc1", Weight: 1}
	acc2 := &store.Account{ID: 2, Name: "Acc2", Weight: 1}
	accounts := []*store.Account{acc1, acc2}

	// Mock active connections
	lb.AcquireConnection(acc1.ID) // acc1 has 1 conn, score 1/1 = 1
	// acc2 has 0 conns, score 0/1 = 0

	// Should always pick acc2
	for i := 0; i < 100; i++ {
		selected := lb.selectAccountWithTracker(accounts, nil)
		testutil.CheckEqual(t, selected.ID, acc2.ID)
	}
}

func TestSelectAccountWithTracker_UsesProvidedTracker(t *testing.T) {
	lb := &LoadBalancer{connTracker: NewMemoryConnTracker()}
	acc1 := &store.Account{ID: 1, Name: "Acc1", Weight: 1}
	acc2 := &store.Account{ID: 2, Name: "Acc2", Weight: 1}
	accounts := []*store.Account{acc1, acc2}

	custom := &fixedConnTracker{
		counts: map[int64]int64{
			acc1.ID: 5,
			acc2.ID: 0,
		},
	}

	for i := 0; i < 100; i++ {
		selected := lb.selectAccountWithTracker(accounts, custom)
		testutil.Falsef(t, selected == nil || selected.ID != acc2.ID, "expected Acc2 to be selected via custom tracker, got %#v", selected)
	}
}

// Qoder accounts are interchangeable credentials behind a per-account daily
// allowance, so selection spreads requests across the whole pool instead of
// pinning them to the lowest account ID. The pinned primary is what drained one
// free account's quota while the rest of the pool sat idle.
func TestQoderAccountsAreLoadBalanced(t *testing.T) {
	now := time.Now()
	first := &store.Account{ID: 2, AccountType: "qoder", Enabled: true}
	second := &store.Account{ID: 7, AccountType: "qoder", Enabled: true}
	lb := &LoadBalancer{
		cachedAccounts: []*store.Account{second, first},
		cacheExpires:   now.Add(time.Minute),
		connTracker:    NewMemoryConnTracker(),
	}
	tracker := &fixedConnTracker{counts: map[int64]int64{}}
	selectAccount := func(exclude []int64) (*store.Account, error) {
		return lb.GetNextAccountExcludingByChannelWithTracker(context.Background(), exclude, "qoder", tracker)
	}

	// Idle pool: repeated selections must reach both accounts, not just the
	// lowest ID.
	counts := map[int64]int{}
	for i := 0; i < 20; i++ {
		selected, err := selectAccount(nil)
		testutil.Falsef(t, err != nil, "selection %d: err=%v", i, err)
		counts[selected.ID]++
	}
	testutil.Falsef(t, counts[first.ID] == 0 || counts[second.ID] == 0, "qoder selection did not spread across the pool: %v", counts)

	// A saturated account is skipped so the rest of the pool absorbs the
	// request rather than the channel failing while capacity remains.
	tracker.counts[first.ID] = EffectiveAccountConcurrencyLimit(first)
	for i := 0; i < 5; i++ {
		selected, err := selectAccount(nil)
		testutil.Equal(t, err, nil)
		testutil.Equal(t, selected.ID, second.ID)
	}

	// Only a fully saturated pool reports a capacity error.
	tracker.counts[second.ID] = EffectiveAccountConcurrencyLimit(second)
	selected, err := selectAccount(nil)
	testutil.Falsef(t, selected != nil || err == nil || !strings.Contains(err.Error(), "concurrency limit"), "saturated pool: account=%v err=%v, want capacity error", selected, err)

	// An explicitly excluded account stays out of the pool.
	tracker.counts[first.ID] = 0
	tracker.counts[second.ID] = 0
	selected, err = selectAccount([]int64{first.ID})
	testutil.Falsef(t, err != nil || selected.ID != second.ID, "excluded first: account=%v err=%v, want second", selected, err)

	// A cooling account falls out of the pool for the next selection.
	first.StatusCode = "429"
	first.LastAttempt = now
	selected, err = selectAccount(nil)
	testutil.Falsef(t, err != nil || selected.ID != second.ID, "cooling first: account=%v err=%v, want second", selected, err)
}

// TestFilterReasonsDecideTheEmptyPoolAnswer pins what the filter's reason is
// for: the same empty pool has to be answered "retry later" or "this model is
// not available here" depending on why every candidate was withheld.
//
// Production asked for a Qoder model whose cooldown on every account was a
// day-long plan verdict, and was answered "the requested model is temporarily
// rate-limited" — an invitation to retry a condition that could never change.
func TestFilterReasonsDecideTheEmptyPoolAnswer(t *testing.T) {
	now := time.Now()
	for name, tc := range map[string]struct {
		filter  AccountFilter
		wantMsg string
	}{
		"every account is throttled": {
			filter:  func(*store.Account) error { return ErrModelThrottled },
			wantMsg: "cooling down for the requested model",
		},
		"no account's plan covers it": {
			filter:  func(*store.Account) error { return ErrModelUnavailable },
			wantMsg: "not covered by any matching account's plan",
		},
		"a mix of throttled and uncovered": {
			filter: func(acc *store.Account) error {
				if acc.ID%2 == 0 {
					return ErrModelUnavailable
				}
				return ErrModelThrottled
			},
			wantMsg: "cooling down on some matching accounts",
		},
		"a caller rule with no model-cooldown verdict": {
			filter:  func(*store.Account) error { return ErrAccountNotEligible },
			wantMsg: "cooling down for the requested model",
		},
	} {
		t.Run(name, func(t *testing.T) {
			tracker := NewMemoryConnTracker()
			lb := &LoadBalancer{
				connTracker: tracker,
				cachedAccounts: []*store.Account{
					{ID: 2, Name: "WB1", AccountType: "workbuddy", Enabled: true},
					{ID: 3, Name: "WB2", AccountType: "workbuddy", Enabled: true},
				},
				cacheExpires: now.Add(time.Minute),
			}
			_, err := lb.GetNextAccountExcludingByChannelWithTrackerFilter(context.Background(), nil, "workbuddy", tracker, tc.filter)
			testutil.False(t, err == nil, "expected an empty-pool error")
			testutil.MustContain(t, err.Error(), tc.wantMsg)
		})
	}
}

func TestMemoryConnTrackerTryAcquireIsBounded(t *testing.T) {
	tracker := NewMemoryConnTracker()
	testutil.False(t, !tracker.TryAcquire(7, 1), "first reservation should succeed")
	testutil.False(t, tracker.TryAcquire(7, 1), "second reservation should be rejected")
	tracker.Release(7)
	testutil.False(t, !tracker.TryAcquire(7, 1), "reservation should succeed after release")
}

func TestPersistAppliedAccountStatus_DoesNotMutateVerdictAgain(t *testing.T) {
	lb := &LoadBalancer{
		Store:          &store.Store{},
		connTracker:    NewMemoryConnTracker(),
		cachedAccounts: []*store.Account{{ID: 1, Name: "account", AccountType: "workbuddy", Enabled: true}},
	}
	acc := &store.Account{ID: 1, AccountType: "workbuddy"}
	at := time.Now().Add(-time.Second)
	verdict := accountpolicy.Verdict{
		Status: "429", Message: "slow down", Scope: accountpolicy.ScopeAccount,
		Cooldown: accountpolicy.CooldownRateLimit, At: at,
	}
	verdict.Apply(acc)
	failures := acc.RateLimitFailures
	lastAttempt := acc.LastAttempt

	lb.PersistAppliedAccountStatus(context.Background(), acc, "test")

	testutil.Equal(t, acc.RateLimitFailures, failures)
	testutil.Equal(t, acc.RateLimitFailures, 1)
	testutil.Falsef(t, !acc.LastAttempt.Equal(lastAttempt), "last attempt mutated during persistence: got=%v want=%v", acc.LastAttempt, lastAttempt)
	cached := lb.cachedAccounts[0]
	testutil.Falsef(t, cached.RateLimitFailures != 1 || cached.StatusMessage != "slow down" || !cached.LastAttempt.Equal(at), "cached account did not copy applied verdict: %+v", cached)
}

func TestMarkAccountStatus_Repeated429RefreshesCooldownStart(t *testing.T) {
	lb := &LoadBalancer{
		Store:          &store.Store{},
		connTracker:    NewMemoryConnTracker(),
		cachedAccounts: []*store.Account{{ID: 1, Name: "WorkBuddy1", AccountType: "workbuddy", Enabled: true}},
	}
	acc := &store.Account{
		ID:          1,
		AccountType: "workbuddy",
		StatusCode:  "429",
		LastAttempt: time.Now().Add(-30 * time.Second),
	}

	before := acc.LastAttempt
	lb.MarkAccountStatus(context.Background(), acc, "429")

	testutil.Falsef(t, !acc.LastAttempt.After(before), "expected repeated 429 to refresh cooldown start, before=%v after=%v", before, acc.LastAttempt)
	got := lb.cachedAccounts[0].LastAttempt
	testutil.Falsef(t, !got.After(before), "expected cached repeated 429 to refresh cooldown start, before=%v after=%v", before, got)
	testutil.Equal(t, acc.RateLimitFailures, 1)
	testutil.Equal(t, lb.cachedAccounts[0].RateLimitFailures, 1)
	remaining := time.Until(acc.QuotaResetAt)
	testutil.Falsef(t, remaining < 29*time.Second || remaining > 31*time.Second, "first 429 cooldown=%v want about 30s", remaining)
	lb.MarkAccountStatus(context.Background(), acc, "429")
	remaining = time.Until(acc.QuotaResetAt)
	testutil.Falsef(t, acc.RateLimitFailures != 2 || remaining < 59*time.Second || remaining > 61*time.Second, "second 429 failures=%d cooldown=%v want about 1m", acc.RateLimitFailures, remaining)
}

// TestSelectAccountRotatesAcrossEqualAccounts pins the two properties the
// selector actually gives accounts that look identical to it: a pool that has
// never handed an account out serves each one once before repeating any, and a
// long run spreads traffic without starving or favouring an account.
//
// It used to assert a fixed per-account minimum over 30 draws. The tie-break
// among equally loaded accounts is a random pick (see selectAccountWithTracker),
// so 30 uniform draws over three accounts leave one of them below 5 often
// enough to fail CI — about one run in eight. The bounds below are wide enough
// that a uniform selector cannot trip them and tight enough that a selector
// which stopped rotating fails on the first property.
func TestSelectAccountRotatesAcrossEqualAccounts(t *testing.T) {
	const (
		accountCount = 3
		draws        = 300
		// A third of an even split, and half of all traffic. For 300 uniform
		// draws over three accounts the expected count is 100 with a standard
		// deviation near 8, so both bounds sit far outside the noise while
		// still failing a selector that pins requests to one account.
		minShare = draws / (accountCount * 3)
		maxShare = draws / 2
	)
	newAccounts := func() []*store.Account {
		return []*store.Account{
			{ID: 1, Name: "a", Weight: 1},
			{ID: 2, Name: "b", Weight: 1},
			{ID: 3, Name: "c", Weight: 1},
		}
	}

	// A fresh pool must try every account before it repeats one: the tie-break
	// prefers accounts that have never been selected, so the first
	// accountCount draws are a permutation. This is what a regression in the
	// rotation breaks first, and it does not depend on the random pick.
	for round := 0; round < 50; round++ {
		lb := &LoadBalancer{connTracker: NewMemoryConnTracker()}
		accounts := newAccounts()
		seen := map[int64]int{}
		for i := 0; i < accountCount; i++ {
			acc := lb.selectAccountWithTracker(accounts, nil)
			testutil.False(t, acc == nil, "nil account")
			seen[acc.ID]++
		}
		testutil.Equal(t, len(seen), accountCount)
		for id, count := range seen {
			testutil.Falsef(t, count != 1, "account %d served %d times in the first %d draws; every account must be tried once first", id, count, accountCount)
		}
	}

	// Over a long run no account starves and none takes over the pool.
	lb := &LoadBalancer{connTracker: NewMemoryConnTracker()}
	accounts := newAccounts()
	counts := map[int64]int{}
	for i := 0; i < draws; i++ {
		acc := lb.selectAccountWithTracker(accounts, nil)
		testutil.False(t, acc == nil, "nil account")
		counts[acc.ID]++
	}
	testutil.Equal(t, len(counts), accountCount)
	for id, count := range counts {
		testutil.Falsef(t, count < minShare || count > maxShare, "account %d selected %d times out of %d; want between %d and %d", id, count, draws, minShare, maxShare)
	}
}

func TestLargePoolIsScannedInRotatingWindows(t *testing.T) {
	testutil.Falsef(t, accountScanWindow < 8, "window %d is too small to be useful", accountScanWindow)
	lb := &LoadBalancer{connTracker: NewMemoryConnTracker()}
	size := accountScanWindow * 3
	seen := map[int]bool{}
	for i := 0; i < size; i++ {
		start := lb.rotateScanCursor(size)
		for offset := 0; offset < accountScanWindow; offset++ {
			seen[(start+offset)%size] = true
		}
	}
	testutil.Equal(t, len(seen), size)
}

// TestQoderExhaustedSnapshotRetiresAtItsResetBoundary pins the scheduling rule
// that kept a spent-looking account parked after its window had reopened.
//
// The exhausted flag is a snapshot: it is written by a sync round (and by an
// in-band quota notice) and stays on the row until something refreshes it.
// Production held six accounts flagged 300/300 while the same accounts answered
// complete generations, so the flag cannot be the only thing that decides. A
// snapshot taken before the boundary it names is describing a window that no
// longer exists.
func TestQoderExhaustedSnapshotRetiresAtItsResetBoundary(t *testing.T) {
	build := func(syncedAt, resetAt time.Time) *store.Account {
		acc := &store.Account{
			ID:          1,
			AccountType: "qoder",
			StatusCode:  store.AccountStatusQoderQuotaExhausted,
			LastAttempt: time.Now().Add(-time.Hour),
		}
		acc.QoderQuota = store.QoderQuotaSnapshot{
			Limit: 300, Used: 300, Remaining: 0, Exhausted: true,
			SyncedAt: syncedAt, ResetAt: resetAt,
		}
		return acc
	}

	now := time.Now()

	// Snapshot predates the reset it named: the window it describes is over, so
	// the account is admitted with its full capability restored.
	lb := &LoadBalancer{connTracker: NewMemoryConnTracker()}
	stale := build(now.Add(-2*time.Hour), now.Add(-time.Hour))
	testutil.False(t, !lb.isAccountAvailable(context.Background(), stale), "an exhausted snapshot taken before its own reset boundary must be admitted")
	testutil.Equal(t, stale.StatusCode, "")

	// Snapshot taken after the reset and still reporting exhaustion describes
	// the current window: clearing on it would route requests at an account the
	// upstream just said was spent.
	lb = &LoadBalancer{connTracker: NewMemoryConnTracker()}
	current := build(now.Add(-time.Minute), now.Add(-time.Hour))
	testutil.False(t, !lb.isAccountAvailable(context.Background(), current), "a spent account is still admitted to the model-aware selector")
	testutil.Equal(t, current.StatusCode, store.AccountStatusQoderQuotaExhausted)

	// No boundary recorded at all: nothing proves the window reopened.
	lb = &LoadBalancer{connTracker: NewMemoryConnTracker()}
	unknown := build(now.Add(-2*time.Hour), time.Time{})
	testutil.False(t, !lb.isAccountAvailable(context.Background(), unknown), "a spent account with no reset time is still admitted")
	testutil.Equal(t, unknown.StatusCode, store.AccountStatusQoderQuotaExhausted)

	// A non-Qoder row must not borrow the Qoder rule.
	lb = &LoadBalancer{connTracker: NewMemoryConnTracker()}
	wrongChannel := build(now.Add(-2*time.Hour), now.Add(-time.Hour))
	wrongChannel.AccountType = "workbuddy"
	testutil.False(t, lb.isAccountAvailable(context.Background(), wrongChannel), "a non-Qoder row holding the Qoder status must stay excluded")
}
