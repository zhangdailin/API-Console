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
				var upstreamResponse map[string]any
				posts, gets := 0, 0
				tools := []any{}
				parameters := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
					"count": map[string]any{"type": "integer"}, "text": map[string]any{"type": "string"},
				}}
				for i := range 7 {
					tools = append(tools, map[string]any{"type": "function", "name": fmt.Sprintf("plain%d", i), "parameters": map[string]any{"type": "object"}})
				}
				for _, ns := range []string{"alpha", "beta"} {
					tools = append(tools, map[string]any{"type": "namespace", "name": ns, "description": "group " + ns, "tools": []any{map[string]any{"type": "function", "name": "read", "description": "read data", "strict": true, "parameters": parameters}}})
				}
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodGet {
						gets++
						w.Header().Set("Content-Type", "application/json")
						_ = json.NewEncoder(w).Encode(upstreamResponse)
						return
					}
					posts++
					var payload map[string]any
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
							if tool["strict"] != true || !reflect.DeepEqual(tool["parameters"], parameters) {
								t.Error("schema changed")
							}
						}
						if wireNames[0] == wireNames[1] {
							t.Error("same-name namespace collision")
						}
						choice := payload["tool_choice"].(map[string]any)
						if choice["name"] != wireNames[0] || choice["namespace"] != nil {
							t.Errorf("choice=%v", choice)
						}
					} else {
						if payload["previous_response_id"] != "resp_ns_1" {
							t.Error("continuation id lost")
						}
						items := responses.InterfaceMaps(payload["input"])
						for i := range 2 {
							call, output := items[i*2], items[i*2+1]
							if call["name"] != wireNames[i] || call["namespace"] != nil || call["arguments"] != arguments || call["call_id"] != fmt.Sprintf("call%d", i) {
								t.Errorf("history=%v", call)
							}
							if output["call_id"] != call["call_id"] || output["output"] != "result" {
								t.Error("call result changed")
							}
						}
					}
					calls := []any{}
					for i, name := range wireNames {
						calls = append(calls, map[string]any{"type": "function_call", "id": fmt.Sprintf("fc%d", i), "call_id": fmt.Sprintf("call%d", i), "name": name, "arguments": arguments, "status": "completed"})
					}
					upstreamResponse = map[string]any{"id": fmt.Sprintf("resp_ns_%d", posts), "object": "response", "model": "grok-4.6", "status": "completed", "output": calls, "vendor_field": "preserved"}
					if posts == 1 {
						upstreamResponse["tools"], upstreamResponse["tool_choice"] = payload["tools"], payload["tool_choice"]
					}
					if !stream {
						w.Header().Set("Content-Type", "application/json")
						_ = json.NewEncoder(w).Encode(upstreamResponse)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					emit := func(event map[string]any) {
						raw, _ := json.Marshal(event)
						_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], raw)
					}
					for i, raw := range calls {
						item := responses.CloneStringInterfaceMap(raw.(map[string]any))
						item["arguments"] = ""
						emit(map[string]any{"type": "response.output_item.added", "output_index": i, "item": item})
						for _, part := range []string{arguments[:10], arguments[10:]} {
							emit(map[string]any{"type": "response.function_call_arguments.delta", "output_index": i, "item_id": fmt.Sprintf("fc%d", i), "delta": part})
						}
						emit(map[string]any{"type": "response.output_item.done", "output_index": i, "item": raw})
					}
					emit(map[string]any{"type": "response.completed", "response": upstreamResponse})
				}))
				defer upstream.Close()
				h, s, _ := setupValidationHandler(t)
				configureStoredResponseTestHandler(t, h, s, upstream)
				request := func(owner, method, path string, payload any, handler http.HandlerFunc) *httptest.ResponseRecorder {
					raw, _ := json.Marshal(payload)
					req := httptest.NewRequest(method, path, strings.NewReader(string(raw)))
					req.Header.Set("Authorization", "Bearer "+owner)
					req.Header.Set("Content-Type", "application/json")
					rec := httptest.NewRecorder()
					middleware.APIKeyAuth(func(context.Context, string) (*middleware.APIKeyPrincipal, error) {
						return &middleware.APIKeyPrincipal{}, nil
					}, handler)(rec, req)
					return rec
				}
				assertResponse := func(rec *httptest.ResponseRecorder, sse bool) map[string]any {
					t.Helper()
					if rec.Code != 200 {
						t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
					}
					var response map[string]any
					if sse {
						var deltas [2]string
						if err := responses.ReadSSEBytes(strings.NewReader(rec.Body.String()), func(_ string, raw []byte) error {
							var event map[string]any
							if err := json.Unmarshal(raw, &event); err != nil {
								return err
							}
							if item, ok := event["item"].(map[string]any); ok && item["type"] == "function_call" {
								if item["name"] != "read" || item["namespace"] == nil {
									t.Errorf("event identity=%v", item)
								}
							}
							if event["type"] == "response.function_call_arguments.delta" {
								deltas[int(event["output_index"].(float64))] += event["delta"].(string)
							}
							if event["type"] == "response.completed" {
								response, _ = event["response"].(map[string]any)
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
				firstPayload := map[string]any{"model": "grok-4.6", "input": "hello", "stream": stream, "tools": tools, "tool_choice": map[string]any{"type": "function", "namespace": "alpha", "name": "read"}, "vendor_field": "preserved"}
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
				history := []any{}
				for _, raw := range first["output"].([]any) {
					item := raw.(map[string]any)
					history = append(history, item, map[string]any{"type": "function_call_output", "call_id": item["call_id"], "output": "result"})
				}
				secondPayload := map[string]any{"model": "grok-4.6", "input": history, "stream": stream, "previous_response_id": "resp_ns_1", "vendor_field": "preserved"}
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
	wrapped := middleware.APIKeyAuth(func(context.Context, string) (*middleware.APIKeyPrincipal, error) {
		return &middleware.APIKeyPrincipal{}, nil
	}, h.HandleResponses)
	req := httptest.NewRequest("POST", "/grok/responses", strings.NewReader(`{"model":"grok-4.6","stream":true,"input":"hello","tools":[{"type":"namespace","name":"functions","tools":[{"type":"function","name":"read","parameters":{"type":"object"}}]}]}`))
	req.Header.Set("Authorization", "Bearer owner")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	wrapped(rec, req)
	// Tool-schema turns are held until validated. A truncated upstream stream
	// now fails before committing SSE or publishing its partial response ID.
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "upstream_error") || strings.Contains(rec.Body.String(), "resp_partial") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var lookupStatus int
	check := middleware.APIKeyAuth(func(context.Context, string) (*middleware.APIKeyPrincipal, error) {
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
	first := map[string]any{"output": []any{
		map[string]any{"type": "function_call", "call_id": "call-a", "name": "read", "namespace": "alpha", "arguments": arguments},
		map[string]any{"type": "function_call", "call_id": "call-b", "name": "read", "namespace": "beta", "arguments": arguments},
	}}
	h.captureReasoningReplayFromMap(context.Background(), "grok-4.6", "namespace-replay", first)
	payload := map[string]any{"input": []any{
		map[string]any{"type": "function_call_output", "call_id": "call-a", "output": "a"},
		map[string]any{"type": "function_call_output", "call_id": "call-b", "output": "b"},
	}}
	h.applyNativeReasoningReplay("grok-4.6", "namespace-replay", payload)
	var calls []map[string]any
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
