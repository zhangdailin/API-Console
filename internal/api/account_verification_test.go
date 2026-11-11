package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

// The verification page is offered only by a channel that has such a
// challenge, only for an account that actually met one, and only on GET.
func TestAccountVerificationEndpoint(t *testing.T) {
	a, s, cleanup := newTestAPI(t)
	defer cleanup()
	get := func(method string, acc *store.Account) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		a.HandleAccountByID(rec, httptest.NewRequest(method, "/api/accounts/"+strconv.FormatInt(acc.ID, 10)+"/verification", nil))
		return rec
	}

	cline := &store.Account{AccountType: "cline", Name: "c", ClineRefreshToken: "r", Enabled: true}
	testutil.NoError(t, s.CreateAccount(context.Background(), cline))
	rec := get(http.MethodGet, cline)
	testutil.Equal(t, rec.Code, http.StatusNotFound)
	testutil.MustContain(t, rec.Body.String(), "no verification challenge")

	doubao := &store.Account{AccountType: "doubao", Name: "d", ClientCookie: `{"sessionid":"x"}`, DoubaoDeviceID: "7500000000000000002", Enabled: true}
	testutil.NoError(t, s.CreateAccount(context.Background(), doubao))
	rec = get(http.MethodGet, doubao)
	testutil.Equal(t, rec.Code, http.StatusNotFound)
	testutil.MustContain(t, rec.Body.String(), "no recent verification challenge")
	// Nothing about the account's identity leaves in a refusal.
	testutil.MustNotContain(t, rec.Body.String(), doubao.DoubaoDeviceID)

	testutil.Equal(t, get(http.MethodPost, doubao).Code, http.StatusMethodNotAllowed)
}
