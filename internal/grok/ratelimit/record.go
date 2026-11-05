package ratelimit

import (
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"orchids-api/internal/accountpolicy"
	"orchids-api/internal/util"
)

// Plane names the caller of Record: its identity prefix and its metrics
// namespace ("grok" for Build, "grok_web" for Web; unchanged from before the
// planes shared this package, so dashboards carry over).
type Plane struct {
	Provider  string
	Namespace string
}

// Record notes the cooldown an upstream 429 asks for, for identity (already
// prefixed with the plane's provider) and, when the body names one, the team.
// It returns nil for any other status.
func Record(plane Plane, identity, model string, status int, header http.Header, body []byte) *Metadata {
	if status != http.StatusTooManyRequests {
		return nil
	}
	meta := FromResponse(status, header, body)
	if meta == nil {
		meta = &Metadata{Scope: ScopeRPM, RetryAfter: time.Minute}
	}
	if retry := ParseRetryAfter(header.Get("Retry-After"), time.Now()); retry > 0 {
		meta.RetryAfter = retry
	}
	meta.RetryAfter = accountpolicy.BoundRateLimitCooldown(meta.RetryAfter)
	for _, target := range util.UniqueStrings([]string{util.FirstNonEmpty(model, meta.Model, "*"), meta.Model}) {
		Cooldowns.Note(meta.Scope, identity, target, meta.RetryAfter)
		if meta.TeamID != "" {
			Cooldowns.Note(meta.Scope, plane.Provider+":team:"+meta.TeamID, target, meta.RetryAfter)
		}
	}
	recordCooldownHit(plane.Namespace, meta)
	return meta
}

var (
	cooldownHitsMu sync.Mutex
	cooldownHits   = map[string]*prometheus.CounterVec{}
)

// recordCooldownHit counts an applied cooldown with a bounded model label so
// the metric does not explode with arbitrary model names.
func recordCooldownHit(namespace string, meta *Metadata) {
	if meta == nil {
		return
	}
	model := meta.Model
	if len(model) > 64 {
		model = model[:64]
	}
	cooldownHitsMu.Lock()
	counter, ok := cooldownHits[namespace]
	if !ok {
		counter = promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "ratelimit",
			Name:      "team_cooldown_hits_total",
			Help:      "Number of times a team+model cooldown was applied after an upstream 429.",
		}, []string{"scope", "model"})
		cooldownHits[namespace] = counter
	}
	cooldownHitsMu.Unlock()
	counter.WithLabelValues(string(meta.Scope), model).Inc()
}
