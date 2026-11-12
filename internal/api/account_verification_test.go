package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"orchids-api/internal/accountpolicy"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

// The challenge is offered only by a channel that has one and only for an
// account that met one; nothing about the account's identity leaves in a
// refusal. Answering it lifts the human-verification park and nothing else.
func TestAccountVerificationEndpoint(t *testing.T) {
	a, s, cleanup := newTestAPI(t)
	defer cleanup()
	call := func(method string, acc *store.Account) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		a.HandleAccountByID(rec, httptest.NewRequest(method, "/api/accounts/"+strconv.FormatInt(acc.ID, 10)+"/verification", nil))
		return rec
	}

	cline := &store.Account{AccountType: "cline", Name: "c", ClineRefreshToken: "r", Enabled: true}
	testutil.NoError(t, s.CreateAccount(context.Background(), cline))
	rec := call(http.MethodGet, cline)
	testutil.Equal(t, rec.Code, http.StatusNotFound)
	testutil.MustContain(t, rec.Body.String(), "no verification challenge")

	doubao := &store.Account{AccountType: "doubao", Name: "d", ClientCookie: `{"sessionid":"x"}`, DoubaoDeviceID: "7500000000000000002", Enabled: true}
	testutil.NoError(t, s.CreateAccount(context.Background(), doubao))
	rec = call(http.MethodGet, doubao)
	testutil.Equal(t, rec.Code, http.StatusNotFound)
	testutil.MustContain(t, rec.Body.String(), "no recent verification challenge")
	testutil.MustNotContain(t, rec.Body.String(), doubao.DoubaoDeviceID)
	testutil.Equal(t, call(http.MethodDelete, doubao).Code, http.StatusMethodNotAllowed)

	// A parked account is released by an answer.
	parkedAt := time.Now().Add(-time.Minute).Truncate(time.Second)
	verifiedAt := parkedAt.Add(-time.Hour)
	doubao.StatusCode, doubao.LastAttempt, doubao.VerifiedAt = "429", parkedAt, verifiedAt
	doubao.StatusMessage = "status=403: doubao account requires human verification: open doubao.com"
	doubao.QuotaResetAt = parkedAt.Add(accountpolicy.CooldownVerification)
	testutil.NoError(t, s.UpdateAccount(context.Background(), doubao))
	rec = call(http.MethodPost, doubao)
	testutil.Equal(t, rec.Code, http.StatusOK)
	saved, err := s.GetAccount(context.Background(), doubao.ID)
	testutil.NoError(t, err)
	testutil.Equal(t, saved.StatusCode, "")
	testutil.Equal(t, saved.StatusMessage, "")
	testutil.True(t, saved.QuotaResetAt.IsZero())
	testutil.True(t, saved.VerifiedAt.Equal(verifiedAt), "answering the challenge must not claim a credential check")
	var out map[string]any
	testutil.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	testutil.MustNotContain(t, rec.Body.String(), "sessionid")

	// A park for another reason is left alone.
	doubao.StatusCode, doubao.StatusMessage, doubao.LastAttempt = "429", "status=429: doubao upstream rate limited", parkedAt
	testutil.NoError(t, s.UpdateAccount(context.Background(), doubao))
	testutil.Equal(t, call(http.MethodPost, doubao).Code, http.StatusOK)
	saved, err = s.GetAccount(context.Background(), doubao.ID)
	testutil.NoError(t, err)
	testutil.Equal(t, saved.StatusCode, "429")
}
