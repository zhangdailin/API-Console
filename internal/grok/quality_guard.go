package grok

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"orchids-api/internal/chatwire"
	"orchids-api/internal/middleware"
	"orchids-api/internal/store"
)

// Quality guard.
//
// A degraded upstream answers 200 with a response that looks fine but carries no
// reasoning: the visible text arrives as one late dump, or the reasoning is a
// cipher-only stub with zero reasoning tokens while the answer streams out.
// Those responses are indistinguishable from a healthy one by status alone, so
// the account keeps serving them and every caller keeps receiving them.
//
// This gateway streams directly to the client and therefore cannot retract a
// dump that has already been written. What it can do — and what this file does
// — is stop the credential from producing more of them: the first degraded turn
// parks the credential for a cooldown, a second one disables it. A non-streaming
// turn, which is buffered anyway, is withheld and retried on another account.
const (
	// qualityCooldown is how long a credential stops serving after a degraded
	// response.
	qualityCooldown = 12 * time.Hour
	// qualityDisableThreshold is the number of degraded turns that disables the
	// credential outright.
	qualityDisableThreshold = 2
	// qualityBurstVisible and qualityBurstReasoning describe the "late dump"
	// shape: a short visible answer billed with a large reasoning count.
	qualityBurstVisible   = int64(32)
	qualityBurstReasoning = int64(80)
	// qualityBurstFlushMS is the window in which a dump's visible text arrives
	// after the reasoning bill.
	qualityBurstFlushMS = int64(2000)
	// qualityMinVisible is the smallest visible answer that may be judged at all.
	qualityMinVisible = int64(8)
)

// qualitySignals is what one turn observed about the upstream response.
type qualitySignals struct {
	// ExpectReasoning is true when the request asked for reasoning (an effort
	// other than none, or an active reasoning replay).
	ExpectReasoning bool
	// SawReasoning is true when any reasoning reached the caller: a summary, a
	// reasoning text delta, or a decrypted reasoning item.
	SawReasoning bool
	// VisibleChars and ReasoningChars are what actually streamed.
	VisibleChars   int64
	ReasoningChars int64
	// EncryptedChars is the length of opaque reasoning (encrypted_content).
	EncryptedChars int64
	// ReasoningTokens is the billed reasoning count when the upstream reported
	// one.
	ReasoningTokens int64
	// FirstVisibleMS is when the first visible text arrived, measured from the
	// start of the turn. Negative means no arrival time was observed, including
	// completed JSON responses that expose no upstream streaming timestamps.
	FirstVisibleMS int64
	// Terminal is true when the response reached a terminal event.
	Terminal bool
	// ToolCalls is the number of tool calls produced.
	ToolCalls int
}

// qualityDegraded reports whether this response is a degraded one: the request
// asked for reasoning, the turn produced an answer, and no reasoning arrived.
//
// A turn that only called tools is not judged: the model is allowed to act
// without narrating its reasoning, and a false positive here disables a working
// credential.
func qualityDegraded(sig qualitySignals) bool {
	if !sig.ExpectReasoning || sig.SawReasoning || !sig.Terminal || sig.ToolCalls > 0 {
		return false
	}
	if sig.VisibleChars < qualityMinVisible && sig.ReasoningChars <= 0 {
		return false
	}
	// A large reasoning bill with almost no visible output that arrived in one
	// late burst is the flagship dump shape.
	if sig.ReasoningTokens >= qualityBurstReasoning &&
		sig.VisibleChars > 0 && sig.VisibleChars < qualityBurstVisible &&
		sig.FirstVisibleMS >= 0 && sig.FirstVisibleMS < qualityBurstFlushMS {
		return true
	}
	// No reasoning at all despite an explicit request.
	return sig.ReasoningChars <= 0
}

// qualityExpectsReasoning reports whether the request asked for reasoning.
func qualityExpectsReasoning(req *chatwire.Request, replay bool) bool {
	if replay {
		return true
	}
	if req == nil || req.ReasoningEffort == nil {
		return false
	}
	return !strings.EqualFold(strings.TrimSpace(*req.ReasoningEffort), "none")
}

// qualityGuardAction is what the guard did to a credential for one turn.
type qualityGuardAction string

const (
	// qualityGuardNone: the turn was not judged, or was healthy with nothing to
	// clear.
	qualityGuardNone qualityGuardAction = ""
	// qualityGuardParked: first offence, the credential cools down.
	qualityGuardParked qualityGuardAction = "parked"
	// qualityGuardDisabled: repeated offence, the credential is disabled.
	qualityGuardDisabled qualityGuardAction = "disabled"
	// qualityGuardCleared: a healthy turn reset a non-zero counter.
	qualityGuardCleared qualityGuardAction = "cleared"
)

// applyQualityGuard records a degraded turn against the credential. The first
// offence parks it, the second disables it; both are persisted.
//
// reason is the hold's detector when the turn was withheld, and empty when the
// guard reached the verdict on its own (a turn that had already streamed).
func (h *Handler) applyQualityGuard(ctx context.Context, acc *store.Account, sig qualitySignals, reason string) qualityGuardAction {
	if h == nil || acc == nil || h.lb == nil || h.lb.Store == nil || (reason == "" && !qualityDegraded(sig)) {
		return qualityGuardNone
	}
	if reason == "" {
		reason = qualityReasonNoReasoning
	}
	action := qualityGuardParked
	acc.QualityFailures++
	acc.QualityReason = reason
	acc.QualityCooldownUntil = time.Now().UTC().Add(qualityCooldown)
	if acc.QualityFailures >= qualityDisableThreshold {
		action = qualityGuardDisabled
		acc.Enabled = false
		acc.StatusMessage = "disabled after repeated degraded responses (" + reason + ")"
		slog.Warn("grok quality guard: credential disabled after repeated degraded responses",
			"request_id", middleware.GetRequestID(ctx), "account_id", acc.ID, "failures", acc.QualityFailures)
	} else {
		acc.StatusMessage = "parked after a degraded response (" + reason + ")"
		slog.Warn("grok quality guard: credential parked after a degraded response",
			"request_id", middleware.GetRequestID(ctx), "account_id", acc.ID, "failures", acc.QualityFailures,
			"cooldown_until", acc.QualityCooldownUntil.Format(time.RFC3339))
	}
	// The verdict needs its own writer: UpdateAccount copies a fixed field list
	// that does not include the quality fields, so going through it parked the
	// credential only in memory. The Enabled change from a second offence still
	// travels through UpdateAccount.
	if err := h.lb.Store.UpdateAccountQuality(ctx, acc.ID, acc.QualityFailures, acc.QualityCooldownUntil, reason); err != nil {
		slog.Warn("grok quality guard: failed to persist the account verdict", "account_id", acc.ID, "error", err)
	}
	if acc.ID != 0 {
		if err := h.lb.Store.UpdateAccount(ctx, acc); err != nil {
			slog.Warn("grok quality guard: failed to persist the account state", "account_id", acc.ID, "error", err)
		}
	}
	return action
}

// clearQualityGuard resets the counter after a healthy turn, so the two-strike
// rule counts consecutive offences rather than a lifetime total. It reports
// whether there was anything to clear.
func (h *Handler) clearQualityGuard(ctx context.Context, acc *store.Account) qualityGuardAction {
	if h == nil || acc == nil || h.lb == nil || h.lb.Store == nil || acc.QualityFailures == 0 {
		return qualityGuardNone
	}
	slog.Info("grok quality guard: credential cleared after a healthy response",
		"request_id", middleware.GetRequestID(ctx), "account_id", acc.ID, "previous_failures", acc.QualityFailures)
	acc.QualityFailures = 0
	acc.QualityCooldownUntil = time.Time{}
	acc.QualityReason = ""
	if err := h.lb.Store.UpdateAccountQuality(ctx, acc.ID, 0, time.Time{}, ""); err != nil {
		slog.Warn("grok quality guard: failed to clear the account verdict", "account_id", acc.ID, "error", err)
	}
	return qualityGuardCleared
}

// applyBuildQualityGuard feeds one finished turn into the quality policy and
// reports what it did to the credential.
//
// A healthy turn clears the counter; a degraded turn parks the credential (and
// disables it on the second offence). A turn the hold withheld is a degraded
// turn by definition, so the two checks cannot disagree about a credential.
// The response itself is left alone: this gateway has already streamed it, and
// rewriting a delivered answer would be worse than cooling the credential that
// produced it.
//
// A change of state is journalled as a system event that carries the request
// which caused it, so "why was this account disabled" has an answer that does
// not depend on the server log still being around.
func (h *Handler) applyBuildQualityGuard(ctx context.Context, acc *store.Account, req *chatwire.Request, outcome chatOutcome) qualityGuardAction {
	if h == nil || acc == nil || outcome.Err != nil || !outcome.Quality.Terminal {
		return qualityGuardNone
	}
	previous := acc.QualityFailures
	action := qualityGuardNone
	switch {
	case outcome.Withheld:
		// The hold refused this turn, so it is an offence whatever the guard's
		// own, narrower test says. The two used to be judged separately, and a
		// turn the hold withheld as a reasoning dump could clear the counter of
		// the credential that produced it.
		reason := outcome.WithheldReason
		if reason == "" {
			reason = qualityReasonNoReasoning
		}
		action = h.applyQualityGuard(ctx, acc, outcome.Quality, reason)
	case qualityDegraded(outcome.Quality):
		action = h.applyQualityGuard(ctx, acc, outcome.Quality, "")
	default:
		action = h.clearQualityGuard(ctx, acc)
	}
	if action != qualityGuardNone {
		h.auditQualityGuard(ctx, acc, req, outcome, action, previous)
	}
	return action
}
