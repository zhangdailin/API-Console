package modelrefresh

import (
	"context"

	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"encoding/json"

	"orchids-api/internal/config"

	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func TestMakeModelRefreshHandler_UsesBodyChannel(t *testing.T) {
	prev := runModelRefresh
	defer func() { runModelRefresh = prev }()

	runModelRefresh = func(ctx context.Context, cfg *config.Config, s *store.Store, channel string, concurrency int) (*Result, error) {
		return &Result{Channel: channel, Source: "stub", Concurrency: concurrency, Discovered: 3, Verified: 2}, nil
	}

	handler := NewRefreshHandler(func() *config.Config { return &config.Config{} }, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/models/refresh?channel=cline&concurrency=99", strings.NewReader(`{"channel":"workbuddy","concurrency":8}`))
	rec := httptest.NewRecorder()

	handler(rec, req)

	testutil.Equal(t, rec.Code, http.StatusOK)

	var resp Result
	testutil.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "decode response: %v")
	testutil.Equal(t, resp.Channel, "workbuddy")
	testutil.Equal(t, resp.Verified, 2)
	testutil.Equal(t, resp.Concurrency, 8)
}

func TestModelRefreshCoordinatorRejectsDuplicateChannel(t *testing.T) {
	prev := runModelRefresh
	defer func() { runModelRefresh = prev }()
	started := make(chan struct{})
	release := make(chan struct{})
	runModelRefresh = func(ctx context.Context, cfg *config.Config, s *store.Store, channel string, concurrency int) (*Result, error) {
		close(started)
		<-release
		return &Result{Channel: channel}, nil
	}
	handler := NewRefreshHandlerWithCoordinator(func() *config.Config { return &config.Config{} }, nil, NewCoordinator())
	firstDone := make(chan struct{})
	go func() {
		handler(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/models/refresh?channel=workbuddy", nil))
		close(firstDone)
	}()
	<-started
	second := httptest.NewRecorder()
	handler(second, httptest.NewRequest(http.MethodPost, "/api/models/refresh?channel=workbuddy", nil))
	testutil.Equal(t, second.Code, http.StatusConflict)
	close(release)
	<-firstDone
}

func TestRunIndexedModelRefreshWorkersVisitsEachIndexOnce(t *testing.T) {
	const total = 37
	counts := make([]int, total)
	var mu sync.Mutex

	runIndexedModelRefreshWorkers(total, 5, func(index int) { mu.Lock(); counts[index]++; mu.Unlock() })

	for _, count := range counts {
		testutil.Equal(t, count, 1)
	}
}

func TestRunIndexedModelRefreshWorkersHandlesEmptyWork(t *testing.T) {
	called := false
	runIndexedModelRefreshWorkers(0, 4, func(int) { called = true })
	runIndexedModelRefreshWorkers(3, 4, nil)
	testutil.False(t, called, "worker called for empty input")
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
			testutil.Equal(t, normalizeModelRefreshConcurrency(tt.in), tt.want)
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
		testutil.Equal(t, got, tt.want)
		testutil.Equal(t, ok, tt.ok)
	}
}

func TestSyncModelsForChannelConcurrent_WorkBuddyRequiresAccountDiscovery(t *testing.T) {
	s, cleanup := setupModelRefreshStore(t)
	defer cleanup()

	ctx := context.Background()
	clearModelsForChannel(t, ctx, s, "WorkBuddy")

	_, err := syncModelsForChannelConcurrent(ctx, &config.Config{}, s, "WorkBuddy", 8)
	testutil.Error(t, err, "syncModelsForChannelConcurrent() result=%+v want error")
	// Without an active account nothing is read and nothing is published: the
	// refresh reports the missing account rather than a cached catalog.
	testutil.True(t, isNoActiveAccounts(err), "error=%v want a no-active-account report")
	models, listErr := s.ListModels(ctx)
	testutil.NoError(t, listErr, "ListModels() error = %v")
	testutil.Equal(t, len(models), 0)
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

	_, err := syncModelsForChannelConcurrent(ctx, &config.Config{}, s, "Cline", 4)
	testutil.Error(t, err, "syncModelsForChannelConcurrent() result=%+v want error")
	testutil.True(t, isNoActiveAccounts(err), "error=%v want a no-active-account report")

	stored, getErr := s.GetModelByChannelAndModelID(ctx, "Cline", "cline-a")
	testutil.Falsef(t, getErr != nil || stored == nil, "stored row was destroyed by a failed refresh: %v", getErr)
	testutil.Falsef(t, stored.Status != store.ModelStatusAvailable || !stored.Verified, "stored row was modified by a failed refresh: %+v", stored)
}

func TestChooseRefreshedDefaultModel_PrefersExistingDefault(t *testing.T) {
	existing := map[string]*store.Model{
		"a": {ModelID: "a", IsDefault: true},
		"b": {ModelID: "b", IsDefault: false},
	}
	ordered := []discoveredModel{{ID: "b"}, {ID: "a"}}

	got := chooseRefreshedDefaultModel(existing, ordered)
	testutil.Equal(t, got, "a")
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
				testutil.NoError(t, s.CreateAccount(ctx, acc))
			}

			done := make(chan struct{})
			var result *Result
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
			testutil.NoError(t, refreshErr, "refresh error = %v")
			testutil.Falsef(t, result.AccountsTotal != 2 || result.AccountsSuccess != 2 || result.AccountsFailed != 0 || result.Partial, "result metadata = %+v", result)
			for _, modelID := range tc.models {
				_, err := s.GetModelByChannelAndModelID(ctx, tc.name, modelID)
				testutil.CheckNoError(t, err)
			}
			for _, id := range []int64{first.ID, second.ID} {
				stored, err := s.GetAccount(ctx, id)
				testutil.NoError(t, err)
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
			testutil.NoError(t, err, "partial refresh error = %v")
			testutil.Falsef(t, !result.Partial || result.AccountsSuccess != 1 || result.AccountsFailed != 1 || !result.KeptLastKnownGood || result.Deleted != 0, "partial metadata = %+v", result)
			for _, modelID := range tc.models {
				_, getErr := s.GetModelByChannelAndModelID(ctx, tc.name, modelID)
				testutil.CheckNoError(t, getErr)
			}
		})
	}
}

func clearModelsForChannel(t *testing.T, ctx context.Context, s *store.Store, channel string) {
	t.Helper()

	models, err := s.ListModels(ctx)
	testutil.NoError(t, err, "ListModels() error = %v")
	for _, model := range models {
		if model == nil || !strings.EqualFold(model.Channel, channel) {
			continue
		}
		err := s.DeleteModel(ctx, model.ID)
		testutil.CheckNoError(t, err)
	}
}
