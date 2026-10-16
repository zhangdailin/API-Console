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

func TestHandleAccountByID_PutEnabledAfterAtomicQualityVerdict(t *testing.T) {
	a, s, cleanup := newTestAPI(t)
	defer cleanup()
	ctx := context.Background()
	acc := &store.Account{AccountType: "grok", CredentialType: "oauth", OAuthAccessToken: "keep-access", Enabled: true, Name: "quality-admin"}
	if err := s.CreateAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := s.ApplyAccountQuality(ctx, acc.ID, store.AccountQualityUpdate{Reason: "no_reasoning", CooldownUntil: time.Now().Add(time.Hour), DisableThreshold: 2}); err != nil {
			t.Fatal(err)
		}
	}
	for _, enabled := range []bool{true, false} {
		body := `{"account_type":"grok","credential_type":"oauth","name":"quality-admin","enabled":false}`
		if enabled {
			body = strings.Replace(body, `"enabled":false`, `"enabled":true`, 1)
		}
		req := httptest.NewRequest(http.MethodPut, "/api/accounts/"+strconv.FormatInt(acc.ID, 10), strings.NewReader(body))
		rec := httptest.NewRecorder()
		a.HandleAccountByID(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("admin update failed: %d %s", rec.Code, rec.Body.String())
		}
		saved, err := s.GetAccount(ctx, acc.ID)
		if err != nil {
			t.Fatal(err)
		}
		if saved.Enabled != enabled || saved.QualityRevision != 2 || saved.QualityFailures != 2 || saved.OAuthAccessToken != "keep-access" {
			t.Fatalf("admin change lost or rewrote runtime verdict: enabled=%v revision=%d failures=%d", saved.Enabled, saved.QualityRevision, saved.QualityFailures)
		}
	}
}
