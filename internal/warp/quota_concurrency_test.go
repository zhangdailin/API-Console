package warp

import (
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"orchids-api/internal/store"
)

// A bounded barrier makes failures terminate without leaving a blocked server.
func quotaAwaitSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for quota request barrier")
	}
}

func TestFetchQuotaOverlappingReadsKeepLaterStart(t *testing.T) {
	ctx := t.Context()
	s := catalogRotationStore(t)
	acc := testAccount()
	if err := s.CreateAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseFirst) }) }
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/graphql/v2" || r.URL.Query().Get("op") != "GetRequestLimitInfo" {
			t.Error("unexpected request in overlapping quota reads")
			w.WriteHeader(http.StatusNotFound)
			return
		}
		used := "20"
		if calls.Add(1) == 1 {
			close(firstStarted)
			select {
			case <-releaseFirst:
			case <-r.Context().Done():
				return
			}
			used = "10"
		}
		_, _ = io.WriteString(w, quotaTestResponse("["+quotaTestWorkspace("100", used)+"]", "null"))
	}))
	t.Cleanup(server.Close)
	t.Cleanup(release)
	c := NewFromAccount(acc, testConfig(server.URL))
	type result struct {
		quota store.WarpQuotaSnapshot
		err   error
	}
	firstResult := make(chan result, 1)
	go func() {
		quota, err := c.FetchQuota(ctx)
		firstResult <- result{quota: quota, err: err}
	}()
	quotaAwaitSignal(t, firstStarted)
	// The first request is already in HTTP. Cross a real clock tick before B
	// starts, rather than assuming a sleep or injecting a production clock.
	barrierAt := time.Now().UTC()
	deadline := time.Now().Add(5 * time.Second)
	for !time.Now().UTC().After(barrierAt) {
		if time.Now().After(deadline) {
			t.Fatal("wall clock did not advance at quota start barrier")
		}
		time.Sleep(time.Microsecond)
	}
	second, err := c.FetchQuota(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateWarpQuota(ctx, acc.ID, acc.WarpRefreshToken, second); err != nil {
		t.Fatal(err)
	}
	release()
	var first result
	select {
	case first = <-firstResult:
	case <-time.After(5 * time.Second):
		t.Fatal("older quota request did not finish after release")
	}
	if first.err != nil {
		t.Fatal(first.err)
	}
	if !first.quota.AttemptedAt.Before(second.AttemptedAt) || !second.AttemptedAt.After(barrierAt) {
		t.Fatal("overlapping quota observations were not ordered strictly by request start")
	}
	for _, quota := range []store.WarpQuotaSnapshot{first.quota, second} {
		if !quota.SyncedAt.Equal(quota.AttemptedAt) {
			t.Fatal("successful quota sync timestamp describes completion instead of request start")
		}
		if err := quota.Validate(); err != nil {
			t.Fatalf("overlapping observation violates store contract: %v", err)
		}
	}
	quotaAssertNumber(t, first.quota.Windows[0].Used, 10)
	quotaAssertNumber(t, second.Windows[0].Used, 20)
	if err := s.UpdateWarpQuota(ctx, acc.ID, acc.WarpRefreshToken, first.quota); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAccount(ctx, acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || !reflect.DeepEqual(got.WarpQuota, second) {
		t.Fatal("late completion of the older HTTP read replaced the later-started quota")
	}
}

func TestRefreshQuotaRotationDuringReadCannotRelabelObservations(t *testing.T) {
	ctx := t.Context()
	s := catalogRotationStore(t)
	acc := testAccount()
	acc.WarpModelsSyncedAt = time.Now().Add(-2 * time.Hour).UTC()
	acc.WarpQuota = store.WarpQuotaSnapshot{Status: "error", Source: quotaSource, AttemptedAt: acc.WarpModelsSyncedAt}
	if err := s.CreateAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}
	callerBefore := acc.Clone()
	started := make(chan struct{})
	releaseRead := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseRead) }) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/graphql/v2" || r.URL.Query().Get("op") != "GetRequestLimitInfo" || r.Header.Get("Authorization") != "Bearer synthetic-access" {
			t.Error("quota did not use the original request generation")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		close(started)
		select {
		case <-releaseRead:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, quotaTestResponse("["+quotaTestWorkspace("100", "90")+"]", "null"))
	}))
	t.Cleanup(server.Close)
	t.Cleanup(release)
	c := newClientWithStore(acc, testConfig(server.URL), s)
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.refreshQuota(ctx, acc, s)
	}()
	quotaAwaitSignal(t, started)
	if err := s.UpdateWarpCredentials(ctx, acc.ID, store.WarpCredentialPatch{
		ExpectedRefreshToken: acc.WarpRefreshToken, AccessToken: "synthetic-new-login-access",
		RefreshToken: "synthetic-new-login-refresh", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateWarpCatalog(ctx, acc.ID, "synthetic-new-login-refresh", []string{"new-login-model"}, callerBefore.WarpModelsSyncedAt.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// A concurrent operation on this same Client adopts the authoritative new
	// generation while the old-generation HTTP response is still outstanding.
	if token, err := c.accessToken(ctx); err != nil || token != "synthetic-new-login-access" {
		t.Fatalf("client did not adopt the replacement generation: %v", err)
	}
	beforeReturn, err := s.GetAccount(ctx, acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	release()
	quotaAwaitSignal(t, done)
	if !reflect.DeepEqual(credentials(acc), credentials(callerBefore)) || !reflect.DeepEqual(acc.WarpModelIDs, callerBefore.WarpModelIDs) || !acc.WarpModelsSyncedAt.Equal(callerBefore.WarpModelsSyncedAt) || !reflect.DeepEqual(acc.WarpQuota, callerBefore.WarpQuota) {
		t.Fatal("old quota read relabeled caller credentials, catalog, or quota as the newer login")
	}
	got, err := s.GetAccount(ctx, acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, beforeReturn) {
		t.Fatal("old-generation quota read changed the replacement login's stored observations")
	}
	if err := s.UpdateAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetAccount(ctx, acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(credentials(got), credentials(beforeReturn)) || !reflect.DeepEqual(got.WarpModelIDs, beforeReturn.WarpModelIDs) || !got.WarpModelsSyncedAt.Equal(beforeReturn.WarpModelsSyncedAt) || !reflect.DeepEqual(got.WarpQuota, beforeReturn.WarpQuota) {
		t.Fatal("generic save relabeled stale caller observations under the replacement login")
	}
}

func TestRefreshQuotaNewGenerationDoesNotRelabelCallerCatalog(t *testing.T) {
	ctx := t.Context()
	s := catalogRotationStore(t)
	acc := testAccount()
	acc.WarpModelsSyncedAt = time.Now().Add(-2 * time.Hour).UTC()
	if err := s.CreateAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}
	original := acc.Clone()
	if err := s.UpdateWarpCredentials(ctx, acc.ID, store.WarpCredentialPatch{
		ExpectedRefreshToken: acc.WarpRefreshToken, AccessToken: "synthetic-before-read-access",
		RefreshToken: "synthetic-before-read-refresh", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	// Give the replacement login its own catalog, then make the caller's old
	// catalog timestamp newer. Freshness alone cannot authorize that catalog.
	current, err := s.GetAccount(ctx, acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	current.WarpModelIDs = []string{"replacement-login-model"}
	current.WarpModelsSyncedAt = original.WarpModelsSyncedAt.Add(time.Hour)
	if err := s.UpdateAccount(ctx, current); err != nil {
		t.Fatal(err)
	}
	acc.WarpModelsSyncedAt = current.WarpModelsSyncedAt.Add(time.Hour)
	callerCatalogAt := acc.WarpModelsSyncedAt
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("op") != "GetRequestLimitInfo" || r.Header.Get("Authorization") != "Bearer synthetic-before-read-access" {
			t.Error("quota read did not use the authoritative replacement login")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, quotaTestResponse("["+quotaTestWorkspace("100", "30")+"]", "null"))
	}))
	defer server.Close()
	c := newClientWithStore(acc, testConfig(server.URL), s)
	c.refreshQuota(ctx, acc, s)
	if !reflect.DeepEqual(credentials(acc), credentials(original)) || !reflect.DeepEqual(acc.WarpModelIDs, original.WarpModelIDs) || !acc.WarpModelsSyncedAt.Equal(callerCatalogAt) {
		t.Fatal("quota refresh copied newer credentials over the caller's old-generation catalog")
	}
	beforeSave, err := s.GetAccount(ctx, acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if beforeSave.WarpQuota.Status != "ok" || len(beforeSave.WarpQuota.Windows) != 1 {
		t.Fatal("quota authenticated by the newer login was not persisted")
	}
	quotaAssertNumber(t, beforeSave.WarpQuota.Windows[0].Used, 30)
	if err := s.UpdateAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAccount(ctx, acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(credentials(got), credentials(beforeSave)) || !reflect.DeepEqual(got.WarpModelIDs, beforeSave.WarpModelIDs) || !got.WarpModelsSyncedAt.Equal(beforeSave.WarpModelsSyncedAt) || !reflect.DeepEqual(got.WarpQuota, beforeSave.WarpQuota) {
		t.Fatal("generic save adopted old catalog observations under the newer quota login")
	}
}
