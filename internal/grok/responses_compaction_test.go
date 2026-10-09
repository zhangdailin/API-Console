package grok

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"orchids-api/internal/responses"
	"strings"
	"testing"
	"time"

	"encoding/json"

	"orchids-api/internal/config"
	"orchids-api/internal/secureblob"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

const compactionTestSummary = "1. Primary Request and Intent: keep the session going across account switches.\n" +
	"2. Key Technical Concepts: Responses wire format, remote-v2 compaction, sealed gateway state, summary turn sampling.\n" +
	"3. Files and Code Sections: internal/grok/responses_compaction.go carries the codec, the classifier and the cleaner.\n" +
	"4. Errors and Fixes: an undecodable blob is a 400 that names the input item, never a silent drop or a relay.\n" +
	"5. Problem Solving: the summary travels inside the client's own history instead of a row on one account.\n" +
	"6. All User Messages: compact this session and continue from the summary that comes back.\n"

func testCompactionCipher(t *testing.T) *secureblob.Cipher {
	t.Helper()
	cipher, err := secureblob.NewCipher([]byte("0123456789abcdef0123456789abcdef"))
	testutil.NoError(t, err, "secureblob.NewCipher: %v")
	testutil.False(t, !cipher.Available(), "cipher is not available")
	return cipher
}

// setupCompactionHandler wires a Build account to a mock upstream and enables
// gateway compaction, the same shape the CLI responses tests use.
func setupCompactionHandler(t *testing.T, upstream *httptest.Server) (*Handler, *store.Store, func()) {
	t.Helper()
	h, s, _ := setupValidationHandler(t)
	if err := s.CreateModel(context.Background(), &store.Model{
		Channel: "Grok", ModelID: "grok-4.5", Name: "Grok 4.5",
		Status: store.ModelStatusAvailable, Verified: true,
	}); err != nil {
		t.Fatalf("CreateModel: %v", err)
	}
	if err := s.CreateAccount(context.Background(), &store.Account{
		AccountType: "grok", Enabled: true, CredentialType: "oauth", GrokProvider: ProviderBuild,
		OAuthAccessToken:   jwtWithClaims(t, `{"sub":"user-1","team_id":"team-1"}`),
		OAuthExpiresAt:     time.Now().Add(time.Hour),
		GrokModels:         []string{"grok-4.5"},
		GrokModelsSyncedAt: time.Now(),
	}); err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	h.cfg = &config.Config{GrokCLIBaseURL: upstream.URL + "/v1"}
	h.cliClient = NewCLIClient(h.cfg)
	h.cliClient.SetAccountStore(s)
	h.cliClient.httpClient = upstream.Client()
	h.cliClient.oauth.httpClient = upstream.Client()
	h.SetCompactionCipher(testCompactionCipher(t))
	return h, s, func() { _ = s.Close() }
}

func compactionUpstream(t *testing.T, summary string, received *map[string]interface{}) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		testutil.NoError(t, err, "read upstream body: %v")
		if received != nil {
			var decoded map[string]interface{}
			testutil.NoError(t, json.Unmarshal(body, &decoded), "decode upstream body: %v")
			*received = decoded
		}
		completed := map[string]interface{}{
			"id": "resp_upstream_1", "object": "response", "status": "completed", "model": "grok-4.5",
			"output": []interface{}{map[string]interface{}{
				"id": "msg_1", "type": "message", "role": "assistant",
				"content": []interface{}{map[string]interface{}{"type": "output_text", "text": summary}},
			}},
			"usage": map[string]interface{}{"input_tokens": float64(1200), "output_tokens": float64(300)},
		}
		completedJSON, _ := json.Marshal(map[string]interface{}{"type": "response.completed", "response": completed})
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\ndata: "+string(completedJSON)+"\n\n")
	}))
}

func TestClassifyResponsesCompactionPayload(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]interface{}
		want    responses.CompactionKind
	}{
		{
			name:    "codex remote-v2 trigger",
			payload: map[string]interface{}{"input": []interface{}{map[string]interface{}{"type": "compaction_trigger"}}},
			want:    responses.CompactionTrigger,
		},
		{
			name: "trigger mixed with ordinary items",
			payload: map[string]interface{}{"input": []interface{}{
				map[string]interface{}{"type": "message", "role": "user", "content": "hello"},
				map[string]interface{}{"type": "compaction_trigger"},
			}},
			want: responses.CompactionTrigger,
		},
		{
			name: "grok tui prompt as last user item",
			payload: map[string]interface{}{"input": []interface{}{
				map[string]interface{}{"type": "message", "role": "user", "content": "earlier turn"},
				map[string]interface{}{"type": "message", "role": "user", "content": []interface{}{
					map[string]interface{}{"type": "input_text", "text": "Please summarize. Note: " + responses.ClientCompactionPromptMarker},
				}},
			}},
			want: responses.CompactionTUI,
		},
		{
			name: "tui prompt recognised on the messages wire too",
			payload: map[string]interface{}{"messages": []interface{}{
				map[string]interface{}{"role": "user", "content": responses.ClientCompactionPromptMarker},
			}},
			want: responses.CompactionTUI,
		},
		{
			name: "tui marker not last is an ordinary turn",
			payload: map[string]interface{}{"input": []interface{}{
				map[string]interface{}{"type": "message", "role": "user", "content": responses.ClientCompactionPromptMarker},
				map[string]interface{}{"type": "message", "role": "assistant", "content": "ok"},
			}},
			want: responses.CompactionNone,
		},
		{
			name:    "assistant cannot trigger it",
			payload: map[string]interface{}{"input": []interface{}{map[string]interface{}{"type": "message", "role": "assistant", "content": responses.ClientCompactionPromptMarker}}},
			want:    responses.CompactionNone,
		},
		{
			name:    "ordinary conversation",
			payload: map[string]interface{}{"input": []interface{}{map[string]interface{}{"type": "message", "role": "user", "content": "hello"}}},
			want:    responses.CompactionNone,
		},
		{name: "empty payload", payload: nil, want: responses.CompactionNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			testutil.Equal(t, responses.ClassifyCompactionPayload(tc.payload), tc.want)
		})
	}
}

func TestBuildGatewayCompactionResponseShape(t *testing.T) {
	response := map[string]interface{}{
		"id": "resp_abc", "status": "completed", "output_text": "leak me not",
		"usage": map[string]interface{}{"input_tokens": float64(10), "output_tokens": float64(5)},
	}
	result := responses.BuildCompactionResponse(response, "g2a_compact_v1.xyz", "grok-4.5")

	testutil.Equal(t, result["id"], "resp_abc")
	testutil.Equal(t, result["object"], "response.compaction")
	testutil.Equal(t, result["status"], "completed")
	testutil.Equal(t, result["model"], "grok-4.5")
	_, present := result["output_text"]
	testutil.False(t, present, "output_text leaked into the compaction response")
	output := result["output"].([]interface{})
	item := output[0].(map[string]interface{})
	testutil.Equal(t, item["type"], "compaction")
	testutil.Equal(t, item["encrypted_content"], "g2a_compact_v1.xyz")
	testutil.Equal(t, item["id"], "cmp_abc")
	usage := result["usage"].(map[string]interface{})
	testutil.EqualAny(t, usage["total_tokens"], int64(15))
	// A response without usage keeps it absent instead of inventing zeros.
	noUsage := responses.BuildCompactionResponse(map[string]interface{}{"id": "resp_x"}, "blob", "grok-4.5")
	_, present = noUsage["usage"]
	testutil.False(t, present, "usage was fabricated")

	// The streamed form is the same answer as six ordered events.
	var builder strings.Builder
	testutil.NoError(t, responses.WriteCompactionStream(&builder, result), "write stream: %v")
	names := make([]string, 0, 6)
	var completedPayload map[string]interface{}
	if err := responses.ConsumeSSE(strings.NewReader(builder.String()), func(event responses.SSEEvent) error {
		names = append(names, event.Event)
		if event.Event == "response.completed" {
			if err := json.Unmarshal(event.Data(), &completedPayload); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("consume stream: %v", err)
	}
	want := []string{"response.created", "response.in_progress", "response.output_item.added", "keepalive", "response.output_item.done", "response.completed"}
	testutil.Equal(t, strings.Join(names, ","), strings.Join(want, ","))
	completedResponse := completedPayload["response"].(map[string]interface{})
	testutil.Equal(t, completedResponse["status"], "completed")
}

// asError is a tiny local alias so the test does not have to import errors just
// for one assertion.
func asError(err error, target interface{}) bool {
	switch typed := target.(type) {
	case **responses.CompactionBlobError:
		blobErr, ok := err.(*responses.CompactionBlobError)
		if ok {
			*typed = blobErr
		}
		return ok
	default:
		return false
	}
}

// A compaction_trigger turn with streaming enabled must come back as a synthetic
// SSE sequence, not as a buffered JSON body: Codex holds the connection open.
func TestHandleResponsesCompactionTriggerStreamsSyntheticEvents(t *testing.T) {
	upstream := compactionUpstream(t, compactionTestSummary, nil)
	defer upstream.Close()
	h, _, cleanup := setupCompactionHandler(t, upstream)
	defer cleanup()

	body, _ := json.Marshal(map[string]interface{}{
		"model": "grok-4.5", "stream": true,
		"input": []interface{}{map[string]interface{}{"type": "compaction_trigger"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	h.HandleResponses(rec, req)

	testutil.Equal(t, rec.Code, http.StatusOK)
	got := rec.Header().Get("Content-Type")
	testutil.Falsef(t, !strings.Contains(got, "text/event-stream"), "Content-Type=%q", got)
	var events []string
	var blob string
	if err := responses.ConsumeSSE(strings.NewReader(rec.Body.String()), func(event responses.SSEEvent) error {
		events = append(events, event.Event)
		if event.Event == "response.completed" {
			var payload map[string]interface{}
			if err := json.Unmarshal(event.Data(), &payload); err != nil {
				return err
			}
			response := payload["response"].(map[string]interface{})
			item := response["output"].([]interface{})[0].(map[string]interface{})
			blob, _ = item["encrypted_content"].(string)
		}
		return nil
	}); err != nil {
		t.Fatalf("consume response stream: %v", err)
	}
	want := []string{"response.created", "response.in_progress", "response.output_item.added", "keepalive", "response.output_item.done", "response.completed"}
	testutil.Equal(t, strings.Join(events, ","), strings.Join(want, ","))
	testutil.Falsef(t, !strings.HasPrefix(blob, responses.CompactionPrefix), "streamed blob=%q is not gateway-owned", blob)
}
