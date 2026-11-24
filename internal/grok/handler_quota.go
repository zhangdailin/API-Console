package grok

import (
	"net/http"
	"orchids-api/internal/accountpolicy"
	"orchids-api/internal/grok/grokbase"
	"orchids-api/internal/grok/ratelimit"
	"orchids-api/internal/grok/web"
	"orchids-api/internal/store"
)

func (h *Handler) syncGrokQuota(acc *store.Account, headers http.Header) {
	if acc == nil || h == nil || h.lb == nil || h.lb.Store == nil {
		return
	}
	h.quota.Enqueue(acc, headers, h.persistGrokQuotaSync)
}

func (h *Handler) persistGrokQuotaSync(job grokbase.QuotaJob) {
	headers := job.Headers
	info := ratelimit.ParseHeaders(headers)
	requests := parseBuildRateLimitWindow(headers, "requests")
	tokens := parseBuildRateLimitWindow(headers, "tokens")
	hasBuildHeaders := requests.HasLimit || requests.HasRemaining || !requests.ResetAt.IsZero() || tokens.HasLimit || tokens.HasRemaining || !tokens.ResetAt.IsZero()
	job.Persist(h.lb.Store, ProviderBuild, info != nil || hasBuildHeaders, func(account *store.Account) bool {
		normalizeBuildProvider(account)
		if accountpolicy.IsGrokAccount(account) {
			return ApplyBuildRateLimits(account, headers)
		}
		return info != nil && web.ApplyQuotaInfo(account, info)
	})
}
