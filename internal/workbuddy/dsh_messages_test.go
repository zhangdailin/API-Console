package workbuddy

import (
	"encoding/json"
	"strings"
	"testing"

	"orchids-api/internal/prompt"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

func TestDSHPathBoilerplateCompatibilityAcrossMessageForms(t *testing.T) {
	const first = "Tokens prefixed with @ are paths the user explicitly referenced."
	const second = "Relative paths resolve from the workspace root; absolute paths identify files or directories on the host."
	const replacement = "Tokens prefixed with @ are paths that the user explicitly referenced."
	for _, ending := range []string{"\n", "\r\n"} {
		original := "Keep this rule." + ending + "  " + first + ending + second
		want := strings.Replace(original, first, replacement, 1)
		for _, form := range []string{"system_items", "system_string", "developer_string", "assistant_string", "system_blocks", "developer_blocks", "assistant_blocks"} {
			t.Run(form+ending, func(t *testing.T) {
				req := upstream.UpstreamRequest{Model: "m"}
				if form == "system_items" {
					req.System = []prompt.SystemItem{{Type: "text", Text: original}}
				} else {
					role := strings.Split(form, "_")[0]
					if strings.HasSuffix(form, "_string") {
						req.Messages = []prompt.Message{roleMessage(t, role, original)}
					} else {
						req.Messages = []prompt.Message{{Role: role, Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "text", Text: original}}}}}
					}
				}
				before, err := json.Marshal(req)
				testutil.NoError(t, err)
				body, err := (&Client{}).buildBody(req)
				testutil.NoError(t, err)
				var wire struct {
					Messages []ChatMessage `json:"messages"`
				}
				testutil.NoError(t, json.Unmarshal(body, &wire))
				index := 0
				if strings.HasPrefix(form, "assistant") {
					index = 1
				}
				testutil.Equal(t, wire.Messages[index].Content, want)
				after, err := json.Marshal(req)
				testutil.NoError(t, err)
				testutil.Equal(t, string(after), string(before))
				testutil.Equal(t, normalizeDSHPathBoilerplate(want), want)
			})
		}
	}
}

func TestDSHPathCompatibilityPreservesUserToolDataAndOtherSentences(t *testing.T) {
	const original = "Tokens prefixed with @ are paths the user explicitly referenced."
	req := upstream.UpstreamRequest{Messages: []prompt.Message{
		userMessage(t, original),
		{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{
			{Type: "text", Text: original},
			{Type: "tool_use", ID: "call-a", Name: "save_text", Input: map[string]any{"text": original}},
		}}},
		{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{
			{Type: "tool_result", ToolUseID: "call-a", Content: original},
			{Type: "text", Text: original},
		}}},
	}}
	messages := buildMessages(req)
	testutil.Equal(t, messages[1].Content, original)
	testutil.MustContain(t, messages[2].Content, "are paths that the user")
	var args map[string]string
	testutil.NoError(t, json.Unmarshal([]byte(messages[2].ToolCalls[0].Function.Arguments), &args))
	testutil.Equal(t, args["text"], original)
	testutil.Equal(t, messages[3].Role, "tool")
	testutil.Equal(t, messages[3].Content, original)
	testutil.Equal(t, messages[4].Content, original)
	for _, text := range []string{
		"Quoted: " + original,
		original + " Preserve this suffix.",
		"Tokens prefixed with @ are paths the user explicitly referenced elsewhere.",
	} {
		testutil.Equal(t, normalizeDSHPathBoilerplate(text), text)
	}
}
