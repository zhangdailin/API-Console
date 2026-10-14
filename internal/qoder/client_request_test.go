package qoder

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"encoding/json"

	"orchids-api/internal/config"
	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/prompt"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

// TestSendRequestSetsTheFullHeaderContract pins the signed header set, including
// the conditional presence rules. The gateway rejects a request that carries an
// empty organization header, so presence is part of correctness, not style.
func TestSendRequestSetsTheFullHeaderContract(t *testing.T) {
	t.Parallel()

	type captured struct {
		headers http.Header
		url     string
		body    []byte
	}
	capturedCh := make(chan captured, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capturedCh <- captured{headers: r.Header.Clone(), url: r.URL.String(), body: body}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(envelope(`{"id":"1","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}`)))
		_, _ = w.Write([]byte("event:finish\ndata: {}\n\n"))
	}))
	defer server.Close()

	acc := signedTestAccount()
	client := NewFromAccount(acc, nil)
	setTestEndpoints(client, server.URL, server.URL, server.URL)

	var events []upstream.SSEMessage
	err := client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{
		Model:    "Qwen3.7-Max",
		Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "hello"}}},
	}, func(msg upstream.SSEMessage) {
		events = append(events, msg)
	}, nil)
	testutil.NoError(t, err, "SendRequestWithPayload() error = %v")

	var got captured
	select {
	case got = <-capturedCh:
	case <-time.After(2 * time.Second):
		t.Fatal("the stub server received no request")
	}

	testutil.MustContain(t, got.url, "/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1")

	// NOTE: net/http canonicalizes header names, so Cosy-ClientType reads back
	// as Cosy-Clienttype and Login-Version as Login-Version.
	// The device id is the identity the credential was authorized under and is
	// sent unchanged; QoderWork uses the same id as its token and type 5.
	want := map[string]string{
		"Accept":                "text/event-stream",
		"Accept-Language":       "*",
		"Cache-Control":         "no-cache",
		"Connection":            "keep-alive",
		"Content-Type":          "application/json",
		"Cosy-Business-Product": "qoder_work",
		"Cosy-Business-Type":    "agent",
		"Cosy-Clienttype":       "6",
		"Cosy-Data-Policy":      "agree",
		"Cosy-Machineid":        acc.QoderMachineID,
		"Cosy-Machineos":        "x86_64_win32",
		"Cosy-Machinetoken":     acc.QoderMachineID,
		"Cosy-Machinetype":      "5",
		"Cosy-Scene":            "qwork",
		"Cosy-User":             "uid-1",
		"Login-Version":         "v2",
		"Sec-Fetch-Mode":        "cors",
		"X-Model-Key":           "qmodel_latest",
		"X-Model-Source":        "system",
		"User-Agent":            "node",
	}
	for name, value := range want {
		testutil.CheckEqual(t, got.headers.Get(name), value)
	}
	// The capture carries a trace context on every API call, and the gateway
	// echoes the trace id back as sw-trace-id, which is what makes a request
	// correlatable upstream-side.
	trace := got.headers.Get("Traceparent")
	testutil.CheckFalsef(t, !validTraceparent(trace), "Traceparent = %q, want a version 00 trace context", trace)
	testutil.CheckFalse(t, got.headers.Get("Cosy-Key") == "" || got.headers.Get("Cosy-Key") == "runtime-key", "Cosy-Key was not rederived using the reference runtime identity")
	testutil.CheckNotEqual(t, got.headers.Get("Cosy-Date"), "")
	auth := got.headers.Get("Authorization")
	testutil.CheckFalsef(t, !strings.HasPrefix(auth, "Bearer COSY."), "Authorization = %q, want a COSY bearer", auth)
	testutil.CheckEqual(t, got.headers.Get("Cosy-Organization-Id"), "")
	if got := len(got.headers); got < 20 {
		t.Errorf("header count = %d, want the full signed set", got)
	}

	// The body is in the private encoding and decodes to the chat payload.
	decoded, err := decodeBodyForTest(got.body)
	testutil.NoError(t, err, "DecodeBody() error = %v")
	text := string(decoded)
	for _, want := range []string{`"chat_task":"FREE_INPUT"`, `"session_type":"qoder_work"`, `"agent_id":"agent_common"`, `"task_id":"common"`, `"stream":true`, `"version":"3"`, `"key":"qmodel_latest"`, `"role":"user"`, `"context_length":1000000`} {
		testutil.CheckContain(t, text, want)
	}

	testutil.Falsef(t, len(events) == 0 || events[len(events)-1].Type != "model.finish", "events = %+v, want a trailing model.finish", events)
	reason, _ := events[len(events)-1].Event["finishReason"].(string)
	testutil.Falsef(t, reason != "end_turn", "finishReason = %v, want end_turn", events[len(events)-1].Event["finishReason"])
}

// TestSendRequestRefreshesOnceOnUnauthorized proves a single 401 forces one
// token refresh and the retry succeeds. The refresh budget is one attempt: a
// second 401 after a fresh token is a real credential problem, and retrying it
// would hammer the token endpoint.
func TestSendRequestRefreshesOnceOnUnauthorized(t *testing.T) {
	t.Parallel()

	var chatCalls, refreshCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "deviceToken/refresh"):
			refreshCalls++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"device_token":"access-2","refresh_token":"refresh-2","expires_in":3600}`))
		case strings.Contains(r.URL.Path, "agent_chat_generation"):
			chatCalls++
			if chatCalls == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"statusCodeValue":401,"body":"{\"message\":\"login expired\"}"}`))
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(envelope(`{"id":"1","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`)))
			_, _ = w.Write([]byte("event:finish\ndata: {}\n\n"))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	acc := signedTestAccount()
	client := NewFromAccount(acc, nil)
	setTestEndpoints(client, server.URL, server.URL, server.URL)

	err := client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{
		Model:    "Qwen3.7-Max",
		Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "hello"}}},
	}, nil, nil)
	testutil.NoError(t, err, "SendRequestWithPayload() error = %v")
	testutil.Equal(t, chatCalls, 2)
	testutil.Equal(t, refreshCalls, 1)
}

func TestForceRefreshRejectsExpiredDurableCredential(t *testing.T) {
	client := NewFromAccount(signedTestAccount(), nil)
	err := client.forceRefresh(context.Background(), Credentials{RefreshToken: "expired", RefreshExpiresAt: time.Now().Add(-time.Minute)})
	testutil.Falsef(t, !errors.Is(err, ErrReLoginRequired), "forceRefresh error=%v want ErrReLoginRequired", err)
	class := apperrors.ClassifyUpstreamError(err.Error())
	testutil.Equal(t, class.Category, "auth")
}

// TestClassifyStatus pins the retry verdicts, including the busy code arriving
// under a 401.
func TestConfiguredClientVersionMatchesReferenceBodyAndHeader(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{QoderClientVersion: "9.8.7"}
	client := NewFromAccount(signedTestAccount(), cfg)
	body, err := buildChatBodyProfile(upstream.UpstreamRequest{}, modelEntry{Key: "m"}, "session", "request", "request-set", client.clientVersion, "", sceneBusinessProduct)
	testutil.NoError(t, err)
	raw, err := decodeBodyForTest(body)
	testutil.NoError(t, err)
	testutil.Falsef(t, !strings.Contains(string(raw), `"business":{"product":"qoder_work","version":"9.8.7"`), "body version is incoherent: %s", raw)
	req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/algo/chat", nil)
	testutil.NoError(t, client.applyAuthHeaders(req, credsOf(signedTestAccount()), RuntimeFields{EncryptUserInfo: "info", Key: "key"}, "request", "m", "system", string(body), "/chat"))
	testutil.Equal(t, req.Header.Get("Cosy-Version"), "9.8.7")
}

func TestReferenceChatBodyCarriesPromptContextAndModel(t *testing.T) {
	model := modelEntry{Key: "qfmodel", DisplayName: "Qwen3.8-Flash", IsReasoning: true, MaxInputTokens: 180000}
	req := upstream.UpstreamRequest{Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "你好 qoder"}}}}
	encoded, err := buildChatBodyProfile(req, model, "session-id", "request-id", "request-set-id", DefaultClientVersion, "", sceneBusinessProduct)
	testutil.NoError(t, err)
	raw, err := decodeBodyForTest(encoded)
	testutil.NoError(t, err)
	var body map[string]any
	testutil.NoError(t, json.Unmarshal(raw, &body))
	context := body["chat_context"].(map[string]any)
	// The capture carries the same plain string in both fields; the
	// {"type":"text","text":...} object shape this channel used to send is not
	// something the QoderWork client produces.
	testutil.Equal(t, context["text"], "你好 qoder")
	testutil.Equal(t, context["extra"].(map[string]any)["originalContent"], "你好 qoder")
	contextModel := context["extra"].(map[string]any)["modelConfig"].(map[string]any)
	testutil.Equal(t, contextModel["key"], "qfmodel")
	testutil.Equal(t, contextModel["is_reasoning"], true)
	modelConfig := body["model_config"].(map[string]any)
	testutil.Equal(t, modelConfig["key"], "qfmodel")
	testutil.Equal(t, modelConfig["is_reasoning"], true)
	testutil.Equal(t, body["business"].(map[string]any)["product"], "qoder_work")
	params := body["parameters"].(map[string]any)
	testutil.EqualAny(t, params["max_tokens"], float64(32000))
	for _, field := range []string{"reasoning_effort", "enable_thinking"} {
		_, present := params[field]
		testutil.Falsef(t, present, "unexpected default %s in %#v", field, params)
	}
}

func TestRefreshedReplayUsesFreshIdentityAndRetryFlag(t *testing.T) {
	original, err := buildChatBodyProfile(upstream.UpstreamRequest{}, modelEntry{Key: "m"}, "session", "old", "request-set", "1.2.3", "", sceneBusinessProduct)
	testutil.NoError(t, err)
	replayed, err := refreshedReplayBody(original, "new")
	testutil.NoError(t, err)
	raw, _ := decodeBodyForTest(replayed)
	var body chatBody
	testutil.NoError(t, json.Unmarshal(raw, &body))
	// request_id and chat_record_id identify the attempt and are refreshed;
	// request_set_id and business.id identify the task and stay put, which is
	// what the capture shows across the requests of one task.
	testutil.Falsef(t, body.RequestID != "new" || body.ChatRecordID != "new" || body.IsRetry, "replay request id not refreshed: %+v", body)
	testutil.Equal(t, body.RequestSetID, "request-set")
	testutil.Equal(t, body.Business.ID, "request-set")
}
