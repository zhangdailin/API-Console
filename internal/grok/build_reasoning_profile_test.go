package grok

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"orchids-api/internal/chatwire"
	"strings"
	"testing"
	"time"

	"encoding/json"

	"orchids-api/internal/config"
	"orchids-api/internal/modelcatalog"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func TestBuildChatAppliesSelectedAccountReasoningProfile(t *testing.T) {
	for _, tc := range []struct {
		name, requested, expected string
		status                    int
	}{
		{"explicit none uses lowest supported effort", "none", "low", http.StatusOK},
		{"unsupported effort", "ultra", "", http.StatusBadRequest},
		{"safe default", "", "low", http.StatusOK},
		{"explicit low", "low", "low", http.StatusOK},
		{"max alias", "max", "xhigh", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var payload map[string]interface{}
				testutil.CheckNoError(t, json.NewDecoder(r.Body).Decode(&payload))
				testutil.CheckEqualAny(t, payload["reasoning"].(map[string]interface{})["effort"], tc.expected)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"resp_profile","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}]}`))
			}))
			defer upstream.Close()
			h, s, _ := setupValidationHandler(t)
			model := "grok-4.7"
			testutil.NoError(t, s.CreateModel(context.Background(), &store.Model{Channel: "Grok", ModelID: model, Name: model, Status: store.ModelStatusAvailable, Verified: true}))
			acc := &store.Account{AccountType: "grok", GrokProvider: ProviderBuild, CredentialType: "oauth", Enabled: true, OAuthAccessToken: jwtWithClaims(t, `{"sub":"profile-user","team_id":"profile-team"}`), OAuthExpiresAt: time.Now().Add(time.Hour), GrokModels: []string{model}, GrokModelsSyncedAt: time.Now(), GrokModelCatalog: []modelcatalog.Profile{{ModelID: model, ReasoningEfforts: []string{"xhigh", "high", "medium", "low"}, DefaultReasoningEffort: "high", SupportsReasoningEffort: true}}}
			testutil.NoError(t, s.CreateAccount(context.Background(), acc))
			h.cfg = &config.Config{GrokCLIBaseURL: upstream.URL + "/v1"}
			h.cliClient = NewCLIClient(h.cfg)
			h.cliClient.httpClient = upstream.Client()
			h.cliClient.oauth.httpClient = upstream.Client()
			req := chatwire.Request{Model: model, Messages: []chatwire.Message{{Role: "user", Content: "hello"}}}
			if tc.requested != "" {
				req.ReasoningEffort = &tc.requested
			}
			body, _ := json.Marshal(req)
			rec := httptest.NewRecorder()
			h.HandleChatCompletions(rec, httptest.NewRequest(http.MethodPost, "/grok/v1/chat/completions", bytes.NewReader(body)))
			testutil.Equal(t, rec.Code, tc.status)
			if tc.status == http.StatusBadRequest {
				testutil.Falsef(t, calls != 0 || !strings.Contains(rec.Body.String(), "supported values: xhigh, high, medium, low"), "calls=%d body=%s", calls, rec.Body.String())
			} else if calls != 1 {
				t.Fatalf("calls=%d", calls)
			}
		})
	}
}

func TestBuildPayloadForAccountUsesCatalogDefaultAndValidation(t *testing.T) {
	acc := &store.Account{GrokModelCatalog: []modelcatalog.Profile{{ModelID: "grok-4.7", ReasoningEfforts: []string{"xhigh", "high", "medium", "low"}, DefaultReasoningEffort: "high", SupportsReasoningEffort: true}}}
	payload, err := buildPayloadForAccount(map[string]interface{}{"reasoning": map[string]interface{}{"summary": "auto"}}, acc, "grok-4.7")
	testutil.NoError(t, err)
	reasoning := payload["reasoning"].(map[string]interface{})
	testutil.Equal(t, reasoning["effort"], "low")
	payload, err = buildPayloadForAccount(map[string]interface{}{"reasoning": map[string]interface{}{"effort": "none"}}, acc, "grok-4.7")
	testutil.Falsef(t, err != nil || payload["reasoning"].(map[string]interface{})["effort"] != "low", "none compatibility payload=%v err=%v", payload, err)
	_, err = buildPayloadForAccount(map[string]interface{}{"reasoning": map[string]interface{}{"effort": "ultra"}}, acc, "grok-4.7")
	var profileErr *buildReasoningProfileError
	testutil.Falsef(t, !errors.As(err, &profileErr), "err=%v", err)
	payload, err = buildPayloadForAccount(map[string]interface{}{"reasoning": map[string]interface{}{"effort": "max"}}, acc, "grok-4.7")
	testutil.NoError(t, err)
	testutil.Equal(t, payload["reasoning"].(map[string]interface{})["effort"], "xhigh")
}

func TestBuildPayloadForAccountDoesNotMutateImmutableSource(t *testing.T) {
	source := map[string]interface{}{"reasoning": map[string]interface{}{"effort": "max"}}
	acc := &store.Account{GrokModelCatalog: []modelcatalog.Profile{{ModelID: "grok-4.7", ReasoningEfforts: []string{"xhigh"}, SupportsReasoningEffort: true}}}
	_, err := buildPayloadForAccount(source, acc, "grok-4.7")
	testutil.NoError(t, err)
	testutil.Equal(t, source["reasoning"].(map[string]interface{})["effort"], "max")
}

func TestBuildExplicitNoneUsesLowestCompatibleEffort(t *testing.T) {
	for _, tc := range []struct {
		name    string
		profile *modelcatalog.Profile
		want    string
	}{
		{"catalog chooses supported low", &modelcatalog.Profile{ModelID: "grok-4.7", SupportsReasoningEffort: true, ReasoningEfforts: []string{"high", "low"}}, "low"},
		{"missing catalog uses static low", nil, "low"},
		{"no effort capability rejects explicit none", &modelcatalog.Profile{ModelID: "grok-4.7"}, "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := map[string]interface{}{"reasoning": map[string]interface{}{"effort": " NONE ", "summary": "auto"}}
			acc := &store.Account{}
			if tc.profile != nil {
				acc.GrokModelCatalog = []modelcatalog.Profile{*tc.profile}
			}
			payload, err := buildPayloadForAccount(source, acc, "grok-4.7")
			if tc.want == "error" {
				var profileErr *buildReasoningProfileError
				testutil.Falsef(t, !errors.As(err, &profileErr), "err=%v", err)
				return
			}
			testutil.NoError(t, err)
			reasoning := payload["reasoning"].(map[string]interface{})
			got := interfaceString(reasoning["effort"])
			testutil.Falsef(t, !strings.EqualFold(got, tc.want), "effort=%q want=%q", got, tc.want)
			testutil.False(t, reasoning["summary"] != "auto" || source["reasoning"].(map[string]interface{})["effort"] != " NONE ", "summary or immutable source changed")
		})
	}
}

func TestBuildMissingCatalogDefaultsKnownReasoningModelToLow(t *testing.T) {
	payload, err := buildPayloadForAccount(map[string]interface{}{}, &store.Account{}, "grok-4.7")
	testutil.NoError(t, err)
	testutil.Equal(t, payload["reasoning"].(map[string]interface{})["effort"], "low")
	payload, err = buildPayloadForAccount(map[string]interface{}{}, &store.Account{}, "unknown-model")
	testutil.NoError(t, err)
	_, exists := payload["reasoning"]
	testutil.Falsef(t, exists, "unexpected reasoning=%v", payload["reasoning"])
}
