package qoder

import (
	"bytes"
	"encoding/json"
	"testing"
)

// streamFixture is a long text answer in the upstream's doubly wrapped framing.
func streamFixture(frames int) []byte {
	var out bytes.Buffer
	for i := range frames {
		chunk, _ := json.Marshal(map[string]any{
			"id": "chatcmpl-1", "object": "chat.completion.chunk", "created": 1, "model": "qmodel",
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "token " + string(rune('a'+i%26)) + " \"quoted\"\n"}}},
		})
		envelope, _ := json.Marshal(map[string]any{"headers": map[string]any{}, "body": string(chunk), "statusCodeValue": 200, "statusCode": "OK"})
		out.WriteString("data:")
		out.Write(envelope)
		out.WriteString("\n\n")
	}
	out.WriteString("event:finish\ndata:{\"firstTokenDuration\":1,\"totalDuration\":2}\n\n")
	return out.Bytes()
}

func BenchmarkConsumeStream(b *testing.B) {
	fixture := streamFixture(2000)
	b.SetBytes(int64(len(fixture)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := consumeStreamObserved(bytes.NewReader(fixture), nil, nil); err != nil {
			b.Fatal(err)
		}
	}
}
