package web

import (
	"context"
	"fmt"
	"net/http"
	"orchids-api/internal/grok/grokbase"
	"orchids-api/internal/grok/ratelimit"
	"orchids-api/internal/store"
	"strings"
	"time"
)

func (h *Handler) openChatAccountSessionForModel(ctx context.Context, spec ModelSpec) (*chatAccountSession, error) {
	return h.openChatAccountSessionForModelExcluding(ctx, nil, spec)
}

func (h *Handler) openChatAccountSessionForModelExcluding(ctx context.Context, excludeIDs []int64, spec ModelSpec) (*chatAccountSession, error) {
	product := mediaProductFromContext(ctx)
	if product == mediaNone {
		product = mediaProductFor(spec, "")
	}
	sess, err := h.openChatAccountSessionExcludingWithPoolsAndFilter(ctx, excludeIDs, spec.PoolCandidates(), func(acc *store.Account) bool {
		return h.routeAllowsAccount(ctx, spec.ID, acc.ID) && mediaAllowed(acc, product, time.Now())
	})
	if err != nil && product != mediaNone {
		return nil, &mediaUnavailableError{message: h.mediaUnavailableMessage(ctx, product), cause: err}
	}
	return sess, err
}

// mediaUnavailableError is the selection failure of a media request: no
// account has the product's allowance left. It answers 429 with the reason.
type mediaUnavailableError struct {
	message string
	cause   error
}

func (e *mediaUnavailableError) Error() string { return e.message }

func (e *mediaUnavailableError) Unwrap() error { return e.cause }

func (h *Handler) routeAllowsAccount(ctx context.Context, modelID string, accountID int64) bool {
	return grokbase.RouteAllowsAccount(ctx, h.affinityStore(), normalizeModelID(modelID), accountID)
}

func (h *Handler) openChatAccountSessionExcludingWithPools(ctx context.Context, excludeIDs []int64, poolCandidates []string) (*chatAccountSession, error) {
	return h.openChatAccountSessionExcludingWithPoolsAndFilter(ctx, excludeIDs, poolCandidates, nil)
}

// modelScopeKey carries the model a request is for, so account selection can
// skip an account that is cooling down for THAT model while still using it for
// the account's other models.
type modelScopeKey struct{}

// WithRequestModel records the model on the request context for account
// selection. It is threaded through the existing context so no selector
// signature has to change.
func WithRequestModel(ctx context.Context, modelID string) context.Context {
	normalized := normalizeModelID(modelID)
	if normalized == "" {
		return ctx
	}
	return context.WithValue(ctx, modelScopeKey{}, normalized)
}

func requestModelFromContext(ctx context.Context) string {
	model, _ := ctx.Value(modelScopeKey{}).(string)
	return model
}

// accountUsableForModel reports whether the account may serve the current
// request's model. The model-scoped cooldown is the persisted verdict of a
// per-model failure: a throttled model must not remove the account's other
// models from the pool.
func accountUsableForModel(ctx context.Context, acc *store.Account) bool {
	return grokbase.AccountUsableForModel(acc, requestModelFromContext(ctx))
}

func (h *Handler) openChatAccountSessionExcludingWithPoolsAndFilter(ctx context.Context, excludeIDs []int64, poolCandidates []string, extraFilter func(*store.Account) bool) (*chatAccountSession, error) {
	if h.lb == nil {
		return nil, fmt.Errorf("load balancer not configured")
	}
	var (
		acc     *store.Account
		err     error
		lastErr error
	)
	// App-chat/media only accepts Web SSO cookie accounts. OAuth Build accounts
	// are selected by their own provider-specific selector.
	ssoFilter := func(acc *store.Account) bool {
		if !acc.IsGrokWeb() {
			return false
		}
		if !accountUsableForModel(ctx, acc) {
			return false
		}
		return extraFilter == nil || extraFilter(acc)
	}
	if pinnedID := h.affinityAccount(ctx, ProviderWeb); pinnedID != 0 && !containsAccountID(excludeIDs, pinnedID) && h.lb.Store != nil {
		if pinned, getErr := h.lb.Store.GetAccount(ctx, pinnedID); getErr == nil && pinned != nil && accountAffinityUsable(pinned) && h.accountCapacityAvailable(pinned) && ssoFilter(pinned) {
			raw := grokSSOTokenRaw(pinned)
			if NormalizeSSOToken(raw) != "" {
				if release, reserved := h.reserveAccount(ctx, pinned); reserved {
					return &chatAccountSession{acc: pinned, token: raw, poolCandidates: normalizeGrokPoolCandidates(poolCandidates), release: release}, nil
				}
			}
		}
	}
	candidates := normalizeGrokPoolCandidates(poolCandidates)
	if len(candidates) == 0 {
		acc, err = h.lb.GetNextAccountExcludingByChannelWithTrackerFilter(ctx, excludeIDs, "grok", h.connTrackerSnapshot(), accountFilter(ssoFilter))
		if err != nil {
			return nil, err
		}
	} else {
		for _, pool := range candidates {
			wantPool := pool
			acc, err = h.lb.GetNextAccountExcludingByChannelWithTrackerFilter(ctx, excludeIDs, "grok", h.connTrackerSnapshot(), accountFilter(func(acc *store.Account) bool {
				return strings.EqualFold(grokAccountPool(acc), wantPool) && ssoFilter(acc)
			}))
			if err == nil && acc != nil {
				break
			}
			if err != nil {
				if lastErr == nil || (!carriesPoolReason(lastErr) && carriesPoolReason(err)) {
					// Keep the error that says why the pool is empty. A bare "no
					// enabled accounts available" carries no reason, so a later pool
					// that reports one (cooling down, rate limited, allowance spent,
					// busy) must replace it — otherwise the failure reaches the client
					// as a 503 that explains nothing.
					lastErr = err
				}
			}
		}
		if acc == nil {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, fmt.Errorf("no enabled grok accounts available for requested pools: %s", strings.Join(candidates, ","))
		}
	}
	raw := grokSSOTokenRaw(acc)
	if NormalizeSSOToken(raw) == "" {
		return nil, fmt.Errorf("grok account token is empty")
	}
	h.bindAffinity(ctx, ProviderWeb, acc.ID)
	release, reserved := h.reserveAccount(ctx, acc)
	if !reserved {
		return h.openChatAccountSessionExcludingWithPoolsAndFilter(ctx, append(excludeIDs, acc.ID), poolCandidates, extraFilter)
	}
	return &chatAccountSession{
		acc:            acc,
		token:          raw,
		poolCandidates: candidates,
		release:        release,
	}, nil
}

func (s *chatAccountSession) Close() {
	if s == nil || s.release == nil {
		return
	}
	s.release()
	s.release = nil
}

// doSingleAccountRequest calls the Grok API once on a single account without retry switching.
func (h *Handler) doSingleAccountRequest(
	ctx context.Context,
	sess *chatAccountSession,
	payload map[string]any,
	shouldMarkStatus grokAccountStatusPolicy,
	callAPI func(*Client, context.Context, string, map[string]any) (*http.Response, error),
) (*http.Response, error) {
	if sess == nil || strings.TrimSpace(sess.token) == "" {
		return nil, fmt.Errorf("empty chat session")
	}
	client := h.currentClient()
	if client == nil {
		return nil, fmt.Errorf("grok client not configured")
	}
	requestCtx := withRateLimitAccount(ctx, sess.acc)
	resp, err := callAPI(client, requestCtx, sess.token, payload)
	if err != nil {
		if shouldMarkStatus == nil || shouldMarkStatus(err) {
			h.markAccountStatus(ctx, sess.acc, err)
		}
		return nil, err
	}
	return resp, nil
}

type grokAccountStatusPolicy func(error) bool

func markAllGrokAccountStatuses(err error) bool {
	if err == nil {
		return false
	}
	// Egress challenges and shared team rate limits are not account failures.
	if isEgressChallengeError(err) {
		return false
	}
	if isSharedGrokRateLimitError(err) {
		return false
	}
	// A generic 403 must not mark the account; only explicit account blocks do.
	if ClassifyUpstreamError(err) == UpstreamErrorGenericForbidden {
		return false
	}
	return true
}

func skipExternalAttachmentFetchGrokAccountStatus(err error) bool {
	if err == nil {
		return false
	}
	return !strings.Contains(strings.ToLower(err.Error()), "fetch url status=")
}

// doChatWithAutoSwitchRebuild calls doChat with automatic account switching and payload rebuild on failure.
func (h *Handler) doChatWithAutoSwitchRebuild(
	ctx context.Context,
	sess *chatAccountSession,
	payload *map[string]any,
	rebuild func(token string) (map[string]any, error),
) (*http.Response, error) {
	return h.doAutoSwitchRequest(ctx, sess, payload, rebuild, (*Client).doChat)
}

// doAutoSwitchRequest calls the Grok API with automatic account switching on
// failure, rebuilding the request payload for the new account when rebuild is set.
func (h *Handler) doAutoSwitchRequest(
	ctx context.Context,
	sess *chatAccountSession,
	payload *map[string]any,
	rebuild func(token string) (map[string]any, error),
	callAPI func(*Client, context.Context, string, map[string]any) (*http.Response, error),
) (*http.Response, error) {
	if sess == nil || strings.TrimSpace(sess.token) == "" {
		return nil, fmt.Errorf("empty chat session")
	}
	if payload == nil {
		return nil, fmt.Errorf("empty payload")
	}
	client := h.currentClient()
	if client == nil {
		return nil, fmt.Errorf("grok client not configured")
	}
	return h.retryWithAccountSwitch(ctx, sess, 100*time.Millisecond,
		func() (*http.Response, error) {
			return callAPI(client, withRateLimitAccount(ctx, sess.acc), sess.token, *payload)
		},
		func(used []int64) (*chatAccountSession, error) {
			return h.openChatAccountSessionExcludingWithPools(ctx, used, sess.poolCandidates)
		},
		func() error {
			if rebuild == nil {
				return nil
			}
			newPayload, rbErr := rebuild(sess.token)
			if rbErr != nil {
				return rbErr
			}
			*payload = newPayload
			return nil
		})
}

// isEgressChallengeError reports whether an error is a Cloudflare interstitial
// or DPoP proof challenge. These are egress/clearance problems, not account
// problems: switching accounts (or cooling the account) is wrong.
func isEgressChallengeError(err error) bool {
	if err == nil {
		return false
	}
	kind := ClassifyUpstreamError(err)
	return kind == UpstreamErrorCloudflareChallenge || kind == UpstreamErrorDPoPChallenge
}

func shouldSwitchGrokAccount(err error) bool {
	if err == nil {
		return false
	}
	// Cloudflare / DPoP challenges should not drain the account pool: switching
	// to another account hits the same wall. Leave to the egress layer instead.
	if isEgressChallengeError(err) {
		return false
	}
	// Response-aware classification: only an explicit account block switches
	// accounts. A generic 403 (feature/plan/permission) is not an account
	// failure.
	switch ClassifyUpstreamError(err) {
	case UpstreamErrorAccountBlock:
		return true
	case UpstreamErrorGenericForbidden:
		return false
	}
	return grokbase.TransientAccountFailure(parseUpstreamStatus(err), err.Error(), isSharedGrokRateLimitError(err))
}

func isSharedGrokRateLimitError(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	if parseUpstreamStatus(err) != http.StatusTooManyRequests && !strings.Contains(lower, "too many requests") {
		return false
	}
	// Only a parsed Team+Model response is shared. A bare 429/"too many
	// requests" is account-scoped in grok2api and must cool the selected account;
	// treating it as shared left every account looking healthy while requests
	// repeatedly rotated through a pool of throttled credentials.
	if ratelimit.ParseMetadata([]byte(err.Error())) != nil {
		return true
	}
	// Errors emitted by waitScopedRateLimit are synthetic: the structured
	// response was parsed on the preceding attempt and the team/model cooldown
	// is already registered, so there is no response body left to parse here.
	return strings.Contains(lower, "body=too_many_requests team ") &&
		strings.Contains(lower, " cooling down; retry-after=")
}

func (h *Handler) syncGrokQuota(acc *store.Account, headers http.Header) {
	if acc == nil || h == nil || h.lb == nil || h.lb.Store == nil {
		return
	}
	h.quota.Enqueue(acc, headers, h.persistGrokQuotaSync)
}

func (h *Handler) persistGrokQuotaSync(job grokbase.QuotaJob) {
	info := ratelimit.ParseHeaders(job.Headers)
	job.Persist(h.lb.Store, job.Account.GrokPlane(), info != nil, func(account *store.Account) bool {
		normalizeWebProvider(account)
		return ApplyQuotaInfo(account, info)
	})
}
