package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/goccy/go-json"

	"orchids-api/internal/config"
	"orchids-api/internal/modelcatalog"
	"orchids-api/internal/store"
)

func TestMakeModelRefreshHandler_UsesBodyChannel(t *testing.T) {
	prev := runModelRefresh
	defer func() { runModelRefresh = prev }()

	runModelRefresh = func(ctx context.Context, cfg *config.Config, s *store.Store, channel string, concurrency int) (*modelRefreshResult, error) {
		return &modelRefreshResult{Channel: channel, Source: "stub", Concurrency: concurrency, Discovered: 3, Verified: 2}, nil
	}

	handler := makeCoordinatedModelRefreshHandler(func() *config.Config { return &config.Config{} }, nil, newModelRefreshCoordinator())
	req := httptest.NewRequest(http.MethodPost, "/api/models/refresh?channel=cline&concurrency=99", strings.NewReader(`{"channel":"workbuddy","concurrency":8}`))
	rec := httptest.NewRecorder()

	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 body=%s", rec.Code, rec.Body.String())
	}

	var resp modelRefreshResult
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Channel != "workbuddy" {
		t.Fatalf("channel=%q want %q", resp.Channel, "workbuddy")
	}
	if resp.Verified != 2 {
		t.Fatalf("verified=%d want 2", resp.Verified)
	}
	if resp.Concurrency != 8 {
		t.Fatalf("concurrency=%d want 8", resp.Concurrency)
	}
}

func TestModelRefreshCoordinatorRejectsDuplicateChannel(t *testing.T) {
	prev := runModelRefresh
	defer func() { runModelRefresh = prev }()
	started := make(chan struct{})
	release := make(chan struct{})
	runModelRefresh = func(ctx context.Context, cfg *config.Config, s *store.Store, channel string, concurrency int) (*modelRefreshResult, error) {
		close(started)
		<-release
		return &modelRefreshResult{Channel: channel}, nil
	}
	coordinator := newModelRefreshCoordinator()
	handler := makeCoordinatedModelRefreshHandler(func() *config.Config { return &config.Config{} }, nil, coordinator)
	firstDone := make(chan struct{})
	go func() {
		handler(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/models/refresh?channel=workbuddy", nil))
		close(firstDone)
	}()
	<-started
	second := httptest.NewRecorder()
	handler(second, httptest.NewRequest(http.MethodPost, "/api/models/refresh?channel=workbuddy", nil))
	if second.Code != http.StatusConflict {
		t.Fatalf("duplicate status=%d want 409", second.Code)
	}
	close(release)
	<-firstDone
}

func TestRunIndexedModelRefreshWorkersVisitsEachIndexOnce(t *testing.T) {
	const total = 37
	counts := make([]int, total)
	var mu sync.Mutex

	runIndexedModelRefreshWorkers(total, 5, func(index int) {
		mu.Lock()
		counts[index]++
		mu.Unlock()
	})

	for index, count := range counts {
		if count != 1 {
			t.Fatalf("index %d visited %d times, want once", index, count)
		}
	}
}

func TestRunIndexedModelRefreshWorkersHandlesEmptyWork(t *testing.T) {
	called := false
	runIndexedModelRefreshWorkers(0, 4, func(int) { called = true })
	runIndexedModelRefreshWorkers(3, 4, nil)
	if called {
		t.Fatal("worker called for empty input")
	}
}

func TestNormalizeModelRefreshConcurrency(t *testing.T) {
	tests := []struct {
		name string
		in   int
		want int
	}{
		{name: "default on zero", in: 0, want: defaultModelRefreshConcurrency},
		{name: "default on negative", in: -2, want: defaultModelRefreshConcurrency},
		{name: "keeps valid", in: 8, want: 8},
		{name: "clamps max", in: 99, want: maxModelRefreshConcurrency},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeModelRefreshConcurrency(tt.in); got != tt.want {
				t.Fatalf("normalizeModelRefreshConcurrency(%d)=%d want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseModelRefreshConcurrency(t *testing.T) {
	tests := []struct {
		raw  string
		want int
		ok   bool
	}{
		{raw: "", want: 0, ok: false},
		{raw: "2", want: 2, ok: true},
		{raw: "99", want: maxModelRefreshConcurrency, ok: true},
		{raw: "bad", want: defaultModelRefreshConcurrency, ok: true},
	}
	for _, tt := range tests {
		got, ok := parseModelRefreshConcurrency(tt.raw)
		if got != tt.want || ok != tt.ok {
			t.Fatalf("parseModelRefreshConcurrency(%q)=(%d,%v) want (%d,%v)", tt.raw, got, ok, tt.want, tt.ok)
		}
	}
}

func TestSyncModelsForChannelConcurrent_WorkBuddyRequiresAccountDiscovery(t *testing.T) {
	s, cleanup := setupModelRefreshStore(t)
	defer cleanup()

	ctx := context.Background()
	clearModelsForChannel(t, ctx, s, "WorkBuddy")

	result, err := syncModelsForChannelConcurrent(ctx, &config.Config{}, s, "WorkBuddy", 8)
	if err == nil {
		t.Fatalf("syncModelsForChannelConcurrent() result=%+v want error", result)
	}
	// Without an active account nothing is read and nothing is published: the
	// refresh reports the missing account rather than a cached catalog.
	if !isNoActiveAccounts(err) {
		t.Fatalf("error=%v want a no-active-account report", err)
	}
	models, listErr := s.ListModels(ctx)
	if listErr != nil {
		t.Fatalf("ListModels() error = %v", listErr)
	}
	if len(models) != 0 {
		t.Fatalf("models=%+v want none published", models)
	}
}

// TestRefreshDoesNotRepublishStoredCatalogOnFailure proves the stored rows are
// last known state, not a fallback: when the upstream read yields nothing, the
// refresh reports that and leaves the rows exactly as they were.
func TestRefreshDoesNotRepublishStoredCatalogOnFailure(t *testing.T) {
	s, cleanup := setupModelRefreshStore(t)
	defer cleanup()
	ctx := context.Background()
	clearModelsForChannel(t, ctx, s, "Cline")
	if err := s.CreateModel(ctx, &store.Model{
		Channel: "Cline", ModelID: "cline-a", Name: "Cline A",
		Status: store.ModelStatusAvailable, Verified: true, Origin: "discovery",
	}); err != nil {
		t.Fatalf("CreateModel() error = %v", err)
	}

	result, err := syncModelsForChannelConcurrent(ctx, &config.Config{}, s, "Cline", 4)
	if err == nil {
		t.Fatalf("syncModelsForChannelConcurrent() result=%+v want error", result)
	}
	if !isNoActiveAccounts(err) {
		t.Fatalf("error=%v want a no-active-account report", err)
	}

	stored, getErr := s.GetModelByChannelAndModelID(ctx, "Cline", "cline-a")
	if getErr != nil || stored == nil {
		t.Fatalf("stored row was destroyed by a failed refresh: %v", getErr)
	}
	if stored.Status != store.ModelStatusAvailable || !stored.Verified {
		t.Fatalf("stored row was modified by a failed refresh: %+v", stored)
	}
}

func TestChooseRefreshedDefaultModel_PrefersExistingDefault(t *testing.T) {
	existing := map[string]*store.Model{
		"a": {ModelID: "a", IsDefault: true},
		"b": {ModelID: "b", IsDefault: false},
	}
	ordered := []discoveredModel{{ID: "b"}, {ID: "a"}}

	got := chooseRefreshedDefaultModel("WorkBuddy", existing, ordered)
	if got != "a" {
		t.Fatalf("default=%q want %q", got, "a")
	}
}

// TestDiscoverGrokModelsWithoutActiveAccountReportsNoAccount proves the channel
// no longer has a historical-catalog fallback.
func TestDiscoverGrokModelsWithoutActiveAccountReportsNoAccount(t *testing.T) {
	s, cleanup := setupModelRefreshStore(t)
	defer cleanup()

	report, err := discoverGrokModelsReport(context.Background(), &config.Config{}, s, 4)
	items, source := report.Candidates, report.Source
	if err == nil {
		t.Fatalf("discoverGrokModelsReport() items=%+v source=%q want error", items, source)
	}
	if !isNoActiveAccounts(err) {
		t.Fatalf("error=%v want a no-active-account report", err)
	}
	if len(items) != 0 {
		t.Fatalf("items=%+v want none published", items)
	}
}

func TestDiscoverGrokModelsUsesOfficialBuildCatalogAndPersistsPerAccountSnapshot(t *testing.T) {
	s, cleanup := setupModelRefreshStore(t)
	defer cleanup()
	ctx := context.Background()
	acc := &store.Account{
		Name:              "build",
		AccountType:       "grok",
		CredentialType:    "oauth",
		OAuthAccessToken:  "access",
		OAuthRefreshToken: "refresh",
		Enabled:           true,
	}
	if err := s.CreateAccount(ctx, acc); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	prevFetch := fetchGrokBuildModelsForRefresh
	t.Cleanup(func() { fetchGrokBuildModelsForRefresh = prevFetch })
	var calls int
	fetchGrokBuildModelsForRefresh = func(ctx context.Context, cfg *config.Config, store *store.Store, got *store.Account) ([]modelcatalog.Profile, error) {
		calls++
		if got.ID != acc.ID {
			t.Fatalf("account id=%d want %d", got.ID, acc.ID)
		}
		return []modelcatalog.Profile{{ModelID: "grok-4.6"}, {ModelID: "grok-4.6"}, {ModelID: "future-private-model"}, {ModelID: "grok-4.5"}}, nil
	}

	report, err := discoverGrokModelsReport(ctx, &config.Config{}, s, 4)
	items, source := report.Candidates, report.Source
	if err != nil {
		t.Fatalf("discoverGrokModelsReport() error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("official catalog calls=%d want 1", calls)
	}
	if source != "grok_build_models" {
		t.Fatalf("source=%q want grok_build_models", source)
	}
	gotIDs := make([]string, 0, len(items))
	for _, item := range items {
		gotIDs = append(gotIDs, item.ID)
	}
	// The upstream catalog plus the entries grok2api derives from the account:
	// 4.6 implies 4.5, and an OAuth Build account can serve Composer. The row for
	// the catalog model is published under its bare public name.
	wantIDs := "grok-4.6,future-private-model,grok-4.5,grok-composer-2.5-fast"
	if strings.Join(gotIDs, ",") != wantIDs {
		t.Fatalf("public IDs=%v want %s", gotIDs, wantIDs)
	}

	persisted, err := s.GetAccount(ctx, acc.ID)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if persisted.GrokProvider != "build" || persisted.GrokModelsSyncedAt.IsZero() {
		t.Fatalf("provider/catalog not persisted: %+v", persisted)
	}
	if strings.Join(persisted.GrokModels, ",") != "grok-4.6,future-private-model,grok-4.5,grok-composer-2.5-fast" {
		t.Fatalf("account capability snapshot=%v", persisted.GrokModels)
	}
}

func TestDiscoverGrokModelsWithoutUpstreamCatalogPublishesNothing(t *testing.T) {
	s, cleanup := setupModelRefreshStore(t)
	defer cleanup()
	ctx := context.Background()
	acc := &store.Account{AccountType: "grok", CredentialType: "oauth", OAuthRefreshToken: "refresh", Enabled: true}
	if err := s.CreateAccount(ctx, acc); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}
	prevFetch := fetchGrokBuildModelsForRefresh
	t.Cleanup(func() { fetchGrokBuildModelsForRefresh = prevFetch })
	fetchGrokBuildModelsForRefresh = func(context.Context, *config.Config, *store.Store, *store.Account) ([]modelcatalog.Profile, error) {
		return nil, errors.New("control plane unavailable")
	}

	report, err := discoverGrokModelsReport(ctx, &config.Config{}, s, 1)
	items, source := report.Candidates, report.Source
	if err == nil {
		t.Fatalf("discoverGrokModelsReport() items=%+v source=%q want error", items, source)
	}
	if source != "" {
		t.Fatalf("source=%q want no source for a failed read", source)
	}
	if len(items) != 0 {
		t.Fatalf("items=%+v want none published on a failed read", items)
	}
	// The failure must not be reported as a cached observation.
	if strings.Contains(err.Error(), "cached") {
		t.Fatalf("error=%v must not describe a cached catalog", err)
	}
}

// TestApplyModelRefresh_RefusesNonUpstreamSources is the gate that keeps a
// cached or compiled-in catalog out of model management: only a source that
// names an upstream catalog read may write.
func TestApplyModelRefresh_RefusesNonUpstreamSources(t *testing.T) {
	for _, source := range []string{
		"test",
		"qoder_builtin_catalog",
		"cline_cached_models",
		"grok_build_models_unavailable_cached",
		"",
	} {
		t.Run(source, func(t *testing.T) {
			s, cleanup := setupModelRefreshStore(t)
			defer cleanup()

			ctx := context.Background()
			clearModelsForChannel(t, ctx, s, "WorkBuddy")
			if err := s.CreateModel(ctx, &store.Model{
				Channel: "WorkBuddy", ModelID: "existing", Name: "existing",
				Status: store.ModelStatusAvailable, Verified: true, IsDefault: true,
			}); err != nil {
				t.Fatalf("CreateModel() error = %v", err)
			}

			result, err := applyModelRefreshWithPrune(ctx, s, "WorkBuddy", source, []discoveredModel{{ID: "injected", Name: "injected", Verified: true}}, true)
			if err == nil {
				t.Fatalf("applyModelRefreshWithPrune() result=%+v want a refusal for source %q", result, source)
			}
			if _, getErr := s.GetModelByChannelAndModelID(ctx, "WorkBuddy", "injected"); getErr == nil {
				t.Fatal("a non-upstream source published a model")
			}
			if _, getErr := s.GetModelByChannelAndModelID(ctx, "WorkBuddy", "existing"); getErr != nil {
				t.Fatalf("a refused refresh mutated the stored catalog: %v", getErr)
			}
		})
	}
}

// TestApplyModelRefresh_IsUpstreamCatalogSource pins the allowlist itself.
func TestApplyModelRefresh_IsUpstreamCatalogSource(t *testing.T) {
	allowed := []string{
		"grok_build_models",
		"workbuddy_cli_models",
		"qoder_upstream_models",
		"cline_recommended_models",
	}
	for _, source := range allowed {
		if !isUpstreamCatalogSource(source) {
			t.Fatalf("isUpstreamCatalogSource(%q) = false, want true", source)
		}
	}
	refused := []string{
		"",
		"test",
		"qoder_builtin_catalog",
		"grok_cached_models",
		"cline_cached_models",
		"grok_build_models_unavailable_cached",
	}
	for _, source := range refused {
		if isUpstreamCatalogSource(source) {
			t.Fatalf("isUpstreamCatalogSource(%q) = true, want false", source)
		}
	}
}

// TestApplyModelRefresh_CountsVerifiedSeparately proves the report distinguishes
// "advertised by the catalog" from "observed as usable".
func TestApplyModelRefresh_CountsVerifiedSeparately(t *testing.T) {
	s, cleanup := setupModelRefreshStore(t)
	defer cleanup()

	ctx := context.Background()
	clearModelsForChannel(t, ctx, s, "WorkBuddy")

	result, err := applyModelRefreshWithPrune(ctx, s, "WorkBuddy", "workbuddy_cli_models", []discoveredModel{
		{ID: "probed", Name: "probed", Verified: true},
		{ID: "listed-only", Name: "listed-only"},
	}, true)
	if err != nil {
		t.Fatalf("applyModelRefreshWithPrune() error = %v", err)
	}
	if result.Discovered != 2 {
		t.Fatalf("Discovered=%d want 2", result.Discovered)
	}
	if result.Verified != 1 {
		t.Fatalf("Verified=%d want 1", result.Verified)
	}
	listed, err := s.GetModelByChannelAndModelID(ctx, "WorkBuddy", "listed-only")
	if err != nil {
		t.Fatalf("GetModelByChannelAndModelID(listed-only) error = %v", err)
	}
	if listed.Verified {
		t.Fatal("a candidate that was never probed was recorded as verified")
	}
	probed, err := s.GetModelByChannelAndModelID(ctx, "WorkBuddy", "probed")
	if err != nil {
		t.Fatalf("GetModelByChannelAndModelID(probed) error = %v", err)
	}
	if !probed.Verified {
		t.Fatal("a probed candidate was recorded as unverified")
	}
}

func TestApplyModelRefresh_DeletesMissingClineModels(t *testing.T) {
	s, cleanup := setupModelRefreshStore(t)
	defer cleanup()

	ctx := context.Background()
	clearModelsForChannel(t, ctx, s, "Cline")
	for _, record := range []*store.Model{
		{Channel: "Cline", ModelID: "cline/free/opus", Name: "Old Opus", Status: store.ModelStatusAvailable, Verified: true, IsDefault: true, SortOrder: 0, Origin: "discovery"},
		{Channel: "Cline", ModelID: "cline/free/auto", Name: "Auto", Status: store.ModelStatusAvailable, Verified: true, SortOrder: 1, Origin: "discovery"},
	} {
		if err := s.CreateModel(ctx, record); err != nil {
			t.Fatalf("CreateModel() error = %v", err)
		}
	}

	result, err := applyModelRefreshWithPrune(ctx, s, "Cline", "cline_recommended_models", []discoveredModel{
		{ID: "cline/free/auto", Name: "Auto", SortOrder: 0},
		{ID: "cline/free/sonnet", Name: "Sonnet", SortOrder: 1},
	}, true)
	if err != nil {
		t.Fatalf("applyModelRefreshWithPrune() error = %v", err)
	}
	if result.Deleted != 1 {
		t.Fatalf("Deleted=%d want 1", result.Deleted)
	}
	if _, err := s.GetModelByChannelAndModelID(ctx, "Cline", "cline/free/opus"); err == nil {
		t.Fatal("expected old model to be deleted")
	}
	model, err := s.GetModelByChannelAndModelID(ctx, "Cline", "cline/free/auto")
	if err != nil {
		t.Fatalf("GetModelByChannelAndModelID(cline/free/auto) error = %v", err)
	}
	if !model.IsDefault {
		t.Fatal("cline/free/auto IsDefault=false want true")
	}
}

func TestApplyModelRefresh_PreservesExistingModelSettings(t *testing.T) {
	s, cleanup := setupModelRefreshStore(t)
	defer cleanup()

	ctx := context.Background()
	clearModelsForChannel(t, ctx, s, "Cline")
	record := &store.Model{
		Channel:   "Cline",
		ModelID:   "cline/free/sonnet",
		Name:      "Old Name",
		Status:    store.ModelStatusOffline,
		Verified:  false,
		IsDefault: false,
		SortOrder: 999,
	}
	if err := s.CreateModel(ctx, record); err != nil {
		t.Fatalf("CreateModel() error = %v", err)
	}

	candidates := []discoveredModel{{ID: "cline/free/sonnet", Name: "Cline Sonnet", SortOrder: 0}}
	result, err := applyModelRefreshWithPrune(ctx, s, "Cline", "cline_recommended_models", candidates, true)
	if err != nil {
		t.Fatalf("applyModelRefreshWithPrune() error = %v", err)
	}
	if result.Deleted != 0 {
		t.Fatalf("Deleted=%d want 0", result.Deleted)
	}
	if result.Updated != 1 {
		t.Fatalf("Updated=%d want 1 (verification promotion)", result.Updated)
	}

	model, err := s.GetModelByChannelAndModelID(ctx, "Cline", "cline/free/sonnet")
	if err != nil {
		t.Fatalf("GetModelByChannelAndModelID() error = %v", err)
	}
	if model == nil {
		t.Fatal("expected model to remain in store")
	}
	if model.Status != store.ModelStatusOffline {
		t.Fatalf("Status=%q want %q", model.Status, store.ModelStatusOffline)
	}
	if !model.Verified {
		t.Fatal("Verified=false want true after upstream observation")
	}
	if model.Name != "Old Name" {
		t.Fatalf("Name=%q want %q", model.Name, "Old Name")
	}
	if model.SortOrder != 999 {
		t.Fatalf("SortOrder=%d want 999", model.SortOrder)
	}
}

func TestSyncAccountCatalogAggregatesAllAccountsAndProtectsPartialPrune(t *testing.T) {
	channels := []struct {
		name        string
		accountType string
		path        string
		bodyA       string
		bodyB       string
		models      []string
		account     func(baseURL string) (*store.Account, *config.Config)
	}{
		{
			name: "WorkBuddy", accountType: "workbuddy", path: "/v3/config",
			bodyA:  `{"code":0,"data":{"models":[{"id":"wb-a","name":"WB A"}],"agents":[{"name":"cli","models":["wb-a"]}]}}`,
			bodyB:  `{"code":0,"data":{"models":[{"id":"wb-b","name":"WB B"}],"agents":[{"name":"cli","models":["wb-b"]}]}}`,
			models: []string{"wb-a", "wb-b"},
			account: func(baseURL string) (*store.Account, *config.Config) {
				return &store.Account{AccountType: "workbuddy", Name: "wb", Enabled: true, Weight: 1, WorkBuddyAccessToken: "access", WorkBuddyRefreshToken: "refresh", WorkBuddyUID: "uid", WorkBuddyExpiresAt: time.Now().Add(time.Hour)}, &config.Config{WorkBuddyBaseURL: baseURL}
			},
		},
		{
			name: "Qoder", accountType: "qoder", path: "/algo/api/v2/model/list",
			bodyA:  `{"chat":[{"key":"qa","display_name":"Qoder-A","enable":true}]}`,
			bodyB:  `{"chat":[{"key":"qb","display_name":"Qoder-B","enable":true}]}`,
			models: []string{"qoder-a", "qoder-b"},
			account: func(baseURL string) (*store.Account, *config.Config) {
				return qoderTestAccount("11111111-2222-4333-8444-555555555555"), &config.Config{QoderInferenceURL: baseURL}
			},
		},
		{
			name: "Cline", accountType: "cline", path: "/ai/cline/recommended-models",
			bodyA:  `{"free":[{"id":"cline/a","name":"Cline A"}]}`,
			bodyB:  `{"free":[{"id":"cline/b","name":"Cline B"}]}`,
			models: []string{"cline/a", "cline/b"},
			account: func(baseURL string) (*store.Account, *config.Config) {
				return &store.Account{AccountType: "cline", Name: "cline", Enabled: true, Weight: 1, ClineAccessToken: "access", ClineRefreshToken: "refresh", ClineExpiresAt: time.Now().Add(time.Hour)}, &config.Config{ClineAPIBaseURL: baseURL}
			},
		},
	}

	for _, tc := range channels {
		t.Run(tc.name, func(t *testing.T) {
			s, cleanup := setupModelRefreshStore(t)
			defer cleanup()
			ctx := context.Background()
			clearModelsForChannel(t, ctx, s, tc.name)

			var mu sync.Mutex
			bodies := []string{tc.bodyA, tc.bodyB}
			partialPhase := false
			started := make(chan struct{}, 2)
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tc.path {
					http.NotFound(w, r)
					return
				}
				mu.Lock()
				if partialPhase {
					mu.Unlock()
					failedAccount := false
					switch tc.accountType {
					case "workbuddy", "cline":
						failedAccount = strings.Contains(r.Header.Get("Authorization"), "access-2")
					case "qoder":
						failedAccount = r.Header.Get("Cosy-MachineId") == "22222222-3333-4444-8555-666666666666"
					}
					if failedAccount {
						http.Error(w, "temporary account failure", http.StatusBadGateway)
						return
					}
					_, _ = w.Write([]byte(tc.bodyA))
					return
				}
				mu.Unlock()
				started <- struct{}{}
				<-release
				mu.Lock()
				body := bodies[0]
				bodies = bodies[1:]
				mu.Unlock()
				_, _ = w.Write([]byte(body))
			}))
			defer server.Close()

			first, cfg := tc.account(server.URL)
			second, _ := tc.account(server.URL)
			second.Name += "-2"
			switch tc.accountType {
			case "workbuddy":
				second.WorkBuddyAccessToken = "access-2"
			case "cline":
				second.ClineAccessToken = "access-2"
			case "qoder":
				second.QoderMachineID = "22222222-3333-4444-8555-666666666666"
			}
			for _, acc := range []*store.Account{first, second} {
				if err := s.CreateAccount(ctx, acc); err != nil {
					t.Fatal(err)
				}
			}

			done := make(chan struct{})
			var result *modelRefreshResult
			var refreshErr error
			go func() {
				result, refreshErr = syncModelsForChannelConcurrent(ctx, cfg, s, tc.name, 2)
				close(done)
			}()
			<-started
			select {
			case <-started:
				close(release)
			case <-time.After(time.Second):
				t.Fatal("second account did not start concurrently")
			}
			<-done
			if refreshErr != nil {
				t.Fatalf("refresh error = %v", refreshErr)
			}
			if result.AccountsTotal != 2 || result.AccountsSuccess != 2 || result.AccountsFailed != 0 || result.Partial {
				t.Fatalf("result metadata = %+v", result)
			}
			for _, modelID := range tc.models {
				if _, err := s.GetModelByChannelAndModelID(ctx, tc.name, modelID); err != nil {
					t.Fatalf("union missed %s: %v", modelID, err)
				}
			}
			for _, id := range []int64{first.ID, second.ID} {
				stored, err := s.GetAccount(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				if (tc.accountType == "workbuddy" && len(stored.WorkBuddyModelIDs) == 0) ||
					(tc.accountType == "qoder" && len(stored.QoderModelIDs) == 0) ||
					(tc.accountType == "cline" && len(stored.ClineModelIDs) == 0) {
					t.Fatalf("account %d snapshot was not persisted", id)
				}
			}

			// A later single-account failure is partial. The failed account's LKG
			// participates in the safe union and partial mode cannot prune it.
			mu.Lock()
			partialPhase = true
			mu.Unlock()
			result, err := syncModelsForChannelConcurrent(ctx, cfg, s, tc.name, 1)
			if err != nil {
				t.Fatalf("partial refresh error = %v", err)
			}
			if !result.Partial || result.AccountsSuccess != 1 || result.AccountsFailed != 1 || !result.KeptLastKnownGood || result.Deleted != 0 {
				t.Fatalf("partial metadata = %+v", result)
			}
			for _, modelID := range tc.models {
				if _, getErr := s.GetModelByChannelAndModelID(ctx, tc.name, modelID); getErr != nil {
					t.Fatalf("partial refresh pruned %s: %v", modelID, getErr)
				}
			}
		})
	}
}

func TestApplyModelRefreshPartialNeverPrunes(t *testing.T) {
	s, cleanup := setupModelRefreshStore(t)
	defer cleanup()
	ctx := context.Background()
	clearModelsForChannel(t, ctx, s, "Cline")
	for _, id := range []string{"fresh", "failed-account-lkg"} {
		if err := s.CreateModel(ctx, &store.Model{Channel: "Cline", ModelID: id, Name: id, Status: store.ModelStatusAvailable, Verified: true}); err != nil {
			t.Fatal(err)
		}
	}
	result, err := applyModelRefreshWithPrune(ctx, s, "Cline", "cline_recommended_models", []discoveredModel{{ID: "fresh", Name: "fresh", Verified: true}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Deleted != 0 {
		t.Fatalf("Deleted=%d want 0", result.Deleted)
	}
	if _, err := s.GetModelByChannelAndModelID(ctx, "Cline", "failed-account-lkg"); err != nil {
		t.Fatalf("partial refresh pruned LKG: %v", err)
	}
}

func setupModelRefreshStore(t *testing.T) (*store.Store, func()) {
	t.Helper()

	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{
		StoreMode:   "redis",
		RedisAddr:   mini.Addr(),
		RedisPrefix: "model_refresh_test:",
	})
	if err != nil {
		mini.Close()
		t.Fatalf("store.New() error = %v", err)
	}

	return s, func() {
		_ = s.Close()
		mini.Close()
	}
}

func clearModelsForChannel(t *testing.T, ctx context.Context, s *store.Store, channel string) {
	t.Helper()

	models, err := s.ListModels(ctx)
	if err != nil {
		t.Fatalf("ListModels() error = %v", err)
	}
	for _, model := range models {
		if model == nil || !strings.EqualFold(model.Channel, channel) {
			continue
		}
		if err := s.DeleteModel(ctx, model.ID); err != nil {
			t.Fatalf("DeleteModel(%q) error = %v", model.ID, err)
		}
	}
}

// TestShouldDeleteMissingModelsOnRefresh_NeverChannelPrunesOnBuildCatalog pins
// the scope guard: Grok Build is reconciled separately from retired providers.
func TestShouldDeleteMissingModelsOnRefresh_NeverChannelPrunesOnBuildCatalog(t *testing.T) {
	if shouldDeleteMissingModelsOnRefresh("Grok", "grok_build_models") {
		t.Fatal("a Build text-catalog read must not prune the channel catalog")
	}
	// Only complete authoritative account catalogs prune automatically.
	// WorkBuddy's degraded whitelist fallback cannot prove absence.
	for _, tc := range []struct{ channel, source string }{
		{"Qoder", "qoder_upstream_models"},
		{"Cline", "cline_recommended_models"},
	} {
		if !shouldDeleteMissingModelsOnRefresh(tc.channel, tc.source) {
			t.Fatalf("%s/%s must be allowed to prune", tc.channel, tc.source)
		}
	}
	for _, tc := range []struct{ channel, source string }{
		{"Grok", "grok_build_models"},
		{"WorkBuddy", "workbuddy_cli_models"},
	} {
		if shouldDeleteMissingModelsOnRefresh(tc.channel, tc.source) {
			t.Fatalf("%s/%s must retain LKG rather than prune", tc.channel, tc.source)
		}
	}
	// A non-upstream source never prunes.
	for _, source := range []string{"", "test", "cline_cached_models", "grok_build_models_unavailable_cached"} {
		if shouldDeleteMissingModelsOnRefresh("Grok", source) {
			t.Fatalf("source %q must not prune", source)
		}
	}
}

// TestApplyModelRefresh_MarksObservedExistingRowsVerified proves a refresh that
// observes an existing row promotes it to verified.
//
// Creation-only verification left rows that predate the observation permanently
// unverified, and an unverified Grok row is not visible. An authoritative Grok
// Build round now also transfers the matching row to discovery ownership so it
// can be removed when the upstream later withdraws it.
func TestApplyModelRefresh_MarksObservedExistingRowsVerified(t *testing.T) {
	s, cleanup := setupModelRefreshStore(t)
	defer cleanup()

	ctx := context.Background()
	clearModelsForChannel(t, ctx, s, "Grok")
	if err := s.CreateModel(ctx, &store.Model{
		Channel: "Grok", ModelID: "grok-4.6", Name: "Grok 4.6",
		Status: store.ModelStatusAvailable, Verified: false, IsDefault: true,
		Provider: "build", UpstreamModel: "grok-4.6", Origin: "catalog",
	}); err != nil {
		t.Fatalf("CreateModel() error = %v", err)
	}

	result, err := applyModelRefreshWithPrune(ctx, s, "Grok", "grok_build_models", []discoveredModel{
		{ID: "grok-4.6", Name: "Grok 4.6", Verified: true},
	}, true)
	if err != nil {
		t.Fatalf("applyModelRefreshWithPrune() error = %v", err)
	}
	if result.Updated != 1 {
		t.Fatalf("Updated=%d want 1 for the promoted row", result.Updated)
	}
	stored, err := s.GetModelByChannelAndModelID(ctx, "Grok", "grok-4.6")
	if err != nil {
		t.Fatalf("GetModelByChannelAndModelID() error = %v", err)
	}
	if !stored.Verified {
		t.Fatal("an observed row was not marked verified")
	}
	if !stored.IsDefault {
		t.Fatal("the operator-owned default was changed by the promotion")
	}
	if stored.Origin != "discovery" {
		t.Fatalf("origin=%q, want authoritative upstream ownership", stored.Origin)
	}
}

func TestGrokPartialCatalogNeverPrunes(t *testing.T) {
	s, cleanup := setupModelRefreshStore(t)
	defer cleanup()
	ctx := context.Background()
	clearModelsForChannel(t, ctx, s, "Grok")
	for id := int64(1); id <= 2; id++ {
		if err := s.CreateAccount(ctx, &store.Account{AccountType: "grok", Name: fmt.Sprintf("build-%d", id), Enabled: true, AuthStatus: store.AccountAuthStatusActive, CredentialType: "oauth", GrokProvider: "build", OAuthAccessToken: "token", GrokModels: []string{"old"}, GrokModelsSyncedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateModel(ctx, &store.Model{Channel: "Grok", ModelID: "old", Name: "old", Provider: "build", Origin: "discovery", Status: store.ModelStatusAvailable, Verified: true}); err != nil {
		t.Fatal(err)
	}
	previous := fetchGrokBuildModelsForRefresh
	defer func() { fetchGrokBuildModelsForRefresh = previous }()
	fetchGrokBuildModelsForRefresh = func(_ context.Context, _ *config.Config, _ *store.Store, acc *store.Account) ([]modelcatalog.Profile, error) {
		if acc.Name == "build-1" {
			return []modelcatalog.Profile{{ModelID: "new"}}, nil
		}
		return nil, fmt.Errorf("temporary")
	}
	result, err := syncModelsForChannelConcurrent(ctx, &config.Config{}, s, "Grok", 2)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Partial || result.Deleted != 0 {
		t.Fatalf("result=%+v", result)
	}
	if _, err := s.GetModelByChannelAndModelID(ctx, "Grok", "old"); err != nil {
		t.Fatalf("partial refresh deleted LKG: %v", err)
	}
}
