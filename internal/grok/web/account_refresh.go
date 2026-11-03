package web

import (
	"context"
	"time"

	"orchids-api/internal/store"
)

// WebRefreshOptions keeps caller-specific timing policy outside the shared Web
// SSO probe. Manual refresh and the background scheduler intentionally use
// different quota deadlines.
type WebRefreshOptions struct {
	IdentityTimeout time.Duration
	QuotaTimeout    time.Duration
	RetryDelay      time.Duration
}

// WebRefreshResult is an in-memory observation. ProbeWebAccount never writes to
// the account store; callers remain responsible for status policy and
// persistence.
type WebRefreshResult struct {
	Identity    AccountIdentity
	IdentityErr error
	Quota       map[string]*RateLimitInfo
	QuotaErr    error

	// AuthRejected is true only after the same surface rejects the credential
	// twice. A non-authentication error on the second attempt is not a credential
	// verdict.
	AuthRejected bool
}

// ProbeWebAccount reads identity and quota with a two-witness authentication
// rule. Quota failures which are not authentication failures remain independent
// from identity validity and are returned for caller-specific handling.
func ProbeWebAccount(ctx context.Context, client *Client, token string, opts WebRefreshOptions) WebRefreshResult {
	identity, identityErr := retryWebObservation(ctx, opts.IdentityTimeout, opts.RetryDelay, func(attemptCtx context.Context) (AccountIdentity, error) {
		return client.FetchSessionIdentity(attemptCtx, token)
	})
	result := WebRefreshResult{Identity: identity, IdentityErr: identityErr}
	if identityErr != nil && IsAuthenticationFailure(identityErr) {
		result.AuthRejected = true
		return result
	}

	quota, quotaErr := retryWebObservation(ctx, opts.QuotaTimeout, opts.RetryDelay, func(attemptCtx context.Context) (map[string]*RateLimitInfo, error) {
		return client.GetWebQuota(attemptCtx, token)
	})
	result.Quota = quota
	result.QuotaErr = quotaErr
	result.AuthRejected = quotaErr != nil && IsAuthenticationFailure(quotaErr)
	return result
}

func retryWebObservation[T any](ctx context.Context, timeout, delay time.Duration, attempt func(context.Context) (T, error)) (T, error) {
	read := func() (T, error) {
		attemptCtx := ctx
		cancel := func() {}
		if timeout > 0 {
			attemptCtx, cancel = context.WithTimeout(ctx, timeout)
		}
		defer cancel()
		return attempt(attemptCtx)
	}

	value, err := read()
	if err == nil || !IsAuthenticationFailure(err) {
		return value, err
	}
	if delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			var zero T
			return zero, err
		case <-timer.C:
		}
	}
	return read()
}

// ApplyWebRefresh copies only successful observations onto an account.
func ApplyWebRefresh(acc *store.Account, result WebRefreshResult) {
	if acc == nil {
		return
	}
	if result.IdentityErr == nil {
		if result.Identity.UserID != "" {
			acc.UserID = result.Identity.UserID
		}
		if result.Identity.Email != "" {
			acc.Email = result.Identity.Email
		}
		if result.Identity.TeamID != "" {
			acc.TeamID = result.Identity.TeamID
		}
	}
	if result.QuotaErr == nil && result.Quota != nil {
		ApplyWebQuotaInfo(acc, result.Quota)
	}
}
