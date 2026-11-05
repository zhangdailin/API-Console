package egresspool

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// metrics are the pool's Prometheus counters, one set per plane namespace.
// Labels are deliberately low cardinality: scope and outcome. They never carry
// proxy URLs, proxy credentials, cookies, OAuth/DPoP tokens, User-Agents,
// affinities or account identifiers.
type metrics struct {
	selections        *prometheus.CounterVec
	failures          *prometheus.CounterVec
	recoveries        *prometheus.CounterVec
	allNodesUnhealthy *prometheus.CounterVec
}

var (
	metricsMu   sync.Mutex
	metricsByNS = map[string]*metrics{}
)

// metricsFor registers a namespace's counters once; pools created later (a
// test, a reconfiguration) reuse them instead of re-registering.
func metricsFor(namespace string) *metrics {
	metricsMu.Lock()
	defer metricsMu.Unlock()
	if m, ok := metricsByNS[namespace]; ok {
		return m
	}
	m := &metrics{
		selections: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: "egress", Name: "node_selections_total",
			Help: "Egress node selections by scope.",
		}, []string{"scope"}),
		failures: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: "egress", Name: "node_failures_total",
			Help: "Egress node degradations by scope and reason (transport|server|challenge).",
		}, []string{"scope", "reason"}),
		recoveries: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: "egress", Name: "node_recoveries_total",
			Help: "Egress node recoveries after a successful response by scope.",
		}, []string{"scope"}),
		allNodesUnhealthy: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: "egress", Name: "all_nodes_unhealthy_total",
			Help: "Times no healthy node was available for a scope.",
		}, []string{"scope"}),
	}
	metricsByNS[namespace] = m
	return m
}
