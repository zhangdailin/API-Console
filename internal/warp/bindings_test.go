package warp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/proto"
)

func initEvent(t *testing.T, conversation string) *warpapi.ResponseEvent {
	t.Helper()
	return warpapi.ResponseEvent_builder{Init: warpapi.ResponseEvent_StreamInit_builder{
		ConversationId: proto.String(conversation), RequestId: proto.String("req"), RunId: proto.String("run"),
	}.Build()}.Build()
}

func TestToolCallBindingIsRememberedAndResumed(t *testing.T) {
	tool := mcpToolCallEvent(t, "call-7", "lookup", map[string]any{"q": "x"}).GetClientActions().GetActions()[0].GetAddMessagesToTask().GetMessages()[0]
	tool.SetId("tool-message")
	tool.SetTaskId("root")
	first := testSSE(t, initEvent(t, "conv-1")) + testSSE(t, testAction(continuationCreate(tool))) + testSSE(t, testDone())
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(first))
	}))
	defer server.Close()
	account := testAccount()
	account.ID = 7
	c := NewFromAccount(account, testConfig(server.URL))
	req := testRequest()
	req.Tools = []any{map[string]any{"type": "function", "function": map[string]any{"name": "lookup", "parameters": map[string]any{"type": "object"}}}}
	ctx := bindingAuthContext(t, 1, "unit-key")
	if err := c.SendRequestWithPayload(ctx, req, nil, nil); err != nil {
		t.Fatalf("tool turn failed: %v", err)
	}
	binding, ok := c.bindings.lookupScoped(ctx, account.ID, req.Model, []string{"call-7"})
	if !ok || binding.ConversationID != "conv-1" || binding.RequestID != "req" || binding.RunID != "run" || binding.ToolRounds != 1 {
		t.Fatalf("binding not remembered: %+v %t", binding, ok)
	}
	root, err := decodeContinuationState(binding.RootTaskContext)
	if err != nil || !proto.Equal(root, continuationCreate(tool).GetCreateTask().GetTask()) {
		t.Fatalf("binding lost full root task context: %v", err)
	}
	if calls != 1 {
		t.Fatalf("tool turn made %d requests, want 1", calls)
	}
}

func TestConversationIDIsSentOnlyForBoundToolResults(t *testing.T) {
	c := NewFromAccount(testAccount(), testConfig("https://example.invalid"))
	c.bindings.save(context.Background(), "call-7", callBinding{ConversationID: "conv-1"})
	base := preparedRequest{Query: "history transcript", CurrentText: "next", ToolResults: []preparedToolResult{{ID: "call-7", Name: "lookup", Content: "ok"}}}
	if binding, ok := c.bindings.lookup(context.Background(), []string{"call-7"}); !ok || binding.ConversationID != "conv-1" {
		t.Fatal("binding lookup failed")
	}
	body, err := buildRequest("model-a", preparedRequest{Query: base.CurrentText, CurrentText: base.CurrentText, ConversationID: "conv-1", ToolResults: base.ToolResults})
	if err != nil {
		t.Fatal(err)
	}
	var request warpapi.Request
	if proto.Unmarshal(body, &request) != nil {
		t.Fatal("request rejected")
	}
	if request.GetMetadata().GetConversationId() != "conv-1" {
		t.Fatalf("conversation id missing: %q", request.GetMetadata().GetConversationId())
	}
	// Without a binding the stateless transcript is sent and no conversation id
	// is claimed.
	if binding, ok := c.bindings.lookup(context.Background(), []string{"unknown"}); ok || binding.ConversationID != "" {
		t.Fatal("unknown call id resolved to a conversation")
	}
	other, err := buildRequest("model-a", preparedRequest{Query: "transcript"})
	if err != nil {
		t.Fatal(err)
	}
	var plain warpapi.Request
	if proto.Unmarshal(other, &plain) != nil {
		t.Fatal("request rejected")
	}
	if plain.GetMetadata().GetConversationId() != "" {
		t.Fatal("unbound request claimed a conversation")
	}
}

func TestBindingRejectsMixedConversationsAndExpiry(t *testing.T) {
	c := NewFromAccount(testAccount(), testConfig("https://example.invalid"))
	c.bindings.save(context.Background(), "a", callBinding{ConversationID: "conv-1"})
	c.bindings.save(context.Background(), "b", callBinding{ConversationID: "conv-2"})
	if _, ok := c.bindings.lookup(context.Background(), []string{"a", "b"}); ok {
		t.Fatal("mixed conversations resumed together")
	}
	c.bindings.save(context.Background(), "old", callBinding{ConversationID: "conv-1"})
	c.bindings.mu.Lock()
	key := scopedBindingID("local", 0, "", "old")
	stale := c.bindings.memory[key]
	stale.SavedAt = time.Now().Add(-2 * time.Hour)
	c.bindings.memory[key] = stale
	c.bindings.mu.Unlock()
	if _, ok := c.bindings.lookup(context.Background(), []string{"old"}); ok {
		t.Fatal("expired binding resumed")
	}
}

func TestUnboundToolResultsFallBackToTranscriptText(t *testing.T) {
	query := appendToolResultsText("User: earlier", []preparedToolResult{{ID: "call-9", Name: "lookup", Content: "found"}})
	if !strings.Contains(query, "Tool result (id=call-9, name=lookup): found") {
		t.Fatalf("tool result not rendered: %q", query)
	}
	if got := appendToolResultsText("", []preparedToolResult{{ID: "x", Content: "y"}}); !strings.Contains(got, "y") {
		t.Fatalf("empty transcript lost the result: %q", got)
	}
}
