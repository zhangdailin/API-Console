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
