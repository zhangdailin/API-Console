package handler

import (
	"testing"
	"time"

	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/testutil"
)

// The exact status_message stored for production Qoder account 22.
const sharedRefusalMessage = `qoder upstream rejected the credential: {"code":"10605","message":"{\"isQueued\":true,\"modelKey\":\"qfmodel\",\"queueCount\":0,\"queueType\":\"p3\",\"retryAfterSeconds\":30,\"serviceAvailable\":false,\"waitTime\":30}"}`

func TestQoderQueueUsesFixedProbeInterval(t *testing.T) {
	wait := sharedRefusalWaitForChannel(30*time.Second, 1, "qoder", "upstream_queue")
	testutil.Equal(t, wait, 5*time.Second)
	testutil.True(t, sharedRefusalWaitAllowedWithin(55*time.Second, wait, sharedRefusalTotalWaitBudget))
	testutil.False(t, sharedRefusalWaitAllowedWithin(time.Minute, wait, sharedRefusalTotalWaitBudget))
	testutil.Equal(t, sharedRefusalSleepForChannel(wait, "qoder", "upstream_queue"), wait)
	testutil.Equal(t, sharedRefusalWaitForChannel(30*time.Second, 1, "qoder", "rate_limit"), 30*time.Second)
	testutil.Equal(t, sharedRefusalWaitForChannel(30*time.Second, 1, "workbuddy", "upstream_queue"), sharedRefusalWait(30*time.Second, 1))
	testutil.Equal(t, sharedRefusalWaitForChannel(0, 1, "qoder", "upstream_queue"), time.Duration(0))
}

// TestSharedRefusalClassIsRecognised pins the signal the retry loop uses. A
// shared refusal is retried on the account already held; anything the classifier
// would rotate away from must not match, or the wait would be spent re-queueing
// behind the same wall.
func TestSharedRefusalClassIsRecognised(t *testing.T) {
	t.Parallel()

	for name, message := range map[string]string{
		"production message":   sharedRefusalMessage,
		"classified busy form": "qoder gateway is busy: serviceAvailable=false retryAfterSeconds=29",
		"upstream pool":        "qoder API error: the available upstream accounts are rate-limited",
	} {
		t.Run(name, func(t *testing.T) {
			testutil.CheckFalsef(t, !isSharedUpstreamRefusalClass(apperrors.ClassifyUpstreamError(message)), "%q was not recognised as a shared refusal", message)
		})
	}

	// An account-scoped throttle keeps rotating: a different account can serve it,
	// so it must not take the wait-and-retry-same-account path.
	for name, message := range map[string]string{
		"plain 429":       "upstream API error: status=429, too many requests",
		"cline prose cap": "cline inference cap reached: Try again in 17h 59m",
	} {
		t.Run(name, func(t *testing.T) {
			testutil.CheckFalsef(t, isSharedUpstreamRefusalClass(apperrors.ClassifyUpstreamError(message)), "%q was treated as shared; it must still rotate", message)
		})
	}
}

// TestSharedRefusalJitterIsBounded pins that the de-synchronising delay stays a
// fraction of the upstream's own window: it may spread waiters, never displace
// the recovery time the upstream asked for by more than a fifth (or five
// seconds, whichever is smaller).
func TestSharedRefusalJitterIsBounded(t *testing.T) {
	t.Parallel()

	for _, delay := range []time.Duration{0, -time.Second, 100 * time.Millisecond, time.Second, 30 * time.Second, time.Hour} {
		limit := delay / 5
		if limit > 5*time.Second {
			limit = 5 * time.Second
		}
		for i := 0; i < 200; i++ {
			got := sharedRefusalJitter(delay)
			testutil.Falsef(t, got < 0, "jitter(%v) = %v, want non-negative", delay, got)
			if limit <= 0 {
				testutil.Equal(t, got, 0)
				continue
			}
			testutil.Falsef(t, got >= limit, "jitter(%v) = %v, want < %v", delay, got, limit)
		}
	}
}

// TestSharedRefusalWaitRampsUpToTheHint pins the latency shape. The upstream's
// Retry-After on a queue refusal is the window's worst case; paying it in full on
// the first retry made every affected request wait the whole window even when the
// queue had already cleared. The schedule probes early and converges on the hint.
func TestSharedRefusalWaitRampsUpToTheHint(t *testing.T) {
	t.Parallel()

	hint := 30 * time.Second
	previous := time.Duration(0)
	cumulative := time.Duration(0)
	for retry := 1; retry <= 3; retry++ {
		wait := sharedRefusalWait(hint, retry)
		testutil.Falsef(t, wait <= previous, "retry %d wait %v did not grow past %v", retry, wait, previous)
		testutil.Falsef(t, wait > hint, "retry %d wait %v exceeds the hint %v", retry, wait, hint)
		previous = wait
		cumulative += wait
	}
	// The first probe must be cheap: that is where the latency win comes from.
	first := sharedRefusalWait(hint, 1)
	testutil.CheckFalsef(t, first > hint/4, "first probe = %v, want well under the hint %v", first, hint)
	// Three retries must still cover roughly the whole window, or a slow queue
	// would never be waited out.
	testutil.CheckFalsef(t, cumulative < hint, "three retries cover %v, want at least the %v hint", cumulative, hint)
	// Beyond the schedule the hint is honoured, and every value stays bounded.
	for retry := 4; retry <= 8; retry++ {
		testutil.CheckEqual(t, sharedRefusalWait(hint, retry), hint)
	}
}

// TestSharedRefusalWaitEdges covers the degenerate hints: no hint means no extra
// wait, and a tiny hint still gets a floor so the first probe cannot turn into a
// tight retry loop against the queue.
func TestSharedRefusalWaitEdges(t *testing.T) {
	t.Parallel()

	testutil.CheckEqual(t, sharedRefusalWait(0, 1), 0)
	testutil.CheckEqual(t, sharedRefusalWait(-time.Second, 1), 0)
	testutil.CheckEqual(t, sharedRefusalWait(2*time.Second, 1), time.Second)
	testutil.CheckEqual(t, sharedRefusalWait(500*time.Millisecond, 3), 500*time.Millisecond)
}

// TestSharedRefusalWaitIsCappedByTheHandler documents that the wait the retry
// loop actually spends is the handler's capped reading of the hint, so a shared
// refusal cannot park a request goroutine on an absurd upstream value.
func TestSharedRefusalWaitIsCappedByTheHandler(t *testing.T) {
	t.Parallel()

	testutil.Equal(t, upstreamRetryAfter(hintedRetryError{delay: 30 * time.Second}), 30*time.Second)
	testutil.Equal(t, upstreamRetryAfter(hintedRetryError{delay: 4 * time.Hour}), 30*time.Second)
}
