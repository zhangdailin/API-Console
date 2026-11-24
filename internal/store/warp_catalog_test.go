package store

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"orchids-api/internal/testutil"
)

func TestWarpCatalogStaleSourcePreservesDisabledAccount(t *testing.T) {
	s, _ := newWarpTestStore(t)
	ctx := t.Context()
	observed := time.Date(2030, 1, 2, 0, 0, 0, 0, time.UTC)
	acc := &Account{
		AccountType: "warp", Name: "original", Enabled: true, Weight: 1,
		WarpAccessToken: "catalog-access", WarpRefreshToken: "catalog-refresh",
		WarpExpiresAt: observed.Add(24 * time.Hour),
		WarpModelIDs:  []string{"old-model"}, WarpModelsSyncedAt: observed,
		UsageCurrent: 3, UsageTotal: 10, RequestCount: 2,
	}
	testutil.NoError(t, s.CreateAccount(ctx, acc))
	stale := acc.Clone()
	admin := acc.Clone()
	admin.Enabled = false
	admin.Name = "disabled by operator"
	admin.Weight = 7
	admin.MaxConcurrent = 4
	admin.StatusCode = "401"
	admin.AuthStatus = AccountAuthStatusReauthRequired
	admin.StatusMessage = "operator requires login"
	testutil.NoError(t, s.UpdateAccount(ctx, admin))
	testutil.NoError(t, s.IncrementAccountStats(ctx, acc.ID, 5, 3))
	before, err := s.GetAccount(ctx, acc.ID)
	testutil.NoError(t, err)
	testutil.False(t, before.Enabled, "admin update did not disable account")
	testutil.Equal(t, before.RequestCount, int64(5))
	testutil.Equal(t, before.UsageTotal, float64(15))

	// Discovery still holds the enabled snapshot taken before the operator edit.
	stale.WarpModelIDs = []string{"new-model-a", "new-model-b"}
	stale.WarpModelsSyncedAt = observed.Add(time.Hour)
	testutil.True(t, stale.Enabled, "source snapshot must predate the disable")
	testutil.NoError(t, s.UpdateWarpCatalogSnapshot(ctx, stale.ID, WarpCatalogPatch{
		ExpectedRefreshToken: stale.WarpRefreshToken, ModelIDs: stale.WarpModelIDs, SyncedAt: stale.WarpModelsSyncedAt,
	}))
	got, err := s.GetAccount(ctx, acc.ID)
	testutil.NoError(t, err)
	testutil.False(t, got.Enabled, "catalog observation re-enabled a disabled account")
	testutil.True(t, slices.Equal(got.WarpModelIDs, stale.WarpModelIDs), "new catalog was not stored")
	testutil.True(t, got.WarpModelsSyncedAt.Equal(stale.WarpModelsSyncedAt), "new catalog timestamp was not stored")
	want := before.Clone()
	want.WarpModelIDs = slices.Clone(stale.WarpModelIDs)
	want.WarpModelsSyncedAt = stale.WarpModelsSyncedAt
	want.UpdatedAt = got.UpdatedAt
	// Compare the whole row, not only Enabled: a catalog owns no other state.
	testutil.True(t, reflect.DeepEqual(got, want), "catalog write changed non-catalog account fields")
	enabled, err := s.GetEnabledAccounts(ctx)
	testutil.NoError(t, err)
	testutil.Equal(t, len(enabled), 0)
}

func TestWarpCatalogRejectsRotatedCredential(t *testing.T) {
	s, _ := newWarpTestStore(t)
	ctx := t.Context()
	observed := time.Date(2030, 1, 2, 0, 0, 0, 0, time.UTC)
	acc := &Account{AccountType: "warp", Enabled: true, WarpAccessToken: "old-access", WarpRefreshToken: "old-refresh", WarpModelIDs: []string{"old-model"}, WarpModelsSyncedAt: observed}
	testutil.NoError(t, s.CreateAccount(ctx, acc))
	testutil.NoError(t, s.UpdateWarpCredentials(ctx, acc.ID, WarpCredentialPatch{
		ExpectedRefreshToken: acc.WarpRefreshToken,
		AccessToken:          "rotated-access", RefreshToken: "rotated-refresh", ExpiresAt: observed.Add(24 * time.Hour),
	}))
	before, err := s.GetAccount(ctx, acc.ID)
	testutil.NoError(t, err)
	patch := WarpCatalogPatch{
		ExpectedRefreshToken: acc.WarpRefreshToken, ModelIDs: []string{"new-model"}, SyncedAt: observed.Add(time.Hour),
	}
	err = s.UpdateWarpCatalogSnapshot(ctx, acc.ID, patch)
	testutil.Error(t, err)
	testutil.MustContain(t, err.Error(), "warp credential changed concurrently")
	got, err := s.GetAccount(ctx, acc.ID)
	testutil.NoError(t, err)
	testutil.True(t, reflect.DeepEqual(got, before), "rejected catalog write changed the account")
	// The same observation is accepted only when tied to the current credential.
	patch.ExpectedRefreshToken = before.WarpRefreshToken
	testutil.NoError(t, s.UpdateWarpCatalogSnapshot(ctx, acc.ID, patch))
	got, err = s.GetAccount(ctx, acc.ID)
	testutil.NoError(t, err)
	testutil.True(t, slices.Equal(got.WarpModelIDs, []string{"new-model"}), "current credential catalog was not stored")
	testutil.Equal(t, got.WarpAccessToken, before.WarpAccessToken)
	testutil.Equal(t, got.WarpRefreshToken, before.WarpRefreshToken)
}

func TestWarpCatalogIgnoresStaleAndEqualTimestamps(t *testing.T) {
	s, _ := newWarpTestStore(t)
	ctx := t.Context()
	observed := time.Date(2030, 1, 2, 0, 0, 0, 0, time.UTC)
	acc := &Account{AccountType: "warp", Enabled: false, WarpRefreshToken: "catalog-refresh", WarpModelIDs: []string{"current-model"}, WarpModelsSyncedAt: observed}
	testutil.NoError(t, s.CreateAccount(ctx, acc))
	before, err := s.GetAccount(ctx, acc.ID)
	testutil.NoError(t, err)
	for _, stamp := range []time.Time{observed.Add(-time.Hour), observed, {}} {
		testutil.NoError(t, s.UpdateWarpCatalogSnapshot(ctx, acc.ID, WarpCatalogPatch{
			ExpectedRefreshToken: acc.WarpRefreshToken, ModelIDs: []string{"stale-model"}, SyncedAt: stamp,
		}))
		got, err := s.GetAccount(ctx, acc.ID)
		testutil.NoError(t, err)
		testutil.True(t, reflect.DeepEqual(got, before), "non-newer catalog changed the account")
	}
}

func TestWarpCatalogRequiresExactExpectedCredential(t *testing.T) {
	s, _ := newWarpTestStore(t)
	ctx := t.Context()
	acc := &Account{AccountType: "warp", WarpRefreshToken: "catalog-refresh"}
	testutil.NoError(t, s.CreateAccount(ctx, acc))
	for _, expected := range []string{"", " catalog-refresh ", "other-refresh"} {
		err := s.UpdateWarpCatalogSnapshot(ctx, acc.ID, WarpCatalogPatch{
			ExpectedRefreshToken: expected, ModelIDs: []string{"model-a"}, SyncedAt: time.Date(2030, 1, 2, 0, 0, 0, 0, time.UTC),
		})
		testutil.Error(t, err)
		testutil.MustContain(t, err.Error(), "warp credential changed concurrently")
	}
}

func TestWarpCatalogSnapshotPersistsMetadataAtomically(t *testing.T) {
	s, _ := newWarpTestStore(t)
	ctx := t.Context()
	observed := time.Date(2030, 1, 2, 0, 0, 0, 0, time.UTC)
	acc := &Account{
		AccountType: "warp", Name: "catalog", Enabled: true, Weight: 1,
		WarpAccessToken: "catalog-access", WarpRefreshToken: "catalog-refresh",
		WarpExpiresAt: observed.Add(24 * time.Hour),
		WarpModelIDs:  []string{"old-model"}, WarpModelsSyncedAt: observed,
		WarpModelWindows: map[string]int{"old-model": 1000},
		WarpModelVision:  map[string]bool{"old-model": true},
		UsageCurrent:     3, UsageTotal: 10, RequestCount: 2,
	}
	testutil.NoError(t, s.CreateAccount(ctx, acc))
	before, err := s.GetAccount(ctx, acc.ID)
	testutil.NoError(t, err)

	newer := observed.Add(time.Hour)
	testutil.NoError(t, s.UpdateWarpCatalogSnapshot(ctx, acc.ID, WarpCatalogPatch{
		ExpectedRefreshToken: "catalog-refresh",
		ModelIDs:             []string{"model-b", "model-a"},
		ModelWindows:         map[string]int{"model-a": 200000, "model-b": 0},
		ModelVision:          map[string]bool{"model-a": true, "model-b": false, "foreign-model": true},
		SyncedAt:             newer,
	}))
	got, err := s.GetAccount(ctx, acc.ID)
	testutil.NoError(t, err)
	want := before.Clone()
	want.WarpModelIDs = []string{"model-b", "model-a"}
	want.WarpModelWindows = map[string]int{"model-a": 200000}
	want.WarpModelVision = map[string]bool{"model-a": true, "model-b": false}
	want.WarpModelsSyncedAt = newer
	want.UpdatedAt = got.UpdatedAt
	// Compare the whole row to detect writes outside the catalog snapshot.
	testutil.True(t, reflect.DeepEqual(got, want), "catalog metadata or account fields changed unexpectedly")

	// A stale or equal observation must not replace IDs and windows, because
	// they belong to the same catalog generation.
	for _, stamp := range []time.Time{observed.Add(-time.Hour), newer} {
		testutil.NoError(t, s.UpdateWarpCatalogSnapshot(ctx, acc.ID, WarpCatalogPatch{
			ExpectedRefreshToken: "catalog-refresh",
			ModelIDs:             []string{"stale-model"},
			ModelWindows:         map[string]int{"stale-model": 1},
			ModelVision:          map[string]bool{"stale-model": true},
			SyncedAt:             stamp,
		}))
		unchanged, err := s.GetAccount(ctx, acc.ID)
		testutil.NoError(t, err)
		testutil.True(t, reflect.DeepEqual(unchanged, got), "non-newer catalog changed the account")
	}

	// The same snapshot is rejected whole when the credential rotated after the
	// observation, so IDs and windows can never be written from mixed evidence.
	testutil.NoError(t, s.UpdateWarpCredentials(ctx, acc.ID, WarpCredentialPatch{
		ExpectedRefreshToken: "catalog-refresh", AccessToken: "rotated-access",
		RefreshToken: "rotated-refresh", ExpiresAt: observed.Add(48 * time.Hour),
	}))
	authoritative, err := s.GetAccount(ctx, acc.ID)
	testutil.NoError(t, err)
	err = s.UpdateWarpCatalogSnapshot(ctx, acc.ID, WarpCatalogPatch{
		ExpectedRefreshToken: "catalog-refresh",
		ModelIDs:             []string{"late-model"},
		ModelWindows:         map[string]int{"late-model": 5},
		ModelVision:          map[string]bool{"late-model": true},
		SyncedAt:             newer.Add(time.Hour),
	})
	testutil.Error(t, err)
	testutil.MustContain(t, err.Error(), "warp credential changed concurrently")
	rejected, err := s.GetAccount(ctx, acc.ID)
	testutil.NoError(t, err)
	testutil.True(t, reflect.DeepEqual(rejected, authoritative), "rejected catalog snapshot changed the account")
}

func TestWarpCatalogSnapshotClearsOlderMetadata(t *testing.T) {
	s, _ := newWarpTestStore(t)
	ctx := t.Context()
	observed := time.Date(2030, 1, 2, 0, 0, 0, 0, time.UTC)
	acc := &Account{
		AccountType: "warp", Enabled: false, WarpRefreshToken: "catalog-refresh",
		WarpModelIDs: []string{"old-model"}, WarpModelsSyncedAt: observed,
		WarpModelWindows: map[string]int{"old-model": 1000},
		WarpModelVision:  map[string]bool{"old-model": true},
	}
	testutil.NoError(t, s.CreateAccount(ctx, acc))
	// A newer catalog observation without window evidence clears the previous
	// generation's budgets instead of letting them outlive the rows they served.
	testutil.NoError(t, s.UpdateWarpCatalogSnapshot(ctx, acc.ID, WarpCatalogPatch{
		ExpectedRefreshToken: "catalog-refresh",
		ModelIDs:             []string{"new-model"},
		SyncedAt:             observed.Add(time.Hour),
	}))
	got, err := s.GetAccount(ctx, acc.ID)
	testutil.NoError(t, err)
	testutil.True(t, slices.Equal(got.WarpModelIDs, []string{"new-model"}), "catalog IDs were not replaced")
	testutil.True(t, got.WarpModelWindows == nil, "older catalog windows survived a newer observation without evidence")
	testutil.True(t, got.WarpModelVision == nil, "older vision capabilities survived a newer observation without evidence")
	testutil.False(t, got.Enabled, "catalog snapshot re-enabled a disabled account")
}

func TestWarpCatalogSnapshotRequiresConfiguredStore(t *testing.T) {
	patch := WarpCatalogPatch{ModelIDs: []string{"model-a"}, SyncedAt: time.Now()}
	var absent *Store
	testutil.Error(t, absent.UpdateWarpCatalogSnapshot(t.Context(), 1, patch))
	testutil.Error(t, (&Store{}).UpdateWarpCatalogSnapshot(t.Context(), 1, patch))
}

func TestWarpAccountUpdateMergesWindowsWithCatalogGeneration(t *testing.T) {
	for _, tc := range []struct {
		name     string
		rotate   bool
		syncedAt time.Time
		windows  map[string]int
		want     map[string]int
	}{
		{name: "matched_newer_replaces", syncedAt: time.Date(2030, 1, 2, 1, 0, 0, 0, time.UTC), windows: map[string]int{"incoming-model": 2}, want: map[string]int{"incoming-model": 2}},
		{name: "matched_equal_nil_preserves", syncedAt: time.Date(2030, 1, 2, 0, 0, 0, 0, time.UTC), want: map[string]int{"old-model": 1}},
		{name: "matched_newer_nil_clears", syncedAt: time.Date(2030, 1, 2, 1, 0, 0, 0, time.UTC), want: nil},
		{name: "stale_generation_rejected", rotate: true, syncedAt: time.Date(2030, 1, 2, 2, 0, 0, 0, time.UTC), windows: map[string]int{"incoming-model": 3}, want: map[string]int{"old-model": 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newWarpTestStore(t)
			ctx := t.Context()
			observed := time.Date(2030, 1, 2, 0, 0, 0, 0, time.UTC)
			seed := &Account{
				AccountType: "warp", Enabled: true, WarpRefreshToken: "original-refresh",
				WarpAccessToken: "original-access", WarpExpiresAt: observed.Add(time.Hour),
				WarpModelIDs: []string{"old-model"}, WarpModelsSyncedAt: observed,
				WarpModelWindows: map[string]int{"old-model": 1},
			}
			testutil.NoError(t, s.CreateAccount(ctx, seed))
			existing, err := s.GetAccount(ctx, seed.ID)
			testutil.NoError(t, err)
			if tc.rotate {
				testutil.NoError(t, s.UpdateWarpCredentials(ctx, seed.ID, WarpCredentialPatch{
					ExpectedRefreshToken: "original-refresh", AccessToken: "rotated-access",
					RefreshToken: "rotated-refresh", ExpiresAt: observed.Add(2 * time.Hour),
				}))
			}
			incoming := existing.Clone()
			incoming.WarpModelsSyncedAt = tc.syncedAt
			incoming.WarpModelWindows = tc.windows
			if tc.windows != nil {
				incoming.WarpModelIDs = []string{"incoming-model"}
			}
			testutil.NoError(t, s.UpdateAccount(ctx, incoming))
			got, err := s.GetAccount(ctx, seed.ID)
			testutil.NoError(t, err)
			testutil.True(t, reflect.DeepEqual(got.WarpModelWindows, tc.want), "windows did not follow the catalog generation rule")
		})
	}
}

func TestWarpAccountCloneIsolatesModelMetadata(t *testing.T) {
	acc := &Account{WarpModelWindows: map[string]int{"model-a": 1000}, WarpModelVision: map[string]bool{"model-a": false}}
	clone := acc.Clone()
	clone.WarpModelWindows["model-a"] = 2000
	clone.WarpModelWindows["model-b"] = 1
	clone.WarpModelVision["model-a"] = true
	clone.WarpModelVision["model-b"] = false
	testutil.Equal(t, acc.WarpModelWindows["model-a"], 1000)
	testutil.Equal(t, len(acc.WarpModelWindows), 1)
	testutil.False(t, acc.WarpModelVision["model-a"], "clone changed original vision capability")
	testutil.Equal(t, len(acc.WarpModelVision), 1)
}
