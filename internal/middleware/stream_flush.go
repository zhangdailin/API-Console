package middleware

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"
)

// CoalesceStreamFlush keeps the first frame and terminal events immediate.
// A bounded timer flushes sparse trailing deltas even if upstream goes idle.
func CoalesceStreamFlush(interval func() time.Duration) func(http.HandlerFunc) http.HandlerFunc {
	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			delay := interval()
			if delay <= 0 {
				next(w, r)
				return
			}
			if _, ok := w.(http.Flusher); !ok {
				next(w, r)
				return
			}
			ctx, cancel := context.WithCancel(r.Context())
			defer cancel()
			writer := &coalescedWriter{ResponseWriter: w, interval: delay, cancel: cancel}
			defer writer.close()
			next(writer, r.WithContext(ctx))
		}
	}
}

type coalescedWriter struct {
	http.ResponseWriter
	mu               sync.Mutex
	interval         time.Duration
	timer            *time.Timer
	last             time.Time
	pending          int
	terminal, closed bool
	err              error
	cancel           context.CancelFunc
	timerGeneration  uint64
}

func (w *coalescedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *coalescedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return 0, w.err
	}
	if w.closed {
		return 0, io.ErrClosedPipe
	}
	n, err := w.ResponseWriter.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	if err != nil {
		w.failLocked(err)
		return n, err
	}
	w.pending += n
	if bytes.Contains(p, []byte("[DONE]")) || bytes.Contains(p, []byte("response.completed")) || bytes.Contains(p, []byte("response.incomplete")) || bytes.Contains(p, []byte("response.failed")) || bytes.Contains(p, []byte("message_stop")) || bytes.Contains(p, []byte("event: error")) {
		w.terminal = true
	}
	return n, err
}
func (w *coalescedWriter) WriteHeader(status int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ResponseWriter.WriteHeader(status)
}
func (w *coalescedWriter) Flush() {
	_ = w.FlushError()
}

func (w *coalescedWriter) FlushError() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return w.err
	}
	if w.closed {
		return io.ErrClosedPipe
	}
	if w.last.IsZero() || w.terminal || w.pending >= 2048 || time.Since(w.last) >= w.interval {
		w.flushLocked()
		return w.err
	}
	if w.pending > 0 && w.timer == nil {
		w.timerGeneration++
		generation := w.timerGeneration
		w.timer = time.AfterFunc(w.interval-time.Since(w.last), func() {
			w.mu.Lock()
			defer w.mu.Unlock()
			if generation != w.timerGeneration {
				return
			}
			w.timer = nil
			if !w.closed && w.pending > 0 {
				w.flushLocked()
			}
		})
	}
	return nil
}

// The timer and the handler must not access the transport concurrently.
// ResponseController would otherwise unwrap this writer and bypass its lock.
func (w *coalescedWriter) SetWriteDeadline(deadline time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return w.err
	}
	return http.NewResponseController(w.ResponseWriter).SetWriteDeadline(deadline)
}

func (w *coalescedWriter) failLocked(err error) {
	if w.err == nil {
		w.err = err
		MarkStreamFailure(w.ResponseWriter)
		if w.cancel != nil {
			w.cancel()
		}
	}
	if w.timer != nil {
		w.timerGeneration++
		w.timer.Stop()
		w.timer = nil
	}
	w.pending = 0
	w.terminal = false
}
func (w *coalescedWriter) flushLocked() {
	if w.timer != nil {
		w.timerGeneration++
		w.timer.Stop()
		w.timer = nil
	}
	if err := http.NewResponseController(w.ResponseWriter).Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		w.failLocked(err)
		return
	}
	if w.pending > 0 {
		w.last = time.Now()
	}
	w.pending = 0
	w.terminal = false
}
func (w *coalescedWriter) close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	if w.timer != nil {
		w.timerGeneration++
		w.timer.Stop()
		w.timer = nil
	}
	if w.pending > 0 && !w.last.IsZero() {
		w.flushLocked()
	}
}
