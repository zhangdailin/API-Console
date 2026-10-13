package grok

import (
	"testing"
	"time"

	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func TestCLIBillingNeedsSyncUsesSlowCadence(t *testing.T) {
	now := time.Now()
	testutil.False(t, cliBillingNeedsSync(&store.Account{GrokBilling: store.GrokBillingSnapshot{SyncedAt: now.Add(-time.Minute)}}, now), "fresh CLI billing was polled on credential tick")
	testutil.True(t, cliBillingNeedsSync(&store.Account{GrokBilling: store.GrokBillingSnapshot{SyncedAt: now.Add(-time.Hour)}}, now), "stale CLI billing was not polled")
}

// TestCatalogEligibleRequiresAUsableBuildCredential keeps a model refresh from
// counting an account that has nothing to present as an active one.
func TestCatalogEligibleRequiresAUsableBuildCredential(t *testing.T) {
	testutil.False(t, catalogEligible(nil), "a nil account is not eligible")
	testutil.False(t, catalogEligible(&store.Account{AccountType: "qoder", CredentialType: "oauth", OAuthAccessToken: "t"}), "another channel's account is not eligible")
	testutil.False(t, catalogEligible(&store.Account{AccountType: "grok", OAuthAccessToken: "t"}), "a non-OAuth credential is not eligible")
	testutil.False(t, catalogEligible(&store.Account{AccountType: "grok", CredentialType: "oauth"}), "an account without a token is not eligible")
	testutil.True(t, catalogEligible(&store.Account{AccountType: "grok", CredentialType: "oauth", OAuthRefreshToken: "r"}), "a refresh token alone is enough to read the catalog")
}
