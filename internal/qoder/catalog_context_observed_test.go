package qoder

import (
	"testing"

	"github.com/goccy/go-json"
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
	if err != nil {
		t.Fatal(err)
	}
	c = catalogFromIDs(catalogToIDs(c))
	for _, tc := range []struct {
		key        string
		input, def int
	}{{"ultimate", 1000000, 200000}, {"performance", 1000000, 272000}, {"qmodel_38max", 180000, 200000}} {
		model, err := c.Resolve(tc.key)
		if err != nil {
			t.Fatal(err)
		}
		info := model.ContextWindowInfo()
		if info.DefaultInputTokens != tc.input || info.DefaultContextTokens != tc.def || info.MaxContextTokens != 1000000 || info.HasUnparsedConfig || info.DefaultConflict {
			t.Fatalf("%s: %+v", tc.key, info)
		}
		encoded, err := buildChatBody(upstream.UpstreamRequest{Prompt: "hello"}, model, "session", "request")
		if err != nil {
			t.Fatal(err)
		}
		raw, err := decodeBody(encoded)
		if err != nil {
			t.Fatal(err)
		}
		var body chatBody
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		if body.Parameters["context_length"] != float64(tc.def) {
			t.Fatalf("%s context_length=%v", tc.key, body.Parameters["context_length"])
		}
		if body.ModelConfig.MaxInputTokens != tc.input {
			t.Fatalf("%s input budget overwritten: %d", tc.key, body.ModelConfig.MaxInputTokens)
		}
	}
}

func TestObservedContextTierDuplicateMerge(t *testing.T) {
	c, err := parseModelList([]byte(`[{"key":"k","max_input_tokens":180000,"context_config":{"default-label":{"token_count":200000,"is_default":true}}},{"key":"k","max_input_tokens":900000,"context_config":{"extended-label":{"token_count":1000000}}}]`))
	if err != nil {
		t.Fatal(err)
	}
	model, _ := catalogFromIDs(catalogToIDs(c)).Resolve("k")
	info := model.ContextWindowInfo()
	if c.Len() != 1 || info.DefaultInputTokens != 180000 || info.DefaultContextTokens != 200000 || info.MaxContextTokens != 1000000 {
		t.Fatalf("merged=%+v", info)
	}
}
