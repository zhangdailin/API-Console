package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/proto"
)

func TestWarpFullContextAcrossPublicProtocols(t *testing.T) {
	for _, prefix := range []string{"/warp", "/warp/v1"} {
		for _, protocol := range []string{"chat/completions", "messages", "responses"} {
			for _, streaming := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", prefix, protocol, streaming), func(t *testing.T) {
					stub := newWarpRouteStub(t, http.StatusOK)
					e, _, _ := newWarpRouteE2E(t, stub, 1)
					var messages []map[string]string
					var transcript []string
					for i := 0; i < 12; i++ {
						user := fmt.Sprintf("history-user-%02d-", i) + strings.Repeat("u", 5000)
						answer := fmt.Sprintf("history-answer-%02d-", i) + strings.Repeat("a", 5000)
						messages = append(messages, map[string]string{"role": "user", "content": user}, map[string]string{"role": "assistant", "content": answer})
						transcript = append(transcript, "User: "+user, "Assistant: "+answer)
					}
					current := "CURRENT_LARGE_MESSAGE-" + strings.Repeat("界", 44000)
					messages = append(messages, map[string]string{"role": "user", "content": current})
					transcript = append(transcript, "User: "+current)
					body := map[string]any{"model": warpRouteModel, "stream": streaming, "messages": messages}
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
					resp := e.do(t, http.MethodPost, prefix+"/"+protocol, string(raw), false)
					out := e.readBody(t, resp)
					if resp.StatusCode != http.StatusOK || !strings.Contains(out, warpRouteAnswer) {
						t.Fatalf("full context request failed: status=%d", resp.StatusCode)
					}
					if stub.aiCalls.Load() != 1 || stub.otherCalls.Load() != 0 {
						t.Fatal("full context caused a retry or unrelated upstream request")
					}
					select {
					case encoded := <-stub.requests:
						var wire warpapi.Request
						if proto.Unmarshal(encoded, &wire) != nil {
							t.Fatal("invalid upstream protobuf")
						}
						inputs := wire.GetInput().GetUserInputs().GetInputs()
						if len(inputs) != 1 || inputs[0].GetUserQuery().GetQuery() != strings.Join(transcript, "\n\n") {
							t.Fatal("public protocol conversion dropped, trimmed or summarized supplied history")
						}
					default:
						t.Fatal("full context did not reach isolated upstream")
					}
				})
			}
		}
	}
}
