package qoder

import (
	"reflect"
	"testing"

	"encoding/json"
	"orchids-api/internal/prompt"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

func TestBlockMessagePreservesMultimodalOrderAcrossToolBoundaries(t *testing.T) {
	t.Parallel()
	var blocks []prompt.ContentBlock
	if err := json.Unmarshal([]byte(`[
  {"type":"text","text":"before"},
  {"type":"image","source":{"type":"url","url":"https://example.test/one.png"}},
  {"type":"text","text":"between"},
  {"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"aW1n"}},
  {"type":"text","text":"after"},
  {"type":"tool_result","tool_use_id":"call-1","content":"one"},
  {"type":"image","url":"https://example.test/two.png"},
  {"type":"text","text":"second"},
  {"type":"tool_result","tool_use_id":"call-2","content":"two"},
  {"type":"text","text":"plain one"},
  {"type":"image"},
  {"type":"text","text":"plain two"},
  {"type":"tool_result","tool_use_id":"call-2","content":"duplicate"},
  {"type":"tool_result","tool_use_id":"missing","content":"dangling"},
  {"type":"text","text":"last"},
  {"type":"image","url":"https://example.test/three.png"},
  {"type":"text","text":"tail"}
 ]`), &blocks); err != nil {
		t.Fatal(err)
	}
	image := func(url string) chatPart { return chatPart{Type: "image_url", ImageURL: &chatImageURL{URL: url}} }
	text := func(value string) chatPart { return chatPart{Type: "text", Text: value} }
	for _, role := range []string{"user", "system"} {
		t.Run(role, func(t *testing.T) {
			ids := map[string]bool{"call-1": true, "call-2": true}
			msg := prompt.Message{Role: role, Content: prompt.MessageContent{Blocks: blocks}}
			got := convertBlockMessage(role, msg, ids)
			want := []chatMessage{
				{Role: role, Contents: []chatPart{text("before"), image("https://example.test/one.png"), text("between"), image("data:image/jpeg;base64,aW1n"), text("after")}},
				{Role: "tool", ToolCallID: "call-1", Content: "one"},
				{Role: role, Contents: []chatPart{image("https://example.test/two.png"), text("second")}},
				{Role: "tool", ToolCallID: "call-2", Content: "two"},
				{Role: role, Content: "plain one\nplain two"},
				{Role: role, Contents: []chatPart{text("last"), image("https://example.test/three.png"), text("tail")}},
			}
			testutil.Falsef(t, !reflect.DeepEqual(got, want), "messages = %#v; want %#v", got, want)
			testutil.Equal(t, len(ids), 0)

			// Verify the signed/encoded request path retains that exact representation.
			req := upstream.UpstreamRequest{Messages: []prompt.Message{
				{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{
					{Type: "tool_use", ID: "call-1", Name: "first", Input: map[string]any{}},
					{Type: "tool_use", ID: "call-2", Name: "second", Input: map[string]any{}},
				}}}, msg,
			}}
			encoded, err := buildChatBodyProfile(req, modelEntry{Key: "test"}, "session", "request", "request-set", DefaultClientVersion, "", sceneBusinessProduct)
			testutil.NoError(t, err)
			raw, err := decodeBodyForTest(encoded)
			testutil.NoError(t, err)
			var body chatBody
			testutil.NoError(t, json.Unmarshal(raw, &body))
			testutil.Falsef(t, len(body.Messages) != len(want)+1 || !reflect.DeepEqual(body.Messages[1:], want), "wire messages = %#v", body.Messages)
		})
	}
}
