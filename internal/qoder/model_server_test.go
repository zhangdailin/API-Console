package qoder

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/upstream"
)

// TestModelServerChatURLDefaultsAndOverrides pins the route the client uses and
// the override it honours.
func TestModelServerChatURLDefaultsAndOverrides(t *testing.T) {
	t.Setenv(envModelServerHost, "")
	testutilEqualString(t, ModelServerChatURL(), "https://api2-v2.qoder.sh/model/v1/chat/completions")

	t.Setenv(envModelServerHost, "https://model.example.test/")
	testutilEqualString(t, ModelServerChatURL(), "https://model.example.test/model/v1/chat/completions")
}

// TestSendModelServerChatForwardsTheBodyAndStreamsDeltas drives the transport
// against a fake model server: bearer auth, the caller's body verbatim, and one
// delta per choice.
func TestSendModelServerChatForwardsTheBodyAndStreamsDeltas(t *testing.T) {
	var seenAuth, seenBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		var builder strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Body.Read(buf)
			builder.Write(buf[:n])
			if err != nil {
				break
			}
		}
		seenBody = builder.String()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"O"}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"K"}}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	t.Setenv(envModelServerHost, server.URL)

	client := NewFromAccount(signedTestAccount(), nil)
	var answer strings.Builder
	result, err := client.SendModelServerChat(context.Background(), []byte(`{"model":"x","stream":true}`), func(message upstream.SSEMessage) {
		if message.Type == "model.text-delta" {
			if delta, ok := message.Event["delta"].(string); ok {
				answer.WriteString(delta)
			}
		}
	})
	if err != nil {
		t.Fatalf("SendModelServerChat() error = %v", err)
	}
	if answer.String() != "OK" {
		t.Fatalf("answer = %q, want %q", answer.String(), "OK")
	}
	if result.TextBytes != 2 || result.Status != http.StatusOK {
		t.Fatalf("result = %+v, want 2 text bytes and status 200", result)
	}
	if seenAuth != "Bearer access-1" {
		t.Fatalf("Authorization = %q, want the account bearer token", seenAuth)
	}
	if seenBody != `{"model":"x","stream":true}` {
		t.Fatalf("body = %q, want the caller's body verbatim", seenBody)
	}
}

func testutilEqualString(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
