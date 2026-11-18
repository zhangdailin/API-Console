package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"orchids-api/internal/store"
)

func TestWarpRouteRefreshesCatalogBeforeOneGeneration(t *testing.T) {
	for _, mode := range []string{"success", "unavailable", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			generation := newWarpRouteStub(t, http.StatusOK)
			var catalogs atomic.Int64
			var s *store.Store
			var accountID int64
			control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/graphql/v2" {
					generation.Config.Handler.ServeHTTP(w, r)
					return
				}
				if r.URL.Query().Get("op") != "GetFeatureModelChoices" {
					t.Error("unexpected control-plane operation")
					w.WriteHeader(500)
					return
				}
				catalogs.Add(1)
				if mode == "unavailable" {
					w.WriteHeader(503)
					return
				}
				if mode == "disabled" {
					acc, err := s.GetAccount(t.Context(), accountID)
					if err != nil {
						t.Error(err)
						return
					}
					acc.Enabled = false
					if err := s.UpdateAccount(t.Context(), acc); err != nil {
						t.Error(err)
					}
				}
				_, _ = fmt.Fprintf(w, `{"data":{"user":{"__typename":"UserOutput","user":{"workspaces":[{"featureModelChoice":{"agentMode":{"choices":[{"id":%q}]}}}]}}}}`, warpRouteModel)
			}))
			defer control.Close()
			e, db, _ := newWarpRouteE2E(t, &warpRouteStub{Server: control}, 0)
			s = db
			acc := &store.Account{Name: "stale route account", AccountType: "warp", Enabled: true, Weight: 1,
				WarpAccessToken: "synthetic-warp-access", WarpRefreshToken: "synthetic-warp-refresh", WarpExpiresAt: time.Now().Add(time.Hour),
				WarpModelIDs: []string{warpRouteModel}, WarpModelsSyncedAt: time.Now().Add(-time.Hour)}
			if err := s.CreateAccount(t.Context(), acc); err != nil {
				t.Fatal(err)
			}
			accountID = acc.ID
			body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":%q}]}`, warpRouteModel, warpRouteQuery)
			resp := e.do(t, http.MethodPost, "/warp/chat/completions", body, false)
			out := e.readBody(t, resp)
			wantPaid := int64(0)
			if mode == "success" {
				wantPaid = 1
				if resp.StatusCode != 200 || !strings.Contains(out, warpRouteAnswer) {
					t.Fatalf("fresh selection failed: %d %s", resp.StatusCode, out)
				}
			} else if resp.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("catalog failure should be capacity, not client error: %d %s", resp.StatusCode, out)
			}
			if generation.aiCalls.Load() != wantPaid || catalogs.Load() != 1 {
				t.Fatalf("generation=%d catalog=%d", generation.aiCalls.Load(), catalogs.Load())
			}
		})
	}
}
