package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/provider"
	"orchids-api/internal/refreshqueue"
	"orchids-api/internal/store"
	"orchids-api/internal/util"
)

func httpStatusFromAccountStatus(status string) int {
	switch strings.TrimSpace(status) {
	case "401":
		return http.StatusUnauthorized
	case "402", store.AccountStatusQoderQuotaExhausted, store.AccountStatusWorkBuddyQuotaExhausted:
		return http.StatusPaymentRequired
	case "403":
		return http.StatusForbidden
	case "404":
		return http.StatusNotFound
	case "429":
		return http.StatusTooManyRequests
	default:
		return http.StatusBadGateway
	}
}

func buildQuotaResponseFields(acc *store.Account) map[string]interface{} {
	return buildQuotaResponseFieldsWithUsage(acc, 0, false)
}

// buildQuotaResponseFieldsWithUsage projects an account's allowance. observedTokens
// is the usage this gateway measured inside observedUsageWindow and
// usageObserved says whether that measurement actually ran; both are used only by the
// Free estimate, which must never present unmeasured usage as if it were measured.
func buildQuotaResponseFieldsWithUsage(acc *store.Account, observedTokens int64, usageObserved bool) map[string]interface{} {
	fields := map[string]interface{}{
		"quota_limit":     0.0,
		"quota_used":      0.0,
		"quota_remaining": 0.0,
		"quota_mode":      "remaining",
		"quota_unit":      "credits",
		"quota_supported": true,
	}
	provider.QuotaProvenance(fields, "unknown", "unknown", "", "", false, false)
	if acc == nil {
		return fields
	}

	limit := acc.UsageLimit
	current := acc.UsageCurrent
	if limit < 0 {
		limit = 0
	}
	if current < 0 {
		current = 0
	}

	projectQuotaFields(fields, acc, limit, current, observedTokens, usageObserved)
	return fields
}

func (a *API) HandleAccounts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		accounts, err := a.store.ListAccounts(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if accounts == nil {
			accounts = []*store.Account{}
		}
		// The Free estimate needs the usage this gateway observed; the scan is one
		// bounded read shared by the whole page, and it is skipped entirely when there
		// is nothing to describe.
		var observed map[int64]int64
		if len(accounts) > 0 {
			if measured, ok := a.observedTokensByAccount(r.Context(), time.Now().Add(-observedUsageWindow())); ok {
				observed = measured
			}
		}
		normalized := make([]*accountOutput, 0, len(accounts))
		for _, acc := range accounts {
			if acc == nil {
				continue
			}
			normalized = append(normalized, normalizeAccountOutputWithUsage(acc, observed))
		}
		util.WriteJSON(w, normalized)

	case http.MethodPost:
		var acc store.Account
		if err := json.NewDecoder(r.Body).Decode(&acc); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		acc.AccountType = strings.ToLower(strings.TrimSpace(acc.AccountType))
		if !validateAccountType(w, acc.AccountType) {
			return
		}
		// The channel decides what a submitted account must carry, and whether
		// it can be created from the form at all.
		if admin := adminFor(acc.AccountType); admin != nil && admin.PrepareCreate != nil {
			if err := admin.PrepareCreate(&acc); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		if existing, err := a.findDuplicateAccountByCredential(r.Context(), &acc, 0); err != nil {
			slog.Error("Failed to detect duplicate account token", "error", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		} else if existing != nil {
			http.Error(w, duplicateAccountError(existing).Error(), http.StatusConflict)
			return
		}

		if err := a.store.CreateAccount(r.Context(), &acc); err != nil {
			slog.Error("Failed to create account", "error", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		if acc.Enabled {
			if strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Account-Sync")), "async") {
				a.syncAccountAfterCreate(acc)
			} else {
				syncCtx, syncCancel := context.WithTimeout(r.Context(), 25*time.Second)
				accountStatus, _, syncErr := a.refreshAccountState(syncCtx, &acc)
				syncCancel()
				if syncErr != nil {
					slog.Warn("Initial account sync failed", "account_id", acc.ID, "type", acc.AccountType, "error", syncErr)
					if accountStatus != "" {
						acc.StatusCode = accountStatus
						acc.StatusMessage = strings.TrimSpace(syncErr.Error())
						acc.LastAttempt = time.Now()
						acc.VerifiedAt = acc.LastAttempt
					}
				} else {
					applySuccessfulAccountRefreshStatus(&acc, accountStatus)
				}
				// A credential the upstream definitively rejects must not be
				// persisted as a healthy account: it would sit in the pool looking
				// healthy while every request routed to it fails.
				if acc.StatusCode == "401" {
					if deleteErr := a.store.DeleteAccount(r.Context(), acc.ID); deleteErr != nil {
						slog.Error("Failed to roll back rejected account", "account_id", acc.ID, "type", acc.AccountType, "error", deleteErr)
					} else {
						slog.Warn("Rejected account was not saved (upstream refused the credential)",
							"account_id", acc.ID, "type", acc.AccountType, "reason", acc.StatusMessage)
					}
					apperrors.New("authentication_error",
						"account was rejected by the upstream and was not saved: "+strings.TrimSpace(acc.StatusMessage),
						http.StatusUnauthorized).WriteResponse(w)
					return
				}
				if updateErr := a.store.UpdateAccount(r.Context(), &acc); updateErr != nil {
					slog.Warn("Failed to persist initial account sync", "account_id", acc.ID, "type", acc.AccountType, "error", updateErr)
				}
			}
		}

		util.WriteJSONStatus(w, http.StatusCreated, normalizeAccountOutput(&acc))

	default:
		writeMethodNotAllowed(w)
	}
}

func (a *API) HandleAccountByID(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/accounts/")
	parts := strings.Split(path, "/")
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	account, err := a.store.GetAccount(r.Context(), id)
	if err != nil {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	isCheck := len(parts) > 1 && parts[1] == "check"
	isUsage := len(parts) > 1 && parts[1] == "usage"
	if len(parts) > 2 || (len(parts) > 1 && !(isCheck || isUsage)) {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	switch r.Method {
	case http.MethodGet:
		if isUsage {
			resp := map[string]interface{}{
				"account_id":     account.ID,
				"name":           account.Name,
				"account_type":   account.AccountType,
				"subscription":   account.Subscription,
				"usage_current":  account.UsageCurrent,
				"usage_limit":    account.UsageLimit,
				"usage_total":    account.UsageTotal,
				"quota_reset_at": account.QuotaResetAt,
			}
			for k, v := range buildQuotaResponseFields(account) {
				resp[k] = v
			}
			util.WriteJSON(w, resp)
			return
		}
		if isCheck {
			// Storm control / backoff: only allow a small number of concurrent checks,
			// and apply exponential backoff per account on failures.
			now := time.Now()
			a.checkMu.Lock()
			if a.checkInFlight[id] {
				a.checkMu.Unlock()
				http.Error(w, "account check already in progress", http.StatusTooManyRequests)
				return
			}
			if next, ok := a.checkNextAllowed[id]; ok && !next.IsZero() && now.Before(next) {
				retryAfter := int(next.Sub(now).Seconds())
				if retryAfter < 1 {
					retryAfter = 1
				}
				a.checkMu.Unlock()
				w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
				http.Error(w, "account check backoff", http.StatusTooManyRequests)
				return
			}
			a.checkInFlight[id] = true
			a.checkMu.Unlock()
			defer func() {
				a.checkMu.Lock()
				delete(a.checkInFlight, id)
				a.checkMu.Unlock()
			}()

			// global concurrency limit
			a.checkSem <- struct{}{}
			defer func() { <-a.checkSem }()

			acc := account
			checkOK := false
			checkErrStatus := ""
			defer func() {
				a.checkMu.Lock()
				defer a.checkMu.Unlock()
				if checkOK {
					a.checkFailCount[id] = 0
					a.checkNextAllowed[id] = time.Now().Add(3 * time.Second)
					return
				}
				fails := a.checkFailCount[id] + 1
				a.checkFailCount[id] = fails
				d := time.Duration(1<<min(fails, 8)) * time.Second
				// For CF/rate-limit style failures, start with a bigger cooldown.
				if checkErrStatus == "403" || checkErrStatus == "429" {
					if d < 60*time.Second {
						d = 60 * time.Second
					}
				}
				if d > 10*time.Minute {
					d = 10 * time.Minute
				}
				a.checkNextAllowed[id] = time.Now().Add(d)
			}()

			// The manual check takes the same process-wide lease as the background
			// scheduler: two refreshes of one account must never run at once, or the
			// slower writer would persist an older snapshot over a newer verdict.
			var accountStatus string
			var httpStatus int
			var refreshErr error
			if !refreshqueue.WithLease(acc.ID, func() {
				accountStatus, httpStatus, refreshErr = a.refreshAccountState(r.Context(), acc)
			}) {
				slog.Info("Account check skipped: a refresh of this account is already running", "account_id", acc.ID)
				checkErrStatus = ""
				a.checkMu.Lock()
				a.checkInFlight[id] = false
				a.checkMu.Unlock()
				writeAccountCheckBusy(w)
				return
			}
			if refreshErr != nil {
				checkErrStatus = accountStatus
				if accountStatus != "" {
					acc.StatusCode = accountStatus
					// The reason matters: a bare "401" cannot tell an operator
					// whether the credential was retired upstream or the record
					// lost it.
					acc.StatusMessage = strings.TrimSpace(refreshErr.Error())
					acc.LastAttempt = time.Now()
					acc.VerifiedAt = acc.LastAttempt
					if updateErr := a.store.UpdateAccount(r.Context(), acc); updateErr != nil {
						slog.Warn("Failed to persist account refresh status", "account_id", acc.ID, "error", updateErr)
					}
				}
				if httpStatus == 0 {
					httpStatus = http.StatusBadRequest
				}
				http.Error(w, refreshErr.Error(), httpStatus)
				return
			}

			// Clear the account status after a successful refresh/verification
			applySuccessfulAccountRefreshStatus(acc, accountStatus)
			checkOK = true

			if err := a.store.UpdateAccount(r.Context(), acc); err != nil {
				http.Error(w, "Failed to save checked account: "+err.Error(), http.StatusInternalServerError)
				return
			}
			util.WriteJSON(w, a.normalizeAccountOutputObserved(r.Context(), acc))
			return
		}
		util.WriteJSON(w, a.normalizeAccountOutputObserved(r.Context(), account))

	case http.MethodPut:
		existing := account

		var acc store.Account
		if err := json.NewDecoder(r.Body).Decode(&acc); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		acc.ID = id
		if strings.TrimSpace(acc.AccountType) == "" {
			acc.AccountType = existing.AccountType
		}
		acc.AccountType = strings.ToLower(strings.TrimSpace(acc.AccountType))
		if !validateAccountType(w, acc.AccountType) {
			return
		}
		// Secrets are redacted on read, so an ordinary edit arrives without
		// them. The channel restores what the form could not have sent and marks
		// a credential that was actually replaced.
		if admin := adminFor(acc.AccountType); admin != nil && admin.PrepareUpdate != nil {
			if err := admin.PrepareUpdate(&acc, existing); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}

		if acc.UserID == "" {
			acc.UserID = existing.UserID
		}
		if acc.Email == "" {
			acc.Email = existing.Email
		}
		if duplicate, err := a.findDuplicateAccountByCredential(r.Context(), &acc, id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		} else if duplicate != nil {
			http.Error(w, duplicateAccountError(duplicate).Error(), http.StatusConflict)
			return
		}

		if err := a.store.UpdateAccount(r.Context(), &acc); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		util.WriteJSON(w, normalizeAccountOutput(&acc))

	case http.MethodDelete:
		if err := a.store.DeleteAccount(r.Context(), id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		writeMethodNotAllowed(w)
	}
}
