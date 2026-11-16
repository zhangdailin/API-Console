package store

import (
	"reflect"
	"testing"
	"time"

	"orchids-api/internal/testutil"
)

func TestWarpGenericSaveRejectsNewerObservationsFromPreviousGeneration(t *testing.T) {
	for _, shape := range []string{"stale_snapshot", "current_account_timestamp", "partial_without_generation"} {
		t.Run(shape, func(t *testing.T) {
			ctx := t.Context()
			s, _ := newWarpTestStore(t)
			at := time.Date(2030, 1, 2, 0, 0, 0, 0, time.UTC)
			acc := &Account{
				AccountType: "warp", Enabled: true, WarpAccessToken: "synthetic-original-access",
				WarpRefreshToken: "synthetic-original-refresh", WarpExpiresAt: at.Add(time.Hour),
				WarpQuota: testWarpQuota(at), WarpModelIDs: []string{"original-model"}, WarpModelsSyncedAt: at,
			}
			testutil.NoError(t, s.CreateAccount(ctx, acc))
			stale := acc.Clone()
			testutil.NoError(t, s.UpdateWarpCredentials(ctx, acc.ID, WarpCredentialPatch{
				ExpectedRefreshToken: acc.WarpRefreshToken, AccessToken: "synthetic-current-access",
				RefreshToken: "synthetic-current-refresh", ExpiresAt: at.Add(2 * time.Hour),
			}))
			current, err := s.GetAccount(ctx, acc.ID)
			testutil.NoError(t, err)
			current.Enabled = false
			current.WarpQuota = testWarpQuota(at.Add(time.Hour))
			current.WarpModelIDs = []string{"current-model"}
			current.WarpModelsSyncedAt = at.Add(time.Hour)
			testutil.NoError(t, s.UpdateAccount(ctx, current))
			before, err := s.GetAccount(ctx, acc.ID)
			testutil.NoError(t, err)
			switch shape {
			case "current_account_timestamp":
				// Account freshness does not prove that these observations used
				// the currently stored credential generation.
				stale.UpdatedAt = before.UpdatedAt
			case "partial_without_generation":
				stale = &Account{ID: acc.ID, AccountType: "warp", Enabled: false}
			}
			stale.WarpQuota = testWarpQuota(at.Add(2 * time.Hour))
			stale.WarpModelIDs = []string{"old-login-late-model"}
			stale.WarpModelsSyncedAt = at.Add(2 * time.Hour)
			testutil.NoError(t, s.UpdateAccount(ctx, stale))
			got, err := s.GetAccount(ctx, acc.ID)
			testutil.NoError(t, err)
			// Generic operator fields have their own merge policy. Do not
			// require a full stale write to preserve Enabled or other fields.
			testutil.Equal(t, got.WarpAccessToken, before.WarpAccessToken)
			testutil.Equal(t, got.WarpRefreshToken, before.WarpRefreshToken)
			testutil.True(t, got.WarpExpiresAt.Equal(before.WarpExpiresAt), "generic save changed credential expiry")
			testutil.True(t, reflect.DeepEqual(got.WarpQuota, before.WarpQuota), "newer quota from an unmatched generation was accepted")
			testutil.True(t, reflect.DeepEqual(got.WarpModelIDs, before.WarpModelIDs), "newer catalog from an unmatched generation was accepted")
			testutil.True(t, got.WarpModelsSyncedAt.Equal(before.WarpModelsSyncedAt), "unmatched generation advanced catalog timestamp")
		})
	}
}

func TestWarpGenericSaveAcceptsMatchedGenerationOrExplicitReplacement(t *testing.T) {
	for _, replace := range []bool{false, true} {
		name := "matched_generation"
		if replace {
			name = "explicit_replacement"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			s, _ := newWarpTestStore(t)
			at := time.Date(2030, 1, 2, 0, 0, 0, 0, time.UTC)
			acc := &Account{
				AccountType: "warp", WarpAccessToken: "synthetic-current-access",
				WarpRefreshToken: "synthetic-current-refresh", WarpExpiresAt: at.Add(time.Hour),
				WarpQuota: testWarpQuota(at), WarpModelIDs: []string{"current-model"}, WarpModelsSyncedAt: at,
			}
			testutil.NoError(t, s.CreateAccount(ctx, acc))
			incoming := acc.Clone()
			incoming.ReplaceWarpCredentials = replace
			if replace {
				incoming.WarpAccessToken = "synthetic-replacement-access"
				incoming.WarpRefreshToken = "synthetic-replacement-refresh"
				incoming.WarpExpiresAt = at.Add(2 * time.Hour)
			}
			incoming.WarpQuota = testWarpQuota(at.Add(time.Hour))
			incoming.WarpModelIDs = []string{"new-observed-model"}
			incoming.WarpModelsSyncedAt = at.Add(time.Hour)
			testutil.NoError(t, s.UpdateAccount(ctx, incoming))
			got, err := s.GetAccount(ctx, acc.ID)
			testutil.NoError(t, err)
			testutil.Equal(t, got.WarpAccessToken, incoming.WarpAccessToken)
			testutil.Equal(t, got.WarpRefreshToken, incoming.WarpRefreshToken)
			testutil.True(t, got.WarpExpiresAt.Equal(incoming.WarpExpiresAt), "authorized generation lost credential expiry")
			testutil.True(t, reflect.DeepEqual(got.WarpQuota, incoming.WarpQuota), "authorized generation did not merge newer quota")
			testutil.True(t, reflect.DeepEqual(got.WarpModelIDs, incoming.WarpModelIDs), "authorized generation did not merge newer catalog")
			testutil.True(t, got.WarpModelsSyncedAt.Equal(incoming.WarpModelsSyncedAt), "authorized generation did not advance catalog timestamp")
			testutil.False(t, got.ReplaceWarpCredentials, "credential replacement intent escaped persistence")
		})
	}
}
