package main

import (
	"context"
	"testing"

	"orchids-api/internal/cline"
	"orchids-api/internal/config"
	"orchids-api/internal/store"
)

// TestClineCatalogToDiscoveredPublishesTheFeedName pins the display name the
// feed published.
//
// The identifier is what a client asks for; the name is what a human reads.
// Publishing the identifier in both columns made the whole Cline page a list of
// slugs, which is what the upstream's own catalog is not.
func TestClineCatalogToDiscoveredPublishesTheFeedName(t *testing.T) {
	got := clineCatalogToDiscovered([]cline.Model{
		{ID: "cline-free/deepseek-v4.1-flash", Name: "Deepseek-v4.1-Flash", Provider: "cline-free"},
		{ID: "z-ai/glm-5.3-flash", Name: "glm-5.3-flash", Provider: "z-ai"},
		// A feed row with no name falls back to the identifier rather than
		// publishing an empty display name.
		{ID: "poolside/laguna-s-2.1:free", Provider: "poolside"},
		{ID: "   "},
	})
	if len(got) != 3 {
		t.Fatalf("discovered=%d want 3: %#v", len(got), got)
	}
	want := []struct {
		id, name, provider string
	}{
		{"cline-free/deepseek-v4.1-flash", "Deepseek-v4.1-Flash", "cline-free"},
		{"z-ai/glm-5.3-flash", "glm-5.3-flash", "z-ai"},
		{"poolside/laguna-s-2.1:free", "poolside/laguna-s-2.1:free", "poolside"},
	}
	for i, want := range want {
		if got[i].ID != want.id || got[i].Name != want.name || got[i].Provider != want.provider {
			t.Errorf("row %d = {%q %q %q}, want {%q %q %q}",
				i, got[i].ID, got[i].Name, got[i].Provider, want.id, want.name, want.provider)
		}
		// The upstream identifier is what the request path sends, so it is the
		// upstream model of every published row.
		if got[i].UpstreamModel != want.id {
			t.Errorf("row %d UpstreamModel=%q want %q", i, got[i].UpstreamModel, want.id)
		}
		if !got[i].Verified {
			t.Errorf("row %d Verified=false: a catalog read is an observation", i)
		}
	}
}

// TestApplyModelRefreshWritesTheClineRouteMetadata checks that a refresh
// publishes the vendor the feed named, so the public model list can tell two
// free models of the same channel apart.
func TestApplyModelRefreshWritesTheClineRouteMetadata(t *testing.T) {
	s, cleanup := setupModelRefreshStore(t)
	defer cleanup()
	ctx := context.Background()
	clearModelsForChannel(t, ctx, s, "Cline")

	result, err := applyModelRefreshWithPrune(ctx, s, "Cline", "cline_recommended_models", clineCatalogToDiscovered([]cline.Model{
		{ID: "cline-free/deepseek-v4.1-flash", Name: "Deepseek-v4.1-Flash", Provider: "cline-free"},
		{ID: "z-ai/glm-5.3-flash", Name: "glm-5.3-flash", Provider: "z-ai"},
	}), true)
	if err != nil {
		t.Fatalf("applyModelRefreshWithPrune() error = %v", err)
	}
	if result.Added != 2 {
		t.Fatalf("Added=%d want 2", result.Added)
	}

	row, err := s.GetModelByChannelAndModelID(ctx, "Cline", "z-ai/glm-5.3-flash")
	if err != nil {
		t.Fatalf("GetModelByChannelAndModelID() error = %v", err)
	}
	if row.Name != "glm-5.3-flash" {
		t.Errorf("Name=%q want the feed's display name", row.Name)
	}
	if row.Provider != "z-ai" {
		t.Errorf("Provider=%q want z-ai", row.Provider)
	}
	if row.UpstreamModel != "z-ai/glm-5.3-flash" {
		t.Errorf("UpstreamModel=%q want the upstream id", row.UpstreamModel)
	}
}

// TestApplyModelRefreshFillsInARowThatPredatesTheMetadata is the migration half:
// the five Cline rows this deployment already has were published before the feed
// named a provider, and a refresh has to complete them without touching a name
// or a provider an operator set by hand.
func TestApplyModelRefreshFillsInARowThatPredatesTheMetadata(t *testing.T) {
	s, cleanup := setupModelRefreshStore(t)
	defer cleanup()
	ctx := context.Background()
	clearModelsForChannel(t, ctx, s, "Cline")

	stored := &store.Model{
		Channel:  "Cline",
		ModelID:  "cline-free/deepseek-v4.1-flash",
		Name:     "cline-free/deepseek-v4.1-flash",
		Status:   store.ModelStatusAvailable,
		Verified: false,
	}
	if err := s.CreateModel(ctx, stored); err != nil {
		t.Fatalf("CreateModel() error = %v", err)
	}
	// A row an operator renamed must keep its name.
	renamed := &store.Model{
		Channel:  "Cline",
		ModelID:  "z-ai/glm-5.3-flash",
		Name:     "运营改过的名字",
		Status:   store.ModelStatusAvailable,
		Verified: true,
		Provider: "hand-set",
	}
	if err := s.CreateModel(ctx, renamed); err != nil {
		t.Fatalf("CreateModel() error = %v", err)
	}

	if _, err := applyModelRefreshWithPrune(ctx, s, "Cline", "cline_recommended_models", clineCatalogToDiscovered([]cline.Model{
		{ID: "cline-free/deepseek-v4.1-flash", Name: "Deepseek-v4.1-Flash", Provider: "cline-free"},
		{ID: "z-ai/glm-5.3-flash", Name: "glm-5.3-flash", Provider: "z-ai"},
	}), true); err != nil {
		t.Fatalf("applyModelRefreshWithPrune() error = %v", err)
	}

	completed, err := s.GetModelByChannelAndModelID(ctx, "Cline", "cline-free/deepseek-v4.1-flash")
	if err != nil {
		t.Fatalf("GetModelByChannelAndModelID() error = %v", err)
	}
	if !completed.Verified {
		t.Error("an observed row stayed unverified")
	}
	// The name was a copy of the identifier, so it adopts the feed's name.
	if completed.Name != "Deepseek-v4.1-Flash" {
		t.Errorf("Name=%q want Deepseek-v4.1-Flash", completed.Name)
	}
	if completed.Provider != "cline-free" {
		t.Errorf("Provider=%q want cline-free", completed.Provider)
	}

	kept, err := s.GetModelByChannelAndModelID(ctx, "Cline", "z-ai/glm-5.3-flash")
	if err != nil {
		t.Fatalf("GetModelByChannelAndModelID() error = %v", err)
	}
	if kept.Name != "运营改过的名字" {
		t.Errorf("Name=%q want the operator's name preserved", kept.Name)
	}
	if kept.Provider != "hand-set" {
		t.Errorf("Provider=%q want the operator's provider preserved", kept.Provider)
	}
}

// TestDiscoverClineModelsReportsNoAccount keeps the channel's contract: without
// an account there is no observation, and no compiled-in list may stand in.
func TestDiscoverClineModelsReportsNoAccount(t *testing.T) {
	s, cleanup := setupModelRefreshStore(t)
	defer cleanup()

	_, err := discoverAccountCatalogModels(context.Background(), &config.Config{}, s, "Cline", defaultModelRefreshConcurrency)
	if err == nil {
		t.Fatal("discoverAccountCatalogModels() error = nil, want no active accounts")
	}
	var noAccount *noActiveAccountsError
	if !errorsAs(err, &noAccount) {
		t.Fatalf("error = %v, want noActiveAccountsError", err)
	}
}

func errorsAs(err error, target **noActiveAccountsError) bool {
	for err != nil {
		if typed, ok := err.(*noActiveAccountsError); ok {
			*target = typed
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		next := unwrapper.Unwrap()
		if next == err {
			return false
		}
		err = next
	}
	return false
}

// TestClineRefreshSourceIsAnUpstreamCatalog keeps the source in the set that may
// publish and prune: a refresh that reads the feed is authoritative for the
// whole channel.
func TestClineRefreshSourceIsAnUpstreamCatalog(t *testing.T) {
	if !isUpstreamCatalogSource("cline_recommended_models") {
		t.Fatal("cline_recommended_models must be an upstream catalog source")
	}
	if normalizeAdminModelChannel("cline") != "Cline" {
		t.Fatalf("normalizeAdminModelChannel(cline) = %q", normalizeAdminModelChannel("cline"))
	}
	if got := refreshModelRequestConfig(&config.Config{RequestTimeout: 600}, "cline").RequestTimeout; got != 15 {
		t.Errorf("refresh timeout = %d, want it bounded to 15s for a catalog read", got)
	}
}
