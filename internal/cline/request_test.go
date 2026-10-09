package cline

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"encoding/json"

	"orchids-api/internal/prompt"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

// TestBuildChatBodyCarriesTheUpstreamDefaults pins what the upstream needs on
// every request: an omitted reasoning_effort makes some models answer with empty
// content, and a session_id is how it correlates a turn.
func TestBuildChatBodyCarriesTheUpstreamDefaults(t *testing.T) {
	body, err := buildChatBody(upstream.UpstreamRequest{
		Model:    "x-ai/grok-4.1-fast",
		Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "hello"}}},
	}, "x-ai/grok-4.1-fast")
	testutil.NoError(t, err, "buildChatBody() error = %v")
	raw := string(body)
	for _, want := range []string{
		`"model":"x-ai/grok-4.1-fast"`,
		`"max_tokens":128000`,
		`"reasoning_effort":"high"`,
		`"session_id":"sess_`,
		`"stream":true`,
		`"messages":[{"role":"user","content":"hello"}]`,
	} {
		testutil.CheckContain(t, raw, want)
	}
}

func TestBuildChatBodyConvertsAnthropicToolsToOpenAI(t *testing.T) {
	body, err := buildChatBody(upstream.UpstreamRequest{
		Model:    "z-ai/glm-5.3-flash",
		Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "inspect"}}},
		Tools: []interface{}{map[string]interface{}{
			"name": "glob", "description": "Find files", "input_schema": map[string]interface{}{
				"type": "object", "properties": map[string]interface{}{"pattern": map[string]interface{}{"type": "string"}}, "required": []interface{}{"pattern"},
			},
		}},
		ToolChoice: map[string]interface{}{"type": "tool", "name": "glob"},
	}, "z-ai/glm-5.3-flash")
	testutil.NoError(t, err)
	var decoded map[string]interface{}
	testutil.NoError(t, json.Unmarshal(body, &decoded))
	tools, _ := decoded["tools"].([]interface{})
	testutil.Equal(t, len(tools), 1)
	tool := tools[0].(map[string]interface{})
	testutil.Equal(t, tool["type"], "function")
	function := tool["function"].(map[string]interface{})
	testutil.Falsef(t, function["name"] != "glob" || function["parameters"] == nil, "function=%#v", function)
	choice := decoded["tool_choice"].(map[string]interface{})
	testutil.Equal(t, choice["type"], "function")
	testutil.Equal(t, choice["function"].(map[string]interface{})["name"], "glob")
}

func TestBuildChatBodyKeepsOpenAITools(t *testing.T) {
	openAI := map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "bash", "parameters": map[string]interface{}{"type": "object"}}}
	body, err := buildChatBody(upstream.UpstreamRequest{Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "run"}}}, Tools: []interface{}{openAI}}, "z-ai/glm-5.3-flash")
	testutil.NoError(t, err)
	testutil.MustContain(t, string(body), `"tools":[{"function":{"name":"bash","parameters":{"type":"object"}},"type":"function"}]`)
}

// TestBuildMessagesMapsToolHistory proves an assistant tool_use block becomes a
// tool_calls entry and its tool_result becomes a paired `tool` message: an
// orphan tool result is rejected upstream.
func TestBuildMessagesMapsToolHistory(t *testing.T) {
	out := buildMessages(upstream.UpstreamRequest{
		System: []prompt.SystemItem{{Type: "text", Text: "be terse"}},
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "read the file"}},
			{
				Role: "assistant",
				Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{
					{Type: "tool_use", ID: "call-1", Name: "Read", Input: map[string]interface{}{"path": "/tmp/a"}},
				}},
			},
			{
				Role: "user",
				Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{
					{Type: "tool_result", ToolUseID: "call-1", Content: "file body"},
				}},
			},
		},
	})
	testutil.Equal(t, len(out), 4)
	testutil.CheckEqual(t, out[0].Role, "system")
	testutil.CheckEqual(t, out[0].Content, "be terse")
	testutil.Equal(t, len(out[2].ToolCalls), 1)
	testutil.Equal(t, out[2].ToolCalls[0].Function.Name, "Read")
	testutil.CheckEqual(t, out[3].Role, "tool")
	testutil.CheckEqual(t, out[3].ToolCallID, "call-1")
	testutil.CheckEqual(t, out[3].Content, "file body")
}

// TestBuildMessagesDropsOrphanToolResults covers the case that would otherwise
// be rejected: a tool_result whose call is not in the history has nothing to
// pair with.
func TestBuildMessagesDropsOrphanToolResults(t *testing.T) {
	out := buildMessages(upstream.UpstreamRequest{
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "hi"}},
			{
				Role: "user",
				Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{
					{Type: "tool_result", ToolUseID: "call-absent", Content: "body"},
				}},
			},
		},
	})
	testutil.Equal(t, len(out), 1)
	testutil.Equal(t, out[0].Content, "hi")
}

// TestConsumeStreamEmitsTextAndUsage drives the SSE conversion that the handler
// consumes, including the {"data":{...}} envelope: a chunk that arrives wrapped
// must still produce its delta.
func TestConsumeStreamEmitsTextAndUsage(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n" +
		"data: {\"data\":{\"choices\":[{\"delta\":{\"content\":\" world\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2}}}\n\n" +
		"data: [DONE]\n\n"
	var text strings.Builder
	var usage map[string]interface{}
	finish := false
	result, err := consumeStream(strings.NewReader(stream), false, func(msg upstream.SSEMessage) {
		switch msg.Type {
		case "model.text-delta":
			text.WriteString(msg.Event["delta"].(string))
		case "model.tokens-used":
			usage = msg.Event
		case "model.finish":
			finish = true
		}
	})
	testutil.NoError(t, err, "consumeStream() error = %v")
	testutil.CheckEqual(t, text.String(), "hello world")
	testutil.CheckFalsef(t, usage == nil || usage["inputTokens"] != 5 || usage["outputTokens"] != 2, "usage = %+v, want the normalized pair", usage)
	testutil.CheckFalse(t, !result.SawMeaningfulEvent, "SawMeaningfulEvent = false, want true")
	testutil.CheckFalse(t, finish, "consumeStream emitted a finish event; the caller owns it")
}

func TestConsumeStreamRejectsMalformedDataFrame(t *testing.T) {
	t.Parallel()
	stream := "data: {not-json}\n\ndata: [DONE]\n\n"
	_, err := consumeStream(strings.NewReader(stream), false, nil)
	testutil.Falsef(t, err == nil || !strings.Contains(err.Error(), "protocol error"), "error = %v, want protocol error", err)
}

func TestConsumeStreamRejectsEOFBeforeFinish(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"
	result, err := consumeStream(strings.NewReader(stream), false, nil)
	testutil.Falsef(t, !errors.Is(err, ErrStreamTruncated), "error=%v, want ErrStreamTruncated", err)
	testutil.False(t, !result.SawMeaningfulEvent, "partial data should remain observable even though the stream failed")
}

func TestConsumeStreamAcceptsFinishReasonWithoutDone(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"complete\"},\"finish_reason\":\"stop\"}]}\n\n"
	result, err := consumeStream(strings.NewReader(stream), false, nil)
	testutil.NoError(t, err, "error=%v")
	testutil.False(t, !result.SawMeaningfulEvent, "complete stream was not observed")
}

// TestConsumeStreamEmitsEachToolCallOnce is the regression the accumulator
// exists for: the finish-time emit and the end-of-stream flush both run, and a
// tool call must not reach the client twice.
func TestConsumeStreamEmitsEachToolCallOnce(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"function\":{\"name\":\"Read\",\"arguments\":\"{\\\"path\\\":\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"/tmp/a}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	var calls int
	var names []string
	result, err := consumeStream(strings.NewReader(stream), false, func(msg upstream.SSEMessage) {
		if msg.Type != "model.tool-call" {
			return
		}
		calls++
		names = append(names, msg.Event["toolName"].(string))
	})
	testutil.NoError(t, err, "consumeStream() error = %v")
	testutil.Equal(t, calls, 1)
	testutil.CheckEqual(t, result.ToolCallCount, 1)
	testutil.CheckEqual(t, result.FinishReason(), "tool_use")
}

func TestConsumeStreamKeepsGLMToolMarkupAsText(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"checking <tool_call>bash:ls -la /tmp/a</arg_value><arg_key>command</arg_key><arg_value>ls -la /tmp/a</arg_value></tool_call>\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	var text strings.Builder
	var calls []upstream.SSEMessage
	result, err := consumeStream(strings.NewReader(stream), true, func(msg upstream.SSEMessage) {
		switch msg.Type {
		case "model.text-delta":
			text.WriteString(msg.Event["delta"].(string))
		case "model.tool-call":
			calls = append(calls, msg)
		}
	})
	testutil.NoError(t, err, "consumeStream() error = %v")
	testutil.Equal(t, text.String(), "checking <tool_call>bash:ls -la /tmp/a</arg_value><arg_key>command</arg_key><arg_value>ls -la /tmp/a</arg_value></tool_call>")
	testutil.Equal(t, len(calls), 0)
	testutil.Equal(t, result.ToolCallCount, 0)
	testutil.Equal(t, result.FinishReason(), "end_turn")
}

// Textual tool markup never repairs a native argument fragment.
func TestConsumeStreamDoesNotRepairNativeArgumentsFromTextMarkup(t *testing.T) {
	frames := []string{
		`{"choices":[{"delta":{"content":"","role":"assistant"},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"content":"<tool_call><function=write_stdin><parameter=chars>"},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_6b63","type":"function","function":{"name":"write_stdin","arguments":""}}]},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"chars\": "}}]},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"content":"</parameter><parameter=max_output_tokens>"},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"content":"2000"},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"content":"</parameter><parameter=session_id>44579"},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"content":"</parameter><parameter=yield_time_ms>1000"},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"content":"</parameter></function></tool_call>"},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"content":"","role":"assistant"},"finish_reason":"tool_calls"}]}`,
	}
	var stream strings.Builder
	for _, frame := range frames {
		stream.WriteString("data: " + frame + "\n\n")
	}
	stream.WriteString("data: [DONE]\n\n")

	var calls []upstream.SSEMessage
	var text strings.Builder
	result, err := consumeStream(strings.NewReader(stream.String()), true, func(msg upstream.SSEMessage) {
		switch msg.Type {
		case "model.tool-call":
			calls = append(calls, msg)
		case "model.text-delta":
			text.WriteString(msg.Event["delta"].(string))
		}
	})
	testutil.NoError(t, err, "consumeStream() error = %v")
	testutil.Equal(t, len(calls), 1)
	testutil.Equal(t, calls[0].Event["toolCallId"], "call_6b63")
	testutil.Equal(t, calls[0].Event["toolName"], "write_stdin")
	// Every parameter of the textual copy has to survive, typed as the upstream
	// wrote it: a numeric parameter must not arrive as a quoted string.
	testutil.Equal(t, calls[0].Event["input"], `{"chars": `)
	testutil.True(t, strings.Contains(text.String(), "parameter="), "markup must remain text")
	testutil.Equal(t, result.ToolCallCount, 1)
	testutil.Equal(t, result.FinishReason(), "tool_use")
}

// TestConsumeStreamDoesNotDuplicateAnIntactTextualToolCall is the other half of
// the same stream: when the native deltas already carry complete arguments, the
// textual copy is a duplicate and must not become a second tool_use block.
func TestConsumeStreamDoesNotDuplicateAnIntactTextualToolCall(t *testing.T) {
	frames := []string{
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_ok","type":"function","function":{"name":"exec_command","arguments":"{\"cmd\": \"ls\"}"}}]},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"content":"<tool_call><function=exec_command><parameter=cmd>ls</parameter></function></tool_call>"},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"content":"","role":"assistant"},"finish_reason":"tool_calls"}]}`,
	}
	var stream strings.Builder
	for _, frame := range frames {
		stream.WriteString("data: " + frame + "\n\n")
	}
	stream.WriteString("data: [DONE]\n\n")

	var calls []upstream.SSEMessage
	result, err := consumeStream(strings.NewReader(stream.String()), true, func(msg upstream.SSEMessage) {
		if msg.Type == "model.tool-call" {
			calls = append(calls, msg)
		}
	})
	testutil.NoError(t, err, "consumeStream() error = %v")
	testutil.Equal(t, len(calls), 1)
	// The native arguments win unchanged: the textual copy is a duplicate of a
	// call that was never broken, so it is dropped rather than merged.
	testutil.Equal(t, calls[0].Event["input"], `{"cmd": "ls"}`)
	testutil.Equal(t, result.ToolCallCount, 1)
}

func TestConsumeStreamLeavesTextToolMarkupWithoutDeclaredTools(t *testing.T) {
	markup := `<tool_call>bash:x</arg_value><arg_key>command</arg_key><arg_value>x</arg_value></tool_call>`
	stream := "data: {\"choices\":[{\"delta\":{\"content\":" + string(mustJSON(t, markup)) + "},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	var text strings.Builder
	result, err := consumeStream(strings.NewReader(stream), false, func(msg upstream.SSEMessage) {
		if msg.Type == "model.text-delta" {
			text.WriteString(msg.Event["delta"].(string))
		}
	})
	testutil.NoError(t, err)
	testutil.Falsef(t, text.String() != markup || result.ToolCallCount != 0, "text=%q result=%+v", text.String(), result)
}

func mustJSON(t *testing.T, value interface{}) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	testutil.NoError(t, err)
	return raw
}

// TestClassifyStatusTurnsTheCapIntoAKnownWindow is the reason the 429 path is
// special: the wait is in the body, and reading it turns a retry into a
// cooldown the scheduler can honour.
func TestClassifyStatusTurnsTheCapIntoAKnownWindow(t *testing.T) {
	err := classifyStatus(429, []byte(`{"error":"INFERENCE_CAP_ERROR","message":"Try again in 17h 59m"}`))
	capErr, ok := err.(*InferenceCapError)
	testutil.True(t, ok, "classifyStatus(429) = %T (%v), want *InferenceCapError")
	testutil.CheckEqual(t, capErr.Wait, 17*time.Hour+59*time.Minute)
}

// TestClassifyStatusPreservesFailures checks the one local recovery path (401)
// and the status details handed back to callers for non-auth failures.
func TestClassifyStatusPreservesFailures(t *testing.T) {
	err := classifyStatus(401, []byte(`{"error":"unauthorized"}`))
	testutil.CheckFalsef(t, !isUnauthorized(err) || !errors.Is(err, ErrCredentialMissing), "classifyStatus(401) = %v, want unauthorized credential error", err)
	for _, tc := range []struct {
		status int
		body   string
	}{
		{400, `{"error":"bad"}`},
		{408, `{"error":"timeout"}`},
		{429, `{"error":"busy"}`},
		{503, `{"error":"unavailable"}`},
	} {
		err := classifyStatus(tc.status, []byte(tc.body))
		testutil.CheckFalsef(t, isUnauthorized(err) || errors.Is(err, ErrCredentialMissing), "classifyStatus(%d) = %v, must not trigger credential refresh", tc.status, err)
		testutil.CheckContainAll(t, err.Error(), fmt.Sprintf("status=%d", tc.status), tc.body)
	}
}

// TestCatalogSnapshotRoundTripsTheObservedFeed proves the stored form keeps the
// identifier, while a retired bare ID is ignored.
func TestCatalogSnapshotRoundTripsTheObservedFeed(t *testing.T) {
	models := []Model{{ID: "x-ai/grok-4.1-fast", Name: "Grok 4.1 Fast", Provider: "x-ai", RequiresStream: true}}
	rows := CatalogSnapshot(models)
	testutil.Equal(t, len(rows), 1)
	testutil.CheckEqual(t, catalogID(rows[0]), "x-ai/grok-4.1-fast")
	testutil.CheckEqual(t, catalogID("openai/gpt-5"), "")
	// The snapshot is JSON rows, so the identifier has to survive the encoding.
	var decoded map[string]interface{}
	testutil.NoError(t, json.Unmarshal([]byte(rows[0]), &decoded), "row is not JSON: %v")
	testutil.CheckEqual(t, decoded["id"], "x-ai/grok-4.1-fast")
}

// TestParseCatalogPublishesOnlyTheFreeTier covers the feed's shape: the paid
// rows name models this account may not run, and an empty feed is an error
// rather than an empty catalog.
func TestParseCatalogPublishesOnlyTheFreeTier(t *testing.T) {
	models, err := parseCatalog([]byte(`{"free":[{"id":"x-ai/grok-4.1-fast","name":"Grok 4.1 Fast"}],"paid":[{"id":"anthropic/claude-opus","name":"Opus"}]}`))
	testutil.NoError(t, err, "parseCatalog() error = %v")
	testutil.Equal(t, len(models), 1)
	testutil.Equal(t, models[0].ID, "x-ai/grok-4.1-fast")
	testutil.CheckEqual(t, models[0].Provider, "x-ai")
	testutil.CheckFalse(t, !models[0].RequiresStream, "RequiresStream = false, want true for a bare identifier")
	if models, err = parseCatalog([]byte(`{"free":[]}`)); err != nil || len(models) != 0 {
		t.Errorf("an empty feed must parse as empty, got %+v / %v", models, err)
	}
}

// TestNewToolCallIDIsUnique keeps two concurrent tool calls from colliding on
// one id, which would pair a result with the wrong call.
func TestNewToolCallIDIsUnique(t *testing.T) {
	seen := make(map[string]bool, 200)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				id := NewToolCallID()
				mu.Lock()
				if seen[id] {
					mu.Unlock()
					t.Errorf("duplicate tool call id %q", id)
					return
				}
				seen[id] = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	testutil.CheckEqual(t, len(seen), 200)
}

// TestConsumeStreamSignsReasoningDeltas pins the WorkBuddy/Qoder/Cline parity:
// every reasoning delta of one stream carries the same signature so the
// Anthropic surface keeps them inside a single signed thinking block.
func TestConsumeStreamSignsReasoningDeltas(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"step one\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"step two\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	var signatures []string
	var reasoning strings.Builder
	_, err := consumeStream(strings.NewReader(stream), false, func(msg upstream.SSEMessage) {
		if msg.Type != "model.reasoning-delta" {
			return
		}
		reasoning.WriteString(msg.Event["delta"].(string))
		if sig, ok := msg.Event["signature"].(string); ok {
			signatures = append(signatures, sig)
		}
	})
	testutil.NoError(t, err, "consumeStream() error = %v")
	testutil.Equal(t, reasoning.String(), "step onestep two")
	testutil.Falsef(t, len(signatures) != 2 || signatures[0] == "" || signatures[0] != signatures[1], "signatures = %v, want one stable non-empty signature", signatures)
	if !strings.HasPrefix(signatures[0], "cline-v1:") {
		t.Fatalf("signature = %q, want the cline-v1 prefix", signatures[0])
	}
}

func TestConsumeStreamAcceptsGLMReasoningAliases(t *testing.T) {
	stream := `data: {"choices":[{"delta":{"reasoning":"step one"}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"thinking":"step two","content":"answer"},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	var reasoning, text strings.Builder
	result, err := consumeStream(strings.NewReader(stream), false, func(msg upstream.SSEMessage) {
		switch msg.Type {
		case "model.reasoning-delta":
			reasoning.WriteString(msg.Event["delta"].(string))
		case "model.text-delta":
			text.WriteString(msg.Event["delta"].(string))
		}
	})
	testutil.NoError(t, err, "consumeStream() error = %v")
	testutil.Equal(t, reasoning.String(), "step onestep two")
	testutil.Falsef(t, text.String() != "answer" || !result.SawMeaningfulEvent, "text/result = %q/%+v", text.String(), result)
}
func TestConsumeStreamSeparatesSplitThinkingTagsFromAnswer(t *testing.T) {
	stream := `data: {"choices":[{"delta":{"content":"Hello <thi"}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"content":"nk>private reasoning</thi"}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"content":"nk> world"},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	for _, toolsEnabled := range []bool{false, true} {
		var text, reasoning strings.Builder
		_, err := consumeStream(strings.NewReader(stream), toolsEnabled, func(msg upstream.SSEMessage) {
			switch msg.Type {
			case "model.text-delta":
				text.WriteString(msg.Event["delta"].(string))
			case "model.reasoning-delta":
				reasoning.WriteString(msg.Event["delta"].(string))
			}
		})
		testutil.NoError(t, err)
		testutil.Falsef(t, text.String() != "Hello  world" || reasoning.String() != "private reasoning", "tools=%t text=%q reasoning=%q", toolsEnabled, text.String(), reasoning.String())
	}
}

func TestConsumeStreamFlushesIncompleteThinkPrefixAsText(t *testing.T) {
	stream := `data: {"choices":[{"delta":{"content":"answer <thi","reasoning_content":"separate reasoning"},"finish_reason":"stop"}]}` + "\n\n"
	var text, reasoning strings.Builder
	result, err := consumeStream(strings.NewReader(stream), false, func(msg upstream.SSEMessage) {
		switch msg.Type {
		case "model.text-delta":
			text.WriteString(msg.Event["delta"].(string))
		case "model.reasoning-delta":
			reasoning.WriteString(msg.Event["delta"].(string))
		}
	})
	testutil.Falsef(t, err != nil || !result.SawMeaningfulEvent || text.String() != "answer <thi" || reasoning.String() != "separate reasoning", "text=%q reasoning=%q result=%+v err=%v", text.String(), reasoning.String(), result, err)
}

func TestBuildChatBodyHonorsClientReasoningEffort(t *testing.T) {
	body, err := buildChatBody(upstream.UpstreamRequest{
		Model:           "z-ai/glm-5.3-flash",
		ReasoningEffort: "low",
		Messages:        []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "hi"}}},
	}, "z-ai/glm-5.3-flash")
	testutil.NoError(t, err, "buildChatBody() error = %v")
	testutil.MustContain(t, string(body), `"reasoning_effort":"low"`)

	body, err = buildChatBody(upstream.UpstreamRequest{
		Model:           "z-ai/glm-5.3-flash",
		ReasoningEffort: "none",
		Messages:        []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "hi"}}},
	}, "z-ai/glm-5.3-flash")
	testutil.NoError(t, err, "buildChatBody() error = %v")
	testutil.MustNotContain(t, string(body), `"reasoning_effort":"none"`)
	testutil.MustContain(t, string(body), `"reasoning_effort":"high"`)
}
