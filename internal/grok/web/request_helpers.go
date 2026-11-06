package web

import (
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"orchids-api/internal/grok/web/egress"
)

// upstreamIdleMode selects the activity model appropriate to an upstream.
type upstreamIdleMode uint8

const (
	upstreamIdleBytes upstreamIdleMode = iota
	upstreamIdleBuildSemantic
)

// doUpstreamHTTP owns response execution, decompression and stream idleness.
// Web uses byte activity; only Build understands Responses events well
// enough to ignore keepalives and control frames semantically.
func doUpstreamHTTP(req *http.Request, do func(*http.Request) (*http.Response, error), idle time.Duration, modes ...upstreamIdleMode) (*http.Response, error) {
	resp, err := do(req)
	if err != nil {
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
		} else {
			resp.Body = wrapByteIdle(resp.Body, idle)
		}
	}
	return resp, nil
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
	r.timer = time.AfterFunc(idle, func() {
		r.timedOut.Store(true)
		_ = r.closeInner()
	})
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

// egressOutcomeForKind maps a classified upstream error kind to the coarser
// node-health outcome. Persistent challenges degrade the node (it cannot serve
// this request); rate limits and account issues do not.
func egressOutcomeForKind(kind UpstreamErrorKind) egress.FeedbackOutcome {
	switch kind {
	case UpstreamErrorCloudflareChallenge, UpstreamErrorDPoPChallenge:
		return egress.OutcomeChallenge
	case UpstreamErrorRateLimited:
		return egress.OutcomeRateLimited
	case UpstreamErrorAccountBlock:
		return egress.OutcomeAccountBlock
	case UpstreamErrorGenericForbidden:
		return egress.OutcomeForbidden
	default:
		return egress.OutcomeTransportError
	}
}
