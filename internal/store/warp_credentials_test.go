package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"orchids-api/internal/testutil"
)

func newWarpTestStore(t *testing.T) (*Store, *miniredis.Miniredis) {
	t.Helper()
	mini := miniredis.RunT(t)
	s, err := New(Options{RedisAddr: mini.Addr(), RedisPrefix: "warp-test:", CredentialEncryptionKey: bytes.Repeat([]byte{0x42}, 32)})
	testutil.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s, mini
}

func TestWarpCredentialsEncryptedAndRotated(t *testing.T) {
	s, mini := newWarpTestStore(t)
	acc := &Account{AccountType: "warp", Enabled: true, WarpAccessToken: "warp-access-initial", WarpRefreshToken: "warp-refresh-initial"}
	testutil.NoError(t, s.CreateAccount(t.Context(), acc))
	expires := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, patch := range []WarpCredentialPatch{
		{},
		{ExpectedRefreshToken: acc.WarpRefreshToken, AccessToken: " warp-access-rotated ", RefreshToken: " warp-refresh-rotated ", ExpiresAt: expires},
	} {
		testutil.NoError(t, s.UpdateWarpCredentials(t.Context(), acc.ID, patch))
	}
	got, err := s.GetAccount(t.Context(), acc.ID)
	testutil.NoError(t, err)
	testutil.Equal(t, got.WarpAccessToken, "warp-access-rotated")
	testutil.Equal(t, got.WarpRefreshToken, "warp-refresh-rotated")
	testutil.True(t, got.WarpExpiresAt.Equal(expires), "expiry was not persisted")
	for _, value := range []string{got.WarpAccessToken, got.WarpRefreshToken} {
		testutil.True(t, slices.Contains(got.Secrets(), value), "Warp credential missing from Secrets")
	}
	raw, err := mini.Get(fmt.Sprintf("warp-test:accounts:id:%d", acc.ID))
	testutil.NoError(t, err)
	for _, value := range []string{acc.WarpAccessToken, acc.WarpRefreshToken, got.WarpAccessToken, got.WarpRefreshToken} {
		testutil.MustNotContain(t, raw, value)
	}
	var row map[string]any
	testutil.NoError(t, json.Unmarshal([]byte(raw), &row))
	for _, key := range []string{"warp_access_token", "warp_refresh_token"} {
		value, ok := row[key].(string)
		testutil.True(t, ok && strings.HasPrefix(value, encryptedCredentialPrefix), "Warp field was not encrypted")
	}
	// The same successful rotation is idempotent; a different stale result loses.
	patch := WarpCredentialPatch{ExpectedRefreshToken: acc.WarpRefreshToken, AccessToken: got.WarpAccessToken, RefreshToken: got.WarpRefreshToken, ExpiresAt: expires}
	testutil.NoError(t, s.UpdateWarpCredentials(t.Context(), acc.ID, patch))
	patch.RefreshToken = "warp-stale-result"
	testutil.Error(t, s.UpdateWarpCredentials(t.Context(), acc.ID, patch))
	// Empty patches never erase a pair or expiry.
	testutil.NoError(t, s.UpdateWarpCredentials(t.Context(), acc.ID, WarpCredentialPatch{}))
	again, err := s.GetAccount(t.Context(), acc.ID)
	testutil.NoError(t, err)
	testutil.Equal(t, again.WarpRefreshToken, got.WarpRefreshToken)
	testutil.True(t, again.WarpExpiresAt.Equal(expires), "empty patch cleared expiry")
}

func TestWarpEncryptedStoreRejectsPlaintextCredentialSlots(t *testing.T) {
	for _, field := range []string{"warp_access_token", "warp_refresh_token"} {
		t.Run(field, func(t *testing.T) {
			s, mini := newWarpTestStore(t)
			body := fmt.Sprintf(`{"id":1,"account_type":"warp","%s":"plaintext-probe"}`, field)
			mini.Set("warp-test:accounts:id:1", body)
			_, err := s.GetAccount(t.Context(), 1)
			testutil.Error(t, err)
			stored, err := mini.Get("warp-test:accounts:id:1")
			testutil.NoError(t, err)
			testutil.Equal(t, stored, body)
		})
	}
}

func TestWarpCredentialUpdateRequiresConfiguredStore(t *testing.T) {
	var absent *Store
	testutil.Error(t, absent.UpdateWarpCredentials(t.Context(), 1, WarpCredentialPatch{}))
	testutil.Error(t, (&Store{}).UpdateWarpCredentials(t.Context(), 1, WarpCredentialPatch{}))
}

func TestWarpSnapshotsPreserveRotationAndCatalog(t *testing.T) {
	s, _ := newWarpTestStore(t)
	observed := time.Date(2030, 1, 2, 0, 0, 0, 0, time.UTC)
	acc := &Account{AccountType: "warp", Name: "original", Enabled: true, WarpAccessToken: "old-access", WarpRefreshToken: "old-refresh", WarpModelIDs: []string{"old-model"}, WarpModelsSyncedAt: observed}
	testutil.NoError(t, s.CreateAccount(t.Context(), acc))
	stale := acc.Clone()
	testutil.NoError(t, s.UpdateWarpCredentials(t.Context(), acc.ID, WarpCredentialPatch{ExpectedRefreshToken: "old-refresh", AccessToken: "new-access", RefreshToken: "new-refresh"}))
	current, err := s.GetAccount(t.Context(), acc.ID)
	testutil.NoError(t, err)
	current.WarpModelIDs = []string{"new-model"}
	current.WarpModelsSyncedAt = observed.Add(time.Hour)
	testutil.NoError(t, s.UpdateAccount(t.Context(), current))
	stale.Name, stale.Enabled = "admin edit", false
	testutil.NoError(t, s.UpdateAccount(t.Context(), stale))
	got, err := s.GetAccount(t.Context(), acc.ID)
	testutil.NoError(t, err)
	testutil.Equal(t, got.Name, "admin edit")
	testutil.False(t, got.Enabled, "rotation lost admin disable")
	testutil.Equal(t, got.WarpAccessToken, "new-access")
	testutil.Equal(t, got.WarpRefreshToken, "new-refresh")
	testutil.True(t, slices.Equal(got.WarpModelIDs, []string{"new-model"}), "stale snapshot rewound catalog")
	testutil.True(t, got.WarpModelsSyncedAt.Equal(current.WarpModelsSyncedAt), "stale snapshot rewound catalog timestamp")
	// A partial account update must keep both independently refreshed subjects.
	testutil.NoError(t, s.UpdateAccount(t.Context(), &Account{ID: acc.ID, AccountType: "warp"}))
	got, err = s.GetAccount(t.Context(), acc.ID)
	testutil.NoError(t, err)
	testutil.Equal(t, got.WarpRefreshToken, "new-refresh")
	testutil.True(t, slices.Equal(got.WarpModelIDs, []string{"new-model"}), "partial update cleared catalog")
	// Explicit official-login replacement is distinct from an ordinary snapshot.
	got.ReplaceWarpCredentials = true
	got.WarpAccessToken, got.WarpRefreshToken = " replacement-access ", " replacement-refresh "
	testutil.NoError(t, s.UpdateAccount(t.Context(), got))
	got, err = s.GetAccount(t.Context(), acc.ID)
	testutil.NoError(t, err)
	testutil.Equal(t, got.WarpRefreshToken, "replacement-refresh")
	testutil.False(t, got.ReplaceWarpCredentials, "write intent escaped persistence")
}

func TestWarpConcurrentRotationsHaveOneWinner(t *testing.T) {
	s, _ := newWarpTestStore(t)
	acc := &Account{AccountType: "warp", Name: "original", Enabled: true, WarpRefreshToken: "original-refresh"}
	testutil.NoError(t, s.CreateAccount(t.Context(), acc))
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Go(func() {
			<-start
			errs <- s.UpdateWarpCredentials(t.Context(), acc.ID, WarpCredentialPatch{ExpectedRefreshToken: "original-refresh", AccessToken: fmt.Sprintf("access-%d", i), RefreshToken: fmt.Sprintf("refresh-%d", i)})
		})
	}
	admin := acc.Clone()
	admin.Name, admin.Enabled = "edited concurrently", false
	wg.Go(func() {
		<-start
		testutil.CheckNoError(t, s.UpdateAccount(t.Context(), admin))
	})
	close(start)
	wg.Wait()
	close(errs)
	var successful, rejected int
	for err := range errs {
		if err == nil {
			successful++
		} else {
			testutil.MustContain(t, err.Error(), "warp credential changed concurrently")
			rejected++
		}
	}
	testutil.Equal(t, successful, 1)
	testutil.Equal(t, rejected, 1)
	got, err := s.GetAccount(t.Context(), acc.ID)
	testutil.NoError(t, err)
	testutil.Equal(t, got.Name, admin.Name)
	testutil.False(t, got.Enabled, "credential refresh re-enabled account")
	testutil.Equal(t, strings.TrimPrefix(got.WarpAccessToken, "access-"), strings.TrimPrefix(got.WarpRefreshToken, "refresh-"))
}

func TestWarpAccountCloneIsolatesCatalogAndWriteIntentIsPrivate(t *testing.T) {
	acc := &Account{WarpModelIDs: []string{"model-a"}, ReplaceWarpCredentials: true, WarpAccessToken: "clone-access", WarpRefreshToken: "clone-refresh"}
	clone := acc.Clone()
	clone.WarpModelIDs[0] = "model-b"
	testutil.Equal(t, acc.WarpModelIDs[0], "model-a")
	testutil.Equal(t, clone.WarpAccessToken, acc.WarpAccessToken)
	raw, err := json.Marshal(acc)
	testutil.NoError(t, err)
	testutil.MustNotContain(t, string(raw), "ReplaceWarpCredentials")
	testutil.MustNotContain(t, string(raw), "replace_warp_credentials")
	var decoded Account
	testutil.NoError(t, json.Unmarshal([]byte(`{"replace_warp_credentials":true,"ReplaceWarpCredentials":true}`), &decoded))
	testutil.False(t, decoded.ReplaceWarpCredentials, "JSON supplied write intent")
}
