package grok

import (
	"context"
	"orchids-api/internal/chatwire"
	"testing"
	"time"

	"orchids-api/internal/audit"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func TestBuildChatSummaryBoundary(t *testing.T) {
	for _, operation := range []string{"", "messages", "responses"} {
		for _, build := range []bool{false, true} {
			for _, effort := range []string{"", "low", "xhigh", "none"} {
				req := &chatwire.Request{Model: "grok-4.6", SourceOperation: operation, Messages: []chatwire.Message{{Role: "user", Content: "hello"}}}
				if effort != "" {
					req.ReasoningEffort = &effort
				}
				payload, err := (&Handler{}).responsesPayloadFromChat(ModelSpec{UpstreamModel: "grok-4.6"}, req, build)
				testutil.NoError(t, err)
				r, _ := payload["reasoning"].(map[string]interface{})
				// Only the Build plane owns a Chat Completions summary contract: the
				// official Build client always asks for a summary, so an omitted one
				// becomes "concise". Native Responses and Anthropic requests are left
				// alone, and "none" must never acquire a summary.
				want := interface{}(nil)
				if build && operation == "" && effort != "none" {
					want = "concise"
				}
				testutil.Equal(t, r["summary"], want)
				testutil.Falsef(t, effort == "" && r["effort"] != nil || effort != "" && r["effort"] != effort, "changed effort: %v", r)
			}
		}
	}
}

func TestChatReasoningSummaryIsClientOwned(t *testing.T) {
	for _, build := range []bool{false, true} {
		for _, operation := range []string{"", "messages", "responses"} {
			summary, effort := "auto", "low"
			req := &chatwire.Request{
				Model: "grok-4.6", SourceOperation: operation,
				ReasoningEffort: &effort, ReasoningSummary: &summary,
				Messages: []chatwire.Message{{Role: "user", Content: "hello"}},
			}
			payload, err := (&Handler{}).responsesPayloadFromChat(ModelSpec{UpstreamModel: "grok-4.6"}, req, build)
			testutil.NoError(t, err)
			r, _ := payload["reasoning"].(map[string]interface{})
			// An explicit summary is never replaced by a plane default.
			testutil.Equal(t, r["summary"], "auto")
			testutil.Equal(t, r["effort"], "low")
		}
	}
}

// Opaque reasoning must be requested on every plane; otherwise the replay cache
// is never populated and a default (auto) turn silently loses continuity.
func TestChatAlwaysRequestsEncryptedReasoning(t *testing.T) {
	for _, build := range []bool{false, true} {
		for _, effort := range []string{"", "none", "low"} {
			req := &chatwire.Request{Model: "grok-4.6", Messages: []chatwire.Message{{Role: "user", Content: "hello"}}}
			if effort != "" {
				req.ReasoningEffort = &effort
			}
			payload, err := (&Handler{}).responsesPayloadFromChat(ModelSpec{UpstreamModel: "grok-4.6"}, req, build)
			testutil.NoError(t, err)
			found := false
			switch values := payload["include"].(type) {
			case []string:
				for _, value := range values {
					if value == "reasoning.encrypted_content" {
						found = true
					}
				}
			case []interface{}:
				for _, value := range values {
					if chatwire.ParseLooseStringAny(value) == "reasoning.encrypted_content" {
						found = true
					}
				}
			}
			testutil.True(t, found, "build=%v effort=%q include=%v")
		}
	}
}

func TestAuditChatOutcomePersistsAccountTokens(t *testing.T) {
	s := newTestGrokStore(t, "grok_usage_test:")
	acc := &store.Account{AccountType: "grok", GrokProvider: ProviderBuild, Enabled: true}
	testutil.NoError(t, s.CreateAccount(context.Background(), acc))
	h := &Handler{lb: loadbalancer.NewWithCacheTTL(s, time.Minute), auditLogger: audit.NewNopLogger()}
	h.auditChatOutcome(context.Background(), acc, &chatwire.Request{Model: "grok-4.7"}, chatOutcome{
		Finish:      "stop",
		Usage:       map[string]interface{}{"prompt_tokens": 120, "completion_tokens": 30, "total_tokens": 150},
		UsageSource: audit.UsageSourceUpstream,
	})
	got, err := s.GetAccount(context.Background(), acc.ID)
	testutil.NoError(t, err)
	testutil.Equal(t, got.TokensToday, 150)
	testutil.Equal(t, got.UsageTotal, 150)
	testutil.Equal(t, got.RequestCount, 0)
}

func TestReasoningDiagnosticsReachAttemptAndOutcome(t *testing.T) {
	for _, effort := range []string{"", "low", "xhigh", "private-secret"} {
		r := map[string]interface{}{"summary": "concise"}
		if effort != "" {
			r["effort"] = effort
		}
		ctx := withReasoningDiagnostics(context.Background(), map[string]interface{}{"reasoning": r})
		log := &parityAuditLog{}
		h := &Handler{auditLogger: log}
		h.auditAttempt(ctx, nil, ProviderBuild, 1, time.Now(), nil)
		h.auditChatOutcome(ctx, nil, &chatwire.Request{}, chatOutcome{Finish: "stop"})
		testutil.Equal(t, len(log.events), 2)
		for _, event := range log.events {
			want := effort
			if effort == "private-secret" {
				want = "other"
			}
			if effort == "" {
				if _, exists := event.Metadata["reasoning_effort"]; exists {
					t.Fatal(event.Metadata)
				}
			} else if event.Metadata["reasoning_effort"] != want {
				t.Fatal(event.Metadata)
			}
			testutil.Equal(t, event.Metadata["reasoning_summary"], "concise")
		}
	}
}
