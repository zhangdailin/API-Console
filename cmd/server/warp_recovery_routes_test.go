package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	"orchids-api/internal/store"
)

const (
	warpRecoveryConversation = "synthetic-route-recovery-conversation"
	warpRecoveryCall         = "synthetic-route-recovery-call"
	warpRecoveryServerData   = "synthetic-accepted-failed-turn"
	warpRecoveryAnswer       = "Recovered route answer."
)

func warpRecoveryRouteSSE(t *testing.T, events ...*warpapi.ResponseEvent) string {
	t.Helper()
	var stream strings.Builder
	for _, event := range events {
		raw, err := proto.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&stream, "data: %s\n\n", base64.RawURLEncoding.EncodeToString(raw))
	}
	return stream.String()
}

func warpRecoveryRouteActions(actions ...*warpapi.ClientAction) *warpapi.ResponseEvent {
	return warpapi.ResponseEvent_builder{ClientActions: warpapi.ResponseEvent_ClientActions_builder{Actions: actions}.Build()}.Build()
}

// All three turns use the official Warp protobuf SSE contract. In particular,
// the failed paid turn closes cleanly at HTTP level but omits StreamFinished.
func newWarpRecoveryRouteStub(t *testing.T) (*warpRouteStub, *warpapi.Task) {
	t.Helper()
	args, err := structpb.NewStruct(map[string]any{"q": "synthetic query"})
	if err != nil {
		t.Fatal(err)
	}
	tool := warpapi.Message_builder{Id: proto.String("tool-message"), TaskId: proto.String("root"), ToolCall: warpapi.Message_ToolCall_builder{
		ToolCallId: proto.String(warpRecoveryCall), CallMcpTool: warpapi.Message_ToolCall_CallMCPTool_builder{
			Name: proto.String("lookup"), Args: args, ServerId: proto.String(warpRouteMCPServerID),
		}.Build(),
	}.Build()}.Build()
	root := warpapi.Task_builder{Id: proto.String("root"), Messages: []*warpapi.Message{tool}}.Build()
	init := warpapi.ResponseEvent_builder{Init: warpapi.ResponseEvent_StreamInit_builder{
		ConversationId: proto.String(warpRecoveryConversation), RequestId: proto.String("synthetic-request"), RunId: proto.String("synthetic-run"),
	}.Build()}.Build()
	done := warpapi.ResponseEvent_builder{Finished: warpapi.ResponseEvent_StreamFinished_builder{Done: warpapi.ResponseEvent_StreamFinished_Done_builder{}.Build()}.Build()}.Build()
	answer := warpapi.Message_builder{Id: proto.String("recovered-answer"), TaskId: proto.String("root"), AgentOutput: warpapi.Message_AgentOutput_builder{Text: proto.String(warpRecoveryAnswer)}.Build()}.Build()
	streams := []string{
		warpRecoveryRouteSSE(t, init, warpRecoveryRouteActions(warpapi.ClientAction_builder{CreateTask: warpapi.ClientAction_CreateTask_builder{Task: root}.Build()}.Build()), done),
		warpRecoveryRouteSSE(t, init, warpRecoveryRouteActions(warpapi.ClientAction_builder{UpdateTaskServerData: warpapi.ClientAction_UpdateTaskServerData_builder{
			TaskId: proto.String("root"), ServerData: proto.String(warpRecoveryServerData),
		}.Build()}.Build())),
		warpRecoveryRouteSSE(t, init, warpRecoveryRouteActions(warpapi.ClientAction_builder{AddMessagesToTask: warpapi.ClientAction_AddMessagesToTask_builder{
			TaskId: proto.String("root"), Messages: []*warpapi.Message{answer},
		}.Build()}.Build()), done),
	}
	stub := &warpRouteStub{requests: make(chan []byte, 16)}
	stub.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ai/multi-agent" {
			stub.otherCalls.Add(1)
			http.NotFound(w, r)
			return
		}
		stage := stub.aiCalls.Add(1)
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/x-protobuf" || r.Header.Get("Accept") != "text/event-stream" {
			t.Error("generation must POST official protobuf and accept SSE")
		}
		if r.Header.Get("Authorization") != "Bearer synthetic-warp-access" {
			t.Error("generation did not use the isolated account credential")
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error("read generation body:", err)
		}
		stub.requests <- raw
		if stage > int64(len(streams)) {
			t.Error("unexpected extra generation")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, streams[stage-1])
	}))
	t.Cleanup(stub.Close)
	return stub, root
}

func warpRecoveryRouteDo(t *testing.T, e *channelE2E, path, body, key string, resume bool) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", e.server.URL)
	req.Header.Set("Authorization", "Bearer "+key)
	if resume {
		req.Header.Set("X-Warp-Resume", "true")
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func warpRecoveryRouteWire(t *testing.T, stub *warpRouteStub) *warpapi.Request {
	t.Helper()
	select {
	case raw := <-stub.requests:
		wire := &warpapi.Request{}
		if err := proto.Unmarshal(raw, wire); err != nil {
			t.Fatal(err)
		}
		if wire.GetSettings().GetModelConfig().GetBase() != warpRouteModel {
			t.Fatal("route substituted the requested exact model")
		}
		return wire
	default:
		t.Fatal("missing upstream generation request")
		return nil
	}
}

// Exercise the actual authenticated route, provider capability and context
// propagation, production client and Redis-backed continuation state together.
// These are isolated mock generations; no live upstream or paid account is used.
func TestWarpRecoveryRoutesExplicitResume(t *testing.T) {
	for _, prefix := range []string{"/warp", "/warp/v1"} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", prefix, streaming), func(t *testing.T) {
				stub, originalRoot := newWarpRecoveryRouteStub(t)
				e, s, _ := newWarpRouteE2E(t, stub, 1)
				const otherKey = "sk-warp-recovery-other-owner"
				digest := sha256.Sum256([]byte(otherKey))
				if err := s.CreateApiKey(context.Background(), &store.ApiKey{Name: "other recovery owner", KeyHash: hex.EncodeToString(digest[:]), KeyPrefix: "sk-", KeySuffix: "-owner", Enabled: true}); err != nil {
					t.Fatal(err)
				}
				path := prefix + "/messages"
				messages := []map[string]any{{"role": "user", "content": "Run the synthetic lookup."}}
				body := map[string]any{"model": warpRouteModel, "stream": streaming,
					"tools": []any{map[string]any{"name": "lookup", "input_schema": map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}}}}, "messages": messages}
				marshal := func() string {
					raw, err := json.Marshal(body)
					if err != nil {
						t.Fatal(err)
					}
					return string(raw)
				}
				resp := warpRecoveryRouteDo(t, e, path, marshal(), e.managedKey, false)
				out := e.readBody(t, resp)
				if resp.StatusCode != http.StatusOK || !strings.Contains(out, warpRecoveryCall) || !strings.Contains(out, "tool_use") {
					t.Fatalf("first genuine MCP tool turn = %d %s", resp.StatusCode, out)
				}
				first := warpRecoveryRouteWire(t, stub)
				if first.GetMetadata().GetConversationId() != "" || first.GetInput().GetResumeConversation() != nil || first.GetMcpContext() == nil {
					t.Fatal("first turn was not a fresh client-tool request")
				}
				messages = append(messages,
					map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": warpRecoveryCall, "name": "lookup", "input": map[string]any{"q": "synthetic query"}}}},
					map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": warpRecoveryCall, "content": "synthetic lookup result"}}})
				body["messages"] = messages
				continuationBody := marshal()
				resp = warpRecoveryRouteDo(t, e, path, continuationBody, e.managedKey, false)
				out = e.readBody(t, resp)
				if resp.StatusCode != http.StatusBadGateway || !strings.Contains(out, "X-Warp-Resume: true") {
					t.Errorf("safe incomplete tool turn = %d %s, want 502 with recovery availability", resp.StatusCode, out)
				}
				second := warpRecoveryRouteWire(t, stub)
				inputs := second.GetInput().GetUserInputs().GetInputs()
				if second.GetMetadata().GetConversationId() != warpRecoveryConversation || len(second.GetTaskContext().GetTasks()) != 1 || !proto.Equal(second.GetTaskContext().GetTasks()[0], originalRoot) || second.GetInput().GetResumeConversation() != nil || len(inputs) != 1 || inputs[0].GetToolCallResult().GetToolCallId() != warpRecoveryCall {
					t.Fatal("owned tool result did not carry its original conversation, complete root and typed result")
				}
				results := inputs[0].GetToolCallResult().GetCallMcpTool().GetSuccess().GetResults()
				if len(results) != 1 || results[0].GetText().GetText() != "synthetic lookup result" {
					t.Fatal("typed MCP result content changed")
				}
				assertConflict := func(name, requestBody, key string, resume bool, wantCalls int64) {
					t.Helper()
					response := warpRecoveryRouteDo(t, e, path, requestBody, key, resume)
					output := e.readBody(t, response)
					if response.StatusCode != http.StatusConflict || !strings.Contains(output, "continuation_conflict") {
						t.Fatalf("%s = %d %s, want continuation conflict", name, response.StatusCode, output)
					}
					if stub.aiCalls.Load() != wantCalls || stub.otherCalls.Load() != 0 || len(stub.requests) != 0 {
						t.Fatalf("%s reached upstream: generations=%d other=%d pending=%d", name, stub.aiCalls.Load(), stub.otherCalls.Load(), len(stub.requests))
					}
				}
				assertConflict("ordinary replay", continuationBody, e.managedKey, false, 2)
				body["system"] = "Changed instructions must not reuse a checkpoint."
				assertConflict("changed resume body", marshal(), e.managedKey, true, 2)
				delete(body, "system")
				assertConflict("different managed key", continuationBody, otherKey, true, 2)
				resp = warpRecoveryRouteDo(t, e, path, continuationBody, e.managedKey, true)
				out = e.readBody(t, resp)
				if resp.StatusCode != http.StatusOK || !strings.Contains(out, warpRecoveryAnswer) {
					t.Fatalf("explicit recovery = %d %s", resp.StatusCode, out)
				}
				if streaming {
					if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") || !strings.Contains(out, "event: message_stop") || !strings.Contains(out, `"stop_reason":"end_turn"`) {
						t.Fatalf("invalid resumed SSE: %s", out)
					}
				} else if !strings.Contains(out, `"type":"message"`) || !strings.Contains(out, `"stop_reason":"end_turn"`) {
					t.Fatalf("invalid resumed message JSON: %s", out)
				}
				third := warpRecoveryRouteWire(t, stub)
				expectedRoot := proto.Clone(originalRoot).(*warpapi.Task)
				expectedRoot.SetServerData(warpRecoveryServerData)
				if third.GetInput().GetResumeConversation() == nil || third.GetInput().GetUserInputs() != nil || third.GetMetadata().GetConversationId() != warpRecoveryConversation || len(third.GetTaskContext().GetTasks()) != 1 || !proto.Equal(third.GetTaskContext().GetTasks()[0], expectedRoot) {
					t.Fatal("resume must send only ResumeConversation and the accepted updated root, without replaying user input or tool results")
				}
				assertConflict("duplicate explicit resume after success", continuationBody, e.managedKey, true, 3)
				assertConflict("ordinary replay after success", continuationBody, e.managedKey, false, 3)
			})
		}
	}
}
