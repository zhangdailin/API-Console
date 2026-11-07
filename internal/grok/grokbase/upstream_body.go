package grokbase

import (
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// ErrStreamIdle is the sentinel for "the upstream stopped producing output".
// Callers classify it with errors.Is and answer each protocol with its own
// dedicated code instead of a generic stream read error.
var ErrStreamIdle = errors.New("upstream stream idle timeout")

// WrapByteIdle closes body when no byte arrives for idle. Closing the body
// unblocks the transport read without a connection-wide deadline; the read
// that observes it returns ErrStreamIdle.
func WrapByteIdle(body io.ReadCloser, idle time.Duration) io.ReadCloser {
	if body == nil || idle <= 0 {
		return body
	}
	r := &byteIdleReadCloser{inner: body, idle: idle}
	r.timer = time.AfterFunc(idle, func() { r.timedOut.Store(true); _ = r.closeInner() })
	return r
}

type byteIdleReadCloser struct {
	inner     io.ReadCloser
	idle      time.Duration
	timer     *time.Timer
	timedOut  atomic.Bool
	closeOnce sync.Once
	closeErr  error
}

func (r *byteIdleReadCloser) Read(p []byte) (int, error) {
	n, err := r.inner.Read(p)
	if n > 0 && !r.timedOut.Load() {
		r.timer.Reset(r.idle)
	}
	if err != nil && r.timedOut.Load() {
		return n, ErrStreamIdle
	}
	return n, err
}

func (r *byteIdleReadCloser) Close() error {
	if r.timer != nil {
		r.timer.Stop()
	}
	return r.closeInner()
}

func (r *byteIdleReadCloser) closeInner() error {
	r.closeOnce.Do(func() { r.closeErr = r.inner.Close() })
	return r.closeErr
}

// LeaseBody releases an egress lease when the upstream body is closed, on
// every exit path. Release runs once.
type LeaseBody struct {
	io.ReadCloser
	Release func()
}

func (b *LeaseBody) Close() error {
	err := b.ReadCloser.Close()
	if b.Release != nil {
		b.Release()
		b.Release = nil
	}
	return err
}
