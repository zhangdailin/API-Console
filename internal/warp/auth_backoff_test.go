package warp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/store"
)

func TestCredentialsRefreshFiveMinutesBeforeExpiry(t *testing.T) {
	for _, tc := range []struct {
		remaining time.Duration
		valid     bool
	}{
		{-time.Second, false}, {time.Minute, false}, {3 * time.Minute, false}, {5 * time.Minute, false}, {6 * time.Minute, true},
	} {
		grant := Credentials{AccessToken: "synthetic-access", ExpiresAt: time.Now().Add(tc.remaining)}
		if got := grant.valid(); got != tc.valid {
			t.Errorf("remaining=%v valid=%t want=%t", tc.remaining, got, tc.valid)
		}
	}
	if (Credentials{ExpiresAt: time.Now().Add(time.Hour)}).valid() || (Credentials{AccessToken: "synthetic-access"}).valid() {
		t.Fatal("missing token or expiry accepted")
	}
}

func TestAuthBackoffProgressionAndExpiryWithoutSleeping(t *testing.T) {
	c := NewFromAccount(testAccount(), nil)
	key := c.authFailureKey(credentials(c.account))
	t.Cleanup(func() { forgetAuthFailure(key) })
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, want := range []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute} {
		err := rememberAuthFailure(key, transientAuthFailure("transport", 0), now)
		var failure *authFailure
		if !errors.As(err, &failure) || failure.RetryAfter() != want {
			t.Fatalf("wrong exponential delay: %v want=%v", err, want)
		}
		if cachedAuthFailure(key, now.Add(want-time.Second)) == nil {
			t.Fatal("backoff expired early")
		}
		now = now.Add(want)
		if cachedAuthFailure(key, now) != nil {
			t.Fatal("backoff did not expire")
		}
	}
	forgetAuthFailure(key)
	err := rememberAuthFailure(key, transientAuthFailure("proxy_unavailable", 90*time.Second), now)
	if err.(*authFailure).RetryAfter() != 90*time.Second {
		t.Fatal("server retry hint lost")
	}
	if cachedAuthFailure(key, now.Add(25*time.Hour)) != nil {
		t.Fatal("stale failure never expired")
	}
}

func TestAuthBackoffIsSharedAcrossConcurrentFreshClients(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "300")
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"error":{"message":"TEMPORARILY_UNAVAILABLE"}}`))
	}))
	defer srv.Close()
	acc := testAccount()
	acc.ID, acc.WarpExpiresAt = 971002, time.Now().Add(-time.Hour)
	owner := &flakyCredentialStore{acc: *acc}
	first := NewFromAccount(acc, testConfig(srv.URL))
	first.SetAccountStore(owner)
	key := first.authFailureKey(credentials(acc))
	t.Cleanup(func() { forgetAuthFailure(key) })
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			c := NewFromAccount(acc, testConfig(srv.URL))
			c.SetAccountStore(owner)
			_, err := c.accessToken(t.Context())
			var failure *authFailure
			if !errors.As(err, &failure) || failure.terminal() || failure.RetryAfter() < 4*time.Minute {
				t.Errorf("wrong shared backoff: %v", err)
			}
		})
	}
	wg.Wait()
	if calls.Load() != 1 || owner.calls != 0 {
		t.Fatalf("refresh storm: network=%d commits=%d", calls.Load(), owner.calls)
	}
	if !store.AccountAuthActive(&owner.acc) {
		t.Fatal("temporary outage marked the account for reauthentication")
	}
}

func TestAuthTerminalSuppressionResetsOnSameRefreshTokenReplacement(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":{"message":"INVALID_REFRESH_TOKEN"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id_token":"synthetic-renewed","refresh_token":"synthetic-rotated","expires_in":"3600"}`))
	}))
	defer srv.Close()
	acc := testAccount()
	acc.ID, acc.WarpExpiresAt = 971003, time.Now().Add(-time.Hour)
	owner := &flakyCredentialStore{acc: *acc}
	makeClient := func() *Client { c := NewFromAccount(acc, testConfig(srv.URL)); c.SetAccountStore(owner); return c }
	first := makeClient()
	key := first.authFailureKey(credentials(acc))
	t.Cleanup(func() { forgetAuthFailure(key) })
	for range 2 {
		if _, err := makeClient().accessToken(t.Context()); err == nil || apperrors.ClassifyAccountStatus(err.Error()) != "401" {
			t.Fatalf("missing terminal failure: %v", err)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("terminal failure retried the same credentials")
	}
	// A login can replace the access token while Firebase retains its refresh token.
	owner.acc.WarpAccessToken = "synthetic-replacement-access"
	if token, err := makeClient().accessToken(t.Context()); err != nil || token != "synthetic-renewed" || calls.Load() != 2 {
		t.Fatalf("replacement remained suppressed: %v", err)
	}
	if cachedAuthFailure(key, time.Now()) != nil {
		t.Fatal("replacement retained stale terminal state")
	}
}

func TestAuthSuccessfulRetryClearsBackoff(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		_, _ = w.Write([]byte(`{"id_token":"synthetic-renewed","expires_in":"3600"}`))
	}))
	defer srv.Close()
	acc := testAccount()
	acc.ID, acc.WarpExpiresAt = 971004, time.Now().Add(-time.Hour)
	owner := &flakyCredentialStore{acc: *acc}
	c := NewFromAccount(acc, testConfig(srv.URL))
	c.SetAccountStore(owner)
	key := c.authFailureKey(credentials(acc))
	t.Cleanup(func() { forgetAuthFailure(key) })
	if _, err := c.accessToken(t.Context()); err == nil {
		t.Fatal("outage was ignored")
	}
	authFailures.Lock()
	entry := authFailures.entries[key]
	entry.retryAt = time.Now().Add(-time.Second)
	authFailures.entries[key] = entry
	authFailures.Unlock()
	if token, err := c.accessToken(t.Context()); err != nil || token != "synthetic-renewed" {
		t.Fatalf("retry failed: %v", err)
	}
	if cachedAuthFailure(key, time.Now()) != nil || owner.acc.WarpRefreshToken != acc.WarpRefreshToken {
		t.Fatal("success retained failure state or erased the omitted refresh token")
	}
}

func TestAuthFailureCacheIsBounded(t *testing.T) {
	authFailures.Lock()
	previous := authFailures.entries
	authFailures.entries = make(map[pendingRotationKey]authFailureEntry)
	authFailures.Unlock()
	t.Cleanup(func() { authFailures.Lock(); authFailures.entries = previous; authFailures.Unlock() })
	c := NewFromAccount(testAccount(), nil)
	key := c.authFailureKey(credentials(c.account))
	for i := range authFailureCap + 20 {
		key.expected[0], key.expected[1] = byte(i), byte(i>>8)
		rememberAuthFailure(key, transientAuthFailure("transport", 0), time.Now())
	}
	authFailures.Lock()
	defer authFailures.Unlock()
	if len(authFailures.entries) > authFailureCap {
		t.Fatal("auth failure cache is unbounded")
	}
}

func newAuthStateTestStore(t *testing.T) *store.Store {
	t.Helper()
	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{RedisAddr: mini.Addr(), RedisPrefix: "warp-auth:", CredentialEncryptionKey: []byte("01234567890123456789012345678901")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestTerminalAuthFailurePersistsButTransientFailureDoesNot(t *testing.T) {
	for _, verdict := range []string{"INVALID_REFRESH_TOKEN", "USER_DISABLED", "TEMPORARILY_UNAVAILABLE"} {
		t.Run(verdict, func(t *testing.T) {
			s := newAuthStateTestStore(t)
			acc := testAccount()
			acc.Name, acc.Weight, acc.WarpExpiresAt = "preserved", 7, time.Now().Add(-time.Hour)
			if err := s.CreateAccount(t.Context(), acc); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`{"error":{"message":"` + verdict + `"}}`))
			}))
			defer srv.Close()
			c := newClientWithStore(acc, testConfig(srv.URL), s)
			key := c.authFailureKey(credentials(acc))
			t.Cleanup(func() { forgetAuthFailure(key) })
			if _, err := c.accessToken(t.Context()); err == nil {
				t.Fatal("upstream error was ignored")
			}
			got, err := s.GetAccount(t.Context(), acc.ID)
			if err != nil {
				t.Fatal(err)
			}
			terminal := verdict != "TEMPORARILY_UNAVAILABLE"
			if store.AccountAuthActive(got) == terminal || got.Name != acc.Name || got.Weight != acc.Weight || !got.Enabled || got.WarpRefreshToken != acc.WarpRefreshToken {
				t.Fatal("incorrect auth status or unrelated fields overwritten")
			}
			if terminal && (got.StatusCode != "401" || got.StatusMessage == "" || got.VerifiedAt.IsZero()) {
				t.Fatal("terminal verdict not visible to operators")
			}
			if _, err := newClientWithStore(acc, testConfig(srv.URL), s).accessToken(t.Context()); err == nil || calls.Load() != 1 {
				t.Fatal("fresh client retried rejected credentials")
			}
		})
	}
}

func TestLateTerminalVerdictCannotPoisonReplacementCredentials(t *testing.T) {
	s := newAuthStateTestStore(t)
	acc := testAccount()
	acc.WarpExpiresAt = time.Now().Add(-time.Hour)
	if err := s.CreateAccount(t.Context(), acc); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := s.UpdateWarpCredentials(t.Context(), acc.ID, store.WarpCredentialPatch{ExpectedRefreshToken: acc.WarpRefreshToken, AccessToken: "synthetic-replacement", RefreshToken: "synthetic-new-refresh", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
			t.Error(err)
		}
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"message":"INVALID_REFRESH_TOKEN"}}`))
	}))
	defer srv.Close()
	c := newClientWithStore(acc, testConfig(srv.URL), s)
	key := c.authFailureKey(credentials(acc))
	t.Cleanup(func() { forgetAuthFailure(key) })
	if _, err := c.accessToken(context.Background()); !errors.Is(err, errPersistence) {
		t.Fatalf("stale verdict was not rejected: %v", err)
	}
	got, err := s.GetAccount(t.Context(), acc.ID)
	if err != nil || !store.AccountAuthActive(got) || got.WarpAccessToken != "synthetic-replacement" {
		t.Fatal("replacement poisoned by late auth failure")
	}
	if token, err := newClientWithStore(acc, testConfig(srv.URL), s).accessToken(t.Context()); err != nil || token != "synthetic-replacement" {
		t.Fatalf("replacement unavailable: %v", err)
	}
}
