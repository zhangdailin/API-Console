package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

// publicListEntry is the shape a client reads. The window fields are the point
// of these tests: a client that cannot see them budgets against its own default.
type publicListEntry struct {
	ID              string `json:"id"`
	ContextLength   int    `json:"context_length"`
	MaxInputTokens  int    `json:"max_input_tokens"`
	MaxOutputTokens int    `json:"max_output_tokens"`
}

func fetchPublicModels(t *testing.T, h *Handler, path string) map[string]publicListEntry {
	t.Helper()
	rec := httptest.NewRecorder()
	h.HandleModels(rec, httptest.NewRequest(http.MethodGet, "http://example.com"+path, nil))
	testutil.Equal(t, rec.Code, http.StatusOK)
	var payload struct {
		Data []publicListEntry `json:"data"`
	}
	testutil.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload), "decode: %v")
	out := make(map[string]publicListEntry, len(payload.Data))
	for _, entry := range payload.Data {
		out[entry.ID] = entry
	}
	return out
}

// The Qoder catalog declares max_input_tokens per model and the request path
// already forwards it. The model list has to publish the same number, otherwise
// a 1M-token model reads as the client's 262144 default.
func TestPublicModelsPublishQoderContextWindow(t *testing.T) {
	h, s, _ := setupModelValidationHandler(t)

	acc := createEnabledTestAccount(t, s, "qoder-1", "qoder")
	acc.QoderModelIDs = []string{
		`{"key":"ultimate","name":"Ultimate","display_name":"Ultimate","max_input_tokens":1000000}`,
		`{"key":"qfmodel","name":"Qwen3.8-Flash","display_name":"Qwen3.8-Flash","max_input_tokens":180000}`,
	}
	testutil.NoError(t, s.UpdateAccount(context.Background(), acc), "UpdateAccount() error = %v")

	publishModel(t, s,
		&store.Model{Channel: "qoder", ModelID: "ultimate"},
		&store.Model{Channel: "qoder", ModelID: "qwen3.8-flash"},
	)

	entries := fetchPublicModels(t, h, "/qoder/v1/models")
	testutil.Equal(t, entries["ultimate"].ContextLength, 1000000)
	testutil.Equal(t, entries["ultimate"].MaxInputTokens, 1000000)
	testutil.Equal(t, entries["qwen3.8-flash"].ContextLength, 180000)
}

// A window that was never observed must be absent, not zero: a client that reads
// zero would treat the model as unable to hold anything.
func TestPublicModelsOmitUnobservedContextWindow(t *testing.T) {
	h, s, _ := setupModelValidationHandler(t)

	publishModel(t, s, &store.Model{Channel: "cline", ModelID: "gpt-5-nano"})

	entries := fetchPublicModels(t, h, "/cline/v1/models")
	entry, ok := entries["gpt-5-nano"]
	testutil.True(t, ok, "model missing from the list: %#v")
	testutil.Equal(t, entry.ContextLength, 0)
	testutil.Equal(t, entry.MaxInputTokens, 0)

	// Re-encode to prove the field is absent rather than present-and-zero.
	rec := httptest.NewRecorder()
	h.HandleModels(rec, httptest.NewRequest(http.MethodGet, "http://example.com/cline/v1/models", nil))
	body := rec.Body.String()
	testutil.Falsef(t, containsJSONField(body, "context_length"), "context_length must not be serialized when unobserved: %s", body)
}

func TestPublicModelsPublishWorkBuddyContextWindow(t *testing.T) {
	h, s, _ := setupModelValidationHandler(t)

	acc := createEnabledTestAccount(t, s, "wb-1", "workbuddy")
	acc.WorkBuddyModelIDs = []string{
		`{"id":"wb-model","name":"WB Model","max_input_tokens":256000,"max_output_tokens":32000}`,
		// A bare id written by an older build still has to resolve, it just has
		// no window to report.
		"legacy-model",
	}
	testutil.NoError(t, s.UpdateAccount(context.Background(), acc), "UpdateAccount() error = %v")

	publishModel(t, s,
		&store.Model{Channel: "workbuddy", ModelID: "wb-model"},
		&store.Model{Channel: "workbuddy", ModelID: "legacy-model"},
	)

	entries := fetchPublicModels(t, h, "/workbuddy/v1/models")
	testutil.Equal(t, entries["wb-model"].ContextLength, 256000)
	testutil.Equal(t, entries["wb-model"].MaxOutputTokens, 32000)
	testutil.Equal(t, entries["legacy-model"].ContextLength, 0)
}

// The Grok window is not account-scoped; it comes from the same table the Codex
// catalog publishes, so both surfaces agree.
func TestPublicModelsPublishGrokContextWindow(t *testing.T) {
	h, s, _ := setupModelValidationHandler(t)

	publishModel(t, s, &store.Model{Channel: "grok", ModelID: "grok-4.6", Verified: true})

	entries := fetchPublicModels(t, h, "/grok/v1/models")
	testutil.Equal(t, entries["grok-4.6"].ContextLength, 500000)
}

// The Codex catalog used to fall back to a Grok-shaped 128k default for every
// non-Grok model, which reported a 1M-token model as eight times smaller. An
// observed window must win over that default.
func TestCodexCatalogPrefersObservedContextWindow(t *testing.T) {
	observed := textModel("qwen3.8-max")
	observed.ContextLength = 1000000
	unobserved := textModel("qwen3.8-flash")

	catalog := newCodexModelCatalog([]PublicModelResponse{observed, unobserved})

	testutil.Equal(t, codexEntryFor(t, catalog, "qwen3.8-max").ContextWindow, 1000000)
	// Nothing was observed for this one, so the historical default still applies
	// and no wrong number is invented.
	testutil.Equal(t, codexEntryFor(t, catalog, "qwen3.8-flash").ContextWindow, 128000)
}

func containsJSONField(body, field string) bool {
	return len(body) > 0 && json.Valid([]byte(body)) && jsonContainsKey([]byte(body), field)
}

func jsonContainsKey(body []byte, field string) bool {
	var decoded map[string]interface{}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return false
	}
	return deepHasKey(decoded, field)
}

func deepHasKey(value interface{}, field string) bool {
	switch typed := value.(type) {
	case map[string]interface{}:
		if _, ok := typed[field]; ok {
			return true
		}
		for _, item := range typed {
			if deepHasKey(item, field) {
				return true
			}
		}
	case []interface{}:
		for _, item := range typed {
			if deepHasKey(item, field) {
				return true
			}
		}
	}
	return false
}
