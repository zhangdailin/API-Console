package console

import (
	"context"
	"errors"
	"net/http"
)

// Diagnostic is a credential-free description of a failed model-list check.
// Stages identify observed failures, not inferred account capabilities.
type Diagnostic struct {
	Reason     string
	Stage      string
	Message    string
	HTTPStatus int
}

type verificationStageError struct {
	stage string
	err   error
}

func (e *verificationStageError) Error() string { return e.err.Error() }
func (e *verificationStageError) Unwrap() error { return e.err }

type apiKeyDiagnosticError struct {
	err, keyError error
}

// Preserve the original request error and its status for existing handlers;
// keep discovery evidence separate and never append its possibly secret prose.
func (e *apiKeyDiagnosticError) Error() string { return e.err.Error() }
func (e *apiKeyDiagnosticError) Unwrap() error { return e.err }

func withAPIKeyDiagnostic(err, keyErr error) error {
	if keyErr == nil {
		return err
	}
	return &apiKeyDiagnosticError{err: err, keyError: keyErr}
}

// DiagnoseVerification classifies an unsuccessful Console verification without
// exposing upstream bodies or retrying any request, key creation or conversion.
func DiagnoseVerification(err error) Diagnostic {
	d := Diagnostic{Reason: "console_verification_failed", Stage: "models", Message: "Console model-list verification failed. The source Web account was not changed."}
	var status *StatusError
	if errors.As(err, &status) {
		d.HTTPStatus = status.Status
	}
	var stage *verificationStageError
	if errors.As(err, &stage) {
		d.Stage = stage.stage
	}
	var key *apiKeyDiagnosticError
	keyFailed := errors.As(err, &key)
	if keyFailed {
		d.Stage = "api_key"
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		d.Reason, d.Message = "timeout", "Console verification timed out or was cancelled. The source Web account was not changed."
	case errors.Is(err, ErrUnauthorized), errors.Is(err, ErrMissingSSOToken), keyFailed && errors.Is(key.keyError, ErrUnauthorized):
		d.Reason, d.Message = "sso_rejected", "Console rejected the SSO session. Sign in again and re-import the Cookie; Web access does not prove Console access."
		if d.HTTPStatus == 0 {
			d.HTTPStatus = http.StatusUnauthorized
		}
	case keyFailed:
		d.Reason, d.Message = "console_api_key_unavailable", "Could not obtain a usable Console API key. Check Console sign-in, team membership and API key access; model permissions could not be verified. The source Web account was not changed."
	case d.Stage == "dpop":
		d.Reason, d.Message = "console_auth_failed", "Console DPoP session establishment failed. Check Console sign-in and access policy; model permissions could not be verified."
	case d.HTTPStatus == http.StatusForbidden:
		d.Reason, d.Message = "console_model_access_denied", "Console rejected the model-list check (HTTP 403). An API key was obtained, but its team or model permissions may be insufficient. Check the same account in console.x.ai; the source Web account was not changed."
	case d.HTTPStatus == http.StatusTooManyRequests:
		d.Reason, d.Message = "console_rate_limited", "Console rate-limited the model-list check. Wait before retrying; the source Web account was not changed."
	case d.HTTPStatus >= 500:
		d.Reason, d.Message = "console_upstream_unavailable", "Console could not complete the model-list check. Retry later; the source Web account was not changed."
	}
	return d
}
