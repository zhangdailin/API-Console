package console

import (
	"log/slog"
	"time"

	"orchids-api/internal/grok/grokbase"
	"orchids-api/internal/store"
)

// allowanceRecovery schedules dashboard re-reads for accounts whose weekly
// pool is spent, so they return to the pool when the week resets.
var allowanceRecovery grokbase.RecoveryQueue

func claimAllowanceRecovery(acc *store.Account, now time.Time) bool {
	if acc == nil {
		return false
	}
	if quota := acc.GrokConsoleQuota; !quota.Exhausted() || quota.Unlinked {
		allowanceRecovery.Done(acc.ID)
		return false
	}
	return allowanceRecovery.Claim(acc.ID, now)
}

func settleAllowanceRecovery(acc *store.Account, now time.Time) {
	quota := acc.GrokConsoleQuota
	if !quota.Exhausted() || quota.Unlinked {
		allowanceRecovery.Done(acc.ID)
		if !quota.Unlinked {
			slog.Info("Grok Console allowance recovered; account returned to the pool", "account_id", acc.ID)
		}
		return
	}
	allowanceRecovery.Reschedule(acc.ID, now, quota.ResetAt)
}
