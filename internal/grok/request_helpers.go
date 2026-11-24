package grok

import (
	"context"
	"io"
	"net/http"
	"orchids-api/internal/grok/grokbase"
	"time"
)

// upstreamIdleMode selects the activity model appropriate to an upstream.
type upstreamIdleMode uint8

const (
	upstreamIdleBytes upstreamIdleMode = iota
	upstreamIdleBuildSemantic
)

// doUpstreamHTTP owns response execution, decompression and stream idleness.
// The Responses mode ignores keepalives and control frames for idle detection.
func doUpstreamHTTP(req *http.Request, do func(*http.Request) (*http.Response, error), idle time.Duration, modes ...upstreamIdleMode) (*http.Response, error) {
	resp, err := do(req)
	if err != nil {
		if context.Cause(req.Context()) == context.DeadlineExceeded {
			return nil, context.DeadlineExceeded
		}
		return nil, err
	}
	wrapIdle := grokbase.WrapByteIdle
	if len(modes) > 0 && modes[0] == upstreamIdleBuildSemantic {
		wrapIdle = wrapBuildSemanticIdle
	}
	if err := grokbase.PrepareUpstreamResponse(resp, idle, wrapIdle); err != nil {
		return nil, err
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
