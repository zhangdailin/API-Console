package qoder

import (
	"testing"

	"encoding/json"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

// Extracted catalog-only fields from the authorized reference probe. No account
// identity or credentials are retained. Labels and numeric counts are observed,
// not inferred from the 1M/200K spelling.
const observedContextCatalog = `{"chat":[
 {"key":"ultimate","display_name":"Ultimate","max_input_tokens":1000000,"context_config":{"1M":{"token_count":1000000},"200K":{"token_count":200000,"is_default":true},"400K":{"token_count":400000}}},
 {"key":"performance","display_name":"Performance","max_input_tokens":1000000,"context_config":{"272K":{"token_count":272000,"is_default":true},"1M":{"token_count":1000000},"400K":{"token_count":400000}}},
 {"key":"qmodel_38max","display_name":"Qwen3.8-Max","max_input_tokens":180000,"context_config":{"1M":{"token_count":1000000},"200K":{"token_count":200000,"is_default":true},"400K":{"token_count":400000}}}
]}`

func TestObservedContextTiersAndRequestDefault(t *testing.T) {
	c, err := parseModelList([]byte(observedContextCatalog))
	testutil.NoError(t, err)
	c = catalogFromIDs(catalogToIDs(c))
	for _, tc := range []struct {
		key        string
		input, def int
	}{{"ultimate", 1000000, 200000}, {"performance", 1000000, 272000}, {"qmodel_38max", 180000, 200000}} {
		model, err := c.Resolve(tc.key)
		testutil.NoError(t, err)
		info := model.ContextWindowInfo()
		testutil.Falsef(t, info.DefaultInputTokens != tc.input || info.DefaultContextTokens != tc.def || info.MaxContextTokens != 1000000 || info.HasUnparsedConfig || info.DefaultConflict, "%s: %+v", tc.key, info)
		encoded, err := buildChatBodyProfile(upstream.UpstreamRequest{Prompt: "hello"}, model, "session", "request", "request-set", DefaultClientVersion, "", sceneBusinessProduct)
		testutil.NoError(t, err)
		raw, err := decodeBody(encoded)
		testutil.NoError(t, err)
		var body chatBody
		testutil.NoError(t, json.Unmarshal(raw, &body))
		parameters := body.Parameters.(map[string]any)
		testutil.EqualAny(t, parameters["context_length"], float64(tc.def))
		testutil.Equal(t, body.ModelConfig.MaxInputTokens, tc.input)
	}
}

func TestObservedContextTierDuplicateMerge(t *testing.T) {
	c, err := parseModelList([]byte(`{"chat":[{"key":"k","max_input_tokens":180000,"context_config":{"default-label":{"token_count":200000,"is_default":true}}},{"key":"k","max_input_tokens":900000,"context_config":{"extended-label":{"token_count":1000000}}}]}`))
	testutil.NoError(t, err)
	model, _ := catalogFromIDs(catalogToIDs(c)).Resolve("k")
	info := model.ContextWindowInfo()
	testutil.Falsef(t, c.Len() != 1 || info.DefaultInputTokens != 180000 || info.DefaultContextTokens != 200000 || info.MaxContextTokens != 1000000, "merged=%+v", info)
}

// The published window is the context a request is served with, so a client
// compacts before the upstream refuses or cuts its input: ultimate is served
// at its 200K default tier, not its 1M max_input_tokens.
func TestPublishedWindowMatchesServedContext(t *testing.T) {
	c, err := parseModelList([]byte(observedContextCatalog))
	testutil.NoError(t, err)
	ids := catalogToIDs(c)
	windows := CatalogContextWindows(ids)
	for key, want := range map[string]int{"ultimate": 200000, "performance": 272000, "qmodel_38max": 180000} {
		testutil.Falsef(t, windows[key] != want, "%s published %d, want %d", key, windows[key], want)
		model, err := catalogFromIDs(ids).Resolve(key)
		testutil.NoError(t, err)
		body := decodeRegressionBody(t, upstream.UpstreamRequest{Prompt: "hi"}, model)
		served := int(body.Parameters.(map[string]any)["context_length"].(float64))
		testutil.Falsef(t, windows[key] > served, "%s published %d above the served context %d", key, windows[key], served)
	}
	// No output budget is published unless the catalog declares one.
	testutil.Falsef(t, CatalogOutputWindows(ids) != nil, "output windows invented: %v", CatalogOutputWindows(ids))
	declared := catalogToIDs(newCatalog([]modelEntry{{Key: "k", MaxOutputTokens: json.RawMessage(`"16384"`)}}))
	testutil.Equal(t, CatalogOutputWindows(declared)["k"], 16384)
}
