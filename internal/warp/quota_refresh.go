package warp

import (
	"context"
	"time"

	"orchids-api/internal/provider"
	"orchids-api/internal/store"
)

// refreshQuota is display-only. A failed billing read must not invalidate a
// credential proven by catalog access, invent zero balance, or undo admin edits.
func (c *Client) refreshQuota(ctx context.Context, acc *store.Account, s *store.Store) {
	if acc == nil {
		return
	}
	expectedRefresh := acc.WarpRefreshToken
	snapshot, err := c.fetchQuota(ctx, &expectedRefresh)
	if err != nil {
		snapshot.Status = "error"
	}
	if err == nil && snapshot.Validate() != nil {
		snapshot = store.WarpQuotaSnapshot{Status: "error", Source: quotaSource, AttemptedAt: time.Now().UTC()}
	}
	if s != nil && acc.ID != 0 {
		if s.UpdateWarpQuota(ctx, acc.ID, expectedRefresh, snapshot) != nil {
			return
		}
		if latest, e := s.GetAccount(ctx, acc.ID); e == nil && latest != nil && latest.WarpRefreshToken == expectedRefresh {
			acc.WarpQuota = latest.WarpQuota.Clone()
		}
		return
	}
	// New login rows have not reached Store yet. Preserve a previous successful
	// read when a later attempt fails, exactly as the atomic storage merge does.
	if snapshot.Status == "error" {
		previous := acc.WarpQuota.Clone()
		snapshot.Windows = previous.Windows
		snapshot.SyncedAt = previous.SyncedAt
	}
	acc.WarpQuota = snapshot.Clone()
}

func quotaDue(acc *store.Account, now time.Time) bool {
	if acc == nil {
		return false
	}
	q := acc.WarpQuota
	interval := provider.HealthRefreshInterval
	if q.Status == "error" {
		interval = 10 * time.Minute
	}
	return q.AttemptedAt.IsZero() || now.Sub(q.AttemptedAt) >= interval
}

func projectWarpQuota(fields map[string]any, acc *store.Account) {
	fields["quota_supported"] = false
	fields["quota_mode"] = "warp_pools"
	fields["quota_unit"] = "credits"
	fields["quota_note"] = "Included pools only; extra credits and pay-as-you-go balances are not included"
	if acc == nil {
		return
	}
	q := acc.WarpQuota
	fields["quota_status"] = q.Status
	fields["quota_synced_at"] = q.SyncedAt
	fields["quota_attempted_at"] = q.AttemptedAt
	for _, w := range q.Windows {
		if w.Limit != nil || w.Used != nil || w.Remaining != nil || (w.HasUnlimited && w.Unlimited) {
			fields["quota_supported"] = true
			break
		}
	}
	// Deliberately do not emit an aggregate quota_limit/remaining: workspace
	// pools are shared and must not be added into a personal spendable balance.
}
