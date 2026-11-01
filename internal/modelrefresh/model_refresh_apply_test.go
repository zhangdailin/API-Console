package modelrefresh

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

// TestApplyModelRefresh_RefusesNonUpstreamSources is the gate that keeps a
// cached or compiled-in catalog out of model management: only a source that
// names an upstream catalog read may write.
func TestApplyModelRefresh_RefusesNonUpstreamSources(t *testing.T) {
	for _, source := range []string{
		"test",
		"workbuddy_builtin_catalog",
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

			_, err := applyModelRefreshWithPrune(ctx, s, "WorkBuddy", source, []discoveredModel{{ID: "injected", Name: "injected", Verified: true}}, true)
			testutil.Error(t, err, "applyModelRefreshWithPrune() result=%+v want a refusal for source %q")
			_, getErr := s.GetModelByChannelAndModelID(ctx, "WorkBuddy", "injected")
			testutil.Error(t, getErr)
			_, getErr = s.GetModelByChannelAndModelID(ctx, "WorkBuddy", "existing")
			testutil.CheckNoError(t, getErr)
		})
	}
}

// TestApplyModelRefresh_IsUpstreamCatalogSource pins the allowlist itself.
func TestApplyModelRefresh_IsUpstreamCatalogSource(t *testing.T) {
	allowed := []string{
		"grok_build_models",
		"workbuddy_cli_models",
		"cline_recommended_models",
	}
	for _, source := range allowed {
		testutil.True(t, isUpstreamCatalogSource(source), "isUpstreamCatalogSource(%q) = false, want true")
	}
	refused := []string{
		"",
		"test",
		"workbuddy_builtin_catalog",
		"grok_cached_models",
		"cline_cached_models",
		"grok_build_models_unavailable_cached",
	}
	for _, source := range refused {
		testutil.Falsef(t, isUpstreamCatalogSource(source), "isUpstreamCatalogSource(%q) = true, want false", source)
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
	testutil.NoError(t, err, "applyModelRefreshWithPrune() error = %v")
	testutil.Equal(t, result.Discovered, 2)
	testutil.Equal(t, result.Verified, 1)
	listed, err := s.GetModelByChannelAndModelID(ctx, "WorkBuddy", "listed-only")
	testutil.NoError(t, err, "GetModelByChannelAndModelID(listed-only) error = %v")
	testutil.False(t, listed.Verified, "a candidate that was never probed was recorded as verified")
	probed, err := s.GetModelByChannelAndModelID(ctx, "WorkBuddy", "probed")
	testutil.NoError(t, err, "GetModelByChannelAndModelID(probed) error = %v")
	testutil.False(t, !probed.Verified, "a probed candidate was recorded as unverified")
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
		testutil.NoError(t, s.CreateModel(ctx, record), "CreateModel() error = %v")
	}

	result, err := applyModelRefreshWithPrune(ctx, s, "Cline", "cline_recommended_models", []discoveredModel{
		{ID: "cline/free/auto", Name: "Auto", SortOrder: 0},
		{ID: "cline/free/sonnet", Name: "Sonnet", SortOrder: 1},
	}, true)
	testutil.NoError(t, err, "applyModelRefreshWithPrune() error = %v")
	testutil.Equal(t, result.Deleted, 1)
	_, err = s.GetModelByChannelAndModelID(ctx, "Cline", "cline/free/opus")
	testutil.Error(t, err)
	model, err := s.GetModelByChannelAndModelID(ctx, "Cline", "cline/free/auto")
	testutil.NoError(t, err, "GetModelByChannelAndModelID(cline/free/auto) error = %v")
	testutil.False(t, !model.IsDefault, "cline/free/auto IsDefault=false want true")
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
	testutil.NoError(t, s.CreateModel(ctx, record), "CreateModel() error = %v")

	candidates := []discoveredModel{{ID: "cline/free/sonnet", Name: "Cline Sonnet", SortOrder: 0}}
	result, err := applyModelRefreshWithPrune(ctx, s, "Cline", "cline_recommended_models", candidates, true)
	testutil.NoError(t, err, "applyModelRefreshWithPrune() error = %v")
	testutil.Equal(t, result.Deleted, 0)
	testutil.Equal(t, result.Updated, 1)

	model, err := s.GetModelByChannelAndModelID(ctx, "Cline", "cline/free/sonnet")
	testutil.NoError(t, err, "GetModelByChannelAndModelID() error = %v")
	testutil.False(t, model == nil, "expected model to remain in store")
	testutil.Equal(t, model.Status, store.ModelStatusOffline)
	testutil.False(t, !model.Verified, "Verified=false want true after upstream observation")
	testutil.Equal(t, model.Name, "Old Name")
	testutil.Equal(t, model.SortOrder, 999)
}

func TestApplyModelRefreshPartialNeverPrunes(t *testing.T) {
	s, cleanup := setupModelRefreshStore(t)
	defer cleanup()
	ctx := context.Background()
	clearModelsForChannel(t, ctx, s, "Cline")
	for _, id := range []string{"fresh", "failed-account-lkg"} {
		testutil.NoError(t, s.CreateModel(ctx, &store.Model{Channel: "Cline", ModelID: id, Name: id, Status: store.ModelStatusAvailable, Verified: true}))
	}
	result, err := applyModelRefreshWithPrune(ctx, s, "Cline", "cline_recommended_models", []discoveredModel{{ID: "fresh", Name: "fresh", Verified: true}}, false)
	testutil.NoError(t, err)
	testutil.Equal(t, result.Deleted, 0)
	_, err = s.GetModelByChannelAndModelID(ctx, "Cline", "failed-account-lkg")
	testutil.CheckNoError(t, err)
}

func setupModelRefreshStore(t *testing.T) (*store.Store, func()) {
	t.Helper()

	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{
		RedisAddr:   mini.Addr(),
		RedisPrefix: "model_refresh_test:",
	})
	testutil.NoError(t, err, "store.New() error = %v")

	return s, func() { _ = s.Close() }
}
