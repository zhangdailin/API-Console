package handler

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"encoding/json"

	"orchids-api/internal/audit"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/prompt"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

type fakePayloadClient struct {
	mu                  sync.Mutex
	calls               []upstream.UpstreamRequest
	conversationIDsByOp []string
	eventsByOp          [][]upstream.SSEMessage
}

func (f *fakePayloadClient) SendRequestWithPayload(ctx context.Context, req upstream.UpstreamRequest, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	idx := len(f.calls) - 1
	var convID string
	if idx >= 0 && idx < len(f.conversationIDsByOp) {
		convID = f.conversationIDsByOp[idx]
	}
	var events []upstream.SSEMessage
	if idx >= 0 && idx < len(f.eventsByOp) {
		events = f.eventsByOp[idx]
	}
	f.mu.Unlock()

	if len(events) > 0 {
		for _, event := range events {
			onMessage(event)
		}
		return nil
	}

	if convID != "" {
		onMessage(upstream.SSEMessage{
			Type:  "model.conversation_id",
			Event: map[string]any{"id": convID},
		})
	}
	onMessage(upstream.SSEMessage{
		Type:  "model.finish",
		Event: map[string]any{"finishReason": "end_turn"},
	})
	return nil
}

func (f *fakePayloadClient) snapshotCalls() []upstream.UpstreamRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]upstream.UpstreamRequest, len(f.calls))
	copy(out, f.calls)
	return out
}

func newTestHandler(client UpstreamClient) *Handler {
	return &Handler{
		config:      &config.Config{DebugEnabled: false},
		client:      client,
		auditLogger: audit.NewNopLogger(),
	}
}

func TestToolResultFollowupWorkdirAfterToolTurn_ReachesUpstream(t *testing.T) {
	t.Parallel()

	client := &fakePayloadClient{}
	h := newTestHandler(client)
	body := []byte(`{
		"model":"claude-opus-5",
		"stream":false,
		"conversation_id":"workbuddy_fresh_reset",
		"messages":[
			{"role":"user","content":[{"type":"text","text":"帮我用python写一个计算器"}]},
			{"role":"assistant","content":[
				{"type":"tool_use","id":"tool_write","name":"Write","input":{"file_path":"calculator.py","content":"print(1)"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"tool_write","content":"File created successfully at: calculator.py"}
			]},
			{"role":"user","content":[{"type":"text","text":"当前运行的目录"}]}
		],
		"tools":[
			{"name":"Read","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}},
			{"name":"Write","input_schema":{"type":"object","properties":{"file_path":{"type":"string"},"content":{"type":"string"}},"required":["file_path","content"]}}
		]
	}`)

	req := httptest.NewRequest(http.MethodPost, "/workbuddy/v1/messages", bytes.NewReader(body))
	req.Header.Set("X-Workdir", `C:\Users\zhangdailin\Desktop\新建文件夹`)
	rec := httptest.NewRecorder()

	h.HandleMessages(rec, req)
	testutil.Equal(t, rec.Code, http.StatusOK)

	calls := client.snapshotCalls()
	testutil.Falsef(t, len(calls) != 1, "upstream calls = %d, want 1: the question must be answered upstream", len(calls))
	out := rec.Body.String()
	testutil.Falsef(t, strings.Contains(out, "当前工作目录未在本次请求中提供"), "gateway still answered the workdir question locally: %s", out)
}

func TestToolResultFollowup_RecoversSandboxPathFailureWithoutNoToolsGate(t *testing.T) {
	t.Parallel()

	client := &fakePayloadClient{}
	h := newTestHandler(client)

	body := []byte(`{
		"model":"claude-opus-5",
		"stream":false,
		"conversation_id":"workbuddy_followup_recover",
		"messages":[
			{"role":"user","content":[{"type":"text","text":"这个项目是干什么的"}]},
			{"role":"assistant","content":[
				{"type":"tool_use","id":"tool_ls","name":"Bash","input":{"command":"ls /tmp/cc-agent/sb1-fxjxbmvk/project","description":"List project files"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"tool_ls","content":"Exit code 2\nls: cannot access '/tmp/cc-agent/sb1-fxjxbmvk/project': No such file or directory"},
				{"type":"text","text":"这个项目是干什么的"}
			]}
		],
		"tools":[
			{"name":"Read","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}},
			{"name":"Bash","input_schema":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}
		]
	}`)

	req := httptest.NewRequest(http.MethodPost, "/workbuddy/v1/messages", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.HandleMessages(rec, req)
	testutil.Equal(t, rec.Code, http.StatusOK)

	calls := client.snapshotCalls()
	testutil.Equal(t, len(calls), 1)
	testutil.False(t, calls[0].NoTools, "expected workbuddy follow-up after sandbox path miss to keep tools enabled")
}

func TestOpenAIChatCompletionsToolFollowup_NormalizesToolMessages(t *testing.T) {
	t.Parallel()

	client := &fakePayloadClient{}
	h := newTestHandler(client)

	body := []byte(`{
		"model":"claude-opus-5",
		"stream":false,
		"conversation_id":"workbuddy_openai_tool_followup",
		"messages":[
			{"role":"user","content":"Create note.txt with hello world"},
			{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_write_1","type":"function","function":{"name":"Write","arguments":"{\"file_path\":\"note.txt\",\"content\":\"hello world\"}"}}
			]},
			{"role":"tool","tool_call_id":"call_write_1","content":"Write succeeded: note.txt created with hello world"}
		],
		"tools":[
			{"type":"function","function":{"name":"Write","description":"Write content to a file","parameters":{"type":"object","properties":{"file_path":{"type":"string"},"content":{"type":"string"}},"required":["file_path","content"]}}}
		]
	}`)

	req := httptest.NewRequest(http.MethodPost, "/workbuddy/v1/chat/completions", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.HandleMessages(rec, req)
	testutil.Equal(t, rec.Code, http.StatusOK)

	calls := client.snapshotCalls()
	testutil.Equal(t, len(calls), 1)
	testutil.False(t, calls[0].NoTools, "expected openai workbuddy tool follow-up to keep tools enabled")

	testutil.Equal(t, len(calls[0].Messages), 3)

	assistantMsg := calls[0].Messages[1]
	testutil.Equal(t, assistantMsg.Role, "assistant")
	testutil.False(t, assistantMsg.Content.IsString(), "expected assistant tool call message to normalize into content blocks")
	assistantBlocks := assistantMsg.Content.GetBlocks()
	testutil.Equal(t, len(assistantBlocks), 1)
	if assistantBlocks[0].Type != "tool_use" || assistantBlocks[0].Name != "Write" || assistantBlocks[0].ID != "call_write_1" {
		t.Fatalf("unexpected assistant tool_use block: %#v", assistantBlocks[0])
	}
	input, ok := assistantBlocks[0].Input.(map[string]any)
	testutil.True(t, ok, "assistant tool input type = %T, want map[string]interface{}")
	testutil.Equal(t, input["file_path"], "note.txt")
	testutil.Equal(t, input["content"], "hello world")

	toolResultMsg := calls[0].Messages[2]
	testutil.Equal(t, toolResultMsg.Role, "user")
	testutil.False(t, toolResultMsg.Content.IsString(), "expected tool result follow-up to normalize into content blocks")
	resultBlocks := toolResultMsg.Content.GetBlocks()
	testutil.Equal(t, len(resultBlocks), 1)
	testutil.Equal(t, resultBlocks[0].Type, "tool_result")
	testutil.Equal(t, resultBlocks[0].ToolUseID, "call_write_1")
	got, ok := resultBlocks[0].Content.(string)
	testutil.Falsef(t, !ok || !strings.Contains(got, "Write succeeded"), "tool_result content = %#v", resultBlocks[0].Content)
}

func TestToolResultFollowup_PassesThroughUpstreamInsteadOfLocalFallback(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		body        []byte
		fullHandler bool
	}{
		{name: "bash_directory_result", body: []byte(`{
		"model":"claude-opus-5",
		"stream":false,
		"conversation_id":"workbuddy_followup_local_fallback",
		"messages":[
			{"role":"user","content":[{"type":"text","text":"这个项目是干什么的"}]},
			{"role":"assistant","content":[
				{"type":"tool_use","id":"tool_ls","name":"Bash","input":{"command":"ls -la","description":"List project files"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"tool_ls","content":"README.md\napi.py\ndashboard.py\nweb-ui/\nweb-ui/package.json\nweb-ui/src/\nrequirements.txt"}
			]}
		],
		"tools":[
			{"name":"Read","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}},
			{"name":"Bash","input_schema":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}
		]
	}`)},
		{name: "read_result_with_text", fullHandler: true, body: []byte(`{
			"model":"claude-3-5-sonnet","conversation_id":"read-followup","stream":false,"system":[],
			"messages":[
				{"role":"user","content":"这个项目使用了哪些技术架构"},
				{"role":"assistant","content":[{"type":"tool_use","id":"tool_1","name":"Read","input":{"file_path":"/tmp/project/utils.py"}}]},
				{"role":"user","content":[
					{"type":"tool_result","tool_use_id":"tool_1","content":"import json\nimport os\nALERTS_FILE='alerts.json'\ndef load_json(path):\n    return json.load(open(path))"},
					{"type":"text","text":"请直接回答"}
				]}
			]
		}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			const answer = "Let me first understand the project structure and code."
			client := &fakePayloadClient{eventsByOp: [][]upstream.SSEMessage{{
				{Type: "model", Event: map[string]any{"type": "text-start"}},
				{Type: "model", Event: map[string]any{"type": "text-delta", "delta": answer}},
				{Type: "model", Event: map[string]any{"type": "finish", "finishReason": "stop"}},
			}}}
			h := newTestHandler(client)
			if tc.fullHandler {
				h = NewWithLoadBalancer(&config.Config{DebugEnabled: false, RequestTimeout: 10}, nil)
				h.client = client
			}
			rec := httptest.NewRecorder()
			h.HandleMessages(rec, httptest.NewRequest(http.MethodPost, "/workbuddy/v1/messages", bytes.NewReader(tc.body)))
			testutil.Equal(t, rec.Code, http.StatusOK)
			calls := client.snapshotCalls()
			testutil.Falsef(t, len(calls) != 1, "expected one upstream passthrough call, got %d", len(calls))
			out := rec.Body.String()
			testutil.MustContain(t, out, answer)
			for _, unwanted := range []string{"Python", "JSON", "前端", "后端", "脚本层", "当前只拿到目录概览", "基于当前已读取内容"} {
				testutil.MustNotContain(t, out, unwanted)
			}
		})
	}
}

func TestMultiTurnEditFollowup_PreservesHistory(t *testing.T) {
	t.Parallel()

	client := &fakePayloadClient{}
	h := newTestHandler(client)
	body := []byte(`{
		"model":"claude-opus-5",
		"stream":false,
		"conversation_id":"workbuddy_multiturn_scientific_notation",
		"messages":[
			{"role":"user","content":[{"type":"text","text":"帮我用python写一个计算器"}]},
			{"role":"assistant","content":[
				{"type":"tool_use","id":"tool_write","name":"Write","input":{"file_path":"calculator.py","content":"print(1)"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"tool_write","content":"File created successfully at: calculator.py"}
			]},
			{"role":"assistant","content":[{"type":"text","text":"完成！计算器已创建在项目目录中。"}]},
			{"role":"user","content":[{"type":"text","text":"帮我添加科学计数法"}]}
		],
		"tools":[
			{"name":"Write","input_schema":{"type":"object","properties":{"file_path":{"type":"string"},"content":{"type":"string"}},"required":["file_path","content"]}},
			{"name":"Edit","input_schema":{"type":"object","properties":{"file_path":{"type":"string"},"old_string":{"type":"string"},"new_string":{"type":"string"}},"required":["file_path","old_string","new_string"]}}
		]
	}`)

	req := httptest.NewRequest(http.MethodPost, "/workbuddy/v1/messages", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.HandleMessages(rec, req)
	testutil.Equal(t, rec.Code, http.StatusOK)

	calls := client.snapshotCalls()
	testutil.Equal(t, len(calls), 1)
	testutil.Equal(t, len(calls[0].Messages), 5)
	testutil.Equal(t, calls[0].Messages[0].ExtractText(), "帮我用python写一个计算器")
	testutil.Equal(t, calls[0].Messages[3].ExtractText(), "完成！计算器已创建在项目目录中。")
	testutil.Equal(t, calls[0].Messages[4].ExtractText(), "帮我添加科学计数法")
}

func TestWorkBuddyPassthrough_DoesNotTrimMessagesOrSanitizeSystem(t *testing.T) {
	t.Parallel()

	client := &fakePayloadClient{}
	h := &Handler{
		config:      &config.Config{DebugEnabled: false},
		client:      client,
		auditLogger: audit.NewNopLogger(),
	}

	reqPayload := ClaudeRequest{
		Model: "claude-opus-4-6",
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "m1"}},
			{Role: "assistant", Content: prompt.MessageContent{Text: "m2"}},
			{Role: "user", Content: prompt.MessageContent{Text: "m3"}},
		},
		System: []prompt.SystemItem{
			{Type: "text", Text: "You are Claude Code, Anthropic's official CLI for Claude."},
			{Type: "text", Text: "cc_entrypoint=claude-code; keep=this"},
		},
		Stream: false,
		Tools:  []any{},
	}

	body, err := json.Marshal(reqPayload)
	testutil.NoError(t, err, "marshal request: %v")
	req := httptest.NewRequest(http.MethodPost, "/workbuddy/v1/messages", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.HandleMessages(rec, req)
	testutil.Equal(t, rec.Code, http.StatusOK)

	calls := client.snapshotCalls()
	testutil.Equal(t, len(calls), 1)

	testutil.Equal(t, len(calls[0].Messages), len(reqPayload.Messages))
	testutil.Equal(t, len(calls[0].System), len(reqPayload.System))
	testutil.MustContain(t, calls[0].System[0].Text, "Claude Code")
	testutil.MustContain(t, calls[0].System[1].Text, "cc_entrypoint=claude-code")
}

func TestToolResultFollowup_RepeatedWriteIsForwarded(t *testing.T) {
	t.Parallel()

	client := &fakePayloadClient{
		eventsByOp: [][]upstream.SSEMessage{
			{
				{
					Type: "model.tool-call",
					Event: map[string]any{
						"toolCallId": "tool_new_1",
						"toolName":   "Write",
						"input":      `{"file_path":"scratch.txt","content":"alpha\nbeta\n"}`,
					},
				},
				{Type: "model.finish", Event: map[string]any{"finishReason": "tool_use"}},
			},
		},
	}
	h := newTestHandler(client)

	body := []byte(`{
		"model":"claude-opus-4-6",
		"stream":false,"conversation_id":"test-conversation",
		"messages":[
			{
				"role":"user",
				"content":[{"type":"text","text":"Create scratch.txt with alpha and beta"}]
			},
			{
				"role":"assistant",
				"content":[
					{"type":"tool_use","id":"tool_old_1","name":"Write","input":{"file_path":"scratch.txt","content":"alpha\nbeta\n"}}
				]
			},
			{
				"role":"user",
				"content":[{"type":"tool_result","tool_use_id":"tool_old_1","content":"Done"}]
			}
		],
		"tools":[
			{
				"name":"Write",
				"description":"Write a file",
				"input_schema":{
					"type":"object",
					"properties":{
						"file_path":{"type":"string"},
						"content":{"type":"string"}
					},
					"required":["file_path","content"]
				}
			}
		]
	}`)

	req := httptest.NewRequest(http.MethodPost, "/workbuddy/v1/messages", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.HandleMessages(rec, req)
	testutil.Equal(t, rec.Code, http.StatusOK)

	out := rec.Body.String()
	testutil.MustNotContain(t, out, "No output was presented to the user")
	testutil.MustNotContain(t, out, "duplicate mutating tool call was suppressed")
	testutil.MustContainAll(t, out, "tool_new_1", `"name":"Write"`)

}

func TestToolResultFollowup_SendsAllCurrentTurnResultsInOneRequest(t *testing.T) {
	t.Parallel()

	client := &fakePayloadClient{}
	h := newTestHandler(client)

	body := []byte(`{
		"model":"claude-opus-4-6",
		"stream":false,
		"conversation_id":"local_conversation_key_split",
		"messages":[
			{"role":"user","content":[{"type":"text","text":"帮我优化一下这个项目"}]},
			{"role":"assistant","content":[
				{"type":"tool_use","id":"tool_ls","name":"Bash","input":{"command":"ls -la /Users/dailin/Documents/GitHub/truth_social_scraper"}},
				{"type":"tool_use","id":"tool_api","name":"Read","input":{"file_path":"/Users/dailin/Documents/GitHub/truth_social_scraper/api.py"}},
				{"type":"tool_use","id":"tool_utils","name":"Read","input":{"file_path":"/Users/dailin/Documents/GitHub/truth_social_scraper/utils.py"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"tool_ls","content":"README.md\napi.py\nutils.py"},
				{"type":"tool_result","tool_use_id":"tool_api","content":"from fastapi import FastAPI\napp = FastAPI()"},
				{"type":"tool_result","tool_use_id":"tool_utils","content":"import json\nALERTS_FILE='alerts.json'"},
				{"type":"text","text":"帮我优化一下这个项目"}
			]}
		],
		"tools":[]
	}`)

	req := httptest.NewRequest(http.MethodPost, "/workbuddy/v1/messages", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.HandleMessages(rec, req)
	testutil.Equal(t, rec.Code, http.StatusOK)

	calls := client.snapshotCalls()
	testutil.Equal(t, len(calls), 1)

	countToolResults := func(msgs []prompt.Message) int {
		total := 0
		for _, msg := range msgs {
			for _, block := range msg.Content.Blocks {
				if block.Type == "tool_result" {
					total++
				}
			}
		}
		return total
	}

	testutil.Equal(t, countToolResults(calls[0].Messages), 3)
	testutil.Equal(t, strings.TrimSpace(calls[0].Messages[len(calls[0].Messages)-1].ExtractText()), "帮我优化一下这个项目")
}

func TestToolResultFollowup_StreamsSingleBatchedResponse(t *testing.T) {
	t.Parallel()

	client := &fakePayloadClient{
		eventsByOp: [][]upstream.SSEMessage{
			{
				{Type: "model.conversation_id", Event: map[string]any{"id": "workbuddy_conv_batch"}},
				{Type: "model.text-delta", Event: map[string]any{"delta": "Let me dig into the rest of the codebase first."}},
				{
					Type: "model.tool-call",
					Event: map[string]any{
						"toolCallId": "tool_visible",
						"toolName":   "Read",
						"input":      `{"file_path":"/Users/dailin/Documents/GitHub/truth_social_scraper/monitor_trump.py"}`,
					},
				},
				{Type: "model.finish", Event: map[string]any{"finishReason": "tool_use"}},
			},
		},
	}
	h := newTestHandler(client)

	body := []byte(`{
		"model":"claude-opus-4-6",
		"stream":true,
		"conversation_id":"local_conversation_key_intermediate",
		"messages":[
			{"role":"user","content":[{"type":"text","text":"帮我优化一下这个项目"}]},
			{"role":"assistant","content":[
				{"type":"tool_use","id":"tool_ls","name":"Bash","input":{"command":"ls -la /Users/dailin/Documents/GitHub/truth_social_scraper"}},
				{"type":"tool_use","id":"tool_api","name":"Read","input":{"file_path":"/Users/dailin/Documents/GitHub/truth_social_scraper/api.py"}},
				{"type":"tool_use","id":"tool_utils","name":"Read","input":{"file_path":"/Users/dailin/Documents/GitHub/truth_social_scraper/utils.py"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"tool_ls","content":"README.md\napi.py\nutils.py"},
				{"type":"tool_result","tool_use_id":"tool_api","content":"from fastapi import FastAPI\napp = FastAPI()"},
				{"type":"tool_result","tool_use_id":"tool_utils","content":"import json\nALERTS_FILE='alerts.json'"},
				{"type":"text","text":"帮我优化一下这个项目"}
			]}
		],
		"tools":[{"name":"Read","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}}]
	}`)

	req := httptest.NewRequest(http.MethodPost, "/workbuddy/v1/messages", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.HandleMessages(rec, req)
	testutil.Equal(t, rec.Code, http.StatusOK)

	calls := client.snapshotCalls()
	testutil.Equal(t, len(calls), 1)

	out := rec.Body.String()
	testutil.MustContain(t, out, "Let me dig into the rest of the codebase first.")
	testutil.MustContain(t, out, "monitor_trump.py")
}
