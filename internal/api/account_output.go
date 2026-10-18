package api

import (
	"context"
	"encoding/json"
	"maps"
	"strings"
	"time"

	"orchids-api/internal/store"
)

// Account rendering for the management API: the projection a caller sees, never
// the record as stored. Credentials are write-only here, so every surface that
// returns an account goes through one marshal that deletes them.

type accountOutput struct {
	*store.Account
	// SessionFingerprint is a short digest of the credential the account is
	// authenticated with. It lets the table tell two sessions apart on channels
	// that carry no email, without returning the secret itself.
	SessionFingerprint string `json:"session_fingerprint,omitempty"`
	// Quota holds the provider-specific quota projection. It is merged into every
	// account response so the management table can render the plan/allowance
	// columns consistently without re-deriving each channel's semantics on the
	// client.
	Quota map[string]any `json:"-"`
	// CheckinSupported tells the console the channel offers a daily check-in.
	CheckinSupported bool `json:"-"`
}

// MarshalJSON flattens the quota projection into the account object itself.
func (o accountOutput) MarshalJSON() ([]byte, error) {
	merged := map[string]any{}
	if o.Account != nil {
		raw, err := json.Marshal(o.Account)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &merged); err != nil {
			return nil, err
		}
	}
	// The session fingerprint identifies a login on channels that carry no email;
	// it is a digest, never the credential, so it is safe to expose to an
	// authenticated administrator.
	if o.SessionFingerprint != "" {
		merged["session_fingerprint"] = o.SessionFingerprint
	}
	maps.Copy(merged, o.Quota)
	merged["checkin_supported"] = o.CheckinSupported
	// Credentials are write-only. The account API exposes only their presence,
	// including for create, update and refresh responses.
	merged["has_credential"] = o.SessionFingerprint != ""
	for _, field := range []string{"token", "client_cookie", "refresh_token", "oauth_access_token", "oauth_refresh_token", "workbuddy_access_token", "workbuddy_refresh_token", "qoder_access_token", "qoder_refresh_token", "qoder_runtime_info", "qoder_runtime_key", "cline_access_token", "cline_refresh_token", "session_fingerprint"} {
		delete(merged, field)
	}
	if o.Account != nil {
		merged["status_message"] = redactAccountSecrets(o.Account.StatusMessage, o.Account)
		merged["quality_reason"] = redactAccountSecrets(o.Account.QualityReason, o.Account)
	}
	return json.Marshal(merged)
}

// redactAccountSecrets replaces every credential value an account holds with a
// placeholder, so a text field that quotes upstream output cannot publish one.
func redactAccountSecrets(message string, acc *store.Account) string {
	for _, secret := range acc.Secrets() {
		if secret = strings.TrimSpace(secret); secret != "" {
			message = strings.ReplaceAll(message, secret, "[REDACTED]")
		}
	}
	return message
}

func normalizeAccountOutput(acc *store.Account) *accountOutput {
	return normalizeAccountOutputWithUsage(acc, nil)
}

// normalizeAccountOutputWithUsage renders one account for the management API.
//
// usage carries the tokens this gateway observed per account inside the Free window;
// a nil map means "not measured", which the quota projection reports honestly instead
// of presenting zero usage as a measurement.
func normalizeAccountOutputWithUsage(acc *store.Account, usage map[int64]int64) *accountOutput {
	// The session fingerprint is derived from the live credential before the
	// redaction below clears it, so the operator can still tell two browser
	// logins apart without the session token ever leaving the server.
	sessionFingerprint := accountSessionFingerprint(acc)
	if acc == nil {
		return nil
	}
	// The projection renders a copy: the channel redaction below clears slots on
	// the way out, and the stored record must keep the credential it was given.
	out := *acc
	// The message is redacted with the same list the final render uses. The two
	// used to differ: this one omitted Qoder's access token and runtime pair, and
	// it ran before the channel projection cleared them, so an upstream error that
	// echoed a Qoder token published it in status_message.
	out.StatusMessage = redactAccountSecrets(acc.StatusMessage, acc)
	out.QualityReason = redactAccountSecrets(acc.QualityReason, acc)
	// The channel clears every credential it must never return. The final
	// marshal deletes the credential keys again whatever the channel did, so a
	// channel that forgot one still cannot publish it.
	if admin := adminFor(out.AccountType); admin != nil && admin.Project != nil {
		admin.Project(&out)
	}
	return &accountOutput{
		Account:            &out,
		SessionFingerprint: sessionFingerprint,
		Quota:              buildQuotaResponseFieldsWithUsage(&out, usage[out.ID], usage != nil),
		CheckinSupported:   checkinSupported(out.AccountType),
	}
}

// normalizeAccountOutputObserved renders one account together with the Free-window
// usage this gateway measured for it, so a single-account response carries the same
// estimate as the list. A failed measurement falls back to the plain projection
// rather than reporting zero usage as if it had been counted.
func (a *API) normalizeAccountOutputObserved(ctx context.Context, acc *store.Account) *accountOutput {
	observed, ok := a.observedTokensByAccount(ctx, time.Now().Add(-observedUsageWindow()))
	if !ok {
		return normalizeAccountOutput(acc)
	}
	return normalizeAccountOutputWithUsage(acc, observed)
}

// observedTokensByAccount sums the tokens the journal recorded for each account
// inside the Free window.
//
// A Build Free allowance is a rolling token window that the upstream only reveals
// once it is exhausted, so the only honest usage figure available to the admin UI is
// what this gateway itself saw. The scan is bounded (auditScanCap newest entries),
// which makes the sum a floor rather than a total: it travels with quota_observed so
// an estimate is never mistaken for a complete count. A failed scan returns ok=false,
// and callers must then leave the usage unmeasured.
func (a *API) observedTokensByAccount(ctx context.Context, since time.Time) (map[int64]int64, bool) {
	if a == nil || a.store == nil || a.store.RedisClient() == nil {
		return nil, false
	}
	entries, err := a.store.RedisClient().XRevRangeN(ctx, a.store.RedisPrefix()+"audit:log", "+", "-", auditScanCap).Result()
	if err != nil {
		return nil, false
	}
	usage := make(map[int64]int64, len(entries))
	for _, entry := range entries {
		event, ok := decodeAuditEvent(entry)
		if !ok || event.AccountID == 0 {
			continue
		}
		if !since.IsZero() && event.Timestamp.Before(since) {
			continue
		}
		tokens := event.TotalTokens
		if tokens <= 0 {
			tokens = event.InputTokens + event.OutputTokens
		}
		if tokens > 0 {
			usage[event.AccountID] += int64(tokens)
		}
	}
	return usage, true
}

// accountSessionFingerprint returns a short, non-reversible identifier of the
// credential an account is authenticated with.
//
// It exists because some channels authenticate with a session token that carries
// no identity at all (there is no email or username to show). The account table
// then had nothing to display but "login session configured", which made two different
// logins look identical. The fingerprint distinguishes them without ever
// exposing the secret: 12 hex characters of a SHA-256 digest, the same shape
// already used for upstream diagnostics.
func accountSessionFingerprint(acc *store.Account) string {
	if acc == nil {
		return ""
	}
	if admin := adminFor(acc.AccountType); admin != nil && admin.Fingerprint != nil {
		return admin.Fingerprint(acc)
	}
	return ""
}
