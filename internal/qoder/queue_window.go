package qoder

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

// The gateway's low-priority queues (queueType p3 and p4) answer 10605 with
// a fixed retryAfterSeconds of 30, but that hint does not describe when they
// admit work.
//
// Two measurements define the window. Probing on 2026-10-21 from two accounts
// at 10s intervals, and from one account at 35s intervals, placed every one of
// about thirty admitted requests between second :08 and :19 of a wall-clock
// minute, with requests from different accounts admitted inside the same window
// and every request outside it refused within ~100ms. A denser probe on
// 2026-11-01 (one request per second for two minutes, then 130 samples across
// three inference nodes) put every admission at :10 through :20 and every
// refusal elsewhere, and showed how small the allowance is: about five
// admissions per minute for one account, shared by api1, api2 and api3 rather
// than granted per node, and unaffected by the client identity the request
// presents.
//
// Two consequences follow. Waiting the hinted 30 seconds lands outside the
// window about half the time, so a refused request waits for the next window
// instead. And because the allowance is spent within seconds, one attempt
// inside the window succeeds only about half the time — the window has to be
// swept, not sampled. A one-second step gives about twelve attempts per window;
// at the per-attempt rate measured above, a single request needs at most two
// windows to be admitted, which is what the bounds below allow.
const (
	queueWindowOpen  = 8 * time.Second
	queueWindowClose = 20 * time.Second
	// queueWindowStep spaces retries inside an open window. It is one second,
	// not three: at three seconds a window offers four attempts, which the
	// measured ~50% per-attempt rate leaves at roughly a 6% failure chance.
	queueWindowStep = 1 * time.Second
	// queueWindowMaxWait bounds the waiting one request spends on windows,
	// and queueWindowMaxRetries the requests it sends doing so: about two
	// windows of twelve attempts each, with room to spare.
	queueWindowMaxWait    = 200 * time.Second
	queueWindowMaxRetries = 24
)

// queueWindowNow is the clock the window is read from; tests replace it.
var queueWindowNow = time.Now

// nextQueueWindowDelay is how long to wait before the next attempt: one step
// while the window is open, otherwise until the next window opens.
func nextQueueWindowDelay(now time.Time) time.Duration {
	intoMinute := time.Duration(now.Second())*time.Second + time.Duration(now.Nanosecond())
	switch {
	case intoMinute < queueWindowOpen:
		return queueWindowOpen - intoMinute
	case intoMinute+queueWindowStep < queueWindowClose:
		return queueWindowStep
	default:
		return time.Minute - intoMinute + queueWindowOpen
	}
}

// NextQueueWindowDelay exposes the retry schedule so the operator probe can
// replay it in-process. A process per attempt pays several seconds of startup
// between attempts and never lands on the window, which measures the CLI
// instead of the policy.
func NextQueueWindowDelay(now time.Time) time.Duration { return nextQueueWindowDelay(now) }

// QueueWindowLimits reports the retry bounds the probe replays.
func QueueWindowLimits() (maxWait time.Duration, maxRetries int) {
	return queueWindowMaxWait, queueWindowMaxRetries
}

// isWindowedQueueRefusal reports a 10605 from one of the low-priority queues
// whose admission follows the per-minute window.
func isWindowedQueueRefusal(err error) bool {
	if !errors.Is(err, ErrBusy) {
		return false
	}
	switch strings.ToLower(queueTypeOf(err.Error())) {
	case "p3", "p4":
		return true
	}
	// A refusal that says the request was queued is a queue refusal even when
	// the upstream omits the class: a CALL_COUNT_LIMIT refusal carries
	// isQueued/serviceAvailable/waitTime and no queueType, and treating it as a
	// plain throttle handed it to the shared retry loop instead of this one.
	return queueRefusalIsCallCountLimit(err.Error())
}

// queueRefusalIsCallCountLimit recognises the refusal the upstream sends when
// an account has spent its call allowance: it carries isQueued, serviceAvailable
// and waitTime but no queueType, which used to hand it to the shared retry loop.
func queueRefusalIsCallCountLimit(text string) bool {
	text = strings.ToLower(strings.ReplaceAll(text, `\`, ""))
	text = strings.ReplaceAll(text, " ", "")
	return strings.Contains(text, "call_count_limit")
}

// queueRefusalWait reads the wait the refusal itself names, for a refusal that
// carries no queue class to align to a window.
func queueRefusalWait(text string) time.Duration {
	text = strings.ToLower(strings.ReplaceAll(text, `\`, ""))
	text = strings.ReplaceAll(text, " ", "")
	const marker = `"retryafterseconds":`
	index := strings.Index(text, marker)
	if index < 0 {
		return 0
	}
	digits := text[index+len(marker):]
	end := 0
	for end < len(digits) && digits[end] >= '0' && digits[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0
	}
	seconds, err := strconv.Atoi(digits[:end])
	if err != nil || seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// modelKeyOf reads the model the queue refusal names, so the status read asks
// about the same model the refusal is about.
func modelKeyOf(text string) string {
	text = strings.ReplaceAll(text, `\`, "")
	const marker = `"modelKey":`
	index := strings.Index(text, marker)
	if index < 0 {
		return ""
	}
	value := strings.TrimLeft(text[index+len(marker):], ` "`)
	if end := strings.IndexAny(value, `",}`); end >= 0 {
		value = value[:end]
	}
	return strings.TrimSpace(value)
}

// queueTypeOf reads queueType out of a refusal however deeply it was quoted.
func queueTypeOf(text string) string {
	text = strings.ReplaceAll(text, `\`, "")
	const marker = `"queueType":`
	index := strings.Index(text, marker)
	if index < 0 {
		return ""
	}
	value := strings.TrimLeft(text[index+len(marker):], ` "`)
	if end := strings.IndexAny(value, `",}`); end >= 0 {
		value = value[:end]
	}
	return value
}

// alignQueueHint replaces a windowed refusal's fixed 30-second hint with the
// time to the next admission window, for whoever waits after this client has
// spent its own window retries: the shared handler's next attempt and the
// Retry-After a final 429 carries. Thirty seconds after a window closes is
// still outside the next one, so the handler's wait used to buy nothing.
func (c *Client) alignQueueHint(err error) error {
	if c == nil || !c.queueWindowRetry || !isWindowedQueueRefusal(err) {
		return err
	}
	var attempt *attemptStreamError
	if errors.As(err, &attempt) {
		attempt.wait = nextQueueWindowDelay(queueWindowNow())
	}
	return err
}
