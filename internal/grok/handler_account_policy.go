package grok

import (
	"context"
	"errors"
	"net/http"
	"orchids-api/internal/grok/grokbase"
	"orchids-api/internal/grok/ratelimit"
	"orchids-api/internal/store"
	"strings"
	"time"
)

func (h *Handler) accountCapacityAvailable(acc *store.Account) bool {
	return grokbase.AccountCapacityAvailable(h.connTrackerSnapshot(), acc)
}

func (h *Handler) reserveAccount(ctx context.Context, acc *store.Account) (func(), bool) {
	var base *grokbase.AccountTracker
	if h != nil {
		base = h.base
	}
	return base.Reserve(ctx, h.connTrackerSnapshot(), acc)
}

func (h *Handler) markAccountStatus(ctx context.Context, acc *store.Account, err error) {
	// A content refusal or a client-version demand says nothing about the credential.
	if _, requestFailure := grokRequestFailure(err); requestFailure {
		return
	}
	// Invalid parameters and missing resources are request errors, not evidence
	// that the credential is unusable. Do not poison account routing with them.
	status := upstreamStatus(err)
	if grokbase.RequestErrorOnly(status) {
		return
	}
	// Only a refused refresh grant retires the stored credential. A 401 from a
	// misconfigured client or an unexplained refusal used to wipe the tokens
	// too, which turned one bad configuration into a re-login for every account.
	if IsCLIPermanentOAuthError(err) && acc != nil {
		acc.OAuthAccessToken = ""
		acc.OAuthRefreshToken = ""
		acc.OAuthExpiresAt = time.Time{}
	}
	if acc == nil {
		return
	}
	var message string
	if err != nil {
		message = err.Error()
	}
	provider := ProviderBuild
	modelOnly := grokbase.ApplyAccountHold(ctx, h.affinityStore(), acc, status, message, requestModelFromContext(ctx), func() string {
		return provider + ":" + rateLimitIdentity(withRateLimitAccount(ctx, acc), "")
	})
	h.unbindAffinity(ctx, provider, acc.ID)
	if !modelOnly {
		h.base.MarkAccountStatus(ctx, acc, err)
	}
}

func (h *Handler) routeAllowsAccount(ctx context.Context, modelID string, accountID int64) bool {
	return grokbase.RouteAllowsAccount(ctx, h.affinityStore(), normalizeModelID(modelID), accountID)
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
	if _, requestFailure := grokRequestFailure(err); requestFailure {
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
	case UpstreamErrorGenericForbidden, UpstreamErrorContentRefusal, UpstreamErrorUpgradeRequired:
		return false
	}
	return grokbase.TransientAccountFailure(upstreamStatus(err), err.Error(), isSharedGrokRateLimitError(err))
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
	if ratelimit.ParseMetadata([]byte(err.Error())) != nil {
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
