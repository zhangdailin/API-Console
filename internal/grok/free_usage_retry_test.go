package grok

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/chatwire"
	"orchids-api/internal/store"
)

const freeUsageRejectionFixture = `{"code":"subscription:free-usage-exhausted","error":"You've used all the included free usage for model grok-4.7 for now. Usage resets over a rolling 24-hour window; tokens (actual/limit): 609544/600000."}`

func TestFreeUsageRejectionRequiresStructuredUpstreamEvidence(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"exact", newCLIUpstreamError(429, nil, []byte(freeUsageRejectionFixture)), true},
		{"wrapped", fmt.Errorf("wrapped: %w", newCLIUpstreamError(429, nil, []byte(freeUsageRejectionFixture))), true},
		{"text only", errors.New(freeUsageRejectionFixture), false},
		{"wrong status", newCLIUpstreamError(500, nil, []byte(freeUsageRejectionFixture)), false},
		{"unstructured", newCLIUpstreamError(429, nil, []byte("subscription:free-usage-exhausted")), false},
		{"message mentions code", newCLIUpstreamError(429, nil, []byte(`{"error":"subscription:free-usage-exhausted"}`)), false},
		{"generic throttle", newCLIUpstreamError(429, nil, []byte(`{"code":"rate_limited"}`)), false},
		{"malformed", newCLIUpstreamError(429, nil, []byte(`{"code":"subscription:free-usage-exhausted"`)), false},
		{"nil", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isFreeUsageExhaustedError(tc.err); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestHostedToolFreeUsageRejectionSwitchesWithinBudget(t *testing.T) {
	for _, endpoint := range []string{"responses", "chat"} {
		for _, mode := range []string{"recover", "exhausted", "budget one", "generic 429", "server failure", "pinned", "previous response", "no spare"} {
			if endpoint == "chat" && (mode == "pinned" || mode == "previous response") {
				continue
			}
			t.Run(endpoint+"/"+mode, func(t *testing.T) {
				var authorizations []string
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					authorizations = append(authorizations, r.Header.Get("Authorization"))
					if mode == "recover" && len(authorizations) == 2 {
						response := nativeQualityFixture(true)
						response["output"] = response["output"].([]any)[:2]
						w.Header().Set("Content-Type", "application/json")
						_ = json.NewEncoder(w).Encode(response)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					if mode == "server failure" {
						w.WriteHeader(503)
						_, _ = fmt.Fprint(w, `{"error":"service unavailable"}`)
						return
					}
					w.WriteHeader(429)
					if mode == "generic 429" {
						_, _ = fmt.Fprint(w, `{"error":"too many requests"}`)
					} else {
						_, _ = fmt.Fprint(w, freeUsageRejectionFixture)
					}
				}))
				defer server.Close()
				h, base := parityBuildHandler(t, server)
				cfg := h.configSnapshot().Clone()
				cfg.AccountSwitchCount = 2
				if mode == "budget one" {
					cfg.AccountSwitchCount = 1
				}
				h.SetConfig(cfg)
				ctx := WithRequestModel(context.Background(), "grok-4.7")
				for i := 0; i < 3; i++ {
					if mode == "no spare" && i > 0 {
						break
					}
					acc := *base
					acc.ID = 0
					acc.TeamID = fmt.Sprintf("%s-%s-%d", endpoint, mode, i)
					acc.OAuthAccessToken = jwtWithClaims(t, fmt.Sprintf(`{"sub":%q}`, acc.TeamID))
					acc.GrokModels, acc.GrokModelsSyncedAt = []string{"grok-4.7"}, time.Now()
					if err := h.lb.Store.CreateAccount(ctx, &acc); err != nil {
						t.Fatal(err)
					}
				}
				sess, err := h.openCLIAccountSession(ctx, nil, "grok-4.7")
				if err != nil {
					t.Fatal(err)
				}
				defer sess.Close()
				firstID := sess.acc.ID
				tools := []map[string]any{{"type": "web_search"}}
				payload := map[string]any{"model": "grok-4.7", "input": "hello", "tools": tools}
				if mode == "previous response" {
					payload["previous_response_id"] = "resp-original"
				}
				rec := httptest.NewRecorder()
				if endpoint == "responses" {
					_, _, _ = h.serveNativeResponses(rec, httptest.NewRequest("POST", "/grok/v1/responses", nil).WithContext(ctx), sess, mode == "pinned", ModelSpec{ID: "grok-4.7", UpstreamModel: "grok-4.7"}, payload, "/responses", nil)
				} else {
					req := &chatwire.Request{Model: "grok-4.7", Messages: []chatwire.Message{{Role: "user", Content: "hello"}}, ResponsesTools: tools}
					h.serveNativeChat(ctx, rec, req, ModelSpec{ID: "grok-4.7", UpstreamModel: "grok-4.7"}, sess, nil)
				}
				wantCalls, wantStatus := 1, 429
				if mode == "recover" || mode == "exhausted" {
					wantCalls = 2
				}
				if mode == "recover" {
					wantStatus = 200
				} else if mode == "server failure" {
					wantStatus = 503
					if endpoint == "chat" {
						wantStatus = 502
					}
				}
				if len(authorizations) != wantCalls || rec.Code != wantStatus {
					t.Fatalf("calls=%d want=%d status=%d want=%d body=%s", len(authorizations), wantCalls, rec.Code, wantStatus, rec.Body.String())
				}
				if wantCalls == 2 && (authorizations[0] == authorizations[1] || firstID == sess.acc.ID) {
					t.Fatal("retry reused the exhausted identity")
				}
				if mode == "recover" && !strings.Contains(rec.Body.String(), "PUBLIC_ANSWER") {
					t.Fatal("recovered answer missing")
				}
				if mode != "server failure" && mode != "generic 429" {
					first, err := h.lb.Store.GetAccount(ctx, firstID)
					if err != nil || store.ModelCooldownRemaining(first, "grok-4.7", time.Now()) < 23*time.Hour {
						t.Fatalf("exhausted model was not kept out of rotation: %v", err)
					}
					if rec.Code == 429 && !strings.Contains(rec.Body.String(), `"code":"free_usage_exhausted"`) {
						t.Fatal("free quota rejection reported as generic throttling")
					}
				}
			})
		}
	}
}

func TestFreeUsageFailureEnvelopeKeepsBackoffAndHidesUpstreamBody(t *testing.T) {
	err := newCLIUpstreamError(429, http.Header{"Retry-After": {"17"}}, []byte(freeUsageRejectionFixture))
	for _, write := range []func(http.ResponseWriter){
		func(w http.ResponseWriter) { writeGrokUpstreamFailure(w, 429, err) },
		func(w http.ResponseWriter) { writeGrokUpstreamError(w, err) },
	} {
		rec := httptest.NewRecorder()
		write(rec)
		var response struct {
			Error struct{ Code, Message string }
		}
		if json.Unmarshal(rec.Body.Bytes(), &response) != nil || rec.Code != 429 || rec.Header().Get("Retry-After") != "17" || response.Error.Code != "free_usage_exhausted" || response.Error.Message != grokFreeUsageExhaustedMessage {
			t.Fatalf("bad envelope: %d %s", rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "609544") || strings.Contains(rec.Body.String(), "subscription:") {
			t.Fatal("upstream refusal leaked")
		}
	}
}
