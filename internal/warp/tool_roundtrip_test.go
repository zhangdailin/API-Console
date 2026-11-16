package warp

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/proto"

	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/prompt"
	"orchids-api/internal/upstream"
)

func TestClientToolRoundtripResumesRedisContextAndRejectsReplay(t *testing.T) {
	ctx := bindingAuthContext(t, 17, "synthetic-roundtrip-key")
	s := catalogRotationStore(t)
	account := testAccount()
	if err := s.CreateAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	if account.ID <= 0 {
		t.Fatal("account was not assigned a persistent ID")
	}

	// Every root message has its own identity, including opaque bootstrap data.
	bootstrap := bookkeepingServerCall("synthetic-bootstrap-payload")
	bootstrap.SetId("server-bootstrap-message")
	bootstrap.SetTaskId("root")
	bootstrap.GetToolCall().SetToolCallId("server-bootstrap-call")
	create := continuationCreate(bootstrap)
	create.GetCreateTask().GetTask().SetServerData("synthetic-server-state-1")
	callEvent := mcpToolCallEvent(t, "call-echo", "echo", map[string]any{"text": "LIME"})
	toolMessage := callEvent.GetClientActions().GetActions()[0].GetAddMessagesToTask().GetMessages()[0]
	toolMessage.SetTaskId("root")
	toolMessage.GetToolCall().GetCallMcpTool().SetServerId("client-request-tools")
	metadata := integrityMetadata(warpapi.Message_RequestMetadata_ToolCallSummary_builder{ToolCalls: proto.Uint32(1)}.Build(), nil)
	metadata.SetTaskId("root")
	firstStream := integrityStream(t, initEvent(t, "conv-roundtrip"), testAction(create), callEvent, integrityTaskMessages(metadata), testDone())
	secondStream := integrityStream(t,
		integrityTaskMessages(continuationText("answer-message", "outputOK")),
		testAction(warpapi.ClientAction_builder{UpdateTaskServerData: warpapi.ClientAction_UpdateTaskServerData_builder{
			TaskId: proto.String("root"), ServerData: proto.String("synthetic-server-state-2"),
		}.Build()}.Build()),
		testDone(),
	)
	expectedRoot := proto.Clone(create.GetCreateTask().GetTask()).(*warpapi.Task)
	expectedRoot.SetMessages([]*warpapi.Message{bootstrap, toolMessage, metadata})

	var calls atomic.Int64
	requests := make(chan *warpapi.Request, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		turn := calls.Add(1)
		if turn > 2 {
			t.Error("replayed tool result reached the upstream")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/ai/multi-agent" || r.Header.Get("Content-Type") != "application/x-protobuf" {
			t.Error("unexpected generation transport")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		raw, err := io.ReadAll(r.Body)
		wire := &warpapi.Request{}
		if err != nil || proto.Unmarshal(raw, wire) != nil {
			t.Error("invalid generation protobuf")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		requests <- wire
		if wire.GetSettings().GetModelConfig().GetBase() != "model-a" {
			t.Error("generation changed the exact model")
		}
		inputs := wire.GetInput().GetUserInputs().GetInputs()
		switch turn {
		case 1:
			if wire.GetMetadata().GetConversationId() != "" || len(wire.GetTaskContext().GetTasks()) != 0 || len(inputs) != 1 || inputs[0].GetUserQuery().GetQuery() != "echo" {
				t.Error("initial request unexpectedly resumed history")
			}
		case 2:
			if wire.GetMetadata().GetConversationId() != "conv-roundtrip" {
				t.Error("continuation lost the conversation ID")
			}
			tasks := wire.GetTaskContext().GetTasks()
			if len(tasks) != 1 || !proto.Equal(tasks[0], expectedRoot) {
				t.Error("continuation lost root identity, MCP call, bootstrap, metadata or server_data")
			}
			if len(inputs) != 1 || inputs[0].GetToolCallResult() == nil {
				t.Error("continuation must send only the current typed result, not a transcript")
			} else {
				result := inputs[0].GetToolCallResult()
				texts := result.GetCallMcpTool().GetSuccess().GetResults()
				if result.GetToolCallId() != "call-echo" || len(texts) != 1 || texts[0].GetText().GetText() != "LIME" {
					t.Error("continuation changed the MCP result identity or content")
				}
			}
			for _, input := range inputs {
				if input.GetUserQuery().GetQuery() != "" {
					t.Error("continuation replayed prior user or assistant transcript as a query")
				}
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		stream := firstStream
		if turn == 2 {
			stream = secondStream
		}
		_, _ = io.WriteString(w, stream)
	}))
	defer server.Close()

	tools := []any{map[string]any{"type": "function", "function": map[string]any{
		"name": "echo", "description": "Echo the supplied text", "parameters": map[string]any{
			"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}, "required": []any{"text"},
		},
	}}}
	first := historyRequest(textMessage("user", "echo"))
	first.Tools = tools
	client := newClientWithStore(account, testConfig(server.URL), s)
	var firstEvents []upstream.SSEMessage
	if err := client.SendRequestWithPayload(ctx, first, func(event upstream.SSEMessage) { firstEvents = append(firstEvents, event) }, nil); err != nil {
		t.Fatalf("initial tool call failed: %v", err)
	}
	if len(firstEvents) != 2 || firstEvents[0].Type != "model.tool-call" || firstEvents[0].Event["toolCallId"] != "call-echo" || firstEvents[0].Event["toolName"] != "echo" || firstEvents[0].Event["input"] != `{"text":"LIME"}` || firstEvents[1].Type != "model.finish" || firstEvents[1].Event["finishReason"] != "tool_use" {
		t.Fatalf("initial turn did not emit exactly the client tool call and tool_use finish: %+v", firstEvents)
	}
	firstWire := <-requests
	servers := firstWire.GetMcpContext().GetServers()
	supported := firstWire.GetSettings().GetSupportedTools()
	if len(servers) != 1 || servers[0].GetId() != "client-request-tools" || len(servers[0].GetTools()) != 1 || servers[0].GetTools()[0].GetName() != "echo" || len(supported) != 1 || supported[0] != warpapi.ToolType_CALL_MCP_TOOL {
		t.Fatal("initial request did not advertise exactly the client MCP tool")
	}

	owner, ok := bindingOwner(ctx)
	if !ok {
		t.Fatal("missing API key ownership scope")
	}
	key := s.RedisPrefix() + bindingKeyPrefix + scopedBindingID(owner, account.ID, "model-a", "call-echo")
	raw, err := s.RedisClient().Get(ctx, key).Bytes()
	if err != nil {
		t.Fatalf("scoped continuation was not saved in Redis: %v", err)
	}
	var binding callBinding
	if err := json.Unmarshal(raw, &binding); err != nil {
		t.Fatalf("invalid Redis continuation: %v", err)
	}
	root, err := decodeContinuationState(binding.RootTaskContext)
	if err != nil || !proto.Equal(root, expectedRoot) || binding.OwnerScope != owner || binding.AccountID != account.ID || binding.Model != "model-a" || binding.ConversationID != "conv-roundtrip" || binding.RequestID != "req" || binding.RunID != "run" || binding.ToolRounds != 1 {
		t.Fatal("Redis continuation did not retain the scoped official root context")
	}
	if ttl, err := s.RedisClient().TTL(ctx, key).Result(); err != nil || ttl <= 0 || ttl > bindingTTL {
		t.Fatalf("Redis continuation TTL = %v, error = %v", ttl, err)
	}

	second := historyRequest(textMessage("user", "echo"),
		prompt.Message{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{
			{Type: "tool_use", ID: "call-echo", Name: "echo", Input: map[string]any{"text": "LIME"}},
		}}},
		prompt.Message{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{
			{Type: "tool_result", ToolUseID: "call-echo", Content: "LIME"},
		}}},
	)
	second.Tools = tools
	// A fresh Client has no memory binding, so this turn must restore Redis state.
	resumed := newClientWithStore(account, testConfig(server.URL), s)
	var secondEvents []upstream.SSEMessage
	if err := resumed.SendRequestWithPayload(ctx, second, func(event upstream.SSEMessage) { secondEvents = append(secondEvents, event) }, nil); err != nil {
		t.Fatalf("tool-result continuation failed without a second CreateTask: %v", err)
	}
	secondWire := <-requests
	if !proto.Equal(firstWire.GetMcpContext(), secondWire.GetMcpContext()) || !proto.Equal(firstWire.GetSettings(), secondWire.GetSettings()) {
		t.Fatal("continuation changed the exact client tools or model settings")
	}
	if len(secondEvents) != 2 || secondEvents[0].Type != "model.text-delta" || secondEvents[0].Event["delta"] != "outputOK" || secondEvents[1].Type != "model.finish" || secondEvents[1].Event["finishReason"] != "stop" {
		t.Fatalf("resumed turn did not emit only the successful answer: %+v", secondEvents)
	}

	var replayEvents []upstream.SSEMessage
	err = newClientWithStore(account, testConfig(server.URL), s).SendRequestWithPayload(ctx, second, func(event upstream.SSEMessage) { replayEvents = append(replayEvents, event) }, nil)
	var app *apperrors.AppError
	if !errors.As(err, &app) || app.HTTPStatus != http.StatusConflict || len(replayEvents) != 0 || calls.Load() != 2 {
		t.Fatalf("tool result replay was not rejected before upstream: error=%v, events=%d, calls=%d", err, len(replayEvents), calls.Load())
	}
}
