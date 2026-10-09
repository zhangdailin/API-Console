package workbuddy

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"orchids-api/internal/httpclient"
	"sort"
	"strings"
	"sync"
	"time"

	"encoding/json"

	"orchids-api/internal/refreshqueue"
	"orchids-api/internal/store"
	"orchids-api/internal/util"
)

// AccountUpdater is the subset of the account store the client needs to persist
// rotated refresh tokens. It is satisfied by *store.Store.
type AccountUpdater interface {
	UpdateWorkBuddyCredentials(ctx context.Context, id int64, patch store.WorkBuddyCredentialPatch) error
}

// Credentials holds the WorkBuddy account material needed to talk to the
// international backend. RefreshToken is the durable credential: the desktop
// client stores it beside the access token, and Keycloak rotates it on every
// refresh.
type Credentials struct {
	AccessToken  string
	RefreshToken string
	UID          string
	Email        string
	ExpiresAt    time.Time
}

// Token returns the stored access token when it is still comfortably valid.
func (c Credentials) Token(now time.Time) (string, bool) {
	if strings.TrimSpace(c.AccessToken) == "" {
		return "", false
	}
	if c.ExpiresAt.IsZero() {
		// An opaque token without a decodable expiry is used as-is; the
		// upstream will answer 12153 if it is stale.
		return c.AccessToken, true
	}
	if c.ExpiresAt.Sub(now) <= minRefreshLead {
		return c.AccessToken, false
	}
	return c.AccessToken, true
}

func ResolveCredentials(acc *store.Account) Credentials {
	if acc == nil {
		return Credentials{}
	}

	creds := Credentials{
		AccessToken:  normalizeToken(acc.WorkBuddyAccessToken),
		RefreshToken: normalizeToken(acc.WorkBuddyRefreshToken),
		UID:          strings.TrimSpace(acc.WorkBuddyUID),
		Email:        strings.TrimSpace(acc.Email),
		ExpiresAt:    acc.WorkBuddyExpiresAt,
	}

	// Fill what the token itself can prove. The stored expiry is intentionally
	// overridden when the token is decodable: a stale stored value would make the
	// client refresh on every request.
	accessClaims := DecodeClaims(creds.AccessToken)
	if creds.UID == "" {
		creds.UID = accessClaims.Sub
	}
	if creds.Email == "" {
		creds.Email = accessClaims.Email
	}
	if accessClaims.ExpiresAt > 0 {
		creds.ExpiresAt = time.Unix(accessClaims.ExpiresAt, 0)
	}
	return creds
}

// Fields projects the credentials onto the account fields that own them. It is
// the single mapping used by both the client and the admin API, so a credential
// added through any path (OAuth login, manual paste, import) stores the same
// values in the same places.
func (c Credentials) Fields() (accessToken, refreshToken, uid, email string, expiresAt time.Time) {
	return strings.TrimSpace(c.AccessToken),
		strings.TrimSpace(c.RefreshToken),
		strings.TrimSpace(c.UID),
		strings.TrimSpace(c.Email),
		c.ExpiresAt
}

// HasCredential reports whether anything usable was supplied.
func (c Credentials) HasCredential() bool {
	return strings.TrimSpace(c.AccessToken) != "" || strings.TrimSpace(c.RefreshToken) != ""
}

func normalizeToken(value string) string {
	value = strings.Trim(strings.TrimSpace(value), `"'`)
	if value == "" {
		return ""
	}
	if idx := strings.Index(value, "="); idx > 0 {
		key := strings.ToLower(strings.TrimSpace(value[:idx]))
		if key == "bearer" || key == "authorization" {
			value = strings.TrimSpace(value[idx+1:])
		}
	}
	value = strings.TrimSpace(strings.TrimPrefix(value, "Bearer "))
	value = strings.TrimSpace(strings.TrimPrefix(value, "bearer "))
	return strings.Trim(value, `"'`)
}

// Claims is the subset of the Keycloak access-token claims the gateway uses.
type Claims struct {
	Sub       string `json:"sub"`
	Email     string `json:"email"`
	Scope     string `json:"scope"`
	ExpiresAt int64  `json:"exp"`
}

// DecodeClaims reads the JWT payload without verifying the signature; the
// upstream remains the authority on validity.
func DecodeClaims(token string) Claims {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) < 2 {
		return Claims{}
	}
	segment := parts[1]
	segment = strings.ReplaceAll(segment, "-", "+")
	segment = strings.ReplaceAll(segment, "_", "/")
	if pad := len(segment) % 4; pad != 0 {
		segment += strings.Repeat("=", 4-pad)
	}
	raw, err := base64.StdEncoding.DecodeString(segment)
	if err != nil {
		return Claims{}
	}
	var claims Claims
	if err := json.Unmarshal(raw, &claims); err != nil {
		return Claims{}
	}
	return claims
}

func applyHeaders(req *http.Request, accessToken, uid, accept string) {
	if accept != "" {
		req.Header.Set("Accept", accept)
	} else {
		req.Header.Set("Accept", "application/json, text/plain, */*")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", originReferer)
	req.Header.Set("Referer", originReferer+"/")
	req.Header.Set("User-Agent", clientUA)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("X-CodeBuddy-Request", "1")
	req.Header.Set("Accept-Language", "en-US")
	if accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	} else {
		req.Header.Set("X-No-Authorization", "1")
	}
	if uid != "" {
		req.Header.Set("X-User-Id", uid)
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}
	req.Header.Set("X-No-Enterprise-Id", "1")
	req.Header.Set("X-No-Department-Info", "1")
	req.Header.Set("X-Product", "SaaS")
}

// applyChatHeaders matches the current WorkBuddy desktop chat fingerprint.
// The attribution and request-family headers are not decoration: the upstream
// uses them to group one user turn and to apply client-specific rate policy.
func applyChatHeaders(req *http.Request, accessToken, uid, conversationID, requestID, traceID string) {
	applyHeaders(req, accessToken, uid, "application/json, text/event-stream")
	req.Header.Del("X-No-Department-Info")
	req.Header.Set("X-Domain", "www.workbuddy.ai")
	req.Header.Set("X-Agent-Purpose", "conversation")
	req.Header.Set("X-IDE-Name", "WorkBuddy")
	req.Header.Set("X-IDE-Type", "WorkBuddy")
	req.Header.Set("X-IDE-Version", clientVersion)
	req.Header.Set("X-Product", "WorkBuddy")

	conversationID = strings.TrimSpace(conversationID)
	if conversationID != "" {
		req.Header.Set("X-Conversation-ID", conversationID)
	}
	conversationRequestID := strings.TrimSpace(requestID)
	if conversationRequestID == "" {
		conversationRequestID = newWorkBuddyMessageID()
	}
	messageID := newWorkBuddyMessageID()
	req.Header.Set("X-Conversation-Request-ID", conversationRequestID)
	req.Header.Set("X-Conversation-Message-ID", messageID)
	req.Header.Set("X-Request-ID", messageID)
	req.Header.Set("X-Root-Request-ID", conversationRequestID)
	traceID = strings.TrimSpace(traceID)
	traceID = util.FirstNonEmptyUntrimmed(traceID, conversationRequestID)
	req.Header.Set("X-Trace-ID", traceID)
	b3TraceID := validWorkBuddyTraceID(conversationRequestID)
	b3TraceID = util.FirstNonEmptyUntrimmed(b3TraceID, messageID)
	req.Header.Set("X-B3-TraceId", b3TraceID)
	req.Header.Set("X-B3-SpanId", messageID[:16])
	req.Header.Set("X-B3-Sampled", "1")
}

func newWorkBuddyMessageID() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err == nil {
		return hex.EncodeToString(raw)
	}
	// The fallback remains a valid 32-hex B3 id. It is deliberately local to
	// correlation and is never used as authentication material.
	return fmt.Sprintf("%032x", time.Now().UnixNano())
}

func validWorkBuddyTraceID(value string) string {
	if len(value) != 16 && len(value) != 32 {
		return ""
	}
	for _, ch := range value {
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F')) {
			return ""
		}
	}
	return strings.ToLower(value)
}

// envelope mirrors the {code,msg,requestId,data} wrapper used by every /v2 and
// /v3 endpoint.
type envelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

var errorHints = map[int]string{
	CodeLoginPending:  "workbuddy login has not completed; finish the browser authorization",
	CodeModelThrottle: "this model hit the per-model frequency limit; wait for the window reset or use another model",
	CodePolicyBlocked: "workbuddy blocked the request by security policy (unapproved channel); the prompt may carry a client identity marker",
	CodeSessionDead:   "workbuddy session is dead (Offline user session not found); re-login the account",
}

// APIError is a typed WorkBuddy business failure. Scope-sensitive callers can
// inspect Code without guessing from prose, while Error retains the stable
// status=/code= markers used by the shared classifiers.
type APIError struct {
	HTTPStatus int
	Code       int
	Message    string
	RetryDelay time.Duration
}

func (e *APIError) Error() string {
	if e == nil {
		return "workbuddy API error"
	}
	status := e.HTTPStatus
	// 12153 is an authentication failure even when the streaming endpoint wraps
	// it in HTTP 200. Expose the semantic status without losing the wire status.
	parts := make([]string, 0, 4)
	switch {
	case e.Code == CodeSessionDead && status != http.StatusUnauthorized:
		parts = append(parts, "status=401", fmt.Sprintf("upstream_status=%d", status))
	case e.Code == CodeModelThrottle && status != http.StatusTooManyRequests:
		parts = append(parts, "status=429", fmt.Sprintf("upstream_status=%d", status))
	default:
		parts = append(parts, fmt.Sprintf("status=%d", status))
	}
	if e.Code != 0 {
		parts = append(parts, fmt.Sprintf("code=%d", e.Code))
	}
	if e.Message != "" {
		parts = append(parts, "message="+util.Truncate(e.Message, 300))
	}
	text := "workbuddy API error: " + strings.Join(parts, ", ")
	if hint := errorHints[e.Code]; hint != "" {
		text += " (" + hint + ")"
	}
	return text
}

func (e *APIError) RetryAfter() time.Duration {
	if e == nil {
		return 0
	}
	return e.RetryDelay
}

// apiError renders an upstream failure in typed form.
func apiError(status int, raw []byte) error { return apiErrorWithRetry(status, raw, 0) }

func apiErrorWithRetry(status int, raw []byte, retryDelay time.Duration) error {
	body := strings.TrimSpace(string(raw))
	code := 0
	msg := ""
	trimmed := strings.TrimSpace(body)
	if strings.HasPrefix(trimmed, "{") {
		var env envelope
		if err := json.Unmarshal([]byte(trimmed), &env); err == nil {
			code = env.Code
			msg = strings.TrimSpace(env.Msg)
		}
	}
	if code != 0 && msg == "" {
		msg = body
	}
	msg = util.FirstNonEmptyUntrimmed(msg, body)
	return &APIError{HTTPStatus: status, Code: code, Message: msg, RetryDelay: retryDelay}
}

// unwrapEnvelope validates the business envelope and returns data.
func unwrapEnvelope(status int, raw []byte) (json.RawMessage, error) {
	if status >= http.StatusBadRequest {
		return nil, apiError(status, raw)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("workbuddy response is not JSON: %s", util.Truncate(string(raw), 200))
	}
	if env.Code != 0 {
		if env.Code == CodeLoginPending {
			// Login polling is expected to observe this while the browser step
			// is still running, so it must be distinguishable from a failure.
			return nil, fmt.Errorf("%w: %s", ErrAuthPending, util.Truncate(env.Msg, 120))
		}
		return nil, apiError(status, raw)
	}
	return env.Data, nil
}

// tokenUpdater owns access-token renewal for one account. Keycloak rotates the
// refresh token on every refresh, so the rotated pair is persisted back to the
// account record.
type tokenUpdater struct {
	baseURL      string
	httpClient   *http.Client
	accountStore AccountUpdater
	account      store.Account
	accountID    int64

	mu                   sync.Mutex
	token                string
	until                time.Time
	creds                Credentials
	initialized          bool
	dirty                bool
	dirtyExpectedRefresh string
}

func newTokenUpdater(baseURL string, httpClient *http.Client, accountStore AccountUpdater, acc *store.Account) *tokenUpdater {
	updater := &tokenUpdater{
		baseURL:      strings.TrimSuffix(baseURL, "/"),
		httpClient:   httpClient,
		accountStore: accountStore,
	}
	if acc != nil {
		updater.account = *acc
		updater.accountID = acc.ID
	}
	return updater
}

func (t *tokenUpdater) SetAccountStore(accountStore AccountUpdater) {
	t.mu.Lock()
	t.accountStore = accountStore
	t.mu.Unlock()
}

// Token returns a valid access token, refreshing only when needed.
func (t *tokenUpdater) Token(ctx context.Context, creds Credentials) (string, error) {
	now := time.Now()

	t.mu.Lock()
	defer t.mu.Unlock()
	t.initialize(creds)
	release, err := t.acquireLatest(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	if t.dirty {
		if err := t.persist(ctx, t.creds, t.dirtyExpectedRefresh); err != nil {
			return "", err
		}
		t.dirty = false
		t.dirtyExpectedRefresh = ""
	}
	creds = t.creds

	if t.token != "" && t.until.Sub(now) > minRefreshLead {
		return t.token, nil
	}

	if token, ok := creds.Token(now); ok {
		t.token = token
		t.until = creds.ExpiresAt
		return token, nil
	}

	if strings.TrimSpace(creds.RefreshToken) == "" {
		if strings.TrimSpace(creds.AccessToken) != "" {
			// Only an access token is available and it is close to expiry.
			// Try it anyway rather than failing the request outright.
			t.token = creds.AccessToken
			return creds.AccessToken, nil
		}
		return "", fmt.Errorf("workbuddy account is missing credentials: paste the refresh token from the WorkBuddy desktop session")
	}

	refreshed, err := t.refresh(ctx, creds)
	if err != nil {
		if strings.TrimSpace(creds.AccessToken) != "" {
			t.token = creds.AccessToken
			return creds.AccessToken, nil
		}
		return "", err
	}
	t.token = refreshed.AccessToken
	t.until = refreshed.ExpiresAt
	t.creds = refreshed
	t.dirty = true
	t.dirtyExpectedRefresh = creds.RefreshToken
	if err := t.persist(ctx, refreshed, t.dirtyExpectedRefresh); err != nil {
		return "", err
	}
	t.dirty = false
	t.dirtyExpectedRefresh = ""
	return refreshed.AccessToken, nil
}

// RefreshNow forces one refresh cycle, used by account verification.
func (t *tokenUpdater) RefreshNow(ctx context.Context, creds Credentials) (Credentials, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.initialize(creds)
	previous := t.creds.AccessToken
	release, err := t.acquireLatest(ctx)
	if err != nil {
		return t.creds, err
	}
	defer release()
	if token, valid := t.creds.Token(time.Now()); valid && token != previous {
		return t.creds, nil
	}
	if t.dirty {
		if err := t.persist(ctx, t.creds, t.dirtyExpectedRefresh); err != nil {
			return t.creds, err
		}
		t.dirty = false
		t.dirtyExpectedRefresh = ""
	}
	creds = t.creds
	if strings.TrimSpace(creds.RefreshToken) == "" {
		return creds, fmt.Errorf("workbuddy account is missing a refresh token")
	}
	refreshed, err := t.refresh(ctx, creds)
	if err != nil {
		return creds, err
	}
	t.token = refreshed.AccessToken
	t.until = refreshed.ExpiresAt
	t.creds = refreshed
	t.dirty = true
	t.dirtyExpectedRefresh = creds.RefreshToken
	if err := t.persist(ctx, refreshed, t.dirtyExpectedRefresh); err != nil {
		return refreshed, err
	}
	t.dirty = false
	t.dirtyExpectedRefresh = ""
	return refreshed, nil
}

func (t *tokenUpdater) initialize(creds Credentials) {
	if t.initialized {
		return
	}
	t.creds = creds
	t.initialized = true
}

// Called with t.mu held. Flush a pending rotation before reading durable state.
func (t *tokenUpdater) acquireLatest(ctx context.Context) (func(), error) {
	release, err := refreshqueue.AcquireCredential(ctx, "workbuddy", t.accountStore, t.accountID)
	if err != nil {
		return nil, err
	}
	if t.dirty {
		if err := t.persist(ctx, t.creds, t.dirtyExpectedRefresh); err != nil {
			release()
			return nil, err
		}
		t.dirty = false
		t.dirtyExpectedRefresh = ""
	}
	latest, err := refreshqueue.LatestAccount(ctx, t.accountStore, t.accountID)
	if err != nil {
		release()
		return nil, err
	}
	if latest != nil {
		t.creds = ResolveCredentials(latest)
		t.token = t.creds.AccessToken
		t.until = t.creds.ExpiresAt
	}
	return release, nil
}

func (t *tokenUpdater) refresh(ctx context.Context, creds Credentials) (Credentials, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.baseURL+"/v2/plugin/auth/token/refresh", nil)
	if err != nil {
		return creds, err
	}
	applyHeaders(req, creds.AccessToken, creds.UID, "application/json")
	req.Header.Set("X-Refresh-Token", creds.RefreshToken)
	req.Header.Set("X-Auth-Refresh-Source", "plugin")

	resp, raw, err := httpclient.DoReadBody(t.httpClient, req, 64<<10)
	if err != nil {
		return creds, fmt.Errorf("failed to refresh workbuddy token: %w", err)
	}
	data, err := unwrapEnvelope(resp.StatusCode, raw)
	if err != nil {
		return creds, fmt.Errorf("failed to refresh workbuddy token: %w", err)
	}

	var payload struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return creds, fmt.Errorf("failed to decode workbuddy refresh response: %w", err)
	}
	if strings.TrimSpace(payload.AccessToken) == "" {
		return creds, fmt.Errorf("workbuddy token refresh returned no accessToken; re-login required")
	}

	out := creds
	out.AccessToken = strings.TrimSpace(payload.AccessToken)
	if token := strings.TrimSpace(payload.RefreshToken); token != "" {
		// Keycloak rotates the refresh token; keeping the old one breaks the
		// next refresh.
		out.RefreshToken = token
	}
	if payload.ExpiresIn > 0 {
		out.ExpiresAt = time.Now().Add(time.Duration(payload.ExpiresIn) * time.Second)
	} else {
		out.ExpiresAt = time.Time{}
	}
	if claims := DecodeClaims(out.AccessToken); claims.ExpiresAt > 0 {
		out.ExpiresAt = time.Unix(claims.ExpiresAt, 0)
		if out.UID == "" {
			out.UID = claims.Sub
		}
		if out.Email == "" {
			out.Email = claims.Email
		}
	}
	return out, nil
}

func (t *tokenUpdater) persist(parent context.Context, creds Credentials, expectedRefreshToken string) error {
	if t.accountStore == nil || t.accountID == 0 {
		return nil
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
	defer cancel()
	patch := store.WorkBuddyCredentialPatch{
		ExpectedRefreshToken: expectedRefreshToken,
		AccessToken:          creds.AccessToken,
		RefreshToken:         creds.RefreshToken,
		ExpiresAt:            creds.ExpiresAt,
		UID:                  creds.UID,
		Email:                creds.Email,
	}
	if err := t.accountStore.UpdateWorkBuddyCredentials(writeCtx, t.accountID, patch); err != nil {
		return fmt.Errorf("persist rotated workbuddy credential: %w", err)
	}
	return nil
}

// WorkBuddyModel is one entry of the /v3/config catalog.
type WorkBuddyModel struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	MaxInputTokens  int64  `json:"maxInputTokens"`
	MaxOutputTokens int64  `json:"maxOutputTokens"`
	SupportsTools   bool   `json:"supportsToolCall"`
	SupportsReason  bool   `json:"supportsReasoning"`
	Disabled        bool   `json:"disabled"`
	Reasoning       struct {
		Effort string `json:"effort"`
	} `json:"reasoning"`
}

type configResponse struct {
	Models []WorkBuddyModel `json:"models"`
	Agents []struct {
		Name   string   `json:"name"`
		Models []string `json:"models"`
	} `json:"agents"`
}

// FetchModels reads the account-scoped model catalog. The effective set is the
// `cli` agent whitelist intersected with the model records.
func (c *Client) FetchModels(ctx context.Context) ([]WorkBuddyModel, error) {
	if c == nil {
		return nil, fmt.Errorf("workbuddy client is nil")
	}
	accessToken, err := c.ensureAccessToken(ctx)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v3/config", nil)
	if err != nil {
		return nil, err
	}
	applyHeaders(req, accessToken, c.creds.UID, "application/json")

	resp, raw, err := httpclient.DoReadBody(c.httpClient, req, 8<<20)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch workbuddy config: %w", err)
	}
	data, err := unwrapEnvelope(resp.StatusCode, raw)
	if err != nil {
		return nil, err
	}

	var cfg configResponse
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to decode workbuddy config: %w", err)
	}

	allow := make(map[string]struct{})
	for _, agent := range cfg.Agents {
		if !strings.EqualFold(strings.TrimSpace(agent.Name), "cli") {
			continue
		}
		for _, id := range agent.Models {
			if id = strings.TrimSpace(id); id != "" {
				allow[id] = struct{}{}
			}
		}
	}

	// advertised is everything the account may run, before the CLI restriction.
	advertised := make([]WorkBuddyModel, 0, len(cfg.Models))
	for _, model := range cfg.Models {
		id := strings.TrimSpace(model.ID)
		if id == "" || model.Disabled {
			continue
		}
		model.ID = id
		if strings.TrimSpace(model.Name) == "" {
			model.Name = id
		}
		advertised = append(advertised, model)
	}
	if len(advertised) == 0 {
		return nil, fmt.Errorf("workbuddy config advertised no enabled models")
	}

	// No CLI restriction declared: the account may run what it advertises.
	if len(allow) == 0 {
		return advertised, nil
	}

	out := make([]WorkBuddyModel, 0, len(advertised))
	for _, model := range advertised {
		if _, ok := allow[model.ID]; ok {
			out = append(out, model)
		}
	}
	if len(out) > 0 {
		return out, nil
	}

	// The whitelist named models that share no identifier with the advertised
	// ones. That is an id-space mismatch between two upstream lists, not an empty
	// catalog, and the difference matters: reporting it as "no models" failed the
	// whole channel's model refresh, so no WorkBuddy model could be published, no
	// request could be routed to the channel, and the entire channel went dark
	// over a naming change. The upstream still refuses a model the account may not
	// run, so serving the advertised list is the safe direction to fail in — and
	// the mismatch is logged loudly enough to be corrected upstream.
	slog.Warn("workbuddy cli whitelist matches no advertised model; serving the advertised list instead",
		"advertised_count", len(advertised),
		"cli_whitelist_count", len(allow),
		"advertised_sample", sampleModelIDs(advertised, 5),
		"cli_whitelist_sample", sampleSetKeys(allow, 5))
	return advertised, nil
}

// sampleModelIDs returns up to limit model ids, so a diagnostic that names both
// sides of a mismatch stays bounded.
func sampleModelIDs(models []WorkBuddyModel, limit int) []string {
	out := make([]string, 0, limit)
	for _, model := range models {
		if len(out) == limit {
			break
		}
		out = append(out, model.ID)
	}
	return out
}

// sampleSetKeys returns up to limit keys of a set, sorted so the log line is
// stable across runs and can be compared between them.
func sampleSetKeys(set map[string]struct{}, limit int) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) > limit {
		keys = keys[:limit]
	}
	return keys
}
