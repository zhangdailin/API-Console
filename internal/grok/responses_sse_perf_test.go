package grok

import (
	"bytes"
	"io"
	"orchids-api/internal/responses"
	"orchids-api/internal/testutil"
	"strings"
	"testing"
)

func TestReadResponseSSEBytesPreservesPayloads(t *testing.T) {
	stream := "\ufeffevent: message\r\ndata: {\"text\":\"hello\"}\r\ndata: \r\n\r\n" +
		"data: [DONE]\n\n"
	var events []string
	var payloads [][]byte
	if err := responses.ReadSSEBytes(strings.NewReader(stream), func(event string, data []byte) error {
		events = append(events, event)
		payloads = append(payloads, bytes.Clone(data))
		return nil
	}); err != nil {
		t.Fatalf("readResponseSSEBytes: %v", err)
	}
	testutil.Equal(t, len(payloads), 2)
	testutil.Equal(t, events[0], "message")
	testutil.Equal(t, string(payloads[0]), "{\"text\":\"hello\"}\n")
	testutil.Equal(t, events[1], "")
	testutil.Equal(t, string(payloads[1]), "[DONE]")
}

func BenchmarkReadResponseSSEBytesJSONPayload(b *testing.B) {
	payload := `{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"` + strings.Repeat("response text ", 32) + `"}}]}`
	stream := []byte("event: message\ndata: " + payload + "\n\ndata: [DONE]\n\n")
	b.ReportAllocs()
	b.SetBytes(int64(len(stream)))
	for b.Loop() {
		if err := responses.ReadSSEBytes(bytes.NewReader(stream), func(_ string, data []byte) error {
			_, _ = io.Discard.Write(data)
			return nil
		}); err != nil {
			b.Fatal(err)
		}
	}
}
