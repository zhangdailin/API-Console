package grok

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"encoding/json"

	"orchids-api/internal/config"
	"orchids-api/internal/responses"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func TestHandleResponses_ProxiesBuildOAuthNatively(t *testing.T) {
	var received map[string]interface{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		testutil.Equal(t, r.Method, http.MethodPost)
		testutil.Equal(t, r.URL.Path, "/v1/responses")
		testutil.NoError(t, json.NewDecoder(r.Body).Decode(&received), "decode upstream request: %v")
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Request-ID", "native-response-test")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n")
	}))
	defer upstream.Close()

	h, s, _ := setupValidationHandler(t)

	if err := s.CreateModel(context.Background(), &store.Model{
		Channel: "Grok", ModelID: "grok-4.5", Name: "Grok 4.5",
		Status: store.ModelStatusAvailable, Verified: true,
	}); err != nil {
		t.Fatalf("CreateModel: %v", err)
	}
	if err := s.CreateAccount(context.Background(), &store.Account{
		AccountType: "grok", Enabled: true, CredentialType: "oauth", GrokProvider: ProviderBuild,
		OAuthAccessToken: jwtWithClaims(t, `{"sub":"user-1","team_id":"team-1"}`),
		OAuthExpiresAt:   time.Now().Add(time.Hour), GrokModels: []string{"grok-4.5"},
		GrokModelsSyncedAt: time.Now(),
	}); err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	h.cfg = &config.Config{GrokCLIBaseURL: upstream.URL + "/v1"}
	h.cliClient = NewCLIClient(h.cfg)
	h.cliClient.SetAccountStore(s)
	h.cliClient.httpClient = upstream.Client()
	h.cliClient.oauth.httpClient = upstream.Client()

	body := `{
		"model":"grok-4.5", "input":"hello", "stream":true,
		"previous_response_id":"resp_previous", "metadata":{"trace":"keep"},
		"include":["reasoning.encrypted_content"],
		"tools":[{"type":"function","name":"weather","description":"get weather","parameters":{"type":"object"}}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()

	h.HandleResponses(rec, req)

	testutil.Equal(t, rec.Code, http.StatusOK)
	testutil.Equal(t, rec.Header().Get("Content-Type"), "text/event-stream")
	testutil.Equal(t, rec.Header().Get("X-Request-Id"), "native-response-test")
	got := rec.Body.String()
	testutil.Falsef(t, !strings.Contains(got, "event: response.created") || strings.Contains(got, "chat.completion.chunk"), "response was not native Responses SSE: %q", got)
	testutil.Equal(t, received["previous_response_id"], "resp_previous")
	metadata, _ := received["metadata"].(map[string]interface{})
	testutil.Equal(t, metadata["trace"], "keep")
	testutil.Equal(t, len(responses.InterfaceSlice(received["tools"])), 1)
}
