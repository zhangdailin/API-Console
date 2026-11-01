package api

import (
	"context"
	"testing"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func TestFindDuplicateAccountUsesStableProviderIdentityAfterTokenRotation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		existing *store.Account
		login    *store.Account
	}{
		{
			name:     "workbuddy uid",
			existing: &store.Account{AccountType: "workbuddy", WorkBuddyUID: "wb-user", WorkBuddyRefreshToken: "old-refresh", Enabled: true},
			login:    &store.Account{AccountType: "workbuddy", WorkBuddyUID: "wb-user", WorkBuddyRefreshToken: "new-refresh", Enabled: true},
		},
		{
			name:     "grok oauth user id",
			existing: &store.Account{AccountType: "grok", CredentialType: "oauth", UserID: "grok-user", OAuthRefreshToken: "old-refresh", Enabled: true},
			login:    &store.Account{AccountType: "grok", CredentialType: "oauth", UserID: "grok-user", OAuthRefreshToken: "new-refresh", Enabled: true},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newTestStore(t, "identity-dedup:")
			testutil.NoError(t, s.CreateAccount(context.Background(), tc.existing))
			a := New(s, "", "", &config.Config{})
			got, err := a.findDuplicateAccountByCredential(context.Background(), tc.login, 0)
			testutil.NoError(t, err)
			testutil.Falsef(t, got == nil || got.ID != tc.existing.ID, "duplicate = %+v, want account %d", got, tc.existing.ID)
		})
	}
}

func TestStableProviderIdentityNeverCrossesProviders(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t, "identity-isolation:")
	existing := &store.Account{AccountType: "workbuddy", WorkBuddyUID: "same-user", WorkBuddyRefreshToken: "wb-token", Enabled: true}
	testutil.NoError(t, s.CreateAccount(context.Background(), existing))
	a := New(s, "", "", &config.Config{})
	got, err := a.findDuplicateAccountByCredential(context.Background(), &store.Account{
		AccountType:       "grok",
		CredentialType:    "oauth",
		UserID:            "same-user",
		OAuthRefreshToken: "g-token",
	}, 0)
	testutil.NoError(t, err)
	testutil.Falsef(t, got != nil, "cross-provider account was treated as duplicate: %+v", got)
}
