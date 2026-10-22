package grok

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"orchids-api/internal/accountpolicy"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/store"
)

func (h *Handler) cliBaseURL() string {
	if h != nil && h.configSnapshot() != nil {
		return h.configSnapshot().GrokCLIBaseURLOrDefault()
	}
	return defaultCLIBaseURL
}

func (h *Handler) cliHeaders(acc *store.Account, token string) http.Header {
	if h == nil || h.buildClient() == nil {
		return nil
	}
	return h.buildClient().cliHeaders(acc, token)
}

// doCLIWithAutoSwitchAt issues a CLI request, switching to another OAuth account
// on transient failures (401 after refresh, 5xx) while treating team-level 429
// as shared (no switch).
func (h *Handler) doCLIWithAutoSwitchAt(ctx context.Context, sess *chatAccountSession, payload map[string]any, modelID, path string) (*http.Response, error) {
	if sess == nil || sess.acc == nil {
		return nil, fmt.Errorf("empty cli chat session")
	}
	if h == nil || h.buildClient() == nil {
		return nil, fmt.Errorf("grok cli client not configured")
	}
	immutable, err := cloneBuildPayload(payload)
	if err != nil {
		return nil, err
	}
	prepare := func() error { return prepareBuildPayload(immutable, payload, sess.acc, modelID) }
	if err := prepare(); err != nil {
		return nil, err
	}
	ctx = withReasoningDiagnostics(ctx, payload)
	return h.retryWithAccountSwitch(ctx, sess, 1500*time.Millisecond,
		func() (*http.Response, error) { return h.buildClient().doResponsesAt(ctx, sess.acc, path, payload) },
		func(used []int64) (*chatAccountSession, error) { return h.openCLIAccountSession(ctx, used, modelID) }, prepare)
}

func (h *Handler) openCLIAccountSessionByID(ctx context.Context, accountID int64, modelID string) (*chatAccountSession, error) {
	if h == nil || h.lb == nil || h.lb.Store == nil || accountID == 0 {
		return nil, fmt.Errorf("stored response account is unavailable")
	}
	acc, err := h.lb.Store.GetAccount(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("stored response account is unavailable: %w", err)
	}
	if acc == nil || !acc.Enabled || !accountpolicy.IsGrokAccount(acc) || !AccountSupportsModel(acc, modelID) || !h.routeAllowsAccount(ctx, modelID, acc.ID) || !accountUsableForModel(ctx, acc) || accountpolicy.AccountHeld(acc, time.Now()) || !h.accountCapacityAvailable(acc) {
		return nil, fmt.Errorf("stored response account is unavailable")
	}
	token := strings.TrimSpace(acc.OAuthAccessToken)
	if token == "" {
		token = strings.TrimSpace(acc.OAuthRefreshToken)
	}
	if token == "" {
		return nil, fmt.Errorf("stored response account token is empty")
	}
	if err := scopedRateLimitCooldown(withRateLimitAccount(ctx, acc), ProviderBuild, token, modelID); err != nil {
		return nil, err
	}
	release, reserved := h.reserveAccount(acc)
	if !reserved {
		return nil, fmt.Errorf("stored response account is at its concurrency limit")
	}
	return &chatAccountSession{acc: acc, token: token, release: release}, nil
}

// openCLIAccountSession selects the next available Build CLI OAuth account.
func (h *Handler) openCLIAccountSession(ctx context.Context, excludeIDs []int64, modelID string) (*chatAccountSession, error) {
	if h == nil || h.lb == nil {
		return nil, fmt.Errorf("load balancer not configured")
	}
	// The model travels on the context so the pool filters every provider with one
	// rule; a caller that already set it (the responses handler) is not overridden
	// because the value is identical.
	ctx = WithRequestModel(ctx, modelID)
	if pinnedID := h.affinityAccount(ctx, ProviderBuild); pinnedID != 0 && !containsAccountID(excludeIDs, pinnedID) {
		if pinned, err := h.openCLIAccountSessionByID(ctx, pinnedID, modelID); err == nil {
			if accountAffinityUsable(pinned.acc) {
				return pinned, nil
			}
			pinned.Close()
		}
	}
	snapshot := &cooldownSnapshot{}
	prepare := func(accounts []*store.Account) error {
		var queries []cooldownQuery
		for _, acc := range accounts {
			if acc == nil || containsAccountID(excludeIDs, acc.ID) || !accountpolicy.IsGrokAccount(acc) || !AccountSupportsModel(acc, modelID) || !accountUsableForModel(ctx, acc) {
				continue
			}
			queries = append(queries, cooldownQueries(ctx, acc, modelID)...)
		}
		return snapshot.prepare(ctx, queries)
	}
	match := func(acc *store.Account) error {
		// A model this credential is cooling down for must not be retried on the
		// same account; the account's other models stay eligible. The pool gets the
		// generic reason here: grok holds a model for its own windows (a free-usage
		// window, an inference cap), and naming those as "the plan does not cover
		// the model" would describe them worse than the existing wording does.
		if acc == nil || !accountpolicy.IsGrokAccount(acc) || !AccountSupportsModel(acc, modelID) {
			return loadbalancer.ErrAccountNotEligible
		}
		if !accountUsableForModel(ctx, acc) || !h.routeAllowsAccount(ctx, modelID, acc.ID) {
			return loadbalancer.ErrAccountNotEligible
		}
		// Sibling accounts can look healthy while their shared team is cooling
		// down. Skip them before they consume the request's attempt budget.
		if snapshot.check(ctx, cooldownQueries(ctx, acc, modelID)) != nil {
			return loadbalancer.ErrModelThrottled
		}
		return nil
	}
	acc, err := h.lb.GetNextAccountWithPreparedFilter(ctx, excludeIDs, "grok", h.connTrackerSnapshot(), loadbalancer.PreparedAccountFilter{Prepare: prepare, Match: match})
	if err != nil {
		return nil, err
	}
	token := strings.TrimSpace(acc.OAuthAccessToken)
	if token == "" {
		token = strings.TrimSpace(acc.OAuthRefreshToken)
	}
	if token == "" {
		return nil, fmt.Errorf("grok cli account token is empty")
	}
	h.bindAffinity(ctx, ProviderBuild, acc.ID)
	release, reserved := h.reserveAccount(acc)
	if !reserved {
		return h.openCLIAccountSession(ctx, append(excludeIDs, acc.ID), modelID)
	}
	return &chatAccountSession{
		acc:            acc,
		token:          token,
		poolCandidates: nil,
		release:        release,
	}, nil
}

func containsAccountID(values []int64, id int64) bool {
	for _, value := range values {
		if value == id {
			return true
		}
	}
	return false
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
