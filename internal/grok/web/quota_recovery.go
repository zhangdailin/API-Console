package web

import (
	"log/slog"
	"strings"
	"time"

	"orchids-api/internal/grok/grokbase"
	"orchids-api/internal/store"
)

// webQuotaRecovery schedules quota re-reads for Web accounts whose chat
// allowance is spent, so they return to the pool when grok.com restores it
// rather than at the next regular maintenance read.
var webQuotaRecovery grokbase.RecoveryQueue

// webQuotaSpent reports whether a quota window the account reported is spent,
// and the earliest moment such a window resets (zero when the upstream gave
// neither a reset time nor a window length).
func webQuotaSpent(acc *store.Account, now time.Time) (spent bool, resetAt time.Time) {
	if acc == nil {
		return false, time.Time{}
	}
	synced := acc.GrokWebQuota.SyncedAt
	for _, window := range []store.GrokQuotaWindow{acc.GrokWebQuota.Auto, acc.GrokWebQuota.Fast} {
		if !window.HasRemaining || window.Remaining > 0 {
			continue
		}
		spent = true
		reset := window.ResetAt
		if reset.IsZero() && window.WindowSeconds > 0 && !synced.IsZero() {
			// grok.com can report a window length without an absolute reset.
			reset = synced.Add(time.Duration(window.WindowSeconds) * time.Second)
		}
		if reset.After(now) && (resetAt.IsZero() || reset.Before(resetAt)) {
			resetAt = reset
		}
	}
	return spent, resetAt
}

// claimWebQuotaRecovery reports whether this maintenance tick should re-read
// the quota of an account with a spent window. A bare 429 without a spent
// window is a throttle, not an allowance: its cooldown is left to expire.
func claimWebQuotaRecovery(acc *store.Account, now time.Time) bool {
	if acc == nil {
		return false
	}
	spent, _ := webQuotaSpent(acc, now)
	if !spent {
		webQuotaRecovery.Done(acc.ID)
		return false
	}
	return webQuotaRecovery.Claim(acc.ID, now)
}

// settleWebQuotaRecovery applies the outcome of a recovery read: a fresh quota
// snapshot with allowance left clears the rate-limit verdict; anything else is
// rescheduled.
func settleWebQuotaRecovery(acc *store.Account, status string, probeErr error, now time.Time) {
	if acc == nil {
		return
	}
	if status == "401" {
		// The credential verdict is handled by the caller; nothing to recover.
		webQuotaRecovery.Done(acc.ID)
		return
	}
	spent, resetAt := webQuotaSpent(acc, now)
	if probeErr == nil && !spent {
		if strings.TrimSpace(acc.StatusCode) == "429" {
			acc.StatusCode = ""
			acc.StatusMessage = ""
			acc.LastAttempt = time.Time{}
			acc.QuotaResetAt = time.Time{}
			acc.RateLimitFailures = 0
		}
		webQuotaRecovery.Done(acc.ID)
		slog.Info("Grok Web allowance recovered; account returned to the pool", "account_id", acc.ID)
		return
	}
	if probeErr != nil {
		// An unread quota says nothing about the reset: back off.
		resetAt = time.Time{}
	}
	next := webQuotaRecovery.Reschedule(acc.ID, now, resetAt)
	slog.Debug("Grok Web allowance still spent; recovery rescheduled", "account_id", acc.ID, "next_probe_at", next)
}
