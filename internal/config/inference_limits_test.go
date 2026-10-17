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
