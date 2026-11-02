package util

import (
	"context"
	"time"
)

// RetryWaitFits rejects waits that cannot finish before the request deadline.
// It never shortens an upstream Retry-After hint.
func RetryWaitFits(ctx context.Context, wait time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Add(max(wait, 0)).Before(deadline) {
		return false
	}
	return true
}
