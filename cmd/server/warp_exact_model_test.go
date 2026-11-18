package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/proto"

	"orchids-api/internal/provider"
	"orchids-api/internal/store"
)

func TestWarpExactModelRouteDoesNotResolveEffortAliases(t *testing.T) {
	const exactModel = "model-a-medium"
	cases := []struct {
		name          string
		model         string
		allowedModels []string
		status        int
	}{
		{
			name: "bare_model_permitted_by_key", model: "model-a",
			allowedModels: []string{"model-a"}, status: http.StatusBadRequest,
		},
		{
			name: "bare_model_and_variant_permitted_by_key", model: "model-a",
			allowedModels: []string{"model-a", exactModel}, status: http.StatusBadRequest,
		},
		{
			name: "exact_variant_not_permitted_by_key", model: exactModel,
			allowedModels: []string{"model-a"}, status: http.StatusForbidden,
		},
		{
			name: "caller_model_case_mismatch", model: "MODEL-A-MEDIUM",
			allowedModels: []string{exactModel}, status: http.StatusNotFound,
		},
		{
			name: "unknown_model_has_no_catalog_fallback", model: "missing-model",
			allowedModels: []string{"*"}, status: http.StatusBadRequest,
		},
		{
			name: "explicit_exact_variant", model: exactModel,
			allowedModels: []string{exactModel}, status: http.StatusOK,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := newWarpRouteStub(t, http.StatusOK)
			e, s, _ := newWarpRouteE2E(t, stub, 1)
			spec, ok := provider.Lookup("warp")
			if !ok || !spec.ExactModelID {
				t.Fatal("Warp must require exact model identifiers")
			}

			accounts, err := s.ListAccounts(t.Context())
			if err != nil || len(accounts) != 1 {
				t.Fatalf("load planted account: count=%d err=%v", len(accounts), err)
			}
			accounts[0].WarpModelIDs = []string{exactModel}
			accounts[0].WarpModelsSyncedAt = time.Now()
			if err := s.UpdateAccount(t.Context(), accounts[0]); err != nil {
				t.Fatal(err)
			}
			if _, err := s.ReconcileDiscoveredModels(t.Context(), "warp", []*store.Model{{
				Channel: "warp", ModelID: exactModel, Name: exactModel, UpstreamModel: exactModel,
				Provider: "warp", Status: store.ModelStatusAvailable, Verified: true,
				Capabilities: []string{store.CapabilityChat, "text"},
			}}, store.ModelReconcileOptions{Prune: true}); err != nil {
				t.Fatal(err)
			}
			models, err := s.ListModels(t.Context())
			if err != nil || len(models) != 1 || models[0].ModelID != exactModel {
				t.Fatalf("catalog must contain only the exact effort variant: models=%v err=%v", models, err)
			}
			keys, err := s.ListApiKeys(t.Context())
			if err != nil || len(keys) != 1 {
				t.Fatalf("load managed key: count=%d err=%v", len(keys), err)
			}
			keys[0].AllowedModels = tc.allowedModels
			if err := s.UpdateApiKey(t.Context(), keys[0]); err != nil {
				t.Fatal(err)
			}

			var generations int64
			for _, prefix := range []string{"/warp", "/warp/v1"} {
				for _, streaming := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/stream=%t", prefix, streaming), func(t *testing.T) {
						body, err := json.Marshal(map[string]any{"model": tc.model, "stream": streaming,
							"messages": []map[string]string{{"role": "user", "content": warpRouteQuery}}})
						if err != nil {
							t.Fatal(err)
						}
						resp := e.do(t, http.MethodPost, prefix+"/chat/completions", string(body), false)
						out := e.readBody(t, resp)
						if resp.StatusCode != tc.status {
							t.Fatalf("model %q = %d %s, want %d", tc.model, resp.StatusCode, out, tc.status)
						}
						if tc.status == http.StatusOK {
							generations++
							if !strings.Contains(out, warpRouteAnswer) || !strings.Contains(out, `"model":"`+exactModel+`"`) {
								t.Fatalf("explicit exact variant did not complete unchanged: %s", out)
							}
							select {
							case raw := <-stub.requests:
								var request warpapi.Request
								if err := proto.Unmarshal(raw, &request); err != nil {
									t.Fatal(err)
								}
								if got := request.GetSettings().GetModelConfig().GetBase(); got != exactModel {
									t.Fatalf("paid request model = %q, want %q", got, exactModel)
								}
							default:
								t.Fatal("explicit exact variant never reached generation")
							}
						} else {
							code := "invalid_request_error"
							if tc.status == http.StatusForbidden {
								code = "permission_error"
							} else if tc.model == "MODEL-A-MEDIUM" {
								// Pool eligibility is now exact too: a differently cased
								// model never reaches client construction or upstream I/O.
								code = `"type":"model_unavailable"`
							}
							if !strings.Contains(out, code) {
								t.Fatalf("unexpected rejection envelope: %s", out)
							}
						}
						if stub.aiCalls.Load() != generations || stub.otherCalls.Load() != 0 {
							t.Fatalf("unexpected paid generation or fallback/refresh: generation=%d want=%d other=%d", stub.aiCalls.Load(), generations, stub.otherCalls.Load())
						}
					})
				}
			}
		})
	}
}

func TestWarpRoutesAcceptUpstreamModelFallback(t *testing.T) {
	for _, tc := range []struct {
		name, reported string
		fallback       bool
	}{
		{"different-model", "other-provider-model", false},
		{"different-model-fallback", "other-provider-model", true},
		{"same-model-fallback", warpRouteModel, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			modelUsed := warpapi.Message_builder{ModelUsed: warpapi.Message_ModelUsed_builder{ModelId: proto.String(tc.reported), IsFallback: proto.Bool(tc.fallback)}.Build()}.Build()
			stub := newWarpRouteStub(t, http.StatusOK, modelUsed)
			e, _, _ := newWarpRouteE2E(t, stub, 2)
			var generations int64
			for _, prefix := range []string{"/warp", "/warp/v1"} {
				for _, protocol := range []string{"chat/completions", "messages", "responses"} {
					for _, streaming := range []bool{false, true} {
						t.Run(fmt.Sprintf("%s/%s/stream=%t", prefix, protocol, streaming), func(t *testing.T) {
							body := map[string]any{"model": warpRouteModel, "stream": streaming}
							if protocol == "responses" {
								body["input"] = warpRouteQuery
							} else {
								body["messages"] = []map[string]string{{"role": "user", "content": warpRouteQuery}}
							}
							raw, err := json.Marshal(body)
							if err != nil {
								t.Fatal(err)
							}
							resp := e.do(t, http.MethodPost, prefix+"/"+protocol, string(raw), false)
							out := e.readBody(t, resp)
							if resp.StatusCode != http.StatusOK || !strings.Contains(out, warpRouteAnswer) || !strings.Contains(out, `"model":"`+warpRouteModel+`"`) || strings.Contains(out, `"type":"error"`) {
								t.Fatalf("upstream fallback did not produce a compatible response: status=%d body=%s", resp.StatusCode, out)
							}
							if streaming && !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
								t.Fatal("fallback changed the requested response format")
							}
							if tc.reported != warpRouteModel && strings.Contains(out, tc.reported) {
								t.Fatal("private upstream model metadata escaped into the public response")
							}
							generations++
							if stub.aiCalls.Load() != generations || stub.otherCalls.Load() != 0 {
								t.Fatal("fallback triggered a replay, account switch, or control-plane request")
							}
							select {
							case raw := <-stub.requests:
								assertWarpRouteRequest(t, raw)
							default:
								t.Fatal("fallback bypassed the real Warp client")
							}
						})
					}
				}
			}
		})
	}
}
