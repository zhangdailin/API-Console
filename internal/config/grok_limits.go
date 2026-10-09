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

// HTTP total deadline (including response body). The ingress request deadline
// remains independently controlled by concurrency_timeout.
func (c *Config) GrokRequestTimeout() time.Duration {
	value, fallback := 0, 600
	if c != nil {
		fallback = boundedDefault(c.RequestTimeout, 600, 86400)
		value = c.GrokBuildTimeout
	}
	return time.Duration(boundedDefault(value, fallback, 86400)) * time.Second
}

// GrokStreamIdleTimeoutFor returns a channel-specific inactivity budget. A
// legacy grok_stream_idle_seconds value remains an all-channel fallback.
func (c *Config) GrokStreamIdleTimeoutFor() time.Duration {
	value, fallback := 0, 120
	if c != nil {
		value = c.GrokStreamIdleSeconds
		if c.GrokBuildStreamIdleSeconds > 0 {
			value = c.GrokBuildStreamIdleSeconds
		}
	}
	seconds := boundedDefault(value, fallback, 600)
	if seconds < 30 {
		seconds = 30
	}
	return time.Duration(seconds) * time.Second
}
