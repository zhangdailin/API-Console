package grok

// What the quality checks leave behind for an operator.
//
// The hold (quality_hold.go) decides whether a turn may be delivered and the
// guard (quality_guard.go) decides whether the credential keeps serving. Each
// conclusion is written twice: to the server log, and to the journal as a row
// whose action starts with "grok_quality_". The rows carry counts, timings and
// identifiers only — never response text and never a credential.

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"orchids-api/internal/audit"
	"orchids-api/internal/chatwire"
	"orchids-api/internal/middleware"
	"orchids-api/internal/responses"
	"orchids-api/internal/store"
)

// qualityWithheldTurn is what the gateway knows about one withheld attempt
// beyond the stream's own accounting.
type qualityWithheldTurn struct {
	// Mode is "stream" or "collected".
	Mode string
	// Attempt is the 1-based position of this attempt among the quality
	// attempts of the request.
	Attempt int
	// Guard is what the credential guard did about the same turn.
	Guard qualityGuardAction
}

// One start and one verdict per attempt, never one event per streamed chunk.
func (h *Handler) auditQualityCheck(ctx context.Context, acc *store.Account, req *chatwire.Request, status, mode string, attempt int, started time.Time, outcome chatOutcome) {
	middleware.AnnotateRequest(ctx, "quality_checked_attempts", attempt)
	middleware.AnnotateRequest(ctx, "quality_check_status", status)
	logger := h.auditLoggerSnapshot()
	if logger == nil {
		return
	}
	policy := h.qualityHoldPolicy()
	accountID := int64(0)
	if acc != nil {
		accountID = acc.ID
	}
	metadata := map[string]any{
		"mode": mode, "max_attempts": policy.MaxAttempts,
		"hold_timeout_ms": policy.HoldTimeout.Milliseconds(), "on_exhausted": qualityRetryFailOpen,
		"visible_chars": outcome.Quality.VisibleChars, "reasoning_chars": outcome.Quality.ReasoningChars,
		"encrypted_chars": outcome.Quality.EncryptedChars, "first_visible_ms": outcome.Quality.FirstVisibleMS,
		"visible_duration_ms": outcome.Quality.visibleDurationMS(),
	}
	addQualityTiming(metadata, outcome)
	if mode == "native" {
		metadata["on_exhausted"] = "fail_closed"
	}
	if outcome.Quality.VisibleChars <= 0 {
		metadata["first_visible_ms"] = int64(-1)
	}
	if status == "error" {
		metadata["reason"] = "upstream_error"
	}
	duration := int64(0)
	if status != "checking" {
		duration = time.Since(started).Milliseconds()
	}
	logger.Log(ctx, audit.Event{
		Kind: audit.KindRequest, RequestID: middleware.GetRequestID(ctx), Action: "grok_quality_check",
		APIKeyID: middleware.APIKeyID(ctx), AccountID: accountID, Model: req.Model, Channel: "grok",
		Provider: ProviderBuild, Attempt: attempt, Status: status, Duration: duration,
		ReasoningTokens: int(outcome.Quality.ReasoningTokens), Metadata: metadata,
	})
}

func (h *Handler) auditQualityRetry(ctx context.Context, previous, next *store.Account, req *chatwire.Request, attempt int) {
	logger := h.auditLoggerSnapshot()
	if logger == nil {
		return
	}
	logger.Log(ctx, audit.Event{
		Kind: audit.KindRequest, RequestID: middleware.GetRequestID(ctx), Action: "grok_quality_retry",
		APIKeyID: middleware.APIKeyID(ctx), AccountID: next.ID, Model: req.Model, Channel: "grok",
		Provider: ProviderBuild, Attempt: attempt, Status: "retrying",
		Metadata: map[string]any{"from_account_id": previous.ID, "to_account_id": next.ID},
	})
}

// auditQualityDegraded records a withheld turn, in the server log and as a
// request-scoped journal row.
//
// The credential penalty is recorded by the guard, but an operator asking "why
// did this request take two accounts" needs a row that belongs to the request
// and says which detector refused the turn. A healthy turn never writes one, so
// the journal stays quiet in normal operation.
//
// Nothing here is response content: the row carries counts and timings only.
func (h *Handler) auditQualityDegraded(ctx context.Context, acc *store.Account, req *chatwire.Request, outcome chatOutcome, turn qualityWithheldTurn) {
	middleware.AnnotateRequest(ctx, "quality_check_status", "degraded")
	accountID := int64(0)
	if acc != nil {
		accountID = acc.ID
	}
	model := ""
	if req != nil {
		model = req.Model
	}
	requestID := middleware.GetRequestID(ctx)
	slog.Warn("grok quality hold: turn withheld",
		"request_id", requestID, "account_id", accountID, "model", model,
		"mode", turn.Mode, "attempt", turn.Attempt, "reason", outcome.WithheldReason, "guard", string(turn.Guard),
		"visible_chars", outcome.Quality.VisibleChars, "reasoning_chars", outcome.Quality.ReasoningChars,
		"encrypted_chars", outcome.Quality.EncryptedChars, "reasoning_tokens", outcome.Quality.ReasoningTokens,
		"first_visible_ms", outcome.Quality.FirstVisibleMS, "visible_duration_ms", outcome.Quality.visibleDurationMS())

	logger := h.auditLoggerSnapshot()
	if logger == nil {
		return
	}
	usageSource := outcome.UsageSource
	if usageSource == "" {
		usageSource = audit.UsageSourceNone
	}
	logger.Log(ctx, audit.Event{
		Kind: audit.KindRequest, RequestID: requestID, Action: "grok_quality_degraded",
		APIKeyID: middleware.APIKeyID(ctx), AccountID: accountID, Model: model, Channel: "grok",
		Provider: ProviderBuild, Attempt: turn.Attempt, Status: "degraded", UsageSource: usageSource,
		Details:         outcome.WithheldReason,
		InputTokens:     responses.InterfaceToInt(outcome.Usage["prompt_tokens"]),
		OutputTokens:    responses.InterfaceToInt(outcome.Usage["completion_tokens"]),
		ReasoningTokens: int(outcome.Quality.ReasoningTokens),
		Metadata: map[string]any{
			"mode":                turn.Mode,
			"reason":              outcome.WithheldReason,
			"guard":               string(turn.Guard),
			"visible_chars":       outcome.Quality.VisibleChars,
			"reasoning_chars":     outcome.Quality.ReasoningChars,
			"encrypted_chars":     outcome.Quality.EncryptedChars,
			"first_visible_ms":    outcome.Quality.FirstVisibleMS,
			"visible_duration_ms": outcome.Quality.visibleDurationMS(),
			"tool_calls":          outcome.Quality.ToolCalls,
			"withheld":            true,
		},
	})
}

// How a request that had a turn withheld ended.
const (
	// qualityResolutionRetried: a later attempt was not withheld. Its own
	// grok_request row says how that attempt went.
	qualityResolutionRetried = "retried"
	// qualityResolutionDeliveredDegraded: the last withheld turn was delivered
	// anyway, because the policy fails open or no other account could serve it.
	qualityResolutionDeliveredDegraded = "delivered_degraded"
	// qualityResolutionUndeliverable: nothing was held to deliver; 502.
	qualityResolutionUndeliverable = "undeliverable"
)

// Why the gateway stopped retrying a withheld turn.
const (
	qualityStopAttemptsExhausted = "attempts_exhausted"
	qualityStopAccountBudget     = "account_budget_exhausted"
	qualityStopReplayUnsafe      = "replay_unsafe"
	qualityStopClientGone        = "client_gone"
	qualityStopNoOtherAccount    = "no_other_account"
)

// auditQualityResolution records how a request that had at least one turn
// withheld finally ended. It is the row that answers "did the client receive a
// degraded answer", which the per-attempt rows cannot: they describe turns the
// client never saw.
//
// The same conclusion is attached to the request's own journal row, which is
// the row an operator opens: reasons lists the detector behind each withheld
// attempt, in order.
func (h *Handler) auditQualityResolution(ctx context.Context, acc *store.Account, req *chatwire.Request, resolution, stopped string, reasons []string, policy qualityHoldPolicy) {
	withheld := len(reasons)
	middleware.AnnotateRequest(ctx, "quality_resolution", resolution)
	middleware.AnnotateRequest(ctx, "quality_withheld_attempts", withheld)
	middleware.AnnotateRequest(ctx, "quality_reasons", append([]string(nil), reasons...))
	middleware.AnnotateRequest(ctx, "quality_client_received_degraded", resolution == qualityResolutionDeliveredDegraded)
	if stopped != "" {
		middleware.AnnotateRequest(ctx, "quality_stopped", stopped)
	}

	accountID := int64(0)
	if acc != nil {
		accountID = acc.ID
	}
	model := ""
	if req != nil {
		model = req.Model
	}
	requestID := middleware.GetRequestID(ctx)
	level := slog.LevelWarn
	if resolution == qualityResolutionRetried {
		// The hold did its job and the client was not affected.
		level = slog.LevelInfo
	}
	slog.Log(ctx, level, "grok quality hold: request resolved",
		"request_id", requestID, "account_id", accountID, "model", model,
		"resolution", resolution, "stopped", stopped, "withheld_attempts", withheld,
		"max_attempts", policy.MaxAttempts, "on_exhausted", qualityRetryFailOpen)

	logger := h.auditLoggerSnapshot()
	if logger == nil {
		return
	}
	metadata := map[string]any{
		"withheld_attempts": withheld,
		"reasons":           reasons,
		"max_attempts":      policy.MaxAttempts,
		"on_exhausted":      qualityRetryFailOpen,
		// delivered_degraded is the one outcome where the client received an
		// answer the classifier refused.
		"client_received_degraded": resolution == qualityResolutionDeliveredDegraded,
	}
	if stopped != "" {
		metadata["stopped"] = stopped
	}
	logger.Log(ctx, audit.Event{
		Kind: audit.KindRequest, RequestID: requestID, Action: "grok_quality_resolution",
		APIKeyID: middleware.APIKeyID(ctx), AccountID: accountID, Model: model, Channel: "grok",
		Provider: ProviderBuild, Attempt: withheld, Status: resolution, Details: stopped,
		Metadata: metadata,
	})
}

// auditQualityGuard journals a change the guard made to a credential.
func (h *Handler) auditQualityGuard(ctx context.Context, acc *store.Account, req *chatwire.Request, outcome chatOutcome, action qualityGuardAction, previousFailures int) {
	logger := h.auditLoggerSnapshot()
	if logger == nil || acc == nil {
		return
	}
	model := ""
	if req != nil {
		model = req.Model
	}
	metadata := map[string]any{
		"failures":            acc.QualityFailures,
		"previous_failures":   previousFailures,
		"disable_threshold":   qualityDisableThreshold,
		"account_enabled":     acc.Enabled,
		"visible_chars":       outcome.Quality.VisibleChars,
		"reasoning_chars":     outcome.Quality.ReasoningChars,
		"encrypted_chars":     outcome.Quality.EncryptedChars,
		"reasoning_tokens":    outcome.Quality.ReasoningTokens,
		"first_visible_ms":    outcome.Quality.FirstVisibleMS,
		"visible_duration_ms": outcome.Quality.visibleDurationMS(),
		// A turn can be penalised without being withheld (it was already
		// streamed, or the hold is off) and withheld without being penalised.
		"withheld": outcome.Withheld,
	}
	if !acc.QualityCooldownUntil.IsZero() {
		metadata["cooldown_until"] = acc.QualityCooldownUntil.Format(time.RFC3339)
	}
	logger.Log(ctx, audit.Event{
		Kind: audit.KindSystem, RequestID: middleware.GetRequestID(ctx), Action: "grok_quality_guard",
		AccountID: acc.ID, Model: model, Channel: "grok", Provider: ProviderBuild,
		Target: "account:" + strconv.FormatInt(acc.ID, 10), Status: string(action),
		Details: acc.StatusMessage, Metadata: metadata,
	})
}
