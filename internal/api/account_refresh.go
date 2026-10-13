package api

// Account refresh: how a channel's credential and allowance are re-proved.
//
// The channels agree on the contract — return the status the scheduler should
// record, the HTTP status the caller should see, and the failure — but disagree
// entirely on how to get there, so each implements it behind provider.Admin.
// What is the same for all of them is how a failure is turned into an account
// status, and that mapping is written once here.

import (
	"errors"
	"fmt"
	"net/http"

	apperrors "orchids-api/internal/errors"
)

// verifyThroughChannel runs a channel's verification and maps its failure onto
// the account status the scheduler records.
//
// Every channel has to tell three failures apart: a missing credential is an
// operator error (400), an upstream verdict the classifier recognises carries
// its own status, and anything else keeps whatever the verify path reported.
// Only the channel's name differs.
func verifyThroughChannel(channel string, missingCredential error, verify func() (string, int, error)) (string, int, error) {
	status, httpStatus, verifyErr := verify()
	if verifyErr == nil {
		return status, httpStatus, nil
	}
	failure := fmt.Errorf("failed to verify %s account: %w", channel, verifyErr)
	if missingCredential != nil && errors.Is(verifyErr, missingCredential) {
		return "", http.StatusBadRequest, failure
	}
	if classified := apperrors.ClassifyAccountStatus(verifyErr.Error()); classified != "" {
		return classified, httpStatusFromAccountStatus(classified), failure
	}
	return status, httpStatus, failure
}
