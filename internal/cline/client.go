package cline

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"orchids-api/internal/httpclient"
	"strings"
	"sync"
	"time"

	"encoding/json"

	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/refreshqueue"
	"orchids-api/internal/store"
	"orchids-api/internal/upstream"
	"orchids-api/internal/util"
)

// Client is one Cline account's upstream client.
//
// A client is cheap and stateless apart from the credential it was built from,
// so the handler caches one per account and rebuilds it when the account
// changes.
type Client struct {
	// apiBase serves the control plane, the model feed and chat.
	apiBase string
	// workOSAuthorizeURL and workOSTokenURL are the device authorization pair,
	// and workOSClientID is the public client id.
	workOSAuthorizeURL string
	workOSTokenURL     string
	workOSClientID     string

	// control answers the short control-plane calls (login, refresh, catalog).
	// stream answers the chat call, and has no client-level deadline so a long
	// generation is not cut off by the request timeout.
	control *http.Client
	stream  *http.Client

	// account is the record this client was built from. It is replaced in place
	// when the credential rotates, so the next request on this client does not
	// replay a consumed refresh token.
	account      *store.Account
	accountStore AccountUpdater

	// creds is the resolved credential.
	creds Credentials

	requestTimeout time.Duration
	streamIdle     time.Duration

	stateMu              sync.RWMutex
	refreshMu            sync.Mutex
	credsDirty           bool
	dirtyExpectedRefresh string
}

// AccountUpdater is the subset of the account store the client needs to persist
// a rotated refresh token. It is satisfied by *store.Store.
type AccountUpdater interface {
	UpdateAccount(ctx context.Context, acc *store.Account) error
}

type accountPatcher interface {
	UpdateClineCredentials(ctx context.Context, id int64, patch store.ClineCredentialPatch) error
}

// NewFromAccount builds a client for one account. cfg supplies endpoints, proxy
// and timeout settings; the account supplies the credentials.
func NewFromAccount(acc *store.Account, cfg *config.Config) *Client {
	timeout := 5 * time.Minute
	if cfg != nil && cfg.RequestTimeout > 0 {
		timeout = time.Duration(cfg.RequestTimeout) * time.Second
		if timeout < 30*time.Second {
			timeout = 30 * time.Second
		}
	}
	proxyFunc := http.ProxyFromEnvironment
	proxyKey := "direct"
	if cfg != nil {
		proxyFunc = httpclient.ProxyFuncFromConfig(cfg)
		proxyKey = httpclient.GenerateProxyKeyFromConfig(cfg)
	}

	client := &Client{
		apiBase:            DefaultAPIBase,
		workOSAuthorizeURL: DefaultWorkOSAuthorizeURL,
		workOSTokenURL:     DefaultWorkOSAuthenticateURL,
		workOSClientID:     DefaultWorkOSClientID,
		control:            httpclient.GetSharedHTTPClient(proxyKey, authRequestTimeout, proxyFunc),
		stream:             httpclient.GetSharedHTTPClientWithLimits(proxyKey+"|cline-chat", 0, proxyFunc, cfg != nil && cfg.ClineHTTP2Enabled, cfg),
		requestTimeout:     timeout,
		streamIdle:         cfg.SharedStreamIdleTimeout(),
	}
	if cfg != nil {
		client.apiBase = util.FirstNonEmptyURL(cfg.ClineAPIBaseURL, client.apiBase)
		client.workOSAuthorizeURL = util.FirstNonEmptyURL(cfg.ClineWorkOSAuthorizeURL, client.workOSAuthorizeURL)
		client.workOSTokenURL = util.FirstNonEmptyURL(cfg.ClineWorkOSTokenURL, client.workOSTokenURL)
		client.workOSClientID = util.FirstNonEmpty(cfg.ClineWorkOSClientID, client.workOSClientID)
	}
	client.apiBase = strings.TrimRight(client.apiBase, "/")
	if acc != nil {
		copied := *acc
		copied.ClineModelIDs = append([]string(nil), acc.ClineModelIDs...)
		client.account = &copied
	}
	client.creds = ResolveCredentials(client.account)
	return client
}

// SetAccountStore lets the client persist a rotated refresh token. Without it a
// rotation would be lost at the next restart, which would break the account.
func (c *Client) SetAccountStore(s AccountUpdater) {
	if c == nil {
		return
	}
	c.stateMu.Lock()
	c.accountStore = s
	c.stateMu.Unlock()
}

// Close satisfies the shared upstream client lifecycle. The HTTP transports are
// process-wide, so a client owns no resources to close.
func (c *Client) Close() {}

// currentCredentials returns the live credential snapshot.
func (c *Client) currentCredentials() Credentials {
	creds, _, _ := c.currentCredentialState()
	return creds
}

func (c *Client) currentCredentialState() (Credentials, bool, string) {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	if c.account == nil {
		return c.creds, c.credsDirty, c.dirtyExpectedRefresh
	}
	resolved := ResolveCredentials(c.account)
	if resolved.HasCredential() {
		return resolved, c.credsDirty, c.dirtyExpectedRefresh
	}
	return c.creds, c.credsDirty, c.dirtyExpectedRefresh
}

// storeCredentials replaces the in-memory snapshot in place.
func (c *Client) storeCredentials(creds Credentials, dirty bool, expectedRefreshToken string) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.creds = creds
	c.credsDirty = dirty
	c.dirtyExpectedRefresh = expectedRefreshToken
	if c.account == nil {
		return
	}
	c.account.ClineAccessToken = creds.AccessToken
	c.account.ClineRefreshToken = creds.RefreshToken
	c.account.ClineExpiresAt = creds.ExpiresAt
	if creds.Email != "" {
		c.account.ClineEmail = creds.Email
	}
}

func (c *Client) markCredentialsClean(creds Credentials) {
	c.stateMu.Lock()
	if c.creds.AccessToken == creds.AccessToken && c.creds.RefreshToken == creds.RefreshToken {
		c.credsDirty = false
		c.dirtyExpectedRefresh = ""
	}
	c.stateMu.Unlock()
}

// ensureAccessToken returns a usable access token, refreshing when the stored
// one is missing or close to expiry.
func (c *Client) ensureAccessToken(ctx context.Context) (Credentials, error) {
	if c == nil {
		return Credentials{}, fmt.Errorf("cline client is nil")
	}
	creds, dirty, expectedRefresh := c.currentCredentialState()
	if dirty {
		if err := c.persistPatch(ctx, store.ClineCredentialPatch{
			ExpectedRefreshToken: expectedRefresh,
			AccessToken:          creds.AccessToken,
			RefreshToken:         creds.RefreshToken,
			ExpiresAt:            creds.ExpiresAt,
		}); err != nil {
			return Credentials{}, err
		}
		c.markCredentialsClean(creds)
	}
	if creds.AccessValid(time.Now()) {
		return creds, nil
	}
	if strings.TrimSpace(creds.RefreshToken) == "" {
		return Credentials{}, ErrCredentialMissing
	}
	return c.refresh(ctx, creds)
}

// forceRefresh renews the credential unconditionally. The stream path calls it
// after the upstream rejected a request that was otherwise well formed.
func (c *Client) forceRefresh(ctx context.Context, rejected Credentials) error {
	if strings.TrimSpace(rejected.RefreshToken) == "" {
		return ErrCredentialMissing
	}
	_, err := c.refresh(ctx, rejected)
	return err
}

// refresh renews the credential and persists the rotation.
func (c *Client) refresh(ctx context.Context, previous Credentials) (Credentials, error) {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	c.stateMu.RLock()
	owner := c.accountStore
	id := int64(0)
	if c.account != nil {
		id = c.account.ID
	}
	c.stateMu.RUnlock()
	release, err := refreshqueue.AcquireCredential(ctx, "cline", owner, id)
	if err != nil {
		return Credentials{}, err
	}
	defer release()

	// Another goroutine may have refreshed while this one waited.
	current, dirty, expectedRefresh := c.currentCredentialState()
	if dirty {
		if err := c.persistPatch(ctx, store.ClineCredentialPatch{
			ExpectedRefreshToken: expectedRefresh,
			AccessToken:          current.AccessToken,
			RefreshToken:         current.RefreshToken,
			ExpiresAt:            current.ExpiresAt,
		}); err != nil {
			return Credentials{}, err
		}
		c.markCredentialsClean(current)
	}
	if latest, err := refreshqueue.LatestAccount(ctx, owner, id); err != nil {
		return Credentials{}, err
	} else if latest != nil {
		current = ResolveCredentials(latest)
		c.storeCredentials(current, false, "")
	}
	if current.AccessValid(time.Now()) && current.AccessToken != previous.AccessToken {
		return current, nil
	}
	previous = current

	refreshed, err := c.Refresh(ctx, previous.RefreshToken)
	if err != nil {
		return Credentials{}, err
	}
	// The refresh answer carries no identity, so the previous one is kept: the
	// email does not change between token rotations.
	merged := Credentials{
		AccessToken:  refreshed.AccessToken,
		RefreshToken: util.FirstNonEmptyUntrimmed(refreshed.RefreshToken, previous.RefreshToken),
		ExpiresAt:    refreshed.ExpiresAt,
		Email:        previous.Email,
	}
	if merged.ExpiresAt.IsZero() {
		merged.ExpiresAt = previous.ExpiresAt
	}
	c.storeCredentials(merged, true, previous.RefreshToken)
	if err := c.persistPatch(ctx, store.ClineCredentialPatch{
		ExpectedRefreshToken: previous.RefreshToken,
		AccessToken:          merged.AccessToken,
		RefreshToken:         merged.RefreshToken,
		ExpiresAt:            merged.ExpiresAt,
	}); err != nil {
		return Credentials{}, err
	}
	c.markCredentialsClean(merged)
	return merged, nil
}

// persistPatch writes only Cline-owned fields. Production stores implement the
// atomic patch API; the full-account fallback keeps lightweight test stores
// source compatible without weakening the real persistence path.
func (c *Client) persistPatch(ctx context.Context, patch store.ClineCredentialPatch) error {
	c.stateMu.RLock()
	accountStore := c.accountStore
	if c.account == nil {
		c.stateMu.RUnlock()
		return nil
	}
	acc := *c.account
	acc.ClineModelIDs = append([]string(nil), c.account.ClineModelIDs...)
	c.stateMu.RUnlock()
	if accountStore == nil || acc.ID == 0 {
		return nil
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if patcher, ok := accountStore.(accountPatcher); ok {
		if err := patcher.UpdateClineCredentials(writeCtx, acc.ID, patch); err != nil {
			return fmt.Errorf("persist cline account state: %w", err)
		}
		return nil
	}
	if patch.AccessToken != "" {
		acc.ClineAccessToken = patch.AccessToken
	}
	if patch.RefreshToken != "" {
		acc.ClineRefreshToken = patch.RefreshToken
	}
	if !patch.ExpiresAt.IsZero() {
		acc.ClineExpiresAt = patch.ExpiresAt
	}
	if patch.Email != "" {
		acc.ClineEmail = patch.Email
	}
	if patch.ModelIDs != nil {
		acc.ClineModelIDs = append([]string(nil), patch.ModelIDs...)
	}
	if err := accountStore.UpdateAccount(writeCtx, &acc); err != nil {
		return fmt.Errorf("persist cline account state: %w", err)
	}
	return nil
}

// Credentials returns the live credential snapshot. It exists so a login can
// read back what it just obtained without reaching into the client's state.
func (c *Client) Credentials() Credentials { return c.currentCredentials() }

// SendRequestWithPayload streams one chat completion to the caller.
func (c *Client) SendRequestWithPayload(ctx context.Context, req upstream.UpstreamRequest, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	if c == nil {
		return fmt.Errorf("cline client is nil")
	}
	creds, err := c.ensureAccessToken(ctx)
	if err != nil {
		return err
	}
	model, err := c.resolveModel(req)
	if err != nil {
		return err
	}
	body, err := buildChatBody(req, model)
	if err != nil {
		return err
	}
	url := c.apiBase + "/chat/completions"
	if logger != nil && !logger.Capturing() {
		logger.LogUpstreamRequest(url, map[string]string{"provider": "cline", "model": model}, body)
	}
	return c.runChat(ctx, url, body, model, logicalTaskID(req.RequestID), creds, onMessage)
}

// runChat performs one upstream call, except that an initial 401 gets one token
// refresh and one replay. All non-auth retry budgeting belongs to the shared
// handler so an account switch and a provider-local replay cannot both consume
// capacity for the same logical attempt.
func (c *Client) runChat(ctx context.Context, url string, body []byte, model, taskID string, creds Credentials, onMessage func(upstream.SSEMessage)) error {
	emitted := false
	emit := func(msg upstream.SSEMessage) {
		emitted = true
		if onMessage != nil {
			onMessage(msg)
		}
	}

	attemptCredentials := creds
	for attempt := 0; attempt < 2; attempt++ {
		if current := c.currentCredentials(); current.HasCredential() {
			attemptCredentials = current
		}
		result, err := c.attemptChat(ctx, url, body, model, taskID, attemptCredentials, emit)
		if err == nil {
			if !result.SawMeaningfulEvent {
				return fmt.Errorf("cline stream produced no usable events")
			}
			if onMessage != nil {
				event := map[string]interface{}{"finishReason": result.FinishReason()}
				if len(result.Usage) > 0 {
					event["usage"] = result.Usage
				}
				onMessage(upstream.SSEMessage{Type: "model.finish", Event: event})
			}
			return nil
		}
		if emitted {
			return err
		}
		if attempt == 0 && isUnauthorized(err) && upstream.RemainingAttempts(ctx) != 0 {
			if refreshErr := c.forceRefresh(ctx, attemptCredentials); refreshErr != nil {
				return refreshErr
			}
			continue
		}
		return err
	}
	return ErrCredentialMissing
}

// attemptChat performs one upstream attempt and consumes its stream.
func (c *Client) attemptChat(ctx context.Context, url string, body []byte, model, taskID string, creds Credentials, emit func(upstream.SSEMessage)) (result streamResult, attemptErr error) {
	reqCtx, cancel := util.WithAttemptTimeout(ctx, c.requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return streamResult{}, &attemptStreamError{err: fmt.Errorf("build cline request: %w", err)}
	}
	req.Header.Set("Authorization", "Bearer "+creds.Bearer())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("X-Task-ID", taskID)
	applyClientHeaders(req.Header)

	attempt := debug.BeginUpstream(ctx, req.Method, req.URL.String(), req.Header, body)
	req = attempt.TraceRequest(req)
	finishAttempt, err := upstream.BeginAttempt(ctx)
	if err != nil {
		return streamResult{}, err
	}
	defer func() { finishAttempt(attemptErr) }()
	resp, err := c.stream.Do(req)
	attempt.Response(resp, err)
	if resp != nil {
		resp.Body = attempt.CaptureBody(resp.Body)
	}
	if err != nil {
		return streamResult{}, &attemptStreamError{err: fmt.Errorf("send cline request: %w", err)}
	}
	defer func() {
		if attemptErr == nil && result.SawMeaningfulEvent {
			httpclient.DrainTerminalResponse(reqCtx, resp)
		}
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return streamResult{}, classifyStatus(resp.StatusCode, raw)
	}

	monitored, progress := httpclient.MonitorProgressIdle(resp.Body, c.streamIdle, cancel, "cline")
	resp.Body = monitored
	return consumeStream(resp.Body, requestCarriesTools(body), func(m upstream.SSEMessage) {
		progress()
		if emit != nil {
			emit(m)
		}
	})
}

// requestCarriesTools keeps fallback parsing disabled for plain-text requests:
// without declared tools, literal <tool_call> text is user-visible content.
func requestCarriesTools(body []byte) bool {
	var payload struct {
		Tools []json.RawMessage `json:"tools"`
	}
	return json.Unmarshal(body, &payload) == nil && len(payload.Tools) > 0
}

// ProbeReachability verifies that this process can actually reach the Cline API.
// It is a cheap unauthenticated request intended for startup diagnostics: a
// blocked egress path makes both the device login and every inference request
// fail, and reporting that at boot is far cheaper to act on than debugging a
// per-request timeout later.
func (c *Client) ProbeReachability(ctx context.Context) error {
	if c == nil {
		return fmt.Errorf("cline client is nil")
	}
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, c.apiBase+"/ai/cline/recommended-models", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Task-ID", "sess_probe")
	applyClientHeaders(req.Header)

	resp, err := c.control.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrAuthUnavailable, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	return nil
}

// FetchUpstreamModels reads the account-scoped model feed.
//
// There is no local fallback. A compiled-in catalog is not an observation of
// what the account may run, so a failed read is reported as a failure and an
// empty feed is an error: publishing nothing would hide an upstream that stopped
// answering.
func (c *Client) FetchUpstreamModels(ctx context.Context) ([]Model, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	if c == nil {
		return nil, fmt.Errorf("cline client is nil")
	}
	creds, err := c.ensureAccessToken(ctx)
	if err != nil {
		return nil, err
	}
	endpoint := c.apiBase + "/ai/cline/recommended-models"

	reqCtx, cancel := context.WithTimeout(ctx, authRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build cline catalog request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+creds.Bearer())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Task-ID", newTaskID(time.Now()))
	applyClientHeaders(req.Header)

	resp, raw, err := httpclient.DoReadBody(c.control, req, 1<<20)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAuthUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, apiError(http.MethodGet, endpoint, resp.StatusCode, raw)
	}
	models, err := parseCatalog(raw)
	if err != nil {
		return nil, err
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("%w: the recommended-models feed published nothing", ErrNoUpstreamCatalog)
	}
	return models, nil
}

// FetchModels is the provider-facing alias of FetchUpstreamModels.
func (c *Client) FetchModels(ctx context.Context) ([]Model, error) { return c.FetchUpstreamModels(ctx) }

// resolveModel maps the client's model name onto the account's observed catalog.
//
// With no observation the catalog is empty on purpose. Resolving against a
// compiled-in list would accept a model the account never advertised; an empty
// catalog makes this report ErrNoUpstreamCatalog instead.
func (c *Client) resolveModel(req upstream.UpstreamRequest) (string, error) {
	requested := strings.TrimSpace(req.Model)
	c.stateMu.RLock()
	ids := append([]string(nil), c.accountIDsLocked()...)
	c.stateMu.RUnlock()
	if requested != "" {
		for _, row := range ids {
			if strings.EqualFold(catalogID(row), requested) {
				return catalogID(row), nil
			}
		}
		if len(ids) == 0 {
			return "", ErrNoUpstreamCatalog
		}
		return "", fmt.Errorf("cline model %q is not in this account's catalog", requested)
	}
	for _, id := range ids {
		if trimmed := strings.TrimSpace(catalogID(id)); trimmed != "" {
			return trimmed, nil
		}
	}
	return "", ErrNoUpstreamCatalog
}

func (c *Client) accountIDsLocked() []string {
	if c.account == nil {
		return nil
	}
	return c.account.ClineModelIDs
}

func logicalTaskID(requestID string) string {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return newTaskID(time.Now())
	}
	if strings.HasPrefix(requestID, "sess_") {
		return requestID
	}
	return "sess_" + requestID
}
