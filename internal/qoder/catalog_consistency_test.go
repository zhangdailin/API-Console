package qoder

import "testing"

func TestCatalogMergesDuplicateKeys(t *testing.T) {
	disabled := false
	catalog := newCatalog([]modelEntry{
		{Key: "model", Name: "Disabled", Enable: &disabled, MaxInputTokens: 1},
		{Key: " model ", Name: " Model Name ", DisplayName: " Display Alias ", MaxInputTokens: 180000},
		{Key: "MODEL", Name: "Model Name", MaxInputTokens: 900000},
	})
	if catalog.Len() != 1 {
		t.Fatalf("Len = %d, want 1", catalog.Len())
	}
	for _, name := range []string{"model", "MODEL", " Model Name ", "model name", "Display Alias", "DISPLAY ALIAS"} {
		entry, err := catalog.Resolve(name)
		if err != nil || entry.Key != "model" || entry.MaxInputTokens != 180000 {
			t.Fatalf("Resolve(%q) = %+v, %v", name, entry, err)
		}
	}
	snapshot := CatalogSnapshot(catalog)
	if len(snapshot) != 1 {
		t.Fatalf("snapshot has %d rows", len(snapshot))
	}
	roundtrip := catalogFromIDs(snapshot)
	if got, err := roundtrip.Resolve("Display Alias"); err != nil || got.MaxInputTokens != 180000 {
		t.Fatalf("roundtrip = %+v, %v", got, err)
	}
}

func TestCatalogMetadataMatchesAliasAndKeyResolution(t *testing.T) {
	free, paid := 0.0, 1.0
	catalog := newCatalog([]modelEntry{
		{Key: "small", Name: "Shared", DisplayName: "Display", MaxInputTokens: 180000, PriceFactor: &paid},
		{Key: "large", Name: "SHARED", DisplayName: "DISPLAY", MaxInputTokens: 900000, PriceFactor: &free},
		{Key: "Display", Name: "Exact Key", MaxInputTokens: 32000, PriceFactor: &paid},
	})
	snapshot := CatalogSnapshot(catalog)
	windows := CatalogContextWindows(snapshot)
	for _, tc := range []struct {
		name, key string
		window    int
	}{
		{"Shared", "small", 180000}, {"SHARED", "small", 180000},
		{"large", "large", 900000}, {"Display", "Display", 32000},
		{"DISPLAY", "Display", 32000}, {"display", "Display", 32000},
	} {
		entry, err := catalog.Resolve(tc.name)
		if err != nil || entry.Key != tc.key || entry.MaxInputTokens != tc.window {
			t.Fatalf("Resolve(%q) = %+v, %v", tc.name, entry, err)
		}
	}
	if windows["shared"] != 180000 || windows["display"] != 32000 || windows["large"] != 900000 {
		t.Fatalf("windows = %#v", windows)
	}
	if IsFreeModel(snapshot, "SHARED") || IsFreeModel(snapshot, "display") || !IsFreeModel(snapshot, "large") {
		t.Fatalf("free metadata disagrees with routing: %#v", FreeModelIDs(snapshot))
	}
}

func TestCatalogUnknownAliasWindowDoesNotBorrowAnotherRow(t *testing.T) {
	snapshot := CatalogSnapshot(newCatalog([]modelEntry{
		{Key: "unknown", Name: "Shared"},
		{Key: "known", Name: "Shared", MaxInputTokens: 900000},
	}))
	windows := CatalogContextWindows(snapshot)
	if _, exists := windows["shared"]; exists {
		t.Fatalf("unknown window inherited another model's budget: %#v", windows)
	}
	if windows["known"] != 900000 {
		t.Fatalf("explicit key budget missing: %#v", windows)
	}
}
