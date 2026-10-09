package grok

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestBuildFunctionSchemaRetainsConstraintsAndCallerInput(t *testing.T) {
	var schema map[string]interface{}
	const original = `{"type":"object","$defs":{"text":{"type":["string","null"]}},"properties":{"text":{"$ref":"#/$defs/text"}},"additionalProperties":false,"allOf":[{"minProperties":1}],"anyOf":[{"required":["text"]},{"required":["file"]}],"oneOf":[{"maxProperties":1},{"minProperties":2}]}`
	if err := json.Unmarshal([]byte(original), &schema); err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(schema)
	got := normalizeBuildFunctionSchema(schema)
	after, _ := json.Marshal(schema)
	if string(before) != string(after) {
		t.Fatal("normalization changed caller schema")
	}
	for _, key := range []string{"type", "$defs", "properties", "additionalProperties"} {
		if !reflect.DeepEqual(got[key], schema[key]) {
			t.Fatalf("lost %s", key)
		}
	}
	want := []interface{}{schema["allOf"].([]interface{})[0], map[string]interface{}{"anyOf": schema["anyOf"]}, map[string]interface{}{"oneOf": schema["oneOf"]}}
	if !reflect.DeepEqual(got["allOf"], want) || got["anyOf"] != nil || got["oneOf"] != nil {
		t.Fatalf("lost root conjunctions: %#v", got)
	}
	// A non-object union must remain a union; guessing an object type would
	// silently alter a declaration the gateway does not understand.
	var nonObject map[string]interface{}
	json.Unmarshal([]byte(`{"anyOf":[{"type":"object"},{"type":"string"}]}`), &nonObject)
	if !reflect.DeepEqual(normalizeBuildFunctionSchema(nonObject), nonObject) {
		t.Fatal("changed non-object union")
	}
}
