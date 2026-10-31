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

// TestNativeResponsesHideInternalSearchSubcalls replays the live x_search shape:
// completed custom_tool_call subcalls interleaved with a search call and the
// answer. The client must see neither the subcalls nor a gap in output_index.
func TestNativeResponsesHideInternalSearchSubcalls(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			output := []any{
				map[string]any{"type": "custom_tool_call", "id": "ctc_0", "call_id": "xs_call-1-0", "name": "x_keyword_search", "input": `{"query":"xai"}`, "status": "completed"},
				map[string]any{"type": "web_search_call", "id": "ws_1", "status": "completed"},
				map[string]any{"type": "custom_tool_call", "id": "ctc_2", "call_id": "xs_call-1-1", "name": "x_user_search", "input": `{"query":"xai"}`, "status": "completed"},
				map[string]any{"type": "message", "id": "msg_3", "role": "assistant", "status": "completed",
					"content": []any{map[string]any{"type": "output_text", "text": "summary", "annotations": []any{}}}},
			}
			upstreamResponse := map[string]any{"id": "resp_xs", "object": "response", "model": "grok-4.6", "status": "completed", "output": output}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
				for i, raw := range output {
					item := raw.(map[string]any)
					emit(map[string]any{"type": "response.output_item.added", "output_index": i, "item": item})
					if item["type"] == "custom_tool_call" {
						emit(map[string]any{"type": "response.custom_tool_call_input.delta", "output_index": i, "item_id": item["id"], "delta": item["input"]})
					}
					if item["type"] == "message" {
						emit(map[string]any{"type": "response.output_text.delta", "output_index": i, "item_id": item["id"], "content_index": 0, "delta": "summary"})
					}
					emit(map[string]any{"type": "response.output_item.done", "output_index": i, "item": item})
				}
				emit(map[string]any{"type": "response.completed", "response": upstreamResponse})
			}))
			defer upstream.Close()
			h, s, _ := setupValidationHandler(t)
			configureStoredResponseTestHandler(t, h, s, upstream)
			raw, _ := json.Marshal(map[string]any{"model": "grok-4.6", "input": "search x", "stream": stream,
				"tools": []any{map[string]any{"type": "web_search"}, map[string]any{"type": "x_search"}}})
			req := httptest.NewRequest(http.MethodPost, "/grok/v1/responses", strings.NewReader(string(raw)))
			req.Header.Set("Authorization", "Bearer owner-a")
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			middleware.APIKeyAuth(func(context.Context, string) (*middleware.APIKeyPrincipal, error) {
				return &middleware.APIKeyPrincipal{}, nil
			}, h.HandleResponses)(rec, req)
			testutil.Equal(t, rec.Code, http.StatusOK)
			body := rec.Body.String()
			testutil.False(t, strings.Contains(body, "custom_tool_call"), "internal subcalls must not reach the client")
			testutil.False(t, strings.Contains(body, "xs_call"), "no subcall id may leak")

			var response map[string]any
			if stream {
				indices := map[string]float64{}
				err := responses.ReadSSEBytes(strings.NewReader(body), func(_ string, raw []byte) error {
					var event map[string]any
					if err := json.Unmarshal(raw, &event); err != nil {
						return err
					}
					if item, ok := event["item"].(map[string]any); ok {
						indices[item["type"].(string)] = event["output_index"].(float64)
					}
					if event["type"] == "response.completed" {
						response, _ = event["response"].(map[string]any)
					}
					return nil
				})
				testutil.NoError(t, err)
				testutil.Equal(t, indices["web_search_call"], 0)
				testutil.Equal(t, indices["message"], 1)
			} else {
				testutil.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
			}
			kept := responses.InterfaceMaps(response["output"])
			testutil.Equal(t, len(kept), 2)
			testutil.Equal[any](t, kept[1]["type"], "message")
		})
	}
}
