package qoder

import (
	"testing"

	"encoding/json"

	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

// thinking_config blocks as the international catalog ships them.
var observedThinkingRows = map[string]string{
	"qfmodel":  `{"disabled":{},"enabled":{"efforts":{"xhigh":{},"low":{},"medium":{"is_default":true}},"is_default":true}}`,
	"dmodel":   `{"disabled":{"description":"Disable thinking"},"enabled":{"description":"Enable thinking","efforts":{"high":{},"max":{"is_default":true}},"is_default":true}}`,
	"ultimate": `{"disabled":{},"enabled":{"efforts":{"xhigh":{},"high":{"is_default":true},"low":{},"max":{},"medium":{}},"is_default":true}}`,
	"onoff":    `{"disabled":{},"enabled":{"is_default":true}}`,
	"off":      `{"disabled":{"is_default":true},"enabled":{"efforts":{"low":{},"high":{"is_default":true}}}}`,
}

func thinkingRow(key string) modelEntry {
	return modelEntry{Key: key, ThinkingConfig: json.RawMessage(observedThinkingRows[key])}
}

func TestReasoningDefaultFollowsModeFlags(t *testing.T) {
	for key, want := range map[string]bool{"qfmodel": true, "dmodel": true, "onoff": true, "off": false} {
		testutil.Falsef(t, thinkingRow(key).reasoningDefault() != want, "%s: default thinking = %v, want %v", key, !want, want)
	}
	// Without a block the top-level flag decides.
	testutil.False(t, modelEntry{IsReasoning: false}.reasoningDefault(), "flag false without a block")
	testutil.False(t, !modelEntry{IsReasoning: true}.reasoningDefault(), "flag true without a block")
}

// The gateway ignores a tier the model lacks, so each request is mapped onto
// the model's own table: nearest tier, ties toward the model default.
func TestNormalizeEffortMapsOntoTheModelTable(t *testing.T) {
	for _, tc := range []struct{ key, requested, want string }{
		{"qfmodel", "medium", "medium"},
		{"qfmodel", "high", "medium"}, // medium and xhigh tie; medium is the default
		{"qfmodel", "max", "xhigh"},
		{"qfmodel", "minimal", "low"},
		{"dmodel", "medium", "high"},
		{"dmodel", "xhigh", "max"}, // high and max tie; max is the default
		{"dmodel", "low", "high"},
		{"ultimate", "minimal", "low"},
		{"dmodel", "turbo", "max"}, // unknown word: the model default
		{"qfmodel", "OFF", "none"},
		{"onoff", "high", ""},            // on/off only: thinking on, no tier
		{"unlisted", "medium", "medium"}, // no block: passed verbatim
	} {
		testutil.Falsef(t, thinkingRow(tc.key).normalizeEffort(tc.requested) != tc.want, "%s %q -> %q, want %q", tc.key, tc.requested, thinkingRow(tc.key).normalizeEffort(tc.requested), tc.want)
	}
}

func TestRequestBodyCarriesNormalizedEffort(t *testing.T) {
	for _, tc := range []struct {
		key, requested, wantEffort string
		wantThinking               bool
	}{
		{"qfmodel", "high", "medium", true},
		{"onoff", "high", "", true},
		{"dmodel", "none", "none", false},
		{"off", "", "", false},
	} {
		body := decodeRegressionBody(t, upstream.UpstreamRequest{Prompt: "hi", ReasoningEffort: tc.requested}, thinkingRow(tc.key))
		parameters := body.Parameters.(map[string]any)
		got, _ := parameters["reasoning_effort"].(string)
		testutil.Falsef(t, got != tc.wantEffort, "%s %q: reasoning_effort = %q, want %q", tc.key, tc.requested, got, tc.wantEffort)
		testutil.Falsef(t, body.ModelConfig.IsReasoning != tc.wantThinking, "%s %q: is_reasoning = %v", tc.key, tc.requested, body.ModelConfig.IsReasoning)
	}
}
