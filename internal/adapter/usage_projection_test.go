package adapter

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strconv"
	"testing"
)

func TestProjectChatUsage(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	for _, tc := range []struct {
		name string
		raw  map[string]any
		want map[string]any
	}{
		{name: "absent"},
		{name: "invalid", raw: map[string]any{"input_tokens": -1, "output_tokens": 1.5}},
		{name: "direct legacy input", raw: map[string]any{"input_tokens": 130, "output_tokens": 506}, want: map[string]any{"prompt_tokens": 130, "completion_tokens": 506, "total_tokens": 636}},
		{name: "native cache exclusive", raw: map[string]any{"input_tokens": 130, "cache_read_input_tokens": 26892, "cache_creation_input_tokens": 64, "output_tokens": 506}, want: map[string]any{"prompt_tokens": 27086, "completion_tokens": 506, "total_tokens": 27592, "cache_read_input_tokens": 26892, "cache_creation_input_tokens": 64, "prompt_tokens_details": map[string]any{"cached_tokens": 26892}}},
		{name: "chat already inclusive", raw: map[string]any{"prompt_tokens": 27086, "input_tokens": 130, "cache_read_input_tokens": 26892, "cache_creation_input_tokens": 64, "completion_tokens": 506}, want: map[string]any{"prompt_tokens": 27086, "completion_tokens": 506, "total_tokens": 27592, "cache_read_input_tokens": 26892, "cache_creation_input_tokens": 64, "prompt_tokens_details": map[string]any{"cached_tokens": 26892}}},
		{name: "missing input", raw: map[string]any{"cache_creation_input_tokens": 64, "output_tokens": 506}, want: map[string]any{"cache_creation_input_tokens": 64, "completion_tokens": 506}},
		{name: "missing output", raw: map[string]any{"input_tokens": 130, "cache_creation_input_tokens": 64}, want: map[string]any{"prompt_tokens": 194, "cache_creation_input_tokens": 64}},
		{name: "known zero", raw: map[string]any{"input_tokens": 0, "cache_read_input_tokens": 0, "output_tokens": 0}, want: map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0, "cache_read_input_tokens": 0, "prompt_tokens_details": map[string]any{"cached_tokens": 0}}},
		{name: "invalid cache", raw: map[string]any{"input_tokens": 130, "cache_creation_input_tokens": -1, "output_tokens": 506}, want: map[string]any{"completion_tokens": 506}},
		{name: "prompt overflow", raw: map[string]any{"input_tokens": maxInt, "cache_creation_input_tokens": 1, "output_tokens": 506}, want: map[string]any{"completion_tokens": 506, "cache_creation_input_tokens": 1}},
		{name: "total overflow", raw: map[string]any{"input_tokens": maxInt, "output_tokens": 1}, want: map[string]any{"prompt_tokens": maxInt, "completion_tokens": 1}},
		{name: "invalid prompt does not fall back", raw: map[string]any{"prompt_tokens": nil, "input_tokens": 130, "cache_creation_input_tokens": 64, "completion_tokens": 506}, want: map[string]any{"cache_creation_input_tokens": 64, "completion_tokens": 506}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ProjectChatUsage(tc.raw)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("usage=%#v, want %#v", got, tc.want)
			}
			if repeated := ProjectChatUsage(got); !reflect.DeepEqual(repeated, got) {
				t.Fatalf("projection was not idempotent: %#v -> %#v", got, repeated)
			}
		})
	}
}

func TestUsageCounterRejectsInvalidOrInexactNumbers(t *testing.T) {
	for _, value := range []any{-1, json.Number("-1"), json.Number("9223372036854775808"), json.Number("1.5"), json.Number("null"), float64(1 << 63), float64(1 << 53), "5", nil} {
		if count, valid := UsageCounter(value); valid {
			t.Errorf("accepted counter %#v as %d", value, count)
		}
	}
	if strconv.IntSize == 64 {
		if count, valid := UsageCounter(json.Number("9007199254740993")); !valid || int64(count) != 9007199254740993 {
			t.Fatalf("exact integer lost: %d, valid=%v", count, valid)
		}
	}
}

func TestOpenAIMessageDeltaUsageIsFinalSnapshot(t *testing.T) {
	data := []byte(`{"delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":130,"cache_read_input_tokens":26892,"cache_creation_input_tokens":64,"output_tokens":506}}`)
	wantUsage := `{"cache_creation_input_tokens":64,"cache_read_input_tokens":26892,"completion_tokens":506,"prompt_tokens":27086,"prompt_tokens_details":{"cached_tokens":26892},"total_tokens":27592}`
	for _, raw := range [][]byte{data, bytes.Replace(data, []byte(`"usage":`), []byte(`"usage" : `), 1)} {
		for _, project := range []func() ([]byte, bool){
			func() ([]byte, bool) { return appendOpenAIChunkFast(nil, "msg_1", 123, "message_delta", raw) },
			func() ([]byte, bool) { return appendOpenAIChunkSlow(nil, "msg_1", 123, "message_delta", raw) },
			func() ([]byte, bool) { return AppendOpenAIChunk([]byte("prefix"), "msg_1", 123, "message_delta", raw) },
		} {
			for range 2 {
				chunk, ok := project()
				chunk = bytes.TrimPrefix(chunk, []byte("prefix"))
				if !ok || bytes.Count(chunk, []byte(`"finish_reason"`)) != 1 {
					t.Fatalf("missing or duplicated terminal chunk: %s", chunk)
				}
				var payload struct {
					Usage json.RawMessage `json:"usage"`
				}
				if err := json.Unmarshal(chunk, &payload); err != nil || string(payload.Usage) != wantUsage {
					t.Fatalf("chunk=%s, want usage=%s, err=%v", chunk, wantUsage, err)
				}
			}
		}
	}
}

func TestOpenAIMessageDeltaUsageDoesNotInventCounters(t *testing.T) {
	for _, tc := range []struct {
		usage string
		want  string
	}{
		{usage: `null`},
		{usage: `{}`},
		{usage: `{"input_tokens":-1,"output_tokens":1.5}`},
		{usage: `{"input_tokens":9223372036854775808}`},
		{usage: `{"output_tokens":506}`, want: `{"completion_tokens":506}`},
		{usage: `{"cache_creation_input_tokens":64}`, want: `{"cache_creation_input_tokens":64}`},
		{usage: `{"input_tokens":9223372036854775807,"cache_creation_input_tokens":1}`, want: `{"cache_creation_input_tokens":1}`},
	} {
		raw, ok := AppendOpenAIChunk(nil, "msg_1", 123, "message_delta", []byte(`{"delta":{"stop_reason":"end_turn"},"usage":`+tc.usage+`}`))
		var payload struct {
			Usage json.RawMessage `json:"usage"`
		}
		if err := json.Unmarshal(raw, &payload); !ok || err != nil || string(payload.Usage) != tc.want {
			t.Errorf("usage=%s chunk=%s, want=%s, err=%v", tc.usage, raw, tc.want, err)
		}
	}
}

func TestOpenAIChunkNoUsageBytesUnchanged(t *testing.T) {
	want := `{"id":"msg_1","object":"chat.completion.chunk","created":123,"model":"","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`
	raw, ok := AppendOpenAIChunk(nil, "msg_1", 123, "message_delta", []byte(`{"delta":{"stop_reason":"end_turn"}}`))
	if !ok || string(raw) != want {
		t.Fatalf("no-usage fast path changed: %s", raw)
	}
	opening, ok := AppendOpenAIChunk(nil, "msg_1", 123, "message_start", []byte(`{"message":{"model":"test","usage":{"input_tokens":999999,"output_tokens":0}}}`))
	if !ok || bytes.Contains(opening, []byte(`"usage"`)) {
		t.Fatalf("opening estimate exposed: %s", opening)
	}
}
