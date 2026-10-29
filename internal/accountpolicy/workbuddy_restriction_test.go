package accountpolicy

import (
	"errors"
	"testing"
	"time"

	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func TestWorkBuddyRestrictionSurvivesHealthyVerdictsWithoutExtendingWindow(t *testing.T) {
	at := time.Now()
	acc := &store.Account{AccountType: "workbuddy"}
	v := Classify(acc, errors.New(store.WorkBuddyRestrictionMessage), "m")
	v.At = at
	v.Apply(acc)
	until := at.Add(store.WorkBuddyRestrictionCooldown)
	testutil.True(t, acc.WorkBuddyRestrictedUntil.Equal(until))
	// Catalog verification does not prove chat was unblocked.
	Success(at.Add(time.Hour)).Apply(acc)
	testutil.Equal(t, acc.StatusCode, "403")
	testutil.True(t, AccountHeld(acc, until.Add(-time.Second)))
	testutil.False(t, AccountHeld(acc, until))
	v.At = at.Add(2 * time.Hour)
	v.Apply(acc)
	testutil.True(t, acc.WorkBuddyRestrictedUntil.Equal(until))
	// A confirmed new refusal after expiry starts the next bounded window.
	v.At = until.Add(time.Second)
	v.Apply(acc)
	testutil.True(t, acc.WorkBuddyRestrictedUntil.Equal(v.At.Add(store.WorkBuddyRestrictionCooldown)))
}
