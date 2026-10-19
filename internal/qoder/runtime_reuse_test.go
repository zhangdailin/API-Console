package qoder

import (
	"testing"

	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

// derivedAccount returns an account whose stored runtime pair was derived by a
// client, the state a login or a first request leaves behind.
func derivedAccount(t *testing.T) (*store.Account, RuntimeFields) {
	t.Helper()
	acc := signedTestAccount()
	acc.QoderRuntimeInfo, acc.QoderRuntimeKey, acc.QoderRuntimeIdentity = "", "", ""
	client := NewFromAccount(acc, nil)
	fields, err := client.ensureRuntimeFields(t.Context(), client.currentCredentials())
	testutil.NoError(t, err)
	client.CopyAccountState(acc)
	testutil.Equal(t, acc.QoderRuntimeIdentity, fields.Identity)
	return acc, fields
}

func TestRebuiltClientReusesStoredRuntimePair(t *testing.T) {
	acc, fields := derivedAccount(t)

	rebuilt := NewFromAccount(acc, nil)
	testutil.Equal(t, rebuilt.RuntimeFields(), fields)
	got, err := rebuilt.ensureRuntimeFields(t.Context(), rebuilt.currentCredentials())
	testutil.NoError(t, err)
	testutil.Equal(t, got, fields)
}

func TestStoredRuntimePairIsNotReusedForAnotherIdentity(t *testing.T) {
	cases := map[string]func(*store.Account){
		"organization": func(a *store.Account) { a.QoderOrganizationID = "other-org" },
		"tags":         func(a *store.Account) { a.QoderOrganizationTags = []string{"other-team"} },
		"data policy":  func(a *store.Account) { a.QoderDataPolicy = !a.QoderDataPolicy },
		"no digest":    func(a *store.Account) { a.QoderRuntimeIdentity = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			acc, _ := derivedAccount(t)
			mutate(acc)
			testutil.False(t, NewFromAccount(acc, nil).RuntimeFields().Complete(), "stale runtime pair was reused")
		})
	}
}

func TestStoredRuntimePairIsReplacedAfterCredentialRotation(t *testing.T) {
	acc, fields := derivedAccount(t)
	client := NewFromAccount(acc, nil)
	creds := client.currentCredentials()
	creds.AccessToken, creds.RefreshToken = "rotated-access", "rotated-refresh"
	client.storeCredentials(creds, false, "")

	got, err := client.ensureRuntimeFields(t.Context(), client.currentCredentials())
	testutil.NoError(t, err)
	testutil.False(t, got.EncryptUserInfo == fields.EncryptUserInfo, "rotation kept the previous runtime pair")
	testutil.Equal(t, got.Identity, fields.Identity)
}
