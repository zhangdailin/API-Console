package web

import (
	"encoding/json"
	"testing"

	"orchids-api/internal/testutil"
)

// The Web request embeds chatwire.Request. Its decoder must still apply the
// shared rules and also keep the Web-only media options, which a promoted
// chatwire UnmarshalJSON alone would drop.
func TestWebChatRequestDecodesSharedFieldsAndMediaOptions(t *testing.T) {
	var req ChatCompletionsRequest
	testutil.NoError(t, json.Unmarshal([]byte(`{
		"model": "grok-imagine-image",
		"messages": [{"role": "user", "content": "draw"}],
		"stream": "true",
		"max_tokens": 100,
		"max_completion_tokens": "250",
		"temperature": "0.5",
		"user": "  caller-1 ",
		"tool_choice": "auto",
		"parallel_tool_calls": false,
		"stop": "END",
		"image_config": {"n": "2", "size": "1024x1024", "response_format": "b64_json"},
		"video_config": {"seconds": "10", "aspect_ratio": "16:9"}
	}`), &req))

	testutil.Equal(t, req.Model, "grok-imagine-image")
	testutil.Equal(t, len(req.Messages), 1)
	testutil.True(t, req.Stream)
	testutil.True(t, req.StreamProvided)
	// max_completion_tokens wins over the deprecated max_tokens.
	testutil.Equal(t, *req.MaxTokens, 250)
	testutil.Equal(t, *req.MaxCompletionTokens, 250)
	testutil.Equal(t, *req.Temperature, 0.5)
	testutil.Equal(t, req.SafetyIdentifier, "caller-1")
	testutil.EqualAny(t, req.ToolChoice, "auto")
	testutil.False(t, *req.ParallelToolCalls)
	testutil.Equal(t, len(req.Stop), 1)

	testutil.True(t, req.ImageConfig != nil)
	testutil.Equal(t, req.ImageConfig.N, 2)
	testutil.Equal(t, req.ImageConfig.ResponseFormat, "b64_json")
	testutil.True(t, req.VideoConfig != nil)
	testutil.Equal(t, req.VideoConfig.VideoLength, 10)
	testutil.Equal(t, req.VideoConfig.AspectRatio, "16:9")
}

func TestWebChatRequestStreamPresence(t *testing.T) {
	for body, provided := range map[string]bool{
		`{"model":"m","messages":[]}`:                false,
		`{"model":"m","messages":[],"stream":null}`:  false,
		`{"model":"m","messages":[],"stream":false}`: true,
	} {
		var req ChatCompletionsRequest
		testutil.NoError(t, json.Unmarshal([]byte(body), &req))
		testutil.Equal(t, req.StreamProvided, provided)
		testutil.True(t, req.ImageConfig == nil && req.VideoConfig == nil)
	}
}

func TestWebChatRequestRejectsMalformedLooseFields(t *testing.T) {
	var req ChatCompletionsRequest
	testutil.Error(t, json.Unmarshal([]byte(`{"model":"m","messages":[],"max_tokens":"lots"}`), &req))
	testutil.Error(t, json.Unmarshal([]byte(`{"model":"m","messages":[],"image_config":{"n":"many"}}`), &req))
}
