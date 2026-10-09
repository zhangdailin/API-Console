package handler

import (
	"net/http"
	"net/http/httptest"
	"orchids-api/internal/testutil"
	"strings"
	"testing"
)

func codexEntryFor(t *testing.T, catalog codexModelCatalog, slug string) codexModelEntry {
	t.Helper()
	for _, entry := range catalog.Models {
		if entry.Slug == slug {
			return entry
		}
	}
	t.Fatalf("catalog is missing %q", slug)
	return codexModelEntry{}
}

func textModel(id string) PublicModelResponse {
	return PublicModelResponse{ID: id, Object: "model", Provider: "build", Capabilities: []string{"chat", "messages", "responses"}}
}

// A client that cannot see the context window falls back to its own default,
// which is what makes a capable model look like an 8k-context one.
func TestCodexCatalogExposesContextWindowAndModalities(t *testing.T) {
	catalog := newCodexModelCatalog([]PublicModelResponse{
		textModel("grok-4.6"),
		textModel("grok-composer-2.5-fast"),
		textModel("grok-unknown-model"),
	})

	for _, tc := range []struct {
		slug       string
		context    int
		modalities []string
	}{
		{"grok-4.6", 500000, []string{"text", "image"}},
		{"grok-composer-2.5-fast", 200000, []string{"text"}},
		{"grok-unknown-model", 128000, []string{"text"}},
	} {
		entry := codexEntryFor(t, catalog, tc.slug)
		testutil.Equal(t, entry.ContextWindow, tc.context)
		testutil.Equal(t, entry.MaxContextWindow, tc.context)
		testutil.Equal(t, len(entry.InputModalities), len(tc.modalities))
		for i, want := range tc.modalities {
			testutil.Equal(t, entry.InputModalities[i], want)
		}
	}
}

func TestCodexCatalogUsesObservedGrokProfile(t *testing.T) {
	supportsReasoning := true
	observed := textModel("grok-4.7")
	observed.ReasoningEfforts = []string{"low", "high", "xhigh"}
	observed.DefaultReasoningEffort = "high"
	observed.SupportsReasoningEffort = &supportsReasoning
	observed.ContextLength = 500000
	observed.MaxInputTokens = 500000
	observed.MaxOutputTokens = 1000000

	entry := codexEntryFor(t, newCodexModelCatalog([]PublicModelResponse{observed}), "grok-4.7")
	levels := make([]string, 0, len(entry.SupportedReasoningLevels))
	for _, level := range entry.SupportedReasoningLevels {
		levels = append(levels, level.Effort)
	}
	testutil.Equal(t, strings.Join(levels, ","), "low,high,xhigh")
	testutil.Equal(t, entry.DefaultReasoningLevel, "high")
	testutil.Equal(t, entry.ContextWindow, 500000)
	testutil.Equal(t, entry.MaxContextWindow, 1500000)
	// The declared output budget travels with the catalog so a client can plan
	// its own completion against the same number the gateway advertises.
	testutil.Equal(t, entry.MaxOutputTokens, 1000000)
}

func TestCodexCatalogReasoningLevels(t *testing.T) {
	catalog := newCodexModelCatalog([]PublicModelResponse{
		textModel("grok-4.6"),
		textModel("grok-4.5"),
	})

	entry := codexEntryFor(t, catalog, "grok-4.6")
	testutil.Equal(t, len(entry.SupportedReasoningLevels), 4)
	testutil.Equal(t, entry.DefaultReasoningLevel, "medium")
	if entry.SupportedReasoningLevels[3].Effort != "xhigh" || entry.SupportedReasoningLevels[3].Description == "" {
		t.Fatalf("grok-4.6 top level = %#v", entry.SupportedReasoningLevels[3])
	}
	// Grok 4.5 tops out at high.
	levels := codexEntryFor(t, catalog, "grok-4.5").SupportedReasoningLevels
	testutil.Falsef(t, len(levels) != 3 || levels[2].Effort != "high", "grok-4.5 levels=%v", levels)
}

func TestCodexCatalogHidesMediaModels(t *testing.T) {
	catalog := newCodexModelCatalog([]PublicModelResponse{
		textModel("grok-4.6"),
		{ID: "grok-imagine-image", Capabilities: []string{"image", "image_edit"}},
		{ID: "grok-imagine-video", Capabilities: []string{"video"}},
	})
	testutil.Equal(t, codexEntryFor(t, catalog, "grok-4.6").Visibility, "list")
	for _, slug := range []string{"grok-imagine-image", "grok-imagine-video"} {
		testutil.Equal(t, codexEntryFor(t, catalog, slug).Visibility, "hide")
	}
	// Agent tooling is only advertised for Responses-capable text models.
	entry := codexEntryFor(t, catalog, "grok-4.6")
	testutil.Falsef(t, entry.ApplyPatchToolType != nil || !entry.SupportsParallelToolCalls, "grok-4.6 tooling = %#v", entry)
	if entry := codexEntryFor(t, catalog, "grok-imagine-image"); entry.ApplyPatchToolType != nil {
		t.Fatalf("media model advertised apply_patch")
	}
}

func TestCodexCatalogJSONIncludesNullableProtocolFields(t *testing.T) {
	catalog := newCodexModelCatalog([]PublicModelResponse{textModel("grok-4.6")})
	rec := httptest.NewRecorder()
	writeCodexModelCatalog(rec, httptest.NewRequest(http.MethodGet, "/v1/models?client_version=1", nil), catalog)
	body := rec.Body.String()
	for _, field := range []string{"default_service_tier", "availability_nux", "upgrade", "model_messages", "auto_compact_token_limit"} {
		testutil.MustContain(t, body, `"`+field+`":null`)
	}
}

func TestWriteCodexModelCatalogServesETag(t *testing.T) {
	catalog := newCodexModelCatalog([]PublicModelResponse{textModel("grok-4.6")})

	first := httptest.NewRecorder()
	writeCodexModelCatalog(first, httptest.NewRequest(http.MethodGet, "/v1/models", nil), catalog)
	testutil.Equal(t, first.Code, http.StatusOK)
	etag := first.Header().Get("ETag")
	testutil.NotEqual(t, etag, "")

	revalidate := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	revalidate.Header.Set("If-None-Match", etag)
	second := httptest.NewRecorder()
	writeCodexModelCatalog(second, revalidate, catalog)
	testutil.Falsef(t, second.Code != http.StatusNotModified || second.Body.Len() != 0, "revalidate status=%d body=%q", second.Code, second.Body.String())
}

// A channel may publish one model per effort level (gpt-5-6-sol-low, -medium,
// ...). The catalog must present that as one reasoning-capable family, otherwise
// a client has to guess a suffix and any family request is rejected.
func TestCodexCatalogGroupsEffortVariantsIntoOneFamily(t *testing.T) {
	catalog := newCodexModelCatalog([]PublicModelResponse{
		textModel("gpt-5-6-sol-low"),
		textModel("gpt-5-6-sol-medium"),
		textModel("gpt-5-6-sol-high"),
		textModel("gpt-5-6-sol-xhigh"),
		textModel("auto-open"),
	})

	testutil.Equal(t, len(catalog.Models), 2)
	family := codexEntryFor(t, catalog, "gpt-5-6-sol")
	levels := make([]string, 0, len(family.SupportedReasoningLevels))
	for _, level := range family.SupportedReasoningLevels {
		levels = append(levels, level.Effort)
	}
	testutil.Equal(t, strings.Join(levels, ","), "low,medium,high,xhigh")
	testutil.Equal(t, family.DefaultReasoningLevel, "medium")
	testutil.Falsef(t, !family.SupportsReasoningSummaries || !family.SupportsReasoningSummaryParameter, "a family with effort levels must advertise reasoning support: %+v", family)

	plain := codexEntryFor(t, catalog, "auto-open")
	// Models without effort variants keep the historical single "none" level.
	testutil.Equal(t, len(plain.SupportedReasoningLevels), 1)
	testutil.Equal(t, plain.SupportedReasoningLevels[0].Effort, "none")
	testutil.Equal(t, plain.DefaultReasoningLevel, "none")
}

func TestSplitEffortVariantSuffix(t *testing.T) {
	cases := map[string][2]string{
		"gpt-5-6-sol-low":        {"gpt-5-6-sol", "low"},
		"gpt-5-3-codex-xhigh":    {"gpt-5-3-codex", "xhigh"},
		"grok-4.6":               {"grok-4.6", ""},
		"auto-open":              {"auto-open", ""},
		"grok-composer-2.5-fast": {"grok-composer-2.5-fast", ""},
	}
	for input, want := range cases {
		family, level := splitEffortVariantSuffix(input)
		testutil.Equal(t, family, want[0])
		testutil.Equal(t, level, want[1])
	}
}

// A lone "<family>-<effort>" id is not a family. Collapsing it would advertise a
// slug the store does not have while hiding the one it does, so a client that
// trusted the catalog would ask for a model that cannot be routed.
func TestCodexCatalogKeepsASingleEffortVariantUnderItsOwnID(t *testing.T) {
	catalog := newCodexModelCatalog([]PublicModelResponse{
		textModel("gpt-5-6-sol-high"),
		textModel("grok-4.6"),
	})

	testutil.Equal(t, len(catalog.Models), 2)
	entry := codexEntryFor(t, catalog, "gpt-5-6-sol-high")
	testutil.Equal(t, entry.DefaultReasoningLevel, "none")
	testutil.Equal(t, len(entry.SupportedReasoningLevels), 1)
	testutil.Equal(t, entry.SupportedReasoningLevels[0].Effort, "none")
}

// The family representative drives visibility, capabilities and metadata. A
// hidden variant must not mask a family that has a visible one.
func TestCodexCatalogPrefersAVisibleVariantsMetadata(t *testing.T) {
	hidden := textModel("gpt-5-6-sol-low")
	hidden.Capabilities = []string{"chat", "messages", "responses", "image"}
	visible := textModel("gpt-5-6-sol-high")

	catalog := newCodexModelCatalog([]PublicModelResponse{
		hidden,
		visible,
	})

	family := codexEntryFor(t, catalog, "gpt-5-6-sol")
	testutil.Equal(t, family.Visibility, "list")
	levels := make([]string, 0, len(family.SupportedReasoningLevels))
	for _, level := range family.SupportedReasoningLevels {
		levels = append(levels, level.Effort)
	}
	testutil.Equal(t, strings.Join(levels, ","), "low,high")
}
