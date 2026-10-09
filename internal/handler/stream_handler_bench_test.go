package handler

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"orchids-api/internal/adapter"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/upstream"
)

type discardStringByteWriter struct{}

type discardFlushResponseWriter struct {
	header http.Header
}

func (discardStringByteWriter) Write(p []byte) (int, error) { return len(p), nil }

func (discardStringByteWriter) WriteString(s string) (int, error) { return len(s), nil }

func newDiscardFlushResponseWriter() *discardFlushResponseWriter {
	return &discardFlushResponseWriter{header: make(http.Header)}
}

func (w *discardFlushResponseWriter) Header() http.Header { return w.header }

func (w *discardFlushResponseWriter) Write(p []byte) (int, error) { return len(p), nil }

func (w *discardFlushResponseWriter) WriteHeader(statusCode int) {}

func (w *discardFlushResponseWriter) Flush() {}

func newBenchmarkStreamHandler(b *testing.B) *streamHandler {
	b.Helper()
	cfg := &config.Config{DebugEnabled: false}
	logger := debug.New(false, false)
	b.Cleanup(func() { logger.Close() })
	sh := newStreamHandler(cfg, newDiscardFlushResponseWriter(), logger, false, true, adapter.FormatAnthropic)
	b.Cleanup(sh.release)
	return sh
}

func writeSSEFrameBytesMultiWrite(w io.Writer, event string, data []byte) error {
	if _, err := io.WriteString(w, sseEventPrefix); err != nil {
		return err
	}
	if _, err := io.WriteString(w, event); err != nil {
		return err
	}
	if _, err := io.WriteString(w, sseDataJoin); err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	_, err := io.WriteString(w, sseLineBreak)
	return err
}

func writeOpenAIFrameMultiWrite(w io.Writer, payload []byte) error {
	if _, err := io.WriteString(w, sseDataPrefix); err != nil {
		return err
	}
	if _, err := w.Write(payload); err != nil {
		return err
	}
	_, err := io.WriteString(w, sseLineBreak)
	return err
}

func BenchmarkToolValidationWrite(b *testing.B) {
	tool := "Write"
	input := `{"file_path":"/tmp/a.txt","content":"hello"}`
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		ok := validToolCallInput(tool, input)
		if !ok {
			b.Fatal("unexpected invalid input")
		}
	}
}

func BenchmarkMarshalContentBlockDeltaText_Bytes(b *testing.B) {
	idx := 7
	text := "hello world"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, _ := marshalSSEContentBlockDeltaTextBytes(idx, text)
		_ = raw
	}
}

func BenchmarkMarshalContentBlockDeltaThinking_Bytes(b *testing.B) {
	idx := 7
	thinking := "analyzing next step"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, _ := appendSSEContentBlockDeltaThinking(nil, idx, thinking)
		_ = raw
	}
}

func BenchmarkMarshalContentBlockDeltaInputJSON_Bytes(b *testing.B) {
	idx := 5
	partialJSON := `{"file_path":"/tmp/a.txt","content":"hello world"}`
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, _ := appendSSEContentBlockDeltaInputJSON(nil, idx, partialJSON)
		_ = raw
	}
}

func BenchmarkMarshalMessageStart_Bytes(b *testing.B) {
	msgID := "msg_123"
	model := "claude-3-7-sonnet"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, _ := marshalSSEMessageStartBytes(msgID, model, 12, 0)
		_ = raw
	}
}

func BenchmarkAppendSSEMessageStart_ReusedBuffer(b *testing.B) {
	msgID := "msg_123"
	model := "claude-3-7-sonnet"
	buf := make([]byte, 0, 256)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, err := appendSSEMessageStart(buf[:0], msgID, model, 12, 0)
		if err != nil {
			b.Fatal(err)
		}
		buf = raw[:0]
	}
}

func BenchmarkAppendSSEContentBlockDeltaText_ReusedBuffer(b *testing.B) {
	idx := 7
	text := "hello world"
	buf := make([]byte, 0, 256)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, err := appendSSEContentBlockDeltaText(buf[:0], idx, text)
		if err != nil {
			b.Fatal(err)
		}
		buf = raw[:0]
	}
}

func BenchmarkAppendSSEContentBlockDeltaThinking_ReusedBuffer(b *testing.B) {
	idx := 7
	thinking := "analyzing next step"
	buf := make([]byte, 0, 256)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, err := appendSSEContentBlockDeltaThinking(buf[:0], idx, thinking)
		if err != nil {
			b.Fatal(err)
		}
		buf = raw[:0]
	}
}

func BenchmarkAppendSSEContentBlockDeltaInputJSON_ReusedBuffer(b *testing.B) {
	idx := 5
	partialJSON := `{"file_path":"/tmp/a.txt","content":"hello world"}`
	buf := make([]byte, 0, 256)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, err := appendSSEContentBlockDeltaInputJSON(buf[:0], idx, partialJSON)
		if err != nil {
			b.Fatal(err)
		}
		buf = raw[:0]
	}
}

func BenchmarkEmitToolCallStream_Final(b *testing.B) {
	sh := newBenchmarkStreamHandler(b)
	call := toolCall{
		id:    "tool_123",
		name:  "Write",
		input: `{"file_path":"/tmp/a.txt","content":"hello world"}`,
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		sh.blockIndex = -1
		sh.emitToolCallStream(call, -1, true)
	}
}

func BenchmarkWriteMessageStartLocked_Stream(b *testing.B) {
	sh := newBenchmarkStreamHandler(b)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		sh.mu.Lock()
		sh.writeMessageStartLocked("claude-3-7-sonnet", 12, 0)
		sh.mu.Unlock()
	}
}

func BenchmarkWriteSSEFrameBytes_MultiWrite(b *testing.B) {
	writer := discardStringByteWriter{}
	event := "content_block_delta"
	data := []byte("{\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := writeSSEFrameBytesMultiWrite(writer, event, data); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWriteOpenAIFrame_MultiWrite(b *testing.B) {
	writer := discardStringByteWriter{}
	payload := []byte("{\"id\":\"msg_1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := writeOpenAIFrameMultiWrite(writer, payload); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAppendJSONBytes_RawASCII(b *testing.B) {
	value := "hello world"
	dst := make([]byte, 0, len(value)+2)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, _ := appendJSONBytes(dst[:0], value)
		_ = raw
	}
}

func BenchmarkAppendJSONBytes_EscapedJSONLike(b *testing.B) {
	value := "{\"file_path\":\"/tmp/a.txt\",\"content\":\"hello \\\"world\\\"\\nnext\"}"
	dst := make([]byte, 0, len(value)+16)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, _ := appendJSONBytes(dst[:0], value)
		_ = raw
	}
}

func BenchmarkMarshalContentBlockStartToolUse_Bytes(b *testing.B) {
	idx := 3
	id := "tool_123"
	name := "Write"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, _ := appendSSEContentBlockStartToolUse(nil, idx, id, name)
		_ = raw
	}
}

func BenchmarkAppendSSEContentBlockStartToolUse_ReusedBuffer(b *testing.B) {
	idx := 3
	id := "tool_123"
	name := "Write"
	buf := make([]byte, 0, 256)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, err := appendSSEContentBlockStartToolUse(buf[:0], idx, id, name)
		if err != nil {
			b.Fatal(err)
		}
		buf = raw[:0]
	}
}

func BenchmarkMarshalContentBlockStartText_Bytes(b *testing.B) {
	idx := 3
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, _ := marshalSSEContentBlockStartTextBytes(idx)
		_ = raw
	}
}

func BenchmarkMarshalContentBlockStartThinking_Bytes(b *testing.B) {
	idx := 3
	signature := "sig_123"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, _ := appendSSEContentBlockStartThinking(nil, idx, signature)
		_ = raw
	}
}

func BenchmarkMarshalContentBlockStop_Bytes(b *testing.B) {
	idx := 3
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, _ := marshalSSEContentBlockStopBytes(idx)
		_ = raw
	}
}

func BenchmarkMarshalMessageDelta_Bytes(b *testing.B) {
	stopReason := "tool_use"
	outputTokens := 42
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, _ := marshalSSEMessageDeltaBytes(stopReason, outputTokens)
		_ = raw
	}
}

func BenchmarkAppendSSEMessageDelta_ReusedBuffer(b *testing.B) {
	stopReason := "tool_use"
	outputTokens := 42
	buf := make([]byte, 0, 256)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, err := appendSSEMessageDelta(buf[:0], stopReason, outputTokens)
		if err != nil {
			b.Fatal(err)
		}
		buf = raw[:0]
	}
}

// Per-frame cost of the streaming relay end to end: one upstream text delta, from
// handleMessage through the client-facing SSE write, carrying the block state it
// actually touches. The benchmarks above measure the marshalling helpers in
// isolation; this is what a long answer pays per frame, so a per-frame allocation
// or a per-frame copy shows up here as a number instead of as a guess.
func BenchmarkStreamingRelayTextDeltaFrame(b *testing.B) {
	const framesPerRun = 512
	cfg := &config.Config{DebugEnabled: false}
	logger := debug.New(false, false)
	defer logger.Close()

	for _, size := range []int{8, 24, 128} {
		b.Run("delta="+strconv.Itoa(size), func(b *testing.B) {
			msg := upstream.SSEMessage{
				Type:  "model.text-delta",
				Event: map[string]interface{}{"delta": strings.Repeat("x", size)},
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				sh := newStreamHandler(cfg, newDiscardFlushResponseWriter(), logger, false, true, adapter.FormatAnthropic)
				for j := 0; j < framesPerRun; j++ {
					sh.handleMessage(msg)
				}
				sh.release()
			}
			b.ReportMetric(float64(framesPerRun), "frames/op")
		})
	}
}
