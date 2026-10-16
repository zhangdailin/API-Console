package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Empty Reason requests a healthy reset at the revision the request observed.
// A failure increments the latest stored counter rather than a caller snapshot.
type AccountQualityUpdate struct {
	ExpectedRevision uint64
	Reason           string
	CooldownUntil    time.Time
	DisableThreshold int
}

type AccountQualityResult struct {
	Applied          bool
	PreviousFailures int
	Failures         int
	Revision         uint64
	CooldownUntil    time.Time
	Reason           string
	Enabled          bool
	StatusMessage    string
}

func (s *Store) ApplyAccountQuality(ctx context.Context, id int64, update AccountQualityUpdate) (AccountQualityResult, error) {
	if s == nil || s.accounts == nil {
		return AccountQualityResult{}, fmt.Errorf("account store not configured")
	}
	return s.accounts.ApplyAccountQuality(ctx, id, update)
}

func (s *redisStore) ApplyAccountQuality(ctx context.Context, id int64, update AccountQualityUpdate) (AccountQualityResult, error) {
	var result AccountQualityResult
	found := false
	err := s.updateAccountAtomic(ctx, id, func(existing *Account) error {
		found = true
		result = AccountQualityResult{PreviousFailures: existing.QualityFailures}
		reason := strings.TrimSpace(update.Reason)
		if reason != "" {
			existing.QualityFailures++
			existing.QualityReason = reason
			if update.CooldownUntil.After(existing.QualityCooldownUntil) {
				existing.QualityCooldownUntil = update.CooldownUntil
			}
			existing.QualityRevision++
			result.Applied = true
			if update.DisableThreshold > 0 && existing.QualityFailures >= update.DisableThreshold {
				existing.Enabled = false
				existing.StatusMessage = "disabled after repeated degraded responses (" + reason + ")"
			} else if existing.StatusCode == "" {
				existing.StatusMessage = "parked after a degraded response (" + reason + ")"
			}
		} else if existing.QualityRevision == update.ExpectedRevision && existing.QualityFailures > 0 {
			existing.QualityFailures = 0
			existing.QualityCooldownUntil = time.Time{}
			existing.QualityReason = ""
			existing.QualityRevision++
			if existing.StatusCode == "" {
				existing.StatusMessage = ""
			}
			result.Applied = true
		}
		result.Failures = existing.QualityFailures
		result.Revision = existing.QualityRevision
		result.CooldownUntil = existing.QualityCooldownUntil
		result.Reason = existing.QualityReason
		result.Enabled = existing.Enabled
		result.StatusMessage = existing.StatusMessage
		return nil
	})
	if err == nil && !found {
		return result, ErrNoRows
	}
	return result, err
}
