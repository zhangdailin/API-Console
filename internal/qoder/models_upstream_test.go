package qoder

import (
	"encoding/json"
	"orchids-api/internal/testutil"
	"testing"
)

// observedCatalogResponse is the shape the live gateway returned for
// GET /algo/api/v2/model/list: the rows are grouped by capability under `chat`,
// each row is a keyed object, and the display name arrives as `display_name`.
const observedCatalogResponse = `{
  "chat": [
    {"key":"auto","format":"openai","source":"system","enable":true,"display_name":"Auto","is_vl":true,"is_reasoning":false,"is_default":true,"price_factor":1.0,"max_input_tokens":200000},
    {"key":"ultimate","format":"openai","source":"system","enable":true,"display_name":"Ultimate","is_vl":true,"is_reasoning":true,"is_default":false,"price_factor":1.6,"max_input_tokens":1000000},
    {"key":"qmodel_latest","format":"openai","source":"system","enable":true,"display_name":"Qwen3.7-Max","is_vl":false,"is_reasoning":false,"is_default":false,"price_factor":0.4,"max_input_tokens":1000000}
  ]
}`

// TestParseModelListReadsTheObservedGroupShape proves the parser accepts what the
// gateway actually answers. The route was previously assumed unreadable, so this
// pins the shape rather than a guess about it.
func TestParseModelListReadsTheObservedGroupShape(t *testing.T) {
	t.Parallel()

	catalog, err := parseModelList([]byte(observedCatalogResponse))
	testutil.NoError(t, err, "parseModelList() error = %v")
	// `auto` is a routing directive rather than a runnable model, so the two
	// concrete rows are what the catalog carries.
	testutil.Equal(t, catalog.Len(), 2)
	entry, err := catalog.Resolve("Qwen3.7-Max")
	testutil.NoError(t, err, "Resolve(display name) error = %v")
	testutil.Equal(t, entry.Key, "qmodel_latest")
	// The wire fields routing needs must survive the parse.
	testutil.Equal(t, entry.MaxInputTokens, 1000000)
	ultimate, err := catalog.Resolve("Ultimate")
	testutil.NoError(t, err, "Resolve(Ultimate) error = %v")
	testutil.False(t, !ultimate.IsReasoning, "is_reasoning was dropped")
}

func TestParseModelListAcceptsObservedEncoding(t *testing.T) {
	t.Parallel()
	encodedGroup, err := json.Marshal(map[string]string{"chat": `[{"key":"qmodel_latest","display_name":"Qwen3.7-Max"}]`})
	testutil.NoError(t, err)
	encodedData, err := json.Marshal(observedCatalogResponse)
	testutil.NoError(t, err)
	for _, payload := range []string{
		string(encodedGroup),
		`{"data":` + observedCatalogResponse + `}`,
		`{"data":` + string(encodedData) + `}`,
		`{"data":` + string(encodedGroup) + `}`,
	} {
		catalog, err := parseModelList([]byte(payload))
		testutil.NoError(t, err)
		_, err = catalog.Resolve("Qwen3.7-Max")
		testutil.NoError(t, err)
	}
}

func TestParseModelListRejectsUnobservedShapes(t *testing.T) {
	t.Parallel()
	row := `{"key":"qmodel_latest","display_name":"Qwen3.7-Max"}`
	for name, payload := range map[string]string{
		"bare array":          `[` + row + `]`,
		"models":              `{"models":[` + row + `]}`,
		"wrapped models":      `{"data":{"models":[` + row + `]}}`,
		"unknown group":       `{"llm":[` + row + `]}`,
		"completion":          `{"completion":[` + row + `]}`,
		"embedding":           `{"embedding":[` + row + `]}`,
		"nested data":         `{"data":{"data":{"chat":[` + row + `]}}}`,
		"outer string":        `"{\"chat\":[]}"`,
		"failure":             `{"success":false,"message":"catalog denied"}`,
		"unkeyed":             `{"chat":[{"display_name":"model"}]}`,
		"empty chat":          `{"chat":[]}`,
		"null":                `null`,
		"empty":               ``,
		"scalar":              `42`,
		"invalid":             `{`,
		"conflicting wrapper": `{"chat":[` + row + `],"data":{}}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseModelList([]byte(payload))
			testutil.Error(t, err)
		})
	}
}
