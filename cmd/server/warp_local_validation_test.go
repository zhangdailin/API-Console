package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestWarpLocalValidationIsExplicitAcrossPublicProtocols(t *testing.T) {
	for _, prefix := range []string{"/warp", "/warp/v1"} {
		for _, protocol := range []string{"chat/completions", "messages", "responses"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", prefix, protocol, stream), func(t *testing.T) {
					stub := newWarpRouteStub(t, http.StatusOK)
					e, s, _ := newWarpRouteE2E(t, stub, 1)
					messages := []any{
						map[string]any{"role": "assistant", "content": "private-historical-answer"},
						map[string]any{"role": "user", "content": "private-user-prompt"},
					}
					body := map[string]any{"model": warpRouteModel, "stream": stream, "messages": messages}
					if protocol == "messages" {
						body["max_tokens"] = 128
					}
					if protocol == "responses" {
						delete(body, "messages")
						body["input"] = messages
					}
					raw, err := json.Marshal(body)
					if err != nil {
						t.Fatal(err)
					}
					response := e.do(t, http.MethodPost, prefix+"/"+protocol, string(raw), false)
					out := e.readBody(t, response)
					if response.StatusCode != 400 || !strings.Contains(out, "gateway rejected this Warp conversation") || !strings.Contains(out, "No upstream generation was made") {
						t.Fatalf("local rejection was not explicit: status=%d body=%s", response.StatusCode, out)
					}
					for _, forbidden := range []string{"The upstream rejected", "private-historical-answer", "private-user-prompt", "synthetic-warp-access"} {
						if strings.Contains(out, forbidden) {
							t.Fatal("local rejection was misattributed or leaked request content")
						}
					}
					if stub.aiCalls.Load() != 0 || stub.otherCalls.Load() != 0 {
						t.Fatal("local validation reached upstream or retried")
					}
					accounts, err := s.ListAccounts(t.Context())
					if err != nil || len(accounts) != 1 || !accounts[0].Enabled || accounts[0].StatusCode != "" {
						t.Fatal("local request rejection poisoned the account")
					}
				})
			}
		}
	}
}
