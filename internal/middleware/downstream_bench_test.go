package middleware

import "testing"

// Includes framing and output evidence extraction, with fragmented writes.
func BenchmarkDownstreamOutputEvidence(b *testing.B) {
	for _, sample := range []struct{ name, frame string }{
		{"chat", "data: {\"choices\":[{\"delta\":{\"content\":\"hello world\"}}]}\n\n"},
		{"messages", "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"checking the evidence\"}}\n\n"},
		{"responses", "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello world\"}\n\n"},
	} {
		b.Run(sample.name, func(b *testing.B) {
			frame := []byte(sample.frame)
			var detector tokenSSEDetector
			b.ReportAllocs()
			b.SetBytes(int64(len(frame)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				detector.observe(frame[:17])
				if !detector.observe(frame[17:]) {
					b.Fatal("generated output was missed")
				}
			}
		})
	}
}
