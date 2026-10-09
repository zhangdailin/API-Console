package qoder

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

// newLoginClient builds an account-less client pointed at stub endpoints. It is
// the shape the admin login flow uses.
func newLoginClient(t *testing.T, oauth, openAPI, inference string) *Client {
	t.Helper()
	client := NewFromAccount(nil, nil)
	setTestEndpoints(client, oauth, openAPI, inference)
	return client
}

// TestStartLoginBuildsOfficialURL pins the authorization URL: the official host,
// the S256 challenge, the nonce, the machine id, the client id, and no redirect
// URI or scope that a device flow does not use.
func TestStartLoginBuildsOfficialURL(t *testing.T) {
	t.Parallel()

	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		testutil.CheckEqual(t, r.Method, http.MethodHead)
		w.WriteHeader(http.StatusOK)
	}))
	defer page.Close()

	client := newLoginClient(t, page.URL, page.URL, page.URL)
	setTestEntropy(client, strings.NewReader(strings.Repeat("\x00", 512)))

	tx, err := client.StartLogin(context.Background())
	testutil.NoError(t, err, "StartLogin() error = %v")
	testutil.Falsef(t, !strings.HasPrefix(tx.VerifyURL, page.URL+"/device/selectAccounts?"), "VerifyURL = %q, want the official authorization path", tx.VerifyURL)
	for _, want := range []string{"challenge=", "challenge_method=S256", "nonce=", "machine_id=", "client_id=" + DefaultClientID} {
		testutil.CheckContain(t, tx.VerifyURL, want)
	}
	for _, unwanted := range []string{"redirect_uri", "scope=", "response_type"} {
		testutil.CheckNotContain(t, tx.VerifyURL, unwanted)
	}
	testutil.CheckEqual(t, len(tx.MachineID), 36)
	testutil.CheckFalsef(t, len(tx.Verifier) < 43 || len(tx.Verifier) > 128, "Verifier length = %d, want 43..128", len(tx.Verifier))
	testutil.CheckFalse(t, !tx.ExpiresAt.After(time.Now()), "ExpiresAt is not in the future")
}

// TestAuthorizationHostAllowlist pins the login redirect boundary: only the
// Qoder hosts, the deployment's configured endpoint and loopback may be handed
// to a browser. A third-party host here would be a credential-phishing path, so
// the allowlist is asserted directly.
func TestAuthorizationHostAllowlist(t *testing.T) {
	t.Parallel()

	client := NewFromAccount(nil, nil)
	for _, host := range []string{"qoder.com", "www.qoder.com", "openapi.qoder.sh", "localhost", "127.0.0.1", "::1", "qoder.com"} {
		testutil.CheckFalsef(t, !client.allowedLoginHost(host), "allowedLoginHost(%q) = false, want true", host)
	}
	for _, host := range []string{"evil.example.com", "qoder.com.evil.example", "", "openapi.qoder.sh.evil"} {
		testutil.CheckFalsef(t, client.allowedLoginHost(host), "allowedLoginHost(%q) = true, want false", host)
	}
}

// TestConfiguredOAuthHostIsAllowedButForeignHostsAreNot proves the deployment's
// own endpoint is honoured while an unrelated host stays refused: the override
// is a configuration seam, not a wildcard.
func TestConfiguredOAuthHostIsAllowedButForeignHostsAreNot(t *testing.T) {
	t.Parallel()

	client := NewFromAccount(nil, &config.Config{QoderOAuthBaseURL: "https://auth.example.cn"})
	testutil.False(t, !client.allowedLoginHost("auth.example.cn"), "the configured OAuth host was refused")
	testutil.False(t, client.allowedLoginHost("attacker.example"), "an unrelated host was accepted because an override exists")
	// A suffix match is not a match.
	testutil.False(t, client.allowedLoginHost("auth.example.cn.evil"), "a look-alike host was accepted")
}

// TestStartLoginClassifiesUnreachable proves a blocked egress path is reported
// as unreachable rather than as a rejected transaction, because the two need
// different operator action.
func TestStartLoginClassifiesUnreachable(t *testing.T) {
	t.Parallel()

	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	base := dead.URL
	dead.Close()

	client := newLoginClient(t, base, base, base)
	setTestEntropy(client, strings.NewReader(strings.Repeat("\x02", 512)))

	_, err := client.StartLogin(context.Background())
	testutil.Falsef(t, !errors.Is(err, ErrAuthUnavailable), "error = %v, want ErrAuthUnavailable", err)
}

// TestPollLoginTreats404AsPending pins the pending signal: the device token
// endpoint answers 404 until the browser step completes, and treating that as a
// hard failure would abort every login that takes more than one poll.
func TestPollLoginTreats404AsPending(t *testing.T) {
	t.Parallel()

	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		testutil.CheckEqual(t, r.URL.Path, "/api/v1/deviceToken/poll")
		testutil.CheckEqual(t, r.Header.Get("Accept"), "application/json")
		if calls < 2 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"access-1","refresh_token":"refresh-1","expires_in":3600,"refresh_token_expires_in":864000,"user_id":"uid-1","user_name":"tester"}`))
	}))
	defer server.Close()

	client := newLoginClient(t, server.URL, server.URL, server.URL)
	tx := &LoginTransaction{Nonce: "n", Verifier: "v", MachineID: "m", ExpiresAt: time.Now().Add(time.Minute)}

	_, err := client.PollLogin(context.Background(), tx)
	testutil.Falsef(t, !errors.Is(err, ErrAuthPending), "first poll error = %v, want ErrAuthPending", err)
	creds, err := client.PollLogin(context.Background(), tx)
	testutil.NoError(t, err, "second poll error = %v")
	testutil.Equal(t, creds.AccessToken, "access-1")
	testutil.Equal(t, creds.RefreshToken, "refresh-1")
	testutil.Equal(t, creds.UID, "uid-1")
	testutil.Equal(t, creds.Name, "tester")
	testutil.Falsef(t, creds.AccessExpiresAt.IsZero() || creds.RefreshExpiresAt.IsZero(), "expiries = %v/%v, want both resolved from expires_in", creds.AccessExpiresAt, creds.RefreshExpiresAt)
}

// TestPollLoginRejectsOtherStatuses proves a non-200, non-404 answer is a
// rejection rather than a silent retry loop.
func TestPollLoginRejectsOtherStatuses(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"bad nonce"}`))
	}))
	defer server.Close()

	client := newLoginClient(t, server.URL, server.URL, server.URL)
	tx := &LoginTransaction{Nonce: "n", Verifier: "v"}
	_, err := client.PollLogin(context.Background(), tx)
	testutil.Falsef(t, !errors.Is(err, ErrAuthRejected), "error = %v, want ErrAuthRejected", err)
}

// TestPollLoginRejectsTokenlessSuccess proves a 200 without a token is not
// mistaken for a successful login.
func TestPollLoginRejectsTokenlessSuccess(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"user_id":"uid-1"}`))
	}))
	defer server.Close()

	client := newLoginClient(t, server.URL, server.URL, server.URL)
	_, err := client.PollLogin(context.Background(), &LoginTransaction{Nonce: "n", Verifier: "v"})
	testutil.Falsef(t, !errors.Is(err, ErrAuthRejected), "error = %v, want ErrAuthRejected", err)
}

// TestRefreshAcceptsBothTokenSpellings pins the field-name difference between
// the poll and refresh endpoints, and the refresh-token rotation.
func TestRefreshAcceptsBothTokenSpellings(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		testutil.CheckEqual(t, r.Method, http.MethodPost)
		testutil.CheckEqual(t, r.URL.Path, "/api/v1/deviceToken/refresh")
		body, _ := io.ReadAll(r.Body)
		testutil.CheckContain(t, string(body), `"refresh_token":"refresh-1"`)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"device_token":"access-2","refresh_token":"refresh-2","expires_in":1800}`))
	}))
	defer server.Close()

	client := newLoginClient(t, server.URL, server.URL, server.URL)
	creds, err := client.Refresh(context.Background(), "refresh-1")
	testutil.NoError(t, err, "Refresh() error = %v")
	testutil.Equal(t, creds.AccessToken, "access-2")
	testutil.Equal(t, creds.RefreshToken, "refresh-2")
}

// TestRefreshClassifiesReLoginRequired proves a refused refresh grant is
// reported as requiring a new login rather than as a transient failure the
// scheduler would retry forever.
func TestRefreshClassifiesReLoginRequired(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"refresh token expired"}`))
	}))
	defer server.Close()

	client := newLoginClient(t, server.URL, server.URL, server.URL)
	_, err := client.Refresh(context.Background(), "refresh-1")
	testutil.Falsef(t, !errors.Is(err, ErrReLoginRequired), "error = %v, want ErrReLoginRequired", err)
}

// TestRefreshWithoutTokenIsMissingCredential pins the refusal to call the
// endpoint with nothing to present.
func TestRefreshWithoutTokenIsMissingCredential(t *testing.T) {
	t.Parallel()

	client := newLoginClient(t, "http://127.0.0.1:1", "http://127.0.0.1:1", "http://127.0.0.1:1")
	_, err := client.Refresh(context.Background(), "  ")
	testutil.Falsef(t, !errors.Is(err, ErrCredentialMissing), "error = %v, want ErrCredentialMissing", err)
}

// TestFetchProfileToleratesFailure proves the enrichment is best effort.
func TestFetchProfileToleratesFailure(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		testutil.CheckEqual(t, r.Header.Get("Authorization"), "Bearer access-1")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client := newLoginClient(t, server.URL, server.URL, server.URL)
	_, err := client.FetchProfile(context.Background(), "access-1")
	testutil.Error(t, err)
}

// TestResolveCredentialsPrefersDedicatedFields pins the field mapping, including
// the refusal to treat a generic slot holding an unrelated value as a Qoder
// credential.
func TestResolveCredentialsPrefersDedicatedFields(t *testing.T) {
	t.Parallel()

	acc := &store.Account{
		AccountType:       "qoder",
		QoderAccessToken:  "access",
		QoderRefreshToken: "refresh",
		QoderUserID:       "uid",
		QoderExpiresAt:    time.Unix(1700000000, 0),
		Token:             "some-other-channel-token",
	}
	creds := ResolveCredentials(acc)
	testutil.Equal(t, creds.AccessToken, "access")
	testutil.Equal(t, creds.RefreshToken, "refresh")
	testutil.Equal(t, creds.UID, "uid")

	// With the dedicated fields empty, an unrelated opaque value must not be
	// adopted.
	other := &store.Account{AccountType: "qoder", Token: "opaque-value"}
	got := ResolveCredentials(other)
	testutil.Falsef(t, got.HasCredential(), "credentials = %+v, want nothing resolved from an opaque generic slot", got)
}

// TestUnixSecondsNormalizesMilliseconds proves a millisecond timestamp does not
// land the expiry tens of thousands of years in the future, which would disable
// refresh silently.
func TestUnixSecondsNormalizesMilliseconds(t *testing.T) {
	t.Parallel()

	testutil.Equal(t, unixSeconds(1700000000000).Unix(), 1700000000)
	testutil.Equal(t, unixSeconds(1700000000).Unix(), 1700000000)
}

// TestParseExpiryAcceptsBothForms proves the relative and absolute spellings are
// both understood; reading a lifetime as an absolute instant would corrupt the
// freshness decision.
func TestParseExpiryAcceptsBothForms(t *testing.T) {
	t.Parallel()

	now := time.Unix(1700000000, 0)
	got := parseExpiry("", 3600, now)
	testutil.Falsef(t, !got.Equal(now.Add(time.Hour)), "parseExpiry(relative) = %v, want %v", got, now.Add(time.Hour))
	got = parseExpiry("2023-11-14T22:13:20Z", 0, now)
	testutil.Falsef(t, got.Unix() != 1700000000, "parseExpiry(absolute) = %v, want 1700000000", got.Unix())
	got = parseExpiry("", 0, now)
	testutil.Falsef(t, !got.IsZero(), "parseExpiry(absent) = %v, want zero", got)
}

// TestProbeReachabilityDetectsBlockedEgress covers the startup diagnostic.
func TestProbeReachabilityDetectsBlockedEgress(t *testing.T) {
	t.Parallel()

	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	base := dead.URL
	dead.Close()

	client := newLoginClient(t, base, base, base)
	err := client.ProbeReachability(context.Background())
	testutil.Falsef(t, !errors.Is(err, ErrAuthUnavailable), "error = %v, want ErrAuthUnavailable", err)

	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer live.Close()
	client = newLoginClient(t, live.URL, live.URL, live.URL)
	testutil.NoError(t, client.ProbeReachability(context.Background()), "ProbeReachability() error = %v, want success")
}

// TestEnsureAccessTokenSkipsRefreshWhileValid proves a fresh token is reused
// instead of burning a rotation on every request.
func TestEnsureAccessTokenSkipsRefreshWhileValid(t *testing.T) {
	t.Parallel()

	var refreshes int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "deviceToken/refresh") {
			refreshes++
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"access-new","refresh_token":"refresh-new"}`))
	}))
	defer server.Close()

	acc := &store.Account{
		AccountType:       "qoder",
		QoderAccessToken:  "access-fresh",
		QoderRefreshToken: "refresh-1",
		QoderExpiresAt:    time.Now().Add(6 * time.Hour),
	}
	client := NewFromAccount(acc, nil)
	setTestEndpoints(client, server.URL, server.URL, server.URL)

	creds, err := client.ensureAccessToken(context.Background())
	testutil.NoError(t, err, "ensureAccessToken() error = %v")
	testutil.Equal(t, creds.AccessToken, "access-fresh")
	testutil.Equal(t, refreshes, 0)
}

func TestResolveCredentialsIgnoresGenericDocument(t *testing.T) {
	raw := `{"access_token":"access","refresh_token":"refresh","uid":"user"}`
	testutil.False(t, ResolveCredentials(&store.Account{Token: raw, RefreshToken: raw, ClientCookie: raw}).HasCredential(), "generic credentials must not migrate")
}
