package config

import (
	"encoding/json"
	"testing"
	"time"
)

func TestWaitBudgetsSurviveConfigRoundTripAndDefaults(t *testing.T) {
	for _, tc := range []struct {
		raw                  string
		refusal, reservation time.Duration
	}{
		{`{}`, 60 * time.Second, 2 * time.Second},
		{`{"shared_refusal_wait_budget_ms":-1,"account_reservation_wait_budget_ms":-1}`, 0, 0},
		{`{"shared_refusal_wait_budget_ms":5000,"account_reservation_wait_budget_ms":250}`, 5 * time.Second, 250 * time.Millisecond},
		{`{"shared_refusal_wait_budget_ms":999999,"account_reservation_wait_budget_ms":999999}`, 60 * time.Second, 2 * time.Second},
	} {
		var c Config
		if err := json.Unmarshal([]byte(tc.raw), &c); err != nil {
			t.Fatal(err)
		}
		ApplyDefaults(&c)
		raw, err := json.Marshal(c.Clone())
		if err != nil {
			t.Fatal(err)
		}
		var restored Config
		if err := json.Unmarshal(raw, &restored); err != nil {
			t.Fatal(err)
		}
		if restored.RefusalWaitBudget() != tc.refusal || restored.ReservationWaitBudget() != tc.reservation {
			t.Fatalf("%s: budgets %v, %v", tc.raw, restored.RefusalWaitBudget(), restored.ReservationWaitBudget())
		}
	}
}

func TestInferenceProfilesHaveBoundedIndependentBudgets(t *testing.T) {
	for _, name := range []string{"", "standard", "interactive", " INTERACTIVE "} {
		cfg := &Config{InferenceProfile: name}
		ApplyHardcoded(cfg)
		if err := ValidateInferenceProfile(cfg); err != nil {
			t.Fatal(err)
		}
		policy := cfg.InferencePolicy()
		if policy.MaxAttempts > GenerationMaxRetries+1 || policy.FirstGeneration > FirstGenerationTimeout {
			t.Fatal("profile exceeded runtime ceilings", policy)
		}
		if cfg.MaxRetries+1 != policy.MaxAttempts || cfg.AccountSwitchCount != policy.MaxAccounts {
			t.Fatal(cfg, policy)
		}
		if policy.Name == "interactive" {
			if policy.MaxAttempts != 4 || policy.MaxAccounts != 3 || policy.FirstGeneration != 30*time.Second {
				t.Fatal(policy)
			}
			if cfg.RefusalWaitBudget() != 5*time.Second || cfg.ReservationWaitBudget() != 250*time.Millisecond {
				t.Fatal(policy)
			}
			cfg.SharedRefusalWaitBudgetMs, cfg.AccountReservationWaitBudgetMs = int(^uint(0)>>1), int(^uint(0)>>1)
			if cfg.RefusalWaitBudget() != policy.RefusalWait || cfg.ReservationWaitBudget() != policy.ReservationWait {
				t.Fatal("profile caps bypassed")
			}
		}
	}
	if ValidateInferenceProfile(&Config{InferenceProfile: "unknown"}) == nil {
		t.Fatal("unknown profile accepted")
	}
}
