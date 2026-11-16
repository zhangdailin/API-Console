package doubao

import (
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"testing"
	"time"

	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func challengeTestAccount() *store.Account {
	return &store.Account{
		ID: 9201, AccountType: "doubao", ClientCookie: `{"sessionid":"synthetic-session","s_v_web_id":"synthetic-fp"}`,
		DoubaoDeviceID: "1000000000000000001", DoubaoWebID: "1000000000000000002", DoubaoTeaUUID: "1000000000000000002",
		AuthStatus: store.AccountAuthStatusReauthRequired, StatusCode: "403", StatusMessage: "requires human verification",
	}
}

func challengeTestError() error {
	return &verificationRequired{
		code: "710022004", decision: sharkAdminDecision{From: "shark_admin", Type: "verify"},
		raw: `{"from":"shark_admin","type":"verify","detail":"synthetic-challenge"}`,
	}
}

func TestChallengeIdentityRotationDoesNotOfferOrClearOldDecision(t *testing.T) {
	for name, rotate := range map[string]func(*store.Account){
		"device": func(acc *store.Account) { acc.DoubaoDeviceID = "2000000000000000001" },
		"web":    func(acc *store.Account) { acc.DoubaoWebID = "2000000000000000002" },
		"tea":    func(acc *store.Account) { acc.DoubaoTeaUUID = "2000000000000000003" },
		"fp": func(acc *store.Account) {
			acc.ClientCookie = `{"sessionid":"synthetic-session","s_v_web_id":"rotated-fp"}`
		},
		"session": func(acc *store.Account) {
			acc.ClientCookie = `{"sessionid":"rotated-session","s_v_web_id":"synthetic-fp"}`
		},
	} {
		t.Run(name, func(t *testing.T) {
			original := challengeTestAccount()
			t.Cleanup(func() { clearChallenge(original.ID) })
			noteChallenge(original, challengeTestError())
			before, ok := verification(original)
			testutil.True(t, ok, "original challenge was not offered")
			rotated := original.Clone()
			rotate(rotated)
			_, ok = verification(rotated)
			testutil.False(t, ok, "old decision was offered for a rotated identity")
			testutil.Equal(t, rotated.AuthStatus, original.AuthStatus)
			testutil.Equal(t, rotated.StatusCode, original.StatusCode)
			testutil.Equal(t, rotated.StatusMessage, original.StatusMessage)

			after, ok := verification(original)
			testutil.True(t, ok, "identity mismatch cleared the original challenge")
			testutil.True(t, reflect.DeepEqual(after.Params, before.Params), "identity mismatch changed challenge parameters")
			testutil.Equal(t, after.RecordedAt, before.RecordedAt)

			// Rotation alone never creates a decision. A fresh refusal is required.
			noteChallenge(rotated, challengeTestError())
			fresh, ok := verification(rotated)
			testutil.True(t, ok, "fresh decision was not offered for the new identity")
			info := fresh.Params["info"].(map[string]any)
			testutil.EqualAny(t, info["did"], rotated.DoubaoDeviceID)
			testutil.EqualAny(t, info["fp"], fingerprintQuery(decodeCookies(rotated.ClientCookie)).Get("fp"))
			_, ok = verification(original)
			testutil.False(t, ok, "fresh decision was offered for the old identity")
		})
	}
}

func TestChallengeSnapshotKeepsOriginalDeviceAndCookieFingerprint(t *testing.T) {
	for name, cookie := range map[string]string{
		"with_fp":    `{"sessionid":"synthetic-session","s_v_web_id":"synthetic-fp"}`,
		"without_fp": `{"sessionid":"synthetic-session"}`,
	} {
		t.Run(name, func(t *testing.T) {
			acc := challengeTestAccount()
			acc.ClientCookie = cookie
			original := acc.Clone()
			t.Cleanup(func() { clearChallenge(acc.ID) })
			noteChallenge(acc, challengeTestError())
			acc.DoubaoDeviceID = "2000000000000000001"
			acc.ClientCookie = `{"sessionid":"other-session","s_v_web_id":"other-fp"}`

			got, ok := verification(original)
			testutil.True(t, ok, "mutating caller account changed recorded identity")
			info := got.Params["info"].(map[string]any)
			testutil.EqualAny(t, info["did"], original.DoubaoDeviceID)
			client := &Client{acc: original}
			jar := client.jar()
			query, err := url.ParseQuery(client.commonQuery(original, fingerprintQuery(jar)))
			testutil.NoError(t, err)
			testutil.EqualAny(t, info["fp"], query.Get("fp"))
			request := &http.Request{Header: client.headers(jar, false)}
			fp, err := request.Cookie("s_v_web_id")
			if name == "with_fp" {
				testutil.NoError(t, err)
				testutil.EqualAny(t, info["fp"], fp.Value)
			} else {
				testutil.True(t, errors.Is(err, http.ErrNoCookie), "unexpected fingerprint cookie")
				testutil.EqualAny(t, info["fp"], "")
			}
		})
	}
}

func TestChallengeRequiresPersistedRequestIdentity(t *testing.T) {
	for name, remove := range map[string]func(*store.Account){
		"account_id": func(acc *store.Account) { acc.ID = 0 },
		"device_id":  func(acc *store.Account) { acc.DoubaoDeviceID = "" },
		"web_id":     func(acc *store.Account) { acc.DoubaoWebID = "" },
	} {
		t.Run(name, func(t *testing.T) {
			acc := challengeTestAccount()
			remove(acc)
			t.Cleanup(func() { clearChallenge(acc.ID) })
			noteChallenge(acc, challengeTestError())
			_, ok := verification(acc)
			testutil.False(t, ok, "challenge was offered for an unstable identity")
			challenges.Lock()
			_, recorded := challenges.byAccount[acc.ID]
			challenges.Unlock()
			testutil.False(t, recorded, "challenge was recorded without its request identity")
		})
	}
	noteChallenge(nil, challengeTestError())
	_, ok := verification(nil)
	testutil.False(t, ok, "nil account offered a challenge")
}

func TestChallengeIdentityExpirationAndNonChallengeErrors(t *testing.T) {
	acc := challengeTestAccount()
	t.Cleanup(func() { clearChallenge(acc.ID) })
	noteChallenge(acc, errors.New("synthetic unrelated failure"))
	_, ok := verification(acc)
	testutil.False(t, ok, "unrelated error created a challenge")
	noteChallenge(acc, challengeTestError())
	challenges.Lock()
	recorded := challenges.byAccount[acc.ID]
	recorded.at = time.Now().Add(-challengeTTL - time.Second)
	challenges.byAccount[acc.ID] = recorded
	challenges.Unlock()
	_, ok = verification(acc)
	testutil.False(t, ok, "expired challenge was offered")
}
