package workbuddy

import (
	"context"
	"encoding/base64"

	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"

	"testing"
	"time"

	"encoding/json"

	"orchids-api/internal/prompt"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

func jwtWithClaims(t *testing.T, claims map[string]interface{}) string {
	t.Helper()
	raw, err := json.Marshal(claims)
	testutil.NoError(t, err, "marshal claims: %v")
	return "header." + base64.RawURLEncoding.EncodeToString(raw) + ".signature"
}

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

func TestResolveCredentials_ReadsAuthDocument(t *testing.T) {
	t.Parallel()

	uid := "0f0f0f0f-1111-2222-3333-444455556666"
	expiry := time.Now().Add(48 * time.Hour)
	token := jwtWithClaims(t, map[string]interface{}{
		"sub":   uid,
		"email": "operator@example.com",
		"exp":   expiry.Unix(),
	})
	doc := `{"account":{"uid":"` + uid + `"},"auth":{"accessToken":"` + token +
		`","refreshToken":"refresh-value","expiresAt":` + strconv.FormatInt(expiry.UnixMilli(), 10) + `}}`

	creds := ResolveCredentials(&store.Account{ClientCookie: doc})
	testutil.Equal(t, creds.AccessToken, token)
	testutil.Equal(t, creds.RefreshToken, "refresh-value")
	testutil.Equal(t, creds.UID, uid)
	testutil.Equal(t, creds.Email, "operator@example.com")
	testutil.Falsef(t, creds.ExpiresAt.IsZero() || creds.ExpiresAt.Before(time.Now().Add(24*time.Hour)), "ExpiresAt = %v, want a future expiry decoded from milliseconds", creds.ExpiresAt)
}

func TestResolveCredentials_SplitsKeyValuePairs(t *testing.T) {
	t.Parallel()

	creds := ResolveCredentials(&store.Account{
		ClientCookie: "accessToken=abc.def.ghi; refreshToken=refresh-xyz",
	})
	testutil.Equal(t, creds.AccessToken, "abc.def.ghi")
	testutil.Equal(t, creds.RefreshToken, "refresh-xyz")
}

func TestResolveCredentials_TreatsOpaqueValueAsRefreshToken(t *testing.T) {
	t.Parallel()

	creds := ResolveCredentials(&store.Account{ClientCookie: "opaque-refresh-value"})
	testutil.Equal(t, creds.RefreshToken, "opaque-refresh-value")
	testutil.Equal(t, creds.AccessToken, "")
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
		Tools: []interface{}{map[string]interface{}{
			"name": "read", "input_schema": map[string]interface{}{"type": "object"},
		}},
		ToolChoice: map[string]interface{}{"type": "tool", "name": "read"},
	})
	testutil.NoError(t, err)
	var decoded map[string]interface{}
	testutil.NoError(t, json.Unmarshal(body, &decoded))
	testutil.Equal(t, decoded["tool_choice"], "required")
}

func TestRunChat_SendsSystemFirstAndSurfacesBusinessError(t *testing.T) {
	t.Parallel()

	var gotBody map[string]interface{}
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
	messages, ok := gotBody["messages"].([]interface{})
	if !ok || len(messages) == 0 {
		t.Fatalf("request body messages = %v", gotBody["messages"])
	}
	first, _ := messages[0].(map[string]interface{})
	testutil.Equal(t, first["role"], "system")
	testutil.Equal(t, gotBody["stream"], true)
	testutil.False(t, err == nil, "runChat() error = nil, want the failure surfaced")
}

func TestApplyChatHeaders_MatchesDesktopFingerprintAndReusesTurnID(t *testing.T) {
	t.Parallel()

	const (
		conversationID = "conv-camel"
		turnID         = "client-req-1"
		traceID        = "client-trace"
	)
	headers := make([]http.Header, 0, 2)
	for range 2 {
		req := httptest.NewRequest(http.MethodPost, DefaultBaseURL+"/v2/chat/completions", nil)
		applyChatHeaders(req, "access", "uid-1", conversationID, turnID, traceID)
		headers = append(headers, req.Header.Clone())
	}

	want := map[string]string{
		"Accept":                    "application/json, text/event-stream",
		"Accept-Language":           "en-US",
		"Authorization":             "Bearer access",
		"Origin":                    originReferer,
		"Referer":                   originReferer + "/",
		"User-Agent":                clientUA,
		"X-Agent-Purpose":           "conversation",
		"X-CodeBuddy-Request":       "1",
		"X-Conversation-ID":         conversationID,
		"X-Conversation-Request-ID": turnID,
		"X-Domain":                  "www.workbuddy.ai",
		"X-IDE-Name":                "WorkBuddy",
		"X-IDE-Type":                "WorkBuddy",
		"X-IDE-Version":             clientVersion,
		"X-No-Enterprise-Id":        "1",
		"X-Product":                 "WorkBuddy",
		"X-Root-Request-ID":         turnID,
		"X-Trace-ID":                traceID,
		"X-User-Id":                 "uid-1",
	}
	for i, header := range headers {
		for name, expected := range want {
			testutil.CheckEqual(t, header.Get(name), expected)
		}
		messageID := header.Get("X-Conversation-Message-ID")
		testutil.CheckFalsef(t, len(messageID) != 32 || validWorkBuddyTraceID(messageID) == "", "request %d message id = %q, want 32 hex", i+1, messageID)
		testutil.CheckEqual(t, header.Get("X-Request-ID"), messageID)
		got := header.Get("X-B3-TraceId")
		testutil.CheckFalsef(t, len(got) != 32 || validWorkBuddyTraceID(got) == "", "request %d X-B3-TraceId = %q, want valid fallback", i+1, got)
		testutil.CheckEqual(t, header.Get("X-B3-SpanId"), messageID[:16])
		testutil.CheckEqual(t, header.Get("X-B3-Sampled"), "1")
	}
	first, second := headers[0].Get("X-Conversation-Message-ID"), headers[1].Get("X-Conversation-Message-ID")
	testutil.Falsef(t, first == second, "message id was reused across attempts: %q", first)
}

func TestBuildBody_IncludesUsageAndCamelCaseConversationID(t *testing.T) {
	t.Parallel()

	body, err := NewFromAccount(nil, nil).buildBody(upstream.UpstreamRequest{ConversationID: "conv-1"})
	testutil.NoError(t, err)
	var decoded map[string]interface{}
	testutil.NoError(t, json.Unmarshal(body, &decoded))
	testutil.Equal(t, decoded["conversationId"], "conv-1")
	options, ok := decoded["stream_options"].(map[string]interface{})
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

func (f *fakeUpdater) UpdateAccount(_ context.Context, acc *store.Account) error {
	copied := *acc
	f.saved = &copied
	return nil
}

func TestBuildBodyForwardsReasoningEffort(t *testing.T) {
	t.Parallel()

	// A stated effort is forwarded as the OpenAI-style field.
	body, err := NewFromAccount(nil, nil).buildBody(upstream.UpstreamRequest{ReasoningEffort: "high"})
	testutil.NoError(t, err)
	var decoded map[string]interface{}
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
		var decoded map[string]interface{}
		testutil.NoError(t, json.Unmarshal(body, &decoded))
		got, present := decoded["reasoning_effort"]
		testutil.True(t, present, "effort %q: reasoning_effort is missing; the upstream would inline its reasoning into content")
		testutil.Equal(t, got, DefaultReasoningEffort)
	}
}
