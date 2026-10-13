package grok

import (
	"context"
	"log/slog"
	"net/http"
	"orchids-api/internal/store"
	"time"
)

type grokQuotaSyncJob struct {
	account  store.Account
	headers  http.Header
	requests int64
}

func (h *Handler) syncGrokQuota(acc *store.Account, headers http.Header) {
	if acc == nil || h == nil || h.lb == nil || h.lb.Store == nil {
		return
	}
	h.quotaOnce.Do(func() {
		h.quotaPending = make(map[int64]grokQuotaSyncJob)
		h.quotaWake = make(chan struct{}, 1)
		go h.runGrokQuotaSync()
	})
	h.quotaMu.Lock()
	job := h.quotaPending[acc.ID]
	job.account = *acc
	job.headers = headers.Clone()
	job.requests++
	h.quotaPending[acc.ID] = job
	h.quotaMu.Unlock()
	select {
	case h.quotaWake <- struct{}{}:
	default:
	}
}

func (h *Handler) runGrokQuotaSync() {
	for range h.quotaWake {
		for {
			h.quotaMu.Lock()
			var accountID int64
			var job grokQuotaSyncJob
			for id, pending := range h.quotaPending {
				accountID, job = id, pending
				delete(h.quotaPending, id)
				break
			}
			h.quotaMu.Unlock()
			if accountID == 0 {
				break
			}
			h.persistGrokQuotaSync(job)
		}
	}
}

func (h *Handler) persistGrokQuotaSync(job grokQuotaSyncJob) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.lb.Store.IncrementAccountStats(ctx, job.account.ID, 0, job.requests); err != nil {
		slog.Warn("grok usage touch failed", "account_id", job.account.ID, "error", err)
	}

	headers := job.headers
	info := parseRateLimitInfo(headers)
	requests := parseBuildRateLimitWindow(headers, "requests")
	tokens := parseBuildRateLimitWindow(headers, "tokens")
	hasBuildHeaders := requests.HasLimit || requests.HasRemaining || !requests.ResetAt.IsZero() || tokens.HasLimit || tokens.HasRemaining || !tokens.ResetAt.IsZero()
	if info == nil && !hasBuildHeaders {
		provider := ProviderForAccount(&job.account)
		if _, err := h.lb.Store.ConsumeGrokQuota(ctx, job.account.ID, provider, float64(job.requests)); err != nil {
			slog.Warn("grok local quota decrement failed", "account_id", job.account.ID, "error", err)
		}
		return
	}
	latest, err := h.lb.Store.GetAccount(ctx, job.account.ID)
	if err != nil || latest == nil {
		slog.Warn("grok quota account reload failed", "account_id", job.account.ID, "error", err)
		return
	}
	NormalizeProvider(latest)
	provider := ProviderForAccount(latest)
	changed := false
	if provider == ProviderBuild {
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
