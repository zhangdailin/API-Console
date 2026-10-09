package grok

import (
	"bytes"
	"context"
	"crypto/sha1"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptrace"
	"orchids-api/internal/httpclient"
	"strings"
	"sync/atomic"
	"time"

	"encoding/json"

	"orchids-api/internal/accountpolicy"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/grok/egress"
	"orchids-api/internal/modelcatalog"
	"orchids-api/internal/store"
	"orchids-api/internal/util"
)

// Build CLI (cli-chat-proxy.grok.com) upstream. It speaks the standard OpenAI
// Responses protocol authenticated with a Bearer OAuth access token, unlike the
// retired website or retired developer plane protocols.

const (
	defaultCLIBaseURL = "https://cli-chat-proxy.grok.com/v1"
	// cli-chat-proxy rejects generic boolean token-auth headers. These values
	// mirror the official Grok shell identity used with Build OAuth tokens.
	defaultCLITokenAuth             = "xai-grok-cli"
	defaultCLIClientMode            = "headless"
	defaultCLIResponseHeaderTimeout = 30 * time.Second
)

var errCLIResponseHeaderTimeout = fmt.Errorf("grok upstream response header timeout")

// CLIClient implements the Grok Build CLI upstream protocol.
type CLIClient struct {
	cfg                   *config.Config
	httpClient            *http.Client
	oauth                 *CLIOAuth
	egress                *egress.Manager
	responseHeaderTimeout time.Duration
}

// NewCLIClient builds a CLI upstream client from configuration.
func NewCLIClient(cfg *config.Config) *CLIClient {
	client := &CLIClient{cfg: cfg, responseHeaderTimeout: defaultCLIResponseHeaderTimeout}
	// Shared browser client keeps the utls Chrome TLS fingerprint; the CLI
	// upstream tolerates browser-like TLS even though headers are CLI identity.
	client.httpClient = httpclient.GetSharedBrowserHTTPClientWithLimits("cli|"+httpclient.GenerateProxyKeyFromConfig(cfg), cfg.GrokRequestTimeout(), 0, httpclient.ProxyFuncFromConfig(cfg), cfg)
	client.oauth = NewCLIOAuth(cfg, client.httpClient)
	client.egress = egress.NewManager(cfg)
	return client
}

// SetAccountStore wires a durable store so OAuth token refreshes performed on
// the request path are written back (including rotated refresh tokens).
func (c *CLIClient) SetAccountStore(s *store.Store) {
	if c == nil || c.oauth == nil {
		return
	}
	c.oauth.SetAccountStore(s)
}

// baseURL returns the CLI proxy base URL (defaults to the official gateway).
func (c *CLIClient) baseURL() string {
	if c != nil && c.cfg != nil {
		return c.cfg.GrokCLIBaseURLOrDefault()
	}
	return defaultCLIBaseURL
}

// OAuthAccessToken returns a valid access token for the account, refreshing it
// in memory when needed. Callers persist the mutated account fields.
func (c *CLIClient) OAuthAccessToken(ctx context.Context, acc *store.Account) (string, error) {
	if c == nil || c.oauth == nil {
		return "", fmt.Errorf("grok cli oauth not configured")
	}
	return c.oauth.AccessToken(ctx, acc)
}

// cliHeaders builds the Build CLI request headers for an OAuth account.
func (c *CLIClient) cliHeaders(acc *store.Account, accessToken string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+accessToken)
	h.Set("X-XAI-Token-Auth", defaultCLITokenAuth)
	h.Set("x-grok-client-version", c.clientVersion())
	h.Set("x-grok-client-identifier", c.clientIdentifier())
	h.Set("x-grok-client-mode", defaultCLIClientMode)
	h.Set("Accept", "application/json")
	h.Set("Accept-Encoding", "gzip")
	h.Set("User-Agent", c.userAgent())
	h.Set("x-xai-request-id", randomUUID())
	if acc != nil {
		if teamID := strings.TrimSpace(acc.TeamID); teamID != "" {
			h.Set("x-grok-team-id", teamID)
		}
		if userID := strings.TrimSpace(acc.UserID); userID != "" {
			h.Set("x-grok-user-id", userID)
			h.Set("x-userid", userID) // Billing's alias must also use the refreshed identity.
		}
	}
	return h
}

func (c *CLIClient) userAgent() string {
	if c != nil && c.cfg != nil {
		return c.cfg.GrokCLIUserAgentOrDefault()
	}
	return "grok-shell/1.0.40 (linux; x86_64)"
}

func (c *CLIClient) clientVersion() string {
	if c != nil && c.cfg != nil {
		return c.cfg.GrokCLIClientVersionOrDefault()
	}
	return "1.0.40"
}

func (c *CLIClient) clientIdentifier() string {
	if c != nil && c.cfg != nil {
		return c.cfg.GrokCLIClientIdentifierOrDefault()
	}
	return "grok-shell"
}

// doResponses issues a standard Responses request to the CLI proxy. It ensures a
// valid access token (refreshing if needed) then POSTs the payload, returning
// the raw upstream response (SSE or JSON) for the caller to stream or collect.
func (c *CLIClient) doResponsesAt(ctx context.Context, acc *store.Account, path string, payload map[string]interface{}) (*http.Response, error) {
	if acc == nil {
		return nil, fmt.Errorf("empty cli account")
	}
	ctx = withRateLimitAccount(ctx, acc)
	modelID := strings.TrimSpace(fmt.Sprint(payload["model"]))
	if err := waitScopedRateLimit(ctx, ProviderBuild, acc.OAuthAccessToken, modelID, c.cfg.GrokRequestsPerSecond()); err != nil {
		return nil, err
	}
	authRetried := false
	for {
		resp, err := c.doResponsesOnceAt(ctx, acc, path, payload)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusOK {
			return resp, nil
		}
		raw, headerCopy := readBoundedResponse(resp)
		if resp.StatusCode == http.StatusUnauthorized && !authRetried && c.oauth != nil && strings.TrimSpace(acc.OAuthRefreshToken) != "" {
			authRetried = true
			if _, refreshErr := c.oauth.ForceRefresh(ctx, acc); refreshErr == nil {
				continue
			} else if IsCLIPermanentOAuthError(refreshErr) {
				// The refresh credential is permanently unusable. Surface the typed
				// OAuth failure so the retry loop retires this identity and switches.
				return nil, refreshErr
			}
		}

		if resp.StatusCode == http.StatusTooManyRequests {
			// A 429 is also how the upstream announces a spent Free allowance, and that
			// refusal is the only place the real actual/limit pair appears. Record it
			// before the throttle bookkeeping, which would otherwise be the only thing
			// this refusal leaves behind.
			c.noteConfirmedFreeQuota(ctx, acc, raw)
			if meta := noteScopedRateLimit(ctx, ProviderBuild, acc.OAuthAccessToken, modelID, resp.StatusCode, resp.Header, raw); meta != nil {
				// Keep the selected account's durable diagnostic state in sync. The
				// in-memory team/model registry remains authoritative for waiting;
				// this timestamp is only for admin visibility and restart diagnostics.
				bounded := accountpolicy.BoundRateLimitCooldown(meta.RetryAfter)
				cooldownUntil := time.Now().Add(bounded)
				if bounded > 0 && (acc.QuotaResetAt.IsZero() || cooldownUntil.After(acc.QuotaResetAt)) {
					acc.QuotaResetAt = cooldownUntil
					// The throttle is scoped to the model that was asked for: the
					// account's OTHER models stay selectable, so the verdict is
					// recorded per model instead of on StatusCode.
					store.RecordModelCooldown(acc, modelID, cooldownUntil)
					if c.oauth != nil && c.oauth.store != nil && acc.ID != 0 {
						if err := c.oauth.store.UpdateAccount(ctx, acc); err != nil {
							slog.Warn("grok cli: failed to persist team cooldown diagnostic", "account_id", acc.ID, "error", err)
						}
					}
				}
			}
			recordCLIUpstreamStatus(resp.StatusCode)
			return nil, newCLIUpstreamError(resp.StatusCode, headerCopy, raw)
		}

		kind := ClassifyUpstreamResponse(resp.StatusCode, resp.Header, raw)
		if kind == UpstreamErrorGenericForbidden {
			recordGenericForbidden()
		}

		// A spent Free allowance is not always reported as a 429: the same refusal can
		// arrive as a 402/403, and it is the only place the real actual/limit pair
		// appears.
		c.noteConfirmedFreeQuota(ctx, acc, raw)
		recordCLIUpstreamStatus(resp.StatusCode)
		return nil, newCLIUpstreamError(resp.StatusCode, headerCopy, raw)
	}
}

// noteConfirmedFreeQuota persists the Free window the upstream reported in a refusal.
//
// It is deliberately fire-and-forget: a refusal that cannot be written back must not
// turn into a request failure, and it must not overwrite anything when the response
// was not a Free refusal at all.
func (c *CLIClient) noteConfirmedFreeQuota(ctx context.Context, acc *store.Account, body []byte) {
	if c == nil || c.oauth == nil || c.oauth.store == nil || acc == nil || acc.ID == 0 {
		return
	}
	// A refusal that names one model ("... free usage for model X") is not an
	// account-wide exhaustion. Cooling the whole credential for 24h also removed
	// it from video, image and every other model it could still serve.
	if model := requestModelFromContext(ctx); model != "" && modelScopedFreeQuotaRefusal(body) {
		store.RecordModelCooldown(acc, model, time.Now().Add(FreeBuildUsageWindow))
		if err := c.oauth.store.UpdateAccount(ctx, acc); err != nil {
			slog.Warn("grok cli: failed to persist the model-scoped free quota window", "account_id", acc.ID, "model", model, "error", err)
		}
		return
	}
	if !ApplyFreeQuotaExhaustion(acc, body) {
		return
	}
	if err := c.oauth.store.UpdateAccount(ctx, acc); err != nil {
		slog.Warn("grok cli: failed to persist the confirmed free quota window", "account_id", acc.ID, "error", err)
	}
}

// modelScopedFreeQuotaRefusal reports whether a spent Free allowance refusal
// names a single model rather than the account's whole included window.
func modelScopedFreeQuotaRefusal(body []byte) bool {
	text := strings.ToLower(string(body))
	if !strings.Contains(text, "free usage") {
		return false
	}
	return strings.Contains(text, "for model") || strings.Contains(text, "model:")
}

func (c *CLIClient) doResponsesOnceAt(ctx context.Context, acc *store.Account, path string, payload map[string]interface{}) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	path = "/" + strings.TrimLeft(strings.TrimSpace(path), "/")
	headers := http.Header{"Content-Type": {"application/json"}}
	if session, _ := payload["prompt_cache_key"].(string); strings.TrimSpace(session) != "" {
		// The Build gateway expects session identity as a UUID. A raw sha256 hex
		// string is not one: the upstream then treats the session as unstable and
		// the prompt cache never warms.
		normalized := buildSessionUUID(strings.TrimSpace(session))
		headers.Set("x-grok-session-id", normalized)
		headers.Set("x-grok-conv-id", normalized)
	}
	// The official Build client identifies itself and traces each request.
	// Without these the upstream sees an anonymous caller, which is both a
	// weaker identity, and the reason session affinity behaved differently
	// without them.
	headers.Set("x-authenticateresponse", "authenticate-response")
	headers.Set("x-grok-agent-id", buildClientIdentifier(c))
	headers.Set("x-grok-model-override", strings.TrimSpace(fmt.Sprint(payload["model"])))
	if version := strings.TrimSpace(c.clientVersion()); version != "" {
		headers.Set("x-grok-client-version", version)
	}
	requestID := util.RandomHex(16)
	headers.Set("x-grok-req-id", requestID)
	headers.Set("traceparent", util.Traceparent(requestID))
	return c.request(ctx, acc, http.MethodPost, c.baseURL()+path, body, headers)
}

// buildSessionUUID normalizes a session identity to the UUID form the Build
// gateway expects. An existing UUID is passed through; anything else is mapped
// deterministically (UUIDv5 over a fixed namespace) so the same session keeps
// the same identity across requests.
func buildSessionUUID(seed string) string {
	trimmed := strings.TrimSpace(seed)
	if trimmed == "" {
		return ""
	}
	if isUUID(trimmed) {
		return trimmed
	}
	sum := sha1.Sum([]byte("orchids:grok-session:" + trimmed))
	var uuid [16]byte
	copy(uuid[:], sum[:16])
	uuid[6] = (uuid[6] & 0x0f) | 0x50 // version 5
	uuid[8] = (uuid[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", uuid[0:4], uuid[4:6], uuid[6:8], uuid[8:10], uuid[10:16])
}

func isUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, r := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if r != '-' {
				return false
			}
			continue
		}
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

// buildClientIdentifier is the agent identity the official Build client sends.
func buildClientIdentifier(c *CLIClient) string {
	if c != nil {
		return buildSessionUUID("agent:" + c.clientIdentifier())
	}
	return buildSessionUUID("agent:grok-shell")
}

// doResponseResource forwards GET/DELETE for a stored Build Responses
// resource. Non-2xx statuses are returned intact so the downstream API can
// preserve the upstream resource semantics.
func (c *CLIClient) doResponseResource(ctx context.Context, acc *store.Account, method, path, rawQuery string) (*http.Response, error) {
	path = "/" + strings.TrimLeft(strings.TrimSpace(path), "/")
	endpoint := c.baseURL() + path
	if strings.TrimSpace(rawQuery) != "" {
		endpoint += "?" + rawQuery
	}
	headers := http.Header{}
	for attempt := 0; ; attempt++ {
		resp, err := c.request(ctx, acc, method, endpoint, nil, headers)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusUnauthorized || attempt > 0 || strings.TrimSpace(acc.OAuthRefreshToken) == "" {
			return resp, nil
		}
		_ = resp.Body.Close()
		if _, err := c.oauth.ForceRefresh(ctx, acc); err != nil {
			return nil, err
		}
	}
}

// VerifyAccount checks a Build CLI OAuth account by minting a token and probing
// the CLI proxy models endpoint. Returns an upstream status string ("401",
// "403", ...) alongside the error so callers can mark the account.
func (c *CLIClient) VerifyAccount(ctx context.Context, acc *store.Account) (string, error) {
	if c == nil || c.oauth == nil {
		return "", fmt.Errorf("grok cli client not configured")
	}
	if acc == nil {
		return "", fmt.Errorf("missing cli account")
	}
	resp, err := c.request(ctx, acc, http.MethodGet, c.baseURL()+"/models", nil, nil)
	if err != nil {
		if oauthErr, ok := err.(*cliOAuthError); ok {
			return oauthErr.Status(), err
		}
		return "", err
	}
	if resp.StatusCode == http.StatusOK {
		_ = resp.Body.Close()
		return "", nil
	}
	raw, headerCopy := readBoundedResponse(resp)

	return classifyAccountStatusFromHTTP(resp.StatusCode), newCLIUpstreamError(resp.StatusCode, headerCopy, raw)
}

// buildModelCatalogEntry accepts both the current snake_case Build response and
// older camelCase response variants.
type buildModelCatalogEntry struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	ModelID string `json:"modelId"`
	Hidden  bool   `json:"hidden"`
	Meta    struct {
		Model   string `json:"model"`
		ModelID string `json:"modelId"`
		Hidden  bool   `json:"hidden"`
	} `json:"_meta"`
	ContextWindow            int64             `json:"context_window"`
	ContextWindowCamel       int64             `json:"contextWindow"`
	MaxCompletionTokens      int64             `json:"max_completion_tokens"`
	MaxCompletionTokensCamel int64             `json:"maxCompletionTokens"`
	ReasoningEffort          string            `json:"reasoning_effort"`
	ReasoningEffortCamel     string            `json:"reasoningEffort"`
	SupportsReasoningEffort  *bool             `json:"supports_reasoning_effort"`
	SupportsReasoningCamel   *bool             `json:"supportsReasoningEffort"`
	SupportsBackendSearch    bool              `json:"supports_backend_search"`
	ReasoningEfforts         []json.RawMessage `json:"reasoning_efforts"`
	ReasoningEffortsCamel    []json.RawMessage `json:"reasoningEfforts"`
}

func (e buildModelCatalogEntry) profile() modelcatalog.Profile {
	if e.Hidden || e.Meta.Hidden {
		return modelcatalog.Profile{}
	}
	profile := modelcatalog.Profile{ModelID: util.FirstNonEmpty(e.ID, e.Model, e.ModelID, e.Meta.Model, e.Meta.ModelID), SupportsBackendSearch: e.SupportsBackendSearch}
	if e.ContextWindow > 0 {
		profile.ContextWindow = clampCatalogInt(e.ContextWindow)
	} else {
		profile.ContextWindow = clampCatalogInt(e.ContextWindowCamel)
	}
	if e.MaxCompletionTokens > 0 {
		profile.MaxCompletionTokens = clampCatalogInt(e.MaxCompletionTokens)
	} else {
		profile.MaxCompletionTokens = clampCatalogInt(e.MaxCompletionTokensCamel)
	}
	if e.SupportsReasoningEffort != nil {
		profile.SupportsReasoningEffort = *e.SupportsReasoningEffort
	} else if e.SupportsReasoningCamel != nil {
		profile.SupportsReasoningEffort = *e.SupportsReasoningCamel
	}
	rawMenu := e.ReasoningEfforts
	if len(rawMenu) == 0 {
		rawMenu = e.ReasoningEffortsCamel
	}
	for _, raw := range rawMenu {
		var bare string
		if json.Unmarshal(raw, &bare) == nil && strings.TrimSpace(bare) != "" {
			profile.ReasoningEfforts = append(profile.ReasoningEfforts, bare)
			continue
		}
		var option struct {
			Value   string `json:"value"`
			Default bool   `json:"default"`
		}
		if json.Unmarshal(raw, &option) != nil || strings.TrimSpace(option.Value) == "" {
			continue
		}
		profile.ReasoningEfforts = append(profile.ReasoningEfforts, option.Value)
		if option.Default && profile.DefaultReasoningEffort == "" {
			profile.DefaultReasoningEffort = option.Value
		}
	}
	if profile.DefaultReasoningEffort == "" {
		profile.DefaultReasoningEffort = util.FirstNonEmpty(e.ReasoningEffort, e.ReasoningEffortCamel)
	}
	return modelcatalog.Normalize(profile)
}

func clampCatalogInt(value int64) int {
	if value <= 0 {
		return 0
	}
	if value > math.MaxInt32 {
		return math.MaxInt32
	}
	return int(value)
}

// FetchModelCatalog returns the complete account-scoped Build catalog profile.
func (c *CLIClient) FetchModelCatalog(ctx context.Context, acc *store.Account) ([]modelcatalog.Profile, error) {
	if c == nil || c.oauth == nil || acc == nil {
		return nil, fmt.Errorf("grok cli models is not configured")
	}
	ApplyCLIOAuthIdentity(acc)
	resp, err := c.request(ctx, acc, http.MethodGet, c.baseURL()+"/models", nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, cliOAuthMaxBodyBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, newCLIUpstreamError(resp.StatusCode, resp.Header, body)
	}
	var payload struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode grok cli models response: %w", err)
	}
	profiles := make([]modelcatalog.Profile, 0, len(payload.Data))
	seen := make(map[string]struct{}, len(payload.Data))
	for _, raw := range payload.Data {
		var item buildModelCatalogEntry
		if json.Unmarshal(raw, &item) != nil {
			continue
		}
		profile := item.profile()
		key := strings.ToLower(profile.ModelID)
		if key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		profiles = append(profiles, profile)
	}
	if len(profiles) == 0 {
		return nil, fmt.Errorf("grok cli models response contains no model ids")
	}
	return profiles, nil
}

// request is the single authenticated Build HTTP entry for chat, resources,
// models, billing and fallback. Callers retain their endpoint status semantics.
func (c *CLIClient) request(ctx context.Context, acc *store.Account, method, endpoint string, body []byte, headers http.Header) (*http.Response, error) {
	if c == nil || c.oauth == nil || acc == nil {
		return nil, fmt.Errorf("grok cli client or account not configured")
	}
	token, err := c.oauth.AccessToken(ctx, acc)
	if err != nil {
		return nil, err
	}
	if token == "" {
		return nil, fmt.Errorf("grok cli account access token is empty")
	}
	requestCtx := ctx
	cancelRequest := func() {}
	var headerTimer *time.Timer
	var headerTimedOut atomic.Bool
	if method == http.MethodPost && strings.HasSuffix(strings.TrimRight(endpoint, "/"), "/responses") && c.responseHeaderTimeout > 0 {
		var cancel context.CancelFunc
		requestCtx, cancel = context.WithCancel(ctx)
		cancelRequest = cancel
		headerTimer = time.AfterFunc(c.responseHeaderTimeout, func() { headerTimedOut.Store(true); cancel() })
		requestCtx = httptrace.WithClientTrace(requestCtx, &httptrace.ClientTrace{
			GotFirstResponseByte: func() { headerTimer.Stop() },
		})
	}
	req, err := http.NewRequestWithContext(requestCtx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		if headerTimer != nil {
			headerTimer.Stop()
		}
		cancelRequest()
		return nil, err
	}
	req.Header = c.cliHeaders(acc, token)
	for key, values := range headers {
		req.Header[key] = append([]string(nil), values...)
	}
	attempt := debug.BeginUpstream(ctx, method, endpoint, req.Header, body)
	req = attempt.TraceRequest(req)
	resp, err := doUpstreamHTTP(req, func(req *http.Request) (*http.Response, error) {
		return c.doCLIRequest(requestCtx, acc, req)
	}, c.cfg.GrokStreamIdleTimeoutFor(), upstreamIdleBuildSemantic)
	if headerTimer != nil {
		headerTimer.Stop()
	}
	attempt.Response(resp, err)
	if headerTimedOut.Load() {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		cancelRequest()
		return nil, fmt.Errorf("%w after %s", errCLIResponseHeaderTimeout, c.responseHeaderTimeout)
	}
	if err != nil {
		cancelRequest()
		return nil, err
	}
	if resp != nil {
		resp.Body = attempt.CaptureBody(resp.Body)
		if headerTimer != nil && resp.Body != nil {
			resp.Body = &cancelOnCloseBody{ReadCloser: resp.Body, cancel: cancelRequest}
		} else {
			cancelRequest()
		}
	}
	return resp, nil
}

type cancelOnCloseBody struct {
	io.ReadCloser
	cancel func()
}

func (b *cancelOnCloseBody) Close() error {
	err := b.ReadCloser.Close()
	if b.cancel != nil {
		b.cancel()
		b.cancel = nil
	}
	return err
}

func cliEgressAffinity(acc *store.Account) string {
	if acc == nil {
		return "build_unknown"
	}
	identity := util.FirstNonEmpty(acc.UserID, acc.Email, acc.OAuthRefreshToken, acc.OAuthAccessToken, fmt.Sprintf("id:%d", acc.ID))
	return credentialAffinity(identity)
}

// doCLIRequest is the fail-closed egress adapter. A successful response owns
// its lease until the shared response lifecycle closes the body.
func (c *CLIClient) doCLIRequest(ctx context.Context, acc *store.Account, req *http.Request) (*http.Response, error) {
	if c.egress == nil || !c.egress.Enabled() {
		return c.httpClient.Do(req)
	}
	lease, err := c.egress.Acquire(ctx, "cli", cliEgressAffinity(acc))
	if err != nil {
		recordEgressAcquireError()
		return nil, fmt.Errorf("grok cli egress unavailable: %w", err)
	}
	// Build is a CLI identity. Its egress lease intentionally carries no
	resp, err := lease.Do(req)
	if err != nil {
		c.egress.FeedbackOutcome(lease.NodeID, egress.OutcomeTransportError)
		lease.Release()
		return nil, err
	}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		c.egress.FeedbackOutcome(lease.NodeID, egress.OutcomeSuccess)
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized:
		// Request validation and credential failures say nothing about the egress
		// node's health.
		c.egress.FeedbackOutcome(lease.NodeID, egress.OutcomeAccountBlock)
	case resp.StatusCode == http.StatusTooManyRequests:
		c.egress.FeedbackOutcome(lease.NodeID, egress.OutcomeRateLimited)
	case resp.StatusCode >= 500:
		c.egress.FeedbackOutcome(lease.NodeID, egress.OutcomeServerError)
	default:
		c.egress.FeedbackOutcome(lease.NodeID, egress.OutcomeForbidden)
	}
	if resp != nil && resp.Body != nil {
		resp.Body = &leaseResponseBody{ReadCloser: resp.Body, release: lease.Release}
	} else {
		lease.Release()
	}
	return resp, nil
}

func classifyAccountStatusFromHTTP(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "401"
	case http.StatusForbidden:
		return "403"
	case http.StatusTooManyRequests:
		return "429"
	case http.StatusPaymentRequired:
		return "402"
	default:
		return ""
	}
}

// cliOAuthError is a lightweight wrapper so VerifyAccount can surface the
// upstream status of a failed refresh.
type cliOAuthError struct {
	status  int
	message string
}

func (e *cliOAuthError) Error() string {
	if e == nil || e.status == 0 {
		return "grok cli oauth error"
	}
	return fmt.Sprintf("grok cli oauth status=%d: %s", e.status, e.message)
}
func (e *cliOAuthError) Status() string {
	if e == nil || e.status == 0 {
		return ""
	}
	return fmt.Sprintf("%d", e.status)
}
