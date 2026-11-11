package util

import (
	"encoding/json"
	"testing"

	"orchids-api/internal/testutil"
)

func TestNormalizeToolDefinitionsDescriptionAndSharing(t *testing.T) {
	openAI := map[string]any{"function": map[string]any{"name": "bash"}}
	tools := []any{
		map[string]any{"name": "glob"},
		map[string]any{"name": "read", "description": "Read a file"},
		openAI,
		map[string]any{"description": "nameless"},
		"not an object",
	}
	render := func(keepEmpty bool) string {
		raw, err := json.Marshal(NormalizeToolDefinitions(tools, keepEmpty))
		testutil.NoError(t, err)
		return string(raw)
	}
	const tail = `{"function":{"description":"Read a file","name":"read","parameters":{"properties":{},"type":"object"}},"type":"function"},` +
		`{"function":{"name":"bash"},"type":"function"}]`
	testutil.Equal(t, render(false), `[{"function":{"name":"glob","parameters":{"properties":{},"type":"object"}},"type":"function"},`+tail)
	testutil.Equal(t, render(true), `[{"function":{"description":"","name":"glob","parameters":{"properties":{},"type":"object"}},"type":"function"},`+tail)
	_, mutated := openAI["type"]
	testutil.False(t, mutated, "the caller's declaration was modified")
}
