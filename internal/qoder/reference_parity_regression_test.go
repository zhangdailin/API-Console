package qoder

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"encoding/json"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

// A 401 whose body names an authentication_error type is still a credential
// verdict. Reading the marker as a client fault skipped the refresh.
func TestUnauthorizedWithAuthenticationErrorTypeRefreshes(t *testing.T) {
	for _, status := range []int{401, 403} {
		err := classifyStatus(status, "", []byte(`{"error":{"type":"authentication_error","message":"token expired"}}`))
		testutil.Falsef(t, !isUnauthorized(err), "status %d: err = %v, want unauthorized", status, err)
	}
	frame := `data: {"statusCodeValue":401,"body":"{\"type\":\"permission_error\",\"message\":\"token expired\"}"}` + "\n\n"
	_, err := consumeStreamObserved(strings.NewReader(frame), nil, nil)
	testutil.Falsef(t, err == nil || !strings.Contains(err.Error(), "rejected the credential"), "in-stream err = %v, want credential rejection", err)
	// A non-auth status keeps the client-fault fast path.
	err = classifyStatus(400, "", []byte(`{"error":{"type":"invalid_request_error"}}`))
	testutil.Falsef(t, isUnauthorized(err) || !strings.Contains(err.Error(), "rejected the request parameters"), "400 err = %v", err)
}

// The busy business code counts only as a code field value; the same digits
// inside a timestamp or trace id must not turn a 401 into a busy verdict.
func TestBusyCodeMatchesOnlyCodeFields(t *testing.T) {
	err := classifyStatus(401, "", []byte(`{"code":"401","message":"invalid token","timestamp":1761060512345}`))
	testutil.Falsef(t, !isUnauthorized(err), "err = %v, want unauthorized", err)
	for _, value := range []string{`{"code":"10605"}`, `{"code":10605}`, `{"msgCode": "10605"}`, `{\"code\":\"10605\",\"message\":\"x\"}`} {
		testutil.Falsef(t, !sharedQueueRefusal(value), "%s not recognised as busy", value)
	}
	for _, value := range []string{`{"traceId":"a10605b"}`, `{"code":"106051"}`, `{"requestTime":1761060512345}`} {
		testutil.Falsef(t, sharedQueueRefusal(value), "%s wrongly treated as busy", value)
	}
}

// Login expired (business code 105) can arrive inside a 200 envelope with no
// success flag. It must reach the refresh, not end as a truncated stream.
func TestInStreamLoginExpiredIsUnauthorized(t *testing.T) {
	for name, body := range map[string]string{
		"wrapped body": envelope(`{"code":"105","message":"Login expired"}`),
		"bare frame":   "data: " + `{"code":105,"message":"Login expired"}` + "\n\n",
	} {
		_, err := consumeStreamObserved(strings.NewReader(body), nil, nil)
		testutil.Falsef(t, err == nil || !strings.Contains(err.Error(), "rejected the credential"), "%s: err = %v", name, err)
	}
}

// The default completion cap and thinking switch come from the catalog row the
// way the official client reads them, and survive the stored snapshot.
func TestCatalogOutputCapAndThinkingConfigDriveDefaults(t *testing.T) {
	catalog, err := parseModelList([]byte(`{"chat":[
 {"key":"capped","max_output_tokens":8000,"is_reasoning":false},
 {"key":"string_cap","max_output_tokens":"6000"},
 {"key":"bad_cap","max_output_tokens":0,"thinking_config":null,"is_reasoning":true},
 {"key":"think","is_reasoning":false,"thinking_config":{"enabled":{"efforts":["low","high"]}}},
 {"key":"no_think","is_reasoning":true,"thinking_config":{"enabled":null}},
 {"key":"odd","is_reasoning":false,"thinking_config":"unexpected"}
]}`))
	testutil.NoError(t, err)
	catalog = catalogFromIDs(catalogToIDs(catalog))
	for _, tc := range []struct {
		key       string
		maxTokens int
		reasoning bool
	}{{"capped", 8000, false}, {"string_cap", 6000, false}, {"bad_cap", 32000, true}, {"think", 32000, true}, {"no_think", 32000, false}, {"odd", 32000, true}} {
		model, err := catalog.Resolve(tc.key)
		testutil.NoError(t, err)
		body := decodeRegressionBody(t, upstream.UpstreamRequest{Prompt: "hello"}, model)
		parameters := body.Parameters.(map[string]any)
		testutil.EqualAny(t, parameters["max_tokens"], float64(tc.maxTokens))
		testutil.Equal(t, body.ModelConfig.IsReasoning, tc.reasoning)
	}

	// Explicit client controls still win over the catalog defaults.
	model, err := catalog.Resolve("think")
	testutil.NoError(t, err)
	limit := 123
	body := decodeRegressionBody(t, upstream.UpstreamRequest{Prompt: "hello", MaxTokens: &limit, ReasoningEffort: "none"}, model)
	testutil.EqualAny(t, body.Parameters.(map[string]any)["max_tokens"], float64(123))
	testutil.Equal(t, body.ModelConfig.IsReasoning, false)
}

func decodeRegressionBody(t *testing.T, req upstream.UpstreamRequest, model modelEntry) chatBody {
	t.Helper()
	encoded, err := buildChatBodyProfile(req, model, "session", "request", "request-set", DefaultClientVersion, "", sceneBusinessProduct)
	testutil.NoError(t, err)
	raw, err := decodeBody(encoded)
	testutil.NoError(t, err)
	var body chatBody
	testutil.NoError(t, json.Unmarshal(raw, &body))
	return body
}

// A dead login session is a credential verdict under any status, and its
// business code counts only as a code field value.
func TestSessionDeadMarkersReachTheRefresh(t *testing.T) {
	for _, body := range []string{
		`{"code":"TOKEN_EXPIRE","message":"token expired"}`,
		`{"code":12153,"message":"x"}`,
		`{"message":"Offline user session not found"}`,
		`{"body":"{\"msgCode\":\"12153\"}"}`,
	} {
		testutil.Falsef(t, !IsSessionDead(body), "%s not recognised as a dead session", body)
		err := classifyStatus(400, "", []byte(body))
		testutil.Falsef(t, !isUnauthorized(err), "400 %s: err = %v, want unauthorized", body, err)
	}
	for _, body := range []string{`{"timestamp":1792312153000}`, `{"traceId":"a12153b"}`, `{"code":"121530"}`} {
		testutil.Falsef(t, IsSessionDead(body), "%s wrongly treated as a dead session", body)
	}
}

func TestRefreshReportsADeadSessionAsReLogin(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":"12153","message":"Offline user session not found"}`))
	}))
	defer server.Close()
	client := NewFromAccount(signedTestAccount(), nil)
	setTestEndpoints(client, server.URL, server.URL, server.URL)
	_, err := client.Refresh(context.Background(), "refresh-1")
	testutil.Falsef(t, !errors.Is(err, ErrReLoginRequired), "err = %v, want ErrReLoginRequired", err)
}
