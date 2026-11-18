package warp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/refreshqueue"
	"orchids-api/internal/store"
)

type Credentials struct {
	AccessToken, RefreshToken string
	ExpiresAt                 time.Time
	Email, UserID             string
}

func credentials(acc *store.Account) Credentials {
	if acc == nil {
		return Credentials{}
	}
	return Credentials{AccessToken: acc.WarpAccessToken, RefreshToken: acc.WarpRefreshToken, ExpiresAt: acc.WarpExpiresAt, Email: acc.Email, UserID: acc.UserID}
}
func (g Credentials) valid() bool {
	return g.AccessToken != "" && !g.ExpiresAt.IsZero() && time.Until(g.ExpiresAt) > 5*time.Minute
}
func applyCredentials(acc *store.Account, g Credentials) {
	acc.WarpAccessToken = g.AccessToken
	acc.WarpRefreshToken = g.RefreshToken
	acc.WarpExpiresAt = g.ExpiresAt
}

func expiry(seconds string) (time.Time, error) {
	n, err := strconv.ParseInt(seconds, 10, 64)
	if err != nil || n <= 0 || n > 86400 {
		return time.Time{}, errResponse
	}
	return time.Now().Add(time.Duration(n) * time.Second), nil
}

// Identity claims are display/deduplication metadata only, read after an authenticated exchange.
func identityMetadata(g *Credentials) {
	parts := strings.Split(g.AccessToken, ".")
	if len(parts) != 3 {
		return
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(raw) > 16<<10 {
		return
	}
	var claims struct {
		Email string `json:"email"`
		Sub   string `json:"sub"`
	}
	if json.Unmarshal(raw, &claims) == nil {
		g.Email = claims.Email
		g.UserID = claims.Sub
	}
}
func (c *Client) exchangeFirebase(ctx context.Context, custom string) (Credentials, error) {
	endpoint, err := c.keyedURL(ctx, c.firebaseBase, "/v1/accounts:signInWithCustomToken")
	if err != nil {
		return Credentials{}, err
	}
	g, err := c.exchangeAuthTokens(ctx, endpoint, "/proxy/customToken", "returnSecureToken", "true", "token", custom)
	if err != nil {
		return Credentials{}, err
	}
	if g.RefreshToken == "" {
		return Credentials{}, transientAuthFailure("invalid_response", 0)
	}
	identityMetadata(&g)
	return g, nil
}
func (c *Client) refresh(ctx context.Context, previous Credentials) (Credentials, error) {
	if previous.RefreshToken == "" {
		return Credentials{}, errCredential
	}
	endpoint, err := c.keyedURL(ctx, c.tokenBase, "/v1/token")
	if err != nil {
		return Credentials{}, err
	}
	g, err := c.exchangeAuthTokens(ctx, endpoint, "/proxy/token", "grant_type", "refresh_token", "refresh_token", previous.RefreshToken)
	if err != nil {
		return Credentials{}, err
	}
	if g.RefreshToken == "" {
		g.RefreshToken = previous.RefreshToken
	}
	g.Email, g.UserID = previous.Email, previous.UserID
	return g, nil
}

const (
	pendingRotationTTL = 10 * time.Minute
	pendingRotationCap = 256
)

type pendingRotationKey struct {
	account  [32]byte
	expected [32]byte
}

type pendingRotation struct {
	grant     Credentials
	expiresAt time.Time
}

// Disposable control-plane clients must recover a rotated grant after a failed
// commit. Cache keys contain fingerprints only, never refresh/access material.
var pendingRotations = struct {
	sync.Mutex
	entries map[pendingRotationKey]pendingRotation
}{entries: make(map[pendingRotationKey]pendingRotation)}

func rotationKey(owner AccountUpdater, id int64, refresh string) pendingRotationKey {
	return pendingRotationKey{
		account:  sha256.Sum256([]byte(fmt.Sprintf("%T:%p:%d", owner, owner, id))),
		expected: sha256.Sum256([]byte(refresh)),
	}
}

// Called while holding the account credential gate. An authoritative replacement
// invalidates recovery material for every previous refresh fingerprint.
func recoverRotation(key pendingRotationKey, now time.Time) (Credentials, bool) {
	pendingRotations.Lock()
	defer pendingRotations.Unlock()
	for k, entry := range pendingRotations.entries {
		if !now.Before(entry.expiresAt) || (k.account == key.account && k.expected != key.expected) {
			delete(pendingRotations.entries, k)
		}
	}
	entry, ok := pendingRotations.entries[key]
	return entry.grant, ok
}

func rememberRotation(key pendingRotationKey, grant Credentials, now time.Time) {
	pendingRotations.Lock()
	defer pendingRotations.Unlock()
	for k, entry := range pendingRotations.entries {
		if !now.Before(entry.expiresAt) {
			delete(pendingRotations.entries, k)
		}
	}
	if _, exists := pendingRotations.entries[key]; !exists && len(pendingRotations.entries) >= pendingRotationCap {
		var oldest pendingRotationKey
		var expiry time.Time
		for k, entry := range pendingRotations.entries {
			if expiry.IsZero() || entry.expiresAt.Before(expiry) {
				oldest, expiry = k, entry.expiresAt
			}
		}
		delete(pendingRotations.entries, oldest)
	}
	pendingRotations.entries[key] = pendingRotation{grant: grant, expiresAt: now.Add(pendingRotationTTL)}
}

func forgetRotation(key pendingRotationKey) {
	pendingRotations.Lock()
	defer pendingRotations.Unlock()
	delete(pendingRotations.entries, key)
}

func usableAccount(acc *store.Account) bool {
	return acc != nil && strings.EqualFold(strings.TrimSpace(acc.AccountType), "warp") && acc.Enabled && store.AccountAuthActive(acc)
}

// Rotation is serialized across client instances, then committed with a compare-and-swap patch.
func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.account == nil {
		return "", errCredential
	}
	id := c.account.ID
	release, err := refreshqueue.AcquireCredential(ctx, "warp", c.owner, id)
	if err != nil {
		return "", err
	}
	defer release()
	latest, err := refreshqueue.LatestAccount(ctx, c.owner, id)
	if err != nil {
		return "", errPersistence
	}
	if _, authoritative := c.owner.(interface {
		GetAccount(context.Context, int64) (*store.Account, error)
	}); authoritative && id != 0 && latest == nil {
		return "", errCredential
	}
	if latest != nil {
		copy := *latest
		copy.WarpModelIDs = append([]string(nil), latest.WarpModelIDs...)
		c.account = &copy
	}
	current := credentials(c.account)
	key := rotationKey(c.owner, id, current.RefreshToken)
	next, pending := recoverRotation(key, time.Now())
	if !usableAccount(c.account) {
		forgetRotation(key)
		return "", errCredential
	}
	if !pending && current.valid() {
		return current.AccessToken, nil
	}
	if !pending {
		failureKey := c.authFailureKey(current)
		if failure := cachedAuthFailure(failureKey, time.Now()); failure != nil {
			return "", c.persistAuthFailure(ctx, current, failure)
		}
		next, err = c.refresh(ctx, current)
		if err != nil {
			failure := rememberAuthFailure(failureKey, err, time.Now())
			return "", c.persistAuthFailure(ctx, current, failure)
		}
		forgetAuthFailure(failureKey)
		if c.owner != nil && id != 0 {
			rememberRotation(key, next, time.Now())
		}
	}
	if c.owner != nil && id != 0 {
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := c.owner.UpdateWarpCredentials(writeCtx, id, store.WarpCredentialPatch{ExpectedRefreshToken: current.RefreshToken, AccessToken: next.AccessToken, RefreshToken: next.RefreshToken, ExpiresAt: next.ExpiresAt}); err != nil {
			return "", errPersistence
		}
	}
	forgetRotation(key)
	applyCredentials(c.account, next)
	return next.AccessToken, nil
}

// Recheck the authoritative account after refresh and immediately before paid
// generation. A concurrent disable, deletion, or login replacement fails closed.
func (c *Client) authorizeGeneration(ctx context.Context, token, model string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.account == nil {
		return errCredential
	}
	latest, err := refreshqueue.LatestAccount(ctx, c.owner, c.account.ID)
	if err != nil {
		return errPersistence
	}
	if _, authoritative := c.owner.(interface {
		GetAccount(context.Context, int64) (*store.Account, error)
	}); authoritative && c.account.ID != 0 && latest == nil {
		return errCredential
	}
	if latest != nil {
		if !usableAccount(latest) || latest.WarpRefreshToken != c.account.WarpRefreshToken || latest.WarpAccessToken != token {
			return errCredential
		}
		if !(capabilities{}).SupportsModel(latest, model) {
			return errCredential
		}
	}
	if !usableAccount(c.account) {
		return errCredential
	}
	return nil
}
