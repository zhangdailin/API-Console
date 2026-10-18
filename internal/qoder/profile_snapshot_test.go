package qoder

import (
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
	"testing"
)

func TestProfileUpdateInvalidatesRuntimeAndFinalizesLatestTokens(t *testing.T) {
	client := NewFromAccount(signedTestAccount(), nil)
	_, err := client.ensureRuntimeFields(t.Context(), client.currentCredentials())
	testutil.NoError(t, err)
	initial := client.RuntimeFields()
	client.ApplyProfile(Profile{UID: "updated-uid", Name: "updated-name", OrgID: "updated-org"})
	testutil.False(t, client.RuntimeFields().Complete(), "identity update retained old runtime ciphertext")
	creds := client.currentCredentials()
	testutil.Equal(t, creds.UID, "updated-uid")
	testutil.Equal(t, creds.OrgID, "updated-org")
	creds.AccessToken = "rotated-access"
	creds.RefreshToken = "rotated-refresh"
	client.storeCredentials(creds, false, "")
	var account store.Account
	testutil.NoError(t, client.FinalizeAccountState(t.Context(), &account))
	testutil.False(t, !client.runtimeTokensMatch(creds), "runtime derived from stale token pair")
	testutil.False(t, account.QoderAccessToken != creds.AccessToken || account.QoderRefreshToken != creds.RefreshToken || account.QoderUserID != creds.UID || account.QoderOrganizationID != creds.OrgID, "final account state does not match latest snapshot")
	testutil.False(t, account.QoderRuntimeInfo == initial.EncryptUserInfo || account.QoderRuntimeInfo == "", "runtime was not regenerated")
	testutil.False(t, account.QoderRuntimeInfo != client.RuntimeFields().EncryptUserInfo || account.QoderRuntimeKey != client.RuntimeFields().Key, "final account lacks regenerated runtime pair")
}

func TestOrganizationTagUpdateInvalidatesRuntime(t *testing.T) {
	client := NewFromAccount(signedTestAccount(), nil)
	_, err := client.ensureRuntimeFields(t.Context(), client.currentCredentials())
	testutil.NoError(t, err)
	initial := client.RuntimeFields()
	client.ApplyProfile(Profile{OrgTags: []string{"new-team"}})
	testutil.False(t, client.RuntimeFields().Complete(), "organization tag update retained stale runtime identity")
	_, err = client.ensureRuntimeFields(t.Context(), client.currentCredentials())
	testutil.NoError(t, err)
	testutil.False(t, client.RuntimeFields() == initial, "organization tag update did not regenerate runtime identity")
}
