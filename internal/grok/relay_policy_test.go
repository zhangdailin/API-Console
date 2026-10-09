package grok

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"orchids-api/internal/chatwire"
	"reflect"
	"strings"
	"testing"
	"time"

	"encoding/json"
	"orchids-api/internal/config"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"

	"github.com/alicebob/miniredis/v2"
)

func parityBuildHandler(t *testing.T, server *httptest.Server) (*Handler, *store.Account) {
	t.Helper()
	mini := miniredis.RunT(t)
	database, err := store.New(store.Options{RedisAddr: mini.Addr(), RedisPrefix: "parity:", CredentialEncryptionKey: bytes.Repeat([]byte{42}, 32)})
	testutil.NoError(t, err)
	t.Cleanup(func() { _ = database.Close() })
	cfg := &config.Config{GrokCLIBaseURL: server.URL + "/v1"}
	h := NewHandler(cfg, loadbalancer.NewWithCacheTTL(database, time.Second))
	h.cliClient.httpClient = server.Client()
	h.cliClient.oauth.httpClient = server.Client()
	acc := &store.Account{ID: 1, Enabled: true, AccountType: "grok", GrokProvider: ProviderBuild, CredentialType: "oauth", OAuthAccessToken: jwtWithClaims(t, `{"sub":"parity-user","team_id":"parity-team"}`), OAuthExpiresAt: time.Now().Add(time.Hour)}
	return h, acc
}

func TestRelayNativeContextAndUpstreamDecisions(t *testing.T) {
	for _, tc := range []struct {
		name, path, body string
		status           int
	}{
		{"answer_without_reasoning", "/responses", `{"id":"resp_plain","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"A complete answer does not have to contain any reasoning."}]}]}`, 200},
		// Build reuses the compaction wording for opaque reasoning and session
		// failures, so only this wording combined with a real compaction item is
		// a genuine compaction rejection and passed through untouched.
		{"compaction_blob_rejected", "/responses", `{"error":{"message":"could not decode the compaction blob"}}`, 400},
		{"native_compact", "/responses/compact", `{"object":"response.compaction","output":[{"type":"compaction","encrypted_content":"native-opaque-result"}]}`, 200},
		{"unsupported_compact", "/responses/compact", `{"error":{"code":"not_found"}}`, 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var received map[string]interface{}
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				testutil.CheckEqual(t, r.URL.Path, "/v1"+tc.path)
				testutil.CheckNoError(t, json.NewDecoder(r.Body).Decode(&received))
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			h, acc := parityBuildHandler(t, server)
			acc.ID, acc.GrokModels, acc.GrokModelsSyncedAt = 0, []string{"grok-4.6"}, time.Now()
			testutil.NoError(t, h.lb.Store.CreateAccount(context.Background(), acc))
			testutil.NoError(t, h.lb.Store.CreateModel(context.Background(), &store.Model{Channel: "Grok", ModelID: "grok-4.6", Name: "Grok 4.6", Status: store.ModelStatusAvailable, Verified: true}))
			payload := map[string]interface{}{
				"model": "grok-4.6", "stream": false, "prompt_cache_key": "client-session",
				// The gateway now applies the Build defaults: store=false for
				// zero-data-retention, and the encrypted-reasoning include that makes
				// a replay chain possible at all. Everything the client sent is still
				// relayed verbatim.
				"store":              false,
				"include":            []interface{}{"reasoning.encrypted_content"},
				"reasoning":          map[string]interface{}{"effort": "future-effort"},
				"context_management": []interface{}{map[string]interface{}{"type": "compaction", "compact_threshold": float64(50000)}},
				"input": []interface{}{
					map[string]interface{}{"type": "reasoning", "encrypted_content": "client-reasoning"},
					map[string]interface{}{"type": "compaction", "encrypted_content": "client-native-compaction"},
					map[string]interface{}{"role": "user", "content": strings.Repeat("完整上下文，不要摘要。", 2000)},
				},
			}
			body, _ := json.Marshal(payload)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1"+tc.path, bytes.NewReader(body))
			if tc.path == "/responses/compact" {
				h.HandleResponsesCompact(rec, req)
			} else {
				h.HandleResponses(rec, req)
			}
			testutil.Falsef(t, rec.Code != tc.status || calls != 1, "status=%d want=%d calls=%d body=%s", rec.Code, tc.status, calls, rec.Body.String())
			// Session keys are tenant-scoped routing metadata; content is not.
			testutil.NotEqual(t, chatwire.ParseLooseStringAny(received["prompt_cache_key"]), "")
			received["prompt_cache_key"] = payload["prompt_cache_key"]
			if !reflect.DeepEqual(received, payload) {
				for key, want := range payload {
					testutil.CheckFalsef(t, !reflect.DeepEqual(received[key], want), "request field %q changed", key)
				}
				for key := range received {
					_, ok := payload[key]
					testutil.CheckFalsef(t, !ok, "unexpected request field %q", key)
				}
			}
			testutil.Falsef(t, tc.status == 200 && !strings.Contains(rec.Body.String(), tc.body), "response changed: %s", rec.Body.String())
			stored, err := h.lb.Store.GetAccount(context.Background(), acc.ID)
			testutil.Falsef(t, err != nil || !stored.Enabled || stored.StatusCode != "", "relay penalized valid account: status=%q error=%v", stored.StatusCode, err)
		})
	}
}

// An opaque-reasoning rejection stays recoverable even when the request also
// carries a client compaction item: recovery rewrites only the reasoning item's
// cipher and never the client-held compaction state.
func TestRelayNativeResponsesRecoversOpaqueReasoning(t *testing.T) {
	var bodies []map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var received map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&received)
		bodies = append(bodies, received)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"code":"invalid_encrypted_content","message":"could not decrypt the provided encrypted_content"}}`)
	}))
	defer server.Close()
	h, acc := parityBuildHandler(t, server)
	acc.ID, acc.GrokModels, acc.GrokModelsSyncedAt = 0, []string{"grok-4.6"}, time.Now()
	testutil.NoError(t, h.lb.Store.CreateAccount(context.Background(), acc))
	testutil.NoError(t, h.lb.Store.CreateModel(context.Background(), &store.Model{Channel: "Grok", ModelID: "grok-4.6", Name: "Grok 4.6", Status: store.ModelStatusAvailable, Verified: true}))
	payload := map[string]interface{}{
		"model": "grok-4.6", "stream": false, "prompt_cache_key": "client-session",
		"input": []interface{}{
			map[string]interface{}{"type": "reasoning", "encrypted_content": "client-reasoning"},
			map[string]interface{}{"type": "compaction", "encrypted_content": "client-native-compaction"},
			map[string]interface{}{"role": "user", "content": "hello"},
		},
	}
	body, _ := json.Marshal(payload)
	rec := httptest.NewRecorder()
	h.HandleResponses(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body)))

	testutil.Falsef(t, len(bodies) < 2, "expected a recovery retry, calls=%d", len(bodies))
	for index, received := range bodies[1:] {
		items, _ := received["input"].([]interface{})
		reasoningSeen := false
		for _, raw := range items {
			item, _ := raw.(map[string]interface{})
			switch chatwire.ParseLooseStringAny(item["type"]) {
			case "reasoning":
				reasoningSeen = true
				testutil.Equal(t, chatwire.ParseLooseStringAny(item["encrypted_content"]), "")
			case "compaction":
				testutil.Equal(t, chatwire.ParseLooseStringAny(item["encrypted_content"]), "client-native-compaction")
			}
		}
		testutil.Falsef(t, reasoningSeen, "retry %d kept an empty reasoning item instead of dropping it: %v", index, items)
	}
}

func TestRelayChatSamplingAndEffortAreClientOwned(t *testing.T) {
	for _, tc := range []struct{ effort, want string }{
		{"max", "high"},
		{"xhigh", "high"},
		{"minimal", "low"},
		{"low", "low"},
		{"future-effort", "future-effort"},
	} {
		effort := tc.effort
		temperature, topP := 3.0, 1.5
		req := &chatwire.Request{Model: "grok-4.5", Messages: []chatwire.Message{{Role: "user", Content: "original"}}, ReasoningEffort: &effort, Temperature: &temperature, TopP: &topP}
		testutil.NoError(t, req.Validate())
		payload, err := (&Handler{}).responsesPayloadFromChat(ModelSpec{ID: req.Model, UpstreamModel: req.Model, Upstream: UpstreamCLI}, req, true)
		testutil.NoError(t, err)
		testutil.Falsef(t, payload["reasoning"].(map[string]interface{})["effort"] != tc.want || payload["temperature"] != temperature || payload["top_p"] != topP, "effort=%q payload=%v", tc.effort, payload)
	}
}

// A model that does advertise xhigh keeps it, and the client-only max alias
// maps onto it. Composer never receives an effort but keeps its summary.
func TestRelayBuildEffortAliasesFollowModelContract(t *testing.T) {
	for _, tc := range []struct{ model, effort, want string }{
		{"grok-4.6", "max", "xhigh"},
		{"grok-4.6", "xhigh", "xhigh"},
		{"grok-4.5", "minimal", "low"},
		{"grok-composer-2.5-fast", "high", ""},
	} {
		effort := tc.effort
		req := &chatwire.Request{Model: tc.model, Messages: []chatwire.Message{{Role: "user", Content: "hi"}}, ReasoningEffort: &effort}
		payload, err := (&Handler{}).responsesPayloadFromChat(ModelSpec{ID: tc.model, UpstreamModel: tc.model, Upstream: UpstreamCLI}, req, true)
		testutil.NoError(t, err)
		reasoning, _ := payload["reasoning"].(map[string]interface{})
		if tc.want == "" {
			_, exists := reasoning["effort"]
			testutil.Falsef(t, exists, "%s kept effort %v", tc.model, reasoning)
			testutil.Equal(t, reasoning["summary"], "concise")
			continue
		}
		testutil.EqualAny(t, reasoning["effort"], tc.want)
	}
}

// Repeated generated deltas remain intact beyond the former 128/256 limits.
func TestRelayRepeatedDeltasAreForwarded(t *testing.T) {
	const repeats = 300
	for _, tc := range []struct{ name, kind, text string }{
		{"content", "response.output_text.delta", "repeat this answer "},
		{"reasoning", "response.reasoning_text.delta", "same thought "},
		{"reasoning_summary", "response.reasoning_summary_text.delta", "same summary "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := strings.Repeat(parityFrame(tc.kind, map[string]interface{}{"delta": tc.text}), repeats)
			if tc.kind != "response.output_text.delta" {
				stream += parityText("visible answer")
			}
			stream += parityTerminal("response.completed")
			rec := httptest.NewRecorder()
			_, _, result := copyNativeCLIResponseAndCaptureModel(rec, strings.NewReader(stream), "text/event-stream", "grok-4.6")
			testutil.NoError(t, result.Err)
			testutil.Equal(t, strings.Count(rec.Body.String(), tc.text), repeats)
			testutil.MustContain(t, rec.Body.String(), "response.completed")
			converted, outcome := parityRun(t, stream)
			testutil.NoError(t, outcome.Err)
			testutil.Equal(t, strings.Count(converted, tc.text), repeats)
			testutil.MustContain(t, converted, "[DONE]")
		})
	}
}
