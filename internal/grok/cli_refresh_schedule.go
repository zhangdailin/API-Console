package grok

import (
	"crypto/sha256"
	"encoding/binary"
	"strconv"
	"strings"
	"time"

	"orchids-api/internal/store"
)

const (
	// cliRefreshBaseLead is how long before expiry the scheduler renews a token.
	cliRefreshBaseLead = 5 * time.Minute
	// cliRefreshJitterSeconds spreads accounts that were logged in together
	// across three minutes, so a pool does not refresh in one burst.
	cliRefreshJitterSeconds = 181
	// cliRefreshConfigurationRetry is the minimum wait after a configuration
	// refusal: retrying sooner cannot succeed until the configuration changes.
	cliRefreshConfigurationRetry = 30 * time.Minute
	// cliRefreshUnclassifiedLimit is how many consecutive identical
	// unclassified refusals, with no live access token left, end in a re-login.
	cliRefreshUnclassifiedLimit = 5
)

// cliRefreshBackoffSteps is the wait after the 1st, 2nd, ... consecutive
// failure; later failures keep the last step.
var cliRefreshBackoffSteps = []time.Duration{30 * time.Second, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 15 * time.Minute}

// cliRefreshLead returns the account's renewal lead time: the base lead plus a
// jitter that is stable per account, so the order of renewals is reproducible.
func cliRefreshLead(id int64) time.Duration {
	sum := sha256.Sum256([]byte(strconv.FormatInt(id, 10)))
	jitter := binary.BigEndian.Uint64(sum[:8]) % cliRefreshJitterSeconds
	return cliRefreshBaseLead + time.Duration(jitter)*time.Second
}

// cliRefreshDue reports whether the scheduler should renew the account's
// token now: it is missing, has no expiry, or is inside its lead time, and no
// earlier failure has asked to wait.
func cliRefreshDue(acc *store.Account, now time.Time) bool {
	if acc == nil || acc.GrokRefresh.InBackoff(now) {
		return false
	}
	if strings.TrimSpace(acc.OAuthAccessToken) == "" || acc.OAuthExpiresAt.IsZero() {
		return true
	}
	return acc.OAuthExpiresAt.Sub(now) <= cliRefreshLead(acc.ID)
}

// cliRefreshBackoff returns the wait after the given number of consecutive
// failures, with a small per-account offset so retries do not align.
func cliRefreshBackoff(failures int, id int64) time.Duration {
	step := cliRefreshBackoffSteps[min(max(failures, 1), len(cliRefreshBackoffSteps))-1]
	offset := id * 37 % 16
	if offset < 0 {
		offset = -offset
	}
	return step + time.Duration(offset)*time.Second
}

// cliAccessTokenAlive reports whether the stored access token can still be
// sent upstream.
func cliAccessTokenAlive(acc *store.Account, now time.Time) bool {
	if acc == nil || strings.TrimSpace(acc.OAuthAccessToken) == "" {
		return false
	}
	return acc.OAuthExpiresAt.IsZero() || now.Before(acc.OAuthExpiresAt)
}

// recordCLIRefreshFailure records a failed scheduled refresh on the account
// and reports whether the account now needs a new login. Only the error class
// and code are stored, never a token or a response body.
//
//   - permanent: the grant is gone. A still-valid access token keeps serving
//     until it expires; after that the account needs a login.
//   - configuration: no account-specific verdict; retry no sooner than 30m.
//   - unclassified: back off; the same code repeated cliRefreshUnclassifiedLimit
//     times with no live access token left needs a login.
//   - transient: back off.
func recordCLIRefreshFailure(acc *store.Account, err error, now time.Time) (reauth bool) {
	class, code := cliOAuthFailure(err)
	state := acc.GrokRefresh
	if state.ErrorClass == class && state.ErrorCode == code {
		state.SameErrorCount++
	} else {
		state.SameErrorCount = 1
	}
	state.Failures++
	state.ErrorClass = class
	state.ErrorCode = code
	state.UpdatedAt = now
	alive := cliAccessTokenAlive(acc, now)
	delay := cliRefreshBackoff(state.Failures, acc.ID)

	switch class {
	case cliOAuthPermanent:
		if alive && !acc.OAuthExpiresAt.IsZero() {
			// Nothing to retry: wait for the live token to run out.
			state.RetryAt = acc.OAuthExpiresAt
		} else {
			reauth = true
			state.RetryAt = time.Time{}
		}
	case cliOAuthConfiguration:
		state.RetryAt = now.Add(max(delay, cliRefreshConfigurationRetry))
	case cliOAuthUnclassified:
		if state.SameErrorCount >= cliRefreshUnclassifiedLimit && !alive {
			reauth = true
			state.RetryAt = time.Time{}
		} else {
			state.RetryAt = now.Add(delay)
		}
	default:
		state.RetryAt = now.Add(delay)
	}
	acc.GrokRefresh = state
	return reauth
}

// recordCLIRefreshSuccess clears the failure history after a renewal.
func recordCLIRefreshSuccess(acc *store.Account, now time.Time) {
	if acc.GrokRefresh.Failures == 0 && acc.GrokRefresh.RetryAt.IsZero() && acc.GrokRefresh.ErrorCode == "" {
		return
	}
	acc.GrokRefresh = store.GrokRefreshState{UpdatedAt: now}
}
