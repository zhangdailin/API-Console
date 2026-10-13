package api

import (
	"strings"
	"time"

	"orchids-api/internal/accountpolicy"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/store"
)

// Capacity is the available channel pool, before model-specific eligibility.
// Busy slots on held/disabled accounts remain in active_requests, not this ratio.
func opsPoolCapacity(accounts []*store.Account, counts map[int64]int64, channel string) map[string]interface{} {
	now := time.Now()
	var capacity, occupied int64
	for _, acc := range accounts {
		if acc == nil || !acc.Enabled || !strings.EqualFold(strings.TrimSpace(acc.AccountType), channel) || accountpolicy.AccountHeld(acc, now) {
			continue
		}
		limit := loadbalancer.EffectiveAccountConcurrencyLimit(acc)
		if limit <= 0 {
			return map[string]interface{}{"capacity_available": false}
		}
		capacity += limit
		occupied += max(0, counts[acc.ID])
	}
	return map[string]interface{}{"capacity_available": true, "capacity_slots": capacity, "occupied_slots": occupied, "capacity_sampled_at": now.UTC().Format(time.RFC3339Nano)}
}
