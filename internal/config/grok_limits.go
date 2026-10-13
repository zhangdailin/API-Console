package config

import (
	"math"
	"time"
)

func boundedDefault(value, fallback, maximum int) int {
	if value <= 0 {
		return fallback
	}
	return min(value, maximum)
}

// Pacing is optional and scoped to an account/team, never the whole endpoint.
func (c *Config) GrokRequestsPerSecond() float64 {
	if c == nil {
		return 0
	}
	value := c.GrokBuildRPS
	if value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	return min(max(value, 0.01), 1000)
}

// UpstreamRequestTimeout uses the fixed inference deadline. Internal catalog reads
// can carry a shorter operation limit; it is never a configurable JSON field.
func (c *Config) UpstreamRequestTimeout() time.Duration {
	if c != nil && c.RequestTimeout > 0 {
		return min(time.Duration(c.RequestTimeout)*time.Second, InferenceRequestTimeout)
	}
	return InferenceRequestTimeout
}
