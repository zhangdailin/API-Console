package grok

import (
	"log/slog"
	"strings"
	"time"

	"orchids-api/internal/grok/grokbase"
	"orchids-api/internal/store"
)

// buildQuotaRecovery schedules billing re-reads for Build accounts parked on a
// spent allowance (402). Without it such an account waits out its passive
// cooldown even when the upstream restored the allowance earlier.
var buildQuotaRecovery grokbase.RecoveryQueue

// claimBuildQuotaRecovery reports whether this maintenance tick should re-read
// the billing of an account parked for its spent allowance.
func claimBuildQuotaRecovery(acc *store.Account, now time.Time) bool {
	if acc == nil {
		return false
	}
	if strings.TrimSpace(acc.StatusCode) != "402" {
		buildQuotaRecovery.Done(acc.ID)
		return false
	}
	return buildQuotaRecovery.Claim(acc.ID, now)
}

// settleBuildQuotaRecovery applies the outcome of a recovery read. The account
// returns to the pool only on a billing snapshot that carries an allowance
// signal and is not exhausted; a failed or signal-less read proves nothing and
// is retried with backoff, or at the reset the upstream stated.
func settleBuildQuotaRecovery(acc *store.Account, billingErr error, now time.Time) {
	if acc == nil {
		return
	}
	billing := acc.GrokBilling
	observed := billingErr == nil && !billing.SyncedAt.IsZero() &&
		(billing.Weekly.HasUsage || billing.Monthly.HasLimit || billing.HasPaidEntitlement())
	if observed && !billing.IsExhausted() && !freeQuotaSpent(acc, now) {
		acc.StatusCode = ""
		acc.StatusMessage = ""
		acc.LastAttempt = time.Time{}
		acc.QuotaResetAt = time.Time{}
		acc.RateLimitFailures = 0
		buildQuotaRecovery.Done(acc.ID)
		slog.Info("Grok Build allowance recovered; account returned to the pool", "account_id", acc.ID)
		return
	}
	reset := billing.PeriodEnd()
	if free := acc.GrokFreeQuota.ResetAt; free.After(now) && (reset.IsZero() || free.Before(reset)) {
		reset = free
	}
	next := buildQuotaRecovery.Reschedule(acc.ID, now, reset)
	slog.Debug("Grok Build allowance still spent; recovery rescheduled", "account_id", acc.ID, "next_probe_at", next, "billing_error", billingErr)
}

// freeQuotaSpent reports a Free window the upstream confirmed as spent and
// that has not reset yet.
func freeQuotaSpent(acc *store.Account, now time.Time) bool {
	free := acc.GrokFreeQuota
	return !free.ResetAt.IsZero() && now.Before(free.ResetAt) && free.HasLimit && free.Used >= free.Limit
}
