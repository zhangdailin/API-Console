package grok

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/middleware"
	"orchids-api/internal/responses"
	"orchids-api/internal/testutil"
)

// TestNativeResponsesLowerCodexExtensionTools drives a Codex turn with a
// freeform custom tool and apply_patch: Build receives functions only, the
// client receives custom_tool_call / apply_patch_call (stream and JSON), the
// envelope arguments never stream, and the continuation and stored resource
// keep the same identities.
func TestNativeResponsesLowerCodexExtensionTools(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			var upstreamResponse map[string]any
			var posted []map[string]any
			calls := []any{
				map[string]any{"type": "function_call", "id": "fc0", "call_id": "call0", "name": "exec", "arguments": `{"input":"ls -la"}`, "status": "completed"},
				map[string]any{"type": "function_call", "id": "fc1", "call_id": "call1", "name": "apply_patch",
					"arguments": `{"operation":{"type":"create_file","path":"a.txt","diff":"+hi"}}`, "status": "completed"},
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(upstreamResponse)
					return
				}
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
					return
				}
				posted = append(posted, payload)
				upstreamResponse = map[string]any{"id": fmt.Sprintf("resp_cx_%d", len(posted)), "object": "response", "model": "grok-4.6", "status": "completed", "output": calls}
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
					arguments := item["arguments"].(string)
					item["arguments"], item["status"] = "", "in_progress"
					emit(map[string]any{"type": "response.output_item.added", "output_index": i, "item": item})
					emit(map[string]any{"type": "response.function_call_arguments.delta", "output_index": i, "item_id": item["id"], "delta": arguments})
					emit(map[string]any{"type": "response.function_call_arguments.done", "output_index": i, "item_id": item["id"], "arguments": arguments})
					emit(map[string]any{"type": "response.output_item.done", "output_index": i, "item": raw})
				}
				emit(map[string]any{"type": "response.completed", "response": upstreamResponse})
			}))
			defer upstream.Close()
			h, s, _ := setupValidationHandler(t)
			configureStoredResponseTestHandler(t, h, s, upstream)
			request := func(method, path string, payload any, handler http.HandlerFunc) *httptest.ResponseRecorder {
				raw, _ := json.Marshal(payload)
				req := httptest.NewRequest(method, path, strings.NewReader(string(raw)))
				req.Header.Set("Authorization", "Bearer owner-a")
				req.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()
				middleware.APIKeyAuth(func(context.Context, string) (*middleware.APIKeyPrincipal, error) {
					return &middleware.APIKeyPrincipal{}, nil
				}, handler)(rec, req)
				return rec
			}
			assertOutput := func(rec *httptest.ResponseRecorder, sse bool) map[string]any {
				t.Helper()
				if rec.Code != http.StatusOK {
					t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
				}
				var response map[string]any
				if sse {
					testutil.False(t, strings.Contains(rec.Body.String(), "function_call_arguments"), "lowered argument events must not stream")
					testutil.False(t, strings.Contains(rec.Body.String(), `"type":"function_call"`), "no lowered call may reach the client")
					err := responses.ReadSSEBytes(strings.NewReader(rec.Body.String()), func(_ string, raw []byte) error {
						var event map[string]any
						if err := json.Unmarshal(raw, &event); err != nil {
							return err
						}
						if event["type"] == "response.completed" {
							response, _ = event["response"].(map[string]any)
						}
						return nil
					})
					testutil.NoError(t, err)
				} else {
					testutil.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
				}
				output := responses.InterfaceMaps(response["output"])
				testutil.Equal(t, len(output), 2)
				testutil.Equal[any](t, output[0]["type"], "custom_tool_call")
				testutil.Equal[any](t, output[0]["input"], "ls -la")
				testutil.Equal[any](t, output[1]["type"], "apply_patch_call")
				testutil.Equal[any](t, output[1]["operation"].(map[string]any)["path"], "a.txt")
				return response
			}

			tools := []any{
				map[string]any{"type": "custom", "name": "exec", "description": "Run a command."},
				map[string]any{"type": "apply_patch"},
			}
			first := assertOutput(request("POST", "/grok/v1/responses", map[string]any{
				"model": "grok-4.6", "input": "hello", "stream": stream, "tools": tools,
				"tool_choice": map[string]any{"type": "custom", "name": "exec"},
			}, h.HandleResponses), stream)

			sent := responses.InterfaceMaps(posted[0]["tools"])
			testutil.Equal(t, len(sent), 2)
			for _, tool := range sent {
				testutil.Equal[any](t, tool["type"], "function")
			}
			testutil.Equal[any](t, posted[0]["tool_choice"].(map[string]any)["name"], "exec")

			history := []any{}
			for _, raw := range first["output"].([]any) {
				item := raw.(map[string]any)
				outputType := "custom_tool_call_output"
				if item["type"] == "apply_patch_call" {
					outputType = "apply_patch_call_output"
				}
				history = append(history, item, map[string]any{"type": outputType, "call_id": item["call_id"], "output": "ok"})
			}
			assertOutput(request("POST", "/grok/v1/responses", map[string]any{
				"model": "grok-4.6", "input": history, "stream": stream, "tools": tools, "previous_response_id": "resp_cx_1",
			}, h.HandleResponses), stream)
			replayed := responses.InterfaceMaps(posted[1]["input"])
			testutil.Equal(t, len(replayed), 4)
			testutil.Equal[any](t, replayed[0]["type"], "function_call")
			testutil.Equal[any](t, replayed[0]["arguments"], `{"input":"ls -la"}`)
			testutil.Equal[any](t, replayed[1]["type"], "function_call_output")
			testutil.Equal[any](t, replayed[2]["name"], "apply_patch")

			assertOutput(request("GET", "/grok/v1/responses/resp_cx_2", nil, h.HandleResponseResource), false)
		})
	}
}
