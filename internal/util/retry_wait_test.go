package util

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

func TestRetryWaitFitsRequestDeadlineWithoutChangingRetryAfter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		if !RetryWaitFits(ctx, 15*time.Second) || RetryWaitFits(ctx, time.Minute) {
			t.Fatal("request deadline ignored")
		}
		if !RetryWaitFits(context.Background(), time.Hour) {
			t.Fatal("wait rejected without a deadline")
		}
		cancel()
		if RetryWaitFits(ctx, 0) {
			t.Fatal("canceled context admitted retry")
		}
	})
}
