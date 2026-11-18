package upstream

import (
	"context"
	"testing"

	"orchids-api/internal/testutil"
)

func TestExplicitResumeDefaultsAndOverrides(t *testing.T) {
	base := context.Background()
	testutil.False(t, ExplicitResume(base))
	enabled := WithExplicitResume(base, true)
	testutil.True(t, ExplicitResume(enabled))
	testutil.False(t, ExplicitResume(base), "parent context must remain unchanged")
	disabled := WithExplicitResume(enabled, false)
	testutil.False(t, ExplicitResume(disabled), "false must override an inherited opt-in")
	testutil.True(t, ExplicitResume(enabled), "child override must not mutate parent")
}

func TestExplicitResumeSurvivesDerivedContexts(t *testing.T) {
	ctx, cancel := context.WithCancel(WithExplicitResume(context.Background(), true))
	defer cancel()
	ctx, budget := WithAttemptBudget(ctx, 1)
	ctx = WithAttemptObserver(ctx, func(bool) {})
	testutil.True(t, ExplicitResume(ctx))
	finish, err := BeginAttempt(ctx)
	testutil.NoError(t, err)
	finish(nil)
	testutil.Equal(t, budget.Used(), int64(1))
	cancel()
	testutil.True(t, ExplicitResume(ctx), "cancellation must not change transport intent")
}
