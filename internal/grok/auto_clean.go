package grok

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"orchids-api/internal/accountpolicy"
	"orchids-api/internal/provider"
	"orchids-api/internal/store"
)

// autoCleanReauthAccount deletes a Grok account (Build or Web) whose
// credential the upstream refused and that nobody re-authorized within the
// configured number of hours. It reports whether the account was deleted.
//
// The cleanup is off unless grok_auto_clean_reauth_hours is positive. The age
// is measured from the verdict stamp (LastAttempt); an account without one is
// never deleted, and neither is one that still owns an unfinished video job,
// whose result is fetched with that account's credential.
func autoCleanReauthAccount(ctx context.Context, env provider.Env, acc *store.Account, now time.Time) bool {
	if acc == nil || env.Config == nil || env.Store == nil || !accountpolicy.IsGrokAccount(acc) {
		return false
	}
	hours := env.Config.GrokAutoCleanReauthHours
	if hours <= 0 || !strings.EqualFold(strings.TrimSpace(acc.AuthStatus), store.AccountAuthStatusReauthRequired) {
		return false
	}
	if acc.LastAttempt.IsZero() || now.Sub(acc.LastAttempt) < time.Duration(hours)*time.Hour {
		return false
	}
	if accountHasPendingVideoJob(ctx, env.Store, acc.ID) {
		return false
	}
	if err := env.Store.DeleteAccount(ctx, acc.ID); err != nil {
		slog.Warn("Auto clean: deleting a Grok account that needs re-login failed", "account_id", acc.ID, "error", err)
		return false
	}
	slog.Info("Auto clean: deleted a Grok account that needed re-login for too long",
		"account_id", acc.ID, "account_name", acc.Name, "provider", acc.GrokProvider,
		"reauth_since", acc.LastAttempt, "threshold_hours", hours)
	return true
}

// accountHasPendingVideoJob reports an unfinished video job owned by the
// account. A job store that cannot be read counts as "pending": deleting on an
// unknown would orphan a running job.
func accountHasPendingVideoJob(ctx context.Context, st *store.Store, accountID int64) bool {
	jobs, err := st.ListStoredVideoJobs(ctx)
	if err != nil {
		return !strings.Contains(err.Error(), "not configured")
	}
	for _, job := range jobs {
		if job == nil || job.AccountID != accountID {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(job.Status)) {
		case "completed", "failed", "cancelled", "canceled", "expired":
		default:
			return true
		}
	}
	return false
}
