package config

import "time"

func (c *Config) FirstGenerationTimeout() time.Duration {
	if c != nil && c.FirstTokenTimeoutSeconds < 0 {
		return 0
	}
	value := 0
	if c != nil {
		value = c.FirstTokenTimeoutSeconds
	}
	return time.Duration(boundedDefault(value, 60, 86400)) * time.Second
}

func (c *Config) WorkBuddyOutputBudget() int {
	value := 0
	if c != nil {
		value = c.WorkBuddyDefaultMaxTokens
	}
	return boundedDefault(value, 8192, 131072)
}

// Negative explicitly disables gateway queue sleeps, while zero inherits the
// shared policy. It never causes an immediate retry against a closed gate.
func (c *Config) QoderQueueBudget() time.Duration {
	if c == nil || c.QoderQueueWaitBudgetMs == 0 {
		return -1
	}
	if c.QoderQueueWaitBudgetMs < 0 {
		return 0
	}
	return time.Duration(min(c.QoderQueueWaitBudgetMs, 86400000)) * time.Millisecond
}

func (c *Config) SharedStreamIdleTimeout() time.Duration {
	value := 0
	if c != nil {
		value = c.SharedStreamIdleTimeoutSeconds
	}
	return time.Duration(max(30, boundedDefault(value, 300, 600))) * time.Second
}
