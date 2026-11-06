package errors

import "strings"

// Shared predicates for provider refusal phrases and business codes.
// Callers normalize phrase inputs to lower case before classification.

// IsClineInferenceCap reports Cline's inference cap.
func IsClineInferenceCap(lower string) bool {
	return strings.Contains(lower, "cline inference cap reached")
}

// IsClineProductSurface reports a model reachable only through a Cline product
// surface.
func IsClineProductSurface(lower string) bool {
	return strings.Contains(lower, "only available via cline product surfaces")
}

// IsUpstreamModelRefusal reports that the channel's own service accepted the
// credential and then failed invoking the model on its backend, which refused
// with 401, 403 or 404. Cline answers this way when the provider behind a model
// rejects it:
//
//	cline stream error: Failed to create stream: inference request failed:
//	failed to generate stream from Vercel: failed to invoke model '…' with
//	streaming: request failed with status 403: {"error":{"message":"Forbidden",…
//
// The refusal belongs to the model's provider, so every account gets the same
// answer. Treating it as the account's 403 once took a whole Cline pool out of
// service from one request.
func IsUpstreamModelRefusal(lower string) bool {
	if !strings.Contains(lower, "failed to invoke model") && !strings.Contains(lower, "failed to generate stream from") {
		return false
	}
	return HasExplicitHTTPStatus(lower, "401") || HasExplicitHTTPStatus(lower, "403") || HasExplicitHTTPStatus(lower, "404")
}
