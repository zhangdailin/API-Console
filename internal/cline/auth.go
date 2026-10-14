// Package cline implements the Cline (api.cline.bot) backend as an upstream
// channel.
//
// The channel is OAuth-only: an account is created exclusively through the
// official WorkOS device authorization flow, whose grant is then exchanged at
// api.cline.bot for the credential this channel spends. There is no pasted
// personal access token.
//
// Credential layers, in order:
//
//	WorkOS          device authorization grant     (client_id is public)
//	Cline register  POST /auth/register            (accessToken + refreshToken)
//	Cline refresh   POST /auth/refresh             (refreshToken)
//	request auth    Authorization: Bearer workos:<accessToken>
//
// The `workos:` prefix is literal, not a scheme: the upstream expects the string
// exactly as the Cline CLI sends it. The refresh token is the durable secret.
//
// Two protocol details a naive client gets wrong:
//
//  1. A pending device authorization is reported as a non-2xx status carrying
//     `error=authorization_pending`. It is a normal step of the flow, not a
//     failure.
//  2. The chat endpoint always answers with SSE, and some chunks arrive wrapped
//     in a {"data":{...}} envelope that has to be unwrapped before the delta is
//     read.
package cline

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"encoding/json"

	"orchids-api/internal/store"
	"orchids-api/internal/util"
)

// Default endpoints.
const (
	// DefaultAPIBase serves the Cline control plane, the model feed and chat.
	DefaultAPIBase = "https://api.cline.bot/api/v1"
	// DefaultWorkOSAuthorizeURL mints one device authorization transaction.
	DefaultWorkOSAuthorizeURL = "https://api.workos.com/user_management/authorize/device"
	// DefaultWorkOSAuthenticateURL exchanges the device code for a grant.
	DefaultWorkOSAuthenticateURL = "https://api.workos.com/user_management/authenticate"
	// DefaultWorkOSClientID is the public WorkOS client id of the Cline CLI. It
	// is not a secret.
	DefaultWorkOSClientID = "client_01K3A541FN8TA3EPPHTD2325AR"
)

// Chat defaults. The upstream answers an omitted reasoning_effort with empty
// content for some models, so the value is always sent.
const (
	// DefaultReasoningEffort is what the Cline CLI sends.
	DefaultReasoningEffort = "high"
	// DefaultMaxTokens is the CLI's completion budget. The upstream treats the
	// field as a cap, not a request for that many tokens.
	DefaultMaxTokens = 128000
	// RefreshLead is how long before expiry the access token is renewed.
	RefreshLead = 5 * time.Minute
	// taskIDPrefix starts the per-request correlation id.
	taskIDPrefix = "sess_"
)

// loginHosts are the only hosts a browser authorization URL may point at. The
// server never sees the operator's password, so a tampered URL is the one place
// a login could be redirected to a third party.
//
// Loopback is additionally accepted: a local endpoint cannot exfiltrate the
// operator's credentials to a third party, and the deployment and test suites
// both need to point the flow at a stub server.
//
// authkit.cline.bot is the host the production WorkOS tenant actually answers
// with: WorkOS serves the device page on the customer's AuthKit domain, so the
// generic WorkOS hosts alone would refuse the real login.
var loginHosts = []string{"api.workos.com", "workos.com", "dashboard.workos.com", "authkit.cline.bot", "api.cline.bot"}

// Errors classified for the admin API so the UI can explain the cause instead of
// echoing upstream text.
var (
	// ErrAuthPending means the browser step has not completed yet.
	ErrAuthPending = errors.New("cline authorization is still pending")
	// ErrAuthUnavailable means no connection could be established at all.
	ErrAuthUnavailable = errors.New("cline authorization endpoint is unreachable")
	// ErrAuthRejected means the endpoint answered but refused the transaction.
	ErrAuthRejected = errors.New("cline authorization was rejected")
	// ErrCredentialMissing means the account carries no Cline credential.
	ErrCredentialMissing = errors.New("cline account is missing an OAuth credential; sign in again")
	// ErrReLoginRequired means the durable credential is gone or expired. Only a
	// new browser authorization can recover it.
	ErrReLoginRequired = errors.New("cline refresh token is expired; a new browser login is required")
	// ErrStreamTruncated means the upstream closed the stream before its finish
	// event. The answer is incomplete and must not be reported as success.
	ErrStreamTruncated = errors.New("cline stream ended before the finish event")
	// ErrNoUpstreamCatalog reports that no model feed has been observed for the
	// account. It is a state rather than a failure: the fix is a model refresh
	// with an active account.
	ErrNoUpstreamCatalog = errors.New("cline account has no upstream model catalog; refresh models with an active account")
)

// Credentials is the Cline credential pair plus the identity observed at login.
// RefreshToken is the durable secret; AccessToken is short lived and renewable.
type Credentials struct {
	AccessToken  string
	RefreshToken string
	// ExpiresAt bounds the access token. A zero value means the upstream did not
	// state one.
	ExpiresAt time.Time
	// Email is display metadata, not a credential.
	Email string
}

// HasCredential reports whether any credential material is present.
func (c Credentials) HasCredential() bool {
	return strings.TrimSpace(c.AccessToken) != "" || strings.TrimSpace(c.RefreshToken) != ""
}

// AccessValid reports whether the access token is usable at the given instant.
func (c Credentials) AccessValid(now time.Time) bool {
	if strings.TrimSpace(c.AccessToken) == "" {
		return false
	}
	if c.ExpiresAt.IsZero() {
		// An opaque token without a stated expiry is tried as-is; the upstream
		// remains the authority on validity.
		return true
	}
	return c.ExpiresAt.Sub(now) > RefreshLead
}

// Bearer renders the request credential. The `workos:` prefix is part of the
// value the upstream expects, not a scheme this client adds.
func (c Credentials) Bearer() string { return "workos:" + strings.TrimSpace(c.AccessToken) }

// Fields returns the normalized credential fields.
func (c Credentials) Fields() (accessToken, refreshToken string, expiresAt time.Time, email string) {
	return strings.TrimSpace(c.AccessToken),
		strings.TrimSpace(c.RefreshToken),
		c.ExpiresAt,
		strings.TrimSpace(c.Email)
}

// ResolveCredentials reads the channel's credential off an account.
func ResolveCredentials(acc *store.Account) Credentials {
	if acc == nil {
		return Credentials{}
	}
	return Credentials{
		AccessToken:  strings.TrimSpace(acc.ClineAccessToken),
		RefreshToken: strings.TrimSpace(acc.ClineRefreshToken),
		ExpiresAt:    acc.ClineExpiresAt,
		Email:        strings.TrimSpace(acc.ClineEmail),
	}
}

// ParseExpiry reads the upstream expiry, which reaches this client as a number,
// a numeric string or an RFC3339 timestamp depending on the endpoint.
func ParseExpiry(value interface{}) time.Time {
	switch typed := value.(type) {
	case float64:
		return millisToTime(int64(typed))
	case int64:
		return millisToTime(typed)
	case int:
		return millisToTime(int64(typed))
	case json.Number:
		if parsed, err := typed.Int64(); err == nil {
			return millisToTime(parsed)
		}
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return time.Time{}
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
			if parsed, err := time.Parse(layout, trimmed); err == nil {
				return parsed
			}
		}
		if parsed, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
			return millisToTime(parsed)
		}
	}
	return time.Time{}
}

// millisToTime treats a plausible epoch-millisecond value as a timestamp. A
// value too small to be one (a duration, or a count) is ignored rather than
// rendered as a date in 1970, which would make the token look permanently
// expired.
func millisToTime(value int64) time.Time {
	if value < 1_000_000_000_000 {
		return time.Time{}
	}
	return time.UnixMilli(value).UTC()
}

// InferenceCapError reports the account's inference cap. The upstream states the
// wait in prose ("Try again in 17h 59m"), so the duration is parsed out of it: a
// known window lets the scheduler cool the account instead of retrying blind.
type InferenceCapError struct {
	Wait    time.Duration
	Message string
}

func (e *InferenceCapError) Error() string {
	if e.Wait <= 0 {
		return "cline inference cap reached"
	}
	return fmt.Sprintf("cline inference cap reached: try again in %s", formatCapWait(e.Wait))
}

// RetryAfter exposes the upstream-stated cap window to shared account policy
// without coupling that package to the Cline implementation.
func (e *InferenceCapError) RetryAfter() time.Duration {
	if e == nil {
		return 0
	}
	return e.Wait
}

// inferenceCapError builds the classified cap error from an upstream body.
func inferenceCapError(raw string) *InferenceCapError {
	return &InferenceCapError{Wait: ParseInferenceCapDuration(raw), Message: strings.TrimSpace(raw)}
}

// ParseInferenceCapDuration reads the wait the upstream wrote in prose.
func ParseInferenceCapDuration(body string) time.Duration {
	_, after, ok := strings.Cut(body, "Try again in")
	if !ok {
		return 0
	}
	rest := after
	end := len(rest)
	if i := strings.IndexAny(rest, "\"\n\r}"); i >= 0 {
		end = i
	}
	return ParseHumanDuration(strings.TrimSpace(rest[:end]))
}

// ParseHumanDuration parses the upstream's own spellings: "17h 59m", "2h",
// "59m", "30s", "1d 2h".
func ParseHumanDuration(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	var total time.Duration
	number := 0
	valid := false
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c >= '0' && c <= '9':
			number = number*10 + int(c-'0')
			continue
		case c == 'd' || c == 'D':
			total += time.Duration(number) * 24 * time.Hour
			number, valid = 0, true
		case c == 'h' || c == 'H':
			total += time.Duration(number) * time.Hour
			number, valid = 0, true
		case c == 'm' || c == 'M':
			total += time.Duration(number) * time.Minute
			number, valid = 0, true
		case c == 's' || c == 'S':
			total += time.Duration(number) * time.Second
			number, valid = 0, true
		default:
			number = 0
		}
	}
	if !valid {
		return 0
	}
	return total
}

func formatCapWait(wait time.Duration) string {
	if wait <= 0 {
		return "a moment"
	}
	wait = wait.Round(time.Second)
	hours := int(wait / time.Hour)
	minutes := int(wait % time.Hour / time.Minute)
	switch {
	case hours > 0 && minutes > 0:
		return fmt.Sprintf("%dh %dm", hours, minutes)
	case hours > 0:
		return fmt.Sprintf("%dh", hours)
	case minutes > 0:
		return fmt.Sprintf("%dm", minutes)
	default:
		return fmt.Sprintf("%ds", int(wait/time.Second))
	}
}

// allowedLoginHost reports whether an authorization URL may be handed to a
// browser.
//
// The allowlist is the WorkOS hosts plus the deployment's own configured
// endpoint. Honouring the configured endpoint is deliberate: a regional or
// self-hosted deployment legitimately points the flow somewhere else, and
// refusing it would make the setting unusable. Everything else is refused,
// including a host that merely looks similar.
func allowedLoginHost(host, configured string) bool {
	return util.AllowedLoginHost(host, configured, loginHosts)
}

// newTaskID mints the per-request correlation id. The upstream uses it as the
// session identity, so a retry of the same turn carries a new one exactly as a
// new turn would.
func newTaskID(now time.Time) string { return fmt.Sprintf("%s%d", taskIDPrefix, now.UnixMilli()) }
