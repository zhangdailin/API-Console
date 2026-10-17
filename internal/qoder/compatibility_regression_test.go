package qoder

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

func TestThinkingControlsAgreeAcrossWireFields(t *testing.T) {
	for _, capability := range []bool{false, true} {
		for _, effort := range []string{"", "none", "high"} {
			t.Run(fmt.Sprintf("capability=%t/effort=%s", capability, effort), func(t *testing.T) {
				body := decodeChatBodyForTestWithModel(t, modelEntry{Key: "probe", IsReasoning: capability}, upstream.UpstreamRequest{ReasoningEffort: effort})
				want := capability
				if effort != "" {
					want = effort != "none"
				}
				testutil.EqualAny(t, body["model_config"].(map[string]any)["is_reasoning"], want)
				testutil.EqualAny(t, body["chat_context"].(map[string]any)["extra"].(map[string]any)["modelConfig"].(map[string]any)["is_reasoning"], want)
				params := body["parameters"].(map[string]any)
				if effort == "none" {
					testutil.EqualAny(t, params["reasoning_effort"], "none")
					testutil.EqualAny(t, params["max_thinking_tokens"], float64(0))
					testutil.EqualAny(t, params["enable_thinking"], false)
				} else if effort == "high" {
					testutil.EqualAny(t, params["reasoning_effort"], "high")
					testutil.EqualAny(t, params["enable_thinking"], true)
				} else {
					_, set := params["enable_thinking"]
					testutil.False(t, set, "omitted preference must not synthesize a thinking override")
				}
			})
		}
	}
}

func TestQuotaAggregatesOnlySpendablePools(t *testing.T) {
	for _, tc := range []struct {
		name, usage            string
		limit, used, remaining float64
		exhausted              bool
	}{
		{"add-on remains", `{"isQuotaExceeded":false,"userQuota":{"total":100,"used":100,"remaining":0},"addOnQuota":{"total":200,"used":50,"remaining":150}}`, 300, 150, 150, false},
		{"available organization", `{"isQuotaExceeded":false,"userQuota":{"total":100,"used":100,"remaining":0},"orgResourcePackage":{"cap":300,"used":20,"remaining":280,"available":true}}`, 400, 120, 280, false},
		{"unavailable organization", `{"userQuota":{"total":100,"used":100,"remaining":0},"orgResourcePackage":{"cap":300,"used":20,"remaining":280,"available":false}}`, 100, 100, 0, true},
		{"organization availability unknown", `{"userQuota":{"total":100,"used":100,"remaining":0},"orgResourcePackage":{"cap":300,"used":20,"remaining":280}}`, 100, 100, 0, true},
		{"free plan with add-on", `{"userQuota":{"total":0,"used":0,"remaining":0},"addOnQuota":{"total":200,"used":0,"remaining":200}}`, 200, 0, 200, false},
		{"authoritative exhaustion", `{"isQuotaExceeded":true,"userQuota":{"total":100,"used":10,"remaining":90},"addOnQuota":{"total":200,"used":0,"remaining":200}}`, 300, 10, 290, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v2/quota/usage" {
					fmt.Fprint(w, tc.usage)
					return
				}
				http.NotFound(w, r)
			}))
			defer srv.Close()
			c := NewFromAccount(signedTestAccount(), nil)
			setTestEndpoints(c, srv.URL, srv.URL, srv.URL)
			q, err := c.FetchQuota(context.Background())
			testutil.NoError(t, err)
			testutil.Equal(t, q.Limit, tc.limit)
			testutil.Equal(t, q.Used, tc.used)
			testutil.Equal(t, q.Remaining, tc.remaining)
			testutil.Equal(t, q.Exhausted, tc.exhausted)
		})
	}
}

func TestQuotaResetUsesReportedBoundary(t *testing.T) {
	reset := time.Now().AddDate(0, 1, 0).Truncate(time.Second)
	for _, tc := range []struct {
		name     string
		status   int
		boundary int64
		want     time.Time
	}{
		{"monthly boundary", 200, reset.UnixMilli(), reset},
		{"seconds boundary", 200, reset.Unix(), reset},
		{"missing boundary", 200, 0, time.Time{}},
		{"never expires sentinel", 200, 253402214400000, time.Time{}},
		{"status unavailable", 503, 0, time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v2/quota/usage":
					fmt.Fprint(w, `{"userQuota":{"total":100,"used":10,"remaining":90},"expiresAt":253402214400000}`)
				case "/api/v2/user/plan":
					fmt.Fprintf(w, `{"plan_tier_name":"Pro","is_paid_plan":true,"start_date":%d}`, time.Now().Add(-48*time.Hour).UnixMilli())
				case "/api/v3/user/status":
					w.WriteHeader(tc.status)
					fmt.Fprintf(w, `{"nextResetAt":%d}`, tc.boundary)
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			c := NewFromAccount(signedTestAccount(), nil)
			setTestEndpoints(c, srv.URL, srv.URL, srv.URL)
			q, err := c.FetchQuota(context.Background())
			testutil.NoError(t, err)
			testutil.Falsef(t, !q.ResetAt.Equal(tc.want), "reset=%v want=%v", q.ResetAt, tc.want)
			testutil.Equal(t, q.PlanTier, "Pro")
			testutil.Equal(t, q.PaidPlan, true)
		})
	}
}

func TestBareStreamErrorsCannotBecomeSuccessfulFinish(t *testing.T) {
	for _, tc := range []struct {
		name, payload string
		target        error
	}{
		{"provider", `{"success":false,"msgCode":500,"message":"provider_error: unavailable"}`, ErrTransientUpstream},
		{"busy", `{"success":false,"msgCode":10605,"message":"queue full"}`, ErrBusy},
		{"parameter", `{"success":false,"msgCode":400,"msgInfo":"invalid_parameter_error: invalid input"}`, ErrClientFault},
		{"auth", `{"success":false,"msgCode":401,"message":"login expired"}`, errUpstreamUnauthorized},
	} {
		for _, prefix := range []string{"", envelope(`{"choices":[{"delta":{"content":"partial"}}]}`)} {
			for _, wrapped := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/partial=%t/wrapped=%t", tc.name, prefix != "", wrapped), func(t *testing.T) {
					frame := "data: " + tc.payload + "\n\n"
					if wrapped {
						frame = envelope(tc.payload)
					}
					events, result, err := collectStream(t, prefix+frame+envelope(`{"choices":[{"delta":{"content":"must not appear"}}]}`)+"event:finish\n\n")
					testutil.Falsef(t, !errors.Is(err, tc.target), "error=%v want=%v", err, tc.target)
					testutil.Equal(t, result.SawMeaningfulEvent, prefix != "")
					for _, event := range events {
						testutil.False(t, strings.Contains(fmt.Sprint(event.Event), "must not appear"), "consumed output after a terminal error")
					}
				})
			}
		}
	}
	// Success and advisory objects must not be confused with terminal errors.
	_, _, err := collectStream(t, "data: {\"success\":true,\"message\":\"ok\"}\n\ndata: {\"success\":false}\n\n"+envelope(`{"choices":[{"delta":{"content":"ok"}}]}`)+"event:finish\n\n")
	testutil.NoError(t, err)
	_, _, err = collectStream(t, "event:error\ndata: {\"success\":false,\"msgCode\":400,\"message\":\"invalid_parameter_error\"}\n\n")
	testutil.Falsef(t, !errors.Is(err, ErrClientFault), "error event lost its classification: %v", err)
}

func TestGatewayFailureDoesNotReplayPartialOutput(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprintf("partial=%t", partial), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if partial {
					fmt.Fprint(w, envelope(`{"choices":[{"delta":{"content":"partial"}}]}`))
					fmt.Fprint(w, "data: {\"success\":false,\"msgCode\":500,\"message\":\"provider_error\"}\n\n")
				} else {
					fmt.Fprint(w, "data: {\"success\":false,\"msgCode\":400,\"message\":\"invalid_parameter_error\"}\n\n")
				}
				fmt.Fprint(w, "event:finish\n\n")
			}))
			defer server.Close()
			client := NewFromAccount(signedTestAccount(), nil)
			setTestEndpoints(client, server.URL, server.URL, server.URL)
			var events []upstream.SSEMessage
			err := client.SendRequestWithPayload(t.Context(), upstream.UpstreamRequest{Model: "Qwen3.7-Max", Prompt: "hello"}, func(event upstream.SSEMessage) { events = append(events, event) }, nil)
			testutil.Error(t, err)
			testutil.Equal(t, calls.Load(), 1)
			testutil.Falsef(t, partial && !errors.Is(err, ErrTransientUpstream), "partial failure lost its classification: %v", err)
			testutil.Falsef(t, !partial && !errors.Is(err, ErrClientFault), "request failure lost its classification: %v", err)
			wantEvents := 0
			if partial {
				wantEvents = 1
			}
			testutil.Equal(t, len(events), wantEvents)
		})
	}
}
