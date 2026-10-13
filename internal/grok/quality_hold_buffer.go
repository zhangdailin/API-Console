package grok

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"orchids-api/internal/middleware"
	"sync"
)

// deferredResponseWriter buffers a response until it is committed.
//
// Nothing reaches the client while a turn is held: not the status line, not the
// headers, not one body byte. That is what makes a withheld turn invisible, and
// therefore retryable. After Commit the writer is transparent, so a released
// stream stays realtime instead of being buffered to the end.
type deferredResponseWriter struct {
	target http.ResponseWriter

	mu        sync.Mutex
	header    http.Header
	status    int
	body      bytes.Buffer
	committed bool
	discarded bool
}

func newDeferredResponseWriter(target http.ResponseWriter) *deferredResponseWriter {
	if target != nil {
		target = deadlineResponseWriter{ResponseWriter: target}
	}
	writer := &deferredResponseWriter{target: target, header: make(http.Header), status: http.StatusOK}
	return writer
}

func (d *deferredResponseWriter) Header() http.Header {
	if d == nil {
		return http.Header{}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.committed && d.target != nil {
		return d.target.Header()
	}
	return d.header
}

func (d *deferredResponseWriter) WriteHeader(status int) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.committed || d.discarded {
		return
	}
	d.status = status
}

func (d *deferredResponseWriter) Write(p []byte) (int, error) {
	if d == nil {
		return len(p), nil
	}
	d.mu.Lock()
	if d.discarded {
		d.mu.Unlock()
		return len(p), nil
	}
	if d.committed {
		target := d.target
		d.mu.Unlock()
		return target.Write(p)
	}
	if d.body.Len()+len(p) > qualityHoldMaxBytes {
		// The cap is enforced by delivering early rather than by refusing the
		// write: the upstream turn is legitimate, it is just too large to hold.
		d.mu.Unlock()
		if err := d.Commit(); err != nil {
			return 0, err
		}
		return d.target.Write(p)
	}
	n, err := d.body.Write(p)
	d.mu.Unlock()
	return n, err
}

// Flush is a no-op while held. After Commit it flushes the target so a released
// SSE stream does not sit in the server's buffer.
func (d *deferredResponseWriter) Flush() {
	_ = d.FlushError()
}

func (d *deferredResponseWriter) FlushError() error {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	committed, discarded, target := d.committed, d.discarded, d.target
	d.mu.Unlock()
	if committed && !discarded && target != nil {
		return http.NewResponseController(target).Flush()
	}
	return nil
}

func (d *deferredResponseWriter) MarkStreamFailure() {
	d.mu.Lock()
	committed, target := d.committed, d.target
	d.mu.Unlock()
	if committed {
		middleware.MarkStreamFailure(target)
	}
}

// Commit writes everything that was held to the real response, then steps out of
// the way.
func (d *deferredResponseWriter) Commit() error {
	if d == nil || d.target == nil {
		return nil
	}
	d.mu.Lock()
	if d.committed {
		d.mu.Unlock()
		return nil
	}
	if d.discarded {
		d.mu.Unlock()
		return nil
	}
	header, status, body := d.header, d.status, d.body.Bytes()
	d.committed = true
	target := d.target
	d.mu.Unlock()

	for key, values := range header {
		target.Header().Del(key)
		for _, value := range values {
			target.Header().Add(key, value)
		}
	}
	target.WriteHeader(status)
	if len(body) > 0 {
		n, err := target.Write(body)
		if err != nil {
			return err
		}
		if n != len(body) {
			return io.ErrShortWrite
		}
	}
	if err := http.NewResponseController(target).Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	return nil
}

// Discard drops the held response. Every later write is swallowed, so a caller
// that keeps writing cannot accidentally reveal a withheld turn.
func (d *deferredResponseWriter) Discard() {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.discarded = true
	d.body.Reset()
	d.mu.Unlock()
}

func (d *deferredResponseWriter) Buffered() int {
	if d == nil {
		return 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.body.Len()
}

func (d *deferredResponseWriter) Status() int {
	if d == nil {
		return 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.status
}

// parkedResponse is a complete response that was withheld: status, headers and
// body, kept aside so the last attempt can still be delivered when the retry
// budget runs out under the fail-open policy.
type parkedResponse struct {
	header http.Header
	status int
	body   []byte
	// outcome is the accounting of the turn that was parked. It is what gets
	// journalled and billed if this turn ends up being the one delivered.
	outcome chatOutcome
}

// Park removes the held response from the writer without sending it. Later
// writes are swallowed, so a caller that keeps streaming cannot reveal the
// withheld turn by accident.
func (d *deferredResponseWriter) Park() *parkedResponse {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.committed || d.discarded {
		return nil
	}
	parked := &parkedResponse{
		header: d.header.Clone(),
		status: d.status,
		body:   append([]byte(nil), d.body.Bytes()...),
	}
	d.discarded = true
	d.body.Reset()
	return parked
}

// CommitTo delivers a parked response to the real client.
func (p *parkedResponse) CommitTo(w http.ResponseWriter) error {
	if p == nil || w == nil {
		return nil
	}
	for key, values := range p.header {
		w.Header().Del(key)
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	status := p.status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	if len(p.body) == 0 {
		return nil
	}
	n, err := (deadlineResponseWriter{ResponseWriter: w}).Write(p.body)
	if err == nil && n != len(p.body) {
		err = io.ErrShortWrite
	}
	return err
}

// isEmpty reports whether there is anything worth delivering.
func (p *parkedResponse) isEmpty() bool { return p == nil || len(p.body) == 0 }
