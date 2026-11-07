package grok

import (
	"context"
	"log/slog"
	"net/http"
	"orchids-api/internal/accountpolicy"
	"orchids-api/internal/grok/grokbase"
	"orchids-api/internal/grok/ratelimit"
	"orchids-api/internal/store"
	"time"
)

func (h *Handler) syncGrokQuota(acc *store.Account, headers http.Header) {
	if acc == nil || h == nil || h.lb == nil || h.lb.Store == nil {
		return
	}
	h.quota.Enqueue(acc, headers, h.persistGrokQuotaSync)
}

func (h *Handler) persistGrokQuotaSync(job grokbase.QuotaJob) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.lb.Store.IncrementAccountStats(ctx, job.Account.ID, 0, job.Requests); err != nil {
		slog.Warn("grok usage touch failed", "account_id", job.Account.ID, "error", err)
	}

	headers := job.Headers
	info := ratelimit.ParseHeaders(headers)
	requests := parseBuildRateLimitWindow(headers, "requests")
	tokens := parseBuildRateLimitWindow(headers, "tokens")
	hasBuildHeaders := requests.HasLimit || requests.HasRemaining || !requests.ResetAt.IsZero() || tokens.HasLimit || tokens.HasRemaining || !tokens.ResetAt.IsZero()
	if info == nil && !hasBuildHeaders {
		if _, err := h.lb.Store.ConsumeGrokQuota(ctx, job.Account.ID, ProviderBuild, float64(job.Requests)); err != nil {
			slog.Warn("grok local quota decrement failed", "account_id", job.Account.ID, "error", err)
		}
		return
	}
	latest, err := h.lb.Store.GetAccount(ctx, job.Account.ID)
	if err != nil || latest == nil {
		slog.Warn("grok quota account reload failed", "account_id", job.Account.ID, "error", err)
		return
	}
	NormalizeProvider(latest)
	changed := false
	if accountpolicy.IsGrokAccount(latest) {
		changed = ApplyBuildRateLimits(latest, headers)
	} else {
		changed = info != nil && ApplyQuotaInfo(latest, info)
	}
	if changed {
		if err := h.lb.Store.UpdateAccount(ctx, latest); err != nil {
			slog.Warn("grok quota update failed", "account_id", latest.ID, "error", err)
		}
	}
}
