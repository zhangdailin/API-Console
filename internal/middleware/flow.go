package middleware

import (
	"context"
	"net/http"
	"sync"
	"time"

	"orchids-api/internal/channel"
)

// FlowTracker counts admitted inference handlers on this gateway instance. It
// retains only fixed provider/protocol counters, never request bodies or keys.
type FlowTracker struct {
	mu          sync.Mutex
	startedAt   time.Time
	routes      map[string]*FlowRoute
	subscribers map[chan struct{}]struct{}
}

type FlowRoute struct {
	Channel        string  `json:"channel"`
	Protocol       string  `json:"protocol"`
	Active         int64   `json:"active"`
	Started        int64   `json:"started"`
	Completed      int64   `json:"completed"`
	QPS            float64 `json:"qps"`
	TPS            float64 `json:"tps"`
	UsageAvailable bool    `json:"usage_available"`
	InputTokens    int64   `json:"input_tokens"`
	OutputTokens   int64   `json:"output_tokens"`
	buckets        [5]flowBucket
}

type flowUsageKey struct{}

type flowBucket struct {
	second                         int64
	requests, tokens, usageSamples int64
}

const flowRateSeconds = 5

func (route *FlowRoute) bucket(now time.Time) *flowBucket {
	second := now.Unix()
	bucket := &route.buckets[second%flowRateSeconds]
	if bucket.second != second {
		*bucket = flowBucket{second: second}
	}
	return bucket
}

type FlowSnapshot struct {
	Available       bool        `json:"available"`
	InstanceStarted string      `json:"instance_started"`
	SampledAt       string      `json:"sampled_at"`
	Routes          []FlowRoute `json:"routes"`
	RateSeconds     int         `json:"rate_seconds"`
}

func NewFlowTracker() *FlowTracker {
	t := &FlowTracker{startedAt: time.Now(), routes: make(map[string]*FlowRoute), subscribers: make(map[chan struct{}]struct{})}
	for _, def := range channel.All() {
		for _, protocol := range []string{"chat", "responses", "messages"} {
			key := string(def.ID) + ":" + protocol
			t.routes[key] = &FlowRoute{Channel: string(def.ID), Protocol: protocol}
		}
	}
	return t
}

func (t *FlowTracker) Track(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, endpoint, ok := channel.EndpointFromPath(r.URL.Path)
		protocol := ""
		if r.Method == http.MethodPost && ok {
			switch endpoint {
			case "chat/completions":
				protocol = "chat"
			case "responses", "responses/", "responses/compact":
				protocol = "responses"
			case "messages":
				protocol = "messages"
			}
		}
		if protocol == "" {
			next(w, r)
			return
		}
		key := string(id) + ":" + protocol
		t.mu.Lock()
		route := t.routes[key]
		route.Active++
		route.Started++
		route.bucket(time.Now()).requests++
		t.notifyLocked()
		t.mu.Unlock()

		// The existing audit observation supplies authoritative cumulative usage;
		// deltas avoid double counting a final row that repeats an attempt's usage.
		var input, output int64
		var hasUsage bool
		observeUsage := func() {
			box, ok := r.Context().Value(requestObservationKey{}).(*requestObservation)
			if !ok {
				return
			}
			box.mu.Lock()
			currentInput, currentOutput, available := box.reportedInput, box.reportedOutput, box.reportedUsage
			box.mu.Unlock()
			if !available {
				return
			}
			t.mu.Lock()
			bucket := route.bucket(time.Now())
			if !hasUsage {
				bucket.usageSamples++
				hasUsage = true
			}
			if currentInput > input {
				delta := currentInput - input
				route.InputTokens += delta
				bucket.tokens += delta
				input = currentInput
			}
			if currentOutput > output {
				delta := currentOutput - output
				route.OutputTokens += delta
				bucket.tokens += delta
				output = currentOutput
			}
			t.notifyLocked()
			t.mu.Unlock()
		}
		r = r.WithContext(context.WithValue(r.Context(), flowUsageKey{}, observeUsage))
		defer func() {
			observeUsage()
			t.mu.Lock()
			route.Active--
			route.Completed++
			t.notifyLocked()
			t.mu.Unlock()
		}()
		next(w, r)
	}
}

func (t *FlowTracker) Snapshot(scope string) FlowSnapshot {
	snapshot := FlowSnapshot{Routes: []FlowRoute{}}
	if t == nil {
		return snapshot
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	snapshot.Available = true
	snapshot.InstanceStarted = t.startedAt.UTC().Format(time.RFC3339Nano)
	now := time.Now()
	snapshot.SampledAt = now.UTC().Format(time.RFC3339Nano)
	snapshot.RateSeconds = flowRateSeconds
	for _, def := range channel.All() {
		if scope != "" && scope != string(def.ID) {
			continue
		}
		for _, protocol := range []string{"chat", "responses", "messages"} {
			route := *t.routes[string(def.ID)+":"+protocol]
			var requests, tokens, samples int64
			for _, bucket := range route.buckets {
				if bucket.second > now.Unix()-flowRateSeconds && bucket.second <= now.Unix() {
					requests += bucket.requests
					tokens += bucket.tokens
					samples += bucket.usageSamples
				}
			}
			route.QPS = float64(requests) / flowRateSeconds
			route.TPS = float64(tokens) / flowRateSeconds
			route.UsageAvailable = samples > 0 || (requests == 0 && route.Active == 0)
			snapshot.Routes = append(snapshot.Routes, route)
		}
	}
	return snapshot
}

// Subscribe coalesces bursts without blocking inference on a slow dashboard.
func (t *FlowTracker) Subscribe() (<-chan struct{}, func()) {
	events := make(chan struct{}, 1)
	t.mu.Lock()
	t.subscribers[events] = struct{}{}
	t.mu.Unlock()
	return events, func() { t.mu.Lock(); delete(t.subscribers, events); t.mu.Unlock() }
}

func (t *FlowTracker) notifyLocked() {
	for events := range t.subscribers {
		select {
		case events <- struct{}{}:
		default:
		}
	}
}
