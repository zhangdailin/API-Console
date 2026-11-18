package warp

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"time"

	"orchids-api/internal/store"
)

const authFailureCap = 1024

type authFailureEntry struct {
	failure   authFailure
	failures  uint32
	retryAt   time.Time
	expiresAt time.Time
}

// This process-local cache is shared by inference and disposable maintenance
// clients. Keys are fingerprints, and terminal verdicts are also persisted by
// production stores. Credential replacement invalidates the previous generation.
var authFailures = struct {
	sync.Mutex
	entries map[pendingRotationKey]authFailureEntry
}{entries: make(map[pendingRotationKey]authFailureEntry)}

func (c *Client) authFailureKey(grant Credentials) pendingRotationKey {
	key := rotationKey(c.owner, c.account.ID, grant.RefreshToken)
	if c.owner == nil || c.account.ID == 0 {
		key.account = sha256.Sum256([]byte(fmt.Sprintf("client:%p", c)))
	}
	key.expected = sha256.Sum256([]byte(grant.RefreshToken + "\x00" + grant.AccessToken + "\x00" + grant.ExpiresAt.UTC().Format(time.RFC3339Nano)))
	return key
}

func cachedAuthFailure(key pendingRotationKey, now time.Time) error {
	authFailures.Lock()
	defer authFailures.Unlock()
	for k, entry := range authFailures.entries {
		if !now.Before(entry.expiresAt) || (k.account == key.account && k.expected != key.expected) {
			delete(authFailures.entries, k)
		}
	}
	entry, ok := authFailures.entries[key]
	if !ok || (!entry.failure.terminal() && !now.Before(entry.retryAt)) {
		return nil
	}
	failure := entry.failure
	if !failure.terminal() {
		failure.delay = entry.retryAt.Sub(now)
	}
	return &failure
}

func rememberAuthFailure(key pendingRotationKey, err error, now time.Time) error {
	var failure *authFailure
	if !errors.As(err, &failure) {
		return err
	}
	authFailures.Lock()
	defer authFailures.Unlock()
	entry := authFailures.entries[key]
	entry.failure = *failure
	entry.failures = min(entry.failures, 4) + 1
	if !failure.terminal() {
		delay := failure.delay
		if delay <= 0 {
			delay = 30 * time.Second * time.Duration(1<<(entry.failures-1))
		}
		delay = min(max(delay, time.Second), 5*time.Minute)
		entry.failure.delay = delay
		entry.retryAt = now.Add(delay)
	}
	entry.expiresAt = now.Add(24 * time.Hour)
	if _, exists := authFailures.entries[key]; !exists && len(authFailures.entries) >= authFailureCap {
		var oldest pendingRotationKey
		var until time.Time
		for k, item := range authFailures.entries {
			if until.IsZero() || item.expiresAt.Before(until) {
				oldest, until = k, item.expiresAt
			}
		}
		delete(authFailures.entries, oldest)
	}
	authFailures.entries[key] = entry
	copy := entry.failure
	return &copy
}

func forgetAuthFailure(key pendingRotationKey) {
	authFailures.Lock()
	defer authFailures.Unlock()
	delete(authFailures.entries, key)
}

func (c *Client) persistAuthFailure(ctx context.Context, current Credentials, err error) error {
	var failure *authFailure
	if !errors.As(err, &failure) || !failure.terminal() {
		return err
	}
	if recorder, ok := c.owner.(interface {
		UpdateWarpAuthFailure(context.Context, int64, store.WarpAuthFailurePatch) error
	}); ok && c.account.ID != 0 {
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if recorder.UpdateWarpAuthFailure(writeCtx, c.account.ID, store.WarpAuthFailurePatch{
			ExpectedRefreshToken: current.RefreshToken, ExpectedAccessToken: current.AccessToken,
			ExpectedExpiresAt: current.ExpiresAt, Message: failure.Error(), ObservedAt: time.Now(),
		}) != nil {
			return errPersistence
		}
	}
	return err
}
