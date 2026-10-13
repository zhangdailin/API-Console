package middleware

import (
	"math"
	"slices"
	"time"
)

// Counters cover every completion; only latency samples have a retention cap.
const (
	flowWindowSeconds        = 60
	flowLatencyWindowSeconds = 15 * 60
	flowSampleLimit          = 256
)

type FlowCounts struct {
	Completed       int64 `json:"completed"`
	Failed          int64 `json:"failed"`
	StreamErrors    int64 `json:"stream_errors"`
	RateLimited     int64 `json:"rate_limited"`
	Canceled        int64 `json:"canceled"`
	Timeouts        int64 `json:"timeouts"`
	Observed        int64 `json:"observed"`
	UsageReported   int64 `json:"usage_reported"`
	Retried         int64 `json:"retried"`
	Attempts        int64 `json:"attempts"`
	AccountSwitches int64 `json:"account_switches"`
	RetryWaitMS     int64 `json:"retry_wait_ms"`
	QueueWaitMS     int64 `json:"queue_wait_ms"`
}

func (c *FlowCounts) add(other FlowCounts) {
	c.Completed += other.Completed
	c.Failed += other.Failed
	c.StreamErrors += other.StreamErrors
	c.RateLimited += other.RateLimited
	c.Canceled += other.Canceled
	c.Timeouts += other.Timeouts
	c.Observed += other.Observed
	c.UsageReported += other.UsageReported
	c.Retried += other.Retried
	c.Attempts += other.Attempts
	c.AccountSwitches += other.AccountSwitches
	c.RetryWaitMS += other.RetryWaitMS
	c.QueueWaitMS += other.QueueWaitMS
}

type FlowDistribution struct {
	Samples int    `json:"samples"`
	P50MS   *int64 `json:"p50_ms"`
	P95MS   *int64 `json:"p95_ms"`
}

type FlowPhase struct {
	Samples int64   `json:"samples"`
	AvgMS   float64 `json:"avg_ms"`
}

type FlowMetrics struct {
	FlowCounts
	Waiting         int64                `json:"waiting"`
	Responding      int64                `json:"responding"`
	LongestActiveMS int64                `json:"longest_active_ms"`
	FailureRate     *float64             `json:"failure_rate"`
	FirstGeneration FlowDistribution     `json:"first_generation"`
	FirstVisible    FlowDistribution     `json:"first_visible"`
	BodyTTFB        FlowDistribution     `json:"body_ttfb"`
	Duration        FlowDistribution     `json:"duration"`
	HTTPPhases      map[string]FlowPhase `json:"http_phases"`
}

type flowRequest struct {
	started    time.Time
	responding bool
}

type flowLatencySample struct {
	at time.Time
	// Negative means absent; a measured sub-millisecond response is valid zero.
	firstGeneration, firstVisible, bodyTTFB, duration int64
}

type flowPhase struct{ sum, samples int64 }

var flowPhaseNames = [5]string{"dns_ms", "tcp_ms", "tls_ms", "connection_wait_ms", "response_headers_ms"}

type flowMetricBuilder struct {
	FlowMetrics
	generation, visible, body, duration []int64
	phases                              [5]flowPhase
}

func (b *flowMetricBuilder) add(route *flowRouteState, now time.Time) {
	for request := range route.activeRequests {
		if request.responding {
			b.Responding++
		} else {
			b.Waiting++
		}
		b.LongestActiveMS = max(b.LongestActiveMS, now.Sub(request.started).Milliseconds())
	}
	for _, bucket := range route.buckets {
		if bucket.second <= now.Unix()-flowWindowSeconds || bucket.second > now.Unix() {
			continue
		}
		b.FlowCounts.add(bucket.FlowCounts)
		for i, phase := range bucket.phases {
			b.phases[i].sum += phase.sum
			b.phases[i].samples += phase.samples
		}
	}
	for _, sample := range route.samples {
		if sample.at.IsZero() || sample.at.Unix() <= now.Unix()-flowLatencyWindowSeconds || sample.at.After(now) {
			continue
		}
		if sample.firstGeneration >= 0 {
			b.generation = append(b.generation, sample.firstGeneration)
		}
		if sample.firstVisible >= 0 {
			b.visible = append(b.visible, sample.firstVisible)
		}
		if sample.bodyTTFB >= 0 {
			b.body = append(b.body, sample.bodyTTFB)
		}
		b.duration = append(b.duration, sample.duration)
	}
}

func flowDistribution(values []int64) FlowDistribution {
	out := FlowDistribution{Samples: len(values)}
	if len(values) == 0 {
		return out
	}
	slices.Sort(values)
	p50, p95 := values[int(math.Ceil(float64(len(values))*.5))-1], values[int(math.Ceil(float64(len(values))*.95))-1]
	out.P50MS, out.P95MS = &p50, &p95
	return out
}

func (b *flowMetricBuilder) metrics() FlowMetrics {
	if b.Completed > 0 {
		rate := float64(b.Failed) / float64(b.Completed)
		b.FailureRate = &rate
	}
	b.FirstGeneration, b.FirstVisible = flowDistribution(b.generation), flowDistribution(b.visible)
	b.BodyTTFB, b.Duration = flowDistribution(b.body), flowDistribution(b.duration)
	b.HTTPPhases = make(map[string]FlowPhase)
	for i, phase := range b.phases {
		if phase.samples > 0 {
			b.HTTPPhases[flowPhaseNames[i]] = FlowPhase{Samples: phase.samples, AvgMS: float64(phase.sum) / float64(phase.samples)}
		}
	}
	return b.FlowMetrics
}
