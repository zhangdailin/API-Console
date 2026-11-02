package config

import "time"

// Fixed generation policy. Client-specified output controls remain protocol data.
const (
	InferenceRequestTimeout = 2 * time.Hour
	SharedStreamIdleTimeout = 300 * time.Second
	// GrokBuildResponseHeaderTimeout bounds the wait for Build response headers.
	// Once headers arrive, only GrokStreamIdleTimeout (semantic idleness) ends a
	// stream, so a model that reasons silently is not cut off by a wall clock.
	GrokBuildResponseHeaderTimeout = 5 * time.Minute
	GrokStreamIdleTimeout          = 120 * time.Second
	SharedRefusalWaitBudget        = 60 * time.Second
	AccountReservationWaitBudget   = 2 * time.Second
	GenerationMaxRetries           = 20
	// GenerationMaxAttempts is the physical upstream call budget per request.
	GenerationMaxAttempts = GenerationMaxRetries + 1
	// MaxAccountSwitches bounds how many distinct accounts one request may try.
	MaxAccountSwitches   = 20
	ChatDefaultStream    = true
	TokenRefreshInterval = time.Minute
	LoadBalancerCacheTTL = 5 * time.Second
)

// RefusalWaitBudget bounds cumulative shared-refusal sleeps, not upstream time.
func (c *Config) RefusalWaitBudget() time.Duration {
	return boundedWaitBudget(c, SharedRefusalWaitBudget, func(c *Config) int { return c.SharedRefusalWaitBudgetMs })
}

func (c *Config) ReservationWaitBudget() time.Duration {
	return boundedWaitBudget(c, AccountReservationWaitBudget, func(c *Config) int { return c.AccountReservationWaitBudgetMs })
}

// boundedWaitBudget applies a configured override no larger than ceiling.
// Zero keeps the ceiling; a negative override disables waiting.
func boundedWaitBudget(c *Config, ceiling time.Duration, field func(*Config) int) time.Duration {
	if c == nil || field(c) == 0 {
		return ceiling
	}
	return time.Duration(min(max(field(c), 0), int(ceiling/time.Millisecond))) * time.Millisecond
}
