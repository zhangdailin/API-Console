package responses

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestUsageFromChatProjectsCacheDialects(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	for _, tc := range []struct {
		name string
		raw  map[string]any
		want map[string]any
	}{
		{name: "native Messages", raw: map[string]any{"input_tokens": 130, "cache_read_input_tokens": 26892, "cache_creation_input_tokens": 64, "output_tokens": 506}, want: map[string]any{"input_tokens": 27086, "output_tokens": 506, "total_tokens": 27592, "input_tokens_details": map[string]any{"cached_tokens": 26892}, "cache_read_input_tokens": 26892, "cache_creation_input_tokens": 64}},
		{name: "inclusive Chat", raw: map[string]any{"prompt_tokens": 27086, "cache_read_input_tokens": 26892, "cache_creation_input_tokens": 64, "completion_tokens": 506}, want: map[string]any{"input_tokens": 27086, "output_tokens": 506, "total_tokens": 27592, "input_tokens_details": map[string]any{"cached_tokens": 26892}, "cache_read_input_tokens": 26892, "cache_creation_input_tokens": 64}},
		{name: "legacy input without cache", raw: map[string]any{"input_tokens": 130, "output_tokens": 506}, want: map[string]any{"input_tokens": 130, "output_tokens": 506, "total_tokens": 636}},
		{name: "cache only does not invent input", raw: map[string]any{"cache_creation_input_tokens": 64, "output_tokens": 506}, want: map[string]any{"cache_creation_input_tokens": 64, "output_tokens": 506}},
		{name: "invalid cache omits input", raw: map[string]any{"input_tokens": 130, "cache_read_input_tokens": -1, "output_tokens": 506}, want: map[string]any{"output_tokens": 506}},
		{name: "overflowing cache omits input", raw: map[string]any{"input_tokens": maxInt, "cache_read_input_tokens": 1, "output_tokens": 506}, want: map[string]any{"cache_read_input_tokens": 1, "input_tokens_details": map[string]any{"cached_tokens": 1}, "output_tokens": 506}},
		{name: "overflowing total omitted", raw: map[string]any{"prompt_tokens": maxInt, "completion_tokens": 1}, want: map[string]any{"input_tokens": maxInt, "output_tokens": 1}},
		{name: "preserve normalized details", raw: map[string]any{"input_tokens": 130, "cache_read_input_tokens": 26892, "input_tokens_details": map[string]any{"audio_tokens": 7}}, want: map[string]any{"input_tokens": 27022, "cache_read_input_tokens": 26892, "input_tokens_details": map[string]any{"audio_tokens": 7, "cached_tokens": 26892}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, err := json.Marshal(tc.raw)
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if got := UsageFromChat(tc.raw); !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("usage=%#v, want %#v", got, tc.want)
				}
			}
			after, err := json.Marshal(tc.raw)
			if err != nil || string(before) != string(after) {
				t.Fatalf("input usage mutated: before=%s after=%s err=%v", before, after, err)
			}
		})
	}
}

func TestMessagesStreamFinalUsageReplacesOpeningEstimate(t *testing.T) {
	input := "event: message_start\ndata: {\"message\":{\"model\":\"test\",\"usage\":{\"input_tokens\":999999,\"output_tokens\":0}}}\n\n" +
		"event: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n" +
		"event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":130,\"cache_read_input_tokens\":26892,\"cache_creation_input_tokens\":64,\"output_tokens\":506}}\n\n" +
		"event: message_stop\ndata: {}\n\n"
	w := httptest.NewRecorder()
	WriteStreamFromChatReader(w, CreateRequest{Model: "test"}, strings.NewReader(input), StreamOptions{})
	got := completedStreamResponse(t, w.Body.String())["usage"]
	want := map[string]any{"input_tokens": float64(27086), "output_tokens": float64(506), "total_tokens": float64(27592), "input_tokens_details": map[string]any{"cached_tokens": float64(26892)}, "cache_read_input_tokens": float64(26892), "cache_creation_input_tokens": float64(64)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("final usage=%#v, want %#v", got, want)
	}
}

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
