package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/handler"
	"orchids-api/internal/provider"
	"orchids-api/internal/store"
	"orchids-api/internal/upstream"
)

func warpToolResultCacheBody(ttl string, streaming bool) map[string]any {
	cache := map[string]any{"type": "ephemeral"}
	if ttl != "" {
		cache["ttl"] = ttl
	}
	return map[string]any{
		"model": warpRouteModel, "stream": streaming,
		"tools": []any{map[string]any{"name": "synthetic_echo", "input_schema": map[string]any{"type": "object"}}},
		"messages": []map[string]any{
			{"role": "user", "content": "synthetic echo"},
			{"role": "assistant", "content": []map[string]any{{"type": "tool_use", "id": "call-synthetic-cache", "name": "synthetic_echo", "input": map[string]any{}, "cache_control": maps.Clone(cache)}}},
			{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "call-synthetic-cache", "content": strings.Repeat("r", 6381), "is_error": false, "cache_control": cache}}},
		},
	}
}

func marshalWarpToolResultCacheBody(t *testing.T, body map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func warpToolResultCacheBlock(body map[string]any) map[string]any {
	return body["messages"].([]map[string]any)[2]["content"].([]map[string]any)[0]
}

func TestWarpToolResultCacheUnboundRouteValidatesThenConflictsWithoutPaidCall(t *testing.T) {
	for _, prefix := range []string{"/warp", "/warp/v1"} {
		for _, ttl := range []string{"", "5m", "1h"} {
			for _, streaming := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/ttl=%s/stream=%t", prefix, ttl, streaming), func(t *testing.T) {
					stub := newWarpRouteStub(t, http.StatusOK)
					e, s, h := newWarpRouteE2E(t, stub, 1)
					var clients atomic.Int64
					payloads := make(chan upstream.UpstreamRequest, 1)
					h.SetClientFactory(func(acc *store.Account, cfg *config.Config) handler.UpstreamClient {
						clients.Add(1)
						spec, _ := provider.Lookup(acc.AccountType)
						return &warpRoutePayloadObserver{next: spec.NewClient(acc, cfg, s), payloads: payloads}
					})
					body := warpToolResultCacheBody(ttl, streaming)
					resp := e.do(t, http.MethodPost, prefix+"/messages", marshalWarpToolResultCacheBody(t, body), false)
					out := e.readBody(t, resp)
					if resp.StatusCode != http.StatusConflict || !strings.Contains(out, "continuation_conflict") {
						t.Fatalf("valid but unbound cache-marked result = %d %s, want 409", resp.StatusCode, out)
					}
					if clients.Load() != 1 || stub.aiCalls.Load() != 0 || stub.otherCalls.Load() != 0 || len(stub.requests) != 0 {
						t.Fatalf("unbound result must validate once without generation/refresh: clients=%d generation=%d other=%d", clients.Load(), stub.aiCalls.Load(), stub.otherCalls.Load())
					}
					select {
					case payload := <-payloads:
						if payload.Model != warpRouteModel || len(payload.Messages) != 3 || len(payload.Messages[2].Content.Blocks) != 1 {
							t.Fatal("route changed exact model or normalized conversation")
						}
						call := payload.Messages[1].Content.Blocks[0]
						if call.Type != "tool_use" || call.ID != "call-synthetic-cache" || call.Name != "synthetic_echo" || call.CacheControl == nil || call.CacheControl.Type != "ephemeral" {
							t.Fatal("route changed the historical tool call or its cache marker")
						}
						block := payload.Messages[2].Content.Blocks[0]
						if block.Type != "tool_result" || block.ToolUseID != "call-synthetic-cache" || block.Content != strings.Repeat("r", 6381) || block.IsError || block.CacheControl == nil || block.CacheControl.Type != "ephemeral" {
							t.Fatal("route changed result identity, full content, error flag or caller cache marker")
						}
					default:
						t.Fatal("valid tool result never reached the normalized client")
					}
				})
			}
		}
	}
}

func TestWarpToolResultCacheInvalidRawShapesRejectedBeforeClient(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"null", func(b map[string]any) { warpToolResultCacheBlock(b)["cache_control"] = nil }},
		{"string", func(b map[string]any) { warpToolResultCacheBlock(b)["cache_control"] = "ephemeral" }},
		{"array", func(b map[string]any) { warpToolResultCacheBlock(b)["cache_control"] = []any{} }},
		{"empty", func(b map[string]any) { warpToolResultCacheBlock(b)["cache_control"] = map[string]any{} }},
		{"unknown_scope", func(b map[string]any) {
			warpToolResultCacheBlock(b)["cache_control"].(map[string]any)["scope"] = "synthetic-global"
		}},
		{"unknown_cache_field", func(b map[string]any) {
			warpToolResultCacheBlock(b)["cache_control"].(map[string]any)["synthetic_unknown"] = true
		}},
		{"type_nonstring", func(b map[string]any) { warpToolResultCacheBlock(b)["cache_control"].(map[string]any)["type"] = 7 }},
		{"type_null", func(b map[string]any) { warpToolResultCacheBlock(b)["cache_control"].(map[string]any)["type"] = nil }},
		{"type_permanent", func(b map[string]any) {
			warpToolResultCacheBlock(b)["cache_control"].(map[string]any)["type"] = "permanent"
		}},
		{"ttl_wrong", func(b map[string]any) { warpToolResultCacheBlock(b)["cache_control"].(map[string]any)["ttl"] = "24h" }},
		{"ttl_empty", func(b map[string]any) { warpToolResultCacheBlock(b)["cache_control"].(map[string]any)["ttl"] = "" }},
		{"ttl_null", func(b map[string]any) { warpToolResultCacheBlock(b)["cache_control"].(map[string]any)["ttl"] = nil }},
		{"ttl_nonstring", func(b map[string]any) { warpToolResultCacheBlock(b)["cache_control"].(map[string]any)["ttl"] = 300 }},
		{"unknown_outer", func(b map[string]any) { warpToolResultCacheBlock(b)["synthetic_unknown"] = true }},
		{"wrong_role_assistant", func(b map[string]any) { b["messages"].([]map[string]any)[2]["role"] = "assistant" }},
		{"wrong_role_system", func(b map[string]any) { b["messages"].([]map[string]any)[2]["role"] = "system" }},
		{"tool_use_invalid_cache_marker", func(b map[string]any) {
			b["messages"].([]map[string]any)[1]["content"].([]map[string]any)[0]["cache_control"] = map[string]any{"type": "permanent"}
		}},
		{"nested_tool_result_text", func(b map[string]any) {
			warpToolResultCacheBlock(b)["content"] = []map[string]any{{"type": "text", "text": "synthetic nested result", "cache_control": map[string]any{"type": "ephemeral"}}}
		}},
	}
	for _, expired := range []bool{false, true} {
		t.Run(fmt.Sprintf("expired=%t", expired), func(t *testing.T) {
			stub := newWarpRouteStub(t, http.StatusOK)
			e, s, h := newWarpRouteE2E(t, stub, 1)
			if expired {
				accounts, err := s.ListAccounts(context.Background())
				if err != nil || len(accounts) != 1 {
					t.Fatalf("load synthetic account: count=%d err=%v", len(accounts), err)
				}
				accounts[0].WarpExpiresAt = time.Now().Add(-time.Hour)
				if err := s.UpdateAccount(context.Background(), accounts[0]); err != nil {
					t.Fatal(err)
				}
			}
			var clients atomic.Int64
			h.SetClientFactory(func(acc *store.Account, cfg *config.Config) handler.UpstreamClient {
				clients.Add(1)
				spec, _ := provider.Lookup(acc.AccountType)
				return spec.NewClient(acc, cfg, s)
			})
			for _, prefix := range []string{"/warp", "/warp/v1"} {
				for _, streaming := range []bool{false, true} {
					for _, tc := range cases {
						t.Run(fmt.Sprintf("%s/stream=%t/%s", prefix, streaming, tc.name), func(t *testing.T) {
							body := warpToolResultCacheBody("", streaming)
							tc.mutate(body)
							resp := e.do(t, http.MethodPost, prefix+"/messages", marshalWarpToolResultCacheBody(t, body), false)
							out := e.readBody(t, resp)
							if resp.StatusCode != http.StatusBadRequest || !strings.Contains(out, "invalid_request_error") {
								t.Fatalf("invalid result cache shape = %d %s, want 400", resp.StatusCode, out)
							}
							if len(out) > 1024 || strings.Contains(out, "synthetic_unknown") || strings.Contains(out, "synthetic-global") || strings.Contains(out, strings.Repeat("r", 128)) {
								t.Fatal("validation response echoed input data or exceeded bounded error size")
							}
							if clients.Load() != 0 || stub.aiCalls.Load() != 0 || stub.otherCalls.Load() != 0 || len(stub.requests) != 0 {
								t.Fatalf("invalid raw result reached client/generation/refresh: clients=%d generation=%d other=%d", clients.Load(), stub.aiCalls.Load(), stub.otherCalls.Load())
							}
						})
					}
				}
			}
		})
	}
}
