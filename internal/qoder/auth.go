// Package qoder implements the Qoder (qoder.com) CLI backend as an upstream
// channel.
//
// The channel is OAuth-only: an account is created exclusively through the
// official CLI device authorization flow (qoder.com/device/selectAccounts plus
// openapi.qoder.sh/api/v1/deviceToken/poll). There is no pasted personal access
// token, and no PAT exchange endpoint is exposed — a leaked PAT cannot be spent
// through this channel.
//
// Credential layers, in order:
//
//	device requirement  device access token + refresh token   (login, refresh)
//	profile             GET /api/v1/userinfo                  (uid, org, tags)
//	runtime fields      encrypt_user_info + RSA key           (derived, per account)
//	request auth        COSY Bearer                           (per request)
//
// Only the device token is a real credential. The runtime fields are derived
// locally from them, exactly as the CLI derives them, so the durable secret an
// operator must protect is the device refresh token.
//
// The upstream protocol has two shapes that a naive client gets wrong:
//
//  1. The chat request body is not raw JSON on the wire. It is the CLI's
//     custom-base64 alphabet with the outer thirds swapped, and the COSY
//     signature covers those encoded bytes — not the JSON.
//  2. The upstream always answers with SSE and terminates with `event:finish`;
//     a `[DONE]` marker may appear only inside a wrapper envelope's body.
//     An EOF before the finish event is a truncation, not a success.
package qoder

import (
	"bytes"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"encoding/json"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
	"orchids-api/internal/util"
)

// Default endpoints. The CLI talks to three hosts, and the CN gateway is a
// separate deployment that answers the same protocol on a different host.
const (
	// DefaultOAuthBaseURL serves the browser authorization page.
	DefaultOAuthBaseURL = "https://qoder.com"
	// DefaultOpenAPIBaseURL serves the device token endpoints and the profile.
	DefaultOpenAPIBaseURL = "https://openapi.qoder.sh"
	// DefaultInferenceURL serves the chat completion SSE endpoint and the model
	// catalog. It is the international node the CLI currently resolves to.
	DefaultInferenceURL = "https://api2.qoder.sh"
	// DefaultClientID is the public OAuth client id of the Qoder CLI. It is not
	// a secret.
	DefaultClientID = "e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb"
	// DefaultClientVersion is the protocol version this channel speaks. It is
	// also the Cosy-Version header and the body's business.version, so the two
	// cannot drift apart.
	DefaultClientVersion = "1.0.45"
	// sceneClientID is the Cosy-ClientType the QoderWork client reports. It is
	// deliberately not reused as the machine type: the capture reports client
	// type 6 and machine type 5 in the same request.
	sceneClientID = "6"
)

// RefreshLead bounds clock skew without immediately refreshing short-lived grants.
const RefreshLead = time.Minute

// loginHosts are the only hosts a browser authorization URL may point at. The
// server never sees the operator's password, so a tampered URL is the one place
// a login could be redirected to a third party.
//
// Loopback is additionally accepted: a local endpoint cannot exfiltrate the
// operator's credentials to a third party, and the deployment and test suites
// both need to point the flow at a stub server.
var loginHosts = []string{"qoder.com", "www.qoder.com", "openapi.qoder.sh"}

// Errors classified for the admin API so the UI can explain the cause instead
// of echoing upstream text.
var (
	// ErrAuthPending means the browser step has not completed yet. The device
	// token endpoint answers HTTP 404 until it has.
	ErrAuthPending = fmt.Errorf("qoder authorization is still pending")
	// ErrAuthUnavailable means no connection could be established at all.
	ErrAuthUnavailable = fmt.Errorf("qoder authorization endpoint is unreachable")
	// ErrAuthRejected means the endpoint answered but refused the transaction.
	ErrAuthRejected = fmt.Errorf("qoder authorization was rejected")
	// ErrCredentialMissing means the account carries no device credential.
	ErrCredentialMissing = fmt.Errorf("qoder account is missing an OAuth credential; sign in again")
	// ErrReLoginRequired means the durable credential is gone or expired. Only a
	// new browser authorization can recover it.
	ErrReLoginRequired = fmt.Errorf("qoder refresh token is expired; a new browser login is required")
	// ErrStreamTruncated means the upstream closed the stream before its finish
	// event. The answer is incomplete and must not be reported as success.
	ErrStreamTruncated = fmt.Errorf("qoder stream ended before the finish event")
	// ErrBusy means the gateway refused the request for queue or concurrency
	// reasons (business code 10605). Backing off helps; refreshing does not.
	ErrBusy = fmt.Errorf("qoder gateway is busy")
	// ErrNoEntitlement means the credential is valid and the gateway accepted the
	// request, but the account has no usable plan or allowance for the model. The
	// gateway reports it as `403 {"pricingUrl":"https://qoder.com/pricing?client=qoder"}`
	// inside a 200 SSE envelope.
	//
	// It is deliberately distinct from an authentication failure: the credential
	// must not be marked dead, because re-authorizing or rotating the token
	// changes nothing. Only a plan change on the Qoder side fixes it.
	ErrNoEntitlement = fmt.Errorf("qoder account has no usable plan or allowance; the model requires a subscription")
	// ErrDailyCountExceeded means the account's daily request count is spent. The
	// upstream reports it under a 401/403 envelope, exactly like a credential
	// rejection, but the credential is fine: reading it as unauthorized forced an
	// OAuth refresh and a replay for an account that could not have been helped
	// by either.
	ErrDailyCountExceeded = fmt.Errorf("qoder daily request count is exceeded")
)

// markerText lowercases an upstream payload and drops the JSON escaping
// backslashes a nested body carries, so a marker is recognised whether the body
// arrived as raw JSON or as a JSON string one envelope deeper: `{"isQueued":true}`
// and `{\"isQueued\":true}` normalise to the same text.
func markerText(value string) string { return strings.ReplaceAll(strings.ToLower(value), `\`, "") }

// IsDailyCountExceeded reports Qoder's per-account daily request-count refusal,
// whatever envelope carried it.
func IsDailyCountExceeded(values ...string) bool {
	return slices.ContainsFunc(values, func(value string) bool { return strings.Contains(markerText(value), "billing daily count exceeded") })
}

// Credentials is the device credential pair plus the identity observed at login.
// RefreshToken is the durable secret; AccessToken is short lived and renewable.
type Credentials struct {
	AccessToken  string
	RefreshToken string
	// AccessExpiresAt and RefreshExpiresAt bound the two tokens. A zero refresh
	// expiry means the upstream did not state one.
	AccessExpiresAt  time.Time
	RefreshExpiresAt time.Time
	// Identity observed at login. These are display metadata, not credentials.
	UID     string
	Name    string
	Email   string
	OrgID   string
	OrgTags []string
	// MachineID is the device identity the credential was authorized under. The
	// upstream rejects a request signed with a different one.
	MachineID string
}

// HasCredential reports whether anything usable was supplied.
func (c Credentials) HasCredential() bool {
	return strings.TrimSpace(c.AccessToken) != "" || strings.TrimSpace(c.RefreshToken) != ""
}

// AccessValid reports whether the access token is usable at the given instant.
func (c Credentials) AccessValid(now time.Time) bool {
	if strings.TrimSpace(c.AccessToken) == "" {
		return false
	}
	if c.AccessExpiresAt.IsZero() {
		// An opaque token without a decodable expiry is tried as-is; the
		// upstream remains the authority on validity.
		return true
	}
	return c.AccessExpiresAt.Sub(now) > RefreshLead
}

// endpoints holds the resolved upstream URLs for one client.
type endpoints struct {
	oauth     string
	openAPI   string
	inference string
}

func resolveEndpoints(cfg *config.Config) endpoints {
	out := endpoints{
		oauth:     DefaultOAuthBaseURL,
		openAPI:   DefaultOpenAPIBaseURL,
		inference: DefaultInferenceURL,
	}
	if cfg == nil {
		return out
	}
	out.oauth = util.FirstNonEmptyURL(cfg.QoderOAuthBaseURL, out.oauth)
	out.openAPI = util.FirstNonEmptyURL(cfg.QoderOpenAPIBaseURL, out.openAPI)
	out.inference = util.FirstNonEmptyURL(cfg.QoderInferenceURL, out.inference)
	return out
}

func resolveClientID(cfg *config.Config) string {
	if cfg != nil {
		if id := strings.TrimSpace(cfg.QoderClientID); id != "" {
			return id
		}
	}
	return DefaultClientID
}

func resolveClientVersion(cfg *config.Config) string {
	if cfg != nil {
		if v := strings.TrimSpace(cfg.QoderClientVersion); v != "" {
			return v
		}
	}
	return DefaultClientVersion
}

// userAgent is the value the CLI identifies itself with, and the Cosy-Version
// family value.
func userAgent(version string) string { return "qoder/" + version }

// signPath is the path component the COSY signature covers: the request path
// without the `/algo` gateway prefix and without the query string.
func signPath(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	path := parsed.Path
	if strings.HasPrefix(path, "/algo") {
		path = strings.TrimPrefix(path, "/algo")
	}
	return path
}

// ResolveCredentials extracts the device credential from an account record.
//
// The dedicated Qoder fields are authoritative. The generic slots are accepted
// as a migration path for an account that was written before this channel had
// its own columns, but only when they hold a JSON document this package
// understands; a raw value is never guessed at, because the generic slots are
// shared with channels whose credentials mean something else.
func ResolveCredentials(acc *store.Account) Credentials {
	if acc == nil {
		return Credentials{}
	}
	creds := Credentials{
		AccessToken:     strings.TrimSpace(acc.QoderAccessToken),
		RefreshToken:    strings.TrimSpace(acc.QoderRefreshToken),
		AccessExpiresAt: acc.QoderExpiresAt,
		UID:             strings.TrimSpace(acc.QoderUserID),
		Name:            strings.TrimSpace(acc.QoderUserName),
		Email:           strings.TrimSpace(acc.Email),
		OrgID:           strings.TrimSpace(acc.QoderOrganizationID),
		OrgTags:         append([]string(nil), acc.QoderOrganizationTags...),
	}
	if creds.AccessToken != "" || creds.RefreshToken != "" {
		return creds
	}
	for _, raw := range []string{acc.Token, acc.RefreshToken} {
		if parsed, ok := ParseCredentialDocument(raw); ok {
			return parsed
		}
	}
	return creds
}

// credentialDocument is the shape this channel accepts as an imported
// credential. It mirrors the CLI's own credential file, so an operator can move
// an existing CLI session into the pool without re-authorizing.
type credentialDocument struct {
	UID                    string   `json:"uid"`
	Name                   string   `json:"name"`
	Email                  string   `json:"email"`
	OrganizationID         string   `json:"organization_id"`
	OrganizationName       string   `json:"organization_name"`
	OrganizationTags       []string `json:"organization_tags"`
	SecurityOAuthToken     string   `json:"security_oauth_token"`
	AccessToken            string   `json:"access_token"`
	RefreshToken           string   `json:"refresh_token"`
	ExpireTime             int64    `json:"expire_time"`
	RefreshTokenExpireTime int64    `json:"refresh_token_expire_time"`
}

// ParseCredentialDocument reads one imported credential document. It reports
// false for anything that does not carry a token, so a caller never turns an
// unrelated value into an account.
func ParseCredentialDocument(raw string) (Credentials, bool) {
	trimmed := strings.TrimSpace(raw)
	if !strings.HasPrefix(trimmed, "{") {
		return Credentials{}, false
	}
	var doc credentialDocument
	if err := json.Unmarshal([]byte(trimmed), &doc); err != nil {
		return Credentials{}, false
	}
	access := strings.TrimSpace(util.FirstNonEmptyUntrimmed(doc.SecurityOAuthToken, doc.AccessToken))
	refresh := strings.TrimSpace(doc.RefreshToken)
	if access == "" && refresh == "" {
		return Credentials{}, false
	}
	creds := Credentials{
		AccessToken:  access,
		RefreshToken: refresh,
		UID:          strings.TrimSpace(doc.UID),
		Name:         strings.TrimSpace(doc.Name),
		Email:        strings.TrimSpace(doc.Email),
		OrgID:        strings.TrimSpace(doc.OrganizationID),
		OrgTags:      append([]string(nil), doc.OrganizationTags...),
	}
	if doc.ExpireTime > 0 {
		creds.AccessExpiresAt = unixSeconds(doc.ExpireTime)
	}
	if doc.RefreshTokenExpireTime > 0 {
		creds.RefreshExpiresAt = unixSeconds(doc.RefreshTokenExpireTime)
	}
	return creds, true
}

// unixSeconds normalizes an upstream timestamp. The CLI stores seconds, but
// some deployments answer milliseconds, and a millisecond value read as seconds
// would place the expiry tens of thousands of years in the future — silently
// disabling refresh.
func unixSeconds(value int64) time.Time {
	if value > 1e11 {
		value /= 1000
	}
	return time.Unix(value, 0)
}

// Fields projects the credentials onto the account columns that own them. It is
// the single mapping used by both the client and the admin API, so a credential
// written through any path lands in the same place.
func (c Credentials) Fields() (accessToken, refreshToken string, accessExpiresAt time.Time, uid, name, email, orgID string, orgTags []string) {
	return strings.TrimSpace(c.AccessToken),
		strings.TrimSpace(c.RefreshToken),
		c.AccessExpiresAt,
		strings.TrimSpace(c.UID),
		strings.TrimSpace(c.Name),
		strings.TrimSpace(c.Email),
		strings.TrimSpace(c.OrgID),
		append([]string(nil), c.OrgTags...)
}

// Profile is the account profile reported by /api/v1/userinfo.
type Profile struct {
	UID     string   `json:"uid"`
	Name    string   `json:"name"`
	Email   string   `json:"email"`
	OrgID   string   `json:"organization_id"`
	OrgTags []string `json:"organization_tags"`
}

// ApplyToAccount applies non-empty identity fields and copies tags, keeping the
// caller's account independent of the profile response backing array.
func (profile Profile) ApplyToAccount(acc *store.Account) {
	if uid := strings.TrimSpace(profile.UID); uid != "" {
		acc.QoderUserID = uid
	}
	if name := strings.TrimSpace(profile.Name); name != "" {
		acc.QoderUserName = name
	}
	if email := strings.TrimSpace(profile.Email); email != "" {
		acc.Email = email
	}
	if orgID := strings.TrimSpace(profile.OrgID); orgID != "" {
		acc.QoderOrganizationID = orgID
	}
	if len(profile.OrgTags) > 0 {
		acc.QoderOrganizationTags = append([]string(nil), profile.OrgTags...)
	}
}

// DeviceToken is the token pair one device flow exchange returned.
type DeviceToken struct {
	AccessToken      string    `json:"token"`
	RefreshToken     string    `json:"refresh_token"`
	ExpiresAt        string    `json:"expires_at"`
	ExpiresIn        int64     `json:"expires_in"`
	RefreshExpiresAt string    `json:"refresh_token_expires_at"`
	RefreshExpiresIn int64     `json:"refresh_token_expires_in"`
	UserID           string    `json:"user_id"`
	UserName         string    `json:"user_name"`
	AccessExpireAt   time.Time `json:"-"`
	RefreshExpireAt  time.Time `json:"-"`
}

// applyExpiries folds the two accepted expiry spellings (RFC3339 or a relative
// lifetime) onto absolute instants.
func (t *DeviceToken) applyExpiries(now time.Time) {
	t.AccessExpireAt = parseExpiry(t.ExpiresAt, t.ExpiresIn, now)
	t.RefreshExpireAt = parseExpiry(t.RefreshExpiresAt, t.RefreshExpiresIn, now)
}

// parseExpiry accepts either an absolute RFC3339 timestamp or a relative
// lifetime in seconds. The CLI answers with whichever the endpoint prefers, and
// treating a relative lifetime as absolute would corrupt the expiry.
func parseExpiry(absolute string, inSeconds int64, now time.Time) time.Time {
	if trimmed := strings.TrimSpace(absolute); trimmed != "" {
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
			if parsed, err := time.Parse(layout, trimmed); err == nil {
				return parsed
			}
		}
		// Some deployments answer a bare Unix timestamp, in seconds or
		// milliseconds.
		if value, err := parseUnixString(trimmed); err == nil {
			return unixSeconds(value)
		}
	}
	if inSeconds != 0 {
		return now.Add(time.Duration(inSeconds) * time.Second)
	}
	return time.Time{}
}

func parseUnixString(raw string) (int64, error) {
	var value int64
	if _, err := fmt.Sscanf(raw, "%d", &value); err != nil {
		return 0, err
	}
	if value <= 0 {
		return 0, fmt.Errorf("non-positive timestamp")
	}
	return value, nil
}

// pricingURLFragment identifies the gateway's entitlement refusal. It is the
// exact marker the upstream uses to point at its pricing page, and it appears
// both as a bare string and as escaped JSON inside the envelope body.
const pricingURLFragment = "qoder.com/pricing"

// DetectNoEntitlement reports whether an upstream message is an entitlement
// refusal rather than a credential failure.
//
// The gateway reports a missing subscription with HTTP 200 and a business status
// of 403 whose body carries a pricing URL. Treating that as an auth problem would
// disable a perfectly valid account — which is exactly the misdiagnosis this
// function exists to prevent.
func DetectNoEntitlement(values ...string) bool {
	return slices.ContainsFunc(values, func(value string) bool { return strings.Contains(value, pricingURLFragment) })
}

// entitlementError renders an entitlement refusal.
//
// It deliberately omits the upstream HTTP status. The shared account classifier
// reads any `status=403` in an error string as "this credential is forbidden" and
// would retire a perfectly valid account; the failure here is about the plan, and
// the reason is carried in operator terms instead.
func entitlementError(body string) error {
	detail := decodeBodyMessage(body)
	if detail == "" {
		detail = util.Truncate(body, 300)
	}
	if code := envelopeCode([]byte(body)); code != "" {
		return fmt.Errorf("%w (upstream code=%s: %s)", ErrNoEntitlement, code, detail)
	}
	return fmt.Errorf("%w (%s)", ErrNoEntitlement, detail)
}

// decodeBodyMessage unwraps one level of escaping so the operator sees readable
// text instead of a JSON string literal.
func decodeBodyMessage(body string) string {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return ""
	}
	var decoded struct {
		Message string `json:"message"`
	}
	if json.Unmarshal([]byte(trimmed), &decoded) == nil && strings.TrimSpace(decoded.Message) != "" {
		inner := strings.TrimSpace(decoded.Message)
		// The message is itself sometimes a JSON document; one more level of
		// unwrapping is all the gateway ever uses.
		var nested struct {
			Message string `json:"message"`
		}
		if json.Unmarshal([]byte(inner), &nested) == nil && strings.TrimSpace(nested.Message) != "" {
			return strings.TrimSpace(nested.Message)
		}
		return inner
	}
	return ""
}

// apiError renders an upstream failure in the form the shared error classifier
// understands, so account health and retry decisions stay accurate.
func apiError(method, rawURL string, status int, raw []byte) error {
	body := strings.TrimSpace(string(raw))
	code := envelopeCode(raw)
	parts := []string{fmt.Sprintf("status=%d", status), fmt.Sprintf("method=%s", method), fmt.Sprintf("path=%s", signPath(rawURL))}
	if code != "" {
		parts = append(parts, "code="+code)
	}
	if body != "" {
		parts = append(parts, "message="+util.Truncate(body, 300))
	}
	return fmt.Errorf("qoder API error: %s", strings.Join(parts, ", "))
}

// sharedQueueRefusal reports whether a failure describes an upstream-wide
// queue/service refusal, whatever shape the envelope arrived in.
//
// The business code is the usual marker and envelopeCode reads it, but the same
// payload has reached production under an envelope that reader could not unwrap.
// Falling through to the status branch then labelled it "qoder upstream rejected
// the credential" -- an authentication failure for a working account -- and,
// worse, dropped the wait the upstream had asked for, so every retry came back
// before the queue cleared. Matching the payload itself makes the classification
// independent of how many times the gateway nested it.
func sharedQueueRefusal(values ...string) bool {
	for _, value := range values {
		lower := markerText(value)
		if strings.Contains(lower, busyCode) ||
			strings.Contains(lower, `"isqueued":true`) ||
			strings.Contains(lower, `"serviceavailable":false`) {
			return true
		}
	}
	return false
}

// envelopeCode returns the business code of an upstream error body. The gateway
// reports 10605 (queue/concurrency refusal) as a string inside a 401/403
// envelope, which is why the code is read before the HTTP status is trusted.
func envelopeCode(raw []byte) string { return envelopeCodeDepth(bytes.TrimSpace(raw), 0) }

func envelopeCodeDepth(raw []byte, depth int) string {
	if depth > 4 || len(raw) == 0 {
		return ""
	}
	// A body that arrives as a JSON *string* -- "\"{\\\"code\\\":\\\"10605\\\"...}\"" --
	// hides the envelope one level deeper. Reading it as opaque text is how a
	// 10605 queue refusal came back as "qoder upstream rejected the credential",
	// which then had the account parked as a rate limit for 30s a turn until the
	// whole pool was empty. Unwrap the string and look again.
	if raw[0] == '"' {
		var nested string
		if err := json.Unmarshal(raw, &nested); err != nil {
			return ""
		}
		return envelopeCodeDepth([]byte(strings.TrimSpace(nested)), depth+1)
	}
	if raw[0] != '{' {
		return ""
	}
	var env struct {
		Code    json.RawMessage `json:"code"`
		MsgCode json.RawMessage `json:"msgCode"`
		Message json.RawMessage `json:"message"`
		Body    json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return ""
	}
	for _, field := range []json.RawMessage{env.Code, env.MsgCode} {
		if len(field) == 0 {
			continue
		}
		var asString string
		if err := json.Unmarshal(field, &asString); err == nil && strings.TrimSpace(asString) != "" {
			return strings.TrimSpace(asString)
		}
		var asNumber json.Number
		if err := json.Unmarshal(field, &asNumber); err == nil && asNumber.String() != "" && asNumber.String() != "0" {
			return asNumber.String()
		}
	}
	for _, field := range []json.RawMessage{env.Message, env.Body} {
		if len(field) == 0 || string(field) == "null" {
			continue
		}
		var nested string
		if json.Unmarshal(field, &nested) == nil {
			if code := envelopeCodeDepth([]byte(strings.TrimSpace(nested)), depth+1); code != "" {
				return code
			}
			continue
		}
		if code := envelopeCodeDepth(bytes.TrimSpace(field), depth+1); code != "" {
			return code
		}
	}
	return ""
}
