package grok

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"orchids-api/internal/util"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// upstreamIdleMode selects the activity model appropriate to an upstream.
type upstreamIdleMode uint8

const (
	upstreamIdleBytes upstreamIdleMode = iota
	upstreamIdleBuildSemantic
)

// doUpstreamHTTP owns response execution, decompression and stream idleness.
// Build uses byte activity; only Build understands Responses events well
// enough to ignore keepalives and control frames semantically.
func doUpstreamHTTP(req *http.Request, do func(*http.Request) (*http.Response, error), idle time.Duration, modes ...upstreamIdleMode) (*http.Response, error) {
	resp, err := do(req)
	if err != nil {
		if context.Cause(req.Context()) == context.DeadlineExceeded {
			return nil, context.DeadlineExceeded
		}
		return nil, err
	}
	if err := decodeHTTPResponseBody(resp); err != nil {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("grok upstream decode failed: %w", err)
	}
	if idle > 0 && resp.StatusCode >= 200 && resp.StatusCode < 300 && resp.Body != nil {
		mode := upstreamIdleBytes
		if len(modes) > 0 {
			mode = modes[0]
		}
		if mode == upstreamIdleBuildSemantic {
			resp.Body = wrapBuildSemanticIdle(resp.Body, idle)
			if monitored, ok := resp.Body.(*semanticIdleReadCloser); ok {
				monitored.generationContext = req.Context()
			}
		} else {
			resp.Body = wrapByteIdle(resp.Body, idle)
		}
	}
	// JSON upstreams reveal no incremental generation progress. Do not apply
	// a streaming first-token deadline to their active response body.
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		util.MarkGenerationProgress(req.Context())
	}
	if resp.Body != nil {
		resp.Body = &deadlineCauseBody{ReadCloser: resp.Body, ctx: req.Context()}
	}
	return resp, nil
}

type deadlineCauseBody struct {
	io.ReadCloser
	ctx context.Context
}

func (b *deadlineCauseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil && context.Cause(b.ctx) == context.DeadlineExceeded {
		return n, context.DeadlineExceeded
	}
	return n, err
}

// byteIdleReadCloser resets its deadline for every upstream byte. Closing the
// body unblocks HTTP transport reads without imposing a connection-wide limit.
type byteIdleReadCloser struct {
	inner     io.ReadCloser
	idle      time.Duration
	timer     *time.Timer
	timedOut  atomic.Bool
	closeOnce sync.Once
	closeErr  error
}

func wrapByteIdle(body io.ReadCloser, idle time.Duration) io.ReadCloser {
	if body == nil || idle <= 0 {
		return body
	}
	r := &byteIdleReadCloser{inner: body, idle: idle}
	r.timer = time.AfterFunc(idle, func() { r.timedOut.Store(true); _ = r.closeInner() })
	return r
}

func (r *byteIdleReadCloser) Read(p []byte) (int, error) {
	n, err := r.inner.Read(p)
	if n > 0 && !r.timedOut.Load() {
		r.timer.Reset(r.idle)
	}
	if err != nil && r.timedOut.Load() {
		return n, ErrGrokSemanticIdle
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

func (r *byteIdleReadCloser) TimedOut() bool { return r.timedOut.Load() }

// leaseResponseBody wraps an upstream response body so the egress lease is
// released when the body is closed (or discarded), on every exit path.
type leaseResponseBody struct {
	io.ReadCloser
	release func()
}

func (b *leaseResponseBody) Close() error {
	err := b.ReadCloser.Close()
	if b.release != nil {
		b.release()
		b.release = nil
	}
	return err
}
