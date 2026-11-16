package handler

import (
	"orchids-api/internal/config"
	"orchids-api/internal/store"
	"testing"
	"time"
)

func TestWarpClientFingerprintIncludesRotationAndCatalogFreshness(t *testing.T) {
	base := &store.Account{ID: 42, AccountType: "warp", WarpAccessToken: "test-access", WarpRefreshToken: "test-refresh", WarpExpiresAt: time.Now().Add(time.Hour), WarpModelIDs: []string{"model-a"}, WarpModelsSyncedAt: time.Now()}
	cfg := &config.Config{}
	before := accountClientFingerprint(base, cfg)
	for _, edit := range []func(*store.Account){
		func(a *store.Account) { a.WarpAccessToken = "rotated" }, func(a *store.Account) { a.WarpRefreshToken = "rotated" },
		func(a *store.Account) { a.WarpExpiresAt = a.WarpExpiresAt.Add(time.Hour) }, func(a *store.Account) { a.WarpModelIDs = []string{"model-b"} }, func(a *store.Account) { a.WarpModelsSyncedAt = a.WarpModelsSyncedAt.Add(time.Minute) },
	} {
		next := base.Clone()
		edit(next)
		if accountClientFingerprint(next, cfg) == before {
			t.Fatal("Warp captured state missing from fingerprint")
		}
	}
	for _, edit := range []func(*config.Config){func(c *config.Config) { c.WarpBaseURL = "https://test.invalid" }, func(c *config.Config) { c.WarpFirebaseBaseURL = "https://test.invalid" }, func(c *config.Config) { c.WarpTokenBaseURL = "https://test.invalid" }, func(c *config.Config) { c.WarpFirebaseAPIKey = "test-config" }} {
		next := cfg.Clone()
		edit(next)
		if accountClientFingerprint(base, next) == before {
			t.Fatal("Warp endpoint/config not invalidating client")
		}
	}
}
