package workbuddy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func TestTokenUpdater_PersistsRotatedRefreshToken(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		testutil.CheckEqual(t, r.Header.Get("X-Refresh-Token"), "old-refresh")
		testutil.CheckEqual(t, r.Header.Get("X-Auth-Refresh-Source"), "plugin")
		_, _ = w.Write([]byte(`{"code":0,"data":{"accessToken":"new-access","refreshToken":"new-refresh","expiresIn":86400}}`))
	}))
	defer srv.Close()

	acc := &store.Account{ID: 7, AccountType: "workbuddy", WorkBuddyRefreshToken: "old-refresh"}
	updater := &fakeUpdater{}
	client := NewFromAccount(acc, nil)
	client.baseURL = srv.URL
	client.httpClient = srv.Client()
	client.SetAccountStore(updater)

	refreshed, err := newTokenUpdater(srv.URL, srv.Client(), updater, acc).
		RefreshNow(context.Background(), Credentials{RefreshToken: "old-refresh"})
	testutil.NoError(t, err, "RefreshNow() error = %v")
	testutil.Equal(t, refreshed.AccessToken, "new-access")
	testutil.False(t, updater.saved == nil, "rotated refresh token was not persisted")
	testutil.Equal(t, updater.saved.WorkBuddyRefreshToken, "new-refresh")
	testutil.Equal(t, acc.WorkBuddyRefreshToken, "old-refresh")
	testutil.Equal(t, acc.WorkBuddyAccessToken, "")
}

func TestClientConcurrentFirstUseRefreshesOnlyOnce(t *testing.T) {
	t.Parallel()

	var refreshes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshes.Add(1)
		_, _ = w.Write([]byte(`{"code":0,"data":{"accessToken":"new-access","refreshToken":"new-refresh","expiresIn":172800}}`))
	}))
	defer srv.Close()

	client := NewFromAccount(&store.Account{
		AccountType:           "workbuddy",
		WorkBuddyRefreshToken: "old-refresh",
	}, nil)
	client.baseURL = srv.URL
	client.httpClient = srv.Client()

	const callers = 24
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token, err := client.ensureAccessToken(context.Background())
			if err == nil && token != "new-access" {
				err = fmt.Errorf("token = %q", token)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		testutil.NoError(t, err)
	}
	testutil.Equal(t, refreshes.Load(), 1)
}

type fakeUpdater struct {
	saved *store.Account
}

type flakyUpdater struct {
	fail  bool
	calls int
}

func (f *flakyUpdater) UpdateWorkBuddyCredentials(_ context.Context, _ int64, _ store.WorkBuddyCredentialPatch) error {
	f.calls++
	if f.fail {
		return errors.New("write failed")
	}
	return nil
}

func TestTokenUpdaterReportsPersistenceFailureAndRetriesWithoutRotatingAgain(t *testing.T) {
	t.Parallel()
	refreshes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshes++
		_, _ = w.Write([]byte(`{"code":0,"data":{"accessToken":"new-access","refreshToken":"new-refresh","expiresIn":172800}}`))
	}))
	defer srv.Close()

	storeUpdater := &flakyUpdater{fail: true}
	updater := newTokenUpdater(srv.URL, srv.Client(), storeUpdater, &store.Account{ID: 1, AccountType: "workbuddy"})
	_, err := updater.RefreshNow(context.Background(), Credentials{RefreshToken: "old-refresh"})
	testutil.Error(t, err)
	storeUpdater.fail = false
	token, err := updater.Token(context.Background(), Credentials{RefreshToken: "old-refresh"})
	testutil.NoError(t, err)
	testutil.Equal(t, token, "new-access")
	testutil.Equal(t, refreshes, 1)
	testutil.Equal(t, storeUpdater.calls, 2)
}
