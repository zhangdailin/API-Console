package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/handler"
	"orchids-api/internal/provider"
	"orchids-api/internal/store"
)

func TestWarpRawControlsRejectedBeforeSelectionRefreshOrGeneration(t *testing.T) {
	controls := []struct{ name, value string }{
		{"n", "1"}, {"seed", "0"}, {"penalties", "{}"}, {"presence_penalty", "0"}, {"frequency_penalty", "0"},
		{"logprobs", "false"}, {"top_logprobs", "0"}, {"logit_bias", "{}"}, {"web_search_options", "{}"},
		{"output_config", "{}"}, {"reasoning", "{}"}, {"reasoning_effort", `"low"`},
		{"functions", "[]"},
		{"response_format", "{}"}, {"stream_options", `{"include_usage":null}`}, {"store", "true"}, {"client_metadata", "{}"},
		{"conversation_id", `""`}, {"prompt_cache_key", `""`}, {"mcp_servers", "[]"},
		{"include", "[]"}, {"unknown_control", "{}"}, {"Model", `"ignored"`},
	}
	for _, expired := range []bool{false, true} {
		t.Run(fmt.Sprintf("expired_credentials=%t", expired), func(t *testing.T) {
			stub := newWarpRouteStub(t, http.StatusOK)
			e, s, h := newWarpRouteE2E(t, stub, 1)
			if expired {
				accounts, err := s.ListAccounts(context.Background())
				if err != nil || len(accounts) != 1 {
					t.Fatalf("load planted account: count=%d err=%v", len(accounts), err)
				}
				accounts[0].WarpExpiresAt = time.Now().Add(-time.Hour)
				if err := s.UpdateAccount(context.Background(), accounts[0]); err != nil {
					t.Fatal(err)
				}
			}
			var clients atomic.Int64
			h.SetClientFactory(func(acc *store.Account, cfg *config.Config) handler.UpstreamClient {
				clients.Add(1)
				spec, ok := provider.Lookup(acc.AccountType)
				if !ok || spec.NewClient == nil {
					t.Error("Warp client is not registered")
					return nil
				}
				return spec.NewClient(acc, cfg, s)
			})
			for _, prefix := range []string{"/warp", "/warp/v1"} {
				for _, control := range controls {
					// A null control must be rejected by presence too, even when the
					// shared request type would decode it as its default value.
					for _, value := range []string{control.value, "null"} {
						t.Run(prefix+"/"+control.name+"="+value, func(t *testing.T) {
							body := `{"model":"` + warpRouteModel + `","messages":[{"role":"user","content":"hello"}],"` + control.name + `":` + value + `}`
							resp := e.do(t, http.MethodPost, prefix+"/chat/completions", body, false)
							out := e.readBody(t, resp)
							if resp.StatusCode != http.StatusBadRequest || !strings.Contains(out, "invalid_request_error") {
								t.Fatalf("unsupported raw control = %d %s", resp.StatusCode, out)
							}
							if clients.Load() != 0 || stub.aiCalls.Load() != 0 || stub.otherCalls.Load() != 0 {
								t.Fatalf("raw control reached account client/paid generation/refresh: clients=%d generation=%d other=%d", clients.Load(), stub.aiCalls.Load(), stub.otherCalls.Load())
							}
						})
					}
				}
			}
		})
	}
}

func TestWarpRawNestedAndShapeFieldsRejectedBeforeUpstream(t *testing.T) {
	stub := newWarpRouteStub(t, http.StatusOK)
	e, s, h := newWarpRouteE2E(t, stub, 1)
	accounts, err := s.ListAccounts(context.Background())
	if err != nil || len(accounts) != 1 {
		t.Fatalf("load planted account: count=%d err=%v", len(accounts), err)
	}
	accounts[0].WarpExpiresAt = time.Now().Add(-time.Hour)
	if err := s.UpdateAccount(context.Background(), accounts[0]); err != nil {
		t.Fatal(err)
	}
	var clients atomic.Int64
	h.SetClientFactory(func(acc *store.Account, cfg *config.Config) handler.UpstreamClient {
		clients.Add(1)
		spec, ok := provider.Lookup(acc.AccountType)
		if !ok || spec.NewClient == nil {
			t.Error("Warp client is not registered")
			return nil
		}
		return spec.NewClient(acc, cfg, s)
	})
	cases := map[string]string{
		"message_name":        `{"role":"user","content":"hello","name":""}`,
		"message_unknown":     `{"role":"user","content":"hello","unknown":null}`,
		"message_metadata":    `{"role":"user","content":"hello","metadata":{}}`,
		"empty_tool_calls":    `{"role":"user","content":"hello","tool_calls":[]}`,
		"empty_tool_call_id":  `{"role":"user","content":"hello","tool_call_id":""}`,
		"empty_reasoning":     `{"role":"user","content":"hello","reasoning_content":""}`,
		"null_refusal":        `{"role":"user","content":"hello","refusal":null}`,
		"message_key_case":    `{"role":"user","content":"hello","Content":"ignored"}`,
		"block_unknown":       `{"role":"user","content":[{"type":"text","text":"hello","unknown":null}]}`,
		"block_cache_control": `{"role":"user","content":[{"type":"text","text":"hello","cache_control":{}}]}`,
		"block_annotations":   `{"role":"user","content":[{"type":"text","text":"hello","annotations":[]}]}`,
		"block_image_url":     `{"role":"user","content":[{"type":"text","text":"hello","image_url":null}]}`,
		"block_key_case":      `{"role":"user","content":[{"type":"text","text":"hello","Text":"ignored"}]}`,
		"null_message":        "null", "array_message": "[]", "string_message": `"hello"`,
		"missing_role": `{"content":"hello"}`, "null_role": `{"role":null,"content":"hello"}`,
		"missing_content": `{"role":"user"}`, "null_content": `{"role":"user","content":null}`,
		"object_content":     `{"role":"user","content":{"type":"text","text":"hello"}}`,
		"null_block":         `{"role":"user","content":[null]}`,
		"missing_block_type": `{"role":"user","content":[{"text":"hello"}]}`,
		"missing_block_text": `{"role":"user","content":[{"type":"text"}]}`,
		"null_block_type":    `{"role":"user","content":[{"type":null,"text":"hello"}]}`,
		"null_block_text":    `{"role":"user","content":[{"type":"text","text":null}]}`,
	}
	bodies := make(map[string]string, len(cases)+10)
	for name, message := range cases {
		bodies[name] = `{"model":"` + warpRouteModel + `","messages":[` + message + `]}`
	}
	for name, raw := range map[string]string{
		"null_stream":     `{"model":"` + warpRouteModel + `","messages":[{"role":"user","content":"hello"}],"stream":null}`,
		"string_stream":   `{"model":"` + warpRouteModel + `","messages":[{"role":"user","content":"hello"}],"stream":"false"}`,
		"null_model":      `{"model":null,"messages":[{"role":"user","content":"hello"}]}`,
		"null_messages":   `{"model":"` + warpRouteModel + `","messages":null}`,
		"object_messages": `{"model":"` + warpRouteModel + `","messages":{}}`,
	} {
		bodies[name] = raw
	}
	for _, prefix := range []string{"/warp", "/warp/v1"} {
		for name, body := range bodies {
			t.Run(prefix+"/"+name, func(t *testing.T) {
				resp := e.do(t, http.MethodPost, prefix+"/chat/completions", body, false)
				out := e.readBody(t, resp)
				if resp.StatusCode != http.StatusBadRequest || !strings.Contains(out, "invalid_request_error") {
					t.Fatalf("invalid nested field or shape = %d %s", resp.StatusCode, out)
				}
				if clients.Load() != 0 || stub.aiCalls.Load() != 0 || stub.otherCalls.Load() != 0 {
					t.Fatalf("invalid shape reached account client/paid generation/refresh: clients=%d generation=%d other=%d", clients.Load(), stub.aiCalls.Load(), stub.otherCalls.Load())
				}
			})
		}
	}
}

func TestWarpRawValidTextBlocksStillReachGeneration(t *testing.T) {
	stub := newWarpRouteStub(t, http.StatusOK)
	e, _, _ := newWarpRouteE2E(t, stub, 1)
	for _, prefix := range []string{"/warp", "/warp/v1"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", prefix, stream), func(t *testing.T) {
				body, err := json.Marshal(map[string]any{"model": warpRouteModel, "stream": stream,
					"messages": []map[string]any{{"role": "user", "content": []map[string]string{
						{"type": "text", "text": "Explain this isolated test."}, {"type": "text", "text": " 好"},
					}}}})
				if err != nil {
					t.Fatal(err)
				}
				resp := e.do(t, http.MethodPost, prefix+"/chat/completions", string(body), false)
				out := e.readBody(t, resp)
				if resp.StatusCode != http.StatusOK || !strings.Contains(out, warpRouteAnswer) {
					t.Fatalf("supported text blocks = %d %s", resp.StatusCode, out)
				}
				select {
				case raw := <-stub.requests:
					assertWarpRouteRequest(t, raw)
				default:
					t.Fatal("supported request never reached generation")
				}
			})
		}
	}
	if stub.aiCalls.Load() != 4 || stub.otherCalls.Load() != 0 {
		t.Fatalf("supported request calls: generation=%d other=%d", stub.aiCalls.Load(), stub.otherCalls.Load())
	}
}
