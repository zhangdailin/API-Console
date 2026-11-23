package grok

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func TestBuildChatSkipsBlankUserTextParts(t *testing.T) {
	type message struct {
		Type, Role string
		Content    any
	}
	textPart := func(text string) map[string]any { return map[string]any{"type": "input_text", "text": text} }
	seen := make(chan []message, 3)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Input  []message
			Stream bool
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		seen <- payload.Input
		if payload.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, parityText("answer")+parityTerminal("response.completed"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_blank_text","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}]}`)
	}))
	defer upstream.Close()

	const model = "grok-4.7"
	h, s, _ := setupValidationHandler(t)
	testutil.NoError(t, s.CreateModel(t.Context(), &store.Model{Channel: "Grok", ModelID: model, Name: model, Status: store.ModelStatusAvailable, Verified: true}))
	testutil.NoError(t, s.CreateAccount(t.Context(), &store.Account{
		AccountType: "grok", GrokProvider: ProviderBuild, CredentialType: "oauth", Enabled: true,
		OAuthAccessToken: jwtWithClaims(t, `{"sub":"blank-text-user","team_id":"blank-text-team"}`),
		OAuthExpiresAt:   time.Now().Add(time.Hour), GrokModels: []string{model}, GrokModelsSyncedAt: time.Now(),
	}))
	h.cfg = &config.Config{GrokCLIBaseURL: upstream.URL + "/v1"}
	h.cliClient = NewCLIClient(h.cfg)
	h.cliClient.httpClient = upstream.Client()

	request := func(stream bool, messages string) *httptest.ResponseRecorder {
		t.Helper()
		body := `{"model":"` + model + `","stream":` + strconv.FormatBool(stream) + `,"messages":` + messages + `}`
		rec := httptest.NewRecorder()
		h.HandleChatCompletions(rec, httptest.NewRequest(http.MethodPost, "/grok/v1/chat/completions", strings.NewReader(body)))
		return rec
	}
	for _, stream := range []bool{false, true} {
		rec := request(stream, `[
			{"role":"user","content":[{"type":"text","text":"\n"},{"type":"text","text":"First question"}]},
			{"role":"assistant","content":"First answer","reasoning_content":"plan"},
			{"role":"user","content":[{"type":"text","text":" "},{"type":"input_text","text":"Next question"},{"type":"input_text","text":"\t"},{"type":"text","text":"More context"}]}
		]`)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"content":"answer"`) {
			t.Fatalf("stream=%t: status=%d body=%s", stream, rec.Code, rec.Body)
		}
		want := []message{
			{"message", "user", []any{textPart("First question")}},
			{"message", "assistant", "Prior model reasoning summary:\nplan"},
			{"message", "assistant", []any{textPart("First answer")}},
			{"message", "user", []any{textPart("Next question"), textPart("More context")}},
		}
		select {
		case got := <-seen:
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("upstream input = %#v, want %#v", got, want)
			}
		default:
			t.Fatal("valid chat was not forwarded")
		}
	}
	for _, messages := range []string{
		`[{"role":"user","content":[{"type":"text","text":" "},{"type":"input_text","text":""}]}]`,
		`[{"role":"user","content":[{"type":"text","text":42},{"type":"text","text":"Valid text"}]}]`,
	} {
		rec := request(false, messages)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "text content cannot be empty") || len(seen) != 0 {
			t.Fatalf("invalid chat: status=%d body=%s upstream calls=%d", rec.Code, rec.Body, len(seen))
		}
	}
}
