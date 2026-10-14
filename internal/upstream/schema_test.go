package upstream

import (
	"encoding/json"
	"testing"

	"orchids-api/internal/prompt"
	"orchids-api/internal/testutil"
)

func compile(t *testing.T, raw string) *Schema {
	t.Helper()
	var value any
	testutil.NoError(t, json.Unmarshal([]byte(raw), &value), "unmarshal schema: %v")
	schema := CompileSchema(value)
	testutil.False(t, schema == nil, "schema did not compile")
	return schema
}

func decodeValue(t *testing.T, raw string) any {
	t.Helper()
	var value any
	testutil.NoError(t, json.Unmarshal([]byte(raw), &value), "unmarshal value: %v")
	return value
}

func TestCompileSchemaRejectsUnreadableInput(t *testing.T) {
	testutil.Equal(t, CompileSchema(nil) == nil, true)
	testutil.Equal(t, CompileSchema("not a schema") == nil, true)
	testutil.Equal(t, CompileSchema(map[string]any{}) == nil, true)
}

func TestSchemaValidateObject(t *testing.T) {
	schema := compile(t, `{
		"type": "object",
		"properties": {
			"name": {"type": "string"},
			"age": {"type": "integer"},
			"tags": {"type": "array", "items": {"type": "string"}}
		},
		"required": ["name", "age"],
		"additionalProperties": false
	}`)

	ok := `{"name":"ada","age":36,"tags":["math"]}`
	testutil.NoError(t, schema.Validate(decodeValue(t, ok)), "valid object rejected: %v")

	// A missing required property is the failure a client's parser would hit.
	err := schema.Validate(decodeValue(t, `{"name":"ada","tags":[]}`))
	testutil.False(t, err == nil, "missing required property accepted")
	testutil.MustContain(t, err.Error(), "missing required property")

	// A property the schema does not name is rejected only when it says so.
	err = schema.Validate(decodeValue(t, `{"name":"ada","age":36,"extra":1}`))
	testutil.False(t, err == nil, "undeclared property accepted")
	testutil.MustContain(t, err.Error(), "is not allowed by the schema")

	// A wrong type inside a nested array item is reported with its path.
	err = schema.Validate(decodeValue(t, `{"name":"ada","age":36,"tags":[1]}`))
	testutil.False(t, err == nil, "wrong item type accepted")
	testutil.MustContain(t, err.Error(), "tags[0]")
}

func TestSchemaValidateOptionalViaNullUnion(t *testing.T) {
	// A strict schema declares an optional member by admitting null, not by
	// leaving it out of `required`.
	schema := compile(t, `{
		"type": "object",
		"properties": {"note": {"type": ["string", "null"]}},
		"required": ["note"]
	}`)
	testutil.NoError(t, schema.Validate(decodeValue(t, `{"note":null}`)), "null rejected: %v")
	testutil.NoError(t, schema.Validate(decodeValue(t, `{"note":"hi"}`)), "string rejected: %v")
	err := schema.Validate(decodeValue(t, `{"note":7}`))
	testutil.False(t, err == nil, "number accepted where a string or null was required")
}

func TestSchemaValidateEnumConstAndBounds(t *testing.T) {
	schema := compile(t, `{
		"type": "object",
		"properties": {
			"kind": {"type": "string", "enum": ["a", "b"]},
			"version": {"type": "number", "minimum": 1, "maximum": 3},
			"marker": {"type": "string", "const": "fixed"}
		},
		"required": ["kind", "version", "marker"]
	}`)
	testutil.NoError(t, schema.Validate(decodeValue(t, `{"kind":"a","version":2,"marker":"fixed"}`)), "valid rejected: %v")
	testutil.False(t, schema.Validate(decodeValue(t, `{"kind":"c","version":2,"marker":"fixed"}`)) == nil, "enum violation accepted")
	testutil.False(t, schema.Validate(decodeValue(t, `{"kind":"a","version":9,"marker":"fixed"}`)) == nil, "maximum violation accepted")
	testutil.False(t, schema.Validate(decodeValue(t, `{"kind":"a","version":0,"marker":"fixed"}`)) == nil, "minimum violation accepted")
	testutil.False(t, schema.Validate(decodeValue(t, `{"kind":"a","version":2,"marker":"other"}`)) == nil, "const violation accepted")
}

func TestSchemaValidateRefAndFormats(t *testing.T) {
	schema := compile(t, `{
		"type": "object",
		"properties": {
			"child": {"$ref": "#/$defs/Child"},
			"when": {"type": "string", "format": "date-time"},
			"link": {"type": "string", "format": "uri"}
		},
		"required": ["child", "when", "link"],
		"$defs": {"Child": {"type": "object", "properties": {"id": {"type": "string"}}, "required": ["id"], "additionalProperties": false}}
	}`)
	testutil.NoError(t, schema.Validate(decodeValue(t,
		`{"child":{"id":"x"},"when":"2026-10-03T10:11:12Z","link":"https://example.com/a"}`)), "valid rejected: %v")
	testutil.False(t, schema.Validate(decodeValue(t,
		`{"child":{},"when":"2026-10-03T10:11:12Z","link":"https://example.com/a"}`)) == nil, "ref violation accepted")
	testutil.False(t, schema.Validate(decodeValue(t,
		`{"child":{"id":"x"},"when":"yesterday","link":"https://example.com/a"}`)) == nil, "bad date-time accepted")
	testutil.False(t, schema.Validate(decodeValue(t,
		`{"child":{"id":"x"},"when":"2026-10-03T10:11:12Z","link":"not a uri"}`)) == nil, "bad uri accepted")
}

// A keyword this validator does not implement must not fail an answer that
// satisfies everything it does understand.
func TestSchemaIgnoresUnsupportedKeywords(t *testing.T) {
	schema := compile(t, `{
		"type": "object",
		"properties": {"name": {"type": "string", "pattern": "^[a-z]+$", "minLength": 2}},
		"required": ["name"],
		"unevaluatedProperties": false,
		"allOf": [{"required": ["name"]}]
	}`)
	testutil.NoError(t, schema.Validate(decodeValue(t, `{"name":"ada"}`)), "valid answer rejected: %v")
}

func TestStrictSchemaOnlyWhenStrict(t *testing.T) {
	strict := UpstreamRequest{ResponseFormat: map[string]any{
		"type": "json_schema",
		"json_schema": map[string]any{
			"name":   "answer",
			"strict": true,
			"schema": map[string]any{"type": "object", "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}, "required": []any{"ok"}},
		},
	}}
	schema, ok := strict.StrictSchema()
	testutil.Equal(t, ok, true)
	testutil.False(t, schema == nil, "strict request produced no schema")

	// The same envelope without strict is a hint, not a contract.
	loose := UpstreamRequest{ResponseFormat: map[string]any{
		"type": "json_schema",
		"json_schema": map[string]any{
			"name":   "answer",
			"strict": false,
			"schema": map[string]any{"type": "object"},
		},
	}}
	_, ok = loose.StrictSchema()
	testutil.Equal(t, ok, false)

	// json_object names no schema at all.
	_, ok = UpstreamRequest{ResponseFormat: map[string]any{"type": "json_object"}}.StrictSchema()
	testutil.Equal(t, ok, false)
	_, ok = UpstreamRequest{}.StrictSchema()
	testutil.Equal(t, ok, false)

	// Responses text.format is the same contract under a different spelling.
	responses := UpstreamRequest{ResponseText: map[string]any{"format": map[string]any{
		"type": "json_schema",
		"json_schema": map[string]any{
			"strict": true,
			"schema": map[string]any{"type": "object"},
		},
	}}}
	_, ok = responses.StrictSchema()
	testutil.Equal(t, ok, true)
}

func TestSystemHintEmptyUnlessStrict(t *testing.T) {
	var none *StructuredOutput
	testutil.Equal(t, none.SystemHint(), "")
	testutil.NoError(t, none.Validate("anything"), "nil validator rejected an answer: %v")

	out := NewStructuredOutput(UpstreamRequest{ResponseFormat: map[string]any{
		"type":        "json_schema",
		"json_schema": map[string]any{"strict": true, "schema": map[string]any{"type": "object"}},
	}})
	testutil.False(t, out == nil, "strict request produced no validator")
	testutil.MustContain(t, out.SystemHint(), "Respond with JSON only.")
	testutil.MustContain(t, out.SystemHint(), `"type":"object"`)
}

func TestValidateAcceptsFencedAndDecoratedJSON(t *testing.T) {
	out := NewStructuredOutput(UpstreamRequest{ResponseFormat: map[string]any{
		"type": "json_schema",
		"json_schema": map[string]any{
			"strict": true,
			"schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{"type": "string"},
				},
				"required":             []any{"name"},
				"additionalProperties": false,
			},
		},
	}})
	testutil.False(t, out == nil, "strict request produced no validator")

	// A fenced block still contains the answer the caller asked for.
	testutil.NoError(t, out.Validate("Here you go:\n```json\n{\"name\":\"ada\"}\n```\nDone."), "fenced JSON rejected: %v")
	testutil.NoError(t, out.Validate(`{"name":"ada"}`), "bare JSON rejected: %v")
	// Prose around the object is tolerated; the object is what validates.
	testutil.NoError(t, out.Validate("Sure! {\"name\":\"ada\"} Hope that helps."), "decorated JSON rejected: %v")

	// Prose with no object at all is a rejection: a strict caller cannot use it.
	err := out.Validate("I cannot answer that.")
	testutil.False(t, err == nil, "prose accepted for a strict request")
	// A wrong shape is a rejection even when it is valid JSON.
	err = out.Validate(`{"name":42}`)
	testutil.False(t, err == nil, "wrong type accepted")
	// Truncated JSON never balances, so it is rejected rather than guessed at.
	err = out.Validate(`{"name":"ada"`)
	testutil.False(t, err == nil, "truncated JSON accepted")
	// An empty answer has nothing to check; the caller sees the ordinary path.
	testutil.NoError(t, out.Validate("   "), "empty answer rejected: %v")
}

func TestPrependSystemHint(t *testing.T) {
	system := []prompt.SystemItem{{Type: "text", Text: "caller"}, {Type: "text", Text: "prose"}}
	testutil.Equal(t, len(PrependSystemHint(system, "   ")), 2)

	out := PrependSystemHint(system, "shape")
	testutil.Equal(t, len(out), 3)
	testutil.Equal(t, out[0].Text, "shape")
	testutil.Equal(t, out[1].Text, "caller")
	testutil.Equal(t, out[2].Text, "prose")
}
