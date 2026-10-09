package modelrefresh

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/modelcatalog"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

// TestDiscoverGrokModelsWithoutActiveAccountReportsNoAccount proves the channel
// no longer has a historical-catalog fallback.
func TestDiscoverGrokModelsWithoutActiveAccountReportsNoAccount(t *testing.T) {
	s, cleanup := setupModelRefreshStore(t)
	defer cleanup()

	report, err := discoverGrokModelsReport(context.Background(), &config.Config{}, s, 4)
	items, _ := report.Candidates, report.Source
	testutil.Error(t, err, "discoverGrokModelsReport() items=%+v source=%q want error")
	testutil.True(t, isNoActiveAccounts(err), "error=%v want a no-active-account report")
	testutil.Equal(t, len(items), 0)
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
	testutil.NoError(t, s.CreateAccount(ctx, acc), "CreateAccount() error = %v")

	prevFetch := fetchGrokBuildModelsForRefresh
	t.Cleanup(func() { fetchGrokBuildModelsForRefresh = prevFetch })
	var calls int
	fetchGrokBuildModelsForRefresh = func(ctx context.Context, cfg *config.Config, store *store.Store, got *store.Account) ([]modelcatalog.Profile, error) {
		calls++
		testutil.Equal(t, got.ID, acc.ID)
		return []modelcatalog.Profile{{ModelID: "grok-4.6"}, {ModelID: "grok-4.6"}, {ModelID: "future-private-model"}, {ModelID: "grok-4.5"}}, nil
	}

	report, err := discoverGrokModelsReport(ctx, &config.Config{}, s, 4)
	items, source := report.Candidates, report.Source
	testutil.NoError(t, err, "discoverGrokModelsReport() error = %v")
	testutil.Equal(t, calls, 1)
	testutil.Equal(t, source, "grok_build_models")
	gotIDs := make([]string, 0, len(items))
	for _, item := range items {
		gotIDs = append(gotIDs, item.ID)
	}
	// The upstream catalog plus the entries derived from the account:
	// 4.6 implies 4.5, and an OAuth Build account can serve Composer. The row for
	// the catalog model is published under its bare public name.
	wantIDs := "grok-4.6,future-private-model,grok-4.5,grok-composer-2.5-fast"
	testutil.Equal(t, strings.Join(gotIDs, ","), wantIDs)

	persisted, err := s.GetAccount(ctx, acc.ID)
	testutil.NoError(t, err, "GetAccount() error = %v")
	testutil.Falsef(t, persisted.GrokProvider != "build" || persisted.GrokModelsSyncedAt.IsZero(), "provider/catalog not persisted: %+v", persisted)
	testutil.Equal(t, strings.Join(persisted.GrokModels, ","), "grok-4.6,future-private-model,grok-4.5,grok-composer-2.5-fast")
}

func TestDiscoverGrokModelsWithoutUpstreamCatalogPublishesNothing(t *testing.T) {
	s, cleanup := setupModelRefreshStore(t)
	defer cleanup()
	ctx := context.Background()
	acc := &store.Account{AccountType: "grok", CredentialType: "oauth", OAuthRefreshToken: "refresh", Enabled: true}
	testutil.NoError(t, s.CreateAccount(ctx, acc), "CreateAccount() error = %v")
	prevFetch := fetchGrokBuildModelsForRefresh
	t.Cleanup(func() { fetchGrokBuildModelsForRefresh = prevFetch })
	fetchGrokBuildModelsForRefresh = func(context.Context, *config.Config, *store.Store, *store.Account) ([]modelcatalog.Profile, error) {
		return nil, errors.New("control plane unavailable")
	}

	report, err := discoverGrokModelsReport(ctx, &config.Config{}, s, 1)
	items, source := report.Candidates, report.Source
	testutil.Error(t, err, "discoverGrokModelsReport() items=%+v source=%q want error")
	testutil.Equal(t, source, "")
	testutil.Equal(t, len(items), 0)
	// The failure must not be reported as a cached observation.
	testutil.MustNotContain(t, err.Error(), "cached")
}

// TestShouldDeleteMissingModelsOnRefresh_NeverChannelPrunesOnBuildCatalog pins
// the scope guard: Grok Build is reconciled separately from retired providers.
func TestShouldDeleteMissingModelsOnRefresh_NeverChannelPrunesOnBuildCatalog(t *testing.T) {
	testutil.False(t, shouldDeleteMissingModelsOnRefresh("Grok", "grok_build_models"), "a Build text-catalog read must not prune the channel catalog")
	// Only complete authoritative account catalogs prune automatically.
	// WorkBuddy's degraded whitelist fallback cannot prove absence.
	for _, tc := range []struct{ channel, source string }{
		{"Qoder", "qoder_upstream_models"},
		{"Cline", "cline_recommended_models"},
	} {
		testutil.True(t, shouldDeleteMissingModelsOnRefresh(tc.channel, tc.source), "%s/%s must be allowed to prune")
	}
	for _, tc := range []struct{ channel, source string }{
		{"Grok", "grok_build_models"},
		{"WorkBuddy", "workbuddy_cli_models"},
	} {
		testutil.Falsef(t, shouldDeleteMissingModelsOnRefresh(tc.channel, tc.source), "%s/%s must retain LKG rather than prune", tc.channel, tc.source)
	}
	// A non-upstream source never prunes.
	for _, source := range []string{"", "test", "cline_cached_models", "grok_build_models_unavailable_cached"} {
		testutil.Falsef(t, shouldDeleteMissingModelsOnRefresh("Grok", source), "source %q must not prune", source)
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
	testutil.NoError(t, err, "applyModelRefreshWithPrune() error = %v")
	testutil.Equal(t, result.Updated, 1)
	stored, err := s.GetModelByChannelAndModelID(ctx, "Grok", "grok-4.6")
	testutil.NoError(t, err, "GetModelByChannelAndModelID() error = %v")
	testutil.False(t, !stored.Verified, "an observed row was not marked verified")
	testutil.False(t, !stored.IsDefault, "the operator-owned default was changed by the promotion")
	testutil.Equal(t, stored.Origin, "discovery")
}

func TestGrokPartialCatalogNeverPrunes(t *testing.T) {
	s, cleanup := setupModelRefreshStore(t)
	defer cleanup()
	ctx := context.Background()
	clearModelsForChannel(t, ctx, s, "Grok")
	for id := int64(1); id <= 2; id++ {
		testutil.NoError(t, s.CreateAccount(ctx, &store.Account{AccountType: "grok", Name: fmt.Sprintf("build-%d", id), Enabled: true, AuthStatus: store.AccountAuthStatusActive, CredentialType: "oauth", GrokProvider: "build", OAuthAccessToken: "token", GrokModels: []string{"old"}, GrokModelsSyncedAt: time.Now()}))
	}
	testutil.NoError(t, s.CreateModel(ctx, &store.Model{Channel: "Grok", ModelID: "old", Name: "old", Provider: "build", Origin: "discovery", Status: store.ModelStatusAvailable, Verified: true}))
	previous := fetchGrokBuildModelsForRefresh
	defer func() { fetchGrokBuildModelsForRefresh = previous }()
	fetchGrokBuildModelsForRefresh = func(_ context.Context, _ *config.Config, _ *store.Store, acc *store.Account) ([]modelcatalog.Profile, error) {
		if acc.Name == "build-1" {
			return []modelcatalog.Profile{{ModelID: "new"}}, nil
		}
		return nil, fmt.Errorf("temporary")
	}
	result, err := syncModelsForChannelConcurrent(ctx, &config.Config{}, s, "Grok", 2)
	testutil.NoError(t, err)
	testutil.Falsef(t, !result.Partial || result.Deleted != 0, "result=%+v", result)
	_, err = s.GetModelByChannelAndModelID(ctx, "Grok", "old")
	testutil.CheckNoError(t, err)
}

func TestGrokCatalogPanicIsAccountFailureWithoutPruning(t *testing.T) {
	for _, workers := range []int{1, 4} {
		t.Run(fmt.Sprint(workers), func(t *testing.T) {
			s, cleanup := setupModelRefreshStore(t)
			defer cleanup()
			ctx := context.Background()
			clearModelsForChannel(t, ctx, s, "Grok")
			for _, name := range []string{"healthy", "panic"} {
				testutil.NoError(t, s.CreateAccount(ctx, &store.Account{AccountType: "grok", Name: name, Enabled: true, CredentialType: "oauth", OAuthAccessToken: "token", GrokModels: []string{"old"}}))
			}
			testutil.NoError(t, s.CreateModel(ctx, &store.Model{Channel: "Grok", ModelID: "old", Name: "old", Provider: "build", Origin: "discovery", Status: store.ModelStatusAvailable, Verified: true}))
			previous := fetchGrokBuildModelsForRefresh
			defer func() { fetchGrokBuildModelsForRefresh = previous }()
			fetchGrokBuildModelsForRefresh = func(_ context.Context, _ *config.Config, _ *store.Store, acc *store.Account) ([]modelcatalog.Profile, error) {
				if acc.Name == "panic" {
					panic("private credential")
				}
				return []modelcatalog.Profile{{ModelID: "new"}}, nil
			}
			report, err := discoverGrokModelsReport(ctx, &config.Config{}, s, workers)
			testutil.NoError(t, err)
			succeeded, failed := report.counts()
			if succeeded != 1 || failed != 1 {
				t.Fatalf("counts=%d/%d", succeeded, failed)
			}
			for _, attempt := range report.Attempts {
				if attempt.Err != nil && attempt.Err.Error() != "task panicked" {
					t.Fatalf("unsafe task error: %v", attempt.Err)
				}
			}
			result, err := syncModelsForChannelConcurrent(ctx, &config.Config{}, s, "Grok", workers)
			testutil.NoError(t, err)
			if !result.Partial || result.AccountsFailed != 1 || result.Deleted != 0 {
				t.Fatalf("result=%+v", result)
			}
			_, err = s.GetModelByChannelAndModelID(ctx, "Grok", "old")
			testutil.NoError(t, err)
		})
	}
}

func TestCatalogSnapshotsRequireJSONRows(t *testing.T) {
	for _, ch := range []string{"workbuddy", "cline", "qoder"} {
		t.Run(ch, func(t *testing.T) {
			account := &store.Account{WorkBuddyModelIDs: []string{"old"}, ClineModelIDs: []string{"old"}, QoderModelIDs: []string{"old"}}
			if got := accountCatalogSnapshotToDiscovered(ch, account); len(got) != 0 {
				t.Fatalf("legacy accepted: %+v", got)
			}
			rows := []string{`{"id":"fresh"}`}
			account.WorkBuddyModelIDs = rows
			account.ClineModelIDs = rows
			account.QoderModelIDs = rows
			if got := accountCatalogSnapshotToDiscovered(ch, account); len(got) != 1 || got[0].ID != "fresh" {
				t.Fatalf("current unavailable: %+v", got)
			}
		})
	}
}
