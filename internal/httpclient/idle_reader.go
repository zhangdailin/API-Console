package httpclient

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// idleSentinels caches one sentinel per label. A channel that wants to
// recognise the condition with errors.Is rather than by matching prose gets the
// same pointer the monitor uses, so identity comparison still holds.
var idleSentinels sync.Map

// ErrStreamIdle returns the idle-timeout error for one channel label. The label
// is what an operator reads, and the value is stable per label so errors.Is
// works from the channel package as well as from here.
func ErrStreamIdle(label string) error {
	if label == "" {
		label = "upstream"
	}
	if cached, ok := idleSentinels.Load(label); ok {
		return cached.(error)
	}
	sentinel := errors.New(label + " stream idle timeout")
	actual, _ := idleSentinels.LoadOrStore(label, sentinel)
	return actual.(error)
}

// MonitorReadIdle interrupts a response body that produces no bytes for the
// configured window. Active long-running streams are unaffected.
func MonitorReadIdle(body io.ReadCloser, idle time.Duration, cancel context.CancelFunc, label string) io.ReadCloser {
	if body == nil || idle <= 0 || cancel == nil {
		return body
	}
	monitored := &readIdleBody{
		body: body, cancel: cancel, idle: idle,
		timeoutErr: ErrStreamIdle(label), done: make(chan struct{}),
	}
	go monitored.watch()
	return monitored
}

type readIdleBody struct {
	progressMu   sync.Mutex
	progressOnly bool
	readStarted  time.Time
	readElapsed  time.Duration
	body         io.ReadCloser
	cancel       context.CancelFunc
	idle         time.Duration
	timeoutErr   error
	lastRead     atomic.Int64
	timedOut     atomic.Bool
	done         chan struct{}
	once         sync.Once
}

// MonitorProgressIdle lets the parser reset the budget only for useful events.
// SSE comments and heartbeat bytes cannot keep a stalled generation alive.
func MonitorProgressIdle(body io.ReadCloser, idle time.Duration, cancel context.CancelFunc, label string) (io.ReadCloser, func()) {
	if body == nil || idle <= 0 || cancel == nil {
		return body, func() {}
	}
	b := &readIdleBody{body: body, cancel: cancel, idle: idle, timeoutErr: ErrStreamIdle(label), done: make(chan struct{}), progressOnly: true}
	go b.watch()
	return b, func() {
		b.progressMu.Lock()
		defer b.progressMu.Unlock()
		b.readElapsed = 0
		if !b.readStarted.IsZero() {
			b.readStarted = time.Now()
		}
	}
}

func (b *readIdleBody) Read(p []byte) (int, error) {
	// Only wait time inside Read counts. A slow downstream must not expire an
	// upstream stream while its consumer is processing the previous frame.
	if b.progressOnly {
		b.progressMu.Lock()
		b.readStarted = time.Now()
		b.progressMu.Unlock()
		defer func() {
			b.progressMu.Lock()
			b.readElapsed += time.Since(b.readStarted)
			b.readStarted = time.Time{}
			b.progressMu.Unlock()
		}()
	} else {
		b.lastRead.Store(time.Now().UnixNano())
		defer b.lastRead.Store(0)
	}
	n, err := b.body.Read(p)
	if err != nil && b.timedOut.Load() {
		return n, b.timeoutErr
	}
	return n, err
}

func (b *readIdleBody) Close() error {
	b.once.Do(func() { close(b.done) })
	return b.body.Close()
}

func (b *readIdleBody) watch() {
	interval := b.idle / 4
	if interval < 10*time.Millisecond {
		interval = 10 * time.Millisecond
	}
	if interval > time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			stalled := false
			if b.progressOnly {
				b.progressMu.Lock()
				stalled = !b.readStarted.IsZero() && b.readElapsed+time.Since(b.readStarted) >= b.idle
				b.progressMu.Unlock()
			} else if started := b.lastRead.Load(); started != 0 {
				stalled = time.Since(time.Unix(0, started)) >= b.idle
			}
			if stalled {
				b.timedOut.Store(true)
				b.cancel()
				return
			}
		case <-b.done:
			return
		}
	}
}
