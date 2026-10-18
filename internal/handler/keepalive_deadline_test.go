package handler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/adapter"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/upstream"
)

// Model a transport whose previous content-write deadline has expired while
// the upstream is thinking. A heartbeat needs a fresh deadline of its own.
type heartbeatDeadlineWriter struct {
	*flushRecorder
	deadline time.Time
}

func (w *heartbeatDeadlineWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadline = deadline
	return nil
}
func (w *heartbeatDeadlineWriter) Write(p []byte) (int, error) {
	if !w.deadline.IsZero() && time.Now().After(w.deadline) {
		return 0, os.ErrDeadlineExceeded
	}
	return w.flushRecorder.Write(p)
}
func TestKeepAliveRefreshesTransportWriteDeadline(t *testing.T) {
	writer := &heartbeatDeadlineWriter{flushRecorder: newFlushRecorder()}
	logger := debug.New(false, false)
	defer logger.Close()
	h := newStreamHandler(&config.Config{}, writer, logger, true, adapter.FormatAnthropic)
	defer h.release()
	h.handleMessage(upstream.SSEMessage{Type: "model.text-delta", Event: map[string]any{"delta": "First part"}})
	// Simulate elapsed time without a 30-second test sleep.
	writer.deadline = time.Now().Add(-time.Second)
	h.writeKeepAlive()
	_, failed := h.terminalState()
	if failed || !writer.deadline.After(time.Now()) {
		t.Fatalf("heartbeat used expired deadline: failed=%v deadline=%v", failed, writer.deadline)
	}
	h.handleMessage(upstream.SSEMessage{Type: "model.text-delta", Event: map[string]any{"delta": " final part"}})
	h.handleMessage(upstream.SSEMessage{Type: "model.finish", Event: map[string]any{"finishReason": "end_turn"}})
	returned, failed := h.terminalState()
	if !returned || failed || h.finalStopReason != "end_turn" {
		t.Fatalf("response cut after heartbeat: returned=%v failed=%v reason=%s", returned, failed, h.finalStopReason)
	}
}

// HTTP/2 expires its stream timer even without another application write.
// Scale only the old deadline down; a heartbeat must replace it before it fires.
func TestKeepAlivePreventsHTTP2StreamTimeout(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger := debug.New(false, false)
		defer logger.Close()
		h := newStreamHandler(&config.Config{}, w, logger, true, adapter.FormatAnthropic)
		defer h.release()
		h.handleMessage(upstream.SSEMessage{Type: "model.text-delta", Event: map[string]any{"delta": "first"}})
		if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
			t.Error(err)
			return
		}
		h.writeKeepAlive()
		timer := time.NewTimer(250 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-r.Context().Done():
			return
		}
		h.handleMessage(upstream.SSEMessage{Type: "model.text-delta", Event: map[string]any{"delta": "last"}})
		h.handleMessage(upstream.SSEMessage{Type: "model.finish", Event: map[string]any{"finishReason": "end_turn"}})
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	client := server.Client()
	client.Timeout = 3 * time.Second
	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.ProtoMajor != 2 {
		t.Fatalf("test requires HTTP/2, got %s", resp.Proto)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("heartbeat failed to keep stream alive: %v", err)
	}
	if !strings.Contains(string(body), `"text":"last"`) || !strings.Contains(string(body), "event: message_stop") {
		t.Fatalf("response cut: %s", body)
	}
}
