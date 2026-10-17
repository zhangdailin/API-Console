package grok

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/responses"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func TestNativeResponsesAgentTaskUsesPortableTextWithoutChangingReasoning(t *testing.T) {
	const task = "Check the permission boundary.\nKeep the existing tests."
	for _, kind := range []string{"message", "agent_message"} {
		for _, stream := range []bool{false, true} {
			t.Run(kind+"/"+map[bool]string{false: "json", true: "stream"}[stream], func(t *testing.T) {
				var received map[string]any
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					testutil.CheckNoError(t, json.NewDecoder(r.Body).Decode(&received))
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-agent\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}]}}\n\n")
					} else {
						w.Header().Set("Content-Type", "application/json")
						io.WriteString(w, `{"id":"resp-agent","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`)
					}
				}))
				defer server.Close()
				h, acc := parityBuildHandler(t, server)
				h.cfg.GrokLowLatency = true
				acc.ID, acc.GrokModels, acc.GrokModelsSyncedAt = 0, []string{"grok-4.6"}, time.Now()
				ctx := context.Background()
				testutil.NoError(t, h.lb.Store.CreateAccount(ctx, acc))
				testutil.NoError(t, h.lb.Store.CreateModel(ctx, &store.Model{Channel: "Grok", ModelID: "grok-4.6", Name: "grok-4.6", Status: store.ModelStatusAvailable, Verified: true}))
				item := map[string]any{"type": kind, "content": []any{
					map[string]any{"type": "input_text", "text": "Message Type: NEW_TASK\nTask name: /root/check\nSender: /root\nPayload:\n"},
					map[string]any{"type": "encrypted_content", "encrypted_content": task},
				}}
				if kind == "message" {
					item["role"] = "user"
				} else {
					item["author"], item["recipient"] = "/root", "/root/check"
				}
				body, err := json.Marshal(map[string]any{"model": "grok-4.6", "stream": stream, "input": []any{item, map[string]any{"type": "reasoning", "summary": []any{}, "encrypted_content": "opaque-reasoning-proof"}}})
				testutil.NoError(t, err)
				rec := httptest.NewRecorder()
				h.HandleResponses(rec, httptest.NewRequest(http.MethodPost, "/grok/v1/responses", strings.NewReader(string(body))))
				testutil.Equal(t, rec.Code, http.StatusOK)
				input := responses.InterfaceMaps(received["input"])
				testutil.Equal(t, len(input), 2)
				testutil.EqualAny(t, input[0]["type"], "message")
				testutil.EqualAny(t, input[0]["role"], "user")
				parts := responses.InterfaceMaps(input[0]["content"])
				testutil.EqualAny(t, parts[1]["type"], "input_text")
				testutil.EqualAny(t, parts[1]["text"], task)
				testutil.EqualAny(t, input[1]["type"], "reasoning")
				testutil.EqualAny(t, input[1]["encrypted_content"], "opaque-reasoning-proof")
			})
		}
	}
}

func TestGatewayCompactionKeepsAgentTaskText(t *testing.T) {
	var received map[string]any
	server := compactionUpstream(t, compactionTestSummary, &received)
	defer server.Close()
	h, _, cleanup := setupCompactionHandler(t, server)
	defer cleanup()
	body := `{"model":"grok-4.5","stream":false,"input":[{"type":"agent_message","content":[{"type":"encrypted_content","encrypted_content":"Keep the authentication regression tests."}]},{"type":"compaction_trigger"}]}`
	rec := httptest.NewRecorder()
	h.HandleResponses(rec, httptest.NewRequest(http.MethodPost, "/grok/v1/responses", strings.NewReader(body)))
	testutil.Equal(t, rec.Code, http.StatusOK)
	items := responses.InterfaceMaps(received["input"])
	if len(items) == 0 {
		t.Fatal("summary request lost its history")
	}
	testutil.EqualAny(t, items[0]["type"], "message")
	parts := responses.InterfaceMaps(items[0]["content"])
	testutil.EqualAny(t, parts[0]["type"], "input_text")
	testutil.EqualAny(t, parts[0]["text"], "Keep the authentication regression tests.")
}
