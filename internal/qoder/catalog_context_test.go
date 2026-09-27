package qoder

import (
	"github.com/goccy/go-json"
	"testing"
)

// Synthetic fixtures exercise the conservative schema, not captured wire data.
func TestCatalogContextConfigMergeAndRoundTrip(t *testing.T) {
	raw := []byte(`{"data":{"chat":[
 {"key":"k","display_name":"Model","max_input_tokens":180000,"context_config":[{"context_length":200000,"is_default":true,"opaque":{"price":1}}]},
 {"key":"k","display_name":"Model","max_input_tokens":900000,"context_config":{"context_length":1000000,"is_default":false,"future":"keep"}},
 {"key":"k","context_config":{"context_length":1000000,"is_default":false,"future":"keep"}},
 {"key":"k","enable":false,"context_config":{"context_length":2000000,"is_default":true}}
 ]}}`)
	catalog, err := parseModelList(raw)
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Len() != 1 {
		t.Fatalf("len=%d", catalog.Len())
	}
	for _, c := range []*Catalog{catalog, catalogFromIDs(catalogToIDs(catalog))} {
		for _, name := range []string{"k", "Model"} {
			model, err := c.Resolve(name)
			if err != nil {
				t.Fatal(err)
			}
			if model.MaxInputTokens != 180000 || len(model.ContextConfigVariants) != 1 {
				t.Fatalf("row=%+v", model)
			}
			info := model.ContextWindowInfo()
			if info.DefaultInputTokens != 180000 || info.DefaultContextTokens != 200000 || info.MaxContextTokens != 1000000 || info.DefaultConflict || info.HasUnparsedConfig {
				t.Fatalf("info=%+v", info)
			}
			var tiers []map[string]json.RawMessage
			if err := json.Unmarshal(model.ContextConfig, &tiers); err != nil || string(tiers[0]["opaque"]) != "{\"price\":1}" {
				t.Fatalf("lost raw config: %s", model.ContextConfig)
			}
		}
	}
	snapshot := catalogToIDs(catalog)
	if CatalogContextWindows(snapshot)["model"] != 180000 {
		t.Fatal("input budget replaced with total tier")
	}
	if CatalogContextWindowDetails(snapshot)["model"].MaxContextTokens != 1000000 {
		t.Fatal("missing separate largest tier")
	}
	if len(catalogFromIDs(snapshot).entries[0].ContextConfigVariants) != 1 {
		t.Fatal("roundtrip grew variants")
	}
}

func TestCatalogContextUnknownAndConflictingDefaults(t *testing.T) {
	for _, tc := range []struct {
		config string
		want   ContextWindowInfo
	}{
		{`{"tiers":{"200K":{"is_default":true}},"future":7}`, ContextWindowInfo{DefaultInputTokens: 180000, HasUnparsedConfig: true}},
		{`[{"context_length":200000,"is_default":true},{"context_length":400000,"is_default":true}]`, ContextWindowInfo{DefaultInputTokens: 180000, MaxContextTokens: 400000, DefaultConflict: true}},
		{`[{"context_length":400000},{"context_length":"1M"}]`, ContextWindowInfo{DefaultInputTokens: 180000, MaxContextTokens: 400000, HasUnparsedConfig: true}},
		{`null`, ContextWindowInfo{DefaultInputTokens: 180000}},
	} {
		c := newCatalog([]modelEntry{{Key: "k", MaxInputTokens: 180000, ContextConfig: json.RawMessage(tc.config)}})
		restored := catalogFromIDs(catalogToIDs(c))
		m, _ := restored.Resolve("k")
		if compactContextConfig(m.ContextConfig) != tc.config {
			t.Fatalf("opaque lost: %s", m.ContextConfig)
		}
		if got := m.ContextWindowInfo(); got != tc.want {
			t.Fatalf("%s got=%+v want=%+v", tc.config, got, tc.want)
		}
	}
}

func TestCatalogContextConfigAbsentCanonicalAndSameName(t *testing.T) {
	c := newCatalog([]modelEntry{
		{Key: "a", Name: "Shared", MaxInputTokens: 180000},
		{Key: "a", Name: "Shared", ContextConfig: json.RawMessage(`{"context_length":200000,"is_default":true}`)},
		{Key: "b", Name: "Shared", MaxInputTokens: 900000, ContextConfig: json.RawMessage(`{"context_length":1000000,"is_default":true}`)},
	})
	details := CatalogContextWindowDetails(catalogToIDs(c))
	if details["shared"].DefaultContextTokens != 200000 || details["b"].DefaultContextTokens != 1000000 {
		t.Fatalf("details=%+v", details)
	}
}
