package api

// Account refresh: how one channel's credential and allowance are re-proved.
//
// The channels agree on the contract — return the status the scheduler should
// record, the HTTP status the caller should see, and the failure — but disagree
// entirely on how to get there, so each keeps its own function. This was a chain
// of if-blocks inside refreshAccountState; the dispatch is now a lookup.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/store"
)

// accountRefreshers dispatches a refresh by account type. A channel that is
// absent is an unsupported account type.
var accountRefreshers = map[string]func(*API, context.Context, *store.Account) (string, int, error){
	"grok":      refreshGrokAccountState,
	"qoder":     refreshQoderAccountState,
	"workbuddy": refreshWorkBuddyAccountState,
	"cline":     refreshClineAccountState,
}

// refreshGrokAccountState re-verifies a Grok credential.
func refreshGrokAccountState(a *API, ctx context.Context, acc *store.Account) (string, int, error) {
	if verifyErr := verifyGrokAccount(ctx, acc, a.config.Load(), a.store); verifyErr != nil {
		message := strings.ToLower(verifyErr.Error())
		if strings.Contains(message, "missing oauth token") {
			return "", http.StatusBadRequest, fmt.Errorf("failed to verify grok account: %w", verifyErr)
		}
		status := apperrors.ClassifyAccountStatus(verifyErr.Error())
		return status, httpStatusFromAccountStatus(status), fmt.Errorf("failed to verify grok account: %w", verifyErr)
	}
	return "", 0, nil
}

// refreshQoderAccountState re-verifies a Qoder credential through the store,
// which may also persist a rotated refresh token.
func refreshQoderAccountState(a *API, ctx context.Context, acc *store.Account) (string, int, error) {
	return verifyThroughStore("qoder", errQoderMissingCredential, func() (string, int, error) {
		return verifyQoderAccountWithStore(ctx, acc, a.config.Load(), a.store)
	})
}

// refreshWorkBuddyAccountState re-verifies a WorkBuddy credential through the
// store, which may also persist a rotated refresh token.
func refreshWorkBuddyAccountState(a *API, ctx context.Context, acc *store.Account) (string, int, error) {
	return verifyThroughStore("workbuddy", errWorkBuddyMissingCredential, func() (string, int, error) {
		return verifyWorkBuddyAccountWithStore(ctx, acc, a.config.Load(), a.store)
	})
}

// refreshClineAccountState re-verifies a Cline credential through the store,
// which may also persist a rotated refresh token.
func refreshClineAccountState(a *API, ctx context.Context, acc *store.Account) (string, int, error) {
	return verifyThroughStore("cline", errClineMissingCredential, func() (string, int, error) {
		return verifyClineAccountWithStore(ctx, acc, a.config.Load(), a.store)
	})
}

// verifyThroughStore runs a channel's verification and maps its failure onto the
// account status the scheduler records.
//
// Qoder and WorkBuddy prove a credential the same way and report through the same
// store, and both have to tell three failures apart: a missing credential is an
// operator error (400), an upstream verdict the classifier recognises carries its
// own status, and anything else keeps whatever the verify path reported. Only the
// channel's name differs, so the mapping is written once here.
func verifyThroughStore(channel string, missingCredential error, verify func() (string, int, error)) (string, int, error) {
	status, httpStatus, verifyErr := verify()
	if verifyErr == nil {
		return status, httpStatus, nil
	}
	failure := fmt.Errorf("failed to verify %s account: %w", channel, verifyErr)
	if errors.Is(verifyErr, missingCredential) {
		return "", http.StatusBadRequest, failure
	}
	if classified := apperrors.ClassifyAccountStatus(verifyErr.Error()); classified != "" {
		return classified, httpStatusFromAccountStatus(classified), failure
	}
	return status, httpStatus, failure
}
