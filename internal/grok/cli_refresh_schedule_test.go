package grok

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func TestClassifyCLIOAuthError(t *testing.T) {
	cases := []struct {
		code   string
		status int
		want   string
	}{
		{"invalid_grant", http.StatusBadRequest, cliOAuthPermanent},
		{"refresh_token_reused", http.StatusBadRequest, cliOAuthPermanent},
		{"expired_token", http.StatusUnauthorized, cliOAuthPermanent},
		{"invalid_client", http.StatusUnauthorized, cliOAuthConfiguration},
		{"unauthorized_client", http.StatusBadRequest, cliOAuthConfiguration},
		{"access_denied", http.StatusForbidden, cliOAuthUnclassified},
		{"something_new", http.StatusBadRequest, cliOAuthUnclassified},
		{"rate_limited", http.StatusTooManyRequests, cliOAuthTransient},
		{"server_error", http.StatusBadGateway, cliOAuthTransient},
	}
	for _, tc := range cases {
		got := classifyCLIOAuthError(tc.code, tc.status)
		testutil.Falsef(t, got != tc.want, "classify(%q, %d) = %q, want %q", tc.code, tc.status, got, tc.want)
	}
}

func TestCLIOAuthNonPermanentErrorIsNoCredentialVerdict(t *testing.T) {
	err := parseCLIOAuthErrorResponse([]byte(`{"error":"invalid_client"}`), http.StatusUnauthorized)
	testutil.False(t, IsCLIPermanentOAuthError(err), "invalid_client must not retire the account")
	var oauthErr *cliOAuthError
	testutil.True(t, errors.As(err, &oauthErr), "expected *cliOAuthError")
	testutil.Equal(t, oauthErr.Status(), "")

	err = parseCLIOAuthErrorResponse([]byte(`{"error":"invalid_grant"}`), http.StatusBadRequest)
	testutil.True(t, IsCLIPermanentOAuthError(err), "invalid_grant is permanent")
	testutil.True(t, errors.As(err, &oauthErr), "expected *cliOAuthError")
	testutil.Equal(t, oauthErr.Status(), "401")
}

func TestCLIRefreshLeadIsStableAndBounded(t *testing.T) {
	seen := map[time.Duration]bool{}
	for id := int64(1); id <= 200; id++ {
		lead := cliRefreshLead(id)
		testutil.Equal(t, lead, cliRefreshLead(id))
		testutil.Falsef(t, lead < cliRefreshBaseLead || lead > cliRefreshBaseLead+180*time.Second, "lead(%d)=%s out of range", id, lead)
		seen[lead] = true
	}
	// 200 accounts must not collapse onto a handful of renewal instants.
	testutil.Falsef(t, len(seen) <= 50, "only %d distinct leads for 200 accounts", len(seen))
}

func TestCLIRefreshDueRespectsBackoff(t *testing.T) {
	now := time.Now()
	acc := &store.Account{ID: 7, OAuthAccessToken: "a", OAuthExpiresAt: now.Add(time.Minute)}
	testutil.True(t, cliRefreshDue(acc, now), "token inside its lead is due")
	acc.GrokRefresh.RetryAt = now.Add(time.Minute)
	testutil.False(t, cliRefreshDue(acc, now), "a backed-off account is not due")
	acc.GrokRefresh.RetryAt = now.Add(-time.Second)
	testutil.True(t, cliRefreshDue(acc, now), "an elapsed backoff is due again")
	acc.OAuthExpiresAt = now.Add(time.Hour)
	testutil.False(t, cliRefreshDue(acc, now), "a token far from expiry is not due")
}

func TestRecordCLIRefreshFailure(t *testing.T) {
	now := time.Now()
	permanent := &cliOAuthError{status: http.StatusUnauthorized, code: "invalid_grant", class: cliOAuthPermanent}

	alive := &store.Account{ID: 1, OAuthAccessToken: "a", OAuthExpiresAt: now.Add(3 * time.Minute)}
	testutil.False(t, recordCLIRefreshFailure(alive, permanent, now), "a live token keeps serving after a refused grant")
	testutil.Equal(t, alive.GrokRefresh.RetryAt, alive.OAuthExpiresAt)
	testutil.Equal(t, alive.GrokRefresh.ErrorClass, cliOAuthPermanent)

	expired := &store.Account{ID: 1, OAuthAccessToken: "a", OAuthExpiresAt: now.Add(-time.Second)}
	testutil.True(t, recordCLIRefreshFailure(expired, permanent, now), "a refused grant with no live token needs a login")

	configuration := &cliOAuthError{status: http.StatusUnauthorized, code: "invalid_client", class: cliOAuthConfiguration}
	acc := &store.Account{ID: 1, OAuthAccessToken: "a", OAuthExpiresAt: now.Add(-time.Second)}
	testutil.False(t, recordCLIRefreshFailure(acc, configuration, now), "configuration errors never retire an account")
	testutil.False(t, acc.GrokRefresh.RetryAt.Before(now.Add(cliRefreshConfigurationRetry)), "configuration retry waits at least 30m")

	unclassified := &cliOAuthError{status: http.StatusBadRequest, code: "mystery", class: cliOAuthUnclassified}
	acc = &store.Account{ID: 1, OAuthAccessToken: "a", OAuthExpiresAt: now.Add(-time.Second)}
	for i := 1; i < cliRefreshUnclassifiedLimit; i++ {
		testutil.Falsef(t, recordCLIRefreshFailure(acc, unclassified, now), "failure %d must only back off", i)
	}
	testutil.True(t, recordCLIRefreshFailure(acc, unclassified, now), "the 5th identical unclassified failure needs a login")

	acc = &store.Account{ID: 1, OAuthAccessToken: "a", OAuthExpiresAt: now.Add(time.Hour)}
	for i := 0; i < 10; i++ {
		testutil.False(t, recordCLIRefreshFailure(acc, unclassified, now), "a live token never needs a login for an unclassified error")
	}

	acc = &store.Account{ID: 1}
	var last time.Time
	for i := 0; i < 7; i++ {
		testutil.False(t, recordCLIRefreshFailure(acc, errors.New("connection reset"), now), "transient")
		testutil.False(t, acc.GrokRefresh.RetryAt.Before(last), "backoff must not shrink")
		last = acc.GrokRefresh.RetryAt
	}
	testutil.Equal(t, acc.GrokRefresh.ErrorClass, cliOAuthTransient)
	testutil.Falsef(t, last.After(now.Add(16*time.Minute)), "backoff capped, got %s", last.Sub(now))

	recordCLIRefreshSuccess(acc, now)
	testutil.Equal(t, acc.GrokRefresh.Failures, 0)
	testutil.True(t, acc.GrokRefresh.RetryAt.IsZero(), "success clears the backoff")
}

func newRefreshScheduleTest(t *testing.T, status int, body string) (*config.Config, *store.Store, *int32) {
	t.Helper()
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	cfg := &config.Config{}
	cfg.GrokCLIOAuthTokenURL = server.URL
	return cfg, newTestGrokStore(t, "refresh-schedule:"), &calls
}

func TestRefreshCLIAccountBacksOffWithoutRetiringOnConfigurationError(t *testing.T) {
	cfg, s, calls := newRefreshScheduleTest(t, http.StatusUnauthorized, `{"error":"invalid_client"}`)
	ctx := context.Background()
	acc := &store.Account{AccountType: "grok", CredentialType: "oauth", Enabled: true,
		OAuthAccessToken: "access", OAuthRefreshToken: "refresh", OAuthExpiresAt: time.Now().Add(-time.Minute)}
	testutil.NoError(t, s.CreateAccount(ctx, acc))

	refreshCLIAccount(ctx, cfg, s, acc)
	got, err := s.GetAccount(ctx, acc.ID)
	testutil.NoError(t, err)
	testutil.Equal(t, got.OAuthRefreshToken, "refresh")
	testutil.True(t, store.AccountAuthActive(got), "a configuration error must not require a login")
	testutil.Equal(t, got.GrokRefresh.ErrorClass, cliOAuthConfiguration)
	testutil.Equal(t, got.GrokRefresh.ErrorCode, "invalid_client")
	testutil.True(t, got.GrokRefresh.InBackoff(time.Now()), "the failure is persisted as a backoff")

	refreshCLIAccount(ctx, cfg, s, got)
	testutil.Equal(t, atomic.LoadInt32(calls), int32(1))

	// Requests do not retry the refresh either while it backs off.
	oauth := NewCLIOAuth(cfg, nil)
	oauth.SetAccountStore(s)
	_, err = oauth.AccessToken(ctx, got)
	testutil.Error(t, err)
	testutil.False(t, IsCLIPermanentOAuthError(err), "a backoff is not a credential verdict")
	testutil.Equal(t, atomic.LoadInt32(calls), int32(1))
}

func TestRefreshCLIAccountRequiresLoginForRefusedGrant(t *testing.T) {
	cfg, s, _ := newRefreshScheduleTest(t, http.StatusBadRequest, `{"error":"invalid_grant"}`)
	ctx := context.Background()
	acc := &store.Account{AccountType: "grok", CredentialType: "oauth", Enabled: true,
		OAuthAccessToken: "access", OAuthRefreshToken: "refresh", OAuthExpiresAt: time.Now().Add(-time.Minute)}
	testutil.NoError(t, s.CreateAccount(ctx, acc))

	refreshCLIAccount(ctx, cfg, s, acc)
	got, err := s.GetAccount(ctx, acc.ID)
	testutil.NoError(t, err)
	testutil.Equal(t, got.AuthStatus, store.AccountAuthStatusReauthRequired)
	testutil.Equal(t, got.GrokRefresh.ErrorCode, "invalid_grant")
}

func TestCLIOAuthServesUnexpiredTokenWhenEarlyRefreshFails(t *testing.T) {
	cfg, s, calls := newRefreshScheduleTest(t, http.StatusServiceUnavailable, `{}`)
	ctx := context.Background()
	acc := &store.Account{AccountType: "grok", CredentialType: "oauth", Enabled: true,
		OAuthAccessToken: "still-valid", OAuthRefreshToken: "refresh", OAuthExpiresAt: time.Now().Add(2 * time.Minute)}
	testutil.NoError(t, s.CreateAccount(ctx, acc))
	oauth := NewCLIOAuth(cfg, nil)
	oauth.SetAccountStore(s)

	token, err := oauth.AccessToken(ctx, acc)
	testutil.NoError(t, err)
	testutil.Equal(t, token, "still-valid")
	testutil.Equal(t, atomic.LoadInt32(calls), int32(1))

	// Once the scheduler has recorded a backoff, requests stop retrying.
	acc.GrokRefresh = store.GrokRefreshState{Failures: 1, ErrorClass: cliOAuthTransient, RetryAt: time.Now().Add(time.Minute), UpdatedAt: time.Now()}
	testutil.NoError(t, s.UpdateAccount(ctx, acc))
	token, err = oauth.AccessToken(ctx, acc)
	testutil.NoError(t, err)
	testutil.Equal(t, token, "still-valid")
	testutil.Equal(t, atomic.LoadInt32(calls), int32(1))
}
