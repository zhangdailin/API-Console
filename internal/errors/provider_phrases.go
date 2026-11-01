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
