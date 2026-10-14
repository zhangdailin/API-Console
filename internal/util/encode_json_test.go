package util

import (
	"testing"

	"orchids-api/internal/testutil"
)

func TestEncodeJSONBytesDoesNotEscapeHTML(t *testing.T) {
	payload := map[string]any{
		"type": "chunk",
		"data": map[string]any{
			"text": "hello <world>",
			"n":    1,
		},
	}
	got := string(EncodeJSONBytes(payload))
	testutil.MustContain(t, got, "hello <world>")
}

func TestEncodeJSONBytesFallsBackToEmptyObject(t *testing.T) {
	got := string(EncodeJSONBytes(func() {}))
	testutil.Equal(t, got, "{}")
}

func BenchmarkEncodeJSON_Bytes(b *testing.B) {
	payload := map[string]any{
		"id": "msg_1",
		"choices": []map[string]any{{
			"index": 0,
			"delta": map[string]any{"content": "hello world"},
		}},
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = EncodeJSONBytes(payload)
	}
}
