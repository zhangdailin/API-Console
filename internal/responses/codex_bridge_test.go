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
