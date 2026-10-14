package grok

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/modelcatalog"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func TestCLIOAuthAccessTokenUnexpired(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("token endpoint must not be called when access token is unexpired")
	}))
	defer server.Close()

	cfg := &config.Config{}
	cfg.GrokCLIOAuthTokenURL = server.URL
	oauth := NewCLIOAuth(cfg, server.Client())
	acc := &store.Account{
		OAuthAccessToken:  "existing-token",
		OAuthRefreshToken: "refresh",
		OAuthExpiresAt:    time.Now().Add(time.Hour),
	}
	token, err := oauth.AccessToken(context.Background(), acc)
	testutil.NoError(t, err, "unexpected error: %v")
	testutil.Equal(t, token, "existing-token")
}

func TestCLIOAuthAccessTokenRefreshes(t *testing.T) {
	var called int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		testutil.Equal(t, r.Method, http.MethodPost)
		testutil.NoError(t, r.ParseForm(), "parse form: %v")
		testutil.Equal(t, r.Form.Get("grant_type"), "refresh_token")
		testutil.Equal(t, r.Form.Get("refresh_token"), "old-refresh")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600}`))
	}))
	defer server.Close()

	cfg := &config.Config{}
	cfg.GrokCLIOAuthTokenURL = server.URL
	oauth := NewCLIOAuth(cfg, server.Client())
	acc := &store.Account{
		OAuthAccessToken:  "stale-token",
		OAuthRefreshToken: "old-refresh",
		OAuthExpiresAt:    time.Now().Add(-time.Hour), // expired → forces refresh
	}
	token, err := oauth.AccessToken(context.Background(), acc)
	testutil.NoError(t, err, "unexpected error: %v")
	testutil.Equal(t, called, 1)
	testutil.Equal(t, token, "new-access")
	testutil.Equal(t, acc.OAuthAccessToken, "new-access")
	testutil.Equal(t, acc.OAuthRefreshToken, "new-refresh")
}

func TestCLIResponsesRefreshesRejectedUnexpiredTokenOnSameAccount(t *testing.T) {
	var responseCalls, refreshCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			refreshCalls++
			_, _ = w.Write([]byte(`{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600}`))
		case "/v1/responses":
			responseCalls++
			if r.Header.Get("Authorization") == "Bearer old-access" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"message":"expired"}}`))
				return
			}
			testutil.Equal(t, r.Header.Get("Authorization"), "Bearer new-access")
			_, _ = w.Write([]byte(`{"id":"resp_refreshed","object":"response"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := &config.Config{GrokCLIBaseURL: server.URL + "/v1", GrokCLIOAuthTokenURL: server.URL + "/oauth/token"}
	client := NewCLIClient(cfg)
	client.httpClient = server.Client()
	client.oauth.httpClient = server.Client()
	acc := &store.Account{
		OAuthAccessToken: "old-access", OAuthRefreshToken: "old-refresh", OAuthExpiresAt: time.Now().Add(time.Hour),
	}
	resp, err := client.doResponsesAt(context.Background(), acc, "/responses", map[string]any{"model": "grok-4.6", "input": "hello"})
	testutil.NoError(t, err)
	_ = resp.Body.Close()
	testutil.Falsef(t, responseCalls != 2 || refreshCalls != 1 || acc.OAuthAccessToken != "new-access", "response_calls=%d refresh_calls=%d access=%q", responseCalls, refreshCalls, acc.OAuthAccessToken)
}

func TestCLIOAuthAccessTokenCoalescesConcurrentRefreshes(t *testing.T) {
	var calls int
	var callsMu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callsMu.Lock()
		calls++
		callsMu.Unlock()
		time.Sleep(20 * time.Millisecond)
		_, _ = w.Write([]byte(`{"access_token":"shared-access","refresh_token":"shared-refresh","expires_in":3600}`))
	}))
	defer server.Close()

	cfg := &config.Config{GrokCLIOAuthTokenURL: server.URL}
	oauth := NewCLIOAuth(cfg, server.Client())
	acc := &store.Account{OAuthRefreshToken: "shared-refresh", OAuthExpiresAt: time.Now().Add(-time.Minute)}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			token, err := oauth.AccessToken(context.Background(), acc)
			if err != nil {
				errs <- err
				return
			}
			if token != "shared-access" {
				errs <- fmt.Errorf("token=%q", token)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	callsMu.Lock()
	defer callsMu.Unlock()
	testutil.Equal(t, calls, 1)
}

func TestCLIOAuthRefreshDenied(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"token expired"}`))
	}))
	defer server.Close()

	cfg := &config.Config{}
	cfg.GrokCLIOAuthTokenURL = server.URL
	oauth := NewCLIOAuth(cfg, server.Client())
	acc := &store.Account{
		OAuthAccessToken:  "stale",
		OAuthRefreshToken: "dead-refresh",
		OAuthExpiresAt:    time.Now().Add(-time.Minute),
	}
	_, err := oauth.AccessToken(context.Background(), acc)
	testutil.False(t, err == nil, "expected error for denied refresh")
	testutil.True(t, IsCLIPermanentOAuthError(err), "invalid_grant should be permanent (401): %v")
	testutil.MustNotContain(t, err.Error(), "token expired")
}

func TestCLIOAuthMissingRefreshToken(t *testing.T) {
	cfg := &config.Config{}
	oauth := NewCLIOAuth(cfg, nil)
	acc := &store.Account{OAuthAccessToken: "", OAuthRefreshToken: "", OAuthExpiresAt: time.Time{}}
	_, err := oauth.AccessToken(context.Background(), acc)
	testutil.Error(t, err)
}

func TestCLIOAuthRefreshServerErrorTransient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	cfg := &config.Config{}
	cfg.GrokCLIOAuthTokenURL = server.URL
	oauth := NewCLIOAuth(cfg, server.Client())
	acc := &store.Account{OAuthRefreshToken: "r", OAuthExpiresAt: time.Now().Add(-time.Minute)}
	_, err := oauth.AccessToken(context.Background(), acc)
	testutil.False(t, err == nil, "expected error")
	testutil.Falsef(t, IsCLIPermanentOAuthError(err), "5xx should be transient, got permanent: %v", err)
}

func TestCLIOAuthErrorStatus(t *testing.T) {
	e := &cliOAuthError{status: http.StatusUnauthorized, message: "denied"}
	testutil.Equal(t, e.Status(), "401")
	zero := &cliOAuthError{}
	testutil.Equal(t, zero.Status(), "")
	testutil.MustContain(t, e.Error(), "denied")
}

func TestCLIOAuthAccessTokenPersistsToStore(t *testing.T) {
	idToken := jwtWithClaims(t, `{"sub":"stored-user","email":"stored@example.com","team_id":"stored-team"}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"stored-access","refresh_token":"stored-refresh","id_token":"` + idToken + `","expires_in":3600}`))
	}))
	defer server.Close()

	s := newTestGrokStore(t, "test:")

	acc := &store.Account{
		AccountType:       "grok",
		CredentialType:    "oauth",
		OAuthAccessToken:  "stale",
		OAuthRefreshToken: "old-refresh",
		OAuthExpiresAt:    time.Now().Add(-time.Minute),
		Enabled:           true,
	}
	testutil.NoError(t, s.CreateAccount(context.Background(), acc), "CreateAccount() error = %v")

	cfg := &config.Config{}
	cfg.GrokCLIOAuthTokenURL = server.URL
	oauth := NewCLIOAuth(cfg, server.Client())
	oauth.SetAccountStore(s)

	token, err := oauth.AccessToken(context.Background(), acc)
	testutil.NoError(t, err, "AccessToken() error = %v")
	testutil.Equal(t, token, "stored-access")

	got, err := s.GetAccount(context.Background(), acc.ID)
	testutil.NoError(t, err, "GetAccount() error = %v")
	testutil.Equal(t, got.OAuthAccessToken, "stored-access")
	testutil.Equal(t, got.OAuthRefreshToken, "stored-refresh")
	testutil.Falsef(t, got.Email != "stored@example.com" || got.Name != "stored@example.com" || got.UserID != "stored-user" || got.TeamID != "stored-team", "stored OAuth identity=%+v", got)
	testutil.False(t, strings.Contains(got.OAuthAccessToken, idToken) || strings.Contains(got.OAuthRefreshToken, idToken), "id_token must not be persisted as a credential")
}

func TestCLIClientFetchModelCatalogParsesRealBuildFixture(t *testing.T) {
	catalogBody, err := os.ReadFile("testdata/build_models_catalog.json")
	testutil.NoError(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(catalogBody)
	}))
	defer server.Close()
	client := NewCLIClient(&config.Config{GrokCLIBaseURL: server.URL})
	catalog, err := client.FetchModelCatalog(context.Background(), &store.Account{OAuthAccessToken: "active-access", OAuthExpiresAt: time.Now().Add(time.Hour)})
	testutil.NoError(t, err)
	testutil.Falsef(t, len(catalog) != 2 || catalog[0].ModelID != "grok-4.7" || catalog[1].ModelID != "grok-4.6", "catalog=%+v", catalog)
	profile := catalog[0]
	testutil.Falsef(t, strings.Join(profile.ReasoningEfforts, ",") != "xhigh,high,medium,low" || profile.DefaultReasoningEffort != "high" || !profile.SupportsReasoningEffort || profile.ContextWindow != 500000 || profile.MaxCompletionTokens != 1000000 || !profile.SupportsBackendSearch, "grok-4.7 profile=%+v", profile)
}

func TestCLIClientFetchModelsReadsOfficialControlPlaneCatalog(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		testutil.Falsef(t, r.Method != http.MethodGet || r.URL.Path != "/models", "request=%s %s want GET /models", r.Method, r.URL.Path)
		testutil.Equal(t, r.Header.Get("Authorization"), "Bearer active-access")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"grok-4.6"},{"id":"grok-4.6"},{"modelId":"grok-4.5"},{"id":"hidden-model","hidden":true},{"id":""}]}`))
	}))
	defer server.Close()

	client := NewCLIClient(&config.Config{GrokCLIBaseURL: server.URL})
	catalog, err := client.FetchModelCatalog(context.Background(), &store.Account{
		AccountType:      "grok",
		CredentialType:   "oauth",
		OAuthAccessToken: "active-access",
		OAuthExpiresAt:   time.Now().Add(time.Hour),
	})
	testutil.NoError(t, err, "FetchModelCatalog() error = %v")
	models := modelcatalog.ModelIDs(catalog)
	testutil.Equal(t, strings.Join(models, ","), "grok-4.6,grok-4.5")
}

func TestCLIResponsesRejectsTeamModelCooldownBeforeUpstream(t *testing.T) {
	previous := teamCooldown
	teamCooldown = newTeamCooldownRegistry()
	defer func() { teamCooldown = previous }()
	teamCooldown.Note(RateLimitScopeRPM, ProviderBuild+":team:team-1", "grok-4.6", time.Minute)

	client := &CLIClient{}
	started := time.Now()
	_, err := client.doResponsesAt(context.Background(), &store.Account{TeamID: "team-1"}, "/responses", map[string]any{"model": "grok-4.6"})
	testutil.Falsef(t, err == nil || !strings.Contains(err.Error(), "status=429") || !strings.Contains(err.Error(), "retry-after"), "error=%v want immediate team/model cooldown 429 with retry-after", err)
	elapsed := time.Since(started)
	testutil.Falsef(t, elapsed > 100*time.Millisecond, "team cooldown blocked for %s; it must reject immediately so the retry loop can rotate accounts", elapsed)
}
