package grok

import (
	"errors"
	"math"
	"orchids-api/internal/chatwire"
	"orchids-api/internal/util"
	"strings"
	"time"
)

// Quality hold: withhold, retry, then deliver.
//
// The detection half of that logic lives in quality_guard.go; this is the half
// that decides what the client is allowed to see.
//
// A degraded answer is a 200 whose content looks plausible but whose reasoning
// never happened: the whole visible text arrives in one late dump, the
// "thinking" is a cipher-only stub with zero reasoning tokens, or the billed
// reasoning is dumped as plaintext in a millisecond. Detecting it after the fact
// only lets the credential be penalised — the client has already read the dump.
// So the turn is held in memory until the classifier is willing to release it:
// a withheld turn was never written, which is what makes retrying it on another
// account possible at all.
//
// The hold is bounded in every direction: a timeout (default 30s), a byte cap,
// and a bounded retry budget (default 6 accounts). The last attempt is
// fail-open, so a pool of equally strange accounts still answers
// instead of failing a request the upstream completed.
const (
	qualityHoldMaxAttemptsDefault = 6
	qualityHoldTimeoutDefault     = 30 * time.Second
	qualityHoldMinOutputDefault   = int64(8)
	// qualityHoldMaxBytes caps what one withheld turn may buffer. Beyond it the
	// turn is delivered rather than held: an unbounded buffer would turn a
	// degraded account into an out-of-memory event.
	qualityHoldMaxBytes = 8 << 20

	// Dump shapes. The thresholds are token counts; the gateway measures the text
	// it saw and converts it with the same rune/4 estimate its usage accounting
	// uses, so a threshold means the same thing on both sides. The encrypted
	// content floor is likewise a byte measure, which is why it is
	// compared against the raw cipher length.
	//
	// qualityFakeEncFlushMS catches the 1.8s / ~2000-token fake-encrypted dump.
	qualityFakeEncFlushMS = int64(2000)
	// qualityCipherDroolVisible is the 128k status-loop drool: cipher-only
	// "thinking" with no plaintext reasoning while visible text already streams.
	qualityCipherDroolVisible              = int64(1024)
	qualityMinEncryptedChars               = int64(256)
	qualityEncryptedCharsPerReasoningToken = int64(4)

	qualityRetryFailOpen = "fail_open"
)

// tokensFromChars applies the gateway's rune/4 text estimate, the same one the
// usage accounting uses, so the token thresholds keep their meaning.
func tokensFromChars(chars int64) int64 {
	if chars <= 0 {
		return 0
	}
	return (chars + 3) / 4
}

// errQualityWithheld aborts a held stream from the inside. It never reaches the
// client: the caller turns it into a retry on another account.
var errQualityWithheld = errors.New("quality_degraded")

type qualityVerdict string

const (
	qualityWait     qualityVerdict = "wait"
	qualityDeliver  qualityVerdict = "deliver"
	qualityWithhold qualityVerdict = "withhold"
)

type qualityRetryAction string

const (
	qualityActionDeliver     qualityRetryAction = "deliver"
	qualityActionDeliverLast qualityRetryAction = "deliver_last"
	qualityActionRetry       qualityRetryAction = "retry"
)

// qualityHoldPolicy bounds withholding and retries; exhaustion always delivers.
type qualityHoldPolicy struct {
	MaxAttempts     int
	HoldTimeout     time.Duration
	MinOutput       int64
	AccountCooldown time.Duration
}

func normalizeQualityHoldPolicy(policy qualityHoldPolicy) qualityHoldPolicy {
	if policy.MaxAttempts <= 0 {
		policy.MaxAttempts = qualityHoldMaxAttemptsDefault
	}
	if policy.HoldTimeout <= 0 {
		policy.HoldTimeout = qualityHoldTimeoutDefault
	}
	if policy.MinOutput <= 0 {
		policy.MinOutput = qualityHoldMinOutputDefault
	}
	if policy.AccountCooldown <= 0 {
		policy.AccountCooldown = qualityCooldown
	}
	return policy
}

// qualityStreamSignals is what one upstream turn observed, expressed in the
// terms the hold classifier needs. It is a superset of the fields the post-hoc
// guard uses: the guard asks "was this degraded", the classifier asks "may this
// be released yet".
type qualityStreamSignals struct {
	// HasThinking is true when any reasoning reached the caller: plaintext, a
	// decrypted item, or an encrypted blob that met the size floor.
	HasThinking bool
	// HasReasoningDelta is true when plaintext reasoning streamed. Cipher-only
	// thinking is deliberately not proof: a fake dump sends the blob too.
	HasReasoningDelta bool
	// ReasoningStarted marks an empty reasoning item or the Chat SSE stub. That
	// is not proof of thinking either: a degraded account still emits the stub.
	ReasoningStarted bool
	//
	// VisibleTokens is estimated from the visible text; ReasoningTokens is the
	// billed count when the upstream reported one and an estimate otherwise.
	VisibleTokens   int64
	ReasoningTokens int64
	OutputTokens    int64
	// EncryptedBytes is the length of the cipher the floor is expressed in.
	EncryptedBytes int64
	EncryptedFloor int64
	UsageReported  bool
	// FirstVisible marks that visible text has arrived at all.
	FirstVisible bool
	// VisibleFlushMS is how long the visible text took to arrive after the turn
	// started. Negative means its arrival timing was not observed.
	VisibleFlushMS int64
	Terminal       bool
	HoldExpired    bool
}

// encryptedThinkingFloor is max(minChars, reasoningTokens*charsPerToken). A
// non-empty stub such as "gAAAA-cipher" is not thinking.
func encryptedThinkingFloor(minChars, charsPerToken, reasoningTokens int64) int64 {
	if minChars <= 0 {
		minChars = qualityMinEncryptedChars
	}
	if charsPerToken <= 0 {
		charsPerToken = qualityEncryptedCharsPerReasoningToken
	}
	floor := minChars
	if reasoningTokens > 0 {
		if reasoningTokens > math.MaxInt64/charsPerToken {
			return math.MaxInt64
		}
		if need := reasoningTokens * charsPerToken; need > floor {
			floor = need
		}
	}
	return floor
}

func qualityFastFlush(sig qualityStreamSignals, limitMS int64) bool {
	return sig.FirstVisible && sig.VisibleFlushMS >= 0 && sig.VisibleFlushMS < limitMS
}

func qualityMeetsEncryptedFloor(sig qualityStreamSignals) bool {
	if sig.EncryptedBytes <= 0 {
		return false
	}
	floor := sig.EncryptedFloor
	if floor <= 0 {
		floor = encryptedThinkingFloor(0, 0, sig.ReasoningTokens)
	}
	return sig.EncryptedBytes >= floor
}

func qualityHasDumpBill(sig qualityStreamSignals) bool {
	return sig.ReasoningTokens >= qualityBurstReasoning || qualityMeetsEncryptedFloor(sig)
}

// qualityIsBurstDump is the late greeting: a short visible answer dumped after a
// long hold with a large reasoning bill.
func qualityIsBurstDump(sig qualityStreamSignals) bool {
	if sig.HasReasoningDelta {
		return false
	}
	shortVisible := sig.VisibleTokens > 0 && sig.VisibleTokens < qualityBurstVisible
	if sig.HoldExpired && shortVisible && sig.ReasoningTokens >= qualityBurstReasoning {
		return true
	}
	return qualityFastFlush(sig, qualityBurstFlushMS) && qualityHasDumpBill(sig)
}

// qualityIsFakeEncryptedDump is the dump where ciphertext or a large reasoning
// bill is followed by the whole visible answer within two seconds. The visible
// size is not a gate: dumps shorter than the minimum output leaked through it.
func qualityIsFakeEncryptedDump(sig qualityStreamSignals) bool {
	if sig.HasReasoningDelta {
		return false
	}
	return qualityFastFlush(sig, qualityFakeEncFlushMS) && qualityHasDumpBill(sig)
}

// qualityIsFastReasoningRatioDump catches plaintext thinking that is still a
// one-shot dump: billed reasoning is at least 80% of the output and the visible
// text arrived within two seconds.
func qualityIsFastReasoningRatioDump(sig qualityStreamSignals) bool {
	if !sig.HasReasoningDelta || !qualityFastFlush(sig, qualityFakeEncFlushMS) {
		return false
	}
	output := sig.OutputTokens
	if output <= 0 {
		output = sig.VisibleTokens + sig.ReasoningTokens
	}
	if output <= 0 || sig.ReasoningTokens <= 0 {
		return false
	}
	return sig.ReasoningTokens*5 >= output*4
}

// qualityIsCipherDrool is the status-loop: ciphertext met the floor so the turn
// looks like it thought, but no plaintext reasoning exists and the reasoning
// bill is zero while visible text streams.
func qualityIsCipherDrool(sig qualityStreamSignals, minOutput int64) bool {
	if minOutput <= 0 {
		minOutput = qualityHoldMinOutputDefault
	}
	if sig.HasReasoningDelta || sig.ReasoningTokens > 0 || sig.EncryptedBytes <= 0 {
		return false
	}
	if sig.VisibleTokens >= qualityCipherDroolVisible {
		return true
	}
	return sig.Terminal && sig.VisibleTokens >= minOutput
}

// The reasons a held turn is withheld. They name the detector that fired, so a
// journal row says why a turn was refused instead of only that it was.
const (
	qualityReasonBurstDump          = "burst_dump"
	qualityReasonCipherDrool        = "cipher_drool"
	qualityReasonFakeEncryptedDump  = "fake_encrypted_dump"
	qualityReasonFastReasoningRatio = "fast_reasoning_ratio_dump"
	// qualityReasonNoReasoning is an answer with no thinking of any kind for a
	// request that asked for it.
	qualityReasonNoReasoning = "no_reasoning"
)

// classifyQualityHoldReason decides whether a held turn may be released.
//
// The dump detectors run first, so plaintext thinking cannot veto a one-shot
// reasoning dump and a small visible answer cannot skip the fake-encrypted
// check. Otherwise plaintext reasoning is delivered, cipher-only thinking waits
// until visible text has streamed for two seconds or the turn ends, and a turn
// with no thinking at all waits for enough visible output.
// The detector reason is empty for every non-withhold verdict.
func classifyQualityHoldReason(sig qualityStreamSignals, minOutput int64) (qualityVerdict, string) {
	if minOutput <= 0 {
		minOutput = qualityHoldMinOutputDefault
	}
	switch {
	case qualityIsBurstDump(sig):
		return qualityWithhold, qualityReasonBurstDump
	case qualityIsCipherDrool(sig, minOutput):
		return qualityWithhold, qualityReasonCipherDrool
	case qualityIsFakeEncryptedDump(sig):
		return qualityWithhold, qualityReasonFakeEncryptedDump
	case qualityIsFastReasoningRatioDump(sig):
		return qualityWithhold, qualityReasonFastReasoningRatio
	}
	if sig.HasThinking {
		if sig.HasReasoningDelta {
			return qualityDeliver, ""
		}
		if sig.Terminal {
			return qualityDeliver, ""
		}
		if sig.VisibleTokens >= minOutput && sig.FirstVisible && sig.VisibleFlushMS >= qualityFakeEncFlushMS {
			return qualityDeliver, ""
		}
		return qualityWait, ""
	}
	output := sig.VisibleTokens
	if output <= 0 {
		output = sig.OutputTokens
	}
	enough := output >= minOutput
	if sig.ReasoningStarted && !sig.Terminal && !sig.HoldExpired {
		return qualityWait, ""
	}
	if sig.Terminal || sig.HoldExpired {
		if output <= 0 {
			return qualityWait, ""
		}
		if enough {
			return qualityWithhold, qualityReasonNoReasoning
		}
		return qualityDeliver, ""
	}
	if enough {
		return qualityWithhold, qualityReasonNoReasoning
	}
	return qualityWait, ""
}

// decideQualityRetry caps withhold recovery at maxAttempts and delivers the last
// withheld response when that budget is spent.
func decideQualityRetry(verdict qualityVerdict, attemptIndex, maxAttempts int) qualityRetryAction {
	if verdict != qualityWithhold {
		return qualityActionDeliver
	}
	if maxAttempts <= 0 {
		maxAttempts = qualityHoldMaxAttemptsDefault
	}
	if attemptIndex < 0 {
		attemptIndex = 0
	}
	if attemptIndex < maxAttempts-1 {
		return qualityActionRetry
	}
	return qualityActionDeliverLast
}

// boundQualityRetry turns a retry into deliver-last when the routing
// loop has no account left, so a held body is never dropped by continuing into
// an exhausted loop.
func boundQualityRetry(action qualityRetryAction, hasNextRoutingAttempt bool) qualityRetryAction {
	if action != qualityActionRetry || hasNextRoutingAttempt {
		return action
	}
	return qualityActionDeliverLast
}

// qualityRequestReplayUnsafe reports whether replaying this request on another
// account could duplicate an external side effect. Detection and penalty still
// apply to such a request; only the retry is refused.
func qualityRequestReplayUnsafe(req *chatwire.Request) bool {
	if req == nil {
		return false
	}
	if req.WebSearchOptions != nil || len(req.MCPServers) > 0 {
		return true
	}
	for _, tool := range req.ResponsesTools {
		switch strings.ToLower(strings.TrimSpace(chatwire.ParseLooseStringAny(tool["type"]))) {
		case "", "function", "custom", "local_shell", "apply_patch", "tool_search":
			// These only ask the model to return a call; the client runs it.
			continue
		case "shell":
			environment, _ := tool["environment"].(map[string]any)
			if strings.ToLower(strings.TrimSpace(chatwire.ParseLooseStringAny(environment["type"]))) != "local" {
				return true
			}
		default:
			// Every server-side tool type, including ones this gateway does not
			// know yet, defaults to "do not replay".
			return true
		}
	}
	for _, tool := range req.Tools {
		declaration := tool.Raw
		if declaration == nil {
			declaration = map[string]any{"type": tool.Type}
		}
		switch strings.ToLower(strings.TrimSpace(util.FirstNonEmpty(tool.Type, chatwire.ParseLooseStringAny(declaration["type"])))) {
		case "", "function", "custom", "local_shell", "apply_patch", "tool_search":
			continue
		case "shell":
			environment, _ := declaration["environment"].(map[string]any)
			if strings.ToLower(strings.TrimSpace(chatwire.ParseLooseStringAny(environment["type"]))) != "local" {
				return true
			}
		default:
			return true
		}
	}
	return false
}
