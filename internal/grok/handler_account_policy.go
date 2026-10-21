package grok

import (
	"context"
	"errors"
	"net/http"
	"orchids-api/internal/accountpolicy"
	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/store"
	"strings"
	"time"
)

func (h *Handler) accountCapacityAvailable(acc *store.Account) bool {
	tracker := h.connTrackerSnapshot()
	limit := loadbalancer.EffectiveAccountConcurrencyLimit(acc)
	if acc == nil || limit <= 0 || tracker == nil {
		return true
	}
	return tracker.GetCount(acc.ID) < limit
}

func (h *Handler) reserveAccount(acc *store.Account) (func(), bool) {
	tracker := h.connTrackerSnapshot()
	if tracker == nil || acc == nil || acc.ID == 0 {
		if h != nil && h.base != nil {
			return h.base.TrackAccount(acc), true
		}
		return func() {}, true
	}
	if limiter, ok := tracker.(loadbalancer.LimitedConnTracker); ok {
		if !limiter.TryAcquire(acc.ID, loadbalancer.EffectiveAccountConcurrencyLimit(acc)) {
			return func() {}, false
		}
	} else {
		if !h.accountCapacityAvailable(acc) {
			return func() {}, false
		}
		tracker.Acquire(acc.ID)
	}
	return func() { tracker.Release(acc.ID) }, true
}

func (h *Handler) markAccountStatus(ctx context.Context, acc *store.Account, err error) {
	// Invalid parameters and missing resources are request errors, not evidence
	// that the credential is unusable. Do not poison account routing with them.
	if status := upstreamStatus(err); status >= 400 && status < 500 && status != 401 && status != 402 && status != 403 && status != 429 {
		return
	}
	var oauthErr *cliOAuthError
	if errors.As(err, &oauthErr) && oauthErr.status == http.StatusUnauthorized && acc != nil {
		acc.OAuthAccessToken = ""
		acc.OAuthRefreshToken = ""
		acc.OAuthExpiresAt = time.Time{}
	}
	// Team-level resource-exhausted 429: the rate limit is on the token/session,
	// not the account. Set a cooldown so the RPM window can reset. Without this,
	// unmarked sibling accounts sharing the same token immediately hit the same
	// team limit. Prefer the precise reset parsed from the 429 body (team+model
	// granularity), falling back to a 60s blanket cooldown.
	if err != nil && isResourceExhaustedError(err) && acc != nil {
		cooldown := accountpolicy.CooldownRateLimitBase
		if meta := ParseRateLimitMetadata([]byte(err.Error())); meta != nil {
			identity := ProviderBuild + ":" + rateLimitIdentity(withRateLimitAccount(ctx, acc), "")
			if remaining := teamCooldown.RetryAfterFor(meta.Scope, identity, meta.Model); remaining > 0 {
				cooldown = accountpolicy.BoundRateLimitCooldown(remaining)
			} else if meta.RetryAfter > 0 {
				cooldown = accountpolicy.BoundRateLimitCooldown(meta.RetryAfter)
			}
		}
		failureCooldown := accountpolicy.RateLimitCooldown(acc.RateLimitFailures + 1)
		if failureCooldown > cooldown {
			cooldown = failureCooldown
		}
		acc.QuotaResetAt = time.Now().Add(cooldown)
	}
	// A refusal that names one model ("access to the chat endpoint is denied",
	// "not available for model X") is a capability problem for that model only.
	// Cooling the whole credential took every other model out of the pool for
	// ten minutes; the model cooldown map exists for exactly this case
	// (the cooldown marks the model, not the account).
	// A 5xx is the upstream's own trouble, not the credential's: a short hold
	// keeps the next request from immediately re-selecting the same account
	// while the upstream recovers, without marking the credential as broken.
	if acc != nil {
		if status := upstreamStatus(err); status >= 500 {
			acc.QuotaResetAt = time.Now().Add(serverFaultHold)
		}
	}
	if acc != nil && isModelScopedRefusal(err) {
		if model := requestModelFromContext(ctx); model != "" {
			store.RecordModelCooldown(acc, model, time.Now().Add(modelScopedRefusalCooldown))
			if h.lb != nil && h.lb.Store != nil {
				_ = h.lb.Store.UpdateAccount(ctx, acc)
			}
			h.unbindAffinity(ctx, ProviderBuild, acc.ID)
			return
		}
	}
	// The credential cannot serve this session any more: drop the affinity so the
	// next turn picks a different account instead of coming back here.
	h.unbindAffinity(ctx, ProviderBuild, acc.ID)
	h.base.MarkAccountStatus(ctx, acc, err)
}

// modelScopedRefusalCooldown is how long one model stays out of rotation after
// the upstream refused it for this credential.
const modelScopedRefusalCooldown = 5 * time.Minute

// serverFaultHold is the short pause applied after an upstream 5xx.
const serverFaultHold = 5 * time.Second

// isModelScopedRefusal reports whether an upstream failure refused one model
// rather than the credential itself.
func isModelScopedRefusal(err error) bool {
	if err == nil || upstreamStatus(err) != 403 {
		return false
	}
	lower := strings.ToLower(err.Error())
	for _, marker := range []string{
		"access to the chat endpoint is denied",
		"for model",
		"model is not available",
		"not available for model",
		"model_not_available",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func isResourceExhaustedError(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "resource-exhausted") ||
		strings.Contains(lower, "resource_exhausted") ||
		strings.Contains(lower, "too many requests for team")
}

func (h *Handler) routeAllowsAccount(ctx context.Context, modelID string, accountID int64) bool {
	if h == nil || h.lb == nil || h.lb.Store == nil || accountID == 0 {
		return true
	}
	model, err := h.lb.Store.GetModelByChannelAndModelID(ctx, "grok", normalizeModelID(modelID))
	if err != nil || model == nil {
		return true
	}
	return model.AllowsAccount(accountID)
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
	if acc == nil {
		return false
	}
	// A Build Free refusal is authoritative for the whole included-usage
	// window. Do not send the account back to the upstream every minute while
	// its confirmed 24-hour reset is still in the future.
	if accountpolicy.IsGrokAccount(acc) && !acc.GrokFreeQuota.ResetAt.IsZero() && time.Now().Before(acc.GrokFreeQuota.ResetAt) {
		return false
	}
	// A credential parked by the quality guard stays out of rotation until its
	// cooldown ends: it answers 200 with degraded content, which no status code
	// reflects.
	if !acc.QualityCooldownUntil.IsZero() && time.Now().Before(acc.QualityCooldownUntil) {
		return false
	}
	model := requestModelFromContext(ctx)
	if model == "" {
		return true
	}
	return store.ModelCooldownRemaining(acc, model, time.Now()) == 0
}

func (s *chatAccountSession) Close() {
	if s == nil || s.release == nil {
		return
	}
	s.release()
	s.release = nil
}

func markAllGrokAccountStatuses(err error) bool {
	if err == nil {
		return false
	}
	// Shared team rate limits and preflight cooldowns are not account failures.
	if isSharedGrokRateLimitError(err) {
		return false
	}
	var oauthErr *cliOAuthError
	if errors.As(err, &oauthErr) && oauthErr.status == http.StatusUnauthorized {
		return true
	}
	// A generic 403 must not mark the account; only explicit account blocks do.
	return !(ClassifyUpstreamError(err) == UpstreamErrorGenericForbidden)
}

func shouldSwitchGrokAccount(err error) bool {
	if err == nil {
		return false
	}
	var oauthErr *cliOAuthError
	if errors.As(err, &oauthErr) && oauthErr.status == http.StatusUnauthorized {
		return true
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
	status := apperrors.ClassifyAccountStatus(err.Error())
	if status == "401" || status == "429" || isSharedGrokRateLimitError(err) {
		return true
	}
	// A spent account ("personal-team-blocked:spending-limit", 402 "You have
	// run out of credits") is this account's problem; another account can
	// serve the request. Not switching answered the caller with the refusal
	// while healthy accounts sat idle in the pool.
	if upstreamStatus(err) == http.StatusPaymentRequired || apperrors.IsCreditExhaustion(err.Error()) {
		return true
	}
	if status := upstreamStatus(err); status == http.StatusBadGateway ||
		status == http.StatusServiceUnavailable ||
		status == http.StatusGatewayTimeout ||
		status == http.StatusInternalServerError {
		return true
	}

	lower := strings.ToLower(err.Error())
	switch {
	case strings.Contains(lower, "timeout"),
		strings.Contains(lower, "deadline exceeded"),
		strings.Contains(lower, "connection reset"),
		strings.Contains(lower, "connection refused"),
		strings.Contains(lower, "broken pipe"),
		strings.HasSuffix(lower, ": eof"),
		lower == "eof":
		return true
	default:
		return false
	}
}

func isSharedGrokRateLimitError(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := errors.AsType[*syntheticCooldownError](err); ok {
		return true
	}
	lower := strings.ToLower(err.Error())
	if upstreamStatus(err) != http.StatusTooManyRequests && !strings.Contains(lower, "too many requests") {
		return false
	}
	// Only a parsed Team+Model response is shared. A bare 429/"too many
	// requests" is account-scoped and must cool the selected account;
	// treating it as shared left every account looking healthy while requests
	// repeatedly rotated through a pool of throttled credentials.
	if ParseRateLimitMetadata([]byte(err.Error())) != nil {
		return true
	}
	return false
}

func upstreamHTTPResponseStatus(err error) int {
	if status := upstreamStatus(err); status >= 400 && status <= 599 {
		return status
	}
	return http.StatusBadGateway
}
