package responses

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestObjectFromChatDoesNotInventUsage(t *testing.T) {
	for _, usage := range []any{nil, map[string]any{}, map[string]any{"prompt_tokens": nil}} {
		response := ObjectFromChat("test", map[string]any{"usage": usage})
		if _, exists := response["usage"]; exists {
			t.Fatalf("absent usage was fabricated: %#v", response["usage"])
		}
	}
}

func TestUsageFromChatPreservesUnknownAndZeroCounters(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  any
		want map[string]any
	}{
		{name: "absent"},
		{name: "empty", raw: map[string]any{}},
		{name: "null", raw: map[string]any{"prompt_tokens": nil}},
		{name: "invalid", raw: map[string]any{"prompt_tokens": "unknown", "completion_tokens": -1, "total_tokens": 1.5}},
		{name: "partial", raw: map[string]any{"completion_tokens": 5}, want: map[string]any{"output_tokens": 5}},
		{name: "zero input", raw: map[string]any{"prompt_tokens": 0}, want: map[string]any{"input_tokens": 0}},
		{name: "reported zero total", raw: map[string]any{"prompt_tokens": 3, "completion_tokens": 5, "total_tokens": 0}, want: map[string]any{"input_tokens": 3, "output_tokens": 5, "total_tokens": 0}},
		{name: "derived total", raw: map[string]any{"prompt_tokens": json.Number("3"), "completion_tokens": float64(5)}, want: map[string]any{"input_tokens": 3, "output_tokens": 5, "total_tokens": 8}},
		{name: "known zero", raw: map[string]any{"input_tokens": 0, "output_tokens": 0}, want: map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}},
		{name: "details", raw: map[string]any{"prompt_tokens_details": map[string]any{"cached_tokens": 2}}, want: map[string]any{"input_tokens_details": map[string]any{"cached_tokens": 2}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := UsageFromChat(tc.raw); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("usage=%#v, want %#v", got, tc.want)
			}
		})
	}
}
