package modelrefresh

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func qoderTestAccount(machineID string) *store.Account {
	return &store.Account{
		AccountType:       "qoder",
		Name:              "qoder-refresh",
		QoderAccessToken:  "access-1",
		QoderRefreshToken: "refresh-1",
		QoderExpiresAt:    time.Now().Add(24 * time.Hour),
		QoderMachineID:    machineID,
		QoderUserID:       "uid-qoder",
		QoderRuntimeInfo:  "runtime-info",
		QoderRuntimeKey:   "runtime-key",
		Enabled:           true,
		Weight:            1,
	}
}

// TestDiscoverQoderModelsRequiresAnActiveAccount proves the channel reports the
// missing pool instead of publishing anything.
//
// The channel used to answer with a compiled-in catalog here. It no longer has
// one: with no active account nothing is observed, so nothing is published.
func TestDiscoverQoderModelsRequiresAnActiveAccount(t *testing.T) {
	s, cleanup := setupModelRefreshStore(t)
	defer cleanup()

	report, err := discoverAccountCatalogModels(context.Background(), &config.Config{}, s, "Qoder", defaultModelRefreshConcurrency)
	items, _ := report.Candidates, report.Source
	testutil.Error(t, err, "discoverAccountCatalogModels() items=%+v source=%q want error")
	testutil.True(t, isNoActiveAccounts(err), "error=%v want a no-active-account report")
	testutil.Equal(t, len(items), 0)
}

// TestDiscoverQoderModelsWithoutAnUpstreamCatalogPublishesNothing proves the
// built-in list is gone: a credential that cannot read the model list leaves the
// channel empty instead of installing a compiled-in catalog.
func TestDiscoverQoderModelsWithoutAnUpstreamCatalogPublishesNothing(t *testing.T) {
	s, cleanup := setupModelRefreshStore(t)
	defer cleanup()
	ctx := context.Background()
	clearModelsForChannel(t, ctx, s, "Qoder")

	acc := qoderTestAccount("11111111-2222-4333-8444-555555555555")
	testutil.NoError(t, s.CreateAccount(ctx, acc), "CreateAccount() error = %v")

	// Every endpoint points at a closed port, so the catalog read cannot succeed.
	dead := "http://127.0.0.1:1"
	cfg := &config.Config{
		QoderOAuthBaseURL:   dead,
		QoderOpenAPIBaseURL: dead,
		QoderInferenceURL:   dead,
	}
	report, err := discoverAccountCatalogModels(ctx, cfg, s, "Qoder", defaultModelRefreshConcurrency)
	items, source := report.Candidates, report.Source
	testutil.Error(t, err, "discoverAccountCatalogModels() items=%+v source=%q want error")
	testutil.Equal(t, source, "")
	testutil.Equal(t, len(items), 0)
	testutil.MustNotContain(t, err.Error(), "builtin")

	models, listErr := s.ListModels(ctx)
	testutil.NoError(t, listErr, "ListModels() error = %v")
	for _, model := range models {
		testutil.Falsef(t, strings.EqualFold(strings.TrimSpace(model.Channel), "qoder"), "a compiled-in catalog was published: %+v", model)
	}

	// The credential is still good, so the account is untouched and a later
	// refresh can record the catalog once the read works.
	stored, getErr := s.GetAccount(ctx, acc.ID)
	testutil.NoError(t, getErr, "GetAccount() error = %v")
	testutil.Equal(t, len(stored.QoderModelIDs), 0)
}

// qoderCatalogStub answers the signed catalog route the way the gateway does,
// so the test exercises the real client, its COSY signature and the real parser
// rather than an injected shortcut.
func qoderCatalogStub(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/algo/api/v2/model/list" {
			http.NotFound(w, r)
			return
		}
		// The catalog read must carry the derived auth chain; without it the
		// gateway refuses and the refresh would report the wrong cause.
		auth := r.Header.Get("Authorization")
		testutil.CheckFalsef(t, !strings.HasPrefix(auth, "Bearer COSY."), "catalog request Authorization = %q, want a COSY bearer", auth)
		for _, header := range []string{"Cosy-Key", "Cosy-MachineId", "Cosy-Date"} {
			testutil.CheckNotEqual(t, r.Header.Get(header), "")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

// TestDiscoverQoderModelsPublishesTheObservedCatalog proves a successful read is
// what fills model management, and that the account snapshot records the same
// rows so routing resolves against the published catalog.
func TestDiscoverQoderModelsPublishesTheObservedCatalog(t *testing.T) {
	s, cleanup := setupModelRefreshStore(t)
	defer cleanup()
	ctx := context.Background()
	clearModelsForChannel(t, ctx, s, "Qoder")

	stub := qoderCatalogStub(t, http.StatusOK, `{"chat":[
		{"key":"qmodel_38max","display_name":"Qwen3.8-Max","format":"openai","source":"system","enable":true,"max_input_tokens":1000000},
		{"key":"dmodel","display_name":"DeepSeek-V4-Pro","format":"openai","source":"system","enable":true,"is_reasoning":true,"max_input_tokens":1000000}
	]}`)
	defer stub.Close()

	acc := qoderTestAccount("11111111-2222-4333-8444-555555555555")
	testutil.NoError(t, s.CreateAccount(ctx, acc), "CreateAccount() error = %v")

	report, err := discoverAccountCatalogModels(ctx, &config.Config{QoderInferenceURL: stub.URL}, s, "Qoder", defaultModelRefreshConcurrency)
	items, source := report.Candidates, report.Source
	testutil.NoError(t, err, "discoverAccountCatalogModels() error = %v")
	testutil.Equal(t, source, "qoder_upstream_models")
	testutil.Equal(t, len(items), 2)
	for _, item := range items {
		testutil.Falsef(t, !item.Verified, "item %+v is not marked verified", item)
		testutil.Equal(t, item.ID, strings.ToLower(item.ID))
	}

	// The snapshot must carry the wire fields routing rebuilds the request from.
	stored, getErr := s.GetAccount(ctx, acc.ID)
	testutil.NoError(t, getErr, "GetAccount() error = %v")
	testutil.NotEqual(t, len(stored.QoderModelIDs), 0)
	testutil.MustContain(t, strings.Join(stored.QoderModelIDs, ""), "max_input_tokens")
}

// TestDiscoverQoderModelsReportsTheReadFailure proves a failed read is reported
// with its cause, so an operator can tell a missing route from a refused
// credential, and that nothing is published either way.
func TestDiscoverQoderModelsReportsTheReadFailure(t *testing.T) {
	s, cleanup := setupModelRefreshStore(t)
	defer cleanup()
	ctx := context.Background()
	clearModelsForChannel(t, ctx, s, "Qoder")

	stub := qoderCatalogStub(t, http.StatusForbidden, `{"code":101,"message":"signature invalid"}`)
	defer stub.Close()

	acc := qoderTestAccount("11111111-2222-4333-8444-555555555555")
	testutil.NoError(t, s.CreateAccount(ctx, acc), "CreateAccount() error = %v")

	report, err := discoverAccountCatalogModels(ctx, &config.Config{QoderInferenceURL: stub.URL}, s, "Qoder", defaultModelRefreshConcurrency)
	items, _ := report.Candidates, report.Source
	testutil.Error(t, err, "discoverAccountCatalogModels() items=%+v source=%q want error")
	testutil.MustContain(t, err.Error(), "status=403")
	testutil.Equal(t, len(items), 0)
	models, listErr := s.ListModels(ctx)
	testutil.NoError(t, listErr, "ListModels() error = %v")
	for _, model := range models {
		testutil.Falsef(t, strings.EqualFold(strings.TrimSpace(model.Channel), "qoder"), "a failed read published a model: %+v", model)
	}
}

// TestNormalizeAdminModelChannel_AcceptsQoder proves the admin refresh endpoint
// recognises the channel name.
func TestNormalizeAdminModelChannel_AcceptsQoder(t *testing.T) {
	testutil.Equal(t, normalizeAdminModelChannel("qoder"), "Qoder")
	testutil.Equal(t, normalizeAdminModelChannel("QODER"), "Qoder")
}

// TestShouldDeleteMissingModelsOnRefresh_OnlyPrunesUpstreamCatalogs proves
// pruning follows an observed catalog, and never a locally produced list.
func TestShouldDeleteMissingModelsOnRefresh_OnlyPrunesUpstreamCatalogs(t *testing.T) {
	testutil.False(t, !shouldDeleteMissingModelsOnRefresh("qoder_upstream_models"), "an observed Qoder catalog must prune rows it no longer advertises")
	for _, source := range []string{"qoder_builtin_catalog", "cline_cached_models", "test", ""} {
		testutil.Falsef(t, shouldDeleteMissingModelsOnRefresh(source), "source %q pruned the catalog", source)
	}
}
