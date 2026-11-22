package cline

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/prompt"
	"orchids-api/internal/responses"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

const fixturePNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="

func TestClineForwardsImagesInChatRequests(t *testing.T) {
	const imageURL = "data:image/png;base64," + fixturePNG
	const chatContent = `[{"type":"text","text":"Before"},{"type":"image_url","image_url":{"url":"` + imageURL + `"}},{"type":"text","text":"After"}]`
	const nativeContent = `[{"type":"text","text":"Before"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + fixturePNG + `"}},{"type":"text","text":"After"}]`
	const responseInput = `[{"role":"user","content":[{"type":"input_text","text":"Before"},{"type":"input_image","image_url":"` + imageURL + `"},{"type":"input_text","text":"After"}]}]`
	var wantContent any
	testutil.NoError(t, json.Unmarshal([]byte(chatContent), &wantContent))

	for _, protocol := range []string{"chat", "messages", "responses"} {
		t.Run(protocol, func(t *testing.T) {
			var user prompt.Message
			var err error
			switch protocol {
			case "chat":
				user, err = prompt.FromChat("user", json.RawMessage(chatContent), "", nil, "")
			case "messages":
				user.Role = "user"
				err = json.Unmarshal([]byte(nativeContent), &user.Content)
			case "responses":
				var input any
				testutil.NoError(t, json.Unmarshal([]byte(responseInput), &input))
				messages, convertErr := responses.InputToMessages(input)
				testutil.NoError(t, convertErr)
				testutil.Equal(t, len(messages), 1)
				user, err = prompt.FromChat(messages[0].Role, messages[0].Content, "", nil, "")
			}
			testutil.NoError(t, err)

			// Cline may repair a refused reasoning field before the first delta.
			// Both attempts must keep the image and the paired tool history.
			rec := &chatRecorder{responses: []func(http.ResponseWriter){
				refuse(http.StatusBadRequest, `{"error":{"message":"Missing required parameter: 'messages[1].reasoning_content'."}}`),
			}}
			var answer strings.Builder
			req := upstream.UpstreamRequest{Messages: []prompt.Message{
				{Role: "user", Content: prompt.MessageContent{Text: "Remember this"}},
				{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{
					{Type: "tool_use", ID: "call_image", Name: "lookup", Input: map[string]any{}},
				}}},
				{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{
					{Type: "tool_result", ToolUseID: "call_image", Content: "Lookup complete"},
				}}},
				user,
			}}
			err = runRecordedChat(t, rec, req, func(msg upstream.SSEMessage) {
				if msg.Type == "model.text-delta" {
					delta, _ := msg.Event["delta"].(string)
					answer.WriteString(delta)
				}
			})
			testutil.NoError(t, err)
			testutil.Equal(t, answer.String(), "ok")
			testutil.Equal(t, len(rec.bodies), 2)
			for i, body := range rec.bodies {
				messages, _ := body["messages"].([]any)
				if len(messages) != 4 {
					t.Fatalf("attempt %d: unexpected messages: %#v", i+1, messages)
				}
				before, _ := messages[0].(map[string]any)
				assistant, _ := messages[1].(map[string]any)
				result, _ := messages[2].(map[string]any)
				after, _ := messages[3].(map[string]any)
				if before["role"] != "user" || before["content"] != "Remember this" || assistant["role"] != "assistant" ||
					result["role"] != "tool" || result["tool_call_id"] != "call_image" || result["content"] != "Lookup complete" {
					t.Fatalf("attempt %d: changed text or tool history: %#v", i+1, messages[:3])
				}
				if after["role"] != "user" || !reflect.DeepEqual(after["content"], wantContent) {
					t.Fatalf("attempt %d: incorrect image message: %#v", i+1, after)
				}
			}
		})
	}
}

func TestClineRejectsInvalidImagesBeforeCallingUpstream(t *testing.T) {
	const invalidData = "invalid-base64-fixture!"
	rec := &chatRecorder{}
	err := runRecordedChat(t, rec, upstream.UpstreamRequest{Messages: []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{
			{Type: "text", Text: "There should be an image here"},
			{Type: "image_url", URL: "data:image/png;base64," + invalidData},
		}}},
	}}, nil)
	message, ok := apperrors.LocalRequestMessage(err)
	var appErr *apperrors.AppError
	if !ok || !strings.Contains(message, "Cline") || strings.Contains(message, invalidData) ||
		!errors.As(err, &appErr) || appErr.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("expected a redacted Cline image validation error with status 400, got %v", err)
	}
	testutil.Equal(t, len(rec.bodies), 0)
}
