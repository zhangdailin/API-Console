package responses

import (
	"bytes"
	"io"
	"testing"

	"orchids-api/internal/httpserver"
)

func BenchmarkConsumeSSETextDeltas(b *testing.B) {
	const frame = "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello world\"}\n\n"
	stream := bytes.Repeat([]byte(frame), 100)
	b.SetBytes(int64(len(stream)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		err := ConsumeSSE(bytes.NewReader(stream), func(event SSEEvent) error {
			if event.Event != "response.output_text.delta" || len(event.Data()) == 0 {
				b.Fatal("invalid SSE frame")
			}
			return nil
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRewrittenSSEFrame(b *testing.B) {
	writer := &httpserver.CheckedStreamWriter{Target: io.Discard}
	event := SSEEvent{Event: "response.output_text.delta"}
	event.SetData(`{"type":"response.output_text.delta","delta":"hello world"}`)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := event.WriteFrame(writer); err != nil {
			b.Fatal(err)
		}
	}
}
