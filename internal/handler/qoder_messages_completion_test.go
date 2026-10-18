package handler

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/qoder"
	"orchids-api/internal/responses"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

// Exercise Qoder decoding and the actual Messages terminal frame together.
func TestQoderMessagesCompletionSemantics(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct {
			name, reason     string
			tool, terminated bool
			want             string
		}{
			{"normal text", "stop", false, true, "end_turn"},
			{"long text", "stop", false, true, "end_turn"},
			{"limited text", "length", false, true, "max_tokens"},
			{"limited tool turn", "length", true, true, "max_tokens"},
			{"complete tool turn", "tool_calls", true, true, "tool_use"},
			{"connection cut", "", false, false, ""},
		} {
			t.Run(fmt.Sprintf("%s/stream=%v", tc.name, stream), func(t *testing.T) {
				expectedText := "First part of the answer."
				maxTokens := 32
				if tc.name == "long text" {
					expectedText = strings.Repeat("中🙂\n", 10000) + "END_OF_ANSWER"
					maxTokens = 32000
				}
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					delta := map[string]any{"content": expectedText}
					if tc.tool {
						delta["tool_calls"] = []any{map[string]any{"index": 0, "id": "call_a", "function": map[string]any{"name": "report", "arguments": `{"result":"OK"}`}}}
					}
					inner, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": tc.reason}}})
					if err != nil {
						t.Error(err)
						return
					}
					outer, err := json.Marshal(map[string]any{"statusCodeValue": 200, "body": string(inner)})
					if err != nil {
						t.Error(err)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprintf(w, "data: %s\n\n", outer)
					if tc.terminated {
						_, _ = io.WriteString(w, "event:finish\ndata: {}\n\n")
					}
				}))
				defer server.Close()
				cfg := &config.Config{RequestTimeout: 10, MaxRetries: 3, QoderInferenceURL: server.URL}
				acc := &store.Account{AccountType: "qoder", QoderUserID: "test-user", QoderMachineID: "test-machine", QoderAccessToken: "test-access", QoderExpiresAt: time.Now().Add(time.Hour), QoderModelIDs: []string{"test-model"}}
				client := qoder.NewFromAccount(acc, cfg)
				defer client.Close()
				h := NewWithLoadBalancer(cfg, nil)
				h.client = client
				defer h.Close()
				body, err := json.Marshal(map[string]any{"model": "test-model", "max_tokens": maxTokens, "stream": stream, "messages": []any{map[string]any{"role": "user", "content": "Report the result."}}, "tools": []any{map[string]any{"name": "report", "input_schema": map[string]any{"type": "object"}}}})
				testutil.NoError(t, err)
				rec := httptest.NewRecorder()
				h.HandleMessages(rec, httptest.NewRequest(http.MethodPost, "/qoder/v1/messages", strings.NewReader(string(body))))
				testutil.Equal(t, calls.Load(), int32(1))
				if !tc.terminated {
					if stream {
						testutil.MustContain(t, rec.Body.String(), "event: error")
						testutil.False(t, strings.Contains(rec.Body.String(), "event: message_stop"))
					} else {
						testutil.True(t, rec.Code >= 400)
					}
					return
				}
				testutil.Equal(t, rec.Code, http.StatusOK)
				var reason string
				var text strings.Builder
				if stream {
					testutil.MustContain(t, rec.Body.String(), "event: message_stop")
					err = responses.ReadSSEBytes(rec.Body, func(event string, data []byte) error {
						if event == "content_block_delta" {
							var payload struct {
								Delta struct {
									Type string `json:"type"`
									Text string `json:"text"`
								} `json:"delta"`
							}
							if err := json.Unmarshal(data, &payload); err != nil {
								return err
							}
							if payload.Delta.Type == "text_delta" {
								text.WriteString(payload.Delta.Text)
							}
							return nil
						}
						if event != "message_delta" {
							return nil
						}
						var value struct {
							Delta struct {
								StopReason string `json:"stop_reason"`
							} `json:"delta"`
						}
						if err := json.Unmarshal(data, &value); err != nil {
							return err
						}
						reason = value.Delta.StopReason
						return nil
					})
					testutil.NoError(t, err)
				} else {
					var value struct {
						StopReason string `json:"stop_reason"`
						Content    []struct {
							Type string `json:"type"`
							Text string `json:"text"`
						} `json:"content"`
					}
					testutil.NoError(t, json.Unmarshal(rec.Body.Bytes(), &value))
					reason = value.StopReason
					for _, block := range value.Content {
						if block.Type == "text" {
							text.WriteString(block.Text)
						}
					}
				}
				testutil.Equal(t, reason, tc.want)
				if text.String() != expectedText {
					t.Fatalf("answer text lost: received %d bytes, want %d", text.Len(), len(expectedText))
				}
			})
		}
	}
}
