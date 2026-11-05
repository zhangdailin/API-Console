package grok

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Prometheus metrics for the Grok integration. Registered on first use via
// promauto so /metrics (already mounted in routes.go) picks them up.

var (
	grokCLIUpstreamStatus = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "grok",
		Subsystem: "cli",
		Name:      "upstream_status_total",
		Help:      "CLI upstream responses by status class.",
	}, []string{"status"})

	grokCLIOAuthRefreshes = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "grok",
		Subsystem: "cli",
		Name:      "oauth_refreshes_total",
		Help:      "Number of Build CLI OAuth refresh_token grants performed.",
	})

	// Response classification metrics. Labels carry only the challenge kind or
	// status class — never credentials, tokens, cookies, UA, affinity or node.

	grokEgressAcquireErrors = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "grok",
		Subsystem: "egress",
		Name:      "acquire_errors_total",
		Help:      "Times the egress manager failed to acquire a lease.",
	})

	grokGenericForbidden = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "grok",
		Subsystem: "upstream",
		Name:      "generic_forbidden_total",
		Help:      "Upstream 403 responses that are not an explicit account block or egress challenge.",
	})
)

func recordCLIUpstreamStatus(status int) {
	class := "2xx"
	switch {
	case status >= 500:
		class = "5xx"
	case status >= 400:
		class = "4xx"
	case status >= 300:
		class = "3xx"
	}
	grokCLIUpstreamStatus.WithLabelValues(class).Inc()
}

func recordCLIOAuthRefresh() { grokCLIOAuthRefreshes.Inc() }

func recordEgressAcquireError() { grokEgressAcquireErrors.Inc() }

func recordGenericForbidden() { grokGenericForbidden.Inc() }
