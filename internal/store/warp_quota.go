package store

import (
	"context"
	"fmt"
	"math"
	"time"
)

// WarpQuotaSnapshot is a display-only observation, independent of routing and
// account health. An error retains the last successful windows and sync time.
type WarpQuotaSnapshot struct {
	Windows     []WarpQuotaWindow `json:"windows"`
	SyncedAt    time.Time         `json:"synced_at"`
	AttemptedAt time.Time         `json:"attempted_at"`
	Status      string            `json:"status"`
	Source      string            `json:"source"`
}

// WarpQuotaWindow describes one upstream pool. Multiple workspace pools stay
// separate, even when they report identical values. No workspace identity is
// stored. Nil numbers mean unknown; a pointer to zero is an observed zero.
type WarpQuotaWindow struct {
	Scope          string    `json:"scope"`
	Unit           string    `json:"unit"`
	Limit          *float64  `json:"limit"`
	Used           *float64  `json:"used"`
	Remaining      *float64  `json:"remaining"`
	Unlimited      bool      `json:"unlimited"`
	HasUnlimited   bool      `json:"has_unlimited"`
	BonusRemaining *float64  `json:"bonus_remaining"`
	ResetAt        time.Time `json:"reset_at"`
}

// Clone returns an independent copy, including every optional numeric value.
func (q WarpQuotaSnapshot) Clone() WarpQuotaSnapshot {
	if q.Windows != nil {
		windows := make([]WarpQuotaWindow, len(q.Windows))
		copy(windows, q.Windows)
		for i := range windows {
			windows[i].Limit = cloneWarpQuotaNumber(windows[i].Limit)
			windows[i].Used = cloneWarpQuotaNumber(windows[i].Used)
			windows[i].Remaining = cloneWarpQuotaNumber(windows[i].Remaining)
			windows[i].BonusRemaining = cloneWarpQuotaNumber(windows[i].BonusRemaining)
		}
		q.Windows = windows
	}
	return q
}

func cloneWarpQuotaNumber(value *float64) *float64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

// Validate rejects malformed observations without deriving missing numbers or
// capping usage: the upstream may legitimately report Used greater than Limit.
// Zero dates are unknown; nonzero dates must be representable as JSON dates.
func (q WarpQuotaSnapshot) Validate() error {
	switch q.Status {
	case "":
		// A legacy account with no observation has the Go zero value.
		if q.Windows != nil || !q.SyncedAt.IsZero() || !q.AttemptedAt.IsZero() || q.Source != "" {
			return fmt.Errorf("invalid warp quota status")
		}
	case "unknown", "ok", "partial", "error":
	default:
		return fmt.Errorf("invalid warp quota status")
	}
	if !validWarpQuotaDate(q.SyncedAt) || !validWarpQuotaDate(q.AttemptedAt) {
		return fmt.Errorf("invalid warp quota observation date")
	}
	if !q.SyncedAt.IsZero() && !q.AttemptedAt.IsZero() && q.SyncedAt.After(q.AttemptedAt) {
		return fmt.Errorf("warp quota sync date exceeds attempt date")
	}
	for i, window := range q.Windows {
		switch window.Scope {
		case "workspace_monthly", "personal", "legacy_requests":
		default:
			return fmt.Errorf("invalid warp quota scope at window %d", i)
		}
		switch window.Unit {
		case "credits", "requests":
		default:
			return fmt.Errorf("invalid warp quota unit at window %d", i)
		}
		if !validWarpQuotaDate(window.ResetAt) {
			return fmt.Errorf("invalid warp quota reset date at window %d", i)
		}
		for _, value := range []*float64{window.Limit, window.Used, window.Remaining, window.BonusRemaining} {
			if value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0) {
				return fmt.Errorf("invalid warp quota number at window %d", i)
			}
		}
	}
	return nil
}

func validWarpQuotaDate(value time.Time) bool {
	_, err := value.MarshalJSON()
	return err == nil
}

// UpdateWarpQuota atomically updates only the display snapshot observed with the
// expected refresh credential. Older or duplicate attempts have no effect.
func (s *Store) UpdateWarpQuota(ctx context.Context, id int64, expectedRefresh string, quota WarpQuotaSnapshot) error {
	if s != nil && s.accounts != nil {
		if backend, ok := s.accounts.(interface {
			UpdateWarpQuota(context.Context, int64, string, WarpQuotaSnapshot) error
		}); ok {
			return backend.UpdateWarpQuota(ctx, id, expectedRefresh, quota)
		}
	}
	return fmt.Errorf("warp quota store not configured")
}

func (s *redisStore) UpdateWarpQuota(ctx context.Context, id int64, expectedRefresh string, quota WarpQuotaSnapshot) error {
	if err := quota.Validate(); err != nil {
		return err
	}
	if quota.AttemptedAt.IsZero() {
		return fmt.Errorf("warp quota attempt date is required")
	}
	quota = quota.Clone()
	return s.updateAccountAtomic(ctx, id, func(acc *Account) error {
		if acc.WarpRefreshToken != expectedRefresh {
			return fmt.Errorf("warp credential changed concurrently")
		}
		acc.WarpQuota = mergeWarpQuota(acc.WarpQuota, quota)
		return nil
	})
}

func mergeWarpQuota(existing, incoming WarpQuotaSnapshot) WarpQuotaSnapshot {
	if !incoming.AttemptedAt.After(existing.AttemptedAt) {
		return existing
	}
	incoming = incoming.Clone()
	if incoming.Status == "error" {
		incoming.Windows = existing.Clone().Windows
		incoming.SyncedAt = existing.SyncedAt
	}
	return incoming
}
