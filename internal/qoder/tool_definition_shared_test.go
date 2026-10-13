package qoder

import (
	"reflect"
	"testing"

	"encoding/json"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
	"orchids-api/internal/util"
)

// TestNormalizeToolDefinitionsMatchesSharedHelper pins the shared
// normalization: Qoder no longer carries its own copy of the loop, so any
// drift between the channels would silently change one upstream's wire format.
func TestNormalizeToolDefinitionsMatchesSharedHelper(t *testing.T) {
	declarations := []interface{}{
		// Already in the OpenAI function envelope: kept as-is with type forced.
		map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "openai_tool",
				"description": "already normalized",
				"parameters":  map[string]interface{}{"type": "object", "properties": map[string]interface{}{"a": map[string]interface{}{"type": "string"}}},
			},
		},
		// Anthropic shape: name + input_schema -> function envelope.
		map[string]interface{}{
			"name":        "anthropic_tool",
			"description": "converted",
			"input_schema": map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"path": map[string]interface{}{"type": "string"}},
				"required":   []interface{}{"path"},
			},
		},
		// Anthropic shape without input_schema: default empty object schema.
		map[string]interface{}{"name": "no_schema"},
		// Names are trimmed; an empty name is dropped either way.
		map[string]interface{}{"name": "  spaced_name  "},
		map[string]interface{}{"name": "   "},
		map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": ""}},
		// Unknown shapes and non-objects are dropped.
		map[string]interface{}{"unrelated": true},
		"not-a-map",
		42,
		nil,
	}

	qoderGot := normalizeToolDefinitions(upstream.UpstreamRequest{Tools: declarations})
	workbuddyGot := util.NormalizeToolDefinitions(declarations)

	qoderJSON, err := json.Marshal(qoderGot)
	testutil.NoError(t, err, "marshal qoder tools: %v")
	workbuddyJSON, err := json.Marshal(workbuddyGot)
	testutil.NoError(t, err, "marshal workbuddy tools: %v")
	testutil.Falsef(t, !reflect.DeepEqual(qoderGot, workbuddyGot), "qoder and workbuddy normalization diverged:\nqoder:     %s\nworkbuddy: %s", qoderJSON, workbuddyJSON)
	testutil.Equal(t, len(qoderGot), 4)

	// The NoTools / empty guard is Qoder's own behavior, not the shared helper's.
	got := normalizeToolDefinitions(upstream.UpstreamRequest{NoTools: true, Tools: declarations})
	testutil.Falsef(t, got != nil, "NoTools must return nil, got %#v", got)
	got = normalizeToolDefinitions(upstream.UpstreamRequest{})
	testutil.Falsef(t, got != nil, "no declarations must return nil, got %#v", got)
	// The shared helper itself stays non-nil so WorkBuddy's empty case is unchanged.
	got = util.NormalizeToolDefinitions(nil)
	testutil.Falsef(t, got == nil || len(got) != 0, "shared helper must return an empty non-nil slice for no tools, got %#v", got)
}
