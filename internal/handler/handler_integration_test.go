package handler

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"encoding/json"

	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

type mockUpstream struct {
	events       []upstream.SSEMessage
	eventBatches [][]upstream.SSEMessage
	capturedReqs []upstream.UpstreamRequest
}

type panicUpstream struct{}

func (m *mockUpstream) SendRequestWithPayload(ctx context.Context, req upstream.UpstreamRequest, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	m.capturedReqs = append(m.capturedReqs, req)
	events := m.events
	if len(m.eventBatches) > 0 {
		idx := len(m.capturedReqs) - 1
		if idx >= len(m.eventBatches) {
			idx = len(m.eventBatches) - 1
		}
		events = m.eventBatches[idx]
	}
	for _, e := range events {
		onMessage(e)
	}
	return nil
}

func (p *panicUpstream) SendRequestWithPayload(ctx context.Context, req upstream.UpstreamRequest, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	panic("unexpected upstream request")
}

// A question about the working directory used to be answered by the gateway
// itself, before the upstream was contacted at all, even when the caller had
// supplied no workdir. The message the operator saw was therefore the gateway's
// canned "not provided" line rather than the agent's own answer. The gateway no
// longer models a workdir, so the request must reach upstream verbatim.
func TestHandleMessages_WorkdirQuestionReachesUpstream(t *testing.T) {
	for _, tc := range []struct {
		name     string
		path     string
		question string
	}{
		{name: "anthropic", path: "/workbuddy/v1/messages", question: "当前运行的目录"},
		{name: "openai", path: "/workbuddy/v1/chat/completions", question: "workspace path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{DebugEnabled: false, RequestTimeout: 10}
			h := NewWithLoadBalancer(cfg, nil)
			client := &relayRecordingClient{}
			h.client = client

			body, _ := json.Marshal(map[string]any{
				"model":    "claude-3-5-sonnet",
				"messages": []map[string]any{{"role": "user", "content": tc.question}},
				"system":   []any{},
				"stream":   false,
			})

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "http://x"+tc.path, bytes.NewReader(body))
			// A workdir header is now inert: it must not change what is answered.
			req.Header.Set("X-Workdir", `C:\Users\zhangdailin\Desktop\新建文件夹`)
			h.HandleMessages(rec, req)

			testutil.Equal(t, rec.Code, 200)
			testutil.Equal(t, len(client.requests), 1)
			out := rec.Body.String()
			testutil.MustNotContain(t, out, "当前工作目录未在本次请求中提供")
			testutil.MustContain(t, out, "upstream answer")
		})
	}
}

func TestHandleMessages_ExplicitConversationIDForwardsAcrossTurns(t *testing.T) {
	cfg := &config.Config{DebugEnabled: false, RequestTimeout: 10}
	h := NewWithLoadBalancer(cfg, nil)
	up := &mockUpstream{events: []upstream.SSEMessage{
		{Type: "model", Event: map[string]any{"type": "conversation_id", "id": "conv1"}},
		{Type: "model", Event: map[string]any{"type": "text-start"}},
		{Type: "model", Event: map[string]any{"type": "text-delta", "delta": "workbuddy-hi"}},
		{Type: "model", Event: map[string]any{"type": "finish", "finishReason": "stop"}},
	}}
	h.client = up

	for _, id := range []string{"c1", "conv1"} {
		payload := map[string]any{
			"model":           "claude-opus-4-5",
			"messages":        []map[string]any{{"role": "user", "content": "hi"}},
			"system":          []any{},
			"stream":          false,
			"conversation_id": id,
		}
		body, _ := json.Marshal(payload)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "http://x/workbuddy/v1/messages", bytes.NewReader(body))
		h.HandleMessages(rec, req)
		testutil.Equal(t, rec.Code, http.StatusOK)
		var response struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		err := json.Unmarshal(rec.Body.Bytes(), &response)
		testutil.CheckNoError(t, err)
		testutil.Falsef(t, response.Type != "message" || len(response.Content) != 1 || response.Content[0].Type != "text" || response.Content[0].Text != "workbuddy-hi", "conversation_id=%q: unexpected protocol response: %+v", id, response)
		testutil.Falsef(t, len(up.capturedReqs) == 0 || up.capturedReqs[len(up.capturedReqs)-1].ConversationID != id, "conversation_id=%q: upstream request did not forward the explicit id", id)
	}
	testutil.Equal(t, len(up.capturedReqs), 2)
}

func TestHandleMessages_WorkBuddy_StreamAndJSON(t *testing.T) {
	cfg := &config.Config{DebugEnabled: false, RequestTimeout: 10}
	h := NewWithLoadBalancer(cfg, nil)
	h.client = &mockUpstream{events: []upstream.SSEMessage{
		{Type: "model", Event: map[string]any{"type": "text-start"}},
		{Type: "model", Event: map[string]any{"type": "text-delta", "delta": "workbuddy-hi"}},
		{Type: "model", Event: map[string]any{"type": "finish", "finishReason": "stop"}},
	}}

	mkBody := func(stream bool) []byte {
		payload := map[string]any{
			"model":    "claude-opus-4-5",
			"messages": []map[string]any{{"role": "user", "content": "hi"}},
			"system":   []any{},
			"stream":   stream,
		}
		b, _ := json.Marshal(payload)
		return b
	}

	{
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "http://x/workbuddy/v1/messages", bytes.NewReader(mkBody(false)))
		h.HandleMessages(rec, req)
		testutil.Equal(t, rec.Code, 200)
		testutil.MustContain(t, rec.Body.String(), "workbuddy-hi")
	}

	{
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "http://x/workbuddy/v1/messages", bytes.NewReader(mkBody(true)))
		h.HandleMessages(rec, req)
		out := rec.Body.String()
		testutil.MustContain(t, out, "workbuddy-hi")
	}
}

func TestHandleMessages_ForwardsAndEnforcesToolControls(t *testing.T) {
	parallel := false
	up := &mockUpstream{events: []upstream.SSEMessage{{
		Type: "model.finish", Event: map[string]any{"finishReason": "end_turn"},
	}}}
	h := NewWithLoadBalancer(&config.Config{RequestTimeout: 10}, nil)
	h.client = up
	tool := map[string]any{"name": "read", "input_schema": map[string]any{"type": "object"}}

	send := func(choice any) upstream.UpstreamRequest {
		body, _ := json.Marshal(map[string]any{
			"model": "claude-opus-5", "messages": []map[string]any{{"role": "user", "content": "hi"}},
			"tools": []any{tool}, "tool_choice": choice, "parallel_tool_calls": parallel, "stream": false,
		})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "http://x/workbuddy/v1/messages", bytes.NewReader(body))
		h.HandleMessages(rec, req)
		testutil.Equal(t, rec.Code, http.StatusOK)
		return up.capturedReqs[len(up.capturedReqs)-1]
	}

	forwarded := send(map[string]any{"type": "tool", "name": "read"})
	choice, _ := forwarded.ToolChoice.(map[string]any)
	testutil.Falsef(t, choice["type"] != "tool" || choice["name"] != "read" || forwarded.ParallelToolCalls == nil || *forwarded.ParallelToolCalls, "tool controls were not forwarded: %#v", forwarded)
	testutil.Falsef(t, forwarded.NoTools || len(forwarded.Tools) != 1, "enabled tools were unexpectedly gated: %#v", forwarded)

	disabled := send("none")
	testutil.Falsef(t, !disabled.NoTools || len(disabled.Tools) != 0, "tool_choice=none was not enforced: %#v", disabled)
}

func TestHandleMessages_WorkBuddy_PreservesContentByDefault(t *testing.T) {
	cfg := &config.Config{DebugEnabled: false, RequestTimeout: 10}
	up := &mockUpstream{events: []upstream.SSEMessage{
		{Type: "model", Event: map[string]any{"type": "text-start"}},
		{Type: "model", Event: map[string]any{"type": "text-delta", "delta": "ok"}},
		{Type: "model", Event: map[string]any{"type": "finish", "finishReason": "stop"}},
	}}
	h := NewWithLoadBalancer(cfg, nil)
	h.client = up

	body, _ := json.Marshal(map[string]any{
		"model": "claude-haiku-4-5-20251001",
		"messages": []map[string]any{
			{
				"role": "user",
				"content": []map[string]any{
					{"type": "text", "text": "<system-reminder>\n# currentDate\nToday's date is 2026-03-27.\n</system-reminder>"},
					{"type": "text", "text": "帮我添加 我是大帅比"},
				},
			},
		},
		"system": []map[string]any{
			{"type": "text", "text": "x-anthropic-billing-header: cc_version=2.1.85.351; cc_entrypoint=cli; cch=5e896;"},
			{"type": "text", "text": "You are Claude Code, Anthropic's official CLI for Claude."},
			{"type": "text", "text": "# Environment\n - Primary working directory: C:\\Users\\zhangdailin\\Desktop\\11112\n\ngitStatus:\nD .claude/settings.local.json\n D calculator.py\n?? test.txt\n\nRecent commits:\nd57a860 add .claude settings"},
		},
		"stream": false,
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://x/workbuddy/v1/messages", bytes.NewReader(body))
	h.HandleMessages(rec, req)
	testutil.Equal(t, rec.Code, 200)
	testutil.Equal(t, len(up.capturedReqs), 1)
	got := up.capturedReqs[0].Messages[0].ExtractText()
	testutil.MustContainAll(t, got, "<system-reminder>", "帮我添加 我是大帅比")
	testutil.Equal(t, len(up.capturedReqs[0].System), 3)
	for _, want := range []string{"cc_entrypoint=cli", "Claude Code", "gitStatus:", "cc_version=2.1.85.351"} {
		found := false
		for _, item := range up.capturedReqs[0].System {
			if strings.Contains(item.Text, want) {
				found = true
				break
			}
		}
		testutil.True(t, found, "fidelity: system item %q was dropped/rewritten")
	}
}

func TestHandleMessages_WorkBuddy_OpenAIToolCall_StreamAndJSON(t *testing.T) {
	cfg := &config.Config{DebugEnabled: false, RequestTimeout: 10}
	h := NewWithLoadBalancer(cfg, nil)
	h.client = &mockUpstream{events: []upstream.SSEMessage{
		{Type: "model.tool-call", Event: map[string]any{
			"toolCallId": "tool_write_openai_style_1",
			"toolName":   "Write",
			"input":      `{"file_path":"note.txt","content":"alpha beta"}`,
		}},
		{Type: "model.finish", Event: map[string]any{
			"finishReason": "tool_use",
			"usage": map[string]any{
				"inputTokens":  12,
				"outputTokens": 7,
			},
		}},
	}}

	mkBody := func(stream bool) []byte {
		body, _ := json.Marshal(map[string]any{
			"model":    "claude-opus-4-5",
			"messages": []map[string]any{{"role": "user", "content": "use the Write tool"}},
			"system":   []any{},
			"stream":   stream,
		})
		return body
	}

	{
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "http://x/workbuddy/v1/messages", bytes.NewReader(mkBody(false)))
		h.HandleMessages(rec, req)
		testutil.Equal(t, rec.Code, 200)
		out := rec.Body.String()
		testutil.MustContainAll(t, out, `"type":"tool_use"`, `"name":"Write"`)
		testutil.MustContain(t, out, `"stop_reason":"tool_use"`)
	}

	{
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "http://x/workbuddy/v1/messages", bytes.NewReader(mkBody(true)))
		h.HandleMessages(rec, req)
		testutil.Equal(t, rec.Code, 200)
		out := rec.Body.String()
		testutil.MustContainAll(t, out, `"type":"tool_use"`, `"name":"Write"`)
		testutil.MustContain(t, out, `alpha beta`)
	}
}

func TestHandleMessages_SuggestionMode_LocalResponse(t *testing.T) {
	cfg := &config.Config{DebugEnabled: false, RequestTimeout: 10}
	h := NewWithLoadBalancer(cfg, nil)
	h.client = &panicUpstream{}

	mkBody := func(stream bool) []byte {
		payload := map[string]any{
			"model": "claude-3-5-sonnet",
			"messages": []map[string]any{
				{"role": "user", "content": "继续处理这个问题"},
				{"role": "assistant", "content": "已经定位完了。如果你要，我下一步可以直接帮你提交修复。"},
				{"role": "user", "content": "[SUGGESTION MODE: Suggest what the user might naturally type next into Claude Code.]"},
			},
			"system": []any{},
			"stream": stream,
		}
		b, _ := json.Marshal(payload)
		return b
	}

	{
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "http://x/workbuddy/v1/messages", bytes.NewReader(mkBody(false)))
		h.HandleMessages(rec, req)
		testutil.Equal(t, rec.Code, 200)
		testutil.MustContain(t, rec.Body.String(), "\"type\":\"message\"")
		testutil.MustContain(t, rec.Body.String(), "可以")
	}

	{
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "http://x/workbuddy/v1/messages", bytes.NewReader(mkBody(true)))
		h.HandleMessages(rec, req)
		testutil.Equal(t, rec.Code, 200)
		out := rec.Body.String()
		testutil.MustContainAll(t, out, "event: message_start", "event: message_stop")
		testutil.MustContain(t, out, "可以")
	}
}

func TestHandleMessages_TitleGeneration_LocalResponse(t *testing.T) {
	cfg := &config.Config{DebugEnabled: false, RequestTimeout: 10}
	h := NewWithLoadBalancer(cfg, nil)
	h.client = &panicUpstream{}

	payload := map[string]any{
		"model":    "claude-haiku-4-5-20251001",
		"messages": []map[string]any{{"role": "user", "content": "添加科学计数法"}},
		"system": []map[string]any{
			{"type": "text", "text": "You are Claude Code, Anthropic's official CLI for Claude."},
			{
				"type": "text",
				"text": "Generate a concise, sentence-case title (3-7 words) that captures the main topic or goal of this coding session.\n\nReturn JSON with a single \"title\" field.",
			},
		},
		"tools":  []any{},
		"stream": true,
	}
	body, _ := json.Marshal(payload)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://x/workbuddy/v1/messages", bytes.NewReader(body))
	h.HandleMessages(rec, req)
	testutil.Equal(t, rec.Code, 200)

	out := rec.Body.String()
	testutil.MustContainAll(t, out, "event: message_start", "event: message_stop")
	testutil.MustContain(t, out, "\"text\":\"{\\\"title\\\":\\\"添加科学计数法\\\"}\"")
}
