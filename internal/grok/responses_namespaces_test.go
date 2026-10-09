package grok

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"orchids-api/internal/middleware"
	"orchids-api/internal/responses"
)

func TestNativeResponsesNamespacesAndContinuation(t *testing.T) {
	for _, prefix := range []string{"/grok", "/grok/v1"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", prefix, stream), func(t *testing.T) {
				const arguments = `{"count":1000.0,"text":"unchanged"}`
				var wireNames []string
				var upstreamResponse map[string]interface{}
				posts, gets := 0, 0
				tools := []interface{}{}
				for i := 0; i < 7; i++ {
					tools = append(tools, map[string]interface{}{"type": "function", "name": fmt.Sprintf("plain%d", i), "parameters": map[string]interface{}{"type": "object"}})
				}
				for _, ns := range []string{"alpha", "beta"} {
					tools = append(tools, map[string]interface{}{"type": "namespace", "name": ns, "description": "group " + ns, "tools": []interface{}{map[string]interface{}{"type": "function", "name": "read", "description": "read data", "strict": true, "parameters": map[string]interface{}{"type": "object", "additionalProperties": false}}}})
				}
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodGet {
						gets++
						w.Header().Set("Content-Type", "application/json")
						_ = json.NewEncoder(w).Encode(upstreamResponse)
						return
					}
					posts++
					var payload map[string]interface{}
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Error(err)
						return
					}
					if payload["vendor_field"] != "preserved" {
						t.Error("native field lost")
					}
					if posts == 1 {
						flat := responses.InterfaceMaps(payload["tools"])
						if len(flat) != 9 {
							t.Errorf("tools=%v", flat)
							return
						}
						for _, tool := range flat {
							if tool["type"] != "function" {
								t.Error("namespace reached Build")
							}
						}
						for _, tool := range flat[7:] {
							wireNames = append(wireNames, tool["name"].(string))
							if tool["strict"] != true || !reflect.DeepEqual(tool["parameters"], map[string]interface{}{"type": "object", "additionalProperties": false}) {
								t.Error("schema changed")
							}
						}
						if wireNames[0] == wireNames[1] {
							t.Error("same-name namespace collision")
						}
						choice := payload["tool_choice"].(map[string]interface{})
						if choice["name"] != wireNames[0] || choice["namespace"] != nil {
							t.Errorf("choice=%v", choice)
						}
					} else {
						if payload["previous_response_id"] != "resp_ns_1" {
							t.Error("continuation id lost")
						}
						items := responses.InterfaceMaps(payload["input"])
						for i := 0; i < 2; i++ {
							call, output := items[i*2], items[i*2+1]
							if call["name"] != wireNames[i] || call["namespace"] != nil || call["arguments"] != arguments || call["call_id"] != fmt.Sprintf("call%d", i) {
								t.Errorf("history=%v", call)
							}
							if output["call_id"] != call["call_id"] || output["output"] != "result" {
								t.Error("call result changed")
							}
						}
					}
					calls := []interface{}{}
					for i, name := range wireNames {
						calls = append(calls, map[string]interface{}{"type": "function_call", "id": fmt.Sprintf("fc%d", i), "call_id": fmt.Sprintf("call%d", i), "name": name, "arguments": arguments, "status": "completed"})
					}
					upstreamResponse = map[string]interface{}{"id": fmt.Sprintf("resp_ns_%d", posts), "object": "response", "model": "grok-4.6", "status": "completed", "output": calls, "vendor_field": "preserved"}
					if posts == 1 {
						upstreamResponse["tools"], upstreamResponse["tool_choice"] = payload["tools"], payload["tool_choice"]
					}
					if !stream {
						w.Header().Set("Content-Type", "application/json")
						_ = json.NewEncoder(w).Encode(upstreamResponse)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					emit := func(event map[string]interface{}) {
						raw, _ := json.Marshal(event)
						_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], raw)
					}
					for i, raw := range calls {
						item := responses.CloneStringInterfaceMap(raw.(map[string]interface{}))
						item["arguments"] = ""
						emit(map[string]interface{}{"type": "response.output_item.added", "output_index": i, "item": item})
						for _, part := range []string{arguments[:10], arguments[10:]} {
							emit(map[string]interface{}{"type": "response.function_call_arguments.delta", "output_index": i, "item_id": fmt.Sprintf("fc%d", i), "delta": part})
						}
						emit(map[string]interface{}{"type": "response.output_item.done", "output_index": i, "item": raw})
					}
					emit(map[string]interface{}{"type": "response.completed", "response": upstreamResponse})
				}))
				defer upstream.Close()
				h, s, _ := setupValidationHandler(t)
				configureStoredResponseTestHandler(t, h, s, upstream)
				request := func(owner, method, path string, payload interface{}, handler http.HandlerFunc) *httptest.ResponseRecorder {
					raw, _ := json.Marshal(payload)
					req := httptest.NewRequest(method, path, strings.NewReader(string(raw)))
					req.Header.Set("Authorization", "Bearer "+owner)
					req.Header.Set("Content-Type", "application/json")
					rec := httptest.NewRecorder()
					middleware.APIKeyAuthWithRequest(func(*http.Request) bool { return true }, func(context.Context, string) (*middleware.APIKeyPrincipal, error) {
						return &middleware.APIKeyPrincipal{}, nil
					}, handler)(rec, req)
					return rec
				}
				assertResponse := func(rec *httptest.ResponseRecorder, sse bool) map[string]interface{} {
					t.Helper()
					if rec.Code != 200 {
						t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
					}
					var response map[string]interface{}
					if sse {
						var deltas [2]string
						if err := responses.ReadSSEBytes(strings.NewReader(rec.Body.String()), func(_ string, raw []byte) error {
							var event map[string]interface{}
							if err := json.Unmarshal(raw, &event); err != nil {
								return err
							}
							if item, ok := event["item"].(map[string]interface{}); ok && item["type"] == "function_call" {
								if item["name"] != "read" || item["namespace"] == nil {
									t.Errorf("event identity=%v", item)
								}
							}
							if event["type"] == "response.function_call_arguments.delta" {
								deltas[int(event["output_index"].(float64))] += event["delta"].(string)
							}
							if event["type"] == "response.completed" {
								response, _ = event["response"].(map[string]interface{})
							}
							return nil
						}); err != nil {
							t.Fatal(err)
						}
						for _, delta := range deltas {
							if delta != arguments {
								t.Errorf("arguments delta=%q", delta)
							}
						}
					} else if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
						t.Fatal(err)
					}
					calls := responses.InterfaceMaps(response["output"])
					if len(calls) != 2 {
						t.Fatalf("output=%v", response)
					}
					for i, item := range calls {
						if item["name"] != "read" || item["namespace"] != []string{"alpha", "beta"}[i] || item["call_id"] != fmt.Sprintf("call%d", i) || item["arguments"] != arguments {
							t.Errorf("output identity=%v", item)
						}
					}
					if response["vendor_field"] != "preserved" {
						t.Error("native response field lost")
					}
					return response
				}
				firstPayload := map[string]interface{}{"model": "grok-4.6", "input": "hello", "stream": stream, "tools": tools, "tool_choice": map[string]interface{}{"type": "function", "namespace": "alpha", "name": "read"}, "vendor_field": "preserved"}
				first := assertResponse(request("owner-a", "POST", prefix+"/responses", firstPayload, h.HandleResponses), stream)
				if !reflect.DeepEqual(first["tools"], tools) {
					t.Errorf("echoed tools=%v", first["tools"])
				}
				if !reflect.DeepEqual(first["tool_choice"], firstPayload["tool_choice"]) {
					t.Errorf("echoed choice=%v", first["tool_choice"])
				}
				assertResponse(request("owner-a", "GET", prefix+"/responses/resp_ns_1", nil, h.HandleResponseResource), false)
				if rec := request("owner-b", "GET", prefix+"/responses/resp_ns_1", nil, h.HandleResponseResource); rec.Code != 404 {
					t.Error("owner isolation failed")
				}
				history := []interface{}{}
				for _, raw := range first["output"].([]interface{}) {
					item := raw.(map[string]interface{})
					history = append(history, item, map[string]interface{}{"type": "function_call_output", "call_id": item["call_id"], "output": "result"})
				}
				secondPayload := map[string]interface{}{"model": "grok-4.6", "input": history, "stream": stream, "previous_response_id": "resp_ns_1", "vendor_field": "preserved"}
				assertResponse(request("owner-a", "POST", prefix+"/responses", secondPayload, h.HandleResponses), stream)
				assertResponse(request("owner-a", "GET", prefix+"/responses/resp_ns_2", nil, h.HandleResponseResource), false)
				items := request("owner-a", "GET", prefix+"/responses/resp_ns_2/input_items", nil, h.HandleResponseResource)
				if items.Code != 200 || !strings.Contains(items.Body.String(), `"namespace":"alpha"`) || strings.Contains(items.Body.String(), wireNames[0]) {
					t.Errorf("stored input=%s", items.Body.String())
				}
				if posts != 2 || gets != 2 {
					t.Errorf("upstream posts=%d gets=%d", posts, gets)
				}
			})
		}
	}
}

func TestNativeNamespaceStreamFailureDoesNotSaveOwnership(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_partial\",\"status\":\"in_progress\"}}\n\n")
	}))
	defer upstream.Close()
	h, s, _ := setupValidationHandler(t)
	configureStoredResponseTestHandler(t, h, s, upstream)
	wrapped := middleware.APIKeyAuthWithRequest(func(*http.Request) bool { return true }, func(context.Context, string) (*middleware.APIKeyPrincipal, error) {
		return &middleware.APIKeyPrincipal{}, nil
	}, h.HandleResponses)
	req := httptest.NewRequest("POST", "/grok/responses", strings.NewReader(`{"model":"grok-4.6","stream":true,"input":"hello","tools":[{"type":"namespace","name":"functions","tools":[{"type":"function","name":"read","parameters":{"type":"object"}}]}]}`))
	req.Header.Set("Authorization", "Bearer owner")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	wrapped(rec, req)
	if !strings.Contains(rec.Body.String(), "response.failed") {
		t.Fatal(rec.Body.String())
	}
	var lookupStatus int
	check := middleware.APIKeyAuthWithRequest(func(*http.Request) bool { return true }, func(context.Context, string) (*middleware.APIKeyPrincipal, error) {
		return &middleware.APIKeyPrincipal{}, nil
	}, func(w http.ResponseWriter, r *http.Request) {
		_, err := h.getStoredResponse(r, "resp_partial", responses.OwnerHash(r.Context()))
		if err == nil {
			lookupStatus = 200
		} else {
			lookupStatus = 404
		}
	})
	check(httptest.NewRecorder(), req)
	if lookupStatus != 404 {
		t.Fatal("partial stream persisted ownership")
	}
}

func TestNativeNamespaceReasoningReplayKeepsToolIdentity(t *testing.T) {
	h, _, _ := setupValidationHandler(t)
	const arguments = `{"number":1.0}`
	first := map[string]interface{}{"output": []interface{}{
		map[string]interface{}{"type": "function_call", "call_id": "call-a", "name": "read", "namespace": "alpha", "arguments": arguments},
		map[string]interface{}{"type": "function_call", "call_id": "call-b", "name": "read", "namespace": "beta", "arguments": arguments},
	}}
	h.captureReasoningReplayFromMap(context.Background(), "grok-4.6", "namespace-replay", first)
	payload := map[string]interface{}{"input": []interface{}{
		map[string]interface{}{"type": "function_call_output", "call_id": "call-a", "output": "a"},
		map[string]interface{}{"type": "function_call_output", "call_id": "call-b", "output": "b"},
	}}
	h.applyNativeReasoningReplay("grok-4.6", "namespace-replay", payload)
	var calls []map[string]interface{}
	for _, item := range responses.InterfaceMaps(payload["input"]) {
		if item["type"] == "function_call" {
			calls = append(calls, item)
		}
	}
	if len(calls) != 2 || calls[0]["namespace"] != "alpha" || calls[1]["namespace"] != "beta" {
		t.Fatalf("replay=%v", payload)
	}
	mappings, err := responses.NormalizeNamespacePayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, item := range responses.InterfaceMaps(payload["input"]) {
		if item["type"] != "function_call" {
			continue
		}
		if item["namespace"] != nil || item["arguments"] != arguments {
			t.Error("wire replay changed")
		}
		names = append(names, item["name"].(string))
		mappings.RestoreItem(item)
		if item["name"] != "read" || item["namespace"] == nil {
			t.Error("restore failed")
		}
	}
	if len(names) != 2 || names[0] == names[1] {
		t.Error("replayed groups collide")
	}
}
