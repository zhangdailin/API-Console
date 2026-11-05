package web

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Prometheus metrics for the Grok integration. Registered on first use via
// promauto so /metrics (already mounted in routes.go) picks them up.

var (
	grokUpstreamChallenges = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "grok_web",
		Subsystem: "upstream",
		Name:      "challenges_total",
		Help:      "Upstream challenges classified by kind (cloudflare|dpop).",
	}, []string{"kind"})

	grokEgressAcquireErrors = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "grok_web",
		Subsystem: "egress",
		Name:      "acquire_errors_total",
		Help:      "Times the egress manager failed to acquire a lease.",
	})

	grokGenericForbidden = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "grok_web",
		Subsystem: "upstream",
		Name:      "generic_forbidden_total",
		Help:      "Upstream 403 responses that are not an explicit account block or egress challenge.",
	})
)

func recordUpstreamChallenge(kind string) {
	if kind == "" {
		kind = "unknown"
	}
	grokUpstreamChallenges.WithLabelValues(kind).Inc()
}

func recordEgressAcquireError() {
	grokEgressAcquireErrors.Inc()
}

func recordGenericForbidden() {
	grokGenericForbidden.Inc()
}
