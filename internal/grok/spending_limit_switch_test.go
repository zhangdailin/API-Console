package grok

import (
	"errors"
	"testing"

	"orchids-api/internal/testutil"
)

// productionSpendingLimit is the refusal account 73 returned on 2026-10-21.
const productionSpendingLimit = `grok cli upstream status=402 body={"code":"personal-team-blocked:spending-limit","error":"You have run out of credits or need a Grok subscription. Add credits at https://grok.com/?_s=usage or upgrade at https://grok.com/supergrok."}`

// TestSpentAccountSwitches: one account's spent credits must not end the
// request while other accounts can serve it. The request answered 429 after a
// single attempt with twenty-three healthy accounts in the pool.
func TestSpentAccountSwitches(t *testing.T) {
	testutil.False(t, !shouldSwitchGrokAccount(errors.New(productionSpendingLimit)), "a spent account did not switch")
	testutil.False(t, !markAllGrokAccountStatuses(errors.New(productionSpendingLimit)), "a spent account was not marked")
}

// TestTokenRenewalKeepsNonCredentialStatuses: a renewed token resolves an
// authentication refusal only; it used to clear a 402 as well.
func TestTokenRenewalKeepsNonCredentialStatuses(t *testing.T) {
	testutil.False(t, !credentialStatus("401"), "a renewed token must clear a 401")
	testutil.False(t, !credentialStatus(""), "nothing to keep")
	for _, status := range []string{"402", "429", "403"} {
		testutil.False(t, credentialStatus(status), "a renewed token cleared "+status)
	}
}
