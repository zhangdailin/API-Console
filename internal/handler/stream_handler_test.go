package handler

import (
	"bytes"
	"io"
	"strings"

	"net/http"

	"testing"

	"encoding/json"

	"orchids-api/internal/adapter"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/testutil"

	"orchids-api/internal/upstream"
)

type flushRecorder struct {
	header  http.Header
	buf     bytes.Buffer
	code    int
	flushes int
}

type failingResponseWriter struct {
	header http.Header
	err    error
}

func (w *failingResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}
func (w *failingResponseWriter) Write([]byte) (int, error) { return 0, w.err }
func (w *failingResponseWriter) WriteHeader(int)           {}
func (w *failingResponseWriter) Flush()                    {}

func newFlushRecorder() *flushRecorder { return &flushRecorder{header: make(http.Header), code: 200} }

func (r *flushRecorder) Header() http.Header         { return r.header }
func (r *flushRecorder) Write(b []byte) (int, error) { return r.buf.Write(b) }
func (r *flushRecorder) WriteHeader(statusCode int)  { r.code = statusCode }
func (r *flushRecorder) Flush()                      { r.flushes++ }

func TestMarshalSSEPayloads_ManualJSONEscapes(t *testing.T) {
	newline := string(byte('\n'))
	expectedText := "he" + "\"" + "llo" + newline + "next"
	raw, err := marshalSSEContentBlockDeltaTextBytes(7, expectedText)
	testutil.NoError(t, err, "marshal text delta: %v")
	var delta map[string]any
	testutil.NoError(t, json.Unmarshal(raw, &delta), "unmarshal text delta: %v")
	testutil.Equal(t, int(delta["index"].(float64)), 7)
	deltaObj := delta["delta"].(map[string]any)
	testutil.Equal(t, deltaObj["type"], "text_delta")
	testutil.EqualAny(t, deltaObj["text"], expectedText)

	expectedToolID := `tool_"1`
	expectedToolName := "Wr" + newline + "ite"
	raw, err = appendSSEContentBlockStartToolUse(nil, 3, expectedToolID, expectedToolName)
	testutil.NoError(t, err, "marshal tool start: %v")
	var startPayload map[string]any
	testutil.NoError(t, json.Unmarshal(raw, &startPayload), "unmarshal tool start: %v")
	contentBlock := startPayload["content_block"].(map[string]any)
	testutil.EqualAny(t, contentBlock["id"], expectedToolID)
	testutil.EqualAny(t, contentBlock["name"], expectedToolName)

	expectedSignature := "sig\"" + newline + "next"
	rawBytes, err := appendSSEContentBlockStartThinking(nil, 4, expectedSignature)
	testutil.NoError(t, err, "marshal thinking start: %v")
	var thinkingStartPayload map[string]any
	testutil.NoError(t, json.Unmarshal(rawBytes, &thinkingStartPayload), "unmarshal thinking start: %v")
	thinkingBlock := thinkingStartPayload["content_block"].(map[string]any)
	testutil.Equal(t, thinkingBlock["type"], "thinking")
	testutil.EqualAny(t, thinkingBlock["signature"], expectedSignature)

	expectedPartialJSON := "{\"path\":\"a.txt\",\"content\":\"he\\\"llo" + newline + "next\"}"
	rawBytes, err = appendSSEContentBlockDeltaInputJSON(nil, 5, expectedPartialJSON)
	testutil.NoError(t, err, "marshal input_json delta: %v")
	var inputJSONPayload map[string]any
	testutil.NoError(t, json.Unmarshal(rawBytes, &inputJSONPayload), "unmarshal input_json delta: %v")
	inputDelta := inputJSONPayload["delta"].(map[string]any)
	testutil.Equal(t, inputDelta["type"], "input_json_delta")
	testutil.EqualAny(t, inputDelta["partial_json"], expectedPartialJSON)

	expectedStopReason := "tool_\"use" + newline + "next"
	rawBytes, err = marshalSSEMessageDeltaBytes(expectedStopReason, 42)
	testutil.NoError(t, err, "marshal message delta: %v")
	var msg map[string]any
	testutil.NoError(t, json.Unmarshal(rawBytes, &msg), "unmarshal message delta: %v")
	testutil.Equal(t, msg["type"], "message_delta")
	testutil.EqualAny(t, msg["delta"].(map[string]any)["stop_reason"], expectedStopReason)
	testutil.Equal(t, int(msg["usage"].(map[string]any)["output_tokens"].(float64)), 42)

	msgStartRaw, err := marshalSSEMessageStartBytes("msg_123", "claude-test", 12, 0)
	testutil.NoError(t, err, "marshal message start: %v")
	var msgStart map[string]any
	testutil.NoError(t, json.Unmarshal(msgStartRaw, &msgStart), "unmarshal message start: %v")
	testutil.Equal(t, msgStart["type"], "message_start")
	messageObj := msgStart["message"].(map[string]any)
	testutil.Equal(t, messageObj["id"], "msg_123")
	testutil.Equal(t, messageObj["model"], "claude-test")
	usageObj := messageObj["usage"].(map[string]any)
	testutil.Equal(t, int(usageObj["input_tokens"].(float64)), 12)
	testutil.Equal(t, int(usageObj["output_tokens"].(float64)), 0)

	plainText := "hello ??"
	rawBytes, err = marshalSSEContentBlockDeltaTextBytes(9, plainText)
	testutil.NoError(t, err, "marshal plain text delta: %v")
	var plainDelta map[string]any
	testutil.NoError(t, json.Unmarshal(rawBytes, &plainDelta), "unmarshal plain text delta: %v")
	testutil.EqualAny(t, plainDelta["delta"].(map[string]any)["text"], plainText)

	htmlEscaped := "<tag>&\u2028\u2029"
	rawBytes, err = marshalSSEContentBlockDeltaTextBytes(10, htmlEscaped)
	testutil.NoError(t, err, "marshal html escaped delta: %v")
	testutil.Falsef(t, !bytes.Contains(rawBytes, []byte("\\u003c")) || !bytes.Contains(rawBytes, []byte("\\u003e")) || !bytes.Contains(rawBytes, []byte("\\u0026")), "expected html-sensitive bytes to be escaped, got: %s", rawBytes)
	testutil.Falsef(t, !bytes.Contains(rawBytes, []byte("\\u2028")) || !bytes.Contains(rawBytes, []byte("\\u2029")), "expected line separator bytes to be escaped, got: %s", rawBytes)
	var escapedDelta map[string]any
	testutil.NoError(t, json.Unmarshal(rawBytes, &escapedDelta), "unmarshal html escaped delta: %v")
	testutil.EqualAny(t, escapedDelta["delta"].(map[string]any)["text"], htmlEscaped)
}

func TestAppendSSEPayloadBuildersMatchMarshal(t *testing.T) {
	tests := []struct {
		name     string
		marshal  func() ([]byte, error)
		appendTo func([]byte) ([]byte, error)
	}{
		{
			name:    "tool start",
			marshal: func() ([]byte, error) { return appendSSEContentBlockStartToolUse(nil, 3, `tool_"1`, "Wr\nite") },
			appendTo: func(dst []byte) ([]byte, error) {
				return appendSSEContentBlockStartToolUse(dst, 3, `tool_"1`, "Wr\nite")
			},
		},
		{
			name:     "text start",
			marshal:  func() ([]byte, error) { return marshalSSEContentBlockStartTextBytes(4) },
			appendTo: func(dst []byte) ([]byte, error) { return appendSSEContentBlockStartText(dst, 4) },
		},
		{
			name:    "thinking start",
			marshal: func() ([]byte, error) { return appendSSEContentBlockStartThinking(nil, 5, "sig\n123") },
			appendTo: func(dst []byte) ([]byte, error) {
				return appendSSEContentBlockStartThinking(dst, 5, "sig\n123")
			},
		},
		{
			name: "input json delta",
			marshal: func() ([]byte, error) {
				return appendSSEContentBlockDeltaInputJSON(nil, 6, `{"path":"a.txt","content":"he\"llo"}`)
			},
			appendTo: func(dst []byte) ([]byte, error) {
				return appendSSEContentBlockDeltaInputJSON(dst, 6, `{"path":"a.txt","content":"he\"llo"}`)
			},
		},
		{
			name:    "text delta",
			marshal: func() ([]byte, error) { return marshalSSEContentBlockDeltaTextBytes(7, "hello\nworld") },
			appendTo: func(dst []byte) ([]byte, error) {
				return appendSSEContentBlockDeltaText(dst, 7, "hello\nworld")
			},
		},
		{
			name:    "thinking delta",
			marshal: func() ([]byte, error) { return appendSSEContentBlockDeltaThinking(nil, 8, "step <1>") },
			appendTo: func(dst []byte) ([]byte, error) {
				return appendSSEContentBlockDeltaThinking(dst, 8, "step <1>")
			},
		},
		{
			name:     "block stop",
			marshal:  func() ([]byte, error) { return marshalSSEContentBlockStopBytes(9) },
			appendTo: func(dst []byte) ([]byte, error) { return appendSSEContentBlockStop(dst, 9) },
		},
		{
			name:    "message delta",
			marshal: func() ([]byte, error) { return marshalSSEMessageDeltaBytes("tool_use\nnext", 42) },
			appendTo: func(dst []byte) ([]byte, error) {
				return appendSSEMessageDelta(dst, "tool_use\nnext", 42)
			},
		},
	}

	buf := make([]byte, 0, 256)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want, err := tt.marshal()
			testutil.NoError(t, err, "marshal: %v")
			got, err := tt.appendTo(buf[:0])
			testutil.NoError(t, err, "append: %v")
			testutil.Falsef(t, !bytes.Equal(got, want), "got=%s want=%s", got, want)
			buf = got[:0]
		})
	}
}

// newStreamTestHandler builds a stream handler over a flushing recorder for a
// test, and tears the logger and handler down with it.
func newStreamTestHandler(t *testing.T, streaming, toolMode bool, format adapter.ResponseFormat) (*streamHandler, *flushRecorder) {
	t.Helper()
	rec := newFlushRecorder()
	logger := debug.New(false, false)
	sh := newStreamHandler(&config.Config{DebugEnabled: false}, rec, logger, streaming, toolMode, format)
	t.Cleanup(func() { sh.release(); logger.Close() })
	return sh, rec
}

func TestStreamHandler_TextFlow_AnthropicSSE(t *testing.T) {
	sh, rec := newStreamTestHandler(t, false, true, adapter.FormatAnthropic)

	// seed a message_start so the stream resembles real output. For
	// "message_start" the live writer deliberately skips ensureMessageStartLocked,
	// so this writes exactly the one frame the retired writer wrote.
	sh.mu.Lock()
	sh.writeSSEBytesLockedWithHint("message_start", []byte(`{"type":"message_start"}`), true)
	sh.mu.Unlock()

	sh.handleMessage(upstream.SSEMessage{Type: "model", Event: map[string]any{"type": "text-start"}})
	sh.handleMessage(upstream.SSEMessage{Type: "model", Event: map[string]any{"type": "text-delta", "delta": "hi"}})
	sh.handleMessage(upstream.SSEMessage{Type: "model", Event: map[string]any{"type": "text-end"}})
	sh.handleMessage(upstream.SSEMessage{Type: "model", Event: map[string]any{"type": "finish", "finishReason": "stop"}})

	out := rec.buf.String()
	testutil.MustContain(t, out, "event: content_block_start")
	testutil.MustContain(t, out, "\"text\":\"hi\"")
	testutil.MustContain(t, out, "event: message_stop")
}

func TestStreamHandler_OpenAI_SendsDONEOnStop(t *testing.T) {
	sh, rec := newStreamTestHandler(t, false, true, adapter.FormatOpenAI)

	sh.finishResponse("end_turn")
	out := rec.buf.String()
	testutil.MustContain(t, out, "[DONE]")
}

func TestWriteSSEFrameBytes_Output(t *testing.T) {
	var buf bytes.Buffer
	testutil.NoError(t, writeSSEFrameBytes(&buf, "content_block_delta", []byte("{\"type\":\"content_block_delta\"}")), "writeSSEFrameBytes: %v")
	got := buf.String()
	want := "event: content_block_delta\ndata: {\"type\":\"content_block_delta\"}\n\n"
	testutil.Equal(t, got, want)
}

func TestWriteOpenAIFrame_Output(t *testing.T) {
	var buf bytes.Buffer
	testutil.NoError(t, writeOpenAIFrame(&buf, []byte("{\"id\":\"msg_1\"}")), "writeOpenAIFrame: %v")
	got := buf.String()
	want := "data: {\"id\":\"msg_1\"}\n\n"
	testutil.Equal(t, got, want)
}

func TestExtractThinkingSignature(t *testing.T) {
	e := map[string]any{"signature": "sig"}
	testutil.Equal(t, extractThinkingSignature(e), "sig")
	e2 := map[string]any{"data": map[string]any{"signature": "sig2"}}
	testutil.Equal(t, extractThinkingSignature(e2), "sig2")
}

func TestStreamHandler_CoalescesNonTextFlushes(t *testing.T) {
	sh, rec := newStreamTestHandler(t, false, true, adapter.FormatAnthropic)

	// The live writers take an explicit flush hint; writeSSEBytesLocked, the live
	// content_block_stop writer, derives its hint from shouldFlushSSEImmediately,
	// so these frames go through the same writer with the same hint source.
	writeFrame := func(event string, data []byte) {
		sh.mu.Lock()
		defer sh.mu.Unlock()
		sh.writeSSEBytesLockedWithHint(event, data, shouldFlushSSEImmediately(event, data))
	}

	writeFrame("message_start", []byte(`{"type":"message_start"}`))
	testutil.Equal(t, rec.flushes, 1)

	thinkingData, err := appendSSEContentBlockDeltaThinking(nil, 0, "step")
	testutil.NoError(t, err, "marshal thinking delta: %v")
	// The seed above wrote the opening frame without marking the stream started
	// (production's writeMessageStartLocked is what sets messageStartWritten), so
	// the first delta also emits the deferred opening frame and flushes it. The
	// first delta itself must also flush, without waiting for another frame.
	writeFrame("content_block_delta", thinkingData)
	testutil.Equal(t, rec.flushes, 3)

	// The remaining deferred thinking frames below the threshold add no flush.
	for i := 0; i < sseDeferredFlushFrameThreshold-1; i++ {
		writeFrame("content_block_delta", thinkingData)
	}
	testutil.Equal(t, rec.flushes, 3)

	// The frame that reaches sseDeferredFlushFrameThreshold flushes the batch.
	writeFrame("content_block_delta", thinkingData)
	testutil.Equal(t, rec.flushes, 4)

	textData, err := marshalSSEContentBlockDeltaTextBytes(0, "hi")
	testutil.NoError(t, err, "marshal text delta: %v")
	writeFrame("content_block_delta", textData)
	testutil.Equal(t, rec.flushes, 5)
}

func TestStreamHandler_FirstReasoningDeltaFlushesWithoutNextEvent(t *testing.T) {
	for _, format := range []adapter.ResponseFormat{adapter.FormatAnthropic, adapter.FormatOpenAI} {
		sh, rec := newStreamTestHandler(t, false, true, format)
		sh.handleMessage(upstream.SSEMessage{Type: "model", Event: map[string]any{"type": "reasoning-start"}})
		before := rec.flushes
		sh.handleMessage(upstream.SSEMessage{Type: "model", Event: map[string]any{"type": "reasoning-delta", "delta": "first thought"}})
		if rec.flushes <= before || !strings.Contains(rec.buf.String(), "first thought") || !sh.firstContentDeltaFlushed {
			t.Fatalf("format %v did not flush first reasoning delta: %s", format, rec.buf.String())
		}
	}
}

type shortFrameWriter struct{ flushRecorder }

func (w *shortFrameWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

func TestStreamHandler_FrameShortWriteCancelsUpstream(t *testing.T) {
	sh, _ := newStreamTestHandler(t, false, true, adapter.FormatAnthropic)
	w := &shortFrameWriter{}
	sh.w = w
	cancelled := false
	sh.cancelUpstream = func() { cancelled = true }
	sh.emitSSEFrameLocked("content_block_delta", []byte(`{"delta":"hello"}`), true, false)
	if !cancelled {
		t.Fatal("short write did not cancel upstream")
	}
	if err := sh.writeStreamFrameLocked("vendor_event", []byte(`{}`)); err != io.ErrShortWrite {
		t.Fatalf("short write = %v", err)
	}
}

func TestStreamHandler_FinishResponse_SuppressesGenericEmptyFallbackWhenRequested(t *testing.T) {
	sh, rec := newStreamTestHandler(t, false, true, adapter.FormatAnthropic)

	sh.finishResponse("end_turn")

	out := rec.buf.String()
	testutil.MustNotContain(t, out, "No output was presented to the user")
	testutil.MustContain(t, out, "event: message_stop")
}
