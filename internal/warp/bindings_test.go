package warp

import (
	"net/http"
	"net/http/httptest"
	"testing"

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
