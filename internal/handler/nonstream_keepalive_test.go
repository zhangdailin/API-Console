package handler

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/adapter"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/testutil"
)

// headerCountingRecorder counts WriteHeader calls: a keep-alive opens the
// response implicitly, and a later WriteHeader would be superfluous.
type headerCountingRecorder struct {
	*flushRecorder
	headerCalls int
}

func (r *headerCountingRecorder) WriteHeader(code int) {
	r.headerCalls++
	r.flushRecorder.WriteHeader(code)
}

func newNonStreamHandler(t *testing.T, waited time.Duration) (*streamHandler, *headerCountingRecorder) {
	t.Helper()
	rec := &headerCountingRecorder{flushRecorder: newFlushRecorder()}
	sh := newStreamHandler(&config.Config{}, rec, debug.New(false, false), false, adapter.FormatAnthropic)
	t.Cleanup(sh.release)
	sh.pendingModel = "qwen3.8-flash"
	sh.nonStreamKeepAlive = true
	sh.startTime = time.Now().Add(-waited)
	return sh, rec
}

// TestNonStreamKeepAliveOpensASilentAnswer: a non-streaming answer silent for
// longer than nonStreamOpenAfter is kept alive with whitespace, and the JSON
// answer that follows still parses. Cloudflare ended such requests with 524
// at 120 seconds.
func TestNonStreamKeepAliveOpensASilentAnswer(t *testing.T) {
	sh, rec := newNonStreamHandler(t, nonStreamOpenAfter+time.Second)
	sh.writeKeepAlive()
	sh.writeKeepAlive()
	testutil.Equal(t, rec.buf.String(), string(nonStreamOpenPadding)+"\n")
	testutil.False(t, rec.flushes < 2, "keep-alives were not flushed")

	// finishMessages writes the answer with json.NewEncoder(w).Encode after the
	// keep-alive whitespace; the whole body must remain one JSON value.
	testutil.NoError(t, json.NewEncoder(rec).Encode(map[string]any{"type": "message", "content": []any{map[string]any{"type": "text", "text": "hello"}}}))
	body := rec.buf.Bytes()
	testutil.False(t, len(bytes.TrimSpace(body)) == len(body), "the keep-alive whitespace is missing")
	var message map[string]any
	testutil.NoError(t, json.Unmarshal(body, &message))
	testutil.Equal(t, message["type"], any("message"))
}

func TestNonStreamKeepAliveWaitsBeforeOpening(t *testing.T) {
	sh, rec := newNonStreamHandler(t, nonStreamOpenAfter-5*time.Second)
	sh.writeKeepAlive()
	testutil.Equal(t, rec.buf.Len(), 0)

	disabled, disabledRec := newNonStreamHandler(t, time.Hour)
	disabled.nonStreamKeepAlive = false
	disabled.writeKeepAlive()
	testutil.Equal(t, disabledRec.buf.Len(), 0)
}

// TestNonStreamFailureAfterOpeningWritesOnlyTheBody: the status is committed
// by then; the JSON error still reaches the client after the whitespace.
func TestNonStreamFailureAfterOpeningWritesOnlyTheBody(t *testing.T) {
	sh, rec := newNonStreamHandler(t, nonStreamOpenAfter+time.Second)
	sh.writeKeepAlive()
	sh.reportRequestFailure("queue refused", "upstream_queue", "Qoder model queue unavailable", 30*time.Second)
	testutil.Equal(t, rec.headerCalls, 0)
	body := rec.buf.String()
	testutil.MustContain(t, body, "Qoder model queue unavailable")
	var parsed map[string]any
	testutil.NoError(t, json.Unmarshal([]byte(body), &parsed))
	testutil.False(t, !strings.HasPrefix(body, " "), "the error was not written after the keep-alive")
	// No keep-alive after the answer is final.
	before := rec.buf.Len()
	sh.writeKeepAlive()
	testutil.Equal(t, rec.buf.Len(), before)
}
