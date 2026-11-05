package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"orchids-api/internal/grok/grokbase"
	"orchids-api/internal/store"
)

// auditAttemptDiagnostic journals one upstream attempt. The event is built by
// grokbase; this plane supplies its error classification and error type.
func (h *Handler) auditAttemptDiagnostic(ctx context.Context, acc *store.Account, provider string, attempt int, started time.Time, err error, stage string, resp *http.Response, body []byte, result string) {
	logger := h.auditLoggerSnapshot()
	if logger == nil {
		return
	}
	kind := ""
	if err != nil {
		kind = fmt.Sprint(ClassifyUpstreamError(err))
	}
	logger.Log(ctx, grokbase.AttemptEvent(ctx, grokbase.Attempt{
		Account: acc, Provider: provider, Number: attempt, Started: started, Err: err, ErrorKind: kind,
		Stage: stage, Response: resp, Body: body, Result: result,
		Upstream:     upstreamFailure,
		IsUpstream:   func(err error) bool { _, ok := errors.AsType[*grokUpstreamError](err); return ok },
		MaxBodyBytes: maxUpstreamBodyBytes,
	}))
}

func upstreamFailure(err error) (grokbase.UpstreamFailure, bool) {
	upstream, ok := errors.AsType[*grokUpstreamError](err)
	if !ok || upstream == nil {
		return grokbase.UpstreamFailure{}, false
	}
	return grokbase.UpstreamFailure{Status: upstream.status, Header: upstream.header, Body: upstream.body}, true
}
