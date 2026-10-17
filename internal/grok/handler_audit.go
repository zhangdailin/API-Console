package grok

import (
	"context"
	"log/slog"
	"orchids-api/internal/audit"
	"orchids-api/internal/chatwire"
	"orchids-api/internal/middleware"
	"orchids-api/internal/pricing"
	"orchids-api/internal/responses"
	"orchids-api/internal/store"
	"time"
)

func (h *Handler) auditAttempt(ctx context.Context, acc *store.Account, provider string, attempt int, started time.Time, err error, stages ...string) {
	stage := "request"
	if len(stages) > 0 {
		stage = stages[0]
	}
	h.auditAttemptDiagnostic(ctx, acc, provider, attempt, started, err, stage, nil, nil, "")
}

func (h *Handler) auditChatOutcome(ctx context.Context, acc *store.Account, req *chatwire.Request, result chatOutcome) {
	logger := h.auditLoggerSnapshot()
	if logger == nil {
		return
	}
	status, message := result.Finish, ""
	if result.Err != nil {
		status = "error"
		message = result.Err.Error()
	}
	usage := result.Usage
	prompt, _ := usage["prompt_tokens_details"].(map[string]any)
	completion, _ := usage["completion_tokens_details"].(map[string]any)
	metadata := map[string]any{"finish_reason": result.Finish, "visible_output": result.SawText}
	metadata["inference_profile"] = h.configSnapshot().InferencePolicy().Name
	metadata["quality_delivery_mode"] = "quality"
	if cfg := h.configSnapshot(); cfg != nil && cfg.GrokLowLatency {
		metadata["quality_delivery_mode"] = "low_latency"
	}
	addQualityTiming(metadata, result)
	if result.QualityChecked {
		metadata["quality_checked_attempts"] = result.QualityAttempt
		metadata["quality_check_status"] = "passed"
		if result.Err != nil {
			metadata["quality_check_status"] = "error"
		} else if result.Withheld {
			metadata["quality_check_status"] = "degraded"
		}
	}
	if result.Withheld {
		// The attempt was held back by the quality hold. The row keeps the
		// upstream's own finish reason in the metadata and reports the attempt
		// itself as degraded, in both modes.
		status = "quality_degraded"
		metadata["quality_withheld"] = true
		metadata["quality_reason"] = result.WithheldReason
		// False for a turn the client never saw; true for the one that was
		// delivered anyway when the retry budget ran out.
		metadata["quality_delivered"] = result.DeliveredDegraded
	}
	addReasoningDiagnostics(ctx, metadata)
	duration := int64(0)
	if !req.StartedAt.IsZero() {
		duration = time.Since(req.StartedAt).Milliseconds()
		if !result.FirstToken.IsZero() {
			metadata["first_token_ms"] = result.FirstToken.Sub(req.StartedAt).Milliseconds()
		}
	}
	accountID := int64(0)
	provider := ""
	if acc != nil {
		accountID = acc.ID
		provider = ProviderBuild
	}
	usageSource := result.UsageSource
	if usageSource == "" {
		if result.Err != nil || len(usage) == 0 {
			usageSource = audit.UsageSourceNone
		} else {
			usageSource = audit.UsageSourceEstimated
		}
	}
	event := audit.Event{Kind: audit.KindRequest, RequestID: middleware.GetRequestID(ctx), Action: "grok_request", APIKeyID: middleware.APIKeyID(ctx),
		AccountID: accountID, Model: req.Model, Channel: "grok", Provider: provider, Status: status, Error: message, Duration: duration, Metadata: metadata,
		InputTokens: responses.InterfaceToInt(usage["prompt_tokens"]), OutputTokens: responses.InterfaceToInt(usage["completion_tokens"]), TotalTokens: responses.InterfaceToInt(usage["total_tokens"]), UsageSource: usageSource,
		CachedInputTokens: responses.InterfaceToInt(prompt["cached_tokens"]), ReasoningTokens: responses.InterfaceToInt(completion["reasoning_tokens"])}
	// Price the turn and book it against the client key's reservation. Only
	// upstream-reported usage is billed: an estimated count is this gateway's
	// own guess and must never turn into money owed.
	//
	// A withheld turn is not billed: the client did not receive it. Settlement
	// is first-call-wins per request, so billing it would also throw away the
	// cost of the answer that is eventually delivered. The one exception is the
	// withheld turn that is delivered after all, which is billed then.
	if !result.Withheld || result.DeliveredDegraded {
		if cost, _ := middleware.SettleAPIKeyBilling(ctx, nil, req.Model, usageSource,
			int64(event.InputTokens), int64(event.CachedInputTokens), int64(event.OutputTokens)); cost.Model != "" {
			event.CostInUSDTicks = cost.CostInUSDTicks
			event.PricingModel = cost.Model
			event.PricingVersion = pricing.Version
		}
	}
	logger.Log(ctx, event)
	if result.DeliveredDegraded {
		// The account's usage was counted when this turn was first journalled as
		// withheld; counting it again would double the account's token total.
		return
	}
	// Grok owns its native request handlers, so it bypasses the generic handler's
	// account-usage accumulator. Persist the same usage attached to the audit
	// event here; otherwise request_count advances through syncGrokQuota while
	// tokens_today and usage_total remain permanently zero.
	if acc != nil && acc.ID != 0 && event.TotalTokens > 0 && h != nil && h.lb != nil && h.lb.Store != nil {
		accountID := acc.ID
		if err := h.lb.Store.IncrementAccountStats(ctx, accountID, float64(event.TotalTokens), 0); err != nil {
			slog.Warn("grok token usage persistence failed", "account_id", accountID, "runtime_account_id", acc.ID, "tokens", event.TotalTokens, "error", err)
		}
	}
}
