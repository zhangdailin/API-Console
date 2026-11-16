package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"orchids-api/internal/config"
	"orchids-api/internal/handler"
	"orchids-api/internal/provider"
	"orchids-api/internal/store"
	"orchids-api/internal/upstream"
)

// The deployed "mix" cache strategy must not add Anthropic cache markers to
// Warp's strict client-tool declarations or system/message content.
func TestWarpCacheStrategyMixDoesNotInjectMarkers(t *testing.T) {
	const system = "Keep the answer brief."
	for _, tc := range []struct {
		name string
		path string
		body string
	}{
		{
			name: "chat_completions",
			path: "/warp/v1/chat/completions",
			body: `{"model":"` + warpRouteModel + `","messages":[
				{"role":"system","content":"` + system + `"},
				{"role":"user","content":[{"type":"text","text":"` + warpRouteQuery + `"}]}],
				"tools":[{"type":"function","function":{"name":"run","description":"run a command","parameters":{"type":"object"}}}]}`,
		},
		{
			name: "anthropic_messages",
			path: "/warp/v1/messages",
			body: `{"model":"` + warpRouteModel + `","max_tokens":16,
				"system":[{"type":"text","text":"` + system + `"}],
				"messages":[{"role":"user","content":[{"type":"text","text":"` + warpRouteQuery + `"}]}],
				"tools":[{"name":"run","description":"run a command","input_schema":{"type":"object"}}]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := newWarpRouteStub(t, http.StatusOK)
			e, s, h := newWarpRouteE2E(t, stub, 1)
			// SetConfig replaces the entire snapshot, so retain the helper's
			// isolated upstream roots and admin/retry settings as well.
			h.SetConfig(&config.Config{
				AdminUser: "admin", AdminPass: "secret", AdminToken: "admintoken", AdminPath: "/admin",
				WarpBaseURL: stub.URL, WarpFirebaseBaseURL: stub.URL, WarpTokenBaseURL: stub.URL,
				WarpFirebaseAPIKey: "synthetic-project-config", MaxRetries: 3, RetryDelay: 1,
				CacheStrategy: "mix",
			})
			payloads := make(chan upstream.UpstreamRequest, 8)
			h.SetClientFactory(func(acc *store.Account, cfg *config.Config) handler.UpstreamClient {
				if cfg.CacheStrategy != "mix" {
					t.Error("request did not use the configured mix cache strategy")
				}
				spec, ok := provider.Lookup(acc.AccountType)
				if !ok || spec.NewClient == nil {
					t.Error("Warp provider is not registered")
					return nil
				}
				return &warpRoutePayloadObserver{next: spec.NewClient(acc, cfg, s), payloads: payloads}
			})

			resp := e.do(t, http.MethodPost, tc.path, tc.body, false)
			out := e.readBody(t, resp)
			if resp.StatusCode != http.StatusOK || !strings.Contains(out, warpRouteAnswer) {
				t.Fatalf("tool request with mix cache strategy = %d %s", resp.StatusCode, out)
			}
			if stub.aiCalls.Load() != 1 || stub.otherCalls.Load() != 0 {
				t.Fatalf("upstream calls: generation=%d other=%d", stub.aiCalls.Load(), stub.otherCalls.Load())
			}
			select {
			case payload := <-payloads:
				if len(payload.Tools) != 1 {
					t.Fatal("shared pipeline lost the client tool")
				}
				if tc.name == "chat_completions" {
					if len(payload.System) != 0 || len(payload.Messages) != 2 || payload.Messages[0].Role != "system" || payload.Messages[0].Content.Text != system {
						t.Fatal("shared pipeline lost the chat system message")
					}
				} else if len(payload.System) != 1 || payload.System[0].Text != system || len(payload.Messages) != 1 {
					t.Fatal("shared pipeline lost the native Anthropic system text or user message")
				}
				blocks := payload.Messages[len(payload.Messages)-1].Content.GetBlocks()
				if len(blocks) != 1 || blocks[0].Text != warpRouteQuery {
					t.Fatal("shared pipeline did not retain the user content block")
				}
				// Inspect the shared request before Warp's protobuf conversion:
				// checking only the wire could hide discarded cache markers.
				raw, err := json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(raw), `"cache_control"`) {
					t.Fatal("mix cache strategy injected cache_control into the Warp shared payload")
				}
			default:
				t.Fatal("request bypassed the shared provider pipeline")
			}
			if len(payloads) != 0 {
				t.Fatal("one client request produced multiple shared upstream payloads")
			}
		})
	}
}
