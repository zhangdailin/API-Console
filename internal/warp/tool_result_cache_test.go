package warp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/prompt"
	"orchids-api/internal/upstream"
)

// The result is deliberately larger than the user-text ceiling. All bytes are
// synthetic; cache hints must not change the separate tool-result budget.
func toolResultCacheText() string {
	return "synthetic-result-" + strings.Repeat("x", 6381-len("synthetic-result-")-len("-end")) + "-end"
}

func toolResultCacheRaw(t *testing.T, ttl string, isError bool, content any) json.RawMessage {
	t.Helper()
	cache := map[string]any{"type": "ephemeral"}
	if ttl != "" {
		cache["ttl"] = ttl
	}
	return clientHintsRaw(t, map[string]any{
		"tools": []any{map[string]any{"name": "echo", "input_schema": map[string]any{"type": "object"}}},
		"messages": []any{
			map[string]any{"role": "user", "content": "echo"},
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "call-cache", "name": "echo", "input": map[string]any{"query": "safe"}, "cache_control": cache}}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "call-cache", "content": content, "is_error": isError, "cache_control": cache}}},
		},
	})
}

func decodeToolResultCacheRequest(t *testing.T, raw json.RawMessage) upstream.UpstreamRequest {
	t.Helper()
	var decoded struct {
		Model    string           `json:"model"`
		Messages []prompt.Message `json:"messages"`
		Tools    []any            `json:"tools"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	return upstream.UpstreamRequest{Model: decoded.Model, Messages: decoded.Messages, Tools: decoded.Tools}
}

func TestToolResultCacheRawAndNormalizedPreserveResult(t *testing.T) {
	text := toolResultCacheText()
	if len(text) != 6381 {
		t.Fatal("synthetic result length drifted")
	}
	for _, ttl := range []string{"", "5m", "1h"} {
		for _, isError := range []bool{false, true} {
			for _, blocks := range []bool{false, true} {
				t.Run(fmt.Sprintf("ttl=%s/error=%t/blocks=%t", ttl, isError, blocks), func(t *testing.T) {
					var content any = text
					if blocks {
						content = []any{map[string]any{"type": "text", "text": text}}
					}
					raw := toolResultCacheRaw(t, ttl, isError, content)
					if err := validateRawRequest(raw); err != nil {
						t.Fatalf("valid outer cache marker rejected: %v", err)
					}
					req := decodeToolResultCacheRequest(t, raw)
					before, err := json.Marshal(req)
					if err != nil {
						t.Fatal(err)
					}
					prepared, err := validateRequest(req)
					if err != nil {
						t.Fatalf("valid normalized tool result rejected: %v", err)
					}
					want := []preparedToolResult{{ID: "call-cache", Name: "echo", Content: text, IsError: isError}}
					if !reflect.DeepEqual(prepared.ToolResults, want) || prepared.CurrentText != "" {
						t.Fatal("cache hint changed result identity, tool name, full content or error status")
					}
					if !strings.Contains(prepared.Query, `{"query":"safe"}`) {
						t.Fatal("cache-marked tool history lost its call arguments")
					}
					for _, marker := range []string{"cache_control", "ephemeral", "5m", "1h"} {
						if strings.Contains(prepared.Query, marker) {
							t.Fatal("cache hint entered the prepared query")
						}
					}
					after, err := json.Marshal(req)
					if err != nil || string(before) != string(after) {
						t.Fatal("request validation mutated the caller-owned normalized request")
					}
				})
			}
		}
	}
}

func TestToolResultCacheNormalizedRejectsOtherVariantsAndLocations(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*upstream.UpstreamRequest)
	}{
		{"permanent", func(r *upstream.UpstreamRequest) { r.Messages[2].Content.Blocks[0].CacheControl.Type = "permanent" }},
		{"invalid_tool_use_marker", func(r *upstream.UpstreamRequest) {
			r.Messages[1].Content.Blocks[0].CacheControl = &prompt.CacheControl{Type: "permanent"}
		}},
		{"nested_text", func(r *upstream.UpstreamRequest) {
			r.Messages[2].Content.Blocks[0].Content = []any{map[string]any{"type": "text", "text": "synthetic", "cache_control": map[string]any{"type": "ephemeral"}}}
		}},
		{"wrong_role", func(r *upstream.UpstreamRequest) { r.Messages[2].Role = "assistant" }},
		{"foreign_field", func(r *upstream.UpstreamRequest) { r.Messages[2].Content.Blocks[0].Name = "echo" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := decodeToolResultCacheRequest(t, toolResultCacheRaw(t, "", false, toolResultCacheText()))
			tc.mutate(&req)
			_, err := validateRequest(req)
			var app *apperrors.AppError
			if !errors.As(err, &app) || app.HTTPStatus != http.StatusBadRequest {
				t.Fatalf("invalid normalized cache placement = %v, want 400", err)
			}
		})
	}
}

func TestToolResultCacheBoundClientRoundtripPreservesWireAndScope(t *testing.T) {
	for _, ttl := range []string{"", "5m", "1h"} {
		for _, isError := range []bool{false, true} {
			t.Run(fmt.Sprintf("ttl=%s/error=%t", ttl, isError), func(t *testing.T) {
				ctx := bindingAuthContext(t, 71, "synthetic-cache-owner")
				s := catalogRotationStore(t)
				account := testAccount()
				account.WarpModelIDs = []string{"model-a", "model-b"}
				if err := s.CreateAccount(ctx, account); err != nil {
					t.Fatal(err)
				}
				// Upstream fallback must not prevent declared tools or weaken
				// the caller/account/requested-model continuation binding.
				create := continuationCreate(
					warpapi.Message_builder{Id: proto.String("cache-root-message"), TaskId: proto.String("root"), UserQuery: warpapi.Message_UserQuery_builder{Query: proto.String("echo")}.Build()}.Build(),
					warpapi.Message_builder{Id: proto.String("cache-model-used"), TaskId: proto.String("root"), ModelUsed: warpapi.Message_ModelUsed_builder{ModelId: proto.String("model-b"), IsFallback: proto.Bool(true)}.Build()}.Build(),
				)
				create.GetCreateTask().GetTask().SetServerData("synthetic-cache-root-state")
				call := mcpToolCallEvent(t, "call-cache", "echo", map[string]any{"query": "safe"})
				message := call.GetClientActions().GetActions()[0].GetAddMessagesToTask().GetMessages()[0]
				message.SetTaskId("root")
				message.GetToolCall().GetCallMcpTool().SetServerId(clientMCPServerID)
				expectedRoot := proto.Clone(create.GetCreateTask().GetTask()).(*warpapi.Task)
				expectedRoot.SetMessages(append(expectedRoot.GetMessages(), message))
				firstStream := integrityStream(t, initEvent(t, "conv-tool-cache"), testAction(create), call, testDone())
				secondStream := integrityStream(t, integrityTaskMessages(continuationText("cache-answer", "synthetic cache answer")), testDone())
				var calls atomic.Int64
				requests := make(chan *warpapi.Request, 2)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					turn := calls.Add(1)
					if turn > 2 || r.URL.Path != "/ai/multi-agent" || r.Method != http.MethodPost {
						t.Error("cache continuation made an extra or unexpected upstream call")
						w.WriteHeader(http.StatusInternalServerError)
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
					w.Header().Set("Content-Type", "text/event-stream")
					stream := firstStream
					if turn == 2 {
						stream = secondStream
					}
					_, _ = io.WriteString(w, stream)
				}))
				defer server.Close()

				second := decodeToolResultCacheRequest(t, toolResultCacheRaw(t, ttl, isError, toolResultCacheText()))
				first := historyRequest(textMessage("user", "echo"))
				first.Tools = second.Tools
				var firstEvents []upstream.SSEMessage
				if err := newClientWithStore(account, testConfig(server.URL), s).SendRequestWithPayload(ctx, first, func(e upstream.SSEMessage) { firstEvents = append(firstEvents, e) }, nil); err != nil {
					t.Fatalf("initial tool call failed: %v", err)
				}
				if len(firstEvents) != 2 || firstEvents[0].Type != "model.tool-call" || firstEvents[1].Event["finishReason"] != "tool_use" {
					t.Fatal("first turn did not emit a tool call followed by tool_use finish")
				}
				firstWire := <-requests
				owner, ok := bindingOwner(ctx)
				if !ok {
					t.Fatal("missing binding owner")
				}
				key := s.RedisPrefix() + bindingKeyPrefix + scopedBindingID(owner, account.ID, "model-a", "call-cache")
				saved, err := s.RedisClient().Get(ctx, key).Bytes()
				if err != nil {
					t.Fatalf("scoped binding not persisted: %v", err)
				}
				var binding callBinding
				if err := json.Unmarshal(saved, &binding); err != nil {
					t.Fatal(err)
				}
				if binding.OwnerScope != owner || binding.AccountID != account.ID || binding.Model != "model-a" || binding.ConversationID != "conv-tool-cache" || binding.ToolRounds != 1 {
					t.Fatal("cache continuation lost persisted owner/account/exact-model scope")
				}
				// Neither owner nor exact-model mismatch may consume the real binding.
				for _, mismatch := range []string{"owner", "model"} {
					wrongCtx, wrongRequest := ctx, second
					if mismatch == "owner" {
						wrongCtx = bindingAuthContext(t, 72, "synthetic-cache-other-owner")
					} else {
						wrongRequest.Model = "model-b"
					}
					var emitted int
					err := newClientWithStore(account, testConfig(server.URL), s).SendRequestWithPayload(wrongCtx, wrongRequest, func(upstream.SSEMessage) { emitted++ }, nil)
					var app *apperrors.AppError
					if !errors.As(err, &app) || app.HTTPStatus != http.StatusConflict || emitted != 0 || calls.Load() != 1 {
						t.Fatalf("%s mismatch was not rejected before generation: err=%v events=%d calls=%d", mismatch, err, emitted, calls.Load())
					}
				}
				var events []upstream.SSEMessage
				if err := newClientWithStore(account, testConfig(server.URL), s).SendRequestWithPayload(ctx, second, func(e upstream.SSEMessage) { events = append(events, e) }, nil); err != nil {
					t.Fatalf("owned cache-marked continuation failed: %v", err)
				}
				secondWire := <-requests
				inputs := secondWire.GetInput().GetUserInputs().GetInputs()
				if len(inputs) != 1 || inputs[0].GetToolCallResult() == nil || inputs[0].GetUserQuery() != nil {
					t.Fatal("continuation must contain only its typed result, without hint or transcript query")
				}
				wantResult := buildMCPToolResult(preparedToolResult{ID: "call-cache", Name: "echo", Content: toolResultCacheText(), IsError: isError})
				if !proto.Equal(inputs[0].GetToolCallResult(), wantResult) {
					t.Fatal("cache marker changed the full typed protobuf tool result")
				}
				result := inputs[0].GetToolCallResult()
				if result.GetToolCallId() != "call-cache" {
					t.Fatal("continuation changed the tool call ID")
				}
				if isError {
					if result.GetCallMcpTool().GetError().GetMessage() != toolResultCacheText() || result.GetCallMcpTool().GetSuccess() != nil {
						t.Fatal("continuation changed the full error result or its variant")
					}
				} else {
					texts := result.GetCallMcpTool().GetSuccess().GetResults()
					if len(texts) != 1 || texts[0].GetText().GetText() != toolResultCacheText() || result.GetCallMcpTool().GetError() != nil {
						t.Fatal("continuation changed the full successful text result or its variant")
					}
				}
				if secondWire.GetMetadata().GetConversationId() != "conv-tool-cache" || len(secondWire.GetTaskContext().GetTasks()) != 1 || !proto.Equal(secondWire.GetTaskContext().GetTasks()[0], expectedRoot) || !proto.Equal(firstWire.GetSettings(), secondWire.GetSettings()) || !proto.Equal(firstWire.GetMcpContext(), secondWire.GetMcpContext()) || secondWire.GetSettings().GetModelConfig().GetBase() != "model-a" {
					t.Fatal("continuation changed conversation, task context, exact model or client tools")
				}
				wireJSON, err := protojson.Marshal(secondWire)
				if err != nil {
					t.Fatal(err)
				}
				for _, marker := range []string{"cache_control", "cacheControl", "ephemeral", "5m", "1h"} {
					if strings.Contains(string(wireJSON), marker) {
						t.Fatal("client-only cache hint leaked into continuation protobuf")
					}
				}
				if len(events) != 2 || events[0].Type != "model.text-delta" || events[0].Event["delta"] != "synthetic cache answer" || events[1].Event["finishReason"] != "stop" || calls.Load() != 2 {
					t.Fatal("continuation did not complete with exactly two total upstream calls")
				}
			})
		}
	}
}
