package middleware

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"orchids-api/internal/testutil"
	"sync"
	"testing"
	"time"
)

type failingFlushRecorder struct {
	*httptest.ResponseRecorder
	flushes int
	err     error
}

func (w *failingFlushRecorder) FlushError() error {
	w.flushes++
	if w.flushes > 1 {
		return w.err
	}
	return nil
}

func TestCoalescedTimerFailureCancelsRequestAndRemainsSticky(t *testing.T) {
	failure := errors.New("flush disconnected")
	target := &failingFlushRecorder{ResponseRecorder: httptest.NewRecorder(), err: failure}
	trace := NewTracedResponseWriter(target)
	h := CoalesceStreamFlush(func() time.Duration { return 2 * time.Millisecond })(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("data: first\n\n"))
		w.(http.Flusher).Flush()
		w.Write([]byte("data: sparse\n\n"))
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
			t.Fatal("timer failure did not stop upstream")
		}
		if err := http.NewResponseController(w).Flush(); !errors.Is(err, failure) {
			t.Fatalf("flush error lost: %v", err)
		}
		if _, err := w.Write([]byte("late")); !errors.Is(err, failure) {
			t.Fatalf("write error lost: %v", err)
		}
	})
	h(trace, httptest.NewRequest("POST", "/", nil))
	if !trace.StreamFailed() || target.flushes != 2 || target.Body.String() != "data: first\n\ndata: sparse\n\n" {
		t.Fatalf("marked=%v flushes=%d body=%q", trace.StreamFailed(), target.flushes, target.Body.String())
	}
}

type coalescedShortWriter struct{ *httptest.ResponseRecorder }

func (w coalescedShortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

func TestCoalescedShortWriteAndWrappedFailureAreObservable(t *testing.T) {
	trace := NewTracedResponseWriter(coalescedShortWriter{httptest.NewRecorder()})
	w := &coalescedWriter{ResponseWriter: trace, interval: time.Hour}
	defer w.close()
	if _, err := w.Write([]byte("frame")); !errors.Is(err, io.ErrShortWrite) || !trace.StreamFailed() {
		t.Fatalf("err=%v marked=%v", err, trace.StreamFailed())
	}
	trace = NewTracedResponseWriter(httptest.NewRecorder())
	w = &coalescedWriter{ResponseWriter: trace, interval: time.Hour}
	MarkStreamFailure(w)
	if !trace.StreamFailed() {
		t.Fatal("coalescing hid the stream failure marker")
	}
}

type flushRecorder struct {
	*httptest.ResponseRecorder
	mu      sync.Mutex
	flushes int
	signal  chan struct{}
}

func (w *flushRecorder) Flush() {
	w.mu.Lock()
	w.flushes++
	w.mu.Unlock()
	if w.signal != nil {
		select {
		case w.signal <- struct{}{}:
		default:
		}
	}
}
func TestCoalescedFlushKeepsFirstTerminalAndFinalDrain(t *testing.T) {
	target := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	writer := &coalescedWriter{ResponseWriter: target, interval: time.Hour}
	writer.Write([]byte("data: first\n\n"))
	writer.Flush()
	for range 10 {
		writer.Write([]byte("data: delta\n\n"))
		writer.Flush()
	}
	testutil.Equal(t, target.flushes, 1)
	writer.Write([]byte("data: [DONE]\n\n"))
	writer.Flush()
	testutil.Equal(t, target.flushes, 2)
	writer.Write([]byte("data: trailing\n\n"))
	writer.Flush()
	writer.close()
	testutil.Equal(t, target.flushes, 3)
}
func TestCoalescedFlushDoesNotHoldSparseDeltaUntilNextToken(t *testing.T) {
	target := &flushRecorder{ResponseRecorder: httptest.NewRecorder(), signal: make(chan struct{}, 4)}
	writer := &coalescedWriter{ResponseWriter: target, interval: 2 * time.Millisecond}
	defer writer.close()
	writer.Write([]byte("data: first\n\n"))
	writer.Flush()
	<-target.signal
	writer.Write([]byte("data: second\n\n"))
	writer.Flush()
	select {
	case <-target.signal:
	case <-time.After(time.Second):
		t.Fatal("sparse delta stuck")
	}
	writer.close()
	target.mu.Lock()
	before := target.flushes
	target.mu.Unlock()
	time.Sleep(5 * time.Millisecond)
	target.mu.Lock()
	defer target.mu.Unlock()
	testutil.Equal(t, target.flushes, before)
}
func TestCoalescingLeavesNonStreamingResponseUnchanged(t *testing.T) {
	rec := httptest.NewRecorder()
	h := CoalesceStreamFlush(func() time.Duration { return time.Millisecond })(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(400); w.Write([]byte("bad")) })
	h(rec, httptest.NewRequest("POST", "/", nil))
	testutil.Falsef(t, rec.Code != 400 || rec.Body.String() != "bad", "response %d %s", rec.Code, rec.Body.String())
}
