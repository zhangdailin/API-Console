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
	routes      map[string]*flowRouteState
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
	InputTPS       float64 `json:"input_tps"`
	OutputTPS      float64 `json:"output_tps"`
}

type flowRouteState struct {
	FlowRoute
	buckets        [flowWindowSeconds]flowBucket
	samples        [flowSampleLimit]flowLatencySample
	sampleNext     int
	activeRequests map[*flowRequest]struct{}
}

type flowUsageKey struct{}

type flowBucket struct {
	second                                int64
	requests, input, output, usageSamples int64
	FlowCounts
	phases [5]flowPhase
}

const flowRateSeconds = 5

func (route *flowRouteState) bucket(now time.Time) *flowBucket {
	second := now.Unix()
	bucket := &route.buckets[second%flowWindowSeconds]
	if bucket.second != second {
		*bucket = flowBucket{second: second}
	}
	return bucket
}

type FlowSnapshot struct {
	Available            bool                   `json:"available"`
	InstanceStarted      string                 `json:"instance_started"`
	SampledAt            string                 `json:"sampled_at"`
	Routes               []FlowRoute            `json:"routes"`
	RateSeconds          int                    `json:"rate_seconds"`
	WindowSeconds        int                    `json:"window_seconds"`
	LatencyWindowSeconds int                    `json:"latency_window_seconds"`
	SampleLimit          int                    `json:"sample_limit_per_route"`
	Nodes                map[string]FlowMetrics `json:"nodes"`
}

func NewFlowTracker() *FlowTracker {
	t := &FlowTracker{startedAt: time.Now(), routes: make(map[string]*flowRouteState), subscribers: make(map[chan struct{}]struct{})}
	for _, def := range channel.All() {
		for _, protocol := range []string{"chat", "responses", "messages"} {
			key := string(def.ID) + ":" + protocol
			t.routes[key] = &flowRouteState{FlowRoute: FlowRoute{Channel: string(def.ID), Protocol: protocol}, activeRequests: make(map[*flowRequest]struct{})}
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
		traced := flowTracedWriter(w)
		if traced == nil {
			traced = NewTracedResponseWriter(w)
			w = traced
		}
		request := &flowRequest{started: time.Now()}
		t.mu.Lock()
		route := t.routes[key]
		route.activeRequests[request] = struct{}{}
		route.Active++
		route.Started++
		route.bucket(time.Now()).requests++
		t.notifyLocked()
		t.mu.Unlock()
		traced.flowProgress = func() {
			t.mu.Lock()
			request.responding = true
			t.notifyLocked()
			t.mu.Unlock()
		}

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
				bucket.input += delta
				input = currentInput
			}
			if currentOutput > output {
				delta := currentOutput - output
				route.OutputTokens += delta
				bucket.output += delta
				output = currentOutput
			}
			t.notifyLocked()
			t.mu.Unlock()
		}
		r = r.WithContext(context.WithValue(r.Context(), flowUsageKey{}, observeUsage))
		finished := false
		defer func() {
			observeUsage()
			now := time.Now()
			counts, sample, phases := flowObservation(r, traced, now, !finished)
			t.mu.Lock()
			delete(route.activeRequests, request)
			route.Active--
			route.Completed++
			bucket := route.bucket(now)
			bucket.FlowCounts.add(counts)
			for i, phase := range phases {
				bucket.phases[i].sum += phase.sum
				bucket.phases[i].samples += phase.samples
			}
			route.samples[route.sampleNext%flowSampleLimit] = sample
			route.sampleNext = (route.sampleNext + 1) % flowSampleLimit
			t.notifyLocked()
			t.mu.Unlock()
			traced.flowProgress = nil
		}()
		next(w, r)
		finished = true
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
	snapshot.WindowSeconds = flowWindowSeconds
	snapshot.LatencyWindowSeconds = flowLatencyWindowSeconds
	snapshot.SampleLimit = flowSampleLimit
	snapshot.Nodes = make(map[string]FlowMetrics)
	nodes := make(map[string]*flowMetricBuilder)
	for _, def := range channel.All() {
		if scope != "" && scope != string(def.ID) {
			continue
		}
		for _, protocol := range []string{"chat", "responses", "messages"} {
			current := t.routes[string(def.ID)+":"+protocol]
			route := current.FlowRoute
			var requests, input, output, samples int64
			for _, bucket := range current.buckets {
				if bucket.second > now.Unix()-flowRateSeconds && bucket.second <= now.Unix() {
					requests += bucket.requests
					input += bucket.input
					output += bucket.output
					samples += bucket.usageSamples
				}
			}
			route.QPS = float64(requests) / flowRateSeconds
			route.InputTPS = float64(input) / flowRateSeconds
			route.OutputTPS = float64(output) / flowRateSeconds
			route.TPS = route.InputTPS + route.OutputTPS
			route.UsageAvailable = samples > 0 || (requests == 0 && route.Active == 0)
			snapshot.Routes = append(snapshot.Routes, route)
			for _, key := range []string{route.Channel, route.Protocol, "gateway"} {
				if nodes[key] == nil {
					nodes[key] = &flowMetricBuilder{}
				}
				nodes[key].add(current, now)
			}
		}
	}
	for key, node := range nodes {
		snapshot.Nodes[key] = node.metrics()
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
