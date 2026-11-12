package grok

import (
	"context"
	"log/slog"
	"orchids-api/internal/audit"
	"orchids-api/internal/chatwire"
	"orchids-api/internal/grok/grokbase"
	"orchids-api/internal/middleware"
	"orchids-api/internal/pricing"
	"orchids-api/internal/responses"
	"orchids-api/internal/store"
	"orchids-api/internal/upstream"
	"time"
)

// grokFinalizeWriteTimeout bounds the account writes that finish a request:
// they run after the client may already have hung up.
const grokFinalizeWriteTimeout = 5 * time.Second

// detachedGrokWrite keeps request values (request ID, tracing) but not its
// cancellation, so a client that disconnects as its stream ends cannot drop
// the account state that request produced.
func detachedGrokWrite(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), grokFinalizeWriteTimeout)
}

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
	grokbase.AddReasoningDiagnostics(ctx, metadata)
	upstream.NoteServedModel(ctx, result.ServedModel)
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
	if cost, _ := middleware.SettleAPIKeyBilling(ctx, nil, req.Model, usageSource,
		int64(event.InputTokens), int64(event.CachedInputTokens), int64(event.OutputTokens)); cost.Model != "" {
		event.CostInUSDTicks = cost.CostInUSDTicks
		event.PricingModel = cost.Model
		event.PricingVersion = pricing.Version
	}
	logger.Log(ctx, event)
	// Grok owns its native request handlers, so it bypasses the generic handler's
	// account-usage accumulator. Persist the same usage attached to the audit
	// event here; otherwise request_count advances through syncGrokQuota while
	// tokens_today and usage_total remain permanently zero.
	if acc != nil && acc.ID != 0 && event.TotalTokens > 0 && h != nil && h.lb != nil && h.lb.Store != nil {
		accountID := acc.ID
		// The client may hang up the moment its stream ends; the usage it
		// consumed must still be booked, so the write outlives the request.
		persistCtx, cancel := detachedGrokWrite(ctx)
		defer cancel()
		if err := h.lb.Store.IncrementAccountStats(persistCtx, accountID, float64(event.TotalTokens), 0); err != nil {
			slog.Warn("grok token usage persistence failed", "account_id", accountID, "runtime_account_id", acc.ID, "tokens", event.TotalTokens, "error", err)
		}
	}
}
