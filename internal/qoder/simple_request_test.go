package qoder

import (
	"encoding/json"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
	"testing"
)

func TestSimplePromptRequestRetainsContextAndExplicitBudget(t *testing.T) {
	for _, limit := range []int{0, 16} {
		req := upstream.UpstreamRequest{Prompt: "测试123123"}
		if limit != 0 {
			req.MaxTokens = &limit
		}
		encoded, err := buildChatBodyProfile(req, modelEntry{Key: "qfmodel"}, "session", "request", "set", DefaultClientVersion, "", sceneBusinessProduct)
		testutil.NoError(t, err)
		raw, err := decodeBody(encoded)
		testutil.NoError(t, err)
		var body chatBody
		err = json.Unmarshal(raw, &body)
		testutil.NoError(t, err)
		testutil.Falsef(t, len(body.Messages) != 1 || body.Messages[0].Content != req.Prompt || len(body.Tools) != 0 || body.System != "", "unexpected simple payload: messages=%v tools=%d", body.Messages, len(body.Tools))
		context := body.ChatContext.(map[string]any)
		testutil.EqualAny(t, context["text"], req.Prompt)
		testutil.EqualAny(t, context["extra"].(map[string]any)["originalContent"], req.Prompt)
		want := 32000
		if limit != 0 {
			want = limit
		}
		parameters := body.Parameters.(map[string]any)
		testutil.EqualAny(t, parameters["max_tokens"], float64(want))
	}
}
