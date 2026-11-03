package web

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Prometheus metrics for the Grok integration. Registered on first use via
// promauto so /metrics (already mounted in routes.go) picks them up.

var (
	grokTeamCooldownHits = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "grok_web",
		Subsystem: "ratelimit",
		Name:      "team_cooldown_hits_total",
		Help:      "Number of times a team+model cooldown was applied after an upstream 429.",
	}, []string{"scope", "model"})

	// Response classification metrics. Labels carry only the challenge kind or
	// status class — never credentials, tokens, cookies, UA, affinity or node.
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

// recordTeamCooldownHit increments the team cooldown counter with a bounded
// model label so the metric does not explode with arbitrary model names.
func recordTeamCooldownHit(meta *RateLimitMetadata) {
	if meta == nil {
		return
	}
	model := meta.Model
	if len(model) > 64 {
		model = model[:64]
	}
	grokTeamCooldownHits.WithLabelValues(string(meta.Scope), model).Inc()
}

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
