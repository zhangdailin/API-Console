package doubao

import (
	"orchids-api/internal/store"
	"testing"
	"time"
)

func TestOldIdentityRefusalCannotReplaceCurrentChallenge(t *testing.T) {
	old := &store.Account{ID: 9281, AccountType: "doubao", DoubaoDeviceID: "1000000000000000001", DoubaoWebID: "1000000000000000002", DoubaoTeaUUID: "1000000000000000002", ClientCookie: `{"sessionid":"synthetic","s_v_web_id":"fp"}`, UpdatedAt: time.Now().Add(-time.Minute)}
	latest := old.Clone()
	latest.DoubaoDeviceID = "1000000000000000003"
	latest.UpdatedAt = time.Now()
	t.Cleanup(func() { clearChallenge(old.ID) })
	freshErr := &verificationRequired{raw: `{"type":"verify","detail":"new"}`}
	noteChallenge(latest, freshErr)
	noteChallenge(old, &verificationRequired{raw: `{"type":"verify","detail":"old"}`})
	got, ok := verification(latest)
	if !ok || got.Params["verify_data"] != freshErr.raw {
		t.Fatal("old refusal replaced current identity challenge")
	}
	clearChallengeForIdentity(old)
	if _, ok := verification(latest); !ok {
		t.Fatal("old success cleared new challenge")
	}
}
