package handler

import (
	"errors"
	"math/rand"
	"orchids-api/internal/config"
	apperrors "orchids-api/internal/errors"
	"strings"
	"time"
)

type retryAfterError interface {
	RetryAfter() time.Duration
}

func upstreamRetryAfter(err error) time.Duration {
	var hinted retryAfterError
	if !errors.As(err, &hinted) {
		return 0
	}
	delay := hinted.RetryAfter()
	if delay <= 0 {
		return 0
	}
	if delay > 30*time.Second {
		return 30 * time.Second
	}
	return delay
}

func computeRetryDelay(base time.Duration, attempt int, category string) time.Duration {
	if base <= 0 {
		return 0
	}
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 4 {
		attempt = 4
	}
	delay := base * time.Duration(1<<(attempt-1))
	if category == "rate_limit" && delay < 2*time.Second {
		delay = 2 * time.Second
	}
	if delay > 30*time.Second {
		delay = 30 * time.Second
	}
	return delay
}

func shouldRetryCurrentAccountWhenNoAlternative(category string) bool {
	switch strings.TrimSpace(category) {
	case "network", "timeout", "server", "model_unavailable", "unknown":
		return true
	default:
		return false
	}
}

// isSharedUpstreamRefusalClass reports whether this failure describes a resource
// shared by every account rather than this account's own limit. It is the
// category/switch pair the classifier produces for that shape, and it is also
// what tells the retry loop to keep the account it already holds: rotating
// would meet the identical refusal, so the wait is spent on the same one.
//
// upstream_unavailable (the upstream saying its own service is down for the
// model) belongs here beside the shared queue refusal: both are answerable only
// by waiting, neither is about the account, and both were previously reported to
// the client as this gateway's rate limit. upstream_queue is the same condition
// with the upstream naming a window, and is answered to the client as 429.
func isSharedUpstreamRefusalClass(class apperrors.UpstreamErrorClass) bool {
	if class.SwitchAccount {
		return false
	}
	return class.Category == "rate_limit" ||
		class.Category == "upstream_unavailable" ||
		class.Category == "upstream_queue"
}

// Default budget used when no per-deployment override is configured.
const sharedRefusalTotalWaitBudget = config.SharedRefusalWaitBudget

// sharedRefusalWaitAllowedWithin reports whether one more wait of next fits
// inside the explicit budget after spending already on waits. It is a plain
// comparison so the bound can be tested without spending the waits themselves.
//
// next is the probe interval; the caller also charges bounded jitter after sleeping.
func sharedRefusalWaitAllowedWithin(already, next, budget time.Duration) bool {
	if next <= 0 {
		return false
	}
	if budget <= 0 {
		budget = sharedRefusalTotalWaitBudget
	}
	return already+next <= budget
}

// Qoder queue gates are probed at a fixed interval. Other refusals retain their
// early-probe schedule. No positive hint means no gateway wait.
func sharedRefusalWaitForChannel(hint time.Duration, retry int, channel, category string) time.Duration {
	if strings.EqualFold(strings.TrimSpace(channel), "qoder") {
		if hint <= 0 {
			return 0
		}
		if category == "upstream_queue" {
			return config.QoderQueueRetryInterval
		}
		return hint
	}
	return sharedRefusalWait(hint, retry)
}

func sharedRefusalSleepForChannel(wait time.Duration, channel, category string) time.Duration {
	if category == "upstream_queue" && strings.EqualFold(strings.TrimSpace(channel), "qoder") {
		return wait
	}
	return wait + sharedRefusalJitter(wait)
}

// sharedRefusalWait is the legacy early-probe policy for non-Qoder channels.
// It is not appropriate for Qoder's server-declared minimum RetryAfter.
func sharedRefusalWait(hint time.Duration, retry int) time.Duration {
	if hint <= 0 {
		return 0
	}
	fractions := [...]float64{1.0 / 16.0, 1.0 / 4.0, 3.0 / 4.0}
	fraction := 1.0
	if retry >= 1 && retry <= len(fractions) {
		fraction = fractions[retry-1]
	}
	wait := min(max(time.Duration(float64(hint)*fraction), time.Second), hint)
	return wait
}

// sharedRefusalJitter spreads retries that were all handed the same upstream
// recovery time, so they do not wake together and re-queue as one spike. It is
// bounded to a fifth of the wait (at most five seconds), which keeps the wait
// anchored to the upstream's own hint.
func sharedRefusalJitter(delay time.Duration) time.Duration {
	if delay <= 0 {
		return 0
	}
	jitter := min(delay/5, 5*time.Second)
	if jitter <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(jitter)))
}
