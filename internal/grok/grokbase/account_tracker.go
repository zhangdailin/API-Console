// Package grokbase holds the account, catalog and diagnostics policies shared
// by the Grok Build and Web planes.
package grokbase

import (
	"context"
	"net/http"
	"orchids-api/internal/util"
	"strings"
	"time"

	"orchids-api/internal/accountpolicy"
	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/grok/ratelimit"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/store"
)

// AccountTracker tracks account connections and records account verdicts for the
// plane handlers. Its methods are safe on a nil receiver, so a Handler built
// without one simply tracks nothing.
type AccountTracker struct {
	LB *loadbalancer.LoadBalancer
}

// MarkAccountStatus classifies an error string and marks the account
// status in the store if it indicates a known HTTP error (401/403/404/429).
func (b *AccountTracker) MarkAccountStatus(ctx context.Context, acc *store.Account, err error) {
	if acc == nil || err == nil || b == nil || b.LB == nil {
		return
	}
	status := apperrors.ClassifyAccountStatus(err.Error())
	if status == "" {
		return
	}
	b.LB.MarkAccountStatus(ctx, acc, status)
}

// NewAccountTracker returns a tracker over lb (nil tracks nothing).
func NewAccountTracker(lb *loadbalancer.LoadBalancer) *AccountTracker {
	return &AccountTracker{LB: lb}
}

// AccountCapacityAvailable reports whether acc has a free connection slot on
// tracker. Without a tracker or a limit every account has capacity.
func AccountCapacityAvailable(tracker loadbalancer.ConnTracker, acc *store.Account) bool {
	limit := loadbalancer.EffectiveAccountConcurrencyLimit(acc)
	if acc == nil || limit <= 0 || tracker == nil {
		return true
	}
	return tracker.GetCount(acc.ID) < limit
}

// Reserve takes an individually releasable slot for the selected account.
func (b *AccountTracker) Reserve(ctx context.Context, tracker loadbalancer.ConnTracker, acc *store.Account) (func(), bool) {
	if acc == nil || acc.ID == 0 {
		return nil, true
	}
	limit := loadbalancer.EffectiveAccountConcurrencyLimit(acc)
	if tracker == nil && b != nil && b.LB != nil {
		return b.LB.ReserveConnection(ctx, acc.ID, limit)
	}
	return loadbalancer.ReserveConnection(ctx, tracker, acc.ID, limit)
}

// RouteAllowsAccount respects an observed route's account list. Missing
// catalog data cannot exclude an otherwise eligible account.
func RouteAllowsAccount(ctx context.Context, s *store.Store, model string, accountID int64) bool {
	if s == nil || accountID == 0 {
		return true
	}
	row, err := s.GetModelByChannelAndModelID(ctx, "grok", model)
	return err != nil || row == nil || row.AllowsAccount(accountID)
}

func AccountUsableForModel(acc *store.Account, model string) bool {
	if acc == nil {
		return false
	}
	now := time.Now()
	if accountpolicy.IsGrokAccount(acc) && now.Before(acc.GrokFreeQuota.ResetAt) {
		return false
	}
	return model == "" || store.ModelCooldownRemaining(acc, model, now) == 0
}

// RequestErrorOnly distinguishes a caller's bad request from an account verdict.
func RequestErrorOnly(status int) bool {
	return status >= 400 && status < 500 && status != 401 && status != 402 && status != 403 && status != 429
}

// ApplyAccountHold updates the throttle deadline. It returns true when a
// refusal was handled for just one model and no account-wide write is needed.
func ApplyAccountHold(ctx context.Context, s *store.Store, acc *store.Account, status int, message, model string, identity func() string) bool {
	if acc == nil {
		return false
	}
	lower := strings.ToLower(message)
	if strings.Contains(lower, "resource-exhausted") || strings.Contains(lower, "resource_exhausted") || strings.Contains(lower, "too many requests for team") {
		cooldown := accountpolicy.CooldownRateLimitBase
		if meta := ratelimit.ParseMetadata([]byte(message)); meta != nil {
			remaining := ratelimit.Cooldowns.RetryAfterFor(meta.Scope, identity(), meta.Model)
			if remaining > 0 {
				cooldown = accountpolicy.BoundRateLimitCooldown(remaining)
			} else if meta.RetryAfter > 0 {
				cooldown = accountpolicy.BoundRateLimitCooldown(meta.RetryAfter)
			}
		}
		acc.QuotaResetAt = time.Now().Add(max(cooldown, accountpolicy.RateLimitCooldown(acc.RateLimitFailures+1)))
	}
	if status >= 500 {
		acc.QuotaResetAt = time.Now().Add(5 * time.Second)
	}
	if model == "" || !IsModelScopedRefusal(status, lower) {
		return false
	}
	if s != nil {
		_ = s.SetModelCooldown(ctx, acc, model, time.Now().Add(5*time.Minute), store.ModelCooldownThrottled)
	}
	return true
}

func IsModelScopedRefusal(status int, message string) bool {
	if status != http.StatusForbidden {
		return false
	}
	lower := strings.ToLower(message)
	for _, marker := range []string{
		"access to the chat endpoint is denied", "for model", "model is not available",
		"not available for model", "model_not_available",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// TransientAccountFailure covers retryable failures after each plane has
// applied its own typed refusal and challenge rules.
func TransientAccountFailure(status int, message string, sharedLimit bool) bool {
	switch apperrors.ClassifyAccountStatus(message) {
	case "401", "429":
		return true
	}
	if sharedLimit || apperrors.IsCreditExhaustion(message) {
		return true
	}
	switch status {
	case http.StatusPaymentRequired, http.StatusBadGateway, http.StatusServiceUnavailable,
		http.StatusGatewayTimeout, http.StatusInternalServerError:
		return true
	}
	lower := strings.ToLower(message)
	for _, marker := range []string{"timeout", "deadline exceeded", "connection reset", "connection refused", "broken pipe"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return strings.HasSuffix(lower, ": eof") || lower == "eof"
}

// MapAccounts runs bounded admin work and reports an error at the same position
// when a request is canceled or a worker fails before returning a result.
func MapAccounts[T any](ctx context.Context, accounts []*store.Account, workers int, work func(*store.Account) T, failed func(*store.Account, error) T) []T {
	results := make([]T, len(accounts))
	errs := util.RunIndexed(len(accounts), workers, func(i int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		results[i] = work(accounts[i])
		return nil
	})
	for i, err := range errs {
		if err != nil {
			results[i] = failed(accounts[i], err)
		}
	}
	return results
}

func WriteAccountBatch[T any](w http.ResponseWriter, results []T, success func(T) bool) {
	ok := 0
	for _, result := range results {
		if success(result) {
			ok++
		}
	}
	util.WriteJSON(w, map[string]any{"status": "success", "total": len(results), "ok": ok, "failed": len(results) - ok, "results": results})
}
