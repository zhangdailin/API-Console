package responses

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/testutil"
)

// Codex declares freeform `custom` tools and the structured `apply_patch`
// tool. The Chat-only channels (WorkBuddy, Cline) refused them, so a Codex
// session on those channels failed with `tool type "custom" is not supported`.
// They are lowered to functions and raised back on the way out, as on Grok.

const codexToolsRequest = `{"model":"m","input":[
 {"type":"message","role":"user","content":"edit it"},
 {"type":"custom_tool_call","call_id":"call_old","name":"exec","input":"ls"},
 {"type":"custom_tool_call_output","call_id":"call_old","output":"a.go"}
],"tools":[
 {"type":"function","name":"shell","parameters":{"type":"object","properties":{}}},
 {"type":"custom","name":"exec","description":"Run a command","format":{"type":"grammar","syntax":"lark","definition":"start: /.+/"}},
 {"type":"apply_patch"}
]`

func chatReturning(t *testing.T, seen *map[string]any, reply string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		testutil.NoError(t, json.Unmarshal(body, seen), "upstream body")
		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		_, _ = io.WriteString(w, reply)
	}
}

func upstreamFunctionNames(body map[string]any) []string {
	var names []string
	for _, raw := range InterfaceSlice(body["tools"]) {
		tool, _ := raw.(map[string]any)
		fn, _ := tool["function"].(map[string]any)
		names = append(names, StreamString(tool["type"])+":"+StreamString(fn["name"]))
	}
	return names
}

func TestResponsesBridgeLowersCodexToolsAndRaisesTheCall(t *testing.T) {
	var seen map[string]any
	reply := `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_new","type":"function","function":{"name":"exec","arguments":"{\"input\":\"go test ./...\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	bridge := ResponsesBridgeHandler(chatReturning(t, &seen, reply), BridgeOptions{})
	rec := httptest.NewRecorder()
	bridge(rec, httptest.NewRequest(http.MethodPost, "/workbuddy/v1/responses", strings.NewReader(codexToolsRequest+`}`)))
	testutil.Equal(t, rec.Code, http.StatusOK)

	// Upstream sees functions only, and the replayed custom call as a tool call.
	testutil.Equal(t, strings.Join(upstreamFunctionNames(seen), ","), "function:shell,function:exec,function:apply_patch")
	raw, _ := json.Marshal(seen["messages"])
	testutil.MustContain(t, string(raw), `"tool_call_id":"call_old"`)
	testutil.MustNotContain(t, string(raw), "custom_tool_call")

	var out map[string]any
	testutil.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), "response")
	items := InterfaceMaps(out["output"])
	testutil.Equal(t, len(items), 1)
	testutil.Equal(t, items[0]["type"], "custom_tool_call")
	testutil.Equal(t, items[0]["name"], "exec")
	testutil.Equal(t, items[0]["call_id"], "call_new")
	testutil.Equal(t, items[0]["input"], "go test ./...")
}

// Streaming: the function envelope's argument deltas never reach the client;
// the custom call arrives whole on output_item.done and in response.completed.
func TestResponsesBridgeStreamRaisesLoweredCallsWhole(t *testing.T) {
	var seen map[string]any
	chunk := func(args string, first bool) string {
		call := `{"index":0,"function":{"arguments":` + args + `}}`
		if first {
			call = `{"index":0,"id":"call_new","type":"function","function":{"name":"apply_patch","arguments":` + args + `}}`
		}
		return `data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[` + call + `]}}]}` + "\n\n"
	}
	reply := chunk(`"{\"operation\":{\"type\":"`, true) + chunk(`"\"update_file\",\"path\":\"a.go\",\"diff\":\"@@\"}}"`, false) +
		`data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\ndata: [DONE]\n\n"
	bridge := ResponsesBridgeHandler(chatReturning(t, &seen, reply), BridgeOptions{})
	rec := httptest.NewRecorder()
	bridge(rec, httptest.NewRequest(http.MethodPost, "/cline/v1/responses", strings.NewReader(codexToolsRequest+`,"stream":true}`)))
	testutil.Equal(t, rec.Code, http.StatusOK)
	body := rec.Body.String()
	testutil.MustNotContain(t, body, "response.function_call_arguments.delta")
	testutil.MustNotContain(t, body, `"type":"function_call"`)

	var done, completed map[string]any
	for _, line := range strings.Split(body, "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok || data == "[DONE]" {
			continue
		}
		var event map[string]any
		testutil.NoError(t, json.Unmarshal([]byte(data), &event), "event")
		switch event["type"] {
		case "response.output_item.done":
			done, _ = event["item"].(map[string]any)
		case "response.completed":
			completed, _ = event["response"].(map[string]any)
		}
	}
	for _, item := range []map[string]any{done, InterfaceMaps(completed["output"])[0]} {
		testutil.Equal(t, item["type"], "apply_patch_call")
		testutil.Equal(t, item["call_id"], "call_new")
		operation, _ := item["operation"].(map[string]any)
		testutil.Equal(t, operation["type"], "update_file")
		testutil.Equal(t, operation["path"], "a.go")
	}
}

// The Codex Desktop failure behind `client-executed tool_search is not
// supported by this upstream`: the bridge declares the search as a function
// and hands the call back as a tool_search_call.
func TestResponsesBridgeServesClientToolSearch(t *testing.T) {
	var seen map[string]any
	reply := `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_s","type":"function","function":{"name":"tool_search","arguments":"{\"query\":\"calendar\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	bridge := ResponsesBridgeHandler(chatReturning(t, &seen, reply), BridgeOptions{})
	request := `{"model":"hy4free","input":"add a meeting","tools":[
	 {"type":"function","name":"shell","parameters":{"type":"object","properties":{}}},
	 {"type":"tool_search","execution":"client","description":"Find tools.","parameters":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}}]}`
	rec := httptest.NewRecorder()
	bridge(rec, httptest.NewRequest(http.MethodPost, "/workbuddy/v1/responses", strings.NewReader(request)))
	testutil.Equal(t, rec.Code, http.StatusOK)
	testutil.Equal(t, strings.Join(upstreamFunctionNames(seen), ","), "function:shell,function:tool_search")

	var out map[string]any
	testutil.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), "response")
	item := InterfaceMaps(out["output"])[0]
	testutil.Equal(t, item["type"], "tool_search_call")
	testutil.Equal(t, item["call_id"], "call_s")
	testutil.Equal(t, item["execution"], "client")
	arguments, _ := item["arguments"].(map[string]any)
	testutil.Equal(t, arguments["query"], "calendar")
}

// Codex 0.167 sends tool output nobody called for (no call_id, named by
// name/namespace) and declares tools part-way through with additional_tools.
// The bridge refused the first with `input[4].call_id is required` and the
// second as an unsupported item type.
func TestResponsesBridgeAcceptsUnpairedOutputsAndAdditionalTools(t *testing.T) {
	var seen map[string]any
	reply := `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	bridge := ResponsesBridgeHandler(chatReturning(t, &seen, reply), BridgeOptions{})
	request := `{"model":"hy4free","tools":[{"type":"function","name":"shell","parameters":{"type":"object","properties":{}}}],"input":[
	 {"type":"message","role":"developer","content":[{"type":"input_text","text":"be careful"}]},
	 {"type":"message","role":"user","content":[{"type":"input_text","text":"check slack"}]},
	 {"type":"function_call","call_id":"c1","name":"shell","arguments":"{}"},
	 {"type":"function_call_output","call_id":"c1","output":"done"},
	 {"type":"function_call_output","name":"notifications","namespace":"slack","output":"Alice mentioned you."},
	 {"type":"function_call_output","name":"screenshot","output":[{"type":"input_text","text":"see image"}]},
	 {"type":"additional_tools","role":"developer","tools":[{"type":"function","name":"slack_reply","defer_loading":true,"parameters":{"type":"object","properties":{}}}]}
	]}`
	rec := httptest.NewRecorder()
	bridge(rec, httptest.NewRequest(http.MethodPost, "/workbuddy/v1/responses", strings.NewReader(request)))
	testutil.Equal(t, rec.Code, http.StatusOK)

	testutil.Equal(t, strings.Join(upstreamFunctionNames(seen), ","), "function:shell,function:slack_reply")
	messages := InterfaceMaps(seen["messages"])
	testutil.Equal(t, len(messages), 6)
	raw, _ := json.Marshal(messages[4:])
	testutil.MustContain(t, string(raw), "Tool output from slack.notifications")
	testutil.MustContain(t, string(raw), "Alice mentioned you.")
	testutil.MustContain(t, string(raw), "Tool output from screenshot")
	testutil.MustContain(t, string(raw), "see image")
	for _, message := range messages[4:] {
		testutil.Equal(t, message["role"], "user")
	}
	testutil.Equal(t, messages[3]["role"], "tool")
	testutil.Equal(t, messages[3]["tool_call_id"], "c1")
}
