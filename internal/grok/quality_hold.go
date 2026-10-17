package grok

import (
	"net/http"
	"orchids-api/internal/audit"
	"orchids-api/internal/chatwire"
	"orchids-api/internal/responses"
	"time"
)

// buildQualityHold carries the withhold state of one streaming attempt.
type buildQualityHold struct {
	writer  *deferredResponseWriter
	policy  qualityHoldPolicy
	started time.Time
	// outcome is read, never written: it is the stream's own accounting.
	outcome *chatOutcome
	// reasoningStarted marks that the upstream opened a reasoning item or sent
	// the reasoning stub. It is not proof of thinking — a degraded account sends
	// it too — but it changes what the classifier waits for.
	reasoningStarted bool
	// expired flips once the hold timeout has passed, which lets the classifier
	// stop waiting and judge what actually arrived.
	expired bool
	// reason names the detector that withheld the turn. It is empty until a
	// withhold verdict, and is what the journal row reports.
	reason string
}

// Timings are relative to the start of upstream body consumption, not ingress.
// Unknown content/release timings stay -1 instead of looking like a zero delay.
type qualityHoldTiming struct {
	HeldMS, FirstContentMS, ReleaseMS, AddedDelayMS int64
}

func (c *buildQualityHold) recordTiming(result *chatOutcome) {
	if c == nil || c.writer == nil || result == nil {
		return
	}
	released := c.writer.releaseTime()
	end := released
	if end.IsZero() {
		end = time.Now()
	}
	timing := &qualityHoldTiming{HeldMS: max(end.Sub(c.started).Milliseconds(), 0), FirstContentMS: -1, ReleaseMS: -1, AddedDelayMS: -1}
	if !released.IsZero() {
		timing.ReleaseMS = timing.HeldMS
	}
	if !result.FirstToken.IsZero() {
		timing.FirstContentMS = max(result.FirstToken.Sub(c.started).Milliseconds(), 0)
		// A discarded attempt has no delivery delay: it never reached the client.
		if !released.IsZero() {
			timing.AddedDelayMS = max(released.Sub(result.FirstToken).Milliseconds(), 0)
		}
	}
	result.QualityTiming = timing
}

func addQualityTiming(metadata map[string]any, result chatOutcome) {
	if timing := result.QualityTiming; timing != nil {
		metadata["quality_hold_ms"] = timing.HeldMS
		metadata["quality_upstream_first_content_ms"] = timing.FirstContentMS
		metadata["quality_release_ms"] = timing.ReleaseMS
		metadata["quality_added_delay_ms"] = timing.AddedDelayMS
	}
}

func newBuildQualityHold(w http.ResponseWriter, policy qualityHoldPolicy, outcome *chatOutcome) *buildQualityHold {
	return &buildQualityHold{
		writer:  newDeferredResponseWriter(w),
		policy:  normalizeQualityHoldPolicy(policy),
		started: time.Now(),
		outcome: outcome,
	}
}

// signals converts the stream's accounting into the classifier's inputs. The
// character counts are converted to the token measure the thresholds are
// written in, with the same rune/4 estimate the usage accounting uses.
func (c *buildQualityHold) signals(terminal bool) qualityStreamSignals {
	if c == nil || c.outcome == nil {
		return qualityStreamSignals{Terminal: terminal}
	}
	quality := c.outcome.Quality
	sig := qualityStreamSignals{
		VisibleTokens:   tokensFromChars(quality.VisibleChars),
		ReasoningTokens: quality.ReasoningTokens,
		EncryptedBytes:  quality.EncryptedChars,
		UsageReported:   c.outcome.UsageSource == audit.UsageSourceUpstream,
		// Timing-based detectors require observed text arrival, not just a
		// character count. Completed JSON bodies keep FirstVisibleMS at -1.
		FirstVisible:          quality.VisibleChars > 0 && quality.VisibleTimingObserved,
		FirstVisibleMS:        quality.FirstVisibleMS,
		VisibleTimingObserved: quality.VisibleTimingObserved,
		VisibleFlushMS:        quality.visibleDurationMS(),
		Terminal:              terminal || quality.Terminal,
		HoldExpired:           c.expired,
		ReasoningStarted:      c.reasoningStarted,
		OutputTokens:          int64(responses.InterfaceToInt(c.outcome.Usage["completion_tokens"])),
	}
	sig.HasReasoningDelta = quality.ReasoningChars > 0
	sig.EncryptedFloor = encryptedThinkingFloor(0, 0, quality.ReasoningTokens)
	sig.HasThinking = sig.HasReasoningDelta || qualityHasBilledEncryptedReasoning(sig) || qualityMeetsEncryptedFloor(sig)
	return sig
}

// step classifies the held turn. waiting reports whether the caller should keep
// reading upstream.
func (c *buildQualityHold) step(terminal bool) (verdict qualityVerdict, waiting bool) {
	if c == nil || c.writer == nil {
		return qualityDeliver, false
	}
	if !c.expired && time.Since(c.started) >= c.policy.HoldTimeout {
		c.expired = true
	}
	sig := c.signals(terminal)
	result, reason := classifyQualityHoldReason(sig, c.policy.MinOutput)
	if result == qualityWithhold {
		c.reason = reason
	} else {
		c.reason = ""
	}
	return result, result == qualityWait
}

// markReasoningStarted records that the upstream began a reasoning item.
func (c *buildQualityHold) markReasoningStarted() {
	if c != nil {
		c.reasoningStarted = true
	}
}

// qualityHoldPolicy withholds degraded turns and fails open at the retry limit.
func (h *Handler) qualityHoldPolicy() qualityHoldPolicy {
	return normalizeQualityHoldPolicy(qualityHoldPolicy{})
}

// shouldHoldQualityTurn reports whether this request and plane take part in the
// hold. Build reasoning requests participate: a request that did not ask for
// reasoning has nothing to be degraded about, and holding a hosted-tool request
// could only ever refuse a turn whose side effect already happened.
func (h *Handler) shouldHoldQualityTurn(req *chatwire.Request, provider string) bool {
	if h == nil || req == nil {
		return false
	}
	if cfg := h.configSnapshot(); cfg != nil && cfg.GrokLowLatency {
		return false
	}
	if provider != ProviderBuild {
		return false
	}
	return qualityExpectsReasoning(req, req.ReasoningReplay)
}
