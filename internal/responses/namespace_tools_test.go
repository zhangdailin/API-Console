package responses

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/store"
)

// Reproduce the real tools[7] rejection, then exercise both response formats
// and a second turn with the exact client-visible function identities.
func TestCodexNamespaceBridgeRoundTrip(t *testing.T) {
	for _, provider := range []string{"workbuddy", "qoder", "cline"} {
		for _, prefix := range []string{"", "/v1"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s%s/stream=%v", provider, prefix, stream), func(t *testing.T) {
					tools := []interface{}{}
					for i := 0; i < 7; i++ {
						tools = append(tools, map[string]interface{}{"type": "function", "name": fmt.Sprintf("plain_%d", i), "parameters": map[string]interface{}{"type": "object"}})
					}
					for _, ns := range []string{"files", "remote"} {
						tools = append(tools, map[string]interface{}{"type": "namespace", "name": ns, "description": "Read tools", "tools": []interface{}{map[string]interface{}{"type": "function", "name": "read", "parameters": map[string]interface{}{"type": "object", "properties": map[string]interface{}{"path": map[string]interface{}{"type": "string"}}}}}})
					}
					calls := 0
					responseStore := store.NewMemoryResponseStore(0)
					bridge := ResponsesBridgeHandler(func(w http.ResponseWriter, r *http.Request) {
						calls++
						var req map[string]interface{}
						if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
							t.Fatal(err)
						}
						defs := InterfaceMaps(req["tools"])
						if len(defs) != 9 {
							t.Fatalf("tools=%v", defs)
						}
						first := defs[7]["function"].(map[string]interface{})["name"].(string)
						second := defs[8]["function"].(map[string]interface{})["name"].(string)
						if first == second {
							t.Fatal("namespaces collided")
						}
						choice := req["tool_choice"].(map[string]interface{})["function"].(map[string]interface{})
						if choice["name"] != first {
							t.Fatalf("choice=%v", choice)
						}
						if calls == 2 {
							messages := InterfaceMaps(req["messages"])
							toolCall := InterfaceMaps(messages[0]["tool_calls"])[0]
							if toolCall["function"].(map[string]interface{})["name"] != first || toolCall["id"] != "call_a" || messages[1]["tool_call_id"] != "call_a" {
								t.Fatalf("history=%v", messages)
							}
						}
						if !stream {
							_, _ = fmt.Fprintf(w, `{"choices":[{"finish_reason":"tool_calls","message":{"tool_calls":[{"id":"call_a","function":{"name":%q,"arguments":"{\"path\":\"a\"}"}},{"id":"call_b","function":{"name":%q,"arguments":"{\"path\":\"b\"}"}}]}}]}`, first, second)
							return
						}
						w.Header().Set("Content-Type", "text/event-stream")
						for index, name := range []string{first, second} {
							_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":%d,\"id\":\"call_%c\",\"function\":{\"name\":%q,\"arguments\":\"{\\\"path\\\":\"}}]}}]}\n\n", index, 'a'+index, name)
						}
						for index := range []int{0, 1} {
							_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":%d,\"function\":{\"arguments\":\"\\\"%c\\\"}\"}}]}}]}\n\n", index, 'a'+index)
						}
						_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
					}, BridgeOptions{Store: responseStore})
					payload := map[string]interface{}{"model": "m", "input": "read both", "tools": tools, "stream": stream, "tool_choice": map[string]interface{}{"type": "function", "namespace": "files", "name": "read"}}
					for turn := 0; turn < 2; turn++ {
						body, _ := json.Marshal(payload)
						rec := httptest.NewRecorder()
						bridge(rec, httptest.NewRequest("POST", "/"+provider+prefix+"/responses", strings.NewReader(string(body))))
						if rec.Code != 200 {
							t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
						}
						var response map[string]interface{}
						if !stream {
							_ = json.Unmarshal(rec.Body.Bytes(), &response)
						} else {
							err := ConsumeSSE(strings.NewReader(rec.Body.String()), func(frame SSEEvent) error {
								if string(frame.Data()) == "[DONE]" {
									return nil
								}
								var event map[string]interface{}
								if err := json.Unmarshal(frame.Data(), &event); err != nil {
									return err
								}
								if item, ok := event["item"].(map[string]interface{}); ok && item["type"] == "function_call" && (item["name"] != "read" || item["namespace"] == nil) {
									t.Fatalf("bad event: %v", event)
								}
								if event["type"] == "response.completed" {
									response = event["response"].(map[string]interface{})
								}
								return nil
							})
							if err != nil {
								t.Fatal(err)
							}
						}
						out := InterfaceMaps(response["output"])
						if len(out) != 2 || out[0]["namespace"] != "files" || out[1]["namespace"] != "remote" || out[0]["name"] != "read" || out[0]["arguments"] != `{"path":"a"}` {
							t.Fatalf("response=%v wire=%s", response, rec.Body.String())
						}
						saved, err := responseStore.GetStoredResponse(context.Background(), response["id"].(string), "anonymous")
						if err != nil || !strings.Contains(string(saved.Body), `"namespace":"files"`) {
							t.Fatalf("client identity lost in stored response: %v %v", saved, err)
						}
						payload["input"] = []interface{}{out[0], map[string]interface{}{"type": "function_call_output", "call_id": "call_a", "output": "found"}, map[string]interface{}{"role": "user", "content": "continue"}}
					}
					if calls != 2 {
						t.Fatalf("upstream calls=%d", calls)
					}
				})
			}
		}
	}
}

func TestNamespaceAdaptationPreservesEvidenceAndRejectsMalformedTools(t *testing.T) {
	parse := func(body string) CreateRequest {
		var req CreateRequest
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatal(err)
		}
		return req
	}
	valid := `{"tools":[{"type":"namespace","name":"files","tools":[{"type":"function","name":"read","strict":true,"parameters":{"type":"object","anyOf":[{"required":["count"]}],"properties":{"count":{"type":"integer"}}}}]}],"input":[{"type":"function_call","name":"read","namespace":"files","call_id":"c","arguments":"{\"count\":1000.0}"}]}`
	req := parse(valid)
	original := req
	before, _ := json.Marshal(req)
	aliases, err := NormalizeBridgedNamespaces(&req)
	if err != nil {
		t.Fatal(err)
	}
	input := InterfaceMaps(req.Input)[0]
	if input["arguments"] != `{"count":1000.0}` || input["call_id"] != "c" || input["namespace"] != nil {
		t.Fatalf("history changed: %v", input)
	}
	schema, _ := json.Marshal(req.Tools[0]["parameters"])
	originalSchema, _ := json.Marshal(InterfaceMaps(original.Tools[0]["tools"])[0]["parameters"])
	if string(schema) != string(originalSchema) || req.Tools[0]["strict"] != true {
		t.Fatal("schema or strict flag changed")
	}
	originalAfter, _ := json.Marshal(original)
	if string(before) != string(originalAfter) {
		t.Fatal("caller-owned request was mutated")
	}
	// The normalizer must not mutate aliased caller-owned objects.
	originalInput, _ := json.Marshal(original.Input)
	if !strings.Contains(string(originalInput), `"namespace":"files"`) {
		t.Fatal("caller input was mutated")
	}
	aliases.RestoreItem(input)
	if input["namespace"] != "files" || input["name"] != "read" {
		t.Fatalf("roundtrip=%v", input)
	}

	for _, body := range []string{
		`{"tools":[{"type":"namespace","name":"n","tools":[null]}]}`,
		`{"tools":[{"type":"namespace","name":"n","tools":[{"type":"function","name":"f"},{"type":"function","name":"f"}]}]}`,
		`{"tools":[{"type":"namespace","name":"n","tools":[{"type":"namespace","name":"nested","tools":[]}]}]}`,
		`{"tools":[{"type":"namespace","name":"n","tools":[{"type":"custom","name":"patch"}]}]}`,
		`{"tools":[{"type":"namespace","name":"n","tools":[{"type":"function","name":"f"}]}],"tool_choice":{"type":"function","namespace":"missing","name":"f"}}`,
		`{"input":[{"type":"function_call","namespace":false,"name":"f"}]}`,
	} {
		req := parse(body)
		if _, err := NormalizeBridgedNamespaces(&req); err == nil {
			t.Fatalf("accepted malformed request: %s", body)
		}
	}
}
