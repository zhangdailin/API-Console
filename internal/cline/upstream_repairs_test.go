package cline

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"encoding/json"

	"orchids-api/internal/prompt"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

const okStream = "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n"

// chatRecorder is a stub chat endpoint that answers each call with the next
// scripted response and keeps every request body.
type chatRecorder struct {
	mu        sync.Mutex
	bodies    []map[string]any
	responses []func(http.ResponseWriter)
}

func (r *chatRecorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	raw, _ := io.ReadAll(req.Body)
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	r.mu.Lock()
	r.bodies = append(r.bodies, decoded)
	n := len(r.bodies)
	r.mu.Unlock()
	if n <= len(r.responses) {
		r.responses[n-1](w)
		return
	}
	_, _ = io.WriteString(w, okStream)
}

func refuse(status int, body string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func runRecordedChat(t *testing.T, rec *chatRecorder, req upstream.UpstreamRequest, onMessage func(upstream.SSEMessage)) error {
	t.Helper()
	server := httptest.NewServer(rec)
	t.Cleanup(server.Close)
	client := &Client{apiBase: server.URL, stream: server.Client(), creds: Credentials{AccessToken: "token"},
		account: &store.Account{ClineModelIDs: []string{`{"id":"model-a"}`}}}
	req.Model = "model-a"
	return client.SendRequestWithPayload(context.Background(), req, onMessage, nil)
}

func historyWithAssistantTurn() []prompt.Message {
	return []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Text: "first"}},
		{Role: "assistant", Content: prompt.MessageContent{Text: "answer"}},
		{Role: "user", Content: prompt.MessageContent{Text: "second"}},
	}
}

func assistantReasoning(body map[string]any) []any {
	var out []any
	for _, raw := range body["messages"].([]any) {
		if message := raw.(map[string]any); message["role"] == "assistant" {
			out = append(out, message["reasoning_content"])
		}
	}
	return out
}

// TestChatRepairsMissingReasoningReplay covers the GLM refusal of a history
// without reasoning_content, both as a status error and as the 200 stream whose
// first frame is the error. The repair fills a placeholder and replays once.
func TestChatRepairsMissingReasoningReplay(t *testing.T) {
	refusal := `{"error":{"message":"Missing required parameter: 'messages[1].reasoning_content'."}}`
	for name, first := range map[string]func(http.ResponseWriter){
		"status": refuse(http.StatusInternalServerError, refusal),
		"stream": refuse(http.StatusOK, "data: "+refusal+"\n\n"),
	} {
		t.Run(name, func(t *testing.T) {
			rec := &chatRecorder{responses: []func(http.ResponseWriter){first}}
			err := runRecordedChat(t, rec, upstream.UpstreamRequest{Messages: historyWithAssistantTurn()}, nil)
			testutil.NoError(t, err)
			testutil.Equal(t, len(rec.bodies), 2)
			testutil.Equal(t, fmt.Sprint(assistantReasoning(rec.bodies[0])), "[<nil>]")
			testutil.Equal(t, fmt.Sprint(assistantReasoning(rec.bodies[1])), "["+reasoningReplayPlaceholder+"]")
		})
	}
}

// TestChatRepairsEachRefusalOnlyOnce proves a repair that did not satisfy the
// upstream is not replayed again: the second identical refusal is final.
func TestChatRepairsEachRefusalOnlyOnce(t *testing.T) {
	refusal := refuse(http.StatusBadRequest, `Missing required parameter: 'messages[1].reasoning_content'`)
	rec := &chatRecorder{responses: []func(http.ResponseWriter){refusal, refusal, refusal}}
	err := runRecordedChat(t, rec, upstream.UpstreamRequest{Messages: historyWithAssistantTurn()}, nil)
	testutil.Error(t, err)
	testutil.Equal(t, len(rec.bodies), 2)
}

// TestChatDropsARefusedReasoningEffort keeps an explicit client "none" on the
// wire and removes it only after the upstream refused it.
func TestChatDropsARefusedReasoningEffort(t *testing.T) {
	rec := &chatRecorder{responses: []func(http.ResponseWriter){
		refuse(http.StatusBadRequest, `{"error":"Invalid value for reasoning_effort: none"}`),
	}}
	err := runRecordedChat(t, rec, upstream.UpstreamRequest{ReasoningEffort: "none",
		Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "hi"}}}}, nil)
	testutil.NoError(t, err)
	testutil.Equal(t, len(rec.bodies), 2)
	testutil.Equal[any](t, rec.bodies[0]["reasoning_effort"], "none")
	_, present := rec.bodies[1]["reasoning_effort"]
	testutil.False(t, present, "a refused reasoning_effort must be dropped on the replay")
}

// TestChatLowersOnlyTheDefaultMaxTokens proves the default budget is lowered
// when the upstream refuses it, while a client-stated budget is protocol data
// and is never rewritten.
func TestChatLowersOnlyTheDefaultMaxTokens(t *testing.T) {
	refusal := refuse(http.StatusBadRequest, `{"error":"maxOutputTokens out of range: Request contains an invalid argument"}`)
	messages := []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "hi"}}}

	rec := &chatRecorder{responses: []func(http.ResponseWriter){refusal}}
	testutil.NoError(t, runRecordedChat(t, rec, upstream.UpstreamRequest{Messages: messages}, nil))
	testutil.Equal(t, len(rec.bodies), 2)
	testutil.Equal[any](t, rec.bodies[0]["max_tokens"], float64(DefaultMaxTokens))
	testutil.Equal[any](t, rec.bodies[1]["max_tokens"], float64(repairMaxTokensBudget))

	stated := 100000
	rec = &chatRecorder{responses: []func(http.ResponseWriter){refusal}}
	testutil.Error(t, runRecordedChat(t, rec, upstream.UpstreamRequest{Messages: messages, MaxTokens: &stated}, nil))
	testutil.Equal(t, len(rec.bodies), 1)
}

func TestBuildChatBodyTreatsNonPositiveMaxTokensAsUnset(t *testing.T) {
	zero := 0
	payload, err := buildChatPayload(upstream.UpstreamRequest{MaxTokens: &zero,
		Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "hi"}}}}, "model-a")
	testutil.NoError(t, err)
	testutil.True(t, payload.defaultMaxTokens, "a zero budget must fall back to the default")
	testutil.MustContain(t, string(payload.body), `"max_tokens":128000`)
}

// TestChatShortensLongToolNamesAndRestoresThem covers the 64-character ceiling:
// declarations, replayed calls and tool_choice all carry the same short name,
// the client sees its own name back, and the request's tools are not mutated.
func TestChatShortensLongToolNamesAndRestoresThem(t *testing.T) {
	long := "mcp__" + strings.Repeat("very_long_server_name_", 3) + "read_file"
	short := upstreamToolName(long)
	testutil.True(t, len(long) > maxToolNameLength && len(short) <= maxToolNameLength, "short name must fit the ceiling")
	testutil.Equal(t, upstreamToolName(long), short)
	testutil.Equal(t, upstreamToolName("bash"), "bash")

	declared := map[string]any{"type": "function", "function": map[string]any{"name": long, "parameters": map[string]any{"type": "object"}}}
	rec := &chatRecorder{responses: []func(http.ResponseWriter){func(w http.ResponseWriter) {
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_2","function":{"name":"`+short+`","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`+"\n\n")
	}}}
	var names []string
	err := runRecordedChat(t, rec, upstream.UpstreamRequest{
		Tools:      []any{declared},
		ToolChoice: map[string]any{"type": "tool", "name": long},
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "read"}},
			{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "call_1", Name: long, Input: map[string]any{}}}}},
			{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "call_1", Content: "done"}}}},
		},
	}, func(msg upstream.SSEMessage) {
		if msg.Type == "model.tool-call" {
			names = append(names, msg.Event["toolName"].(string))
		}
	})
	testutil.NoError(t, err)
	body := rec.bodies[0]
	testutil.Equal[any](t, body["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["name"], short)
	testutil.Equal[any](t, body["tool_choice"].(map[string]any)["function"].(map[string]any)["name"], short)
	call := body["messages"].([]any)[1].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	testutil.Equal[any](t, call["function"].(map[string]any)["name"], short)
	testutil.Equal(t, strings.Join(names, ","), long)
	testutil.Equal[any](t, declared["function"].(map[string]any)["name"], long)
}

func testJWT(exp int64) string {
	claims, _ := json.Marshal(map[string]any{"exp": exp})
	return "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".sig"
}

// TestRefreshFallsBackToWorkOS proves a refusal at the Cline refresh endpoint
// is retried against WorkOS, which issued the underlying grant. The renewed
// grant is exchanged at /auth/register when that works and used bare when it
// does not; only a refusal from both endpoints still demands a browser login.
func TestRefreshFallsBackToWorkOS(t *testing.T) {
	exp := time.Now().Add(time.Hour).Unix()
	var workOSForm, registered string
	workOSStatus, registerStatus := http.StatusOK, http.StatusServiceUnavailable
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/refresh":
			w.WriteHeader(http.StatusUnauthorized)
		case "/auth/register":
			raw, _ := io.ReadAll(r.Body)
			registered = string(raw)
			w.WriteHeader(registerStatus)
			if registerStatus == http.StatusOK {
				_, _ = io.WriteString(w, `{"data":{"accessToken":"cline-access","refreshToken":"cline-refresh","expiresAt":"2099-01-01T00:00:00Z"}}`)
			}
		case "/workos":
			raw, _ := io.ReadAll(r.Body)
			workOSForm = string(raw)
			w.WriteHeader(workOSStatus)
			if workOSStatus == http.StatusOK {
				_, _ = io.WriteString(w, `{"access_token":"`+testJWT(exp)+`","refresh_token":"rotated"}`)
			} else {
				_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
			}
		}
	}))
	defer server.Close()
	client := &Client{apiBase: server.URL, control: server.Client(), workOSTokenURL: server.URL + "/workos", workOSClientID: "client_x"}

	creds, err := client.Refresh(context.Background(), "durable")
	testutil.NoError(t, err)
	testutil.Equal(t, creds.AccessToken, testJWT(exp))
	testutil.Equal(t, creds.RefreshToken, "rotated")
	testutil.Equal(t, creds.ExpiresAt.Unix(), exp)
	for _, want := range []string{"grant_type=refresh_token", "client_id=client_x", "refresh_token=durable"} {
		testutil.CheckContain(t, workOSForm, want)
	}

	// A renewed grant is exchanged for a Cline pair, as at login.
	registerStatus = http.StatusOK
	creds, err = client.Refresh(context.Background(), "durable")
	testutil.NoError(t, err)
	testutil.Equal(t, creds.AccessToken, "cline-access")
	testutil.Equal(t, creds.RefreshToken, "cline-refresh")
	testutil.Equal(t, creds.ExpiresAt.Year(), 2099)
	testutil.CheckContain(t, registered, `"refreshToken":"rotated"`)

	workOSStatus = http.StatusBadRequest
	_, err = client.Refresh(context.Background(), "durable")
	testutil.True(t, errors.Is(err, ErrReLoginRequired), "both refusals must still demand a new login")
}

func TestJWTExpiryIgnoresOpaqueTokens(t *testing.T) {
	testutil.True(t, jwtExpiry("opaque").IsZero(), "an opaque token has no stated expiry")
	testutil.True(t, jwtExpiry("a.!!!.c").IsZero(), "an undecodable claim set has no stated expiry")
	testutil.Equal(t, jwtExpiry(testJWT(4102444800)).Unix(), int64(4102444800))
}

// TestConsumeStreamKeepsAnAnswerQuotingTheRefusal proves the refusal probe only
// reads error frames: an answer whose first delta quotes the phrase is content.
func TestConsumeStreamKeepsAnAnswerQuotingTheRefusal(t *testing.T) {
	body := `data: {"choices":[{"delta":{"content":"Missing required parameter means a field is absent"},"finish_reason":"stop"}]}` + "\n\n"
	var text strings.Builder
	_, err := consumeStream(strings.NewReader(body), false, func(msg upstream.SSEMessage) {
		if msg.Type == "model.text-delta" {
			text.WriteString(fmt.Sprint(msg.Event["delta"]))
		}
	})
	testutil.NoError(t, err)
	testutil.MustContain(t, text.String(), "Missing required parameter")
}

// TestUpstreamToolNameCutsOnARuneBoundary pins the byte ceiling for a name
// whose 55-byte prefix would end inside a multi-byte character.
func TestUpstreamToolNameCutsOnARuneBoundary(t *testing.T) {
	name := strings.Repeat("a", 54) + "工具" + strings.Repeat("b", 20)
	short := upstreamToolName(name)
	testutil.True(t, len(short) <= maxToolNameLength, "the short name must fit the byte ceiling")
	testutil.True(t, utf8.ValidString(short), "the short name must stay valid UTF-8")
}

// TestRepairKeepsLargeIntegersExact proves the repair round trip does not
// round numbers it does not touch, such as a tool schema bound.
func TestRepairKeepsLargeIntegersExact(t *testing.T) {
	body := []byte(`{"max_tokens":128000,"tools":[{"type":"function","function":{"name":"f","parameters":{"maximum":9007199254740993}}}]}`)
	repaired, ok := repairChatBody(body, repairMaxTokens)
	testutil.True(t, ok, "the default budget must be lowered")
	testutil.MustContain(t, string(repaired), `9007199254740993`)
	testutil.MustContain(t, string(repaired), `"max_tokens":32000`)
	_, ok = repairChatBody([]byte(`{"messages":[]}`), repairMaxTokens)
	testutil.False(t, ok, "a body without max_tokens has nothing to lower")
}

// liveReasoningRefusal is the refusal observed from the live gateway for
// reflection/beam-free (2026-10-31). The provider message sits behind ~250
// characters of wrapper text, past the length an error string is truncated to.
const liveReasoningRefusal = `{"error":{"code":"stream_initialization_failed","message":"Failed to create stream: inference request failed: failed to generate stream from Vercel: failed to invoke model 'reflection/beam-free' with streaming: request failed with status 400: {\"error\":{\"message\":\"Missing required parameter: 'messages[1].reasoning_content'. This model needs the reasoning of every earlier assistant message: send it back.\"}}"}}`

// TestChatRepairsTheLiveRefusalShape proves the repair is classified on the
// untruncated upstream text, both for the 200 stream whose first frame is the
// refusal (the live shape) and for the same text as a status error.
func TestChatRepairsTheLiveRefusalShape(t *testing.T) {
	testutil.True(t, strings.Index(liveReasoningRefusal, "reasoning_content") > 300, "the fixture must exceed the truncation length")
	for name, first := range map[string]func(http.ResponseWriter){
		"stream": refuse(http.StatusOK, "data: "+liveReasoningRefusal+"\n\n"),
		"status": refuse(http.StatusBadRequest, liveReasoningRefusal),
	} {
		t.Run(name, func(t *testing.T) {
			rec := &chatRecorder{responses: []func(http.ResponseWriter){first}}
			err := runRecordedChat(t, rec, upstream.UpstreamRequest{Messages: historyWithAssistantTurn()}, nil)
			testutil.NoError(t, err)
			testutil.Equal(t, len(rec.bodies), 2)
			testutil.Equal(t, fmt.Sprint(assistantReasoning(rec.bodies[1])), "["+reasoningReplayPlaceholder+"]")
		})
	}
}
