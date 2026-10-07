package httpclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync"
	"time"
)

// h2ConnectionLimits bounds actual sockets, including those still dialing or
// draining after GOAWAY. HTTP/2 stream concurrency remains peer-controlled.
type h2ConnectionLimits struct {
	mu    sync.Mutex
	max   int
	hosts map[string]*socketGate
}
type socketGate struct {
	slots chan struct{}
	users int
}

func (l *h2ConnectionLimits) acquire(ctx context.Context, host string) (func(), error) {
	l.mu.Lock()
	if l.hosts == nil {
		l.hosts = make(map[string]*socketGate)
	}
	g := l.hosts[host]
	if g == nil {
		g = &socketGate{slots: make(chan struct{}, max(1, l.max))}
		l.hosts[host] = g
	}
	g.users++
	l.mu.Unlock()
	drop := func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		g.users--
		if g.users == 0 {
			delete(l.hosts, host)
		}
	}
	select {
	case g.slots <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-g.slots; drop() }) }, nil
	case <-ctx.Done():
		drop()
		return nil, ctx.Err()
	}
}

type limitedH2Conn struct {
	net.Conn
	release func()
	once    sync.Once
}

func (c *limitedH2Conn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

var errBrowserHeaders = fmt.Errorf("browser http2 response headers timeout: %w", context.DeadlineExceeded)

// A header timer starts after request transmission, then stops before the body
// is handed to the caller. It must not become a total generation deadline.
func roundTripH2Headers(rt http.RoundTripper, req *http.Request, timeout time.Duration) (*http.Response, error) {
	if timeout <= 0 {
		return rt.RoundTrip(req)
	}
	ctx, cancel := context.WithCancelCause(req.Context())
	var mu sync.Mutex
	var timer *time.Timer
	finished := false
	trace := &httptrace.ClientTrace{WroteRequest: func(info httptrace.WroteRequestInfo) {
		if info.Err != nil {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if !finished && timer == nil {
			timer = time.AfterFunc(timeout, func() {
				mu.Lock()
				defer mu.Unlock()
				if !finished {
					cancel(errBrowserHeaders)
				}
			})
		}
	}}
	resp, err := rt.RoundTrip(req.WithContext(httptrace.WithClientTrace(ctx, trace)))
	mu.Lock()
	finished = true
	if timer != nil {
		timer.Stop()
	}
	mu.Unlock()
	if err != nil {
		if errors.Is(context.Cause(ctx), errBrowserHeaders) {
			err = errBrowserHeaders
		}
		cancel(nil)
		return resp, err
	}
	if context.Cause(ctx) != nil {
		cause := context.Cause(ctx)
		resp.Body.Close()
		cancel(nil)
		return nil, cause
	}
	resp.Body = &cancelOnCloseBody{ReadCloser: resp.Body, cancel: func() { cancel(nil) }}
	return resp, nil
}

type cancelOnCloseBody struct {
	io.ReadCloser
	cancel func()
}

func (b *cancelOnCloseBody) Close() error { defer b.cancel(); return b.ReadCloser.Close() }
