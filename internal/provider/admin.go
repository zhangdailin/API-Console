package provider

import (
	"context"
	"strings"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
)

// Admin is what the management API needs from a channel: how its accounts are
// recognised, written, verified, shown and created by login.
//
// The management API owns the HTTP surface, the state machines and the response
// shape. What a credential is, which fields hold it and which of them may leave
// the server is the channel's knowledge, and lives here so no handler has to
// know it.
type Admin struct {
	// CredentialKey identifies an account by its durable credential, so the
	// same credential cannot be stored twice. Empty means "no credential".
	CredentialKey func(acc *store.Account) string
	// IdentityKey identifies the upstream user behind an account. It survives a
	// token rotation, which the credential key does not: a fresh login issues a
	// new durable token for a user the gateway already has. Empty means the
	// account carries no stable identity yet.
	IdentityKey func(acc *store.Account) string
	// Fingerprint returns a short, non-reversible digest of the credential the
	// account authenticates with. It is the only form of a credential a
	// response may carry.
	Fingerprint func(acc *store.Account) string

	// PrepareCreate validates and normalises an account submitted through the
	// account form. The error text is shown to the operator as a 400.
	PrepareCreate func(acc *store.Account) error
	// PrepareUpdate does the same for an edit. Secrets are redacted on read, so
	// an empty inbound field means "keep the stored value"; the channel
	// restores what the form could not have sent and marks a credential that
	// was actually replaced.
	PrepareUpdate func(acc, existing *store.Account) error
	// PrepareImport validates a row from an export file without contacting the
	// upstream. It returns a machine-readable reason, or "" when the row can be
	// restored.
	PrepareImport func(acc *store.Account) string

	// Project shapes the copy of an account a response is built from: it clears
	// every credential the channel must never return. It is given a copy and
	// must not be handed the stored record.
	Project func(out *store.Account)
	// ProjectQuota renders the account's allowance into the response fields.
	// limit and current are the generic usage slots, already clamped to
	// non-negative; observedTokens and usageObserved carry the usage this
	// gateway measured itself inside ObservedUsageWindow.
	ProjectQuota func(fields map[string]interface{}, acc *store.Account, limit, current float64, observedTokens int64, usageObserved bool)
	// ObservedUsageWindow is how far back the gateway's own token count has to
	// reach for ProjectQuota. Zero means the channel does not use it.
	ObservedUsageWindow time.Duration

	// Verify proves the account's credential against the upstream and refreshes
	// what the channel observes on the way (catalog, allowance, identity). It
	// returns the account status to record, the HTTP status for the caller and
	// the failure. A zero HTTP status leaves the choice to the caller.
	Verify func(ctx context.Context, env Env, acc *store.Account) (status string, httpStatus int, err error)
	// ErrMissingCredential is what Verify returns for an account that has
	// nothing to present. It is an operator error, not an upstream verdict.
	ErrMissingCredential error

	// Login is the channel's browser authorization flow, or nil when the
	// channel is not added that way.
	Login *Login
}

// Login is a browser or device authorization flow: open an official page, poll
// until the operator finishes, then turn the grant into an account.
//
// Only what LoginChallenge exposes as browser-safe ever reaches the console.
// The exchangeable half of a transaction stays inside Private.
type Login struct {
	// UnreachableDetail is what the operator is told when the authorization
	// endpoint cannot be reached. It names the host, because that is the one
	// thing they can check.
	UnreachableDetail string
	// ErrPending means the operator has not finished yet; ErrUnavailable and
	// ErrRejected classify a start that failed. Nothing else about an upstream
	// failure is shown to the browser.
	ErrPending     error
	ErrUnavailable error
	ErrRejected    error
	// NeedsDeviceCode reports whether a transaction can be polled only once it
	// holds a device code. It is false for a flow whose upstream issues the
	// code during the first poll.
	NeedsDeviceCode bool
	// BuildVerifies says that building the account already proves the
	// credential, so a failed build is reported as a failed verification
	// rather than a failed save.
	BuildVerifies bool

	Start func(ctx context.Context, cfg *config.Config) (LoginChallenge, error)
	// Exchange trades the challenge for a grant. It returns ErrPending while
	// the browser step is outstanding.
	Exchange func(ctx context.Context, cfg *config.Config, challenge LoginChallenge) (LoginGrant, error)
	// Build turns a grant into an account record ready to be stored.
	Build func(ctx context.Context, cfg *config.Config, loginID string, challenge LoginChallenge, grant LoginGrant) (*store.Account, error)
	// MarkReplace tells the store that the credential on this account
	// supersedes the stored one, when a login lands on an existing row.
	MarkReplace func(acc *store.Account)

	// SlowDown reports whether a pending answer also asked for a longer poll
	// interval. Nil means the upstream never does.
	SlowDown func(err error) bool
	// Merge writes a fresh grant onto the row the same upstream user already
	// has, keeping the operator settings and runtime state that row holds. Nil
	// means the fresh account simply replaces the stored one.
	Merge func(existing, fresh *store.Account)
}

// LoginChallenge is one open authorization transaction.
type LoginChallenge struct {
	// DeviceCode is the value the exchange trades. It is held server-side and
	// never serialized into a response.
	DeviceCode string
	// UserCode is the short code the operator confirms on the page. It is safe
	// to show: it cannot be exchanged without the device code.
	UserCode string
	// VerifyURI is the human-facing page and VerifyFull the single-use link.
	VerifyURI  string
	VerifyFull string
	ExpiresAt  time.Time
	Interval   time.Duration
	// Private carries the channel's own server-side material (a verifier, a
	// nonce). Nothing outside the channel reads it.
	Private any
}

// LoginGrant is the credential a completed authorization produced. It is opaque
// outside the channel that issued it.
type LoginGrant any

// PreserveBlank keeps the stored value when an edit left the field blank. It
// normalizes neither the stored credential nor a whitespace-only inbound value
// whose stored counterpart is also empty.
func PreserveBlank(target *string, previous string) {
	if strings.TrimSpace(*target) == "" {
		*target = previous
	}
}

// PreserveTime keeps the stored time when an edit left it zero.
func PreserveTime(target *time.Time, previous time.Time) {
	if target.IsZero() {
		*target = previous
	}
}

// PreserveObservedState keeps the usage and health a channel observed, which
// the account form cannot edit. A partial edit must not zero the meter the
// account table displays.
func PreserveObservedState(acc, existing *store.Account) {
	acc.UsageLimit = existing.UsageLimit
	acc.UsageCurrent = existing.UsageCurrent
	acc.UsageTotal = existing.UsageTotal
	PreserveTime(&acc.QuotaResetAt, existing.QuotaResetAt)
	PreserveBlank(&acc.StatusCode, existing.StatusCode)
	PreserveTime(&acc.LastAttempt, existing.LastAttempt)
}

// QuotaProvenance records where a quota number came from and how far it should
// be trusted, next to the number itself.
//
// Without it a table can only say "unknown", which conflates three different
// facts — a paid account whose numeric window upstream does not publish, a Free
// account whose window has to be estimated, and an account that was never
// synced. quota_type is paid/free/unknown, quota_source names the signal,
// quota_confidence is confirmed/observed/estimated, and quota_limit_known is
// false whenever the limit is an estimate that the upstream has not confirmed.
func QuotaProvenance(fields map[string]interface{}, quotaType, source, confidence, note string, limitKnown, observed bool) {
	fields["quota_type"] = quotaType
	fields["quota_source"] = source
	fields["quota_confidence"] = confidence
	fields["quota_limit_known"] = limitKnown
	fields["quota_observed"] = observed
	if note != "" {
		fields["quota_note"] = note
	}
}
