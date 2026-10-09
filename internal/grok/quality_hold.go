package grok

import (
	"bytes"
	"context"
	"errors"
	"math"
	"net/http"
	"orchids-api/internal/chatwire"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/audit"
	"orchids-api/internal/middleware"
	"orchids-api/internal/store"
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
// fail-open by default, so a pool of equally strange accounts still answers
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

	qualityRetryFailOpen   = "fail_open"
	qualityRetryFailClosed = "fail_closed"
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
	qualityActionReject      qualityRetryAction = "reject"
)

// qualityHoldPolicy is the runtime withhold/retry configuration.
type qualityHoldPolicy struct {
	Enabled         bool
	MaxAttempts     int
	HoldTimeout     time.Duration
	MinOutput       int64
	OnExhausted     string
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
	if !strings.EqualFold(strings.TrimSpace(policy.OnExhausted), qualityRetryFailOpen) {
		policy.OnExhausted = qualityRetryFailClosed
	} else {
		policy.OnExhausted = qualityRetryFailOpen
	}
	return policy
}

func (p qualityHoldPolicy) failOpen() bool {
	return normalizeQualityHoldPolicy(p).OnExhausted == qualityRetryFailOpen
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
	// started. Negative means it has not arrived.
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

// classifyQualityHold decides whether a held turn may be released.
//
// The dump detectors run first, so plaintext thinking cannot veto a one-shot
// reasoning dump and a small visible answer cannot skip the fake-encrypted
// check. Otherwise plaintext reasoning is delivered, cipher-only thinking waits
// until visible text has streamed for two seconds or the turn ends, and a turn
// with no thinking at all waits for enough visible output.
func classifyQualityHold(sig qualityStreamSignals, minOutput int64) qualityVerdict {
	if minOutput <= 0 {
		minOutput = qualityHoldMinOutputDefault
	}
	if qualityIsBurstDump(sig) || qualityIsCipherDrool(sig, minOutput) ||
		qualityIsFakeEncryptedDump(sig) || qualityIsFastReasoningRatioDump(sig) {
		return qualityWithhold
	}
	if sig.HasThinking {
		if sig.HasReasoningDelta {
			return qualityDeliver
		}
		if sig.Terminal {
			return qualityDeliver
		}
		if sig.VisibleTokens >= minOutput && sig.FirstVisible && sig.VisibleFlushMS >= qualityFakeEncFlushMS {
			return qualityDeliver
		}
		return qualityWait
	}
	output := sig.VisibleTokens
	if output <= 0 {
		output = sig.OutputTokens
	}
	enough := output >= minOutput
	if sig.ReasoningStarted && !sig.Terminal && !sig.HoldExpired {
		return qualityWait
	}
	if sig.Terminal || sig.HoldExpired {
		if output <= 0 {
			return qualityWait
		}
		if enough {
			return qualityWithhold
		}
		return qualityDeliver
	}
	if enough {
		return qualityWithhold
	}
	return qualityWait
}

// decideQualityRetry caps withhold recovery at maxAttempts. The last attempt is
// fail-open unless the deployment asked for fail-closed.
func decideQualityRetry(verdict qualityVerdict, attemptIndex, maxAttempts int, onExhausted string) qualityRetryAction {
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
	if normalizeQualityHoldPolicy(qualityHoldPolicy{OnExhausted: onExhausted}).failOpen() {
		return qualityActionDeliverLast
	}
	return qualityActionReject
}

// boundQualityRetry turns a retry into deliver-last or reject when the routing
// loop has no account left, so a held body is never dropped by continuing into
// an exhausted loop.
func boundQualityRetry(action qualityRetryAction, hasNextRoutingAttempt bool, onExhausted string) qualityRetryAction {
	if action != qualityActionRetry || hasNextRoutingAttempt {
		return action
	}
	if normalizeQualityHoldPolicy(qualityHoldPolicy{OnExhausted: onExhausted}).failOpen() {
		return qualityActionDeliverLast
	}
	return qualityActionReject
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
		switch strings.ToLower(strings.TrimSpace(interfaceString(tool["type"]))) {
		case "", "function", "custom", "local_shell", "apply_patch", "tool_search":
			// These only ask the model to return a call; the client runs it.
			continue
		case "shell":
			environment, _ := tool["environment"].(map[string]interface{})
			if strings.ToLower(strings.TrimSpace(interfaceString(environment["type"]))) != "local" {
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
			declaration = map[string]interface{}{"type": tool.Type}
		}
		switch strings.ToLower(strings.TrimSpace(firstNonEmpty(tool.Type, interfaceString(declaration["type"])))) {
		case "", "function", "custom", "local_shell", "apply_patch", "tool_search":
			continue
		case "shell":
			environment, _ := declaration["environment"].(map[string]interface{})
			if strings.ToLower(strings.TrimSpace(interfaceString(environment["type"]))) != "local" {
				return true
			}
		default:
			return true
		}
	}
	return false
}

// deferredResponseWriter buffers a response until it is committed.
//
// Nothing reaches the client while a turn is held: not the status line, not the
// headers, not one body byte. That is what makes a withheld turn invisible, and
// therefore retryable. After Commit the writer is transparent, so a released
// stream stays realtime instead of being buffered to the end.
type deferredResponseWriter struct {
	target http.ResponseWriter

	mu        sync.Mutex
	header    http.Header
	status    int
	body      bytes.Buffer
	committed bool
	discarded bool
	flusher   http.Flusher
}

func newDeferredResponseWriter(target http.ResponseWriter) *deferredResponseWriter {
	writer := &deferredResponseWriter{target: target, header: make(http.Header), status: http.StatusOK}
	if flusher, ok := target.(http.Flusher); ok {
		writer.flusher = flusher
	}
	return writer
}

func (d *deferredResponseWriter) Header() http.Header {
	if d == nil {
		return http.Header{}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.committed && d.target != nil {
		return d.target.Header()
	}
	return d.header
}

func (d *deferredResponseWriter) WriteHeader(status int) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.committed || d.discarded {
		return
	}
	d.status = status
}

func (d *deferredResponseWriter) Write(p []byte) (int, error) {
	if d == nil {
		return len(p), nil
	}
	d.mu.Lock()
	if d.discarded {
		d.mu.Unlock()
		return len(p), nil
	}
	if d.committed {
		target := d.target
		d.mu.Unlock()
		return target.Write(p)
	}
	if d.body.Len()+len(p) > qualityHoldMaxBytes {
		// The cap is enforced by delivering early rather than by refusing the
		// write: the upstream turn is legitimate, it is just too large to hold.
		d.mu.Unlock()
		if err := d.Commit(); err != nil {
			return 0, err
		}
		return d.target.Write(p)
	}
	n, err := d.body.Write(p)
	d.mu.Unlock()
	return n, err
}

// Flush is a no-op while held. After Commit it flushes the target so a released
// SSE stream does not sit in the server's buffer.
func (d *deferredResponseWriter) Flush() {
	if d == nil {
		return
	}
	d.mu.Lock()
	committed, discarded, flusher := d.committed, d.discarded, d.flusher
	d.mu.Unlock()
	if committed && !discarded && flusher != nil {
		flusher.Flush()
	}
}

// Commit writes everything that was held to the real response, then steps out of
// the way.
func (d *deferredResponseWriter) Commit() error {
	if d == nil || d.target == nil {
		return nil
	}
	d.mu.Lock()
	if d.committed {
		d.mu.Unlock()
		return nil
	}
	if d.discarded {
		d.mu.Unlock()
		return nil
	}
	header, status, body := d.header, d.status, d.body.Bytes()
	d.committed = true
	target, flusher := d.target, d.flusher
	d.mu.Unlock()

	for key, values := range header {
		target.Header().Del(key)
		for _, value := range values {
			target.Header().Add(key, value)
		}
	}
	target.WriteHeader(status)
	if len(body) > 0 {
		if _, err := target.Write(body); err != nil {
			return err
		}
	}
	if flusher != nil {
		flusher.Flush()
	}
	return nil
}

// Discard drops the held response. Every later write is swallowed, so a caller
// that keeps writing cannot accidentally reveal a withheld turn.
func (d *deferredResponseWriter) Discard() {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.discarded = true
	d.body.Reset()
	d.mu.Unlock()
}

func (d *deferredResponseWriter) Buffered() int {
	if d == nil {
		return 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.body.Len()
}

func (d *deferredResponseWriter) Status() int {
	if d == nil {
		return 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.status
}

// parkedResponse is a complete response that was withheld: status, headers and
// body, kept aside so the last attempt can still be delivered when the retry
// budget runs out under the fail-open policy.
type parkedResponse struct {
	header http.Header
	status int
	body   []byte
}

// Park removes the held response from the writer without sending it. Later
// writes are swallowed, so a caller that keeps streaming cannot reveal the
// withheld turn by accident.
func (d *deferredResponseWriter) Park() *parkedResponse {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.committed || d.discarded {
		return nil
	}
	parked := &parkedResponse{
		header: d.header.Clone(),
		status: d.status,
		body:   append([]byte(nil), d.body.Bytes()...),
	}
	d.discarded = true
	d.body.Reset()
	return parked
}

// CommitTo delivers a parked response to the real client.
func (p *parkedResponse) CommitTo(w http.ResponseWriter) error {
	if p == nil || w == nil {
		return nil
	}
	for key, values := range p.header {
		w.Header().Del(key)
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	status := p.status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	if len(p.body) == 0 {
		return nil
	}
	_, err := w.Write(p.body)
	return err
}

// isEmpty reports whether there is anything worth delivering.
func (p *parkedResponse) isEmpty() bool { return p == nil || len(p.body) == 0 }

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
		// FirstVisibleMS is zero until visible text arrives, so the flag needs
		// the character count too: an encrypted blob alone is not "visible text
		// arrived", and treating it as such withheld healthy turns before their
		// answer had a chance to stream.
		FirstVisible:     quality.VisibleChars > 0 && quality.FirstVisibleMS >= 0,
		VisibleFlushMS:   quality.FirstVisibleMS,
		Terminal:         terminal || quality.Terminal,
		HoldExpired:      c.expired,
		ReasoningStarted: c.reasoningStarted,
		OutputTokens:     int64(interfaceToInt(c.outcome.Usage["completion_tokens"])),
	}
	sig.HasReasoningDelta = quality.ReasoningChars > 0
	sig.EncryptedFloor = encryptedThinkingFloor(0, 0, quality.ReasoningTokens)
	sig.HasThinking = sig.HasReasoningDelta || qualityMeetsEncryptedFloor(sig)
	return sig
}

// step classifies the held turn. waiting reports whether the caller should keep
// reading upstream.
func (c *buildQualityHold) step(terminal bool) (verdict qualityVerdict, waiting bool) {
	if c == nil || c.writer == nil {
		return qualityDeliver, false
	}
	if c.writer.Buffered() >= qualityHoldMaxBytes {
		// Too large to hold: deliver rather than grow without bound. The writer
		// commits on overflow, so the verdict is delivered here too.
		return qualityDeliver, false
	}
	if !c.expired && time.Since(c.started) >= c.policy.HoldTimeout {
		c.expired = true
	}
	sig := c.signals(terminal)
	result := classifyQualityHold(sig, c.policy.MinOutput)
	return result, result == qualityWait
}

// markReasoningStarted records that the upstream began a reasoning item.
func (c *buildQualityHold) markReasoningStarted() {
	if c != nil {
		c.reasoningStarted = true
	}
}

// qualityHoldPolicy reads the deployment's withhold policy. The feature is on by
// default: it is the only thing that stops a degraded account from dumping into
// a client, and it fails open at the end of its retry budget.
func (h *Handler) qualityHoldPolicy() qualityHoldPolicy {
	policy := qualityHoldPolicy{Enabled: true, OnExhausted: qualityRetryFailOpen}
	if h == nil || h.configSnapshot() == nil {
		return normalizeQualityHoldPolicy(policy)
	}
	cfg := h.configSnapshot()
	if cfg.QualityHoldEnabled != nil {
		policy.Enabled = *cfg.QualityHoldEnabled
	}
	policy.MaxAttempts = cfg.QualityHoldMaxAttempts
	if cfg.QualityHoldTimeoutMs > 0 {
		policy.HoldTimeout = time.Duration(cfg.QualityHoldTimeoutMs) * time.Millisecond
	}
	if strings.TrimSpace(cfg.QualityHoldOnExhausted) != "" {
		policy.OnExhausted = cfg.QualityHoldOnExhausted
	}
	return normalizeQualityHoldPolicy(policy)
}

// shouldHoldQualityTurn reports whether this request and plane take part in the
// hold. Only the two reasoning planes are held: a request that did not ask for
// reasoning has nothing to be degraded about, and holding a hosted-tool request
// could only ever refuse a turn whose side effect already happened.
func (h *Handler) shouldHoldQualityTurn(req *chatwire.Request, provider string) bool {
	if h == nil || req == nil {
		return false
	}
	policy := h.qualityHoldPolicy()
	if !policy.Enabled || provider != ProviderBuild {
		return false
	}
	return qualityExpectsReasoning(req, req.ReasoningReplay)
}

// auditQualityDegraded records a withheld turn.
//
// The credential penalty is logged and persisted by the guard, but an operator
// asking "why did this request take two accounts" needs a request-scoped row. A
// healthy turn never writes one, so the journal stays quiet in normal operation.
func (h *Handler) auditQualityDegraded(ctx context.Context, acc *store.Account, req *chatwire.Request, outcome chatOutcome, mode string) {
	if h == nil || h.auditLogger == nil {
		return
	}
	accountID := int64(0)
	if acc != nil {
		accountID = acc.ID
	}
	model := ""
	if req != nil {
		model = req.Model
	}
	usageSource := outcome.UsageSource
	if usageSource == "" {
		usageSource = audit.UsageSourceNone
	}
	h.auditLogger.Log(ctx, audit.Event{
		Kind: audit.KindRequest, RequestID: middleware.GetRequestID(ctx), Action: "grok_quality_degraded",
		APIKeyID: middleware.APIKeyID(ctx), AccountID: accountID, Model: model, Channel: "grok",
		Provider: ProviderForAccount(acc), Status: "degraded", UsageSource: usageSource,
		InputTokens:     interfaceToInt(outcome.Usage["prompt_tokens"]),
		OutputTokens:    interfaceToInt(outcome.Usage["completion_tokens"]),
		ReasoningTokens: int(outcome.Quality.ReasoningTokens),
		Metadata: map[string]interface{}{
			"mode":             mode,
			"visible_chars":    outcome.Quality.VisibleChars,
			"reasoning_chars":  outcome.Quality.ReasoningChars,
			"encrypted_chars":  outcome.Quality.EncryptedChars,
			"first_visible_ms": outcome.Quality.FirstVisibleMS,
			"withheld":         true,
		},
	})
}
