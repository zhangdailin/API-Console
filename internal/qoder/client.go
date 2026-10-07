package qoder

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"orchids-api/internal/httpclient"
	"slices"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/refreshqueue"
	"orchids-api/internal/store"
	"orchids-api/internal/upstream"
	"orchids-api/internal/util"
)

// Client is one Qoder account's upstream client.
//
// A client is cheap and stateless apart from the credential it was built from
// and the derived runtime fields, so the handler caches one per account and
// rebuilds it when the account changes.
type Client struct {
	// endpoints are resolved once from configuration so a request never has to
	// consult the config snapshot again.
	endpoints endpoints
	// clientID and clientVersion identify the CLI build this channel emulates.
	clientID      string
	clientVersion string
	// machineID is the device identity. It is bound to the credential: the
	// upstream rejects a request whose machine id did not perform the login.
	machineID string
	// machineToken and machineType are the rest of the device fingerprint.
	// They are not part of the credential binding, and are derived from the
	// account so the device is stable across requests and restarts instead of
	// drifting under a live credential.
	machineToken string
	machineType  string

	// control answers the short control-plane calls (token, profile, catalog).
	// stream answers the chat call, and has no client-level deadline so a long
	// generation is not cut off by the request timeout.
	control *http.Client
	stream  *http.Client

	// account is the record this client was built from. It is replaced in place
	// when the credential rotates, so the next request on this client does not
	// replay a consumed refresh token.
	account      *store.Account
	accountStore AccountUpdater
	catalog      *Catalog
	catalogIDs   []string

	// creds is the resolved credential. runtime holds the derived auth pair.
	creds   Credentials
	runtime RuntimeFields

	// Reference COSY encrypted identity embeds the access/refresh token; unlike
	// the old CLI runtime pair, it must be rederived when either rotates.
	runtimeAccessToken  string
	runtimeRefreshToken string
	requestTimeout      time.Duration
	streamIdle          time.Duration

	entropy source

	refreshMu            sync.Mutex
	runtimeMu            sync.Mutex
	stateMu              sync.RWMutex
	credsDirty           bool
	dirtyExpectedRefresh string
}

// AccountUpdater is the subset of the account store the client needs to persist
// a rotated refresh token and a derived runtime pair. It is satisfied by
// *store.Store.
type AccountUpdater interface {
	UpdateAccount(ctx context.Context, acc *store.Account) error
}

type accountPatcher interface {
	UpdateQoderAccount(ctx context.Context, id int64, patch store.QoderAccountPatch) error
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
	http2 := false
	if cfg != nil {
		proxyFunc = httpclient.ProxyFuncFromConfig(cfg)
		proxyKey = httpclient.GenerateProxyKeyFromConfig(cfg)
		http2 = cfg.QoderHTTP2Enabled
	}

	client := &Client{
		endpoints:      resolveEndpoints(cfg),
		clientID:       resolveClientID(cfg),
		clientVersion:  resolveClientVersion(cfg),
		control:        httpclient.GetSharedHTTPClient(proxyKey, authRequestTimeout, proxyFunc),
		stream:         httpclient.GetSharedHTTPClientWithLimits(proxyKey+"|qoder-chat", 0, proxyFunc, http2, cfg),
		requestTimeout: timeout,
		streamIdle:     cfg.SharedStreamIdleTimeout(),
		entropy:        cryptoSource{},
	}
	if acc != nil {
		copied := *acc
		copied.QoderOrganizationTags = append([]string(nil), acc.QoderOrganizationTags...)
		copied.QoderModelIDs = append([]string(nil), acc.QoderModelIDs...)
		client.account = &copied
	}
	client.creds = ResolveCredentials(client.account)
	if client.machineID == "" {
		// An account-less or freshly completed credential may not carry the
		// device identity yet; the resolved credential is the other place it
		// travels, and a login result always sets it.
		client.machineID = strings.TrimSpace(client.creds.MachineID)
	}
	if acc != nil {
		client.machineID = strings.TrimSpace(acc.QoderMachineID)
		// Derive runtime ciphertext from the current reference identity.
	}
	client.applyFingerprint()
	return client
}

// aliyunUserType returns the account class this account reported, for example
// "personal_standard" or "personal_professional_trial".
//
// It travels in the chat body as aliyun_user_type. Upstream 10605 responses
// alone do not establish whether this field influences queue admission.
func (c *Client) aliyunUserType() string {
	if c == nil {
		return ""
	}
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	if c.account != nil {
		if class := strings.TrimSpace(c.account.QoderQuota.UserType); class != "" {
			return class
		}
	}
	return ""
}

// applyFingerprint preserves the QoderWork login device identity.
func (c *Client) applyFingerprint() {
	// QoderWork binds the machine token to the login device ID.
	c.machineToken = c.machineID
	c.machineType = machineSceneType
}

// SetAccountStore lets the client persist a rotated refresh token and a newly
// derived runtime pair. Without it a rotation would be lost at the next
// restart, which would break the account.
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

// SendRequestWithPayload streams one chat completion to the caller.
func (c *Client) SendRequestWithPayload(ctx context.Context, req upstream.UpstreamRequest, onMessage func(upstream.SSEMessage), logger *debug.Logger) (sendErr error) {
	if c == nil {
		return fmt.Errorf("qoder client is nil")
	}
	preparation := debug.BeginUpstream(ctx, "PREPARE", c.endpoints.inference, nil, nil)
	var prepMetadata map[string]interface{}
	if debug.FromContext(ctx) != nil {
		prepMetadata = map[string]interface{}{"kind": "preparation", "provider": "qoder"}
	}
	_, prepTrace := preparation.Trace(ctx, prepMetadata)
	defer func() {
		if prepTrace != nil {
			prepTrace.Finish(sendErr)
		}
	}()
	prepTrace.Set("credential_refresh_expected", !c.currentCredentials().AccessValid(time.Now()))
	creds, err := c.ensureAccessToken(ctx)
	prepTrace.Mark("access_ready_ms")
	if err != nil {
		return err
	}
	fields, err := c.ensureRuntimeFields(ctx, creds)
	prepTrace.Mark("runtime_ready_ms")
	if err != nil {
		return err
	}
	model, err := c.resolveModel(req)
	prepTrace.Mark("model_ready_ms")
	if err != nil {
		return err
	}

	// The task set id is a second, independent uuid. The capture shows
	// request_set_id and request_id as different values within one task, with
	// business.id carrying the set id.
	requestID, requestSetID, sessionID, err := newChatUUIDs(c.entropy)
	if err != nil {
		return err
	}
	if conversation := strings.TrimSpace(req.ConversationID); conversation != "" {
		sessionID = conversationSessionID(creds.UID, conversation)
	} else if conversation := strings.TrimSpace(req.ChatSessionID); conversation != "" {
		sessionID = conversationSessionID(creds.UID, conversation)
	}
	// aliyun_user_type is deliberately empty: the QoderWork client sends no
	// account class, and the account's own class is still reported through the
	// quota path.
	body, err := buildChatBodyProfile(req, model, sessionID, requestID, requestSetID, c.clientVersion, "", sceneBusinessProduct)
	if err != nil {
		return err
	}

	url := chatURL(c.endpoints.inference)
	if logger != nil && !logger.Capturing() {
		logger.LogUpstreamRequest(url, map[string]string{"provider": "qoder", "model": model.Key}, body)
	}

	toolsEnabled := !req.NoTools && len(normalizeToolDefinitions(req, model)) > 0
	prepTrace.Mark("body_ready_ms")
	prepTrace.Finish(nil)
	prepTrace = nil
	return c.runChat(ctx, url, body, model, requestID, fields, toolsEnabled, onMessage)
}

// runChat performs one upstream attempt. The shared request handler owns
// transport/status retry and account switching, so retrying four times here as
// well multiplied one API call by both budgets.
//
// Two local repairs are kept, because both fix the request in place instead of
// spending another account:
//
//   - a single 401 credential refresh, which repairs this account;
//   - a bounded retry of a transient provider fault (418/5xx/provider_error or
//     a connection hiccup), which is the upstream's own problem rather than
//     this account's.
//
// Anything that has already emitted content is returned immediately: a replay
// would duplicate the answer, and a usage-only frame would double-count billing.
func (c *Client) runChat(ctx context.Context, url string, body []byte, model modelEntry, requestID string, fields RuntimeFields, toolsEnabled bool, onMessage func(upstream.SSEMessage)) error {
	// Authentication repair has its own one-shot budget; only transient
	// failures consume the transient retry counter.
	transientRetries := 0
	emitted := false
	refreshed := false
	emit := func(msg upstream.SSEMessage) {
		emitted = true
		if onMessage != nil {
			onMessage(msg)
		}
	}

	for {
		attemptCredentials := c.currentCredentials()
		result, err := c.attemptChat(ctx, url, body, model, requestID, fields, attemptCredentials, toolsEnabled, emit)
		if err == nil {
			// A quota notice travels with the attempt's result whichever way the
			// attempt settled, but it is only recorded on a settled stream: the
			// notice is a statement about the account, and a stream that produced
			// nothing usable has not proved anything except that the upstream
			// refused, which its own error already says.
			if result.QuotaNotice != nil && result.SawMeaningfulEvent {
				c.recordQuotaNotice(ctx, result.QuotaNotice)
			}
			if !result.SawMeaningfulEvent {
				// A 200 stream that carried nothing is the upstream refusing
				// quietly. It is reported as an error, not as an empty success,
				// and it is not replayed: the empty answer came from load, and
				// a replay adds to that load.
				return fmt.Errorf("%w: %v", ErrEmptyStream, fmt.Errorf("qoder stream produced no usable events"))
			}
			result.emitFinish(onMessage)
			return nil
		}
		if emitted {
			// Usage is observable output too: replaying after a usage-only frame can
			// double-count billing even when no assistant token was emitted.
			return err
		}
		if upstream.RemainingAttempts(ctx) == 0 {
			return err
		}
		var agentErr *agentLimitError
		if errors.As(err, &agentErr) {
			// agentLimitResetTime is emitted by the inference agent and is not an
			// authoritative account credit snapshot.  The OpenAPI quota endpoint
			// may still report spendable credits, so do not overwrite QoderQuota or
			// globally quarantine the account here.  The handler records a
			// model-scoped cooldown; periodic/manual quota sync remains the sole
			// authority for account-wide exhaustion.
			return err
		}

		switch {
		case isUnauthorized(err) && !refreshed:
			refreshed = true
			if refreshErr := c.forceRefresh(ctx, attemptCredentials); refreshErr != nil {
				// The refresh can fail for reasons that have nothing to do with the
				// refusal this attempt met (a throttled or unreachable authorization
				// endpoint, a transient network fault). Returning it would replace a
				// verdict about the request with one about the credential, and could
				// retire an account the upstream never actually rejected. Only an
				// answer that says the durable credential is gone outranks it.
				if errors.Is(refreshErr, ErrReLoginRequired) || errors.Is(refreshErr, ErrCredentialMissing) {
					return refreshErr
				}
				slog.Warn("Qoder token refresh failed while handling a refused credential; reporting the original refusal",
					"error", refreshErr)
				return err
			}
			if fields, err = c.ensureRuntimeFields(ctx, c.currentCredentials()); err != nil {
				return err
			}
			// A replay with the original request id is rejected as a duplicate even
			// though the bearer changed. Give the repaired attempt a fresh identity
			// and explicitly mark it as a retry.
			requestID, err = newUUID(c.entropy)
			if err != nil {
				return err
			}
			body, err = refreshedReplayBody(body, requestID)
			if err != nil {
				return err
			}
			continue
		// A provider-side hiccup is worth a bounded local retry on the account
		// that already holds the request; switching accounts for the upstream's
		// own fault takes a healthy account out of rotation for nothing.
		case isTransientError(err) && transientRetries < TransientMaxRetries:
			transientRetries++
			waitStarted := time.Now()
			delay := TransientBackoff(transientRetries)
			waitErr := sleepCtx(ctx, delay)
			debug.RecordWait(ctx, "qoder_transient", delay, time.Since(waitStarted), waitErr != nil)
			if waitErr != nil {
				return waitErr
			}
			requestID, err = newUUID(c.entropy)
			if err != nil {
				return err
			}
			body, err = refreshedReplayBody(body, requestID)
			if err != nil {
				return err
			}
			continue
		case isRetryable(err):
			// Return the typed retryable error to the shared handler. It applies the
			// configured backoff and can switch accounts without multiplying budgets.
			return err
		default:
			return err
		}
	}
}

// sleepCtx waits, but never past the caller's deadline.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// attemptStreamError carries the retry decision an attempt reached.
type attemptStreamError struct {
	err       error
	retryable bool
	unauth    bool
	busy      bool
	wait      time.Duration
}

// RetryAfter exposes the upstream backoff hint to the shared request handler.
func (e *attemptStreamError) RetryAfter() time.Duration {
	if e == nil {
		return 0
	}
	return e.wait
}

func (e *attemptStreamError) Error() string { return e.err.Error() }
func (e *attemptStreamError) Unwrap() error { return e.err }

func isUnauthorized(err error) bool {
	var target *attemptStreamError
	return errors.As(err, &target) && target.unauth
}

func isRetryable(err error) bool {
	var target *attemptStreamError
	return errors.As(err, &target) && (target.retryable || target.busy)
}

// ensureAccessToken returns a usable device access token, refreshing when the
// stored one is missing or close to expiry.
func (c *Client) ensureAccessToken(ctx context.Context) (Credentials, error) {
	creds, dirty, expectedRefresh := c.currentCredentialState()
	if dirty {
		if err := c.persistCredentials(ctx, creds, expectedRefresh); err != nil {
			return Credentials{}, err
		}
		c.markCredentialsClean(creds)
	}
	now := time.Now()
	if creds.AccessValid(now) {
		return creds, nil
	}
	if strings.TrimSpace(creds.RefreshToken) == "" {
		if strings.TrimSpace(creds.AccessToken) != "" {
			// Only an access token exists and it is close to expiry. Use it
			// rather than failing outright: a stale clock or an opaque token
			// without an expiry is common, and the upstream is the authority.
			return creds, nil
		}
		return Credentials{}, ErrCredentialMissing
	}
	if !creds.RefreshExpiresAt.IsZero() && now.After(creds.RefreshExpiresAt) {
		return Credentials{}, ErrReLoginRequired
	}
	return c.refresh(ctx, creds)
}

// forceRefresh renews the credential unconditionally. The stream path calls it
// after the upstream rejected a request that was otherwise well formed.
func (c *Client) forceRefresh(ctx context.Context, rejected Credentials) error {
	if strings.TrimSpace(rejected.RefreshToken) == "" || !rejected.RefreshExpiresAt.IsZero() && time.Now().After(rejected.RefreshExpiresAt) {
		return ErrReLoginRequired
	}
	_, err := c.refresh(ctx, rejected)
	return err
}

// refresh renews the device credential and persists the rotation.
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
	release, err := refreshqueue.AcquireCredential(ctx, "qoder", owner, id)
	if err != nil {
		return Credentials{}, err
	}
	defer release()

	// Another goroutine may have refreshed while this one waited.
	current, dirty, expectedRefresh := c.currentCredentialState()
	if dirty {
		if err := c.persistCredentials(ctx, current, expectedRefresh); err != nil {
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
	// profile fields do not change between token rotations.
	merged := Credentials{
		AccessToken:      refreshed.AccessToken,
		RefreshToken:     util.FirstNonEmptyUntrimmed(refreshed.RefreshToken, previous.RefreshToken),
		AccessExpiresAt:  refreshed.AccessExpiresAt,
		RefreshExpiresAt: refreshed.RefreshExpiresAt,
		UID:              util.FirstNonEmptyUntrimmed(refreshed.UID, previous.UID),
		Name:             util.FirstNonEmptyUntrimmed(refreshed.Name, previous.Name),
		Email:            previous.Email,
		OrgID:            previous.OrgID,
		OrgTags:          previous.OrgTags,
	}
	c.storeCredentials(merged, true, previous.RefreshToken)
	if err := c.persistCredentials(ctx, merged, previous.RefreshToken); err != nil {
		return Credentials{}, err
	}
	c.markCredentialsClean(merged)
	return merged, nil
}

// ensureRuntimeFields returns the derived authentication pair, deriving and
// persisting it on first use.
//
// The pair is per account rather than per request because the CLI reuses the
// pair it generated at login; it is the account's runtime fields, and the
// upstream treats a rotation as new device material.
func (c *Client) ensureRuntimeFields(ctx context.Context, creds Credentials) (RuntimeFields, error) {
	// The normal immutable snapshot needs no serialization with other readers.
	c.stateMu.RLock()
	fieldsReady := c.runtime.Complete() && c.runtimeAccessToken == creds.AccessToken && c.runtimeRefreshToken == creds.RefreshToken
	ready := c.runtime
	c.stateMu.RUnlock()
	if fieldsReady {
		return ready, nil
	}
	c.runtimeMu.Lock()
	defer c.runtimeMu.Unlock()
	// The reference identity includes both tokens. Reuse the pair only while
	// they still match; a refresh must not sign new tokens with stale ciphertext.
	if fields := c.runtimeSnapshot(); fields.Complete() && c.runtimeTokensMatch(creds) {
		return fields, nil
	}
	if strings.TrimSpace(creds.UID) == "" {
		// The runtime fields encrypt the UID, so they cannot be derived before
		// the identity is known.
		return RuntimeFields{}, fmt.Errorf("qoder account has no user id yet; sign in again")
	}
	fields, err := referenceRuntimeFieldsFor(c.entropy, referenceRuntimeFieldInput{
		Name:               creds.Name,
		Aid:                creds.UID,
		UID:                creds.UID,
		OrganizationID:     creds.OrgID,
		UserType:           aliyunUserTypeOr(c.aliyunUserType()),
		SecurityOAuthToken: creds.AccessToken,
		RefreshToken:       creds.RefreshToken,
	})
	if err != nil {
		return RuntimeFields{}, err
	}
	if err := c.persistPatch(ctx, store.QoderAccountPatch{
		RuntimeInfo: fields.EncryptUserInfo,
		RuntimeKey:  fields.Key,
		UserID:      creds.UID,
	}); err != nil {
		return RuntimeFields{}, err
	}
	c.stateMu.Lock()
	c.runtime = fields
	c.runtimeAccessToken = creds.AccessToken
	c.runtimeRefreshToken = creds.RefreshToken
	if c.account != nil {
		c.account.QoderRuntimeInfo = fields.EncryptUserInfo
		c.account.QoderRuntimeKey = fields.Key
		if strings.TrimSpace(c.account.QoderUserID) == "" {
			c.account.QoderUserID = creds.UID
		}
	}
	c.stateMu.Unlock()
	return fields, nil
}

// dataPolicyAgreed reports the recorded agreement. An account created through
// this channel always agreed during the browser step, and an account whose
// agreement is unknown reports disagreement, which the gateway accepts.
func (c *Client) dataPolicyAgreed() bool {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	if c.account == nil {
		return true
	}
	return c.account.QoderDataPolicy
}

// resolveModel maps the client's model name onto a catalog entry.
func (c *Client) resolveModel(req upstream.UpstreamRequest) (modelEntry, error) {
	catalog := c.loadCatalog()
	entry, err := catalog.Resolve(req.Model)
	if err != nil {
		return modelEntry{}, err
	}
	return entry, nil
}

// loadCatalog returns the account's observed catalog. It never performs I/O: a
// chat request must not depend on a catalog read, and the handler refreshes the
// catalog out of band.
//
// With no observation the catalog is empty on purpose. Resolving against a
// compiled-in list would accept a model the account never advertised; an empty
// catalog makes Resolve report ErrNoUpstreamCatalog instead.
func (c *Client) loadCatalog() *Catalog {
	c.stateMu.RLock()
	var ids []string
	if c.account != nil {
		ids = c.account.QoderModelIDs
	}
	if c.catalog != nil && slices.Equal(ids, c.catalogIDs) {
		catalog := c.catalog
		c.stateMu.RUnlock()
		return catalog
	}
	c.stateMu.RUnlock()
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	ids = nil
	if c.account != nil {
		ids = c.account.QoderModelIDs
	}
	if c.catalog == nil || !slices.Equal(ids, c.catalogIDs) {
		c.catalog = catalogFromIDs(ids)
		c.catalogIDs = append([]string(nil), ids...)
	}
	return c.catalog
}

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
	c.account.QoderAccessToken = creds.AccessToken
	c.account.QoderRefreshToken = creds.RefreshToken
	c.account.QoderExpiresAt = creds.AccessExpiresAt
	if creds.UID != "" {
		c.account.QoderUserID = creds.UID
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

func (c *Client) runtimeTokensMatch(creds Credentials) bool {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	return c.runtimeAccessToken == creds.AccessToken && c.runtimeRefreshToken == creds.RefreshToken
}

func (c *Client) runtimeSnapshot() RuntimeFields {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	return c.runtime
}

func (c *Client) persistCredentials(ctx context.Context, creds Credentials, expectedRefreshToken string) error {
	return c.persistPatch(ctx, store.QoderAccountPatch{
		ExpectedRefreshToken: expectedRefreshToken,
		AccessToken:          creds.AccessToken,
		RefreshToken:         creds.RefreshToken,
		ExpiresAt:            creds.AccessExpiresAt,
		UserID:               creds.UID,
	})
}

// recordQuotaNotice stores a quota verdict the upstream stated in band.
//
// The notice is applied through the account store's freshness-guarded quota
// path, so it can only move the snapshot forward. It never shortens a window
// the upstream already named, and it never overrides a newer synced reading.
//
// The failure is logged and swallowed: the answer the caller asked for has
// already been produced, and losing a bookkeeping write must not turn a
// successful generation into an error.
func (c *Client) recordQuotaNotice(ctx context.Context, notice *QuotaNotice) {
	if c == nil || notice == nil || !notice.Exhausted {
		return
	}
	now := time.Now()
	c.stateMu.RLock()
	if c.account == nil {
		c.stateMu.RUnlock()
		return
	}
	snapshot := c.account.QoderQuota
	c.stateMu.RUnlock()
	snapshot.Exhausted = true
	snapshot.SyncedAt = now
	if !notice.NextResetAt.IsZero() {
		// The stream's own reset boundary is authoritative for this window:
		// take it even when an earlier reading named a different one, because
		// the window the notice describes is the one that just closed.
		snapshot.ResetAt = notice.NextResetAt
	}
	snapshot.UpgradeURL = util.FirstNonEmpty(notice.UpgradeURL, snapshot.UpgradeURL)
	c.stateMu.Lock()
	if c.account != nil {
		c.account.QoderQuota = snapshot
	}
	c.stateMu.Unlock()
	if err := c.persistPatch(ctx, store.QoderAccountPatch{Quota: &snapshot}); err != nil {
		slog.Warn("Failed to persist a Qoder quota notice; the answer is unaffected",
			"error", err, "next_reset_at", notice.NextResetAt)
	}
}

// persistPatch writes only Qoder-owned fields. Production stores implement the
// atomic patch API; the full-account fallback keeps lightweight test stores
// source compatible without weakening the real persistence path.
func (c *Client) persistPatch(ctx context.Context, patch store.QoderAccountPatch) error {
	c.stateMu.RLock()
	accountStore := c.accountStore
	if c.account == nil {
		c.stateMu.RUnlock()
		return nil
	}
	acc := *c.account
	acc.QoderOrganizationTags = append([]string(nil), c.account.QoderOrganizationTags...)
	acc.QoderModelIDs = append([]string(nil), c.account.QoderModelIDs...)
	c.stateMu.RUnlock()
	if accountStore == nil || acc.ID == 0 {
		return nil
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if patcher, ok := accountStore.(accountPatcher); ok {
		if err := patcher.UpdateQoderAccount(writeCtx, acc.ID, patch); err != nil {
			return fmt.Errorf("persist qoder account state: %w", err)
		}
		return nil
	}
	if patch.AccessToken != "" {
		acc.QoderAccessToken = patch.AccessToken
	}
	if patch.RefreshToken != "" {
		acc.QoderRefreshToken = patch.RefreshToken
	}
	if !patch.ExpiresAt.IsZero() {
		acc.QoderExpiresAt = patch.ExpiresAt
	}
	if patch.UserID != "" {
		acc.QoderUserID = patch.UserID
	}
	if patch.RuntimeInfo != "" {
		acc.QoderRuntimeInfo = patch.RuntimeInfo
	}
	if patch.RuntimeKey != "" {
		acc.QoderRuntimeKey = patch.RuntimeKey
	}
	if patch.ModelIDs != nil {
		acc.QoderModelIDs = append([]string(nil), patch.ModelIDs...)
	}
	if patch.Quota != nil {
		// The fallback has no atomic guard to lean on, so the freshness rule is
		// applied here: a reading older than the one already on the record is
		// dropped rather than allowed to rewind it. UpdateAccount re-applies the
		// same rule server-side when the store supports it.
		incoming := *patch.Quota
		if acc.QoderQuota.SyncedAt.IsZero() || !incoming.SyncedAt.Before(acc.QoderQuota.SyncedAt) {
			acc.QoderQuota = incoming
		}
	}
	if err := accountStore.UpdateAccount(writeCtx, &acc); err != nil {
		return fmt.Errorf("persist qoder account state: %w", err)
	}
	return nil
}

// RuntimeFields returns the derived pair. It is empty until the pair has been
// prepared.
func (c *Client) RuntimeFields() RuntimeFields {
	if c == nil {
		return RuntimeFields{}
	}
	return c.runtimeSnapshot()
}

func (c *Client) machineTokenOr(fallback string) string {
	if c == nil || strings.TrimSpace(c.machineToken) == "" {
		return fallback
	}
	return c.machineToken
}

func (c *Client) machineTypeOr(fallback string) string {
	if c == nil || strings.TrimSpace(c.machineType) == "" {
		return fallback
	}
	return c.machineType
}

// MachineID returns the device identity this client binds its requests to.
func (c *Client) MachineID() string {
	if c == nil {
		return ""
	}
	return c.machineID
}

// NormalizeLoginResult folds a device flow result and the machine id it was
// authorized under onto the field set the account record owns. It is the single
// mapping used by the admin login flow, so a credential written through any path
// lands in the same columns.
func NormalizeLoginResult(creds Credentials, machineID string) Credentials {
	out := creds
	out.AccessToken = strings.TrimSpace(out.AccessToken)
	out.RefreshToken = strings.TrimSpace(out.RefreshToken)
	out.UID = strings.TrimSpace(out.UID)
	out.Name = strings.TrimSpace(out.Name)
	out.Email = strings.TrimSpace(out.Email)
	out.OrgID = strings.TrimSpace(out.OrgID)
	out.MachineID = strings.TrimSpace(machineID)
	if out.MachineID == "" {
		out.MachineID = strings.TrimSpace(creds.MachineID)
	}
	return out
}

// CatalogSnapshot renders a catalog as the account's stored snapshot.
func CatalogSnapshot(catalog *Catalog) []string { return catalogToIDs(catalog) }

// ApplyProfile updates the client's private account snapshot as well as its
// resolved identity. NewFromAccount copies its input, so changing the caller's
// account alone would derive runtime fields from the pre-profile UID/org.
func (c *Client) ApplyProfile(profile Profile) {
	if c == nil {
		return
	}
	c.runtimeMu.Lock()
	defer c.runtimeMu.Unlock()
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	oldUID, oldName, oldOrg := c.creds.UID, c.creds.Name, c.creds.OrgID
	if uid := strings.TrimSpace(profile.UID); uid != "" {
		c.creds.UID = uid
		if c.account != nil {
			c.account.QoderUserID = uid
		}
	}
	if name := strings.TrimSpace(profile.Name); name != "" {
		c.creds.Name = name
		if c.account != nil {
			c.account.QoderUserName = name
		}
	}
	if email := strings.TrimSpace(profile.Email); email != "" {
		c.creds.Email = email
		if c.account != nil {
			c.account.Email = email
		}
	}
	if org := strings.TrimSpace(profile.OrgID); org != "" {
		c.creds.OrgID = org
		if c.account != nil {
			c.account.QoderOrganizationID = org
		}
	}
	if len(profile.OrgTags) > 0 {
		c.creds.OrgTags = append([]string(nil), profile.OrgTags...)
		if c.account != nil {
			c.account.QoderOrganizationTags = append([]string(nil), profile.OrgTags...)
		}
	}
	if oldUID != c.creds.UID || oldName != c.creds.Name || oldOrg != c.creds.OrgID {
		c.runtime = RuntimeFields{}
		c.runtimeAccessToken = ""
		c.runtimeRefreshToken = ""
	}
}

// CurrentAccessToken renews an expiring credential before optional profile
// enrichment, returning the token the subsequent profile request must use.
func (c *Client) CurrentAccessToken(ctx context.Context) (string, error) {
	if c == nil {
		return "", fmt.Errorf("qoder client is nil")
	}
	creds, err := c.ensureAccessToken(ctx)
	if err != nil {
		return "", err
	}
	return creds.AccessToken, nil
}

// PrepareCurrentRuntimeFields refreshes first: runtime ciphertext includes both
// tokens and must be derived for the final pair, not the consumed login pair.
func (c *Client) PrepareCurrentRuntimeFields(ctx context.Context) error {
	if c == nil {
		return fmt.Errorf("qoder client is nil")
	}
	creds, err := c.ensureAccessToken(ctx)
	if err != nil {
		return err
	}
	_, err = c.ensureRuntimeFields(ctx, creds)
	return err
}

// FinalizeAccountState derives a runtime pair for the latest observed token
// without refreshing again (a short-lived upstream token can stay within the
// refresh lead), then copies the final state before full-account persistence.
func (c *Client) FinalizeAccountState(ctx context.Context, acc *store.Account) error {
	if c == nil {
		return fmt.Errorf("qoder client is nil")
	}
	if _, err := c.ensureRuntimeFields(ctx, c.currentCredentials()); err != nil {
		return err
	}
	c.CopyAccountState(acc)
	return nil
}

// CopyAccountState copies the final, mutex-protected private snapshot into the
// caller's record. Login clients have ID zero and cannot persist rotations
// directly, while verification's caller may later save the whole account.
func (c *Client) CopyAccountState(acc *store.Account) {
	if c == nil || acc == nil {
		return
	}
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	if c.account == nil {
		return
	}
	acc.QoderAccessToken = c.account.QoderAccessToken
	acc.QoderRefreshToken = c.account.QoderRefreshToken
	acc.QoderExpiresAt = c.account.QoderExpiresAt
	acc.QoderUserID = c.account.QoderUserID
	acc.QoderUserName = c.account.QoderUserName
	acc.Email = c.account.Email
	acc.QoderOrganizationID = c.account.QoderOrganizationID
	acc.QoderOrganizationTags = append([]string(nil), c.account.QoderOrganizationTags...)
	acc.QoderRuntimeInfo = c.account.QoderRuntimeInfo
	acc.QoderRuntimeKey = c.account.QoderRuntimeKey
}
