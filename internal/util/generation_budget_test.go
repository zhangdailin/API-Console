package util

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

func TestGenerationBudgetStopsAtOutputAndPreservesParent(t *testing.T) {
	parent, end := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer end()
	ctx, cancel := WithFirstGenerationTimeout(parent, 15*time.Millisecond)
	defer cancel()
	MarkGenerationProgress(ctx)
	select {
	case <-ctx.Done():
		t.Fatal("generation progress must release first-token timer")
	case <-time.After(35 * time.Millisecond):
	}
	<-ctx.Done()
	if context.Cause(ctx) != context.DeadlineExceeded {
		t.Fatal(context.Cause(ctx))
	}
}

func TestRetryWaitFitsActiveBudgetsWithoutChangingRetryAfter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		parent, end := context.WithTimeout(context.Background(), time.Minute)
		defer end()
		ctx, cancel := WithFirstGenerationTimeout(parent, 10*time.Second)
		defer cancel()
		if !RetryWaitFits(ctx, time.Second) || RetryWaitFits(ctx, 15*time.Second) {
			t.Fatal("first-generation deadline ignored")
		}
		MarkGenerationProgress(ctx)
		if !RetryWaitFits(ctx, 15*time.Second) || RetryWaitFits(ctx, time.Minute) {
			t.Fatal("parent deadline ignored after progress")
		}
		cancel()
		if RetryWaitFits(ctx, 0) {
			t.Fatal("canceled context admitted retry")
		}
	})
}

func TestGenerationBudgetExpiresAndCanBeDisabled(t *testing.T) {
	ctx, cancel := WithFirstGenerationTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	<-ctx.Done()
	if context.Cause(ctx) != context.DeadlineExceeded {
		t.Fatal(context.Cause(ctx))
	}
	ctx, cancel = WithFirstGenerationTimeout(context.Background(), 0)
	cancel()
	if context.Cause(ctx) != context.Canceled {
		t.Fatal(context.Cause(ctx))
	}
}
