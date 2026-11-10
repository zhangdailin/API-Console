package api

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"orchids-api/internal/accountpolicy"
	"orchids-api/internal/provider"
	"orchids-api/internal/store"
)

// What a refresh or a manual check concludes about an account, and how long that
// verdict holds. A cleared verdict must agree with the selector that parks the
// account, or the console shows green and the next request turns it red again.

func (a *API) syncAccountAfterCreate(acc store.Account) {
	if !acc.Enabled {
		return
	}

	go func(account store.Account) {
		syncCtx, syncCancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer syncCancel()

		accountStatus, _, syncErr := a.refreshAccountState(syncCtx, &account)
		if syncErr != nil {
			slog.Warn("Initial account sync failed", "account_id", account.ID, "type", account.AccountType, "error", syncErr)
			if accountStatus != "" {
				account.StatusCode = accountStatus
				account.StatusMessage = strings.TrimSpace(syncErr.Error())
				account.LastAttempt = time.Now()
			}
		} else {
			applySuccessfulAccountRefreshStatus(&account, accountStatus)
		}

		if updateErr := a.store.UpdateAccount(context.Background(), &account); updateErr != nil {
			slog.Warn("Failed to persist initial account sync", "account_id", account.ID, "type", account.AccountType, "error", updateErr)
		}
		if spec, ok := provider.Lookup(account.AccountType); ok && spec.PublishModels != nil {
			if err := spec.PublishModels(context.Background(), provider.Env{Config: a.config.Load(), Store: a.store}); err != nil {
				slog.Warn("Initial account model publication failed", "account_id", account.ID, "type", account.AccountType, "error", err)
			}
		}
	}(acc)
}

func applySuccessfulAccountRefreshStatus(acc *store.Account, status string) {
	if acc == nil {
		return
	}
	status = strings.TrimSpace(status)
	// The credentials answered the upstream, whatever the verdict: stamp it so a
	// scheduler can tell a verified account from one that was never checked. The
	// policy package owns that pairing so every entrance behaves identically.
	if status == "" {
		// A successful check proves the *credential* works; it says nothing about the
		// allowance. Clearing a spent-allowance verdict here is what made the console
		// show a green account that failed again on the very next request — the check
		// answered for the token, while the verdict that parked the account came from
		// the upstream refusing an actual request.
		if accountHoldsAllowanceVerdict(acc, time.Now()) && allowanceStillSpent(acc) {
			acc.VerifiedAt = time.Now()
			return
		}
		accountpolicy.Success(time.Now()).Apply(acc)
		return
	}
	verdict := accountpolicy.Verdict{Status: status, At: time.Now()}
	if verdict.Scope = accountpolicy.ScopeForStatus(status); verdict.Scope == accountpolicy.ScopeCredential {
		verdict.NeedsLogin = true
	}
	// A verifier that reports only a status has no better explanation than the one
	// already on the record. The reason is the operator's only signal — the account
	// table shows a bare code without it — so an unchanged verdict keeps the
	// specific wording (the verifier returns "402" with nothing else, and a
	// manual check used to wipe the upstream's own explanation).
	//
	// The carry-over is limited to the same status: a new code means the old reason
	// described a different problem and would mislead.
	if strings.TrimSpace(verdict.Message) == "" && strings.TrimSpace(acc.StatusCode) == status {
		verdict.Message = strings.TrimSpace(acc.StatusMessage)
	}
	verdict.Apply(acc)
}

// accountHoldsAllowanceVerdict reports whether the account is parked for an
// allowance the upstream refused, with its reset time still ahead.
//
// The question is "would the selector still hold this account?", so the answer has
// to match the selector: a 402 with a future reset time is out of rotation, and a
// check that cleared the marker would only make the next request re-park it — which
// is what the console showed as an account turning green and then red again.
func accountHoldsAllowanceVerdict(acc *store.Account, now time.Time) bool {
	if acc == nil || strings.TrimSpace(acc.StatusCode) != "402" || acc.QuotaResetAt.IsZero() {
		return false
	}
	return now.Before(acc.QuotaResetAt)
}

// allowanceStillSpent reports whether the meter still says the allowance is gone.
//
// This is the half that keeps the rule from stranding an account: an operator who
// buys credits is released by the next check rather than waiting for the cycle
// boundary the reset time names. It reads the snapshot the check just refreshed,
// so a top-up is visible immediately.
//
// A failed meter read leaves the previous snapshot in place, and a stale "spent"
// answer holds the account until its reset time. That is the conservative
// direction: the alternative is re-offering an account whose allowance was last
// observed to be gone.
func allowanceStillSpent(acc *store.Account) bool {
	if acc == nil {
		return false
	}
	return acc.UsageLimit > 0 && acc.UsageCurrent <= 0
}
