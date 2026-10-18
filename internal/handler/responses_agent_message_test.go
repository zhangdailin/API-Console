package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/config"
	"orchids-api/internal/responses"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

func TestResponsesAgentTaskReachesSharedUpstream(t *testing.T) {
	const task = "Check login permissions.\nPreserve the existing authentication checks."
	for _, channel := range []string{"workbuddy", "qoder", "cline"} {
		for _, stream := range []bool{false, true} {
			for _, kind := range []string{"message", "agent_message"} {
				t.Run(channel+"/"+kind+"/"+map[bool]string{false: "json", true: "stream"}[stream], func(t *testing.T) {
					h, s, _ := setupModelValidationHandler(t)
					h.SetConfig(&config.Config{RequestTimeout: 5})
					t.Cleanup(h.Close)
					ctx := context.Background()
					testutil.NoError(t, s.CreateAccount(ctx, &store.Account{AccountType: channel, Enabled: true, Weight: 1,
						QoderModelIDs: []string{"deepseek-test"}, WorkBuddyAccessToken: "test-access", QoderAccessToken: "test-access", ClineAccessToken: "test-access"}))
					testutil.NoError(t, s.CreateModel(ctx, &store.Model{Channel: channel, ModelID: "deepseek-test", Name: "deepseek-test", Status: store.ModelStatusAvailable, Verified: true}))
					client := &fakePayloadClient{eventsByOp: [][]upstream.SSEMessage{{
						{Type: "model.text-delta", Event: map[string]any{"delta": "ok"}},
						{Type: "model.finish", Event: map[string]any{"finishReason": "end_turn"}},
					}}}
					h.SetClientFactory(func(*store.Account, *config.Config) UpstreamClient { return client })
					item := map[string]any{"type": kind, "content": []any{
						map[string]any{"type": "input_text", "text": "Message Type: NEW_TASK\nTask name: /root/login\nSender: /root\nPayload:\n"},
						map[string]any{"type": "encrypted_content", "encrypted_content": task},
					}}
					if kind == "message" {
						item["role"] = "user"
					} else {
						item["author"], item["recipient"] = "/root", "/root/login"
					}
					raw, err := json.Marshal(map[string]any{"model": "deepseek-test", "input": []any{item}, "stream": stream})
					testutil.NoError(t, err)
					bridge := responses.ResponsesBridgeHandler(h.HandleMessages, responses.BridgeOptions{Store: s, ExecuteChat: h.HandleChatRequest})
					rec := httptest.NewRecorder()
					bridge(rec, httptest.NewRequest(http.MethodPost, "/"+channel+"/v1/responses", strings.NewReader(string(raw))))
					testutil.Equal(t, rec.Code, http.StatusOK)
					calls := client.snapshotCalls()
					if len(calls) != 1 {
						t.Fatalf("upstream calls=%d, response=%s", len(calls), rec.Body.String())
					}
					if len(calls[0].Messages) != 1 {
						t.Fatal("delegated message was dropped")
					}
					testutil.MustContain(t, calls[0].Messages[0].Content.ExtractText(), task)
					testutil.Equal(t, calls[0].Messages[0].Role, "user")
				})
			}
		}
	}
}
