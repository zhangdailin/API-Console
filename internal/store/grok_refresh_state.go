package store

import "time"

// GrokRefreshState is the outcome of the scheduled Grok Build OAuth refreshes:
// how many have failed in a row, why, and when the next attempt is due. It
// holds error codes only, never a token.
//
// It lets the scheduler back off instead of retrying every tick, and lets an
// account whose refresh grant was refused keep serving with its still-valid
// access token until that token expires.
type GrokRefreshState struct {
	// Failures counts consecutive failed refreshes; a success clears it.
	Failures int `json:"failures,omitempty"`
	// SameErrorCount counts consecutive failures with the same ErrorCode.
	SameErrorCount int `json:"same_error_count,omitempty"`
	// ErrorCode is the OAuth error code of the last failure.
	ErrorCode string `json:"error_code,omitempty"`
	// ErrorClass is how the last failure was classified: permanent,
	// configuration, unclassified or transient.
	ErrorClass string `json:"error_class,omitempty"`
	// RetryAt is the earliest time the scheduler may refresh again.
	RetryAt time.Time `json:"retry_at,omitempty"`
	// UpdatedAt orders concurrent writes; a state is only replaced by a newer one.
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// InBackoff reports whether a scheduled refresh must wait.
func (s GrokRefreshState) InBackoff(now time.Time) bool {
	return !s.RetryAt.IsZero() && now.Before(s.RetryAt)
}
