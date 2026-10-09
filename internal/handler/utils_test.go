package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"encoding/json"

	"orchids-api/internal/middleware"
	"orchids-api/internal/prompt"
	"orchids-api/internal/testutil"
)

func TestConversationKeyForRequestPriority(t *testing.T) {
	baseReq := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, "http://example.com/workbuddy/v1/messages", nil)
		r.RemoteAddr = "203.0.113.9:1234"
		r.Header.Set("User-Agent", "test-agent")
		return r
	}

	tests := []struct {
		name       string
		req        ClaudeRequest
		headerKey  string
		headerVal  string
		remoteAddr string
		userAgent  string
		want       string
	}{
		{
			name: "conversation_id highest priority",
			req: ClaudeRequest{
				ConversationID: "cid",
				Metadata:       map[string]interface{}{"user_id": "u1"},
			},
			headerKey: "X-Conversation-Id",
			headerVal: "header",
			want:      "cid",
		},
		{
			name: "metadata conversation_id before header",
			req: ClaudeRequest{
				Metadata: map[string]interface{}{"conversation_id": "meta"},
			},
			headerKey: "X-Conversation-Id",
			headerVal: "header",
			want:      "meta",
		},
		{
			name: "header before metadata user_id",
			req: ClaudeRequest{
				Metadata: map[string]interface{}{"user_id": "u1"},
			},
			headerKey: "X-Conversation-Id",
			headerVal: "header",
			want:      "header",
		},
		{
			name: "no explicit session key returns empty",
			req: ClaudeRequest{
				Metadata: map[string]interface{}{"user_id": "u1"},
			},
			want: "u1",
		},
		{
			name: "no fallback to host and user agent",
			req:  ClaudeRequest{},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := baseReq()
			if tt.headerKey != "" {
				r.Header.Set(tt.headerKey, tt.headerVal)
			}
			if tt.remoteAddr != "" {
				r.RemoteAddr = tt.remoteAddr
			}
			if tt.userAgent != "" {
				r.Header.Set("User-Agent", tt.userAgent)
			}
			testutil.Equal(t, conversationKeyForRequest(r, tt.req), tt.want)
		})
	}
}

func TestExplicitConversationID_AcceptsCamelCaseJSON(t *testing.T) {
	t.Parallel()

	var payload ClaudeRequest
	testutil.NoError(t, json.Unmarshal([]byte(`{"conversationId":"conv-camel"}`), &payload))
	req := httptest.NewRequest(http.MethodPost, "http://example.com/workbuddy/v1/messages", nil)
	testutil.Equal(t, explicitConversationID(req, payload), "conv-camel")
}

func TestWorkBuddyConversationRequestID_PrefersInboundAggregationHeader(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodPost, "http://example.com/workbuddy/v1/messages", nil)
	req.Header.Set("X-Conversation-Request-ID", "client-req-1")
	testutil.Equal(t, workBuddyConversationRequestID(req), "client-req-1")

	// A request without that header falls back to the trace middleware's stable
	// request identity. Run the middleware because its context key is private.
	var got string
	middleware.TraceMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = workBuddyConversationRequestID(r)
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodPost, "http://example.com", nil))
	testutil.Equal(t, len(got), 32)
}

func TestChannelFromPath(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{path: "/workbuddy/v1/messages", want: "workbuddy"},
		{path: "/grok/v1/chat/completions", want: "grok"},
		{path: "/v1/messages", want: ""},
	}
	for _, tt := range tests {
		testutil.Equal(t, channelFromPath(tt.path), tt.want)
	}
}

func TestIsTopicClassifierRequest(t *testing.T) {
	req := ClaudeRequest{
		System: SystemItems{
			{
				Type: "text",
				Text: "Analyze if this message indicates a new conversation topic. Format your response as a JSON object with two fields: 'isNewTopic' and 'title'.",
			},
		},
	}
	testutil.True(t, isTopicClassifierRequest(req), "expected topic classifier request to be detected")

	nonClassifier := ClaudeRequest{
		System: SystemItems{{Type: "text", Text: "You are Claude Code"}},
	}
	testutil.False(t, isTopicClassifierRequest(nonClassifier), "expected non-topic-classifier request")
}

func TestIsTitleGenerationRequest(t *testing.T) {
	req := ClaudeRequest{
		System: SystemItems{
			{Type: "text", Text: "You are Claude Code, Anthropic's official CLI for Claude."},
			{
				Type: "text",
				Text: "Generate a concise, sentence-case title (3-7 words) that captures the main topic or goal of this coding session.\n\nReturn JSON with a single \"title\" field.",
			},
		},
	}
	testutil.True(t, isTitleGenerationRequest(req), "expected title generation request to be detected")

	nonTitle := ClaudeRequest{
		System: SystemItems{{
			Type: "text",
			Text: "Analyze if this message indicates a new conversation topic. Format your response as a JSON object with two fields: 'isNewTopic' and 'title'.",
		}},
	}
	testutil.False(t, isTitleGenerationRequest(nonTitle), "expected non-title-generation request")
}

func TestClassifyTopicRequest(t *testing.T) {
	tests := []struct {
		name      string
		messages  []prompt.Message
		wantIsNew bool
	}{
		{
			name:      "single user message treated as new topic",
			messages:  []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "帮我用python写一个计算器"}}},
			wantIsNew: true,
		},
		{
			name: "same user message treated as same topic",
			messages: []prompt.Message{
				{Role: "user", Content: prompt.MessageContent{Text: "帮我用python写一个计算器"}},
				{Role: "assistant", Content: prompt.MessageContent{Text: "好的"}},
				{Role: "user", Content: prompt.MessageContent{Text: "帮我用python写一个计算器"}},
			},
			wantIsNew: false,
		},
		{
			name: "greeting treated as same topic",
			messages: []prompt.Message{
				{Role: "user", Content: prompt.MessageContent{Text: "帮我用python写一个计算器"}},
				{Role: "assistant", Content: prompt.MessageContent{Text: "好的"}},
				{Role: "user", Content: prompt.MessageContent{Text: "hi"}},
			},
			wantIsNew: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := ClaudeRequest{Messages: tt.messages}
			gotNew, title := classifyTopicRequest(req)
			testutil.Equal(t, gotNew, tt.wantIsNew)
			testutil.False(t, gotNew && strings.TrimSpace(title) == "", "expected non-empty title for new topic")
			testutil.Falsef(t, !gotNew && title != "", "expected empty title when not a new topic, got %q", title)
		})
	}
}

func TestBuildLocalSuggestion(t *testing.T) {
	tests := []struct {
		name     string
		messages []prompt.Message
		want     string
	}{
		{
			name: "chinese follow up offer returns chinese suggestion",
			messages: []prompt.Message{
				{Role: "user", Content: prompt.MessageContent{Text: "继续处理这个问题"}},
				{Role: "assistant", Content: prompt.MessageContent{Text: "已经定位完了。如果你要，我下一步可以直接帮你提交修复。"}},
				{Role: "user", Content: prompt.MessageContent{Text: "[SUGGESTION MODE: Suggest what the user might naturally type next into Claude Code.]"}},
			},
			want: "可以",
		},
		{
			name: "non obvious next step stays silent",
			messages: []prompt.Message{
				{Role: "user", Content: prompt.MessageContent{Text: "当前运行的目录"}},
				{Role: "assistant", Content: prompt.MessageContent{Text: "当前运行目录：`/Users/dailin/Documents/GitHub/TEST`"}},
				{Role: "user", Content: prompt.MessageContent{Text: "[SUGGESTION MODE: Suggest what the user might naturally type next into Claude Code.]"}},
			},
			want: "",
		},
		{
			name: "english follow up offer returns english suggestion",
			messages: []prompt.Message{
				{Role: "user", Content: prompt.MessageContent{Text: "check the logs"}},
				{Role: "assistant", Content: prompt.MessageContent{Text: "I found the issue. If you'd like, I can restart the server and verify it."}},
				{Role: "user", Content: prompt.MessageContent{Text: "[SUGGESTION MODE: Suggest what the user might naturally type next into Claude Code.]"}},
			},
			want: "go ahead",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { testutil.Equal(t, buildLocalSuggestion(tt.messages), tt.want) })
	}
}

func TestStripSystemRemindersForMode_StripsLocalCommandMetadata(t *testing.T) {
	text := "<local-command-caveat>Caveat</local-command-caveat>\n<command-name>/model</command-name>\n<command-message>model</command-message>\n<command-args></command-args>\n<local-command-stdout>Set model to opus</local-command-stdout>\n[SUGGESTION MODE: Suggest what the user might naturally type next into Claude Code.]"
	got := stripSystemRemindersForMode(text)
	testutil.MustNotContainAny(t, got, "<local-command-caveat>", "/model", "Set model to opus")
	testutil.MustContain(t, got, "[SUGGESTION MODE: Suggest what the user might naturally type next into Claude Code.]")
}

func TestLastUserIsToolResultFollowup_AllowsTextAlongsideToolResult(t *testing.T) {
	messages := []prompt.Message{
		{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_1", Name: "Read", Input: map[string]interface{}{"file_path": "utils.py"}}}}},
		{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{
			{Type: "tool_result", ToolUseID: "tool_1", Content: "import flask"},
			{Type: "text", Text: "这个项目使用了哪些技术架构"},
		}}},
	}

	testutil.True(t, lastUserIsToolResultFollowup(messages), "expected tool_result+text to be recognized as follow-up")
}
