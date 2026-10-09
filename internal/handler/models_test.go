package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/modelcatalog"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func TestPublicModelResponseUsesRouteCreatedAt(t *testing.T) {
	created := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	entry := publicModelResponse("grok-4.6", "grok", created)
	testutil.Equal(t, entry.Created, created.Unix())
	// A row stored before the field existed keeps the legacy placeholder.
	legacy := publicModelResponse("grok-4.6", "grok", time.Time{})
	testutil.Equal(t, legacy.Created, legacyModelCreated)
}

func TestAppendGrokReasoningVariantsUsesCaseInsensitiveIndex(t *testing.T) {
	items := []PublicModelResponse{{ID: "GROK-4.6-HIGH", OwnedBy: "grok"}}
	seen := map[string]struct{}{publicModelIDKey(items[0].ID): {}}
	entry := PublicModelResponse{ID: "grok-4.6", OwnedBy: "Grok"}

	items = appendGrokReasoningVariants(items, seen, entry)

	counts := make(map[string]int)
	for _, item := range items {
		counts[publicModelIDKey(item.ID)]++
	}
	testutil.Equal(t, counts["grok-4.6-high"], 1)
	for _, want := range []string{"grok-4.6-low", "grok-4.6-medium", "grok-4.6-xhigh"} {
		testutil.Equal(t, counts[want], 1)
	}
}

// The catalog publishes bare model names: the console/ and build/ qualifiers are
// routing details, and two routes that differ only by plane are the same public
// model. Resolution keeps accepting both spellings.
func TestPublicModelsPublishExternalIDs(t *testing.T) {
	h, s, _ := setupModelValidationHandler(t)
	publishModel(t, s,
		&store.Model{Channel: "Grok", ModelID: "grok-4.3"},
		&store.Model{Channel: "Grok", ModelID: "grok-4.5"},
		&store.Model{Channel: "Grok", ModelID: "grok-4.6"},
	)

	rec := httptest.NewRecorder()
	h.HandleModels(rec, httptest.NewRequest(http.MethodGet, "http://example.com/grok/v1/models", nil))
	testutil.Equal(t, rec.Code, http.StatusOK)
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	testutil.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload), "decode: %v")
	seen := map[string]int{}
	for _, entry := range payload.Data {
		seen[entry.ID]++
	}
	for _, want := range []string{"grok-4.3", "grok-4.5", "grok-4.6"} {
		testutil.NotEqual(t, seen[want], 0)
	}
	for id := range seen {
		testutil.MustNotContain(t, id, "/")
	}
	testutil.Equal(t, seen["grok-4.6"], 1)
}

func TestHandleModelsPublishesConservativeEnabledBuildProfile(t *testing.T) {
	h, s, _ := setupModelValidationHandler(t)
	publishModel(t, s, &store.Model{Channel: "Grok", ModelID: "grok-4.6", Provider: "build", UpstreamModel: "grok-4.6"})

	ctx := context.Background()
	for _, acc := range []*store.Account{
		{AccountType: "grok", Enabled: true, CredentialType: "oauth", GrokProvider: "build", OAuthAccessToken: "one", GrokModelCatalog: []modelcatalog.Profile{{ModelID: "grok-4.6", ReasoningEfforts: []string{"low", "high", "xhigh"}, DefaultReasoningEffort: "high", SupportsReasoningEffort: true, SupportsBackendSearch: true, ContextWindow: 500000, MaxCompletionTokens: 100000}}},
		{AccountType: "grok", Enabled: true, CredentialType: "oauth", GrokProvider: "build", OAuthAccessToken: "two", GrokModelCatalog: []modelcatalog.Profile{{ModelID: "GROK-4.6", ReasoningEfforts: []string{"low", "high"}, DefaultReasoningEffort: "high", SupportsReasoningEffort: true, SupportsBackendSearch: false, ContextWindow: 256000, MaxCompletionTokens: 64000}}},
		{AccountType: "grok", Enabled: false, CredentialType: "oauth", GrokProvider: "build", OAuthAccessToken: "disabled", GrokModelCatalog: []modelcatalog.Profile{{ModelID: "grok-4.6", ReasoningEfforts: []string{"none"}, ContextWindow: 1, MaxCompletionTokens: 1}}},
	} {
		testutil.NoError(t, s.CreateAccount(ctx, acc), "CreateAccount: %v")
	}

	rec := httptest.NewRecorder()
	h.HandleModels(rec, httptest.NewRequest(http.MethodGet, "http://example.com/grok/v1/models", nil))
	testutil.Equal(t, rec.Code, http.StatusOK)
	var payload PublicModelsListResponse
	testutil.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload), "decode: %v")
	var got *PublicModelResponse
	for i := range payload.Data {
		if payload.Data[i].ID == "grok-4.6" {
			got = &payload.Data[i]
			break
		}
	}
	testutil.Falsef(t, got == nil, "grok-4.6 missing: %+v", payload.Data)
	testutil.Equal(t, strings.Join(got.ReasoningEfforts, ","), "low,high")
	testutil.Equal(t, got.DefaultReasoningEffort, "high")
	if got.SupportsReasoningEffort == nil || !*got.SupportsReasoningEffort || got.SupportsBackendSearch == nil || *got.SupportsBackendSearch {
		t.Fatalf("feature metadata=%+v", *got)
	}
	if got.ContextLength != 256000 || got.MaxInputTokens != 256000 || got.MaxOutputTokens != 64000 {
		t.Fatalf("dynamic budgets=%+v", *got)
	}
}

// TestAppendGrokReasoningVariantsRespectsThePlane keeps the advertised alias
// set equal to the set the resolver accepts. The entry carries the bare public
// name, so the plane has to come from the row: a Build model that refuses an
// effort parameter must not publish <name>-<effort> aliases that every request
// then rejects as model_not_found.
