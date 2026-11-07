package web

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/store"
	"orchids-api/internal/util"
)

// errNotWebAccount withholds a candidate that is not a usable Web SSO account.
var errNotWebAccount = errors.New("account is not an eligible Grok Web SSO account")

// accountFilter adapts a predicate to the load balancer's verdict filter.
func accountFilter(match func(*store.Account) bool) loadbalancer.AccountFilter {
	return func(acc *store.Account) error {
		if match(acc) {
			return nil
		}
		return errNotWebAccount
	}
}

func containsAccountID(values []int64, id int64) bool {
	return slices.Contains(values, id)
}

func accountAffinityUsable(acc *store.Account) bool {
	if acc == nil || !acc.Enabled {
		return false
	}
	status := strings.TrimSpace(acc.StatusCode)
	if status == "" {
		return true
	}
	return !acc.QuotaResetAt.IsZero() && time.Now().After(acc.QuotaResetAt)
}

const (
	defaultAccountSwitchBudget = 20
	maxAccountSwitchBudget     = 100
)

// retryWithAccountSwitch runs a request in a bounded loop, switching to
// the next account whenever shouldSwitchGrokAccount fires. doRequest issues the
// request against the current session; openNext returns its replacement.
// onSwitch runs after each successful account swap (e.g. to rebuild the request
// payload for the new account).
func (h *Handler) retryWithAccountSwitch(ctx context.Context, sess *chatAccountSession, switchPace time.Duration, doRequest func() (*http.Response, error), openNext func(used []int64) (*chatAccountSession, error), onSwitch func() error) (*http.Response, error) {
	return h.retryWithAccountSwitchLimit(ctx, sess, switchPace, doRequest, openNext, onSwitch, 0)
}

func (h *Handler) retryWithAccountSwitchLimit(ctx context.Context, sess *chatAccountSession, switchPace time.Duration, doRequest func() (*http.Response, error), openNext func(used []int64) (*chatAccountSession, error), onSwitch func() error, hardLimit int) (*http.Response, error) {
	maxAttempts := defaultAccountSwitchBudget
	if h != nil && h.configSnapshot() != nil && h.configSnapshot().AccountSwitchCount > 0 {
		maxAttempts = min(h.configSnapshot().AccountSwitchCount, maxAccountSwitchBudget)
	}
	if hardLimit > 0 && maxAttempts > hardLimit {
		maxAttempts = hardLimit
	}

	used := make([]int64, 0)
	attempt := 0
	for {
		attempt++
		if sess.acc != nil && sess.acc.ID != 0 {
			used = append(used, sess.acc.ID)
		}
		started := time.Now()
		resp, err := doRequest()
		provider := "web"
		if sess.acc != nil {
			provider = ProviderForAccount(sess.acc)
		}
		h.auditAttemptDiagnostic(ctx, sess.acc, provider, attempt, started, err, "account_attempt", resp, nil, "")
		if err == nil {
			return resp, nil
		}
		if markAllGrokAccountStatuses(err) {
			h.markAccountStatus(ctx, sess.acc, err)
		}
		if !shouldSwitchGrokAccount(err) || attempt >= maxAttempts {
			return nil, err
		}

		sess.Close()
		if !util.SleepWithContext(ctx, switchPace) {
			return nil, ctx.Err()
		}
		next, switchErr := openNext(used)
		if switchErr != nil {
			return nil, err
		}
		sess.acc = next.acc
		sess.token = next.token
		sess.poolCandidates = next.poolCandidates
		sess.release = next.release
		if onSwitch != nil {
			if err := onSwitch(); err != nil {
				return nil, err
			}
		}
	}
}
