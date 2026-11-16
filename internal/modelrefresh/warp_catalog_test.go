package modelrefresh

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

const warpRefreshCatalogResponse = `{"data":{"user":{"__typename":"UserOutput","user":{"workspaces":[{"featureModelChoice":{
	"agentMode":{"defaultId":"auto","choices":[
		{"id":"model-b","disableReason":null,"contextWindow":{"default":200000,"max":300000,"min":1000,"isConfigurable":true}},
		{"id":"auto","disableReason":null},
		{"id":"model-disabled","disableReason":"not available"},
		{"id":"model-a","disableReason":"","contextWindow":{"default":0,"max":128000,"min":1000,"isConfigurable":true}},
		{"id":"model-b","disableReason":null,"contextWindow":{"default":200000,"max":300000,"min":1000,"isConfigurable":true}}
	]},
	"planning":{"defaultId":"auto","choices":[{"id":"model-b","contextWindow":{"default":200000,"max":300000}}]},
	"coding":{"choices":[{"id":"auto","disableReason":null}]},
	"cliAgent":{"defaultId":"cli-agent","choices":[{"id":"cli-agent","disableReason":null},{"id":"model-a","contextWindow":{"default":0,"max":128000}}]},
	"computerUseAgent":{"defaultId":"computer-use-agent","choices":[{"id":"computer-use-agent","disableReason":null}]}
}}]}}}}`

func TestSyncModelsForChannelConcurrent_WarpPublishesObservedCatalog(t *testing.T) {
	s, cleanup := setupModelRefreshStore(t)
	defer cleanup()
	ctx := t.Context()
	clearModelsForChannel(t, ctx, s, "Warp")

	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/graphql/v2" || r.URL.Query().Get("op") != "GetFeatureModelChoices" {
			t.Errorf("unexpected catalog request: %s %s", r.Method, r.URL)
			http.NotFound(w, r)
			return
		}
		var request struct {
			OperationName string `json:"operationName"`
			Query         string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.OperationName != "GetFeatureModelChoices" || !strings.Contains(request.Query, "featureModelChoice") {
			t.Error("catalog request did not query featureModelChoice")
			http.Error(w, "invalid catalog query", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(warpRefreshCatalogResponse))
	}))
	defer server.Close()

	observedBefore := time.Now().Add(-time.Hour).UTC()
	acc := warpCatalogRefreshAccount(observedBefore, "cached-only-model")
	testutil.NoError(t, s.CreateAccount(ctx, acc))
	result, err := syncModelsForChannelConcurrent(ctx, warpCatalogRefreshConfig(server.URL), s, "warp", 2)
	testutil.NoError(t, err)
	testutil.Equal(t, reads.Load(), int32(1))
	testutil.Equal(t, result.Channel, "Warp")
	testutil.Equal(t, result.Source, "warp_feature_model_choice")
	testutil.True(t, isUpstreamCatalogSource(result.Source), "observed Warp catalog source must be allowed to publish")
	testutil.False(t, isUpstreamCatalogSource(result.Source+"_cached"), "cached source must not pass the exact source whitelist")
	testutil.Equal(t, result.Outcome, "success")
	testutil.Equal(t, result.AccountsTotal, 1)
	testutil.Equal(t, result.AccountsSuccess, 1)
	testutil.Equal(t, result.AccountsFailed, 0)
	testutil.Equal(t, result.Discovered, 2)
	testutil.Equal(t, result.Verified, 2)
	testutil.Equal(t, result.Added, 2)

	rows, err := s.ListModels(ctx)
	testutil.NoError(t, err)
	testutil.Equal(t, len(rows), 2)
	for _, id := range []string{"model-a", "model-b"} {
		row, err := s.GetModelByChannelAndModelID(ctx, "Warp", id)
		testutil.NoError(t, err)
		testutil.Equal(t, row.Name, id)
		testutil.Equal(t, row.UpstreamModel, id)
		testutil.Equal(t, row.Provider, "warp")
		testutil.Equal(t, row.Origin, "discovery")
		testutil.Equal(t, row.Status, store.ModelStatusAvailable)
		testutil.True(t, row.Verified, "published row must be an upstream observation")
		testutil.True(t, slices.Equal(row.Capabilities, []string{"chat", "text"}), "Warp catalog must shape chat/text route metadata")
	}
	stored, err := s.GetAccount(ctx, acc.ID)
	testutil.NoError(t, err)
	testutil.True(t, slices.Equal(stored.WarpModelIDs, []string{"model-a", "model-b"}), "snapshot must contain only observed enabled model IDs, not auto, disabled, duplicate or cached choices")
	testutil.True(t, stored.WarpModelsSyncedAt.After(observedBefore), "successful catalog must advance the persisted snapshot timestamp")
	testutil.True(t, reflect.DeepEqual(stored.WarpModelWindows, map[string]int{"model-a": 128000, "model-b": 200000}),
		"per-plane context windows must be persisted with the catalog row and never defaulted")
}

func TestSyncModelsForChannelConcurrent_WarpFailureDoesNotPublishCachedCatalog(t *testing.T) {
	for _, response := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "upstream failure", status: http.StatusBadGateway, body: `{"error":"unavailable"}`},
		{name: "GraphQL error", status: http.StatusOK, body: `{"errors":[{"message":"unavailable"}]}`},
		{name: "no enabled choices", status: http.StatusOK, body: `{"data":{"user":{"__typename":"UserOutput","user":{"workspaces":[{"featureModelChoice":{"agentMode":{"choices":[{"id":"auto"},{"id":"disabled-model","disableReason":"not available"}]}}}]}}}}`},
	} {
		t.Run(response.name, func(t *testing.T) {
			s, cleanup := setupModelRefreshStore(t)
			defer cleanup()
			ctx := t.Context()
			clearModelsForChannel(t, ctx, s, "Warp")
			var reads atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reads.Add(1)
				if r.URL.Path != "/graphql/v2" || r.URL.Query().Get("op") != "GetFeatureModelChoices" {
					t.Errorf("unexpected upstream request: %s %s", r.Method, r.URL)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(response.status)
				_, _ = w.Write([]byte(response.body))
			}))
			defer server.Close()

			// A fresh, valid cache still cannot stand in for a failed upstream read.
			observedBefore := time.Now().Add(-time.Minute).UTC()
			acc := warpCatalogRefreshAccount(observedBefore, "cached-only-model")
			testutil.NoError(t, s.CreateAccount(ctx, acc))
			cfg := warpCatalogRefreshConfig(server.URL)
			report, err := discoverAccountCatalogModels(ctx, cfg, s, "Warp", 1)
			testutil.Error(t, err)
			testutil.Equal(t, len(report.Candidates), 0)
			result, err := syncModelsForChannelConcurrent(ctx, cfg, s, "Warp", 1)
			testutil.Error(t, err)
			testutil.True(t, result == nil, "failed refresh must not return a publication result")
			testutil.Equal(t, reads.Load(), int32(2))
			rows, err := s.ListModels(ctx)
			testutil.NoError(t, err)
			testutil.Equal(t, len(rows), 0)
			stored, err := s.GetAccount(ctx, acc.ID)
			testutil.NoError(t, err)
			testutil.True(t, slices.Equal(stored.WarpModelIDs, acc.WarpModelIDs), "failure must preserve account cache without publishing it")
			testutil.True(t, stored.WarpModelsSyncedAt.Equal(observedBefore), "failure must not make the cached observation look fresh")
		})
	}
}

func TestSyncModelsForChannelConcurrent_WarpPartialFailureExcludesCachedAccount(t *testing.T) {
	s, cleanup := setupModelRefreshStore(t)
	defer cleanup()
	ctx := t.Context()
	clearModelsForChannel(t, ctx, s, "Warp")
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		if r.URL.Path != "/graphql/v2" || r.URL.Query().Get("op") != "GetFeatureModelChoices" {
			t.Errorf("unexpected upstream request: %s %s", r.Method, r.URL)
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") == "Bearer synthetic-failed-access" {
			http.Error(w, "temporary catalog failure", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(warpRefreshCatalogResponse))
	}))
	defer server.Close()

	observedBefore := time.Now().Add(-time.Minute).UTC()
	active := warpCatalogRefreshAccount(observedBefore, "old-active-model")
	failed := warpCatalogRefreshAccount(observedBefore, "cached-failed-account-model")
	failed.Name = "failed Warp catalog"
	failed.WarpAccessToken = "synthetic-failed-access"
	failed.WarpRefreshToken = "synthetic-failed-refresh"
	for _, acc := range []*store.Account{active, failed} {
		testutil.NoError(t, s.CreateAccount(ctx, acc))
	}

	result, err := syncModelsForChannelConcurrent(ctx, warpCatalogRefreshConfig(server.URL), s, "Warp", 2)
	testutil.NoError(t, err)
	testutil.Equal(t, reads.Load(), int32(2))
	testutil.Equal(t, result.Source, "warp_feature_model_choice")
	testutil.Equal(t, result.Outcome, "partial")
	testutil.True(t, result.Partial, "one failed account must make the round partial")
	testutil.Equal(t, result.AccountsTotal, 2)
	testutil.Equal(t, result.AccountsSuccess, 1)
	testutil.Equal(t, result.AccountsFailed, 1)
	testutil.Equal(t, result.Discovered, 2)
	testutil.Equal(t, result.Added, 2)
	rows, err := s.ListModels(ctx)
	testutil.NoError(t, err)
	testutil.Equal(t, len(rows), 2)
	for _, row := range rows {
		testutil.True(t, row.ModelID == "model-a" || row.ModelID == "model-b", "partial refresh must publish only live observations, not failed account cache")
	}
	stored, err := s.GetAccount(ctx, failed.ID)
	testutil.NoError(t, err)
	testutil.True(t, slices.Equal(stored.WarpModelIDs, failed.WarpModelIDs), "failed account cache should remain stored, not published")
	testutil.True(t, stored.WarpModelsSyncedAt.Equal(observedBefore), "failed account observation timestamp must not advance")
}

func warpCatalogRefreshAccount(observedAt time.Time, cachedID string) *store.Account {
	return &store.Account{
		AccountType: "warp", Name: "Warp catalog", Enabled: true, Weight: 1,
		WarpAccessToken: "synthetic-access", WarpRefreshToken: "synthetic-refresh",
		WarpExpiresAt: time.Now().Add(time.Hour), WarpModelIDs: []string{cachedID}, WarpModelsSyncedAt: observedAt,
	}
}

func warpCatalogRefreshConfig(baseURL string) *config.Config {
	// Keep every possible Warp endpoint local, even if the tested flow regresses.
	return &config.Config{WarpBaseURL: baseURL, WarpFirebaseBaseURL: baseURL, WarpTokenBaseURL: baseURL}
}
