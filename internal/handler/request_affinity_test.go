package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/middleware"
	"orchids-api/internal/prompt"
	"orchids-api/internal/responses"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

func TestRequestAffinityIdentityIsolation(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/qoder/v1/messages", nil)
	req := ClaudeRequest{Model: "m", ClientMetadata: map[string]any{"thread_id": "thread", "turn_id": "first"}, Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "Inspect source"}}}}
	first := deriveRequestAffinity("owner", r, req, "qoder")
	testutil.False(t, first.Key == "" || first.SessionID == "", "explicit session lost")
	req.ClientMetadata["turn_id"] = "second"
	req.Messages = append(req.Messages, prompt.Message{Role: "assistant", Content: prompt.MessageContent{Text: "Working"}})
	testutil.Equal(t, deriveRequestAffinity("owner", r, req, "qoder"), first)
	for _, changed := range []requestAffinity{
		deriveRequestAffinity("another-owner", r, req, "qoder"),
		deriveRequestAffinity("owner", r, req, "cline"),
		deriveRequestAffinity("owner", r, ClaudeRequest{Model: "other", ClientMetadata: req.ClientMetadata}, "qoder"),
	} {
		testutil.False(t, changed.Key == first.Key || changed.SessionID == first.SessionID, "session crossed an ownership boundary")
	}
	req.ClientMetadata = nil
	prefix := deriveRequestAffinity("owner", r, req, "qoder")
	testutil.False(t, prefix.Key == "" || prefix.SessionID != "", "prefix must be affinity-only")
	req.Messages = append(req.Messages, prompt.Message{Role: "user", Content: prompt.MessageContent{Text: "Continue"}})
	testutil.Equal(t, deriveRequestAffinity("owner", r, req, "qoder"), prefix)
	req.Messages[0].Content.Text = "Different task"
	testutil.False(t, deriveRequestAffinity("owner", r, req, "qoder").Key == prefix.Key, "different prefixes shared affinity")
	testutil.Equal(t, deriveRequestAffinity("", r, req, "qoder"), requestAffinity{})
	cacheRequest := ClaudeRequest{Model: "m", System: SystemItems{{Type: "text", Text: "System"}}, Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "text", Text: "Inspect source"}}}}}}
	uncached := deriveRequestAffinity("owner", r, cacheRequest, "qoder")
	cacheRequest.System[0].CacheControl = &prompt.CacheControl{Type: "ephemeral"}
	cacheRequest.Messages[0].Content.Blocks[0].CacheControl = &prompt.CacheControl{Type: "ephemeral"}
	testutil.Equal(t, deriveRequestAffinity("owner", r, cacheRequest, "qoder"), uncached)
	testutil.False(t, cacheRequest.System[0].CacheControl == nil || cacheRequest.Messages[0].Content.Blocks[0].CacheControl == nil, "affinity mutated caller cache controls")
}

type affinityRecordingClient struct {
	account int64
	seen    *[]struct {
		account int64
		request upstream.UpstreamRequest
	}
	failure *bool
}

func (c *affinityRecordingClient) SendRequestWithPayload(_ context.Context, req upstream.UpstreamRequest, emit func(upstream.SSEMessage), _ *debug.Logger) error {
	*c.seen = append(*c.seen, struct {
		account int64
		request upstream.UpstreamRequest
	}{c.account, req})
	emit(upstream.SSEMessage{Type: "model.text-delta", Event: map[string]any{"delta": "Inspected source."}})
	if c.failure != nil && *c.failure {
		return errors.New("upstream connection reset after partial output")
	}
	emit(upstream.SSEMessage{Type: "model.finish", Event: map[string]any{"finishReason": "end_turn"}})
	return nil
}

func TestResponsesAffinitySurvivesReplicaAndTurnChanges(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "sse"}[stream], func(t *testing.T) {
			h, s, _ := setupModelValidationHandler(t)
			t.Cleanup(h.Close)
			cfg := &config.Config{RequestTimeout: 10}
			h.SetConfig(cfg)
			ctx := context.Background()
			for range 2 {
				testutil.NoError(t, s.CreateAccount(ctx, &store.Account{AccountType: "qoder", QoderRefreshToken: "test-token", Enabled: true, Weight: 1}))
			}
			publishModel(t, s, &store.Model{Channel: "Qoder", ModelID: "m"})
			var seen []struct {
				account int64
				request upstream.UpstreamRequest
			}
			fail := false
			factory := func(acc *store.Account, _ *config.Config) UpstreamClient {
				return &affinityRecordingClient{account: acc.ID, seen: &seen, failure: &fail}
			}
			h.SetClientFactory(factory)
			call := func(handler *Handler, key, turn string, followup bool) {
				t.Helper()
				input := []any{map[string]any{"role": "user", "content": "Inspect source"}}
				if followup {
					input = append(input, map[string]any{"type": "function_call", "call_id": "call-a", "name": "inspect", "arguments": "{}"}, map[string]any{"type": "function_call_output", "call_id": "call-a", "output": "source files"})
				}
				body, err := json.Marshal(map[string]any{"model": "m", "stream": stream, "input": input, "client_metadata": map[string]any{"thread_id": "thread", "turn_id": turn}, "tools": []any{map[string]any{"type": "function", "name": "inspect", "parameters": map[string]any{"type": "object"}}}})
				testutil.NoError(t, err)
				r := httptest.NewRequest(http.MethodPost, "/qoder/v1/responses", strings.NewReader(string(body)))
				r.Header.Set("Authorization", "Bearer "+key)
				w := httptest.NewRecorder()
				var identity requestAffinity
				bridge := responses.ResponsesBridgeHandler(handler.HandleMessages, responses.BridgeOptions{Store: s})
				middleware.APIKeyAuth(func(context.Context, string) (*middleware.APIKeyPrincipal, error) {
					return &middleware.APIKeyPrincipal{ID: 1}, nil
				}, func(w http.ResponseWriter, r *http.Request) {
					identity = prepareRequestAffinity(r, ClaudeRequest{Model: "m", ClientMetadata: map[string]any{"thread_id": "thread"}}, "qoder")
					bridge(w, r)
				})(w, r)
				if fail {
					testutil.Equal(t, handler.preferredAccount(ctx, identity), int64(0))
					testutil.MustContain(t, w.Body.String(), "error")
					return
				}
				testutil.Falsef(t, w.Code != 200 || !strings.Contains(w.Body.String(), "Inspected source."), "status=%d body=%s", w.Code, w.Body.String())
			}
			call(h, "owner-a", "first", false)
			testutil.Equal(t, len(seen), 1)
			first := seen[0]
			h.connTracker.Acquire(first.account)
			defer h.connTracker.Release(first.account)
			replica := NewWithLoadBalancer(cfg, loadbalancer.NewWithCacheTTL(s, time.Second))
			t.Cleanup(replica.Close)
			replica.SetConnTracker(h.connTracker)
			replica.SetClientFactory(factory)
			call(replica, "owner-a", "second", true)
			testutil.Equal(t, seen[1].account, first.account)
			testutil.False(t, first.request.ConversationID == "", "explicit upstream session missing")
			testutil.Equal(t, seen[1].request.ConversationID, first.request.ConversationID)
			testutil.Equal(t, seen[1].request.ChatSessionID, first.request.ChatSessionID)
			testutil.Equal(t, seen[1].request.Messages[2].Content.Blocks[0].ToolUseID, "call-a")
			call(replica, "owner-b", "third", false)
			testutil.False(t, seen[2].account == first.account || seen[2].request.ConversationID == first.request.ConversationID, "another owner inherited affinity")
			fail = true
			call(replica, "owner-c", "failed", false)
			testutil.Equal(t, len(seen), 4)
		})
	}
}

func TestPreferredAccountStillRequiresEligibilityAndCapacity(t *testing.T) {
	h, s, mini := setupModelValidationHandler(t)
	ctx := context.Background()
	preferred := &store.Account{AccountType: "qoder", QoderRefreshToken: "test-token", Enabled: true, Weight: 1, MaxConcurrent: 1}
	spare := &store.Account{AccountType: "qoder", QoderRefreshToken: "test-token", Enabled: true, Weight: 1}
	testutil.NoError(t, s.CreateAccount(ctx, preferred))
	testutil.NoError(t, s.CreateAccount(ctx, spare))
	affinity := requestAffinity{Provider: "qoder", Model: "m", Key: "scoped-key"}
	h.rememberAccount(ctx, affinity, preferred)
	testutil.Equal(t, h.preferredAccount(ctx, affinity), preferred.ID)
	selectAccount := func(excluded []int64) int64 {
		t.Helper()
		acc, err := h.selectAccountRecordWithOptions(ctx, "qoder", excluded, accountSelectionOptions{ModelID: "m", PreferredAccountID: h.preferredAccount(ctx, affinity)})
		testutil.NoError(t, err)
		return acc.ID
	}
	testutil.Equal(t, selectAccount(nil), preferred.ID)
	testutil.Equal(t, selectAccount([]int64{preferred.ID}), spare.ID)
	h.connTracker.Acquire(preferred.ID)
	testutil.Equal(t, selectAccount(nil), spare.ID)
	h.connTracker.Release(preferred.ID)
	store.RecordModelCooldownWithReason(preferred, "m", time.Now().Add(time.Hour), store.ModelCooldownThrottled)
	testutil.NoError(t, s.UpdateAccount(ctx, preferred))
	h.loadBalancer.InvalidateAccounts([]int64{preferred.ID})
	testutil.Equal(t, selectAccount(nil), spare.ID)
	h.rememberAccount(ctx, affinity, spare)
	testutil.Equal(t, h.preferredAccount(ctx, affinity), spare.ID)
	mini.FastForward(requestAffinityTTL + time.Second)
	testutil.Equal(t, h.preferredAccount(ctx, affinity), int64(0))
}
