package qoder

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/prompt"
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
	protocol  protocolProfile
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

	// creds is the resolved credential. runtime holds the derived auth pair.
	creds   Credentials
	runtime RuntimeFields

	// Reference COSY encrypted identity embeds the access/refresh token; unlike
	// the old CLI runtime pair, it must be rederived when either rotates.
	runtimeAccessToken  string
	runtimeRefreshToken string
	requestTimeout      time.Duration

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
	if cfg != nil {
		proxyFunc = util.ProxyFuncFromConfig(cfg)
		proxyKey = util.GenerateProxyKeyFromConfig(cfg)
	}

	client := &Client{
		protocol:       resolveProtocolProfile(cfg),
		endpoints:      resolveEndpoints(cfg),
		clientID:       resolveClientID(cfg),
		clientVersion:  resolveClientVersion(cfg),
		control:        util.GetSharedHTTPClient(proxyKey, authRequestTimeout, proxyFunc),
		stream:         util.GetSharedHTTPClient(proxyKey+"|qoder-chat", 0, proxyFunc),
		requestTimeout: timeout,
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
		// Stored runtime ciphertext is deliberately not reused: it may belong
		// to a different protocol profile. Derive the selected layout below.
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

// applyFingerprint resolves the device headers this client sends.
//
// The machine id is the identity the login was authorized under and stays as
// recorded, but the token and the type are derived from the account so the
// device is the same one on every request and every restart: a device that
// changes under a live credential is what the upstream throttles.
func (c *Client) applyFingerprint() {
	if c.protocol.machineTokenIsID {
		c.machineToken = c.machineID
		c.machineType = sceneClientID
		return
	}
	creds := c.creds
	if c.account != nil {
		creds = ResolveCredentials(c.account)
	}
	device := FingerprintFor(c.machineID, creds.UID, creds.AccessToken)
	if device.Token == "" {
		device.Token = c.machineID
	}
	if device.Type == "" {
		device.Type = sceneClientID
	}
	c.machineToken = device.Token
	c.machineType = device.Type
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
func (c *Client) SendRequestWithPayload(ctx context.Context, req upstream.UpstreamRequest, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	if c == nil {
		return fmt.Errorf("qoder client is nil")
	}
	creds, err := c.ensureAccessToken(ctx)
	if err != nil {
		return err
	}
	fields, err := c.ensureRuntimeFields(ctx, creds)
	if err != nil {
		return err
	}
	model, err := c.resolveModel(req)
	if err != nil {
		return err
	}

	requestID, err := newUUID(c.entropy)
	if err != nil {
		return err
	}
	sessionID, err := newUUID(c.entropy)
	if err != nil {
		return err
	}
	body, err := buildChatBodyProfile(req, model, sessionID, requestID, c.clientVersion, c.aliyunUserType(), c.businessProduct())
	if err != nil {
		return err
	}

	url := chatURL(c.endpoints.inference)
	if logger != nil && !logger.Capturing() {
		logger.LogUpstreamRequest(url, map[string]string{"provider": "qoder", "model": model.Key}, body)
	}

	toolsEnabled := !req.NoTools && len(normalizeToolDefinitions(req, model)) > 0
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
				return refreshErr
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
			if waitErr := sleepCtx(ctx, TransientBackoff(transientRetries)); waitErr != nil {
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
	return asAttemptError(err, &target) && target.unauth
}

func isRetryable(err error) bool {
	var target *attemptStreamError
	return asAttemptError(err, &target) && (target.retryable || target.busy)
}

func asAttemptError(err error, target **attemptStreamError) bool {
	for err != nil {
		if typed, ok := err.(*attemptStreamError); ok {
			*target = typed
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		next := unwrapper.Unwrap()
		if next == err {
			return false
		}
		err = next
	}
	return false
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
	if strings.TrimSpace(rejected.RefreshToken) == "" {
		return ErrReLoginRequired
	}
	if !rejected.RefreshExpiresAt.IsZero() && time.Now().After(rejected.RefreshExpiresAt) {
		return ErrReLoginRequired
	}
	_, err := c.refresh(ctx, rejected)
	return err
}

// refresh renews the device credential and persists the rotation.
func (c *Client) refresh(ctx context.Context, previous Credentials) (Credentials, error) {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

	// Another goroutine may have refreshed while this one waited.
	current, dirty, expectedRefresh := c.currentCredentialState()
	if dirty {
		if err := c.persistCredentials(ctx, current, expectedRefresh); err != nil {
			return Credentials{}, err
		}
		c.markCredentialsClean(current)
	}
	if current.AccessValid(time.Now()) && current.AccessToken != previous.AccessToken {
		return current, nil
	}

	refreshed, err := c.Refresh(ctx, previous.RefreshToken)
	if err != nil {
		return Credentials{}, err
	}
	// The refresh answer carries no identity, so the previous one is kept: the
	// profile fields do not change between token rotations.
	merged := Credentials{
		AccessToken:      refreshed.AccessToken,
		RefreshToken:     firstNonEmptyToken(refreshed.RefreshToken, previous.RefreshToken),
		AccessExpiresAt:  refreshed.AccessExpiresAt,
		RefreshExpiresAt: refreshed.RefreshExpiresAt,
		UID:              firstNonEmptyToken(refreshed.UID, previous.UID),
		Name:             firstNonEmptyToken(refreshed.Name, previous.Name),
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
	var fields RuntimeFields
	var err error
	if c.protocol.tokenlessRuntime {
		fields, err = runtimeFieldsFor(c.entropy, runtimeFieldInput{
			UID:              creds.UID,
			OrganizationID:   creds.OrgID,
			OrganizationTags: creds.OrgTags,
			DataPolicyAgreed: c.dataPolicyAgreed(),
		})
	} else {
		fields, err = referenceRuntimeFieldsFor(c.entropy, referenceRuntimeFieldInput{
			Name:               creds.Name,
			Aid:                creds.UID,
			UID:                creds.UID,
			OrganizationID:     creds.OrgID,
			UserType:           aliyunUserTypeOr(c.aliyunUserType()),
			SecurityOAuthToken: creds.AccessToken,
			RefreshToken:       creds.RefreshToken,
		})
	}
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
	defer c.stateMu.RUnlock()
	if c.account != nil {
		if catalog := catalogFromIDs(c.account.QoderModelIDs); catalog.Len() > 0 {
			return catalog
		}
	}
	return newCatalog(nil)
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
	if err := accountStore.UpdateAccount(writeCtx, &acc); err != nil {
		return fmt.Errorf("persist qoder account state: %w", err)
	}
	return nil
}

// VerifyModel proves the account can actually run one model by issuing a
// minimal streaming completion. Model refresh uses the catalog instead, so this
// is reserved for an explicit single-model probe.
func (c *Client) VerifyModel(ctx context.Context, modelID string) error {
	return c.SendRequestWithPayload(ctx, upstream.UpstreamRequest{
		Model:    strings.TrimSpace(modelID),
		Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "ping"}}},
	}, nil, nil)
}

// PrepareRuntimeFields derives the authentication pair if it is not present yet.
// It exists so a login can prove the derivation works before the credential is
// persisted, instead of surfacing the failure on the first chat request.
func (c *Client) PrepareRuntimeFields(ctx context.Context) error {
	if c == nil {
		return fmt.Errorf("qoder client is nil")
	}
	_, err := c.ensureRuntimeFields(ctx, c.currentCredentials())
	return err
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
func CatalogSnapshot(catalog *Catalog) []string {
	return catalogToIDs(catalog)
}

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
	oldTags := append([]string(nil), c.creds.OrgTags...)
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
	if oldUID != c.creds.UID || oldName != c.creds.Name || oldOrg != c.creds.OrgID || (c.protocol.tokenlessRuntime && !slices.Equal(oldTags, c.creds.OrgTags)) {
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
