package handler

import (
	"bufio"
	"io"
	"net/http"
	"slices"
	"testing"

	"orchids-api/internal/adapter"
	"orchids-api/internal/config"
)

type pipeRelayWriter struct {
	io.Writer
	header http.Header
}

func (w *pipeRelayWriter) Header() http.Header { return w.header }
func (w *pipeRelayWriter) WriteHeader(int)     {}
func (w *pipeRelayWriter) Flush()              {}

// Measures the local SSE-to-pipe bridge, excluding provider generation and
// network latency. The reader acknowledges complete frames, not first bytes.
func BenchmarkStreamRelayPipe(b *testing.B) {
	for _, variant := range []struct {
		format adapter.ResponseFormat
		legacy bool
	}{
		{adapter.FormatAnthropic, true}, {adapter.FormatAnthropic, false},
		{adapter.FormatOpenAI, true}, {adapter.FormatOpenAI, false},
	} {
		format := variant.format
		name := "messages"
		if format == adapter.FormatOpenAI {
			name = "chat"
		}
		if variant.legacy {
			name += "/three-writes"
		} else {
			name += "/one-write"
		}
		b.Run(name, func(b *testing.B) {
			reader, writer := io.Pipe()
			ack := make(chan struct{})
			go func() {
				defer close(ack)
				scanner := bufio.NewScanner(reader)
				for scanner.Scan() {
					if scanner.Text() == "" {
						ack <- struct{}{}
					}
				}
			}()
			sh := newStreamHandler(&config.Config{}, &pipeRelayWriter{Writer: writer, header: make(http.Header)}, nil, false, true, format)
			defer sh.release()
			defer reader.Close()
			defer writer.Close()
			data := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`)
			samples := make([]int64, 0, min(b.N, 100000))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				start := relayBenchNow()
				if variant.legacy {
					var err error
					if format == adapter.FormatOpenAI {
						raw, _ := adapter.AppendOpenAIChunk(sh.openAIChunkScratch[:0], sh.msgID, sh.startTime.Unix(), "content_block_delta", data)
						sh.openAIChunkScratch = raw[:0]
						err = writeOpenAIFrame(sh.w, raw)
					} else {
						err = writeSSEFrameBytes(sh.w, "content_block_delta", data)
					}
					if err != nil {
						b.Fatal(err)
					}
					sh.flushSSEWithLenLocked("content_block_delta", len(data), true, false)
				} else {
					sh.emitSSEFrameLocked("content_block_delta", data, true, false)
				}
				if _, ok := <-ack; !ok {
					b.Fatal("pipe reader stopped")
				}
				if len(samples) < cap(samples) {
					samples = append(samples, relayBenchElapsed(start))
				}
			}
			b.StopTimer()
			slices.Sort(samples)
			for _, q := range []struct {
				name    string
				percent int
			}{{"p90-ns", 90}, {"p95-ns", 95}, {"p99-ns", 99}} {
				if len(samples) > 0 {
					b.ReportMetric(float64(samples[(len(samples)*q.percent+99)/100-1]), q.name)
				}
			}
		})
	}
}
