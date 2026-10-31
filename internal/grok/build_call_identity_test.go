package grok

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

// TestConversationGroupIDMatchesTheOfficialDerivation pins grok-build's
// derive_conversation_group_id. The vector is Python's
// uuid.uuid5(uuid.NAMESPACE_OID, "xai:grok-build:conversation-group:" + root).
func TestConversationGroupIDMatchesTheOfficialDerivation(t *testing.T) {
	testutil.Equal(t, conversationGroupID("0f8fad5b-d9cb-469f-a165-70867728950e"), "ad426e20-d9fe-5695-aa8d-b5dde5acc1e2")
	testutil.Equal(t, conversationGroupID(" "), "")
}

func TestNormalizeTurnIndexAcceptsOnlyAU64(t *testing.T) {
	for value, want := range map[string]string{
		"0": "0", " 7 ": "7", "18446744073709551615": "18446744073709551615",
		"18446744073709551616": "", "-1": "", "1.5": "", "abc": "", "": "",
	} {
		testutil.Equal(t, normalizeTurnIndex(value), want)
	}
}

func buildIdentityServer(t *testing.T, seen *http.Header, etag string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if etag != "" {
			w.Header().Set("x-models-etag", etag)
		}
		switch r.URL.Path {
		case "/v1/responses":
			*seen = r.Header.Clone()
			_, _ = w.Write([]byte(`{"id":"resp_1","object":"response"}`))
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"grok-4.6"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func identityClient(server *httptest.Server) *CLIClient {
	client := NewCLIClient(&config.Config{GrokCLIBaseURL: server.URL + "/v1"})
	client.httpClient = server.Client()
	return client
}

// TestResponsesCarryConversationGroupAndClientTurn proves a stable session
// sends the group id derived from it, and that a turn index is forwarded only
// when the client sent one.
func TestResponsesCarryConversationGroupAndClientTurn(t *testing.T) {
	var seen http.Header
	server := buildIdentityServer(t, &seen, "")
	client := identityClient(server)
	acc := &store.Account{OAuthAccessToken: "access", OAuthExpiresAt: time.Now().Add(time.Hour)}

	incoming := httptest.NewRequest(http.MethodPost, "/grok/v1/responses", nil)
	incoming.Header.Set("x-grok-turn-idx", "3")
	ctx := withClientTurnIndex(context.Background(), incoming)
	resp, err := client.doResponsesAt(ctx, acc, "/responses", map[string]any{"model": "grok-4.6", "input": "hi", "prompt_cache_key": "session-a"})
	testutil.NoError(t, err)
	_ = resp.Body.Close()
	session := seen.Get("x-grok-conv-id")
	testutil.True(t, session != "", "a prompt cache key must yield a session")
	testutil.Equal(t, seen.Get("x-grok-conv-group-id"), conversationGroupID(session))
	testutil.Equal(t, seen.Get("x-grok-turn-idx"), "3")

	resp, err = client.doResponsesAt(context.Background(), acc, "/responses", map[string]any{"model": "grok-4.6", "input": "hi", "prompt_cache_key": "session-a"})
	testutil.NoError(t, err)
	_ = resp.Body.Close()
	testutil.Equal(t, seen.Get("x-grok-turn-idx"), "")

	resp, err = client.doResponsesAt(ctx, acc, "/responses", map[string]any{"model": "grok-4.6", "input": "hi"})
	testutil.NoError(t, err)
	_ = resp.Body.Close()
	testutil.Equal(t, seen.Get("x-grok-conv-group-id"), "")
	testutil.Equal(t, seen.Get("x-grok-turn-idx"), "")
}

// TestModelsETagChangeMarksTheCatalogStale proves an inference response whose
// x-models-etag differs from the last catalog read schedules a catalog read,
// and that the read clears the mark.
func TestModelsETagChangeMarksTheCatalogStale(t *testing.T) {
	previous := buildModelsETags
	buildModelsETags = &modelsETagTracker{baseline: map[int64]string{}, stale: map[int64]bool{}}
	defer func() { buildModelsETags = previous }()

	var seen http.Header
	etag := "v1"
	server := buildIdentityServer(t, &seen, "")
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-models-etag", etag)
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"grok-4.6"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response"}`))
	})
	client := identityClient(server)
	now := time.Now()
	acc := &store.Account{ID: 7, AccountType: "grok", CredentialType: "oauth", OAuthAccessToken: "access", OAuthExpiresAt: now.Add(time.Hour),
		GrokModels: []string{"grok-4.6"}, GrokModelsSyncedAt: now}

	_, err := client.FetchModelCatalog(context.Background(), acc)
	testutil.NoError(t, err)
	testutil.False(t, CLIModelsNeedSync(acc, now), "a fresh catalog read must not be stale")

	resp, err := client.doResponsesAt(context.Background(), acc, "/responses", map[string]any{"model": "grok-4.6", "input": "hi"})
	testutil.NoError(t, err)
	_ = resp.Body.Close()
	testutil.False(t, CLIModelsNeedSync(acc, now), "an unchanged etag must not mark the catalog")

	etag = "v2"
	resp, err = client.doResponsesAt(context.Background(), acc, "/responses", map[string]any{"model": "grok-4.6", "input": "hi"})
	testutil.NoError(t, err)
	_ = resp.Body.Close()
	testutil.True(t, CLIModelsNeedSync(acc, now), "a changed etag must mark the catalog")

	_, err = client.FetchModelCatalog(context.Background(), acc)
	testutil.NoError(t, err)
	testutil.False(t, CLIModelsNeedSync(acc, now), "the catalog read must clear the mark")
}
