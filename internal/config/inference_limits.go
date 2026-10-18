package config

import (
	"errors"
	"strings"
	"time"
)

// Fixed generation policy. Client-specified output controls remain protocol data.
const (
	FirstGenerationTimeout  = 180 * time.Second
	InferenceRequestTimeout = 2 * time.Hour
	SharedStreamIdleTimeout = 300 * time.Second
	GrokStreamIdleTimeout   = 120 * time.Second
	SharedRefusalWaitBudget = 60 * time.Second
	GenerationMaxRetries    = 20
	ChatDefaultStream       = true
	TokenRefreshInterval    = time.Minute
	LoadBalancerCacheTTL    = 5 * time.Second
)

// InferencePolicy selects fixed budgets within the existing runtime ceilings.
// Profiles change recovery patience, never client output controls or auth.
type InferencePolicy struct {
	Name                         string
	MaxAttempts, MaxAccounts     int
	FirstGeneration              time.Duration
	RefusalWait, ReservationWait time.Duration
}

var ErrInvalidInferenceProfile = errors.New("inference_profile must be standard or interactive")

func (c *Config) InferencePolicy() InferencePolicy {
	if c != nil && strings.EqualFold(strings.TrimSpace(c.InferenceProfile), "interactive") {
		return InferencePolicy{
			Name: "interactive", MaxAttempts: 4, MaxAccounts: 3,
			FirstGeneration: 30 * time.Second, RefusalWait: 5 * time.Second, ReservationWait: 250 * time.Millisecond,
		}
	}
	return InferencePolicy{
		Name: "standard", MaxAttempts: GenerationMaxRetries + 1, MaxAccounts: 20,
		FirstGeneration: FirstGenerationTimeout, RefusalWait: SharedRefusalWaitBudget, ReservationWait: 2 * time.Second,
	}
}

func ValidateInferenceProfile(c *Config) error {
	if c == nil {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(c.InferenceProfile)) {
	case "", "standard", "interactive":
		return nil
	default:
		return ErrInvalidInferenceProfile
	}
}

// RefusalWaitBudget bounds cumulative shared-refusal sleeps, not upstream time.
func (c *Config) RefusalWaitBudget() time.Duration {
	ceiling := c.InferencePolicy().RefusalWait
	if c == nil || c.SharedRefusalWaitBudgetMs == 0 {
		return ceiling
	}
	return time.Duration(min(max(c.SharedRefusalWaitBudgetMs, 0), int(ceiling/time.Millisecond))) * time.Millisecond
}

func (c *Config) ReservationWaitBudget() time.Duration {
	ceiling := c.InferencePolicy().ReservationWait
	if c == nil || c.AccountReservationWaitBudgetMs == 0 {
		return ceiling
	}
	return time.Duration(min(max(c.AccountReservationWaitBudgetMs, 0), int(ceiling/time.Millisecond))) * time.Millisecond
}
