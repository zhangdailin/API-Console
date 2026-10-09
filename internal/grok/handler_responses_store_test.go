package grok

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"orchids-api/internal/chatwire"
	"strings"
	"testing"
	"time"

	"encoding/json"

	"orchids-api/internal/config"
	"orchids-api/internal/middleware"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func TestBuildStoredResponseLifecycleAndOwnerIsolation(t *testing.T) {
	var calls []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/responses":
			_, _ = io.WriteString(w, `{"id":"resp_owned","object":"response","status":"completed","model":"grok-4.6","output":[]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/responses/resp_owned":
			_, _ = io.WriteString(w, `{"id":"resp_owned","object":"response","status":"completed","output":[]}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/responses/resp_owned":
			_, _ = io.WriteString(w, `{"id":"resp_owned","object":"response.deleted","deleted":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	h, s, _ := setupValidationHandler(t)
	configureStoredResponseTestHandler(t, h, s, upstream)

	call := func(token, method, path, body string, handler http.HandlerFunc) *httptest.ResponseRecorder {
		wrapped := middleware.APIKeyAuthWithRequest(func(*http.Request) bool { return true }, func(context.Context, string) (*middleware.APIKeyPrincipal, error) {
			return &middleware.APIKeyPrincipal{}, nil
		}, handler)
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		wrapped(rec, req)
		return rec
	}

	created := call("owner-a", http.MethodPost, "/v1/responses", `{"model":"grok-4.6","input":"hello","stream":false}`, h.HandleResponses)
	testutil.Falsef(t, created.Code != http.StatusOK || !strings.Contains(created.Body.String(), "resp_owned"), "create status=%d body=%s", created.Code, created.Body.String())
	deniedContinuation := call("owner-b", http.MethodPost, "/v1/responses", `{"model":"grok-4.6","input":"continue","stream":false,"previous_response_id":"resp_owned"}`, h.HandleResponses)
	testutil.Equal(t, deniedContinuation.Code, http.StatusNotFound)
	denied := call("owner-b", http.MethodGet, "/v1/responses/resp_owned", "", h.HandleResponseResource)
	testutil.Equal(t, denied.Code, http.StatusNotFound)
	got := call("owner-a", http.MethodGet, "/v1/responses/resp_owned", "", h.HandleResponseResource)
	testutil.Falsef(t, got.Code != http.StatusOK || !strings.Contains(got.Body.String(), "resp_owned"), "get status=%d body=%s", got.Code, got.Body.String())
	deleted := call("owner-a", http.MethodDelete, "/v1/responses/resp_owned", "", h.HandleResponseResource)
	testutil.Falsef(t, deleted.Code != http.StatusOK || !strings.Contains(deleted.Body.String(), `"deleted":true`), "delete status=%d body=%s", deleted.Code, deleted.Body.String())
	missing := call("owner-a", http.MethodGet, "/v1/responses/resp_owned", "", h.HandleResponseResource)
	testutil.Equal(t, missing.Code, http.StatusNotFound)
	testutil.Equal(t, len(calls), 3)
}

func TestBuildPreviousResponsePinsCreatingAccount(t *testing.T) {
	var authHeaders []string
	responseNumber := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeaders = append(authHeaders, r.Header.Get("Authorization"))
		responseNumber++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, fmt.Sprintf(`{"id":"resp_%d","object":"response","status":"completed","output":[]}`, responseNumber))
	}))
	defer upstream.Close()

	h, s, _ := setupValidationHandler(t)
	configureStoredResponseTestHandler(t, h, s, upstream)
	testutil.NoError(t, s.CreateAccount(context.Background(), buildTestAccount(t, "user-2", "team-2")))

	wrapped := middleware.APIKeyAuthWithRequest(func(*http.Request) bool { return true }, func(context.Context, string) (*middleware.APIKeyPrincipal, error) {
		return &middleware.APIKeyPrincipal{}, nil
	}, h.HandleResponses)
	request := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer owner")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		wrapped(rec, req)
		return rec
	}
	first := request(`{"model":"grok-4.6","input":"first","stream":false}`)
	testutil.Equal(t, first.Code, http.StatusOK)
	var firstBody map[string]interface{}
	_ = json.Unmarshal(first.Body.Bytes(), &firstBody)
	second := request(`{"model":"grok-4.6","input":"second","stream":false,"previous_response_id":"` + chatwire.ParseLooseStringAny(firstBody["id"]) + `"}`)
	testutil.Equal(t, second.Code, http.StatusOK)
	testutil.Falsef(t, len(authHeaders) != 2 || authHeaders[0] == "" || authHeaders[0] != authHeaders[1], "requests were not pinned to the creating account: %v", authHeaders)
}

func TestResponsesCompactForcesNonStreamingNativeEndpoint(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		testutil.Equal(t, r.URL.Path, "/v1/responses/compact")
		var payload map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if stream, _ := payload["stream"].(bool); stream {
			t.Fatal("compact request remained streaming")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_compact","object":"response","output":[{"type":"compaction","encrypted_content":"opaque"}]}`)
	}))
	defer upstream.Close()
	h, s, _ := setupValidationHandler(t)
	configureStoredResponseTestHandler(t, h, s, upstream)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", strings.NewReader(`{"model":"grok-4.6","input":[{"type":"compaction_trigger"}],"stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.HandleResponsesCompact(rec, req)
	testutil.Falsef(t, rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "encrypted_content"), "status=%d body=%s", rec.Code, rec.Body.String())
}

func configureStoredResponseTestHandler(t *testing.T, h *Handler, s *store.Store, upstream *httptest.Server) {
	t.Helper()
	if err := s.CreateModel(context.Background(), &store.Model{
		Channel: "Grok", ModelID: "grok-4.6", Name: "Grok 4.6", Status: store.ModelStatusAvailable, Verified: true,
	}); err != nil {
		t.Fatal(err)
	}
	testutil.NoError(t, s.CreateAccount(context.Background(), buildTestAccount(t, "user-1", "team-1")))
	h.cfg = &config.Config{GrokCLIBaseURL: upstream.URL + "/v1", ResponseStoreTTL: 24}
	h.cliClient = NewCLIClient(h.cfg)
	h.cliClient.SetAccountStore(s)
	h.cliClient.httpClient = upstream.Client()
	h.cliClient.oauth.httpClient = upstream.Client()
}

func buildTestAccount(t *testing.T, userID, teamID string) *store.Account {
	t.Helper()
	return &store.Account{
		AccountType: "grok", Enabled: true, CredentialType: "oauth", GrokProvider: ProviderBuild,
		OAuthAccessToken: jwtWithClaims(t, `{"sub":"`+userID+`","team_id":"`+teamID+`"}`),
		OAuthExpiresAt:   time.Now().Add(time.Hour), TeamID: teamID,
		GrokModels: []string{"grok-4.6"}, GrokModelsSyncedAt: time.Now(),
	}
}

func TestResponseIDCaptureFromSSE(t *testing.T) {
	input := "event: response.completed\ndata: {\"response\":\ndata: {\"id\":\"resp_stream\"}}\n\n"
	recorder := httptest.NewRecorder()
	got, _, _ := copyNativeCLIResponseAndCaptureModel(recorder, strings.NewReader(input), "text/event-stream", "grok-4.6")
	testutil.Falsef(t, got != "resp_stream", "response id=%q", got)
}
