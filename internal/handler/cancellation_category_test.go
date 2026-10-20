package handler

import (
	"context"
	"testing"
	"time"

	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/testutil"
)

// TestCancellationCategory: a caller that goes away is not a malformed
// request. It was filed as "client" and logged as 400.
func TestCancellationCategory(t *testing.T) {
	closed, cancel := context.WithCancel(context.Background())
	cancel()
	testutil.Equal(t, cancellationCategory(closed), "client_closed")
	testutil.Equal(t, apperrors.StatusForCategory("client_closed"), 499)

	expired, cancelExpired := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancelExpired()
	<-expired.Done()
	testutil.Equal(t, cancellationCategory(expired), "timeout")

	// The first-generation budget cancels with a DeadlineExceeded cause.
	budget, cancelBudget := context.WithCancelCause(context.Background())
	cancelBudget(context.DeadlineExceeded)
	testutil.Equal(t, cancellationCategory(budget), "timeout")
	testutil.MustContain(t, apperrors.CategoryMessage("client_closed"), "closed")
}
