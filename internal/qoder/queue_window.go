package qoder

import (
	"errors"
	"strings"
	"time"
)

// The gateway's low-priority queues (queueType p3 and p4) answer 10605 with
// a fixed retryAfterSeconds of 30, but that hint does not describe when they
// admit work. Probing on 2026-10-21 from two accounts at 10s intervals, and
// from one account at 35s intervals, placed every one of about thirty admitted
// requests between second :08 and :19 of a wall-clock minute, with requests
// from different accounts admitted inside the same window and every request
// outside it refused within ~100ms. The capacity behaves like a per-minute
// allowance shared with every other caller, reset shortly after the minute
// turns and spent within seconds.
//
// Waiting the hinted 30 seconds therefore lands outside the window about half
// the time. A refused request instead waits for the next window and retries
// a few times inside it. The bound below keeps that to two windows.
const (
	queueWindowOpen  = 8 * time.Second
	queueWindowClose = 20 * time.Second
	// queueWindowStep spaces retries inside an open window.
	queueWindowStep = 3 * time.Second
	// queueWindowMaxWait bounds the waiting one request spends on windows,
	// and queueWindowMaxRetries the requests it sends doing so: two windows
	// of four steps each, with room to spare.
	queueWindowMaxWait    = 130 * time.Second
	queueWindowMaxRetries = 12
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
	return false
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
