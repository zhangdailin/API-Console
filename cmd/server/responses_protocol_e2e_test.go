package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/handler"
	"orchids-api/internal/store"
	"orchids-api/internal/upstream"
)

type responsesProtocolRecorder struct {
	requests  []upstream.UpstreamRequest
	emitTools bool
}

func (c *responsesProtocolRecorder) SendRequestWithPayload(_ context.Context, req upstream.UpstreamRequest, emit func(upstream.SSEMessage), _ *debug.Logger) error {
	c.requests = append(c.requests, req)
	if c.emitTools {
		for _, call := range []struct{ id, name, value string }{{"probe_a", "probe_first", "FIRST"}, {"probe_b", "probe_second", "SECOND"}} {
			emit(upstream.SSEMessage{Type: "model", Event: map[string]interface{}{"type": "tool-call", "toolCallId": call.id, "toolName": call.name, "input": `{"value":"` + call.value + `"}`}})
		}
		emit(upstream.SSEMessage{Type: "model", Event: map[string]interface{}{"type": "finish", "finishReason": "tool_calls"}})
		return nil
	}
	for _, event := range []map[string]interface{}{{"type": "text-start"}, {"type": "text-delta", "delta": "Preserve task A and main.go"}, {"type": "finish", "finishReason": "stop"}} {
		emit(upstream.SSEMessage{Type: "model", Event: event})
	}
	return nil
}

func TestResponsesProtocolThroughAuthenticatedRoutes(t *testing.T) {
	for _, channel := range []string{"workbuddy", "qoder", "cline"} {
		t.Run(channel, func(t *testing.T) {
			cfg := &config.Config{AdminUser: "admin", AdminPass: "secret", AdminToken: "admintoken", AdminPath: "/admin", RequestTimeout: 10}
			mux, s, h := newRouteMux(t, "responses-protocol:"+channel+":", cfg)
			key := "sk-test-protocol"
			digest := sha256.Sum256([]byte(key))
			if err := s.CreateApiKey(context.Background(), &store.ApiKey{Name: "test", KeyHash: hex.EncodeToString(digest[:]), Enabled: true}); err != nil {
				t.Fatal(err)
			}
			if err := s.CreateModel(context.Background(), &store.Model{Channel: channel, ModelID: "m", Name: "m", Status: store.ModelStatusAvailable, Verified: true, Origin: "discovery"}); err != nil {
				t.Fatal(err)
			}
			if err := s.CreateAccount(context.Background(), &store.Account{Name: "test", AccountType: channel, Enabled: true, Weight: 1, ClineModelIDs: []string{`{"id":"m"}`}}); err != nil {
				t.Fatal(err)
			}
			client := &responsesProtocolRecorder{}
			h.SetClientFactory(func(*store.Account, *config.Config) handler.UpstreamClient { return client })
			call := func(path, body string) *httptest.ResponseRecorder {
				r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
				r.Header.Set("Authorization", "Bearer "+key)
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, r)
				return w
			}
			path := "/" + channel + "/v1/responses"
			created := call(path, `{"model":"m","input":"task A","max_output_tokens":17,"temperature":0,"text":{"format":{"type":"json_object"}}}`)
			if created.Code != 200 || len(client.requests) != 1 {
				t.Fatalf("create=%d %s calls=%d", created.Code, created.Body.String(), len(client.requests))
			}
			got := client.requests[0]
			if got.ResponseText["format"] == nil || got.MaxTokens == nil || *got.MaxTokens != 17 || got.Temperature == nil || *got.Temperature != 0 {
				t.Fatalf("controls lost: %#v", got)
			}
			compressed := call(path+"/compact", `{"model":"m","input":"task A"}`)
			if compressed.Code != 200 {
				t.Fatalf("compact=%d %s", compressed.Code, compressed.Body.String())
			}
			var result map[string]interface{}
			if err := json.Unmarshal(compressed.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result["object"] != "response.compaction" {
				t.Fatalf("not compact: %v", result)
			}
			input := append(result["output"].([]interface{}), map[string]interface{}{"type": "message", "role": "user", "content": "continue"})
			body, _ := json.Marshal(map[string]interface{}{"model": "m", "input": input})
			// A summary made on /provider/v1 must continue on /provider as well.
			continued := call("/"+channel+"/responses", string(body))
			if continued.Code != 200 || len(client.requests) != 3 {
				t.Fatalf("continue=%d %s calls=%d", continued.Code, continued.Body.String(), len(client.requests))
			}
			if !strings.Contains(client.requests[2].Messages[0].Content.GetText(), "main.go") {
				t.Fatalf("summary lost: %#v", client.requests[2])
			}
			unversioned := call("/"+channel+"/responses/compact", `{"model":"m","input":[{"role":"user","content":"task A"}]}`)
			if unversioned.Code != 200 {
				t.Fatalf("unversioned compact=%d %s", unversioned.Code, unversioned.Body.String())
			}
			if err := json.Unmarshal(unversioned.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			body, _ = json.Marshal(map[string]interface{}{"model": "m", "input": append(result["output"].([]interface{}), map[string]interface{}{"role": "user", "content": "continue"})})
			// The reverse direction must retain the same compaction identity.
			unversionedNext := call(path, string(body))
			if unversionedNext.Code != 200 || len(client.requests) != 5 {
				t.Fatalf("unversioned continuation=%d %s calls=%d", unversionedNext.Code, unversionedNext.Body.String(), len(client.requests))
			}
			client.emitTools = true
			toolStream := call(path, `{"model":"m","input":"Call both probes","stream":true,"tools":[{"type":"function","name":"probe_first","parameters":{"type":"object"}},{"type":"function","name":"probe_second","parameters":{"type":"object"}}]}`)
			wire := toolStream.Body.String()
			if toolStream.Code != 200 || !strings.Contains(wire, "event: response.completed") || strings.Contains(wire, "event: response.failed") || !strings.Contains(wire, `"call_id":"probe_a"`) || !strings.Contains(wire, `"call_id":"probe_b"`) || strings.Count(wire, "event: response.function_call_arguments.done") != 2 {
				t.Fatalf("two tools lost across Handler/Chat/Responses: %d %s", toolStream.Code, wire)
			}
		})
	}
}
