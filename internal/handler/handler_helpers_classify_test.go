package handler

import (
	"errors"
	"fmt"
	"testing"
	"time"

	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/testutil"
)

type hintedRetryError struct{ delay time.Duration }

func (e hintedRetryError) Error() string             { return "retry later" }
func (e hintedRetryError) RetryAfter() time.Duration { return e.delay }

func TestUpstreamRetryAfterReadsWrappedHint(t *testing.T) {
	err := fmt.Errorf("wrapped: %w", hintedRetryError{delay: 7 * time.Second})
	testutil.Equal(t, upstreamRetryAfter(err), 7*time.Second)
	testutil.Equal(t, upstreamRetryAfter(hintedRetryError{delay: time.Minute}), time.Minute)
	testutil.Equal(t, upstreamRetryAfter(errors.New("plain")), 0)
}

func TestClassifyUpstreamErrorCreditsExhausted(t *testing.T) {
	t.Parallel()

	errClass := apperrors.ClassifyUpstreamError("workbuddy upstream error: no remaining quota: You have run out of credits.")
	testutil.Equal(t, errClass.Category, "quota_exhausted")
	testutil.False(t, !errClass.Retryable, "expected credits exhausted to be retryable")
	testutil.False(t, !errClass.SwitchAccount, "expected credits exhausted to trigger account switch")
}

func TestShouldRetryCurrentAccountWhenNoAlternative_RateLimit(t *testing.T) {
	t.Parallel()

	testutil.False(t, shouldRetryCurrentAccountWhenNoAlternative("rate_limit"), "expected rate_limit to stop retrying the same account when no alternative exists")
}

func TestShouldRetryCurrentAccountWhenNoAlternative_ModelUnavailable(t *testing.T) {
	t.Parallel()

	testutil.False(t, !shouldRetryCurrentAccountWhenNoAlternative("model_unavailable"), "expected model_unavailable to retry the current account when no alternative exists")
}
