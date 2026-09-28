package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
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
	items, source := report.Candidates, report.Source
	if err == nil {
		t.Fatalf("discoverAccountCatalogModels() items=%+v source=%q want error", items, source)
	}
	if !isNoActiveAccounts(err) {
		t.Fatalf("error=%v want a no-active-account report", err)
	}
	if len(items) != 0 {
		t.Fatalf("items=%+v want none published", items)
	}
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
	if err := s.CreateAccount(ctx, acc); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	// Every endpoint points at a closed port, so the catalog read cannot succeed.
	dead := "http://127.0.0.1:1"
	cfg := &config.Config{
		QoderOAuthBaseURL:   dead,
		QoderOpenAPIBaseURL: dead,
		QoderInferenceURL:   dead,
	}
	report, err := discoverAccountCatalogModels(ctx, cfg, s, "Qoder", defaultModelRefreshConcurrency)
	items, source := report.Candidates, report.Source
	if err == nil {
		t.Fatalf("discoverAccountCatalogModels() items=%+v source=%q want error", items, source)
	}
	if source != "" {
		t.Fatalf("source=%q want no source for a failed read", source)
	}
	if len(items) != 0 {
		t.Fatalf("items=%+v want none published", items)
	}
	if strings.Contains(err.Error(), "builtin") {
		t.Fatalf("error=%v must not describe a built-in catalog", err)
	}

	models, listErr := s.ListModels(ctx)
	if listErr != nil {
		t.Fatalf("ListModels() error = %v", listErr)
	}
	for _, model := range models {
		if strings.EqualFold(strings.TrimSpace(model.Channel), "qoder") {
			t.Fatalf("a compiled-in catalog was published: %+v", model)
		}
	}

	// The credential is still good, so the account is untouched and a later
	// refresh can record the catalog once the read works.
	stored, getErr := s.GetAccount(ctx, acc.ID)
	if getErr != nil {
		t.Fatalf("GetAccount() error = %v", getErr)
	}
	if len(stored.QoderModelIDs) != 0 {
		t.Fatalf("account snapshot = %v, want it left empty", stored.QoderModelIDs)
	}
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
		if auth := r.Header.Get("Authorization"); !strings.HasPrefix(auth, "Bearer COSY.") {
			t.Errorf("catalog request Authorization = %q, want a COSY bearer", auth)
		}
		for _, header := range []string{"Cosy-Key", "Cosy-MachineId", "Cosy-Date"} {
			if r.Header.Get(header) == "" {
				t.Errorf("catalog request is missing %s", header)
			}
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
	if err := s.CreateAccount(ctx, acc); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	report, err := discoverAccountCatalogModels(ctx, &config.Config{QoderInferenceURL: stub.URL}, s, "Qoder", defaultModelRefreshConcurrency)
	items, source := report.Candidates, report.Source
	if err != nil {
		t.Fatalf("discoverAccountCatalogModels() error = %v", err)
	}
	if source != "qoder_upstream_models" {
		t.Fatalf("source=%q want qoder_upstream_models", source)
	}
	if len(items) != 2 {
		t.Fatalf("items=%+v want the two observed rows", items)
	}
	for _, item := range items {
		if !item.Verified {
			t.Fatalf("item %+v is not marked verified", item)
		}
		if item.ID != strings.ToLower(item.ID) {
			t.Fatalf("item %+v is not lowercased", item)
		}
	}

	// The snapshot must carry the wire fields routing rebuilds the request from.
	stored, getErr := s.GetAccount(ctx, acc.ID)
	if getErr != nil {
		t.Fatalf("GetAccount() error = %v", getErr)
	}
	if len(stored.QoderModelIDs) == 0 {
		t.Fatal("the observed catalog was not recorded on the account")
	}
	if !strings.Contains(strings.Join(stored.QoderModelIDs, ""), "max_input_tokens") {
		t.Fatalf("snapshot lost the routing fields: %v", stored.QoderModelIDs)
	}
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
	if err := s.CreateAccount(ctx, acc); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	report, err := discoverAccountCatalogModels(ctx, &config.Config{QoderInferenceURL: stub.URL}, s, "Qoder", defaultModelRefreshConcurrency)
	items, source := report.Candidates, report.Source
	if err == nil {
		t.Fatalf("discoverAccountCatalogModels() items=%+v source=%q want error", items, source)
	}
	if !strings.Contains(err.Error(), "status=403") {
		t.Fatalf("error=%v does not carry the upstream cause", err)
	}
	if len(items) != 0 {
		t.Fatalf("items=%+v want none published", items)
	}
	models, listErr := s.ListModels(ctx)
	if listErr != nil {
		t.Fatalf("ListModels() error = %v", listErr)
	}
	for _, model := range models {
		if strings.EqualFold(strings.TrimSpace(model.Channel), "qoder") {
			t.Fatalf("a failed read published a model: %+v", model)
		}
	}
}

// TestNormalizeAdminModelChannel_AcceptsQoder proves the admin refresh endpoint
// recognises the channel name.
func TestNormalizeAdminModelChannel_AcceptsQoder(t *testing.T) {
	if got := normalizeAdminModelChannel("qoder"); got != "Qoder" {
		t.Fatalf("normalizeAdminModelChannel(qoder) = %q, want Qoder", got)
	}
	if got := normalizeAdminModelChannel("QODER"); got != "Qoder" {
		t.Fatalf("normalizeAdminModelChannel(QODER) = %q, want Qoder", got)
	}
}

// TestShouldDeleteMissingModelsOnRefresh_OnlyPrunesUpstreamCatalogs proves
// pruning follows an observed catalog, and never a locally produced list.
func TestShouldDeleteMissingModelsOnRefresh_OnlyPrunesUpstreamCatalogs(t *testing.T) {
	if !shouldDeleteMissingModelsOnRefresh("qoder", "qoder_upstream_models") {
		t.Fatal("an observed Qoder catalog must prune rows it no longer advertises")
	}
	for _, source := range []string{"qoder_builtin_catalog", "cline_cached_models", "test", ""} {
		if shouldDeleteMissingModelsOnRefresh("qoder", source) {
			t.Fatalf("source %q pruned the catalog", source)
		}
	}
}
