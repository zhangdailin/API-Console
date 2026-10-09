package grok

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"orchids-api/internal/chatwire"
	"orchids-api/internal/responses"
	"strings"
	"sync"
	"testing"
	"time"

	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func TestResolveConversationModelUsesAccountBuildCatalog(t *testing.T) {
	h, s, _ := setupValidationHandler(t)
	if err := s.CreateAccount(context.Background(), &store.Account{
		Name: "build", AccountType: "grok", GrokProvider: ProviderBuild, CredentialType: "oauth", Enabled: true,
		OAuthAccessToken: "token", GrokModels: []string{"grok-future-account-model"}, GrokModelsSyncedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	spec, ok := h.resolveConversationModel(context.Background(), "grok-future-account-model")
	testutil.Falsef(t, !ok || spec.Upstream != UpstreamCLI || spec.UpstreamModel != "grok-future-account-model", "dynamic spec=%#v,%v", spec, ok)
	_, ok = h.resolveConversationModel(context.Background(), "arbitrary-unadvertised-model")
	testutil.False(t, ok, "unadvertised model must not become an upstream probe")
	if err := s.CreateModel(context.Background(), &store.Model{
		Channel: "grok", ModelID: "grok-future-account-model", Name: "future",
		Status: store.ModelStatusOffline, Verified: true,
	}); err != nil {
		t.Fatal(err)
	}
	err := h.ensureResolvedModelEnabled(context.Background(), spec.ID, spec)
	testutil.Error(t, err)
}

func TestResponsesStreamTranslationIsIncremental(t *testing.T) {
	reader, writer := io.Pipe()
	recorder := newObservedStreamWriter("response.output_text.delta")
	done := make(chan struct{})
	go func() {
		writeResponsesStreamFromChatReaderRequestWithHook(recorder, responses.CreateRequest{Model: "grok-4.6"}, reader, nil)
		close(done)
	}()

	_, _ = io.WriteString(writer, "data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n")
	select {
	case <-recorder.observed:
	case <-time.After(time.Second):
		t.Fatal("first Responses delta was buffered until stream completion")
	}
	_ = writer.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("translator did not finish")
	}
}

func TestResponsesStreamAggregatesFragmentedToolArguments(t *testing.T) {
	raw := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"x\\\":\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"1}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	recorder := httptest.NewRecorder()
	writeResponsesStreamFromChatReaderRequestWithHook(recorder, responses.CreateRequest{Model: "grok-4.6"}, strings.NewReader(raw), nil)
	body := recorder.Body.String()
	testutil.Equal(t, strings.Count(body, "event: response.output_item.added"), 1)
	testutil.MustContain(t, body, `"arguments":"{\"x\":1}"`)
}

type observedStreamWriter struct {
	header   http.Header
	mu       sync.Mutex
	body     bytes.Buffer
	needle   string
	observed chan struct{}
	once     sync.Once
}

func newObservedStreamWriter(needle string) *observedStreamWriter {
	return &observedStreamWriter{header: make(http.Header), needle: needle, observed: make(chan struct{})}
}

func (w *observedStreamWriter) Header() http.Header { return w.header }
func (w *observedStreamWriter) WriteHeader(int)     {}
func (w *observedStreamWriter) Flush()              {}
func (w *observedStreamWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.body.Write(data)
	if strings.Contains(w.body.String(), w.needle) {
		w.once.Do(func() { close(w.observed) })
	}
	return n, err
}

func TestAnthropicAdvancedFieldsAndReasoningReplayArePreserved(t *testing.T) {
	req := anthropicMessagesRequest{
		Model: "grok-4.6", MaxTokens: 4096,
		Thinking:      map[string]interface{}{"type": "enabled", "budget_tokens": float64(2048)},
		StopSequences: []string{"END"},
		OutputConfig: map[string]interface{}{"effort": "high", "format": map[string]interface{}{
			"type": "json_schema", "schema": map[string]interface{}{"type": "object"},
		}},
		MCPServers: []map[string]interface{}{{"name": "example", "url": "https://example.test/mcp"}},
		Metadata:   map[string]interface{}{"session_id": "claude-session"},
		Messages: []anthropicMessage{
			{Role: "assistant", Content: []interface{}{map[string]interface{}{
				"type": "thinking", "thinking": "private plan", "signature": "opaque-cipher",
			}, map[string]interface{}{"type": "text", "text": "previous answer"}}},
			{Role: "user", Content: "continue"},
		},
	}
	chat, err := anthropicRequestToChat(req)
	testutil.NoError(t, err, "anthropicRequestToChat() error = %v")
	testutil.Falsef(t, chat.ReasoningEffort == nil || *chat.ReasoningEffort != "high" || chat.PromptCacheKey != "claude-session", "reasoning/session not preserved: %#v", chat)
	testutil.Falsef(t, len(chat.Stop) != 1 || chat.Stop[0] != "END" || len(chat.ResponsesTools) != 1 || len(chat.ResponseText) == 0, "advanced fields not preserved: %#v", chat)
	testutil.Equal(t, chat.Messages[0].ReasoningContent, "private plan")
	testutil.Equal(t, chat.Messages[0].ReasoningEncryptedContent, "opaque-cipher")
}

func TestReasoningReplayIsModelAndSessionIsolated(t *testing.T) {
	h := &Handler{affinity: map[string]sessionAffinityEntry{}, replay: map[string]reasoningReplayEntry{}}
	replayCipher := func(model, key string) string {
		items := h.loadReasoningReplayItems(model, key)
		if len(items) != 1 {
			return ""
		}
		item, _ := items[0].(map[string]interface{})
		return interfaceString(item["encrypted_content"])
	}
	h.storeReasoningReplay("grok-4.6", "session-a", validTestReplayCipher())
	testutil.Equal(t, replayCipher("grok-4.6", "session-a"), validTestReplayCipher())
	testutil.Equal(t, replayCipher("grok-4.5", "session-a"), "")
	testutil.Equal(t, replayCipher("grok-4.6", "session-b"), "")

	payload := map[string]interface{}{"input": []interface{}{map[string]interface{}{"role": "user", "content": "continue"}}}
	h.applyNativeReasoningReplay("grok-4.6", "session-a", payload)
	input := payload["input"].([]interface{})
	testutil.Equal(t, len(input), 2)
	testutil.EqualAny(t, input[0].(map[string]interface{})["encrypted_content"], validTestReplayCipher())
}

func TestReasoningReplayUsesPortableShapeAndCanBeStripped(t *testing.T) {
	h := &Handler{}
	h.storeReasoningReplay("grok-4.6", "session-a", validTestReplayCipher())
	req := &chatwire.Request{Model: "grok-4.6", PromptCacheKey: "session-a", ReasoningReplay: true, Messages: []chatwire.Message{{Role: "user", Content: "next"}}}
	payload, err := h.responsesPayloadFromChat(ModelSpec{UpstreamModel: "grok-4.6"}, req, true)
	testutil.NoError(t, err)
	input := payload["input"].([]interface{})
	reasoning := input[0].(map[string]interface{})
	_, exists := reasoning["content"]
	testutil.Falsef(t, exists, "replay contains non-portable content field: %#v", reasoning)
	if !stripInjectedReasoningReplay(payload) || len(payload["input"].([]interface{})) != 1 {
		t.Fatalf("replay was not stripped: %#v", payload["input"])
	}
	stripped := payload["input"].([]interface{})[0].(map[string]interface{})
	_, exists = stripped["encrypted_content"]
	testutil.False(t, exists, "encrypted replay content was not removed")
	testutil.False(t, !isReasoningReplayDecodeError(newCLIUpstreamError(400, nil, []byte("Could not decode the compaction blob"))), "compaction decode error was not recognized")
}

func TestNativeReasoningReplayConvertsStringInput(t *testing.T) {
	h := &Handler{affinity: map[string]sessionAffinityEntry{}, replay: map[string]reasoningReplayEntry{}}
	h.storeReasoningReplay("grok-4.6", "session", validTestReplayCipher())
	payload := map[string]interface{}{"input": "continue"}
	h.applyNativeReasoningReplay("grok-4.6", "session", payload)
	input, ok := payload["input"].([]interface{})
	if !ok || len(input) != 2 {
		t.Fatalf("input=%#v", payload["input"])
	}
	testutil.EqualAny(t, input[0].(map[string]interface{})["encrypted_content"], validTestReplayCipher())
}

func TestPrepareGrokSessionSeparatesTenantsAndSoftReplay(t *testing.T) {
	reqA := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqA.Header.Set("x-session-id", "same-client-session")
	a := prepareGrokSession(reqA, "grok-4.6", "", []chatwire.Message{{Role: "user", Content: "hello"}})
	testutil.Falsef(t, a.Key == "" || !a.Replay, "explicit session=%#v", a)
	soft := prepareGrokSession(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), "grok-4.6", "", []chatwire.Message{{Role: "user", Content: "hello"}})
	testutil.Falsef(t, soft.Key == "" || soft.Replay, "soft session=%#v", soft)
	otherModel := prepareGrokSession(reqA, "grok-4.5", "", []chatwire.Message{{Role: "user", Content: "hello"}})
	testutil.NotEqual(t, otherModel.Key, a.Key)
}
