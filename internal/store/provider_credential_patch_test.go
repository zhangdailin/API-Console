package store

import (
	"context"
	"orchids-api/internal/testutil"
	"testing"
	"time"
)

func TestProviderCredentialPatchesPreserveConcurrentAccountFields(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := newTestRedisStore(t, "provider-patch:")

	wb := &Account{
		AccountType: "workbuddy", Enabled: true, Name: "before", StatusCode: "429",
		UsageCurrent: 17, WorkBuddyAccessToken: "access-old", WorkBuddyRefreshToken: "refresh-old",
	}
	testutil.NoError(t, s.CreateAccount(ctx, wb))
	admin := *wb
	admin.Name = "after"
	admin.StatusCode = ""
	admin.WorkBuddyRefreshToken = "stale-refresh"
	testutil.NoError(t, s.UpdateAccount(ctx, &admin))
	if err := s.UpdateWorkBuddyCredentials(ctx, wb.ID, WorkBuddyCredentialPatch{
		ExpectedRefreshToken: "refresh-old",
		AccessToken:          "access-new",
		RefreshToken:         "refresh-new",
		ExpiresAt:            time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	storedWB, err := s.GetAccount(ctx, wb.ID)
	testutil.NoError(t, err)
	testutil.Equal(t, storedWB.Name, "after")
	testutil.Equal(t, storedWB.UsageCurrent, 17)
	testutil.Equal(t, storedWB.WorkBuddyRefreshToken, "refresh-new")
	testutil.Equal(t, storedWB.ClientCookie, "")
	testutil.Equal(t, storedWB.Token, "")
	testutil.Equal(t, storedWB.RefreshToken, "")

}

func TestGrokCredentialPatchPreservesEditsAndRejectsStaleRotation(t *testing.T) {
	s, _ := newTestRedisStore(t, "grok-patch:")
	ctx := context.Background()
	acc := &Account{AccountType: "grok", Name: "admin-name", Enabled: true, OAuthRefreshToken: "old", OAuthAccessToken: "old-access", UsageLimit: 91}
	if err := s.CreateAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}
	patch := GrokCredentialPatch{ExpectedRefreshToken: "old", AccessToken: "new", RefreshToken: "rotated", ExpiresAt: time.Now().Add(time.Hour), Name: "oauth-name"}
	if err := s.UpdateGrokCredentials(ctx, acc.ID, patch); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAccount(ctx, acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "admin-name" || got.UsageLimit != 91 || got.OAuthRefreshToken != "rotated" {
		t.Fatalf("rotation overwrote unrelated state: name=%s limit=%v", got.Name, got.UsageLimit)
	}
	patch.RefreshToken = "stale-rotation"
	if err := s.UpdateGrokCredentials(ctx, acc.ID, patch); err == nil {
		t.Fatal("stale rotation accepted")
	}
}

func TestClineCredentialPatchPreservesEditsAndRejectsStaleRotation(t *testing.T) {
	s, _ := newTestRedisStore(t, "cline-patch:")
	ctx := context.Background()
	acc := &Account{AccountType: "cline", Name: "original", Enabled: true, ClineRefreshToken: "old", ClineAccessToken: "old-access"}
	testutil.NoError(t, s.CreateAccount(ctx, acc))
	admin := *acc
	admin.Name = "edited"
	admin.Enabled = false
	testutil.NoError(t, s.UpdateAccount(ctx, &admin))
	patch := ClineCredentialPatch{ExpectedRefreshToken: "old", AccessToken: "new", RefreshToken: "rotated", ModelIDs: []string{"model-a"}}
	testutil.NoError(t, s.UpdateClineCredentials(ctx, acc.ID, patch))
	got, err := s.GetAccount(ctx, acc.ID)
	testutil.NoError(t, err)
	testutil.Equal(t, got.Name, "edited")
	testutil.False(t, got.Enabled, "credential rotation re-enabled a disabled account")
	testutil.Equal(t, got.ClineRefreshToken, "rotated")
	patch.RefreshToken = "stale-rotation"
	testutil.Error(t, s.UpdateClineCredentials(ctx, acc.ID, patch))
	got, err = s.GetAccount(ctx, acc.ID)
	testutil.NoError(t, err)
	testutil.Equal(t, got.ClineRefreshToken, "rotated")
}
