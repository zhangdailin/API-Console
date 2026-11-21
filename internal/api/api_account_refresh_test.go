package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func newTestAPI(t *testing.T) (*API, *store.Store, func()) {
	t.Helper()

	s, _ := newTestStore(t, "api_test:")

	return New(s, "", "", &config.Config{}), s, func() { _ = s.Close() }
}

func TestAccountCheckAndUsageRejectWriteMethods(t *testing.T) {
	s, _ := newTestStore(t, "account-query-method:")
	a := &API{store: s}
	acc := &store.Account{AccountType: "doubao", Name: "synthetic account", Enabled: true}
	testutil.NoError(t, s.CreateAccount(t.Context(), acc))
	for _, action := range []string{"check", "usage"} {
		for _, method := range []string{http.MethodPut, http.MethodDelete, http.MethodPost} {
			t.Run(action+"/"+method, func(t *testing.T) {
				path := "/api/accounts/" + strconv.FormatInt(acc.ID, 10) + "/" + action
				rec := httptest.NewRecorder()
				a.HandleAccountByID(rec, httptest.NewRequest(method, path, strings.NewReader("{")))
				testutil.Equal(t, rec.Code, http.StatusMethodNotAllowed)
				testutil.Equal(t, rec.Header().Get("Allow"), http.MethodGet)
				current, err := s.GetAccount(t.Context(), acc.ID)
				testutil.NoError(t, err)
				testutil.Equal(t, current.Name, acc.Name)
			})
		}
	}
}

func TestHandleAccountByID_PutPreservesGrokOAuthTokens(t *testing.T) {
	a, s, cleanup := newTestAPI(t)
	defer cleanup()

	acc := &store.Account{
		AccountType:       "grok",
		CredentialType:    "oauth",
		OAuthAccessToken:  "keep-access",
		OAuthRefreshToken: "keep-refresh",
		OAuthExpiresAt:    time.Now().UTC().Add(time.Hour).Truncate(time.Second),
		TeamID:            "team-1",
		Enabled:           true,
		Name:              "oauth-acc",
	}
	testutil.NoError(t, s.CreateAccount(context.Background(), acc), "CreateAccount() error = %v")

	// Simulate admin UI edit/save with redacted empty secrets.
	body := `{"account_type":"grok","credential_type":"oauth","name":"oauth-acc","enabled":false,"oauth_access_token":"","oauth_refresh_token":""}`
	req := httptest.NewRequest(http.MethodPut, "/api/accounts/"+strconv.FormatInt(acc.ID, 10), strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	a.HandleAccountByID(rec, req)
	testutil.Equal(t, rec.Code, http.StatusOK)

	got, err := s.GetAccount(context.Background(), acc.ID)
	testutil.NoError(t, err, "GetAccount() error = %v")
	testutil.Equal(t, got.OAuthAccessToken, "keep-access")
	testutil.Equal(t, got.OAuthRefreshToken, "keep-refresh")
	testutil.False(t, got.Enabled, "expected enabled=false after update")
	testutil.Equal(t, got.TeamID, "team-1")
}

func TestHandleAccounts_PostRejectsEmptyGrokOAuth(t *testing.T) {
	a, _, cleanup := newTestAPI(t)
	defer cleanup()

	body := `{"account_type":"grok","credential_type":"oauth","enabled":true}`
	req := httptest.NewRequest(http.MethodPost, "/api/accounts", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	a.HandleAccounts(rec, req)
	testutil.Equal(t, rec.Code, http.StatusBadRequest)
	testutil.MustContain(t, rec.Body.String(), "missing oauth token")
}
