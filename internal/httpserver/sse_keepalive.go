package httpserver

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/middleware"
)

const SSEKeepaliveInterval = 15 * time.Second

// SSEKeepalive writes comments only after a successful SSE response is committed.
// This preserves error responses written before the commit. All transport
// writes/flushes are serialized, and partial SSE frames are never interrupted.
type SSEKeepalive struct {
	http.ResponseWriter
	mu          sync.Mutex
	interval    time.Duration
	frame       string
	lastWrite   time.Time
	wroteHeader bool
	active      bool
	boundary    bool
	lineHasData bool
	err         error
	stop        chan struct{}
	done        chan struct{}
	stopOnce    sync.Once
	onError     func()
}

func NewSSEKeepalive(w http.ResponseWriter, interval time.Duration, onError func()) *SSEKeepalive {
	return newSSEKeepalive(w, interval, ": keepalive\n\n", onError)
}

// NewAnthropicSSEKeepalive emits ping events, which Messages clients use as
// stream activity even when they do not count SSE comments.
func NewAnthropicSSEKeepalive(w http.ResponseWriter, interval time.Duration, onError func()) *SSEKeepalive {
	return newSSEKeepalive(w, interval, "event: ping\ndata: {\"type\":\"ping\"}\n\n", onError)
}

func newSSEKeepalive(w http.ResponseWriter, interval time.Duration, frame string, onError func()) *SSEKeepalive {
	if interval <= 0 {
		interval = SSEKeepaliveInterval
	}
	s := &SSEKeepalive{ResponseWriter: w, interval: interval, frame: frame, boundary: true,
		stop: make(chan struct{}), done: make(chan struct{}), onError: onError}
	go s.run()
	return s
}

func (s *SSEKeepalive) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (s *SSEKeepalive) WriteHeader(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writeHeader(status)
}

func (s *SSEKeepalive) writeHeader(status int) {
	if s.wroteHeader {
		return
	}
	s.ResponseWriter.WriteHeader(status)
	if status < 200 {
		return
	}
	s.wroteHeader = true
	s.active = status < 300 && strings.Contains(strings.ToLower(s.Header().Get("Content-Type")), "text/event-stream")
	s.lastWrite = time.Now()
}

func (s *SSEKeepalive) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return 0, s.err
	}
	if !s.wroteHeader {
		s.writeHeader(http.StatusOK)
	}
	n, err := (DeadlineResponseWriter{ResponseWriter: s.ResponseWriter}).Write(p)
	if n > 0 {
		s.lastWrite = time.Now()
		for _, b := range p[:n] {
			if b == '\n' {
				s.boundary, s.lineHasData = !s.lineHasData, false
			} else if b != '\r' {
				s.boundary, s.lineHasData = false, true
			}
		}
	}
	s.fail(err)
	return n, err
}

func (s *SSEKeepalive) Flush() { _ = s.FlushError() }

func (s *SSEKeepalive) FlushError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if !s.wroteHeader {
		s.writeHeader(http.StatusOK)
	}
	err := (DeadlineResponseWriter{ResponseWriter: s.ResponseWriter}).FlushError()
	if !errors.Is(err, http.ErrNotSupported) {
		s.fail(err)
	}
	return err
}

func (s *SSEKeepalive) fail(err error) {
	if err != nil && s.err == nil {
		s.err = err
		middleware.MarkStreamFailure(s.ResponseWriter)
	}
}

func (s *SSEKeepalive) heartbeat(now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if !s.active || !s.boundary || now.Sub(s.lastWrite) < s.interval {
		return nil
	}
	target := DeadlineResponseWriter{ResponseWriter: s.ResponseWriter}
	err := writeAll(target, []byte(s.frame))
	if err == nil {
		err = target.FlushError()
		if errors.Is(err, http.ErrNotSupported) {
			err = nil
		}
	}
	s.lastWrite = now
	s.fail(err)
	return err
}

func (s *SSEKeepalive) run() {
	defer close(s.done)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case now := <-ticker.C:
			if s.heartbeat(now) != nil {
				if s.onError != nil {
					s.onError() // Close the upstream body to unblock its reader.
				}
				return
			}
		}
	}
}

// Close joins the heartbeat goroutine before the handler releases its writer.
// The underlying response writer remains available afterwards.
func (s *SSEKeepalive) Close() error {
	s.stopOnce.Do(func() { close(s.stop) })
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

var _ io.WriteCloser = (*SSEKeepalive)(nil)
