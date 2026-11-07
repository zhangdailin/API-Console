package grok

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"orchids-api/internal/grok/grokbase"
	"orchids-api/internal/httpclient"
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
	if err := httpclient.DecodeContentEncoding(resp); err != nil {
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
			resp.Body = grokbase.WrapByteIdle(resp.Body, idle)
		}
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
