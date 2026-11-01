package qoder

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"orchids-api/internal/upstream"
)

// TestCloudEventTextShapes pins the two text shapes the session stream has been
// observed to carry: a delta string, and a message whose content is a string or a
// list of parts.
func TestCloudEventTextShapes(t *testing.T) {
	for _, tc := range []struct {
		name string
		data map[string]any
		want string
	}{
		{"delta", map[string]any{"delta": "hello"}, "hello"},
		{"string content", map[string]any{"content": "hello"}, "hello"},
		{"parts", map[string]any{"content": []any{
			map[string]any{"type": "text", "text": "hel"},
			map[string]any{"type": "text", "text": "lo"},
		}}, "hello"},
		{"nested", map[string]any{"content": map[string]any{"content": "hello"}}, "hello"},
		{"absent", map[string]any{"type": "session.status_idle"}, ""},
	} {
		if got := cloudEventText(tc.data); got != tc.want {
			t.Fatalf("%s: cloudEventText() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestRunCloudTurnDrivesTheSessionLoop drives the whole closed loop against a
// fake session API: environment and agent creation, session creation, the event
// post, and the stream that answers it.
func TestRunCloudTurnDrivesTheSessionLoop(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	posted := make(chan struct{})
	var once sync.Once

	frames := []string{
		`data: {"type":"agent.message","sequence_num":1,"content":[{"type":"text","text":"OK"}]}`,
		"",
		`data: {"type":"agent.message","sequence_num":2,"content":[{"type":"text","text":"OK."}]}`,
		"",
		`data: {"type":"session.status_idle","sequence_num":3}`,
		"",
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()

		if got := r.Header.Get("Authorization"); got != "Bearer access-1" {
			t.Errorf("Authorization = %q, want the account bearer token", got)
		}
		if got := r.Header.Get("X-Qoder-Remote-Api-Version"); got != "2" {
			t.Errorf("X-Qoder-Remote-Api-Version = %q, want 2", got)
		}

		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/environments":
			_, _ = w.Write([]byte(`{"id":"env-1"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/agents":
			_, _ = w.Write([]byte(`{"id":"agent-1"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/sessions":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["agent"] != "agent-1" || body["environment_id"] != "env-1" {
				t.Errorf("session body = %v, want the created targets", body)
			}
			_, _ = w.Write([]byte(`{"id":"sess-1"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/sessions/sess-1/events":
			var body struct {
				Events []map[string]any `json:"events"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if len(body.Events) != 1 || body.Events[0]["type"] != "user.message" {
				t.Errorf("events = %v, want one user.message", body.Events)
			}
			once.Do(func() { close(posted) })
			_, _ = w.Write([]byte(`{"data":[{"id":"e1","type":"user.message"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/sessions/sess-1/events/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			// Answer only after the message was posted, the way a session does.
			select {
			case <-posted:
			case <-time.After(2 * time.Second):
			}
			for _, frame := range frames {
				if frame == "" {
					_, _ = fmt.Fprint(w, "\n")
					continue
				}
				_, _ = fmt.Fprintln(w, frame)
			}
		default:
			t.Errorf("unexpected call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	SetCloudBaseURL(server.URL)
	defer SetCloudBaseURL(DefaultCloudBaseURL)

	client := NewFromAccount(signedTestAccount(), nil)
	var answer strings.Builder
	result, err := client.RunCloudTurn(context.Background(), "say OK", func(message upstream.SSEMessage) {
		if message.Type == "model.text-delta" {
			if delta, ok := message.Event["delta"].(string); ok {
				answer.WriteString(delta)
			}
		}
	})
	if err != nil {
		t.Fatalf("RunCloudTurn() error = %v", err)
	}
	if result.SessionID != "sess-1" {
		t.Fatalf("session = %q, want sess-1", result.SessionID)
	}
	// The stream repeats the growing message, so the second frame must not
	// re-emit what the first already produced.
	if answer.String() != "OK." {
		t.Fatalf("answer = %q, want %q", answer.String(), "OK.")
	}
	if result.TextBytes != len("OK.") {
		t.Fatalf("text bytes = %d, want %d", result.TextBytes, len("OK."))
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{
		"POST /environments",
		"POST /agents",
		"POST /sessions",
		"POST /sessions/sess-1/events",
		"GET /sessions/sess-1/events/stream",
	}
	for _, path := range want {
		found := false
		for _, call := range calls {
			if call == path {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing call %q in %v", path, calls)
		}
	}
}
