package grok

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func TestTerminalBuildFailuresDoNotRotateOrPoisonAccounts(t *testing.T) {
	for _, tc := range []struct {
		name, body, code string
		upstream, status int
	}{
		{"refusal", `{"code":"permission-denied","error":"I'm sorry, I can’t help with that request. token=private-fixture"}`, "content_policy_violation", 403, 403},
		{"refusal code", `{"error":{"code":"content_policy_violation","message":"private-fixture"}}`, "content_policy_violation", 403, 403},
		{"escaped refusal", `{"error":"I can\u2019t help with that request. private-fixture"}`, "content_policy_violation", 403, 403},
		{"upgrade", `{"error":"upgrade required; private-fixture"}`, "upstream_upgrade_required", 426, 503},
	} {
		for _, endpoint := range []string{"chat", "responses", "messages"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", tc.name, endpoint, stream), func(t *testing.T) {
					var calls atomic.Int32
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls.Add(1)
						w.Header().Set("Content-Type", "application/json")
						w.Header().Set("Retry-After", "60")
						w.WriteHeader(tc.upstream)
						_, _ = io.WriteString(w, tc.body)
					}))
					defer server.Close()
					h, base := parityBuildHandler(t, server)
					ctx := context.Background()
					var ids []int64
					for i := range 3 {
						acc := *base
						acc.ID, acc.TeamID = 0, fmt.Sprintf("terminal-%d", i)
						acc.OAuthAccessToken = jwtWithClaims(t, fmt.Sprintf(`{"sub":"terminal-%d"}`, i))
						acc.GrokModels, acc.GrokModelsSyncedAt = []string{"grok-4.7"}, time.Now()
						testutil.NoError(t, h.lb.Store.CreateAccount(ctx, &acc))
						ids = append(ids, acc.ID)
					}
					testutil.NoError(t, h.lb.Store.CreateModel(ctx, &store.Model{Channel: "grok", ModelID: "grok-4.7", Name: "Grok", Status: store.ModelStatusAvailable, Verified: true, Provider: ProviderBuild, UpstreamModel: "grok-4.7", Capabilities: []string{store.CapabilityChat, store.CapabilityMessages, store.CapabilityResponses}}))
					body := fmt.Sprintf(`{"model":"grok-4.7","stream":%t,"messages":[{"role":"user","content":"hello"}],"max_tokens":100}`, stream)
					if endpoint == "responses" {
						body = fmt.Sprintf(`{"model":"grok-4.7","stream":%t,"input":"hello"}`, stream)
					}
					rec := httptest.NewRecorder()
					req := httptest.NewRequest("POST", "/grok/v1/"+endpoint, strings.NewReader(body))
					switch endpoint {
					case "chat":
						h.HandleChatCompletions(rec, req)
					case "responses":
						h.HandleResponses(rec, req)
					case "messages":
						h.HandleMessages(rec, req)
					}
					testutil.Equal(t, rec.Code, tc.status)
					testutil.Equal(t, calls.Load(), int32(1))
					testutil.MustContain(t, rec.Body.String(), tc.code)
					testutil.MustNotContain(t, rec.Body.String(), "private-fixture")
					testutil.MustNotContain(t, rec.Body.String(), "keepalive")
					testutil.Equal(t, rec.Header().Get("Retry-After"), "")
					for _, id := range ids {
						acc, err := h.lb.Store.GetAccount(ctx, id)
						testutil.NoError(t, err)
						testutil.Equal(t, acc.StatusCode, "")
						testutil.True(t, acc.Enabled)
						testutil.True(t, acc.QuotaResetAt.IsZero())
						testutil.True(t, acc.OAuthAccessToken != "")
					}
				})
			}
		}
	}
}

func TestBuildFailureClassificationKeepsPermissionAndBlockDistinct(t *testing.T) {
	for _, tc := range []struct {
		body string
		kind UpstreamErrorKind
	}{
		{`{"error":"feature requires a paid plan"}`, UpstreamErrorGenericForbidden},
		{`{"error":"user is blocked"}`, UpstreamErrorAccountBlock},
		{`{"code":"content_policy_violation"}`, UpstreamErrorContentRefusal},
	} {
		err := newCLIUpstreamError(403, nil, []byte(tc.body))
		testutil.Equal(t, ClassifyUpstreamError(err), tc.kind)
		if tc.kind == UpstreamErrorContentRefusal {
			testutil.False(t, shouldSwitchGrokAccount(err))
			testutil.False(t, markAllGrokAccountStatuses(err))
		}
	}
}
