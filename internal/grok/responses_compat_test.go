package grok

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"orchids-api/internal/responses"
	"orchids-api/internal/testutil"
	"strings"
	"testing"
)

func TestNativeResponsesPreservesOutputAndUsage(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, status := range []string{"completed", "incomplete"} {
			t.Run(status+"/stream="+fmt.Sprint(streaming), func(t *testing.T) {
				response := map[string]interface{}{
					"id": "resp_native", "object": "response", "model": "grok-4.6", "created_at": 42, "status": status,
					"output": []interface{}{
						map[string]interface{}{"id": "rs_a", "type": "reasoning", "summary": []interface{}{map[string]interface{}{"type": "summary_text", "text": "first plan"}}, "encrypted_content": "opaque-a"},
						map[string]interface{}{"id": "rs_b", "type": "reasoning", "summary": []interface{}{map[string]interface{}{"type": "summary_text", "text": "second plan"}}, "encrypted_content": "opaque-b"},
						map[string]interface{}{"id": "ws_a", "type": "web_search_call", "status": "completed", "action": map[string]interface{}{"type": "search", "query": "news"}},
						map[string]interface{}{"id": "fc_a", "type": "function_call", "call_id": "call_a", "name": "lookup", "arguments": `{"city":"Shanghai"}`},
						map[string]interface{}{"id": "msg_a", "type": "message", "role": "assistant", "content": []interface{}{
							map[string]interface{}{"type": "output_text", "text": " answer ", "annotations": []interface{}{map[string]interface{}{"type": "url_citation", "url": "https://example.com", "title": "source", "start_index": 0, "end_index": 6}}},
							map[string]interface{}{"type": "refusal", "refusal": "Cannot help."},
						}},
					},
					"usage": map[string]interface{}{"input_tokens": 10, "output_tokens": 4, "input_tokens_details": map[string]interface{}{"cached_tokens": 3}, "output_tokens_details": map[string]interface{}{"reasoning_tokens": 2}},
				}
				if status == "incomplete" {
					response["incomplete_details"] = map[string]interface{}{"reason": "max_output_tokens"}
				}
				raw, err := json.Marshal(response)
				testutil.NoError(t, err)
				input, contentType := string(raw), "application/json"
				if streaming {
					input = parityFrame("response."+status, map[string]interface{}{"response": response})
					contentType = "text/event-stream"
				}
				rec := httptest.NewRecorder()
				_, _, outcome := copyNativeCLIResponseAndCaptureModel(rec, strings.NewReader(input), contentType, "grok-4.6")
				testutil.NoError(t, outcome.Err)
				var got map[string]interface{}
				if streaming {
					err = responses.ReadSSE(strings.NewReader(rec.Body.String()), func(kind, data string) error {
						if kind == "response."+status {
							var event map[string]interface{}
							if err := json.Unmarshal([]byte(data), &event); err != nil {
								return err
							}
							got, _ = event["response"].(map[string]interface{})
						}
						return nil
					})
				} else {
					err = json.Unmarshal(rec.Body.Bytes(), &got)
				}
				testutil.NoError(t, err)
				for _, field := range []string{"output", "usage", "status", "incomplete_details"} {
					wantJSON, err := json.Marshal(response[field])
					testutil.NoError(t, err)
					gotJSON, err := json.Marshal(got[field])
					testutil.NoError(t, err)
					testutil.Equal(t, string(gotJSON), string(wantJSON))
				}
			})
		}
	}
}

func TestNativeResponsesCompatibilityPreservesCompleteSSEBytes(t *testing.T) {
	line := `event: response.completed
data: { "type":"response.completed", "id":"resp_ok", "response":{"id":"resp_ok","object":"response","created_at":42,"model":"grok-4.6","output":[{"type":"message","id":"msg_1","content":[{"type":"output_text","text":"ok","annotations":[]}]}]}}

`
	rec := httptest.NewRecorder()
	_, _, result := copyNativeCLIResponseAndCaptureModel(rec, strings.NewReader(line), "text/event-stream", "grok-4.6")
	testutil.Fail(t, result.Err != nil, result.Err)
	// The native relay is byte-transparent: an event that already carries every
	// field reaches the client exactly as the upstream wrote it, with no added
	// frame (the relay adds nothing).
	testutil.Equal(t, rec.Body.String(), line)
}

func TestNativeResponsesCompatibilitySupplementsStrictClientFields(t *testing.T) {
	stream := "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}}\n\n" +
		"event: response.output_text.done\ndata: {\"type\":\"response.output_text.done\",\"output_index\":0,\"delta\":\"ok\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"
	rec := httptest.NewRecorder()
	_, _, result := copyNativeCLIResponseAndCaptureModel(rec, strings.NewReader(stream), "text/event-stream", "grok-4.6")
	testutil.Fail(t, result.Err != nil, result.Err)
	got := rec.Body.String()
	for _, required := range []string{`"annotations":[]`, `"id":"item_1"`, `"item_id":"item_1"`, `"object":"response"`, `"model":"grok-4.6"`, `"output":[]`} {
		testutil.MustContain(t, got, required)
	}
}
