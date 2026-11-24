package store

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"orchids-api/internal/testutil"
)

func warpQuotaNumber(value float64) *float64 { return &value }

func testWarpQuota(at time.Time) WarpQuotaSnapshot {
	return WarpQuotaSnapshot{
		Windows: []WarpQuotaWindow{{
			Scope: "workspace_monthly", Unit: "credits", Limit: warpQuotaNumber(100),
			Used: warpQuotaNumber(25), Remaining: warpQuotaNumber(75),
			HasUnlimited: true, BonusRemaining: warpQuotaNumber(0), ResetAt: at.Add(24 * time.Hour),
		}},
		SyncedAt: at, AttemptedAt: at, Status: "ok", Source: "graphql",
	}
}

func TestWarpQuotaAtomicUpdatePreservesDisabledAccountAndRotation(t *testing.T) {
	s, _ := newWarpTestStore(t)
	ctx := t.Context()
	at := time.Date(2030, 1, 2, 0, 0, 0, 0, time.UTC)
	acc := &Account{
		AccountType: "warp", Name: "original", Enabled: true, Weight: 1,
		WarpAccessToken: "quota-access", WarpRefreshToken: "quota-refresh",
		WarpExpiresAt: at.Add(time.Hour), WarpModelIDs: []string{"catalog-model"}, WarpModelsSyncedAt: at,
		WarpQuota: testWarpQuota(at), UsageCurrent: 3, UsageLimit: 4, UsageTotal: 10, RequestCount: 2,
	}
	testutil.NoError(t, s.CreateAccount(ctx, acc))
	admin := acc.Clone()
	admin.Enabled, admin.Name, admin.Weight, admin.MaxConcurrent = false, "operator disabled", 7, 4
	admin.StatusCode, admin.AuthStatus, admin.StatusMessage = "401", AccountAuthStatusReauthRequired, "login required"
	testutil.NoError(t, s.UpdateAccount(ctx, admin))
	testutil.NoError(t, s.IncrementAccountStats(ctx, acc.ID, 5, 3))
	testutil.NoError(t, s.UpdateWarpCredentials(ctx, acc.ID, WarpCredentialPatch{
		ExpectedRefreshToken: acc.WarpRefreshToken, AccessToken: "rotated-access", RefreshToken: "rotated-refresh", ExpiresAt: at.Add(2 * time.Hour),
	}))
	before, err := s.GetAccount(ctx, acc.ID)
	testutil.NoError(t, err)
	quota := testWarpQuota(at.Add(time.Hour))
	testutil.Error(t, s.UpdateWarpQuota(ctx, acc.ID, acc.WarpRefreshToken, quota))
	got, err := s.GetAccount(ctx, acc.ID)
	testutil.NoError(t, err)
	testutil.True(t, reflect.DeepEqual(got, before), "rejected quota write changed the account")
	for _, expected := range []string{"", " rotated-refresh ", "other-refresh"} {
		testutil.Error(t, s.UpdateWarpQuota(ctx, acc.ID, expected, quota))
	}
	testutil.NoError(t, s.UpdateWarpQuota(ctx, acc.ID, before.WarpRefreshToken, quota))
	got, err = s.GetAccount(ctx, acc.ID)
	testutil.NoError(t, err)
	want := before.Clone()
	want.WarpQuota, want.UpdatedAt = quota, got.UpdatedAt
	testutil.True(t, reflect.DeepEqual(got, want), "quota write changed non-quota state")
	enabled, err := s.GetEnabledAccounts(ctx)
	testutil.NoError(t, err)
	testutil.Equal(t, len(enabled), 0)
}

func TestWarpQuotaPreservesPartialZeroUnknownAndSeparateWindows(t *testing.T) {
	s, _ := newWarpTestStore(t)
	at := time.Date(2030, 1, 2, 0, 0, 0, 0, time.UTC)
	acc := &Account{AccountType: "warp", WarpRefreshToken: "quota-refresh"}
	testutil.NoError(t, s.CreateAccount(t.Context(), acc))
	quota := WarpQuotaSnapshot{
		Windows: []WarpQuotaWindow{
			{Scope: "workspace_monthly", Unit: "credits", Limit: warpQuotaNumber(0), Remaining: warpQuotaNumber(0)},
			{Scope: "workspace_monthly", Unit: "credits", Unlimited: true, HasUnlimited: true},
			{Scope: "legacy_requests", Unit: "requests", Limit: warpQuotaNumber(5), Used: warpQuotaNumber(8), Remaining: warpQuotaNumber(0), HasUnlimited: true},
		},
		SyncedAt: at, AttemptedAt: at, Status: "partial", Source: "graphql",
	}
	testutil.NoError(t, s.UpdateWarpQuota(t.Context(), acc.ID, acc.WarpRefreshToken, quota))
	got, err := s.GetAccount(t.Context(), acc.ID)
	testutil.NoError(t, err)
	testutil.True(t, reflect.DeepEqual(got.WarpQuota, quota), "windows were aggregated or unknown/zero was lost")
	raw, err := json.Marshal(got.WarpQuota)
	testutil.NoError(t, err)
	text := string(raw)
	testutil.MustContain(t, text, `"limit":0`)
	testutil.MustContain(t, text, `"limit":null`)
	testutil.MustNotContain(t, text, "workspace_uid")
	testutil.MustNotContain(t, text, "workspace_name")
	for _, typ := range []reflect.Type{reflect.TypeFor[WarpQuotaSnapshot](), reflect.TypeFor[WarpQuotaWindow]()} {
		for i := range typ.NumField() {
			name := strings.ToLower(typ.Field(i).Name)
			testutil.False(t, strings.Contains(name, "uid") || strings.Contains(name, "name"), "quota has a workspace identity field")
		}
	}
}

func TestWarpQuotaAttemptOrderAndErrorRetainLastKnown(t *testing.T) {
	s, _ := newWarpTestStore(t)
	at := time.Date(2030, 1, 2, 0, 0, 0, 0, time.UTC)
	acc := &Account{AccountType: "warp", WarpRefreshToken: "quota-refresh", WarpQuota: testWarpQuota(at)}
	testutil.NoError(t, s.CreateAccount(t.Context(), acc))
	failure := WarpQuotaSnapshot{Status: "error", AttemptedAt: at.Add(time.Hour), Source: "graphql"}
	// Error payloads cannot replace the previous successful reading, even if
	// they happen to carry windows or a new sync date.
	failure.Windows, failure.SyncedAt = testWarpQuota(failure.AttemptedAt).Windows, failure.AttemptedAt
	*failure.Windows[0].Remaining = 0
	testutil.NoError(t, s.UpdateWarpQuota(t.Context(), acc.ID, acc.WarpRefreshToken, failure))
	got, err := s.GetAccount(t.Context(), acc.ID)
	testutil.NoError(t, err)
	want := acc.WarpQuota.Clone()
	want.Status, want.AttemptedAt = "error", failure.AttemptedAt
	testutil.True(t, reflect.DeepEqual(got.WarpQuota, want), "error erased the last known quota")
	before := got.Clone()
	for _, stamp := range []time.Time{at, failure.AttemptedAt} {
		testutil.NoError(t, s.UpdateWarpQuota(t.Context(), acc.ID, acc.WarpRefreshToken, testWarpQuota(stamp)))
		got, err = s.GetAccount(t.Context(), acc.ID)
		testutil.NoError(t, err)
		testutil.True(t, reflect.DeepEqual(got, before), "older/equal attempt changed the account")
	}
	recovered := testWarpQuota(at.Add(2 * time.Hour))
	testutil.NoError(t, s.UpdateWarpQuota(t.Context(), acc.ID, acc.WarpRefreshToken, recovered))
	got, err = s.GetAccount(t.Context(), acc.ID)
	testutil.NoError(t, err)
	testutil.True(t, reflect.DeepEqual(got.WarpQuota, recovered), "new successful observation did not recover")
	// A first failed attempt stores its status without inventing quota values.
	empty := &Account{AccountType: "warp"}
	testutil.NoError(t, s.CreateAccount(t.Context(), empty))
	failure.Windows, failure.SyncedAt = nil, time.Time{}
	testutil.NoError(t, s.UpdateWarpQuota(t.Context(), empty.ID, "", failure))
	got, err = s.GetAccount(t.Context(), empty.ID)
	testutil.NoError(t, err)
	testutil.True(t, got.WarpQuota.Windows == nil && got.WarpQuota.SyncedAt.IsZero(), "first error invented a reading")
}

func TestWarpQuotaGenericUpdatesDoNotRewindSnapshot(t *testing.T) {
	s, _ := newWarpTestStore(t)
	at := time.Date(2030, 1, 2, 0, 0, 0, 0, time.UTC)
	acc := &Account{AccountType: "warp", Enabled: true, WarpRefreshToken: "quota-refresh", WarpQuota: testWarpQuota(at)}
	testutil.NoError(t, s.CreateAccount(t.Context(), acc))
	stale := acc.Clone()
	newer := testWarpQuota(at.Add(time.Hour))
	testutil.NoError(t, s.UpdateWarpQuota(t.Context(), acc.ID, acc.WarpRefreshToken, newer))
	for _, patch := range []*Account{stale, {ID: acc.ID, AccountType: "warp"}} {
		patch.Enabled = false
		testutil.NoError(t, s.UpdateAccount(t.Context(), patch))
		got, err := s.GetAccount(t.Context(), acc.ID)
		testutil.NoError(t, err)
		testutil.False(t, got.Enabled, "admin disable was not applied")
		testutil.True(t, reflect.DeepEqual(got.WarpQuota, newer), "generic update rewound quota")
	}
	current, err := s.GetAccount(t.Context(), acc.ID)
	testutil.NoError(t, err)
	// Even a current account timestamp cannot authorize an older quota read.
	current.WarpQuota = testWarpQuota(at)
	testutil.NoError(t, s.UpdateAccount(t.Context(), current))
	current.WarpQuota = WarpQuotaSnapshot{Status: "error", AttemptedAt: at.Add(2 * time.Hour), Source: "graphql"}
	testutil.NoError(t, s.UpdateAccount(t.Context(), current))
	got, err := s.GetAccount(t.Context(), acc.ID)
	testutil.NoError(t, err)
	want := newer.Clone()
	want.Status, want.AttemptedAt = "error", current.WarpQuota.AttemptedAt
	testutil.True(t, reflect.DeepEqual(got.WarpQuota, want), "generic error update lost last-known windows")
	// A successful reading newer than the failed attempt may replace it.
	stale.WarpQuota = testWarpQuota(at.Add(3 * time.Hour))
	testutil.NoError(t, s.UpdateAccount(t.Context(), stale))
	got, err = s.GetAccount(t.Context(), acc.ID)
	testutil.NoError(t, err)
	testutil.True(t, reflect.DeepEqual(got.WarpQuota, stale.WarpQuota), "newer generic quota was not applied")
}

func TestWarpQuotaCloneIsolation(t *testing.T) {
	at := time.Date(2030, 1, 2, 0, 0, 0, 0, time.UTC)
	acc := &Account{WarpQuota: testWarpQuota(at)}
	want := acc.WarpQuota.Clone()
	clone := acc.Clone()
	window := &clone.WarpQuota.Windows[0]
	window.Scope, window.Unit = "legacy_requests", "requests"
	for _, value := range []*float64{window.Limit, window.Used, window.Remaining, window.BonusRemaining} {
		*value = 999
	}
	clone.WarpQuota.Windows = append(clone.WarpQuota.Windows, WarpQuotaWindow{})
	testutil.True(t, reflect.DeepEqual(acc.WarpQuota, want), "cloning shared mutable quota data")
	unknown := WarpQuotaSnapshot{}.Clone()
	testutil.True(t, unknown.Windows == nil, "clone turned unknown windows into an empty observation")
	empty := (WarpQuotaSnapshot{Windows: []WarpQuotaWindow{}}).Clone()
	testutil.True(t, empty.Windows != nil, "clone lost an explicit empty observation")
}

func TestWarpQuotaValidationRejectsMalformedSnapshots(t *testing.T) {
	at := time.Date(2030, 1, 2, 0, 0, 0, 0, time.UTC)
	invalidDate := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	mutations := map[string]func(*WarpQuotaSnapshot){
		"status":             func(q *WarpQuotaSnapshot) { q.Status = "healthy" },
		"missing status":     func(q *WarpQuotaSnapshot) { q.Status = "" },
		"scope":              func(q *WarpQuotaSnapshot) { q.Windows[0].Scope = "private-workspace-identity" },
		"unit":               func(q *WarpQuotaSnapshot) { q.Windows[0].Unit = "dollars" },
		"attempt date":       func(q *WarpQuotaSnapshot) { q.AttemptedAt = invalidDate },
		"sync date":          func(q *WarpQuotaSnapshot) { q.SyncedAt = invalidDate },
		"reset date":         func(q *WarpQuotaSnapshot) { q.Windows[0].ResetAt = invalidDate },
		"sync after attempt": func(q *WarpQuotaSnapshot) { q.SyncedAt = at.Add(time.Hour) },
		"negative limit":     func(q *WarpQuotaSnapshot) { q.Windows[0].Limit = warpQuotaNumber(-1) },
		"negative used":      func(q *WarpQuotaSnapshot) { q.Windows[0].Used = warpQuotaNumber(-1) },
		"negative remaining": func(q *WarpQuotaSnapshot) { q.Windows[0].Remaining = warpQuotaNumber(-1) },
		"negative bonus":     func(q *WarpQuotaSnapshot) { q.Windows[0].BonusRemaining = warpQuotaNumber(-1) },
		"nan":                func(q *WarpQuotaSnapshot) { q.Windows[0].Remaining = warpQuotaNumber(math.NaN()) },
		"positive infinity":  func(q *WarpQuotaSnapshot) { q.Windows[0].Used = warpQuotaNumber(math.Inf(1)) },
		"negative infinity":  func(q *WarpQuotaSnapshot) { q.Windows[0].Limit = warpQuotaNumber(math.Inf(-1)) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			s, _ := newWarpTestStore(t)
			acc := &Account{AccountType: "warp", WarpQuota: testWarpQuota(at)}
			testutil.NoError(t, s.CreateAccount(t.Context(), acc))
			before, err := s.GetAccount(t.Context(), acc.ID)
			testutil.NoError(t, err)
			invalid := testWarpQuota(at)
			mutate(&invalid)
			testutil.Error(t, invalid.Validate())
			testutil.Error(t, s.UpdateWarpQuota(t.Context(), acc.ID, "", invalid))
			patch := before.Clone()
			patch.WarpQuota = invalid
			testutil.Error(t, s.UpdateAccount(t.Context(), patch))
			testutil.Error(t, s.CreateAccount(t.Context(), &Account{AccountType: "warp", WarpQuota: invalid}))
			got, err := s.GetAccount(t.Context(), acc.ID)
			testutil.NoError(t, err)
			testutil.True(t, reflect.DeepEqual(got, before), "invalid quota mutated persisted account")
		})
	}
}

func TestWarpQuotaConcurrentAttemptsKeepNewest(t *testing.T) {
	s, _ := newWarpTestStore(t)
	at := time.Date(2030, 1, 2, 0, 0, 0, 0, time.UTC)
	acc := &Account{AccountType: "warp", WarpRefreshToken: "quota-refresh", Enabled: false}
	testutil.NoError(t, s.CreateAccount(t.Context(), acc))
	start := make(chan struct{})
	errs := make(chan error, 8)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			<-start
			errs <- s.UpdateWarpQuota(t.Context(), acc.ID, acc.WarpRefreshToken, testWarpQuota(at.Add(time.Duration(i)*time.Hour)))
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		testutil.NoError(t, err)
	}
	got, err := s.GetAccount(t.Context(), acc.ID)
	testutil.NoError(t, err)
	testutil.True(t, reflect.DeepEqual(got.WarpQuota, testWarpQuota(at.Add(7*time.Hour))), "concurrent attempt rewound quota")
	testutil.False(t, got.Enabled, "concurrent quota update enabled account")
}

func TestWarpQuotaRequiresConfiguredStore(t *testing.T) {
	var absent *Store
	for _, s := range []*Store{absent, {}} {
		testutil.Error(t, s.UpdateWarpQuota(t.Context(), 1, "", WarpQuotaSnapshot{}))
	}
	redisStore, _ := newWarpTestStore(t)
	testutil.Error(t, redisStore.UpdateWarpQuota(t.Context(), 1, "", WarpQuotaSnapshot{Status: "unknown"}))
}
