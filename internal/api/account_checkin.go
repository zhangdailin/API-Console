package api

import (
	"log/slog"
	"net/http"
	"time"

	"orchids-api/internal/provider"
	"orchids-api/internal/refreshqueue"
	"orchids-api/internal/store"
	"orchids-api/internal/util"
)

// checkinSupported reports whether the account's channel offers a daily
// check-in, so the console shows the action only where it exists.
func checkinSupported(accountType string) bool {
	admin := adminFor(accountType)
	return admin != nil && admin.Checkin != nil
}

// handleAccountCheckin serves POST /api/accounts/{id}/checkin: one claim round
// through the channel, recorded on the account whatever the outcome.
//
// It takes the same per-account lease as a refresh, because a check-in may
// rotate the credential on a refused token and two rotations of one account
// must never run at once.
func (a *API) handleAccountCheckin(w http.ResponseWriter, r *http.Request, account *store.Account) {
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w)
		return
	}
	admin := adminFor(account.AccountType)
	if admin == nil || admin.Checkin == nil {
		http.Error(w, "check-in is not supported for this channel", http.StatusNotFound)
		return
	}
	var (
		result provider.CheckinResult
		err    error
	)
	if !refreshqueue.WithLease(account.ID, func() {
		result, err = admin.Checkin(r.Context(), provider.Env{Config: a.config.Load(), Store: a.store}, account)
	}) {
		writeAccountCheckBusy(w)
		return
	}
	outcome := provider.CheckinOutcome(result, err, time.Now())
	outcome.Message = redactAccountSecrets(outcome.Message, account)
	if recordErr := a.store.RecordAccountCheckin(r.Context(), account.ID, outcome); recordErr != nil {
		slog.Warn("Failed to record account check-in", "account_id", account.ID, "error", recordErr)
	}
	updated, loadErr := a.store.GetAccount(r.Context(), account.ID)
	if loadErr != nil {
		updated = account
		updated.Checkin = outcome
	}
	body := map[string]any{
		"checkin": outcome,
		"account": a.normalizeAccountOutputObserved(r.Context(), updated),
	}
	status := http.StatusOK
	if err != nil {
		// The upstream refused or could not be reached; the shape matches the
		// console's error envelope so its shared helper shows the reason.
		status = http.StatusBadGateway
		body["error"] = map[string]any{"type": "checkin_failed", "message": outcome.Message}
	}
	util.WriteJSONStatus(w, status, body)
}
