package store

import (
	"context"
	"errors"
	"testing"

	"orchids-api/internal/testutil"
)

func TestRetiredGrokRoutesAreHiddenWithoutDeletingStoredRows(t *testing.T) {
	s, _ := newTestRedisStore(t, "test:")
	ctx := context.Background()
	build := &Model{Channel: "grok", ModelID: "grok-4.5", Provider: GrokProviderBuild}
	web := &Model{Channel: "grok", ModelID: "grok-chat-auto", Provider: GrokProviderWeb}
	retired := []*Model{
		{Channel: "grok", ModelID: "console/grok-4.5", Provider: "console"},
		{Channel: "grok", ModelID: "console/grok-imagine-video-1.5"},
	}
	for _, m := range []*Model{build, web} {
		testutil.NoError(t, s.CreateModel(ctx, m))
	}
	for _, m := range retired {
		testutil.NoError(t, s.createModel(ctx, m))
	}

	s.prepareModels()
	models, err := s.ListModels(ctx)
	testutil.NoError(t, err)
	testutil.Equal(t, len(models), 2)
	for _, m := range retired {
		_, err := s.GetModel(ctx, m.ID)
		testutil.True(t, errors.Is(err, ErrNoRows))
		_, err = s.GetModelByChannelAndModelID(ctx, m.Channel, m.ModelID)
		testutil.True(t, errors.Is(err, ErrNoRows))
		stored, err := s.readModel(ctx, m.ID)
		testutil.NoError(t, err)
		testutil.Equal(t, stored.Provider, m.Provider)
	}
}

func TestReconcileDiscoveredModelsProtectsManualAndPrunesDiscovery(t *testing.T) {
	s, _ := newTestRedisStore(t, "test:")
	ctx := context.Background()
	manual := &Model{Channel: "Cline", ModelID: "manual", Name: "operator name", Status: ModelStatusMaintenance}
	stale := &Model{Channel: "Cline", ModelID: "stale", Name: "stale", Status: ModelStatusAvailable, Origin: "discovery"}
	other := &Model{Channel: "WorkBuddy", ModelID: "stale", Name: "other channel", Status: ModelStatusAvailable, Origin: "discovery"}
	for _, m := range []*Model{manual, stale, other} {
		testutil.NoError(t, s.CreateModel(ctx, m))
	}

	result, err := s.ReconcileDiscoveredModels(ctx, " Cline ", []*Model{
		{ModelID: "manual", Name: "feed name", Status: ModelStatusAvailable},
		{ModelID: "fresh", Name: "Fresh", Status: ModelStatusAvailable, Verified: true},
	}, ModelReconcileOptions{Prune: true})
	testutil.NoError(t, err)
	testutil.Falsef(t, result.Added != 1 || result.Deleted != 1 || result.Protected != 1, "result=%+v", result)
	gotManual, err := s.GetModelByChannelAndModelID(ctx, "cline", "manual")
	testutil.Falsef(t, err != nil || gotManual.Name != "operator name" || gotManual.Status != ModelStatusMaintenance || gotManual.Origin != "manual", "manual=%+v err=%v", gotManual, err)
	_, err = s.GetModelByChannelAndModelID(ctx, "cline", "stale")
	testutil.Error(t, err)
	_, err = s.GetModelByChannelAndModelID(ctx, "workbuddy", "stale")
	testutil.CheckNoError(t, err)
	fresh, err := s.GetModelByChannelAndModelID(ctx, "cline", "fresh")
	testutil.Equal(t, err, nil)
	testutil.Equal(t, fresh.Origin, "discovery")
	testutil.Equal(t, fresh.Channel, "Cline")
}

func TestReconcileDiscoveredModelsUpsertsAndOptionalPrune(t *testing.T) {
	s, _ := newTestRedisStore(t, "test:")
	ctx := context.Background()
	old := &Model{Channel: "Cline", ModelID: "same", Name: "old", Status: ModelStatusOffline, Origin: "discovery"}
	missing := &Model{Channel: "Cline", ModelID: "missing", Name: "missing", Status: ModelStatusAvailable, Origin: "discovery"}
	for _, m := range []*Model{old, missing} {
		testutil.NoError(t, s.CreateModel(ctx, m))
	}
	oldID := old.ID

	result, err := s.ReconcileDiscoveredModels(ctx, "Cline", []*Model{{ModelID: "same", Name: "new", Status: ModelStatusAvailable}}, ModelReconcileOptions{})
	testutil.NoError(t, err)
	testutil.Equal(t, result.Updated, 1)
	testutil.Equal(t, result.Deleted, 0)
	updated, err := s.GetModelByChannelAndModelID(ctx, "cline", "same")
	testutil.Falsef(t, err != nil || updated.ID != oldID || updated.Name != "new" || updated.Origin != "discovery", "updated=%+v err=%v", updated, err)
	_, err = s.GetModelByChannelAndModelID(ctx, "cline", "missing")
	testutil.CheckNoError(t, err)
}

func TestReconcileDiscoveredModelsValidatesBeforeWriting(t *testing.T) {
	s, _ := newTestRedisStore(t, "test:")
	ctx := context.Background()
	_, err := s.ReconcileDiscoveredModels(ctx, "workbuddy", []*Model{{ModelID: "dup"}, {ModelID: "dup"}}, ModelReconcileOptions{Prune: true})
	testutil.False(t, err == nil, "expected duplicate error")
	models, err := s.ListModels(ctx)
	testutil.Falsef(t, err != nil || len(models) != 0, "partial write: models=%+v err=%v", models, err)
}
