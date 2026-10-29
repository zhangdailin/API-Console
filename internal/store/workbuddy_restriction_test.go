package store

import (
	"context"
	"testing"
	"time"

	"orchids-api/internal/testutil"
)

func TestWorkBuddyRestrictionSurvivesStaleHealthAndCredentialWrites(t *testing.T) {
	s, _ := newTestRedisStore(t, "wb-restriction:")
	ctx := context.Background()
	acc := &Account{AccountType: "workbuddy", Enabled: true, WorkBuddyRefreshToken: "old"}
	testutil.NoError(t, s.CreateAccount(ctx, acc))
	stale := *acc
	// Legacy production records acquire the original deadline on their next
	// write; an upgrade must not restart their cooldown.
	acc.StatusCode, acc.StatusMessage = "403", WorkBuddyRestrictionMessage
	acc.LastAttempt = time.Now().Add(-time.Hour)
	until := acc.LastAttempt.Add(WorkBuddyRestrictionCooldown)
	testutil.NoError(t, s.UpdateAccount(ctx, acc))
	stale.ClearVerifiedAt = true
	testutil.NoError(t, s.UpdateAccount(ctx, &stale))
	testutil.NoError(t, s.UpdateWorkBuddyCredentials(ctx, acc.ID, WorkBuddyCredentialPatch{
		ExpectedRefreshToken: "old", AccessToken: "new-access", RefreshToken: "new-refresh",
	}))
	after, err := s.GetAccount(ctx, acc.ID)
	testutil.NoError(t, err)
	testutil.True(t, after.WorkBuddyRestrictedUntil.Equal(until))
	testutil.Equal(t, after.StatusCode, "403")
	testutil.Equal(t, after.WorkBuddyRefreshToken, "new-refresh")
	// Concurrent failures must not keep extending the first active window.
	after.WorkBuddyRestrictedUntil = time.Now().Add(WorkBuddyRestrictionCooldown)
	testutil.NoError(t, s.UpdateAccount(ctx, after))
	after, err = s.GetAccount(ctx, acc.ID)
	testutil.NoError(t, err)
	testutil.True(t, after.WorkBuddyRestrictedUntil.Equal(until))
}
