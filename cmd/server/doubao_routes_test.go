package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

// doubaoE2EStub answers the two Doubao endpoints a text turn touches and
// records the chat body, so a test can read the prompt the gateway rendered.
type doubaoE2EStub struct {
	*httptest.Server
	answer   string
	chatBody string
}

func newDoubaoE2EStub() *doubaoE2EStub {
	stub := &doubaoE2EStub{}
	stub.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		switch {
		case strings.HasPrefix(r.URL.Path, "/im/conversation/create"):
			_, _ = io.WriteString(w, `{"status_code":0,"downlink_body":{"create_conv_downlink_body":{"conversation_info":{"conversation_id":"c1"}}}}`)
		case strings.HasPrefix(r.URL.Path, "/chat/completion"):
			stub.chatBody = string(raw)
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: FULL_MSG_NOTIFY\ndata: {\"message\":{\"message_id\":\"u1\"}}\n\n")
			fmt.Fprintf(w, "event: CHUNK_DELTA\ndata: {\"text\":%q}\n\n", stub.answer)
			fmt.Fprintf(w, "event: SSE_REPLY_END\ndata: {\"end_type\":3}\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	return stub
}

// TestDoubaoChatCompletionsWireShape drives the Doubao channel through the
// registered route table and checks the OpenAI wire on both sides: what the
// client is sent back, and what of the request reaches the upstream prompt.
func TestDoubaoChatCompletionsWireShape(t *testing.T) {
	stub := newDoubaoE2EStub()
	defer stub.Close()
	cfg := &config.Config{AdminUser: "admin", AdminPass: "secret", AdminToken: "admintoken", AdminPath: "/admin",
		DoubaoBaseURL: stub.URL, DoubaoAccountsURL: stub.URL, DoubaoRequestGapMS: 1}
	mux, s, h := newRouteMux(t, "doubao-e2e:", cfg)
	e := newChannelE2EWith(t, mux, s, h, "sk-doubao-e2e")
	testutil.NoError(t, s.CreateAccount(context.Background(), &store.Account{Name: "d", AccountType: "doubao",
		ClientCookie: `{"sessionid":"x","s_v_web_id":"fp"}`, Enabled: true, Weight: 1, DoubaoVerifiedAt: time.Now()}))
	_, err := s.ReconcileDiscoveredModels(context.Background(), "doubao", []*store.Model{{Channel: "doubao", ModelID: "doubao",
		Name: "Doubao", Status: store.ModelStatusAvailable, Verified: true, Provider: "doubao-web", Capabilities: []string{store.CapabilityChat}}},
		store.ModelReconcileOptions{Prune: true})
	testutil.NoError(t, err)

	chat := func(answer, body string) (int, string) {
		stub.answer, stub.chatBody = answer, ""
		resp := e.do(t, http.MethodPost, "/doubao/v1/chat/completions", body, false)
		return resp.StatusCode, e.readBody(t, resp)
	}
	type completion struct {
		Object  string `json:"object"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Role      string `json:"role"`
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct{ Name, Arguments string }
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage map[string]int `json:"usage"`
	}
	decode := func(body string) completion {
		var out completion
		testutil.NoError(t, json.Unmarshal([]byte(body), &out))
		testutil.Equal(t, len(out.Choices), 1)
		return out
	}
	const user = `"messages":[{"role":"user","content":"hi"}]`
	const tools = `"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]`

	// A plain non-stream turn is one chat.completion object.
	status, body := chat("hello there", `{"model":"doubao",`+user+`}`)
	testutil.Equal(t, status, http.StatusOK)
	plain := decode(body)
	testutil.Equal(t, plain.Object, "chat.completion")
	testutil.Equal(t, plain.Choices[0].Message.Content, "hello there")
	testutil.Equal(t, plain.Choices[0].FinishReason, "stop")

	// A stream is chunks, a finish reason and the terminator.
	_, body = chat("hello there", `{"model":"doubao","stream":true,`+user+`}`)
	testutil.MustContainAll(t, body, `"object":"chat.completion.chunk"`, `"delta":{"content":"hello there"}`, `"finish_reason":"stop"`, "data: [DONE]")

	// A tool call ends with finish_reason tool_calls in both modes, also when
	// the model fenced the JSON it was asked to return bare.
	call := "```json\n{\"tool_calls\":[{\"name\":\"get_weather\",\"arguments\":{\"city\":\"Beijing\"}}]}\n```"
	_, body = chat(call, `{"model":"doubao",`+tools+`,`+user+`}`)
	called := decode(body)
	testutil.Equal(t, called.Choices[0].FinishReason, "tool_calls")
	testutil.Equal(t, len(called.Choices[0].Message.ToolCalls), 1)
	testutil.Equal(t, called.Choices[0].Message.ToolCalls[0].Function.Name, "get_weather")
	testutil.Equal(t, called.Choices[0].Message.ToolCalls[0].Function.Arguments, `{"city":"Beijing"}`)
	_, body = chat(call, `{"model":"doubao","stream":true,`+tools+`,`+user+`}`)
	testutil.MustContainAll(t, body, `"name":"get_weather"`, `"finish_reason":"tool_calls"`)

	// The follow-up turn must carry the call and its result, or the model is
	// asked the original question again and calls again.
	_, body = chat(`{"content":"It is sunny."}`, `{"model":"doubao",`+tools+`,"messages":[{"role":"user","content":"weather?"},`+
		`{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Beijing\"}"}}]},`+
		`{"role":"tool","tool_call_id":"call_1","content":"sunny 20C"}]}`)
	testutil.Equal(t, decode(body).Choices[0].Message.Content, "It is sunny.")
	testutil.MustContainAll(t, stub.chatBody, `[tool result of get_weather]\nsunny 20C`, `{\"city\":\"Beijing\"}`)

	// tool_choice "none" asks for a plain answer, and json_object is stated in
	// the prompt because the upstream has no field for it.
	chat("plain", `{"model":"doubao","tool_choice":"none",`+tools+`,`+user+`}`)
	testutil.MustNotContain(t, stub.chatBody, "Declared functions")
	chat(`{"a":1}`, `{"model":"doubao","response_format":{"type":"json_object"},`+user+`}`)
	testutil.MustContain(t, stub.chatBody, "Respond with one valid JSON object only")

	// An image in the standard chat shape is an attachment, not a part to drop:
	// the stub has no upload endpoint, so reaching it is a failure here.
	status, _ = chat("ok", `{"model":"doubao","messages":[{"role":"user","content":[{"type":"text","text":"what is this"},`+
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}]}]}`)
	testutil.NotEqual(t, status, http.StatusOK)
	testutil.Equal(t, stub.chatBody, "")
}
