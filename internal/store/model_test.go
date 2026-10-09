package store

import (
	"context"
	"encoding/json"
	"orchids-api/internal/testutil"
	"testing"

	"github.com/alicebob/miniredis/v2"
)

func TestModelStatus_UnmarshalJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    ModelStatus
		enabled bool
	}{
		{name: "bool true", input: `true`, want: ModelStatusOffline, enabled: false},
		{name: "bool false", input: `false`, want: ModelStatusOffline, enabled: false},
		{name: "available", input: `"available"`, want: ModelStatusAvailable, enabled: true},
		{name: "maintenance", input: `"maintenance"`, want: ModelStatusMaintenance, enabled: false},
		{name: "offline", input: `"offline"`, want: ModelStatusOffline, enabled: false},
		{name: "old alias", input: `"enabled"`, want: ModelStatusOffline},
		{name: "case alias", input: `"Available"`, want: ModelStatusOffline},
		{name: "unknown", input: `"something"`, want: ModelStatusOffline, enabled: false},
		{name: "null", input: `null`, want: ModelStatusOffline, enabled: false},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var s ModelStatus
			testutil.NoError(t, json.Unmarshal([]byte(tt.input), &s), "unmarshal failed: %v")
			testutil.Equal(t, s, tt.want)
			testutil.Equal(t, s.Enabled(), tt.enabled)
		})
	}
}

func TestModelStatus_MarshalJSON(t *testing.T) {
	t.Parallel()

	b, err := json.Marshal(ModelStatusAvailable)
	testutil.NoError(t, err, "marshal failed: %v")
	testutil.Equal(t, string(b), `"available"`)
}

func TestGetModelByChannelAndModelID_AllowsDuplicateModelIDsAcrossChannels(t *testing.T) {
	t.Parallel()

	s, _ := newTestRedisStore(t, "test:")

	ctx := context.Background()

	// The store publishes nothing on its own, so the two channels' fixtures are
	// created explicitly. The point of the test is that the lookup index is keyed
	// by channel *and* model id, not that either channel has a catalog.
	const sharedModelID = "shared-model"
	if err := s.CreateModel(ctx, &Model{
		Channel: "WorkBuddy", ModelID: sharedModelID, Name: "WorkBuddy Shared",
		Status: ModelStatusAvailable, Verified: true,
	}); err != nil {
		t.Fatalf("CreateModel(workbuddy) error = %v", err)
	}
	if err := s.CreateModel(ctx, &Model{
		Channel: "Qoder", ModelID: sharedModelID, Name: "Qoder Shared",
		Status: ModelStatusAvailable, Verified: true,
	}); err != nil {
		t.Fatalf("CreateModel(qoder) error = %v", err)
	}

	workBuddyModel, err := s.GetModelByChannelAndModelID(ctx, "workbuddy", sharedModelID)
	testutil.NoError(t, err, "GetModelByChannelAndModelID(workbuddy) error = %v")
	testutil.Equal(t, workBuddyModel.Channel, "WorkBuddy")

	qoderModel, err := s.GetModelByChannelAndModelID(ctx, "qoder", sharedModelID)
	testutil.NoError(t, err, "GetModelByChannelAndModelID(qoder) error = %v")
	testutil.Equal(t, qoderModel.Channel, "Qoder")
	if workBuddyModel.ModelID != sharedModelID || qoderModel.ModelID != sharedModelID ||
		workBuddyModel.Name != "WorkBuddy Shared" || qoderModel.Name != "Qoder Shared" {
		t.Fatalf("channel index mixed records: workbuddy=%+v qoder=%+v", workBuddyModel, qoderModel)
	}
	testutil.NotEqual(t, qoderModel.ID, workBuddyModel.ID)
}

// TestStoreNew_PublishesNoBuiltInModels pins the startup contract: a new store
// carries no model rows at all.
//
// Model management publishes only catalogs read from upstream for an active
// account, so a fresh deployment starts empty. A compiled-in seed here would
// make the admin page report models no account ever advertised, and would keep
// them served after an upstream withdrew them.
func TestStoreNew_PublishesNoBuiltInModels(t *testing.T) {
	t.Parallel()

	s, _ := newTestRedisStore(t, "test:")

	ctx := context.Background()
	models, err := s.ListModels(ctx)
	testutil.NoError(t, err, "ListModels() error = %v")
	testutil.Equal(t, len(models), 0)
	for _, probe := range []struct{ channel, modelID string }{
		{"Grok", "grok-4.5"},
		{"Grok", "grok-imagine-image"},
		{"Qoder", "auto-open"},
		{"WorkBuddy", "claude-opus-5"},
		{"WorkBuddy", "default-model"},
		{"Qoder", "Qwen3.7-Max"},
	} {
		_, err := s.GetModelByChannelAndModelID(ctx, probe.channel, probe.modelID)
		testutil.CheckError(t, err)
	}
}

// TestStoreNew_PreservesExistingModelList proves a restart does not resurrect a
// deleted row: nothing recreates model records at startup.
func TestStoreNew_PreservesExistingModelList(t *testing.T) {
	t.Parallel()

	mini := miniredis.RunT(t)
	opts := Options{
		RedisAddr:   mini.Addr(),
		RedisDB:     0,
		RedisPrefix: "test:",
	}
	s, err := New(opts)
	testutil.NoError(t, err, "store.New() error = %v")

	ctx := context.Background()
	if err := s.CreateModel(ctx, &Model{
		Channel: "WorkBuddy", ModelID: "deepseek-v4-pro", Name: "deepseek-v4-pro",
		Status: ModelStatusAvailable, Verified: true,
	}); err != nil {
		t.Fatalf("CreateModel() error = %v", err)
	}
	model, err := s.GetModelByChannelAndModelID(ctx, "workbuddy", "deepseek-v4-pro")
	testutil.NoError(t, err, "GetModelByChannelAndModelID() error = %v")
	testutil.NoError(t, s.DeleteModel(ctx, model.ID), "DeleteModel() error = %v")
	_ = s.Close()

	s, err = New(opts)
	testutil.NoError(t, err, "store.New() second error = %v")
	t.Cleanup(func() { _ = s.Close() })

	_, err = s.GetModelByChannelAndModelID(ctx, "workbuddy", "deepseek-v4-pro")
	testutil.Error(t, err)
}

// TestStoreNew_KeepsUpstreamDiscoveredModels proves startup maintenance never
// prunes a row that an upstream refresh published. Pruning is the refresh's job:
// it knows which catalog it just read, while startup knows nothing.
func TestStoreNew_KeepsUpstreamDiscoveredModels(t *testing.T) {
	t.Parallel()

	mini := miniredis.RunT(t)
	opts := Options{
		RedisAddr:   mini.Addr(),
		RedisDB:     0,
		RedisPrefix: "test:",
	}
	s, err := New(opts)
	testutil.NoError(t, err, "store.New() error = %v")
	ctx := context.Background()
	if err := s.CreateModel(ctx, &Model{
		Channel: "WorkBuddy", ModelID: "claude-opus-5", Name: "claude-opus-5",
		Status: ModelStatusAvailable, Verified: true, Origin: "discovery",
	}); err != nil {
		t.Fatalf("CreateModel(discovered) error = %v", err)
	}
	if err := s.CreateModel(ctx, &Model{
		Channel: "Grok", ModelID: "grok-4.6", Name: "Grok 4.6",
		Status: ModelStatusAvailable, Verified: true, Origin: "discovery",
	}); err != nil {
		t.Fatalf("CreateModel(grok discovered) error = %v", err)
	}
	_ = s.Close()

	s, err = New(opts)
	testutil.NoError(t, err, "store.New() second error = %v")
	t.Cleanup(func() { _ = s.Close() })

	for _, probe := range []struct{ channel, modelID string }{
		{"WorkBuddy", "claude-opus-5"},
		{"Grok", "grok-4.6"},
	} {
		model, err := s.GetModelByChannelAndModelID(ctx, probe.channel, probe.modelID)
		testutil.Falsef(t, err != nil || model == nil, "discovered model %s/%s did not survive a restart: %v", probe.channel, probe.modelID, err)
		testutil.Falsef(t, !model.Verified, "%s/%s lost its verified flag", probe.channel, probe.modelID)
	}
}

// Startup preserves observed identifiers without a historical-name blacklist.
func TestStoreNewPreservesObservedModelNames(t *testing.T) {
	t.Parallel()

	mini := miniredis.RunT(t)
	opts := Options{
		RedisAddr:   mini.Addr(),
		RedisDB:     0,
		RedisPrefix: "test:",
	}
	s, err := New(opts)
	testutil.NoError(t, err, "store.New() error = %v")

	ctx := context.Background()
	for _, record := range []*Model{
		{Channel: "Grok", ModelID: "grok-4.5", Name: "Grok 4.5", Status: ModelStatusAvailable, Verified: true, Origin: "discovery"},
		{Channel: "Grok", ModelID: "grok-imagine-image-quality", Name: "Grok Imagine Image Quality", Status: ModelStatusAvailable, Verified: true, Origin: "discovery"},
		{Channel: "Grok", ModelID: "grok-4.3", Name: "legacy model", Status: ModelStatusAvailable, Verified: true},
		{Channel: "Grok", ModelID: "grok-user-custom", Name: "User Custom", Status: ModelStatusAvailable, Verified: true},
	} {
		err := s.CreateModel(ctx, record)
		testutil.CheckNoError(t, err)
	}
	_ = s.Close()

	s, err = New(opts)
	testutil.NoError(t, err, "store.New() second error = %v")
	t.Cleanup(func() { _ = s.Close() })

	for _, id := range []string{"grok-4.5", "grok-imagine-image-quality", "grok-user-custom", "grok-4.3"} {
		_, err := s.GetModelByChannelAndModelID(ctx, "grok", id)
		testutil.CheckNoError(t, err)
	}
}
