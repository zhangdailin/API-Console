package store

import (
	"testing"
	"time"
)

func TestWarpAuthFailurePatchPreservesUnrelatedFields(t *testing.T) {
	s, _ := newWarpTestStore(t)
	acc := &Account{AccountType: "warp", Enabled: false, Name: "admin-disabled", Weight: 9, WarpAccessToken: "synthetic-access", WarpRefreshToken: "synthetic-refresh", WarpExpiresAt: time.Now().Add(-time.Hour), WarpModelIDs: []string{"model-a"}, WarpModelsSyncedAt: time.Now()}
	if err := s.CreateAccount(t.Context(), acc); err != nil {
		t.Fatal(err)
	}
	patch := WarpAuthFailurePatch{ExpectedRefreshToken: acc.WarpRefreshToken, ExpectedAccessToken: acc.WarpAccessToken, ExpectedExpiresAt: acc.WarpExpiresAt, Message: "401: Warp refresh token rejected; sign in again", ObservedAt: time.Now()}
	if err := s.UpdateWarpAuthFailure(t.Context(), acc.ID, patch); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAccount(t.Context(), acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if AccountAuthActive(got) || got.StatusCode != "401" || got.StatusMessage != patch.Message || !got.VerifiedAt.Equal(patch.ObservedAt) {
		t.Fatal("terminal verdict was not persisted")
	}
	if got.Enabled || got.Name != acc.Name || got.Weight != acc.Weight || len(got.WarpModelIDs) != 1 || got.WarpAccessToken != acc.WarpAccessToken || got.WarpRefreshToken != acc.WarpRefreshToken {
		t.Fatal("auth verdict overwrote administrator settings or credentials")
	}
}

func TestWarpAuthFailurePatchRejectsChangedCredentialGeneration(t *testing.T) {
	for _, field := range []string{"refresh", "access", "expiry", "channel"} {
		t.Run(field, func(t *testing.T) {
			s, _ := newWarpTestStore(t)
			acc := &Account{AccountType: "warp", Enabled: true, WarpAccessToken: "synthetic-access", WarpRefreshToken: "synthetic-refresh", WarpExpiresAt: time.Now().Add(time.Hour)}
			if err := s.CreateAccount(t.Context(), acc); err != nil {
				t.Fatal(err)
			}
			patch := WarpAuthFailurePatch{ExpectedRefreshToken: acc.WarpRefreshToken, ExpectedAccessToken: acc.WarpAccessToken, ExpectedExpiresAt: acc.WarpExpiresAt, ObservedAt: time.Now()}
			switch field {
			case "refresh":
				patch.ExpectedRefreshToken = "stale-refresh"
			case "access":
				patch.ExpectedAccessToken = "stale-access"
			case "expiry":
				patch.ExpectedExpiresAt = acc.WarpExpiresAt.Add(-time.Hour)
			case "channel":
				acc.AccountType = "cline"
				if err := s.UpdateAccount(t.Context(), acc); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.UpdateWarpAuthFailure(t.Context(), acc.ID, patch); err == nil {
				t.Fatal("stale auth verdict was applied")
			}
			got, err := s.GetAccount(t.Context(), acc.ID)
			if err != nil || !AccountAuthActive(got) || got.StatusCode != "" {
				t.Fatal("stale failure poisoned current account state")
			}
		})
	}
}

func TestWarpAuthFailurePatchRequiresConfiguredStoreAndEvidence(t *testing.T) {
	var absent *Store
	if absent.UpdateWarpAuthFailure(t.Context(), 1, WarpAuthFailurePatch{}) == nil || (&Store{}).UpdateWarpAuthFailure(t.Context(), 1, WarpAuthFailurePatch{}) == nil {
		t.Fatal("missing store accepted auth failure")
	}
	s, _ := newWarpTestStore(t)
	if s.UpdateWarpAuthFailure(t.Context(), 1, WarpAuthFailurePatch{}) == nil {
		t.Fatal("missing credential generation accepted")
	}
}
