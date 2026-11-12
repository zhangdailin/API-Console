package grok

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"orchids-api/internal/config"
	"orchids-api/internal/provider"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func TestAutoCleanDeletesOnlyLongStandingReauthAccounts(t *testing.T) {
	mini := miniredis.RunT(t)
	st, err := store.New(store.Options{RedisAddr: mini.Addr(), RedisPrefix: "auto-clean:"})
	testutil.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	now := time.Now()

	create := func(name, auth string, since time.Time) *store.Account {
		acc := &store.Account{Name: name, AccountType: "grok", GrokProvider: ProviderBuild, CredentialType: "oauth",
			Enabled: true, AuthStatus: auth, StatusCode: "401", LastAttempt: since}
		testutil.NoError(t, st.CreateAccount(ctx, acc))
		return acc
	}
	stale := create("stale", store.AccountAuthStatusReauthRequired, now.Add(-49*time.Hour))
	recent := create("recent", store.AccountAuthStatusReauthRequired, now.Add(-time.Hour))
	unstamped := create("unstamped", store.AccountAuthStatusReauthRequired, time.Time{})
	healthy := create("healthy", store.AccountAuthStatusActive, now.Add(-49*time.Hour))
	busy := create("busy", store.AccountAuthStatusReauthRequired, now.Add(-49*time.Hour))
	testutil.NoError(t, st.SaveStoredVideoJob(ctx, &store.StoredVideoJob{ID: "video_1", OwnerHash: "owner", AccountID: busy.ID, Model: "grok-imagine-video", Status: "in_progress"}, time.Hour))

	// Off by default.
	off := provider.Env{Config: &config.Config{}, Store: st}
	testutil.False(t, autoCleanReauthAccount(ctx, off, stale, now))

	env := provider.Env{Config: &config.Config{GrokAutoCleanReauthHours: 48}, Store: st}
	testutil.True(t, autoCleanReauthAccount(ctx, env, stale, now))
	for _, kept := range []*store.Account{recent, unstamped, healthy, busy} {
		testutil.False(t, autoCleanReauthAccount(ctx, env, kept, now), kept.Name)
		_, err := st.GetAccount(ctx, kept.ID)
		testutil.NoError(t, err)
	}
	_, err = st.GetAccount(ctx, stale.ID)
	testutil.True(t, err != nil, "the stale account is gone")

	// A non-Grok account is never this channel's to delete.
	other := &store.Account{ID: 99, AccountType: "cline", AuthStatus: store.AccountAuthStatusReauthRequired, LastAttempt: now.Add(-100 * time.Hour)}
	testutil.False(t, autoCleanReauthAccount(ctx, env, other, now))
}
