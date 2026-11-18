package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"orchids-api/internal/config"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/provider"
	"orchids-api/internal/store"
)

func warpSelectionTest(t *testing.T, serve func(http.ResponseWriter, *http.Request)) (*Handler, *store.Store, *store.Account) {
	t.Helper()
	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{RedisAddr: mini.Addr(), RedisPrefix: "warp-selection:", CredentialEncryptionKey: []byte("01234567890123456789012345678901")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	srv := httptest.NewServer(http.HandlerFunc(serve))
	t.Cleanup(srv.Close)
	h := NewWithLoadBalancer(&config.Config{WarpBaseURL: srv.URL, WarpTokenBaseURL: srv.URL, WarpFirebaseBaseURL: srv.URL}, loadbalancer.NewWithCacheTTL(s, time.Hour))
	t.Cleanup(h.Close)
	acc := &store.Account{AccountType: "warp", Name: "admin name", Weight: 3, Enabled: true, WarpAccessToken: "synthetic-access", WarpRefreshToken: "synthetic-refresh", WarpExpiresAt: time.Now().Add(time.Hour), WarpModelIDs: []string{"model-a"}, WarpModelsSyncedAt: time.Now().Add(-time.Hour)}
	if err := s.CreateAccount(t.Context(), acc); err != nil {
		t.Fatal(err)
	}
	return h, s, acc
}

const selectionCatalog = `{"data":{"user":{"__typename":"UserOutput","user":{"workspaces":[{"featureModelChoice":{"agentMode":{"choices":[{"id":"model-a","contextWindow":{"default":200000}}]}}}]}}}}`

func TestWarpSelectionPrefersFreshEligibleAccount(t *testing.T) {
	var calls atomic.Int64
	h, s, acc := warpSelectionTest(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) })
	fresh := acc.Clone()
	fresh.ID = 0
	fresh.WarpModelsSyncedAt = time.Now()
	if err := s.CreateAccount(t.Context(), fresh); err != nil {
		t.Fatal(err)
	}
	got, err := h.selectAccountRecord(t.Context(), "warp", nil, "model-a")
	if err != nil || got.ID != fresh.ID || calls.Load() != 0 {
		t.Fatal("stale account displaced a ready account or caused unnecessary refresh")
	}
}

func TestWarpSelectionRefreshesConcurrentStaleCandidatesOnce(t *testing.T) {
	var catalogs, paid atomic.Int64
	h, s, acc := warpSelectionTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ai/multi-agent" {
			paid.Add(1)
			t.Error("selection generated upstream")
			return
		}
		if r.URL.Path != "/graphql/v2" || r.URL.Query().Get("op") != "GetFeatureModelChoices" {
			t.Error("unexpected control-plane read")
			w.WriteHeader(500)
			return
		}
		catalogs.Add(1)
		_, _ = fmt.Fprint(w, selectionCatalog)
	})
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			got, err := h.selectAccountRecord(t.Context(), "warp", nil, "model-a")
			if err != nil {
				t.Error(err)
				return
			}
			if got.ID != acc.ID || time.Since(got.WarpModelsSyncedAt) > time.Minute || got.WarpModelWindows["model-a"] != 200000 {
				t.Error("selection retained stale client/catalog state")
			}
		})
	}
	wg.Wait()
	if catalogs.Load() != 1 || paid.Load() != 0 {
		t.Fatalf("catalog=%d generation=%d", catalogs.Load(), paid.Load())
	}
	got, err := s.GetAccount(t.Context(), acc.ID)
	if err != nil || got.Name != "admin name" || got.Weight != 3 {
		t.Fatal("catalog write overwrote admin state")
	}
}

func TestWarpSelectionDoesNotUseFailedOrReplacedCatalog(t *testing.T) {
	for _, mode := range []string{"unavailable", "disabled", "new-login", "unsupported"} {
		t.Run(mode, func(t *testing.T) {
			var s *store.Store
			var acc *store.Account
			var calls atomic.Int64
			h, db, account := warpSelectionTest(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != "/graphql/v2" {
					t.Error("refresh escaped catalog endpoint")
				}
				if mode == "unavailable" {
					w.WriteHeader(503)
					return
				}
				if mode == "disabled" || mode == "new-login" {
					latest, err := s.GetAccount(t.Context(), acc.ID)
					if err != nil {
						t.Error(err)
						return
					}
					if mode == "disabled" {
						latest.Enabled = false
						latest.Name = "disabled by administrator"
					} else {
						latest.ReplaceWarpCredentials = true
						latest.WarpAccessToken = "synthetic-new-access"
						latest.WarpRefreshToken = "synthetic-new-refresh"
					}
					if err := s.UpdateAccount(t.Context(), latest); err != nil {
						t.Error(err)
					}
				}
				response := selectionCatalog
				if mode == "unsupported" {
					response = `{"data":{"user":{"__typename":"UserOutput","user":{"workspaces":[{"featureModelChoice":{"agentMode":{"choices":[{"id":"other-model"}]}}}]}}}}`
				}
				_, _ = fmt.Fprint(w, response)
			})
			s, acc = db, account
			if got, err := h.selectAccountRecord(t.Context(), "warp", nil, "model-a"); err == nil || got != nil {
				t.Fatal("invalid refreshed state entered the pool")
			}
			if calls.Load() != 1 {
				t.Fatalf("unexpected control-plane retries: %d", calls.Load())
			}
			latest, err := s.GetAccount(t.Context(), acc.ID)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "disabled" && (latest.Enabled || latest.Name != "disabled by administrator") {
				t.Fatal("refresh undid administrator disable")
			}
			if mode == "new-login" && (latest.WarpRefreshToken != "synthetic-new-refresh" || time.Since(latest.WarpModelsSyncedAt) < provider.HealthRefreshInterval) {
				t.Fatal("old generation wrote catalog into new login")
			}
		})
	}
}

func TestWarpSelectionRefreshBudgetAndCancellation(t *testing.T) {
	var calls atomic.Int64
	h, s, acc := warpSelectionTest(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(503) })
	for range 4 {
		copy := acc.Clone()
		copy.ID = 0
		if err := s.CreateAccount(t.Context(), copy); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.selectAccountRecord(t.Context(), "warp", nil, "model-a"); err == nil || calls.Load() != 3 {
		t.Fatalf("refresh budget not enforced: calls=%d err=%v", calls.Load(), err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := h.selectAccountRecord(ctx, "warp", nil, "model-a"); err == nil || calls.Load() != 3 {
		t.Fatal("cancelled selection started control-plane work")
	}
}
