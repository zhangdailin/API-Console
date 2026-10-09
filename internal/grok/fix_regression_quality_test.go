package grok

import (
	"context"
	"orchids-api/internal/chatwire"
	"testing"
	"time"

	"orchids-api/internal/testutil"
)

func TestQualityDegradedDetection(t *testing.T) {
	cases := []struct {
		name string
		sig  qualitySignals
		want bool
	}{
		{
			name: "healthy reasoning turn",
			sig:  qualitySignals{ExpectReasoning: true, SawReasoning: true, ReasoningChars: 120, VisibleChars: 40, Terminal: true, FirstVisibleMS: 900},
			want: false,
		},
		{
			name: "no reasoning despite the request",
			sig:  qualitySignals{ExpectReasoning: true, VisibleChars: 200, Terminal: true, FirstVisibleMS: 500},
			want: true,
		},
		{
			name: "late dump with a large reasoning bill",
			sig:  qualitySignals{ExpectReasoning: true, VisibleChars: 20, ReasoningTokens: 900, Terminal: true, FirstVisibleMS: 1800},
			want: true,
		},
		{
			name: "tool-only turn is not judged",
			sig:  qualitySignals{ExpectReasoning: true, VisibleChars: 0, ToolCalls: 1, Terminal: true, FirstVisibleMS: -1},
			want: false,
		},
		{
			name: "no reasoning expected",
			sig:  qualitySignals{ExpectReasoning: false, VisibleChars: 200, Terminal: true, FirstVisibleMS: 400},
			want: false,
		},
		{
			name: "stream never terminated",
			sig:  qualitySignals{ExpectReasoning: true, VisibleChars: 200, Terminal: false, FirstVisibleMS: 400},
			want: false,
		},
		{
			name: "empty answer is not judged",
			sig:  qualitySignals{ExpectReasoning: true, VisibleChars: 0, Terminal: true, FirstVisibleMS: -1},
			want: false,
		},
	}
	for _, tc := range cases {
		testutil.Equal(t, qualityDegraded(tc.sig), tc.want)
	}
}

func TestQualityExpectsReasoning(t *testing.T) {
	none, low := "none", "low"
	testutil.False(t, qualityExpectsReasoning(&chatwire.Request{ReasoningEffort: &none}, false), "effort=none must not expect reasoning")
	testutil.False(t, !qualityExpectsReasoning(&chatwire.Request{ReasoningEffort: &low}, false), "effort=low must expect reasoning")
	testutil.False(t, !qualityExpectsReasoning(nil, true), "an active reasoning replay must expect reasoning")
	testutil.False(t, qualityExpectsReasoning(&chatwire.Request{}, false), "a request without an effort must not expect reasoning")
}

func TestUnbindAffinityDropsTheSessionBinding(t *testing.T) {
	h := &Handler{affinity: map[string]sessionAffinityEntry{}}
	ctx := withGrokSession(context.Background(), grokSessionContext{Key: "session-1", Model: "grok-4.6"})
	h.sessionMu.Lock()
	key := affinityMapKey(grokSessionContext{Key: "session-1", Model: "grok-4.6"}, ProviderBuild)
	h.affinity[key] = sessionAffinityEntry{AccountID: 7, ExpiresAt: time.Now().Add(time.Hour)}
	h.sessionMu.Unlock()

	h.unbindAffinity(ctx, ProviderBuild, 7)

	testutil.Equal(t, h.affinityAccount(ctx, ProviderBuild), 0)
	// An unrelated account id must not clear the binding.
	h.sessionMu.Lock()
	h.affinity[key] = sessionAffinityEntry{AccountID: 7, ExpiresAt: time.Now().Add(time.Hour)}
	h.sessionMu.Unlock()
	h.unbindAffinity(ctx, ProviderBuild, 9)
	testutil.Equal(t, h.affinityAccount(ctx, ProviderBuild), 7)
}
