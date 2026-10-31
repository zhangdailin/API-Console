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
// is retried against WorkOS, which issued the underlying grant, and that only a
// refusal from both still demands a browser login.
func TestRefreshFallsBackToWorkOS(t *testing.T) {
	exp := time.Now().Add(time.Hour).Unix()
	var workOSForm string
	workOSStatus := http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/refresh":
			w.WriteHeader(http.StatusUnauthorized)
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

	workOSStatus = http.StatusBadRequest
	_, err = client.Refresh(context.Background(), "durable")
	testutil.True(t, errors.Is(err, ErrReLoginRequired), "both refusals must still demand a new login")
}

func TestJWTExpiryIgnoresOpaqueTokens(t *testing.T) {
	testutil.True(t, jwtExpiry("opaque").IsZero(), "an opaque token has no stated expiry")
	testutil.True(t, jwtExpiry("a.!!!.c").IsZero(), "an undecodable claim set has no stated expiry")
	testutil.Equal(t, jwtExpiry(testJWT(4102444800)).Unix(), int64(4102444800))
}
