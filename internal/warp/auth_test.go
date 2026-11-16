package warp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"orchids-api/internal/store"
)

type flakyCredentialStore struct {
	acc   store.Account
	calls int
	fail  bool
}

func (s *flakyCredentialStore) GetAccount(context.Context, int64) (*store.Account, error) {
	copy := s.acc
	return &copy, nil
}
func (s *flakyCredentialStore) UpdateWarpCredentials(_ context.Context, _ int64, p store.WarpCredentialPatch) error {
	s.calls++
	if s.fail {
		s.fail = false
		return errors.New("private persistence detail")
	}
	if p.ExpectedRefreshToken != s.acc.WarpRefreshToken {
		return errors.New("stale refresh")
	}
	s.acc.WarpAccessToken = p.AccessToken
	s.acc.WarpRefreshToken = p.RefreshToken
	s.acc.WarpExpiresAt = p.ExpiresAt
	return nil
}
func TestFailedPersistenceDoesNotSpendRotatingGrantAgain(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"id_token":"renewed","refresh_token":"rotated","expires_in":"3600"}`))
	}))
	defer server.Close()
	acc := testAccount()
	acc.ID = 1
	acc.WarpExpiresAt = time.Now().Add(-time.Hour)
	owner := &flakyCredentialStore{acc: *acc, fail: true}
	c := NewFromAccount(acc, testConfig(server.URL))
	c.SetAccountStore(owner)
	if _, err := c.accessToken(context.Background()); !errors.Is(err, errPersistence) {
		t.Fatal("failed persistence must stop use")
	}
	token, err := c.accessToken(context.Background())
	if err != nil || token != "renewed" || calls != 1 || owner.calls != 2 {
		t.Fatal("rotating token replayed or not durably retried")
	}
}
func TestRefreshReadsLatestAccountBeforeUsingOldGrant(t *testing.T) {
	acc := testAccount()
	acc.ID = 1
	acc.WarpExpiresAt = time.Now().Add(-time.Hour)
	latest := *acc
	latest.WarpAccessToken = "latest"
	latest.WarpRefreshToken = "replaced"
	latest.WarpExpiresAt = time.Now().Add(time.Hour)
	owner := &flakyCredentialStore{acc: latest}
	c := NewFromAccount(acc, testConfig("http://127.0.0.1:1"))
	c.SetAccountStore(owner)
	token, err := c.accessToken(context.Background())
	if err != nil || token != "latest" || owner.calls != 0 {
		t.Fatal("stale grant was used after replacement")
	}
}
