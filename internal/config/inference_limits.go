package config

import "time"

// Fixed generation policy. Client-specified output controls remain protocol data.
const (
	FirstGenerationTimeout  = 180 * time.Second
	InferenceRequestTimeout = 2 * time.Hour
	SharedStreamIdleTimeout = 300 * time.Second
	GrokStreamIdleTimeout   = 120 * time.Second
	SharedRefusalWaitBudget = 60 * time.Second
	QoderQueueRetryInterval = 5 * time.Second
	GenerationMaxRetries    = 20
	ChatDefaultStream       = true
	TokenRefreshInterval    = time.Minute
	LoadBalancerCacheTTL    = 5 * time.Second
)

// RefusalWaitBudget bounds cumulative shared-refusal sleeps, not upstream time.
func (c *Config) RefusalWaitBudget() time.Duration {
	if c == nil || c.SharedRefusalWaitBudgetMs == 0 {
		return SharedRefusalWaitBudget
	}
	return time.Duration(min(max(c.SharedRefusalWaitBudgetMs, 0), 60000)) * time.Millisecond
}

func (c *Config) ReservationWaitBudget() time.Duration {
	if c == nil || c.AccountReservationWaitBudgetMs == 0 {
		return 2 * time.Second
	}
	return time.Duration(min(max(c.AccountReservationWaitBudgetMs, 0), 2000)) * time.Millisecond
}
