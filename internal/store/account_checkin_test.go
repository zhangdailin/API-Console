package store

import (
	"context"
	"testing"
	"time"

	"orchids-api/internal/testutil"
)

// The opt-in survives a partial update that never mentions it, an explicit
// value replaces it, and an ordinary update never touches the outcome.
func TestAutoCheckinMergeAndOutcomeRecording(t *testing.T) {
	s := newBatchReadStore(t)
	ctx := context.Background()
	testutil.NoError(t, s.client.Set(ctx, s.accountsKey(1), `{"id":1,"name":"q","account_type":"qoder","enabled":true}`, 0).Err())

	on := true
	testutil.NoError(t, s.UpdateAccount(ctx, &Account{ID: 1, Name: "q", AccountType: "qoder", Enabled: true, AutoCheckin: &on}))
	testutil.NoError(t, s.UpdateAccount(ctx, &Account{ID: 1, Name: "q", AccountType: "qoder", Enabled: true}))
	acc, err := s.GetAccount(ctx, 1)
	testutil.NoError(t, err)
	testutil.Falsef(t, !acc.AutoCheckinEnabled(), "a partial update erased the opt-in")

	now := time.Now().Truncate(time.Second)
	testutil.NoError(t, s.RecordAccountCheckin(ctx, 1, AccountCheckin{AttemptedAt: now, Status: "success", RewardCredits: 100}))
	testutil.NoError(t, s.RecordAccountCheckin(ctx, 1, AccountCheckin{AttemptedAt: now.Add(-time.Minute), Status: "failed"}))
	off := false
	testutil.NoError(t, s.UpdateAccount(ctx, &Account{ID: 1, Name: "q", AccountType: "qoder", Enabled: true, AutoCheckin: &off, Checkin: AccountCheckin{Status: "forged"}}))
	acc, err = s.GetAccount(ctx, 1)
	testutil.NoError(t, err)
	testutil.Falsef(t, acc.AutoCheckinEnabled(), "an explicit opt-out was ignored")
	testutil.Equal(t, acc.Checkin.Status, "success")
	testutil.Equal(t, acc.Checkin.RewardCredits, 100.0)
}
