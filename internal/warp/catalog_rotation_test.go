package warp

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"orchids-api/internal/provider"
	"orchids-api/internal/store"
)

const rotationCatalogResponse = `{"data":{"user":{"__typename":"UserOutput","user":{"workspaces":[{"featureModelChoice":{"agentMode":{"choices":[{"id":"rotated-model","disableReason":null}]}}}]}}}}`

func catalogRotationStore(t *testing.T) *store.Store {
	t.Helper()
	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{RedisAddr: mini.Addr(), RedisPrefix: "warp-catalog-rotation:", CredentialEncryptionKey: []byte("01234567890123456789012345678901")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestCatalogRotationPersistsAuthenticatedGeneration(t *testing.T) {
	for _, flow := range []string{"discovery", "maintenance", "verification"} {
		for _, quotaFailure := range []bool{false, true} {
			if flow == "discovery" && quotaFailure {
				continue // Catalog discovery does not read quota.
			}
			for _, disable := range []bool{false, true} {
				name := flow + "/enabled"
				if disable {
					name = flow + "/disabled_during_catalog"
				}
				if quotaFailure {
					name += "/quota_failure"
				}
				t.Run(name, func(t *testing.T) {
					ctx := t.Context()
					s := catalogRotationStore(t)
					acc := testAccount()
					acc.Name = "original"
					acc.Weight = 1
					acc.WarpExpiresAt = time.Now().Add(-time.Hour)
					acc.WarpModelsSyncedAt = time.Now().Add(-provider.HealthRefreshInterval - time.Minute)
					acc.RequestCount, acc.UsageTotal = 29, 9876
					acc.UsageCurrent, acc.UsageLimit = 42, 100
					acc.TokensToday, acc.TokensDate = 1234, time.Now().UTC().Format("2006-01-02")
					acc.LastUsedAt = time.Now().Add(-time.Minute)
					limit, used, remaining := 80.0, 20.0, 60.0
					acc.WarpQuota = store.WarpQuotaSnapshot{
						Status: "ok", Source: quotaSource,
						AttemptedAt: acc.WarpModelsSyncedAt, SyncedAt: acc.WarpModelsSyncedAt,
						Windows: []store.WarpQuotaWindow{{Scope: "workspace_monthly", Unit: "credits", Limit: &limit, Used: &used, Remaining: &remaining}},
					}
					if err := s.CreateAccount(ctx, acc); err != nil {
						t.Fatal(err)
					}
					original := acc.Clone()
					var refreshes, catalogs, quotas atomic.Int64
					var beforeCatalog *store.Account
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						switch r.URL.Path {
						case "/v1/token":
							refreshes.Add(1)
							if err := r.ParseForm(); err != nil || r.Form.Get("refresh_token") != "synthetic-refresh" {
								t.Error("refresh did not use the original credential generation")
							}
							_, _ = w.Write([]byte(`{"id_token":"synthetic-catalog-access","refresh_token":"synthetic-catalog-refresh","expires_in":"3600"}`))
						case "/graphql/v2":
							if r.Header.Get("Authorization") != "Bearer synthetic-catalog-access" {
								t.Error("observation did not use the rotated access credential")
							}
							switch r.URL.Query().Get("op") {
							case "GetRequestLimitInfo":
								quotas.Add(1)
								if catalogs.Load() != 1 {
									t.Error("quota read preceded catalog authentication")
								}
								if quotaFailure {
									http.Error(w, "synthetic billing read failure", http.StatusServiceUnavailable)
									return
								}
								_, _ = w.Write([]byte(quotaTestResponse("["+quotaTestWorkspace("100", "25")+"]", `{"isUnlimited":true}`)))
								return
							case "GetFeatureModelChoices":
								catalogs.Add(1)
							default:
								t.Error("unexpected GraphQL operation")
								w.WriteHeader(http.StatusBadRequest)
								return
							}
							latest, err := s.GetAccount(ctx, acc.ID)
							if err != nil {
								t.Error(err)
								w.WriteHeader(http.StatusInternalServerError)
								return
							}
							if latest.WarpRefreshToken != "synthetic-catalog-refresh" {
								t.Error("catalog preceded durable credential rotation")
							}
							if disable {
								latest.Enabled = false
								latest.Name = "disabled by operator"
								latest.Weight = 7
								latest.MaxConcurrent = 4
								latest.AuthStatus = store.AccountAuthStatusReauthRequired
								latest.StatusCode = "401"
								latest.StatusMessage = "operator requires a new login"
								latest.LastAttempt = time.Now().UTC().Truncate(time.Second)
								latest.QuotaResetAt = latest.LastAttempt.Add(time.Hour)
								if err := s.UpdateAccount(ctx, latest); err != nil {
									t.Error(err)
									w.WriteHeader(http.StatusInternalServerError)
									return
								}
							}
							beforeCatalog, err = s.GetAccount(ctx, acc.ID)
							if err != nil {
								t.Error(err)
								w.WriteHeader(http.StatusInternalServerError)
								return
							}
							_, _ = w.Write([]byte(rotationCatalogResponse))
						default:
							t.Error("catalog flow made an unexpected request")
							w.WriteHeader(http.StatusNotFound)
						}
					}))
					defer server.Close()
					env := provider.Env{Config: testConfig(server.URL), Store: s}
					catalog := catalogPort()
					switch flow {
					case "discovery":
						observation, err := catalog.Observe(ctx, env, acc)
						if err != nil {
							t.Fatal(err)
						}
						if !reflect.DeepEqual(acc, original) {
							t.Fatal("Observe mutated the caller's account before Record")
						}
						if len(observation.Models) != 1 || observation.Models[0].ID != "rotated-model" || !observation.Record(acc, time.Now()) {
							t.Fatal("catalog did not record the successful observation")
						}
					case "maintenance":
						maintain(ctx, env, acc)
					case "verification":
						if _, status, err := adminPort().Verify(ctx, env, acc); err != nil || status != 0 {
							t.Fatalf("verification failed: status=%d error=%v", status, err)
						}
					}
					if flow != "maintenance" {
						if err := catalog.Persist(ctx, s, acc); err != nil {
							t.Fatalf("authenticated catalog credential generation was rejected: %v", err)
						}
					}
					wantQuotas := int64(1)
					if flow == "discovery" || disable {
						// A disable during catalog is authoritative before the quota read.
						wantQuotas = 0
					}
					if refreshes.Load() != 1 || catalogs.Load() != 1 || quotas.Load() != wantQuotas || beforeCatalog == nil {
						t.Fatalf("unexpected request counts: refresh=%d catalog=%d quota=%d", refreshes.Load(), catalogs.Load(), quotas.Load())
					}
					if acc.WarpAccessToken != "synthetic-catalog-access" || acc.WarpRefreshToken != "synthetic-catalog-refresh" || !credentials(acc).valid() || acc.ReplaceWarpCredentials {
						t.Fatal("recorded account does not carry the authenticated credential generation without replacement intent")
					}
					got, err := s.GetAccount(ctx, acc.ID)
					if err != nil {
						t.Fatal(err)
					}
					if !slices.Equal(got.WarpModelIDs, []string{"rotated-model"}) || !(capabilities{}).SupportsModel(got, "rotated-model") || (capabilities{}).SupportsModel(got, "model-a") {
						t.Fatal("persisted catalog was not immediately usable for exact model selection")
					}
					if flow == "discovery" {
						if !reflect.DeepEqual(got.WarpQuota, beforeCatalog.WarpQuota) {
							t.Fatal("catalog discovery changed the quota snapshot")
						}
					} else {
						q := got.WarpQuota
						if q.Source != quotaSource || !q.AttemptedAt.After(beforeCatalog.WarpQuota.AttemptedAt) || !reflect.DeepEqual(acc.WarpQuota, q) {
							t.Fatal("quota attempt was not recorded on the latest credential generation")
						}
						if disable || quotaFailure {
							if q.Status != "error" || !q.SyncedAt.Equal(beforeCatalog.WarpQuota.SyncedAt) || !reflect.DeepEqual(q.Windows, beforeCatalog.WarpQuota.Windows) {
								t.Fatal("failed quota read changed the previous measurements")
							}
						} else {
							if q.Status != "ok" || !q.SyncedAt.Equal(q.AttemptedAt) || len(q.Windows) != 2 {
								t.Fatal("successful quota read was not persisted")
							}
							workspace, personal := q.Windows[0], q.Windows[1]
							if workspace.Scope != "workspace_monthly" || workspace.Unit != "credits" || personal.Scope != "personal" || personal.Unit != "credits" || !personal.HasUnlimited || !personal.Unlimited {
								t.Fatal("quota pools were conflated or relabeled")
							}
							quotaAssertNumber(t, workspace.Limit, 100)
							quotaAssertNumber(t, workspace.Used, 25)
							quotaAssertNumber(t, workspace.Remaining, 75)
						}
					}
					want := beforeCatalog.Clone()
					want.WarpModelIDs = slices.Clone(got.WarpModelIDs)
					want.WarpModelsSyncedAt = got.WarpModelsSyncedAt
					want.WarpQuota = got.WarpQuota.Clone()
					want.UpdatedAt = got.UpdatedAt
					if !got.WarpModelsSyncedAt.After(original.WarpModelsSyncedAt) || !reflect.DeepEqual(got, want) {
						t.Fatal("catalog/quota persistence failed to advance the snapshot or changed unrelated account fields")
					}
					if disable {
						enabled, err := s.GetEnabledAccounts(ctx)
						if err != nil || len(enabled) != 0 {
							t.Fatal("catalog persistence re-enabled the operator-disabled account")
						}
					}
				})
			}
		}
	}
}

func TestCatalogObservationRejectsLaterCredentialReplacement(t *testing.T) {
	ctx := t.Context()
	s := catalogRotationStore(t)
	acc := testAccount()
	if err := s.CreateAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/graphql/v2" {
			t.Error("unexpected request")
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(rotationCatalogResponse))
	}))
	defer server.Close()
	catalog := catalogPort()
	observation, err := catalog.Observe(ctx, provider.Env{Config: testConfig(server.URL), Store: s}, acc)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateWarpCredentials(ctx, acc.ID, store.WarpCredentialPatch{ExpectedRefreshToken: acc.WarpRefreshToken, AccessToken: "synthetic-later-access", RefreshToken: "synthetic-later-refresh", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	before, err := s.GetAccount(ctx, acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !observation.Record(acc, time.Now()) {
		t.Fatal("successful observation was not recorded")
	}
	if err := catalog.Persist(ctx, s, acc); err == nil {
		t.Fatal("catalog observed under a previous credential generation was accepted")
	}
	got, err := s.GetAccount(ctx, acc.ID)
	if err != nil || !reflect.DeepEqual(got, before) {
		t.Fatal("rejected catalog persistence changed the authoritative account")
	}
}
