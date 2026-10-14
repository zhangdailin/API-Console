//go:build live

package live

// Live end-to-end verification against the real WorkBuddy international
// backend. It is skipped unless both environment variables are present:
//
//	WB_LIVE=1
//	WB_AUTH_FILE=D:\path\to\auths\workbuddy-<uid>.json
//
// The account file may be either the bridge shape
// ({account:{uid},auth:{accessToken,refreshToken}}) or the desktop session file
// (workbuddy-desktop-ai.info). Running it spends real credits on the account,
// so it stays opt-in.

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/prompt"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
	"orchids-api/internal/workbuddy"
)

func liveClient(t *testing.T) *workbuddy.Client {
	t.Helper()

	path := strings.TrimSpace(os.Getenv("WB_AUTH_FILE"))
	if strings.TrimSpace(os.Getenv("WB_LIVE")) != "1" || path == "" {
		t.Skip("set WB_LIVE=1 and WB_AUTH_FILE to run the live WorkBuddy checks")
	}
	raw, err := os.ReadFile(path)
	testutil.NoError(t, err, "read auth file: %v")

	return workbuddy.NewFromAccount(&store.Account{
		AccountType:  "workbuddy",
		Enabled:      true,
		ClientCookie: string(raw),
	}, &config.Config{RequestTimeout: 120})
}

func TestLive_FetchModels(t *testing.T) {
	client := liveClient(t)
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	models, err := client.FetchModels(ctx)
	testutil.NoError(t, err, "FetchModels() error = %v")
	testutil.NotEqual(t, len(models), 0)

	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	t.Logf("cli catalog (%d): %s", len(ids), strings.Join(ids, ", "))

	want := []string{"default-model", "hy3"}
	for _, id := range want {
		found := false
		for _, got := range ids {
			if got == id {
				found = true
				break
			}
		}
		testutil.CheckTrue(t, found, "model %q missing from the cli catalog")
	}
}

func TestLive_ChatStream(t *testing.T) {
	client := liveClient(t)
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	var text strings.Builder
	var reasoning strings.Builder
	var eventTypes []string

	err := client.SendRequestWithPayload(ctx, upstream.UpstreamRequest{
		Model: "hy3",
		Messages: []prompt.Message{{
			Role:    "user",
			Content: prompt.MessageContent{Text: "Reply with exactly: pong"},
		}},
	}, func(msg upstream.SSEMessage) {
		switch msg.Type {
		case "model.text-delta":
			if delta, ok := msg.Event["delta"].(string); ok {
				text.WriteString(delta)
			}
		case "model.reasoning-delta":
			if delta, ok := msg.Event["delta"].(string); ok {
				reasoning.WriteString(delta)
			}
		case "model.tokens-used":
			t.Logf("usage: %+v", msg.Event)
		}
		eventTypes = append(eventTypes, msg.Type)
	}, nil)
	testutil.NoError(t, err, "SendRequestWithPayload() error = %v")

	t.Logf("events=%v reasoning_len=%d", eventTypes, reasoning.Len())
	t.Logf("text=%q", text.String())
	testutil.MustContain(t, strings.ToLower(text.String()), "pong")
}

func TestLive_ToolCall(t *testing.T) {
	client := liveClient(t)
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	type toolCall struct {
		name  string
		input string
	}
	var calls []toolCall

	err := client.SendRequestWithPayload(ctx, upstream.UpstreamRequest{
		Model: "gpt-5.6-sol",
		Messages: []prompt.Message{{
			Role:    "user",
			Content: prompt.MessageContent{Text: "List the files in the current directory."},
		}},
		Tools: []any{map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "list_files",
				"description": "List files in a directory",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"path": map[string]any{"type": "string"}},
				},
			},
		}},
	}, func(msg upstream.SSEMessage) {
		if msg.Type != "model.tool-call" {
			return
		}
		name, _ := msg.Event["toolName"].(string)
		input, _ := msg.Event["input"].(string)
		calls = append(calls, toolCall{name: name, input: input})
	}, nil)
	testutil.NoError(t, err, "SendRequestWithPayload() error = %v")

	testutil.NotEqual(t, len(calls), 0)
	t.Logf("tool calls: %+v", calls)
	if calls[0].name == "" || !strings.HasPrefix(strings.TrimSpace(calls[0].input), "{") {
		t.Fatalf("tool call = %+v, want a named call with a JSON object input", calls[0])
	}
}
