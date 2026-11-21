package web

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"orchids-api/internal/grok/grokbase"
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

func (h *Handler) retryWithAccountSwitch(ctx context.Context, sess *chatAccountSession, switchPace time.Duration, call func() (*http.Response, error), next func([]int64) (*chatAccountSession, error), rebuild func() error) (*http.Response, error) {
	limit := defaultAccountSwitchBudget
	if cfg := h.configSnapshot(); cfg != nil && cfg.AccountSwitchCount > 0 {
		limit = min(cfg.AccountSwitchCount, maxAccountSwitchBudget)
	}
	return grokbase.RetryAccountSwitch(ctx, sess, grokbase.AccountSwitch[chatAccountSession]{
		Limit:   limit,
		Account: func() *store.Account { return sess.acc }, Release: sess.Close,
		Call: call, Next: next, Rebuild: rebuild,
		Audit: func(a grokbase.Attempt) {
			a.Provider = "web"
			if a.Account != nil {
				a.Provider = a.Account.GrokPlane()
			}
			grokbase.LogAttempt(ctx, h.auditLoggerSnapshot(), a, ClassifyUpstreamError)
		},
		MarkFailure: func(acc *store.Account, err error) {
			if markAllGrokAccountStatuses(err) {
				h.markAccountStatus(ctx, acc, err)
			}
		},
		ShouldSwitch: shouldSwitchGrokAccount,
		Wait: func(error) error {
			if !util.SleepWithContext(ctx, switchPace) {
				return ctx.Err()
			}
			return nil
		},
	})
}
