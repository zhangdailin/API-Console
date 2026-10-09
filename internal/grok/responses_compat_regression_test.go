package grok

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/config"
	"orchids-api/internal/middleware"
	"orchids-api/internal/modelcatalog"
	"orchids-api/internal/store"
)

// The upstream validates declarations before generating any tool call. These
// fixtures complete with text only, including the previous_response_id turn.
func TestGrok47ResponsesCompatibilityBeforeToolExecution(t *testing.T) {
	for _, schema := range []string{
		`{"type":["object","null"],"properties":{"text":{"type":["string","null"]}}}`,
		`{"anyOf":[{"type":"object","properties":{"text":{"type":"string"}}},{"type":"null"}]}`,
		`{"type":"object","oneOf":[{"required":["text"]},{"required":["file"]}]}`,
	} {
		t.Run(schema, func(t *testing.T) {
			calls := 0
			var auth string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload map[string]interface{}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
					return
				}
				calls++
				w.Header().Set("Content-Type", "application/json")
				reasoning, _ := payload["reasoning"].(map[string]interface{})
				tools := payload["tools"].([]interface{})
				params := tools[0].(map[string]interface{})["parameters"].(map[string]interface{})
				if params["type"] != "object" || params["anyOf"] != nil || params["oneOf"] != nil || reasoning["effort"] != "low" {
					w.WriteHeader(http.StatusBadRequest)
					fmt.Fprint(w, `{"error":{"message":"invalid function root or reasoning effort"}}`)
					return
				}
				if calls == 1 {
					auth = r.Header.Get("Authorization")
				} else if auth != r.Header.Get("Authorization") {
					t.Error("continuation changed account")
				}
				fmt.Fprintf(w, `{"id":"resp_compat_%d","object":"response","status":"completed","model":"grok-4.7","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`, calls)
			}))
			defer upstream.Close()
			h, s, _ := setupValidationHandler(t)
			acc := buildTestAccount(t, "compat-user", "compat-team")
			acc.GrokModels = []string{"grok-4.7"}
			acc.GrokModelCatalog = []modelcatalog.Profile{{ModelID: "grok-4.7", SupportsReasoningEffort: true, ReasoningEfforts: []string{"low", "high"}, DefaultReasoningEffort: "high"}}
			if err := s.CreateAccount(context.Background(), acc); err != nil {
				t.Fatal(err)
			}
			if err := s.CreateModel(context.Background(), &store.Model{Channel: "Grok", ModelID: "grok-4.7", Status: store.ModelStatusAvailable, Verified: true}); err != nil {
				t.Fatal(err)
			}
			h.cfg = &config.Config{GrokCLIBaseURL: upstream.URL + "/v1", ResponseStoreTTL: 24}
			h.cliClient = NewCLIClient(h.cfg)
			h.cliClient.SetAccountStore(s)
			h.cliClient.httpClient = upstream.Client()
			h.cliClient.oauth.httpClient = upstream.Client()
			wrapped := middleware.APIKeyAuthWithRequest(func(*http.Request) bool { return true }, func(context.Context, string) (*middleware.APIKeyPrincipal, error) {
				return &middleware.APIKeyPrincipal{}, nil
			}, h.HandleResponses)
			request := func(previous, effort string) *httptest.ResponseRecorder {
				body := fmt.Sprintf(`{"model":"grok-4.7","input":"hello","stream":false,"reasoning":{"effort":%q},"tools":[{"type":"namespace","name":"functions","tools":[{"type":"function","name":"read","parameters":%s}]}]}`, effort, schema)
				if previous != "" {
					body = strings.TrimSuffix(body, "}") + fmt.Sprintf(`,"previous_response_id":%q}`, previous)
				}
				r := httptest.NewRequest(http.MethodPost, "/grok/v1/responses", strings.NewReader(body))
				r.Header.Set("Authorization", "Bearer compat-owner")
				r.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()
				wrapped(rec, r)
				return rec
			}
			for i := 1; i <= 2; i++ {
				previous := ""
				if i == 2 {
					previous = "resp_compat_1"
				}
				got := request(previous, "none")
				if got.Code != http.StatusOK {
					t.Fatalf("text turn %d: status=%d body=%s", i, got.Code, got.Body)
				}
			}
			bad := request("resp_compat_2", "ultra")
			if bad.Code != http.StatusBadRequest || !strings.Contains(bad.Body.String(), `"param":"reasoning.effort"`) || !strings.Contains(bad.Body.String(), "supported values: low, high") {
				t.Fatalf("local validation status=%d body=%s", bad.Code, bad.Body)
			}
			if calls != 2 {
				t.Fatalf("invalid effort reached upstream: calls=%d", calls)
			}
		})
	}
}
