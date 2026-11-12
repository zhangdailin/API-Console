package grok

import (
	"context"
	"orchids-api/internal/grok/grokbase"
	"testing"

	"orchids-api/internal/testutil"
)

func TestUnbindAffinityDropsTheSessionBinding(t *testing.T) {
	h := &Handler{}
	ctx := grokbase.WithSession(context.Background(), grokbase.Session{Key: "session-1", Model: "grok-4.6"})
	h.bindAffinity(ctx, ProviderBuild, 7)
	testutil.Equal(t, h.affinityAccount(ctx, ProviderBuild), 7)

	h.unbindAffinity(ctx, ProviderBuild, 7)

	testutil.Equal(t, h.affinityAccount(ctx, ProviderBuild), 0)
	// An unrelated account id must not clear the binding.
	h.bindAffinity(ctx, ProviderBuild, 7)
	h.unbindAffinity(ctx, ProviderBuild, 9)
	testutil.Equal(t, h.affinityAccount(ctx, ProviderBuild), 7)
}
