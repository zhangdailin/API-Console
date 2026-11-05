package egress

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Clearance metrics. Node selection, failure and recovery counters live with
// the shared pool (internal/grok/egresspool). Labels never carry proxy URLs,
// credentials, cookies, User-Agents, affinities or account identifiers.
var (
	grokEgressClearanceSolves = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "grok_web",
		Subsystem: "egress",
		Name:      "clearance_solve_total",
		Help:      "Cloudflare clearance solve attempts by result (success|failure).",
	}, []string{"result"})

	grokEgressClearanceInvalidations = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "grok_web",
		Subsystem: "egress",
		Name:      "clearance_invalidations_total",
		Help:      "Cloudflare clearance invalidations after confirmed challenges.",
	})
)

func recordClearanceSolve(success bool) {
	result := "failure"
	if success {
		result = "success"
	}
	grokEgressClearanceSolves.WithLabelValues(result).Inc()
}

func recordClearanceInvalidation() {
	grokEgressClearanceInvalidations.Inc()
}
