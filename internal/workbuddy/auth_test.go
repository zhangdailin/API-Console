package workbuddy

import (
	"context"

	"io"
	"net/http"
	"net/http/httptest"

	"strings"

	"testing"
	"time"

	"encoding/json"

	"orchids-api/internal/prompt"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

// jsonMessage builds a message through the wire shape so MessageContent keeps
// its string-vs-blocks union semantics (a nil Blocks slice alone reads as
// "string mode" only for non-empty text).
func jsonMessage(t *testing.T, role, text string) prompt.Message {
	t.Helper()
	var msg prompt.Message
	raw, err := json.Marshal(map[string]string{"role": role, "content": text})
	testutil.NoError(t, err, "marshal message: %v")
	testutil.NoError(t, json.Unmarshal(raw, &msg), "unmarshal message: %v")
	return msg
}

func userMessage(t *testing.T, text string) prompt.Message {
	t.Helper()
	return jsonMessage(t, "user", text)
}

func roleMessage(t *testing.T, role, text string) prompt.Message {
	t.Helper()
	return jsonMessage(t, role, text)
}

func TestResolveCredentials_PrefersDedicatedFields(t *testing.T) {
	t.Parallel()

	creds := ResolveCredentials(&store.Account{
		WorkBuddyAccessToken:  "stored-access",
		WorkBuddyRefreshToken: "stored-refresh",
		WorkBuddyUID:          "stored-uid",
		ClientCookie:          "pasted-refresh",
	})
	testutil.Equal(t, creds.AccessToken, "stored-access")
	testutil.Equal(t, creds.RefreshToken, "stored-refresh")
	testutil.Equal(t, creds.UID, "stored-uid")
}

func TestCredentialsToken_RefreshesBeforeExpiry(t *testing.T) {
	t.Parallel()

	fresh := Credentials{AccessToken: "token", ExpiresAt: time.Now().Add(72 * time.Hour)}
	_, ok := fresh.Token(time.Now())
	testutil.False(t, !ok, "a token valid for 72h must be reused")
	stale := Credentials{AccessToken: "token", ExpiresAt: time.Now().Add(30 * time.Second)}
	_, ok = stale.Token(time.Now())
	testutil.False(t, ok, "a token expiring within the refresh lead must trigger a refresh")
	opaque := Credentials{AccessToken: "token"}
	_, ok = opaque.Token(time.Now())
	testutil.False(t, !ok, "an opaque token without expiry must be used as-is")
}

func TestRunChat_RequiresCredentials(t *testing.T) {
	t.Parallel()

	client := NewFromAccount(&store.Account{}, nil)
	client.baseURL = "https://example.invalid"
	err := client.runChat(context.Background(), upstream.UpstreamRequest{Model: defaultModel}, time.Second, nil, nil)
	testutil.Falsef(t, err == nil || !strings.Contains(err.Error(), "missing credentials"), "runChat() error = %v, want a missing-credential error", err)
}

func TestBuildBodyNormalizesStringOnlyToolChoice(t *testing.T) {
	client := NewFromAccount(nil, nil)
	body, err := client.buildBody(upstream.UpstreamRequest{
		Tools: []any{map[string]any{
			"name": "read", "input_schema": map[string]any{"type": "object"},
		}},
		ToolChoice: map[string]any{"type": "tool", "name": "read"},
	})
	testutil.NoError(t, err)
	var decoded map[string]any
	testutil.NoError(t, json.Unmarshal(body, &decoded))
	testutil.Equal(t, decoded["tool_choice"], "required")
}

func TestRunChat_SendsSystemFirstAndSurfacesBusinessError(t *testing.T) {
	t.Parallel()

	var gotBody map[string]any
	var sawUID bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/plugin/auth/token/refresh":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"data":{"accessToken":"fresh-access","refreshToken":"rotated-refresh","expiresIn":3600}}`))
		case "/v2/chat/completions":
			sawUID = r.Header.Get("X-User-Id") != ""
			testutil.CheckEqual(t, r.Header.Get("Origin"), originReferer)
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &gotBody)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(`{"code":6004,"msg":"6004:usage exceeds frequency limit"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client := NewFromAccount(&store.Account{WorkBuddyUID: "uid-1", WorkBuddyRefreshToken: "old-refresh"}, nil)
	client.baseURL = srv.URL
	client.httpClient = srv.Client()

	err := client.runChat(context.Background(), upstream.UpstreamRequest{
		Model:    "hy3",
		Messages: []prompt.Message{userMessage(t, "hi")},
	}, 5*time.Second, nil, nil)

	testutil.CheckTrue(t, sawUID, "X-User-Id header missing on the chat request")
	messages, ok := gotBody["messages"].([]any)
	if !ok || len(messages) == 0 {
		t.Fatalf("request body messages = %v", gotBody["messages"])
	}
	first, _ := messages[0].(map[string]any)
	testutil.Equal(t, first["role"], "system")
	testutil.Equal(t, gotBody["stream"], true)
	testutil.False(t, err == nil, "runChat() error = nil, want the failure surfaced")
}

func TestApplyChatHeadersMatches584AndRefreshesIdentity(t *testing.T) {
	t.Parallel()
	var previous http.Header
	for range 2 {
		req := httptest.NewRequest(http.MethodPost, DefaultBaseURL+"/v2/chat/completions", nil)
		req.Header.Set("X-Machine-ID", "old-machine")
		req.Header.Set("X-Session-ID", "old-session")
		applyChatHeaders(req, "access", "uid-1")
		h := req.Header
		for key, want := range map[string]string{
			"Accept": "application/json, text/event-stream", "Accept-Language": "en-US",
			"Authorization": "Bearer access", "Origin": originReferer, "Referer": originReferer + "/",
			"User-Agent":      "WorkBuddy/5.8.4 workbuddy-ai/5.8.4 CLI/2.161.0",
			"X-Agent-Purpose": "conversation", "X-Agent-Intent": "craft", "X-Agent-Type": "main",
			"X-Private-Data": "true", "X-CodeBuddy-Request": "1", "X-Domain": "www.workbuddy.ai",
			"X-IDE-Name": "WorkBuddy", "X-IDE-Type": "WorkBuddy", "X-IDE-Version": "5.8.4",
			"X-No-Enterprise-Id": "1", "X-Product": "SaaS", "X-User-Id": "uid-1",
			"X-Machine-ID": "", "X-Session-ID": "", "X-B3-Sampled": "1",
		} {
			if got := h.Get(key); got != want {
				t.Errorf("%s=%q, want %q", key, got, want)
			}
		}
		conv := h.Get("X-Conversation-ID")
		if len(conv) != 36 || conv[8] != '-' || conv[13] != '-' || conv[18] != '-' || conv[23] != '-' {
			t.Fatalf("invalid conversation UUID: %q", conv)
		}
		for _, key := range []string{"X-Conversation-ID", "X-Conversation-Request-ID", "X-Root-Request-ID", "X-Conversation-Message-ID", "X-Request-ID", "X-Trace-ID"} {
			value := strings.ReplaceAll(h.Get(key), "-", "")
			if len(value) != 32 || validWorkBuddyTraceID(value) == "" || value[12] != '7' || !strings.ContainsRune("89ab", rune(value[16])) {
				t.Fatalf("%s invalid UUIDv7: %q", key, value)
			}
			if previous != nil && previous.Get(key) == h.Get(key) {
				t.Errorf("%s reused across attempts", key)
			}
		}
		trace, span := h.Get("X-Trace-ID"), h.Get("X-B3-SpanId")
		if len(span) != 16 || validWorkBuddyTraceID(span) == "" {
			t.Fatalf("invalid span %q", span)
		}
		testutil.Equal(t, h.Get("X-Request-ID"), h.Get("X-Conversation-Message-ID"))
		testutil.Equal(t, h.Get("X-Root-Request-ID"), h.Get("X-Conversation-Request-ID"))
		testutil.Equal(t, h.Get("X-B3-TraceId"), trace)
		testutil.Equal(t, h.Get("traceparent"), "00-"+trace+"-"+span+"-01")
		testutil.Equal(t, h.Get("b3"), trace+"-"+span+"-1")
		previous = h.Clone()
	}
}

func TestBuildBody_IncludesUsageAndCamelCaseConversationID(t *testing.T) {
	t.Parallel()

	body, err := NewFromAccount(nil, nil).buildBody(upstream.UpstreamRequest{ConversationID: "conv-1"})
	testutil.NoError(t, err)
	var decoded map[string]any
	testutil.NoError(t, json.Unmarshal(body, &decoded))
	testutil.Equal(t, decoded["conversationId"], "conv-1")
	options, ok := decoded["stream_options"].(map[string]any)
	if !ok || options["include_usage"] != true {
		t.Fatalf("stream_options = %#v, want include_usage=true", decoded["stream_options"])
	}
}

func TestApiError_CarriesStatusAndCode(t *testing.T) {
	t.Parallel()

	err := apiError(http.StatusUnauthorized, []byte(`{"code":12153,"msg":"Offline user session not found"}`))
	msg := err.Error()
	for _, want := range []string{"status=401", "code=12153", "Offline user session not found"} {
		testutil.MustContain(t, msg, want)
	}
}

func TestFetchModels_FiltersCLIWhitelist(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{
			"models":[
				{"id":"hy3","name":"HY3","maxInputTokens":131072,"maxOutputTokens":8192,"supportsToolCall":true,"supportsReasoning":true,"disabled":false},
				{"id":"default-model","name":"Default","disabled":false},
				{"id":"hidden-model","name":"Hidden","disabled":false},
				{"id":"disabled-model","name":"Disabled","disabled":true}
			],
			"agents":[{"name":"cli","models":["default-model","hy3","disabled-model"]}]
		}}`))
	}))
	defer srv.Close()

	client := NewFromAccount(&store.Account{}, nil)
	client.baseURL = srv.URL
	client.httpClient = srv.Client()
	client.creds = Credentials{AccessToken: "access", UID: "uid"}

	models, err := client.FetchModels(context.Background())
	testutil.NoError(t, err, "FetchModels() error = %v")
	got := make([]string, 0, len(models))
	for _, model := range models {
		got = append(got, model.ID)
	}
	testutil.Equal(t, strings.Join(got, ","), "hy3,default-model")
	if !models[0].SupportsTools || !models[0].SupportsReason || models[0].MaxInputTokens != 131072 || models[0].MaxOutputTokens != 8192 {
		t.Fatalf("model capabilities were lost: %+v", models[0])
	}
}

func (f *fakeUpdater) UpdateWorkBuddyCredentials(_ context.Context, id int64, patch store.WorkBuddyCredentialPatch) error {
	f.saved = &store.Account{ID: id, WorkBuddyAccessToken: patch.AccessToken, WorkBuddyRefreshToken: patch.RefreshToken, WorkBuddyExpiresAt: patch.ExpiresAt}
	return nil
}

func TestBuildBodyForwardsReasoningEffort(t *testing.T) {
	t.Parallel()

	// A stated effort is forwarded as the OpenAI-style field.
	body, err := NewFromAccount(nil, nil).buildBody(upstream.UpstreamRequest{ReasoningEffort: "high"})
	testutil.NoError(t, err)
	var decoded map[string]any
	testutil.NoError(t, json.Unmarshal(body, &decoded))
	testutil.Equal(t, decoded["reasoning_effort"], "high")

	// A silent request must still carry an effort. Omitting the field makes the
	// upstream write its chain of thought into content, where the stream reader
	// cannot separate it from the answer and the client renders the model's
	// private reasoning as the reply.
	body, err = NewFromAccount(nil, nil).buildBody(upstream.UpstreamRequest{})
	testutil.NoError(t, err)
	decoded = nil
	testutil.NoError(t, json.Unmarshal(body, &decoded))
	testutil.Equal(t, decoded["reasoning_effort"], DefaultReasoningEffort)

	// A stated effort still wins over the default, including a lower level.
	body, err = NewFromAccount(nil, nil).buildBody(upstream.UpstreamRequest{ReasoningEffort: "low"})
	testutil.NoError(t, err)
	decoded = nil
	testutil.NoError(t, json.Unmarshal(body, &decoded))
	testutil.Equal(t, decoded["reasoning_effort"], "low")

	// "none" means the client asked for no reasoning; omit the field instead of
	// sending a level the upstream would reject.
	body, err = NewFromAccount(nil, nil).buildBody(upstream.UpstreamRequest{ReasoningEffort: "none"})
	testutil.NoError(t, err)
	decoded = nil
	testutil.NoError(t, json.Unmarshal(body, &decoded))
	_, present := decoded["reasoning_effort"]
	testutil.Falsef(t, present, "none must omit reasoning_effort, got %#v", decoded["reasoning_effort"])
}

// TestSilentRequestKeepsChainOfThoughtPrivate pins the shape that leaked the
// model's scratchpad to clients: with reasoning_effort omitted the upstream
// wrote its chain of thought into delta.content, and the stream reader has no
// delimiter to separate that prose from the answer, so it was forwarded as
// visible text. A request that states nothing about reasoning must still carry
// an effort.
func TestSilentRequestKeepsChainOfThoughtPrivate(t *testing.T) {
	t.Parallel()

	for _, effort := range []string{"", "   "} {
		body, err := NewFromAccount(nil, nil).buildBody(upstream.UpstreamRequest{ReasoningEffort: effort})
		testutil.NoError(t, err)
		var decoded map[string]any
		testutil.NoError(t, json.Unmarshal(body, &decoded))
		got, present := decoded["reasoning_effort"]
		testutil.True(t, present, "effort %q: reasoning_effort is missing; the upstream would inline its reasoning into content")
		testutil.Equal(t, got, DefaultReasoningEffort)
	}
}

func TestResolveCredentialsIgnoresGenericSlots(t *testing.T) {
	for _, raw := range []string{"opaque", "accessToken=access; refreshToken=refresh", `{"auth":{"accessToken":"access","refreshToken":"refresh"}}`} {
		testutil.False(t, ResolveCredentials(&store.Account{ClientCookie: raw, Token: raw, RefreshToken: raw}).HasCredential(), "generic credentials must not migrate")
	}
}

func TestRunChatCreatesFreshWireIdentityForRepeatedConversation(t *testing.T) {
	captured := make(chan http.Header, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured <- r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"code":6004,"msg":"usage exceeds frequency limit"}`)
	}))
	defer srv.Close()
	client := NewFromAccount(&store.Account{WorkBuddyUID: "uid-1", WorkBuddyAccessToken: "access", WorkBuddyExpiresAt: time.Now().Add(time.Hour)}, nil)
	client.baseURL = srv.URL
	client.httpClient = srv.Client()
	payload := upstream.UpstreamRequest{Model: "hy3", ConversationID: "client-conversation", RequestID: "client-request", TraceID: "client-trace", Messages: []prompt.Message{userMessage(t, "hi")}}
	for range 2 {
		if err := client.runChat(context.Background(), payload, time.Second, nil, nil); err == nil {
			t.Fatal("upstream refusal was lost")
		}
	}
	first, second := <-captured, <-captured
	for _, key := range []string{"X-Conversation-ID", "X-Conversation-Request-ID", "X-Request-ID", "X-Trace-ID"} {
		if first.Get(key) == "" || first.Get(key) == second.Get(key) || strings.HasPrefix(first.Get(key), "client-") {
			t.Errorf("%s did not get fresh wire identity: %q / %q", key, first.Get(key), second.Get(key))
		}
	}
}
