package web

import (
	"fmt"
	"net/http"
	"time"

	"orchids-api/internal/grok/grokbase"
	"orchids-api/internal/grok/web/egress"
	"orchids-api/internal/httpclient"
)

// doUpstreamHTTP owns response execution, decompression and stream idleness.
// Web uses byte activity; only Build understands Responses events well
// enough to ignore keepalives and control frames semantically.
func doUpstreamHTTP(req *http.Request, do func(*http.Request) (*http.Response, error), idle time.Duration) (*http.Response, error) {
	resp, err := do(req)
	if err != nil {
		return nil, err
	}
	if err := httpclient.DecodeContentEncoding(resp); err != nil {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("grok upstream decode failed: %w", err)
	}
	if idle > 0 && resp.StatusCode >= 200 && resp.StatusCode < 300 && resp.Body != nil {
		resp.Body = grokbase.WrapByteIdle(resp.Body, idle)
	}
	return resp, nil
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

// ErrGrokSemanticIdle reports that the upstream stopped producing output.
var ErrGrokSemanticIdle = grokbase.ErrStreamIdle
