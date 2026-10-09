package workbuddy

import (
	"orchids-api/internal/testutil"
	"testing"
)

// The snapshot used to be a bare list of ids, which preserved the whitelist but
// discarded the windows it was observed alongside. A client that budgets its
// context then had nothing to read.
func TestCatalogSnapshotRoundTripsWindows(t *testing.T) {
	models := []WorkBuddyModel{
		{ID: "wb-model", Name: "WB Model", MaxInputTokens: 256000, MaxOutputTokens: 32000},
		{ID: "wb-small", Name: "WB Small", MaxInputTokens: 128000},
	}
	rows := CatalogSnapshot(models)
	testutil.Equal(t, len(rows), 2)

	input, output := CatalogContextWindows(rows)
	testutil.Equal(t, input["wb-model"], 256000)
	testutil.Equal(t, output["wb-model"], 32000)
	testutil.Equal(t, input["wb-small"], 128000)
	testutil.Equal(t, output["wb-small"], 0)
}

// Retired bare IDs do not contribute catalog information.
func TestCatalogContextWindowsIgnoresRetiredBareIDs(t *testing.T) {
	input, output := CatalogContextWindows([]string{"legacy-model", `{"id":"new-model","max_input_tokens":1000000}`})
	_, ok := input["legacy-model"]
	testutil.Falsef(t, ok, "a bare id must not invent a window: %#v", input)
	testutil.Equal(t, input["new-model"], 1000000)
	testutil.Falsef(t, output != nil, "output budgets = %#v, want nil when none were declared", output)
}

func TestCatalogContextWindowsIgnoresMalformedRows(t *testing.T) {
	input, _ := CatalogContextWindows([]string{"{not json", "", "   "})
	testutil.Falsef(t, input != nil, "input windows = %#v, want nil", input)
}
