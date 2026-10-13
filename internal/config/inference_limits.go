package config

import "time"

// Fixed generation policy. Client-specified output controls remain protocol data.
const (
	FirstGenerationTimeout    = 180 * time.Second
	InferenceRequestTimeout   = 2 * time.Hour
	SharedStreamIdleTimeout   = 300 * time.Second
	GrokStreamIdleTimeout     = 120 * time.Second
	WorkBuddyDefaultMaxTokens = 8192
	SharedRefusalWaitBudget   = 60 * time.Second
	QoderQueueRetryInterval   = 5 * time.Second
	GenerationMaxRetries      = 20
)
